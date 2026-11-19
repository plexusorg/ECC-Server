package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"ecc-server/core"
	"ecc-server/model"
)

// Line ids remembered, so a resend after a lost answer gets the same verdict and is not counted twice.
const seenIDs = 50000

type server struct {
	cfg Config
	// boot is a random id made at start: GET /actions, POST /presence and /status return it, so the plugin sees a
	// restart (a new id) and sends its start, its players and the lines it holds without a verdict again.
	boot     string
	ctx      context.Context // cancelled when the host stops
	rec      *recordLog
	recorder *recorder
	acts     *actionStore
	spend    *spend
	br       *bedrock
	judge    core.Judge
	upd      *updater // nil without a W&B key
	switchMu sync.Mutex
	// scoreMu is held while the scoring worker takes a line, so a bundle switch comes between two lines.
	scoreMu sync.Mutex
	jobs    sync.WaitGroup
	// noticed is when a notice of each kind was last sent (notify).
	noticeMu sync.Mutex
	noticed  map[string]time.Time
	// queue holds the lines waiting for the scoring worker, in arrival order.
	queue chan *lineJob
	// callStart is when the model call in progress started (Unix nanoseconds; 0 when none); slowCalls counts the
	// calls since start that ran slowCall or longer.
	callStart atomic.Int64
	slowCalls atomic.Int64

	// mu guards the engine and what follows; the engine is not safe for concurrent use.
	mu       sync.Mutex
	eng      *core.Engine // nil until a bundle is served
	bundle   *Bundle
	scorer   core.Scorer
	models   *model.Scorer // the bundle's models; nil without a bundle
	scorerIs string
	last     time.Time
	seen     map[string]string
	seenLog  []string
	pending  map[string]*lineJob
}

// Lines waiting for the scoring worker at most; a line beyond is answered 503 and the plugin asks again.
const queueCap = 10000

func run(cfg Config) error {
	ctx, stopped := context.WithCancel(context.Background())
	defer stopped()
	s := &server{cfg: cfg, ctx: ctx, boot: newUUID(), noticed: map[string]time.Time{}, seen: map[string]string{}, pending: map[string]*lineJob{},
		queue: make(chan *lineJob, queueCap)}
	var err error
	if goruntime.GOOS != "linux" {
		return errors.New("the models run on Linux hosts only")
	}
	token, err := loadOrCreateToken(cfg)
	if err != nil {
		return fmt.Errorf("the token: %w", err)
	}
	brKey, err := credential(bedrockCredential)
	if err != nil {
		return fmt.Errorf("reading the Bedrock key credential: %w", err)
	}
	s.br = newBedrock(brKey)
	key, err := credential(wandbCredential)
	if err != nil {
		return fmt.Errorf("reading the W&B key credential: %w", err)
	}
	if key != "" {
		s.upd = newUpdater(s, key)
	} else if err := pruneStaging(cfg, ""); err != nil {
		// staged downloads a removed key left behind
		return err
	}
	if s.rec, err = openRecord(ctx, cfg); err != nil {
		return fmt.Errorf("opening the record: %w", err)
	}
	defer s.rec.close()
	if s.recorder, err = newRecorder(cfg, s.rec); err != nil {
		return err
	}
	if err := scanRestart(s.recorder, s.rec.dir, time.Now()); err != nil {
		return fmt.Errorf("reading the record for sessions left open: %w", err)
	}
	if s.spend, err = openSpend(cfg); err != nil {
		return err
	}
	if s.acts, err = openActions(cfg, s.rec); err != nil {
		return err
	}
	s.judge = &bedrockJudge{ctx: ctx, br: s.br, spend: s.spend, stopped: s.notify, record: s.rec.append, backoff: judgeBackoff,
		retryFor: judgeRetryFor}
	st, err := readState(cfg)
	if err != nil {
		return err
	}
	if err := removeStaleInstalls(cfg); err != nil {
		return err
	}
	if st.Current != "" {
		// A bundle this build cannot load (one an earlier build installed under another bundle contract, or a damaged
		// one) leaves the server running without a bundle, so the W&B check or `ecc-server use` can replace it.
		b, err := loadInstalledBundle(cfg, st.Current)
		var sc core.Scorer
		if err == nil {
			sc, err = s.scorerFor(b)
		}
		if err == nil {
			s.use(b, sc)
		} else if s.upd != nil {
			slog.Error("the installed bundle does not load; not ready until production is fetched from W&B",
				"bundle", st.Current, "err", err)
		} else {
			slog.Error("the installed bundle does not load; not ready until `ecc-server use` installs one",
				"bundle", st.Current, "err", err)
		}
	} else if s.upd != nil {
		slog.Warn("no bundle installed; not ready until one is fetched from W&B")
	} else {
		slog.Warn("no bundle installed; not ready until `ecc-server use` installs one")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /line", s.handleLine)
	mux.HandleFunc("POST /presence", s.handlePresence)
	mux.HandleFunc("GET /actions", s.handleActions)
	mux.HandleFunc("POST /actions/ack", s.handleAck)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("POST /switch", s.handleSwitch)
	mux.HandleFunc("POST /update", s.handleUpdate)
	srv := &http.Server{Addr: cfg.Listen, Handler: requireToken(token, mux)}
	if s.upd != nil {
		slog.Info("checking W&B for the production bundle", "artifact", s.upd.name(), "every", checkInterval.String())
		go s.upd.loop(ctx)
	} else {
		slog.Info("no W&B key is set; serving the bundle on disk (set one with sudo ecc-server wandb login)")
	}
	if brKey == "" {
		slog.Warn("no Bedrock key is set; checkpoints are not judged until one is set with sudo ecc-server bedrock login")
	} else {
		slog.Info("the judge calls Bedrock's Responses API", "model", judgeModel, "region", judgeRegion,
			"service_tier", judgeServiceTier, "reasoning_effort", judgeReasoningEffort, "endpoint", s.br.endpoint)
	}
	go s.tick()
	go s.scoreLines()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("listening", "addr", cfg.Listen, "boot", s.boot)
	select {
	case err = <-errc:
	case sig := <-stop:
		slog.Info("stopping", "signal", sig.String())
	}
	stopped()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	// Judge calls in flight end with the host's context; their sessions are ended by the next start's scan.
	done := make(chan struct{})
	go func() { s.jobs.Wait(); close(done) }()
	select {
	case <-done:
	case <-shutdown.Done():
	}
	return err
}

// Each model call's CPU threads, at most the host's CPU count; one call runs at a time.
const modelThreads = 6

// scorerFor loads the bundle's models.
func (s *server) scorerFor(b *Bundle) (core.Scorer, error) {
	if b.specErr != nil {
		return nil, fmt.Errorf("bundle %s: %w", b.Version, b.specErr)
	}
	threads := min(modelThreads, goruntime.NumCPU())
	t0 := time.Now()
	sc, err := model.Open(b.dir, b.spec, model.Options{Threads: threads, Threshold: s.cfg.Model.Threshold})
	if err != nil {
		return nil, fmt.Errorf("bundle %s: the models: %w", b.Version, err)
	}
	slog.Info("models loaded", "bundle", b.Version, "seconds", fmt.Sprintf("%.1f", time.Since(t0).Seconds()),
		"threads", threads, "threshold", sc.Threshold())
	if !sc.MarksCategories() {
		slog.Warn("the bundle sets no category thresholds; no line is marked with a category", "bundle", b.Version)
	}
	return sc, nil
}

// use serves a bundle's judge texts, settings and scorer: the engine is made with the first bundle and takes later
// ones in place, keeping its sessions. The switch waits for the line being scored, if any; the new models take the
// previous models' context, and the previous models are freed.
func (s *server) use(b *Bundle, sc core.Scorer) {
	cc := coreConfig(s.cfg, b)
	s.scoreMu.Lock()
	defer s.scoreMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.models
	s.scorer, s.models = sc, nil
	if m, ok := sc.(*model.Scorer); ok {
		s.models = m
		s.scorerIs = "the bundle's models, hold threshold " + m.Threshold()
		if old != nil {
			m.TakeContext(old)
		}
	}
	if old != nil {
		go old.Close()
	}
	if s.eng == nil {
		s.eng = core.NewEngine(cc, b.texts, sc, s.recorder)
		if allowed, err := loadLinks(s.cfg); err != nil {
			slog.Error("the saved allowlist of links is not read; no link is replaced until the plugin's next start", "err", err)
		} else if err := s.eng.AllowLinks(allowed); err != nil {
			slog.Error("the saved allowlist of links is not used; no link is replaced until the plugin's next start", "err", err)
		}
	} else {
		s.eng.Swap(cc, b.texts, sc)
	}
	s.bundle = b
	slog.Info("serving bundle", "version", b.Version, "cases", len(b.texts.Examples), "scorer", s.scorerIs)
}

// now is the engine's clock: the host's time, never earlier than a time the engine has seen. The caller holds mu.
func (s *server) now() time.Time {
	t := time.Now()
	if t.Before(s.last) {
		t = s.last
	}
	s.last = t
	return t
}

// tick runs the waiting-time checkpoints and session ends once a second, on the host's clock.
func (s *server) tick() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		if s.eng != nil {
			s.dispatch(s.eng.Tick(s.now()))
		}
		s.mu.Unlock()
	}
}

// dispatch hands the engine's actions to the plugin and starts its judge calls. The caller holds mu.
func (s *server) dispatch(r core.Result) {
	for _, a := range r.Actions {
		if act, ok := fromCore(a); ok {
			s.acts.add(act)
		}
	}
	for _, j := range r.Jobs {
		s.jobs.Add(1)
		go s.runJob(j)
	}
}

// runJob puts one checkpoint to the judge and hands the answer to the engine.
func (s *server) runJob(j *core.Job) {
	defer s.jobs.Done()
	a, err := s.judge.Rule(j)
	switch {
	case errors.Is(err, core.ErrPending):
		slog.Warn("the judge could not be reached; the checkpoint is put again later", "session", j.Session, "k", j.K,
			"err", err.Error())
	case err != nil:
		slog.Warn("checkpoint not ruled; its lines stay held", "session", j.Session, "k", j.K, "err", err.Error())
	default:
		slog.Info("ruling", "session", j.Session, "k", j.K, "ruling", a.Ruling, "by", a.By)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now()
	s.dispatch(s.eng.Rule(j, a, err))
}

// A notice of one cause is sent at most this often.
const noticeEvery = time.Hour

// notify tells staff with the alert permission that judging stopped, and why, or that a model call is slow: a notice
// of each kind at most once an hour. The budget's notice comes once per UTC day or month
// (spend), within that.
func (s *server) notify(kind, text string) {
	now := time.Now()
	s.noticeMu.Lock()
	last, ok := s.noticed[kind]
	send := !ok || now.Sub(last) >= noticeEvery
	if send {
		s.noticed[kind] = now
	}
	s.noticeMu.Unlock()
	if !send {
		return
	}
	slog.Error("staff are told", "notice", kind, "text", text)
	s.acts.add(action{Type: actionNotice, Kind: kind, Text: text})
}

// The plugin's allowlist of links from its last start, in <data>/links.json, so a restart of this server replaces the
// same links before the plugin's next start.
func linksPath(cfg Config) string { return filepath.Join(cfg.Data, "links.json") }

func saveLinks(cfg Config, allowed []string) error {
	data, _ := json.Marshal(allowed)
	return writeAtomic(linksPath(cfg), data)
}

func loadLinks(cfg Config) ([]string, error) {
	var allowed []string
	if err := readJSON(linksPath(cfg), &allowed); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return allowed, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

var errNoBundle = errors.New("no bundle is being served")

// lineJob is one line waiting for its verdict, timed when it arrived.
type lineJob struct {
	req     wireLine
	arrived time.Time
	done    chan struct{}
	verdict string
}

// handleLine gives a line its verdict. Lines are scored one at a time, in the order they arrive, by scoreLines; a
// resent id gets the verdict its line got (or waits for it) and is not counted twice.
func (s *server) handleLine(w http.ResponseWriter, r *http.Request) {
	var req wireLine
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := req.check(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	if verdict, ok := s.seen[req.ID]; ok {
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"verdict": verdict})
		return
	}
	j := s.pending[req.ID]
	if j == nil {
		if s.eng == nil {
			s.mu.Unlock()
			writeError(w, http.StatusServiceUnavailable, errNoBundle)
			return
		}
		// the line's time is when it arrived, not when the worker takes it
		j = &lineJob{req: req, arrived: s.now(), done: make(chan struct{})}
		select {
		case s.queue <- j:
			s.pending[req.ID] = j
		default:
			s.mu.Unlock()
			writeError(w, http.StatusServiceUnavailable, errors.New("too many lines are waiting to be scored"))
			return
		}
	}
	s.mu.Unlock()
	select {
	case <-j.done:
		writeJSON(w, http.StatusOK, map[string]string{"verdict": j.verdict})
	case <-r.Context().Done():
	case <-s.ctx.Done():
		writeError(w, http.StatusServiceUnavailable, errors.New("the server is stopping"))
	}
}

// scoreLines is the scoring worker: it takes the queued lines in arrival order, one at a time, so that each line's
// context is the lines before it and one model call runs at a time. A model call always runs to its end. A line still
// waiting for its verdict maxWait after it arrived is answered hold and is context for later lines at once:
// when its models are running then, their verdict is applied when they end; when the worker comes to it later than
// that, it is scored, with the context it had, when no line is waiting (rescore). The engine's lock is not held while
// the models run.
func (s *server) scoreLines() {
	var later []laterLine
	for {
		var j *lineJob
		select {
		case <-s.ctx.Done():
			return
		case j = <-s.queue:
		default:
			if len(later) > 0 {
				s.rescore(later[0])
				later = later[1:]
				continue
			}
			select {
			case <-s.ctx.Done():
				return
			case j = <-s.queue:
			}
		}
		if l, ok := s.scoreLine(j); ok {
			later = append(later, l)
		}
	}
}

// laterLine is a line answered hold before its models ran, to score when no line is waiting: its call, and the models
// that took it (nil without models).
type laterLine struct {
	ref  string
	call scoreCall
	by   *model.Scorer
}

// scoreCall scores one line taken by the scorer, with the context the line had when it was taken.
type scoreCall func() (core.Verdict, error)

// The worker's errors for a line answered hold before its verdict (errors.Is core.ErrLater).
type laterError string

func (e laterError) Error() string        { return string(e) }
func (e laterError) Is(target error) bool { return target == core.ErrLater }

// A line still waiting for its verdict this long after it arrived is answered hold (core.ErrLater), a little under
// Module-ECC's 1000 ms wait for a verdict. Its models run to their end, when the worker comes to it if they had not
// started, and their verdict is applied then.
const maxWait = 900 * time.Millisecond

const (
	errQueued  laterError = "the verdict's wait passed before its models ran; scored when no line is waiting"
	errRunning laterError = "the verdict's wait passed while its models ran; scored when they end"
)

// A model call that runs this long is logged and staff are told, since every line behind it waits; it is not cut.
const slowCall = 10 * time.Second

// Notice kind of a slow model call.
const noticeSlowModel = "slow_model"

// rescore scores a line answered hold before its models ran and hands its verdict to the engine, unless its session
// settled it meanwhile. A line the models since replaced took (a bundle switch) is held without a score.
func (s *server) rescore(l laterLine) {
	s.scoreMu.Lock()
	defer s.scoreMu.Unlock()
	s.mu.Lock()
	waiting, current := s.eng.Unscored(l.ref), s.models == l.by
	s.mu.Unlock()
	if !waiting {
		return
	}
	v, err := core.Verdict{}, errors.New("the models that took the line were replaced before it was scored")
	if current {
		v, err = s.call(l.ref, l.call, time.Time{}, nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now()
	s.dispatch(s.eng.Rescored(l.ref, v, err))
}

// scoreLine gives one queued line its verdict; ok when the line was answered hold before its models ran, to be scored
// when no line is waiting.
func (s *server) scoreLine(j *lineJob) (later laterLine, ok bool) {
	s.scoreMu.Lock()
	defer s.scoreMu.Unlock()
	l := j.req.coreLine(j.arrived)
	deadline := j.arrived.Add(maxWait)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.eng.Known(l.Channel) {
		slog.Error("a line on a channel this server does not know is held; its session goes on the review queue",
			"channel", l.Channel, "id", j.req.ID)
	}
	var call scoreCall
	if scored, score := s.eng.Prepare(l); score {
		call = s.take(scored)
	}
	switch {
	case call == nil:
		s.answer(j, s.eng.Line(l))
	case !time.Now().Before(deadline):
		s.answer(j, s.eng.LineScored(l, core.Verdict{}, errQueued))
		later, ok = laterLine{l.Ref, call, s.models}, s.eng.Unscored(l.Ref)
	default:
		s.mu.Unlock()
		answered := false
		v, err := s.call(l.Ref, call, deadline, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.answer(j, s.eng.LineScored(l, core.Verdict{}, errRunning))
			answered = true
		})
		s.mu.Lock()
		if answered {
			s.now()
			s.dispatch(s.eng.Rescored(l.Ref, v, err))
		} else {
			s.answer(j, s.eng.LineScored(l, v, err))
		}
	}
	return later, ok
}

// take makes a line the engine scores context for later lines and gives the call that scores it. The caller holds mu.
func (s *server) take(l core.Line) scoreCall {
	if s.models == nil {
		return nil
	}
	c, err := s.models.Take(l)
	if err != nil {
		return func() (core.Verdict, error) { return core.Verdict{}, err }
	}
	return func() (core.Verdict, error) {
		d, err := c.Run()
		return d.Verdict, err
	}
}

// call runs one model call to its end and gives its verdict. When late is set and the call has not ended at the time
// at, late runs then, while the call goes on. A call that has run slowCall is logged as an error
// and staff are told (notice slow_model), at most once an hour; it is not cut, and the lines behind it wait.
func (s *server) call(ref string, f scoreCall, at time.Time, late func()) (core.Verdict, error) {
	type result struct {
		v   core.Verdict
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	s.callStart.Store(start.UnixNano())
	defer s.callStart.Store(0)
	go func() {
		v, err := f()
		done <- result{v, err}
	}()
	var lateC <-chan time.Time
	if late != nil {
		t := time.NewTimer(time.Until(at))
		defer t.Stop()
		lateC = t.C
	}
	slow := time.NewTimer(slowCall)
	defer slow.Stop()
	reported := false
	for {
		select {
		case r := <-done:
			if reported {
				slog.Warn("the slow model call ended", "id", ref, "seconds", fmt.Sprintf("%.1f", time.Since(start).Seconds()))
			}
			return r.v, r.err
		case <-lateC:
			lateC = nil
			late()
		case <-slow.C:
			reported = true
			s.slowCalls.Add(1)
			slog.Error("a model call has run without ending; it is not cut, and the lines behind it wait", "id", ref,
				"seconds", fmt.Sprintf("%.1f", time.Since(start).Seconds()), "lines_waiting", len(s.queue))
			s.notify(noticeSlowModel, fmt.Sprintf("ECC: one line's model call has run for %s without ending, so every "+
				"chat line waits behind it and is held. Check the ECC-Server host's CPU and memory.", slowCall))
		}
	}
}

// answer gives the line its verdict and hands the engine's actions on. The caller holds mu.
func (s *server) answer(j *lineJob, res core.Result) {
	s.dispatch(res)
	j.verdict = res.Verdict
	s.seen[j.req.ID] = res.Verdict
	s.seenLog = append(s.seenLog, j.req.ID)
	if len(s.seenLog) > seenIDs {
		delete(s.seen, s.seenLog[0])
		s.seenLog = s.seenLog[1:]
	}
	delete(s.pending, j.req.ID)
	close(j.done)
}

// handlePresence takes the plugin's start (a new stream: every open session ends; the allowlist of links) and
// players' joins and leaves.
func (s *server) handlePresence(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind         string   `json:"kind"`
		UUID         string   `json:"uuid"`
		Name         string   `json:"name"`
		AllowedLinks []string `json:"allowed_links"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Kind != "start" && (req.Kind != "join" && req.Kind != "leave" || req.UUID == "") {
		writeError(w, http.StatusBadRequest, errors.New(`expected {"kind": "start"} or {"kind": "join"|"leave", "uuid"}`))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eng == nil {
		writeError(w, http.StatusServiceUnavailable, errNoBundle)
		return
	}
	if req.Kind == "start" {
		if err := s.eng.AllowLinks(req.AllowedLinks); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := saveLinks(s.cfg, req.AllowedLinks); err != nil {
			slog.Error("could not save the allowlist of links; a restart replaces no link until the next start", "err", err)
		}
	}
	now := s.now()
	switch req.Kind {
	case "start":
		slog.Info("the game server started: a new stream, every open session ends", "allowed_links", len(req.AllowedLinks))
		s.dispatch(s.eng.Start(stamp(now), now))
	default:
		s.dispatch(s.eng.Presence(req.UUID, req.Kind == "join", now))
	}
	s.rec.append("presence", struct {
		Time         string   `json:"time"`
		Kind         string   `json:"kind"`
		UUID         string   `json:"uuid,omitempty"`
		Name         string   `json:"name,omitempty"`
		AllowedLinks []string `json:"allowed_links,omitempty"`
	}{stamp(now), req.Kind, req.UUID, req.Name, req.AllowedLinks})
	writeJSON(w, http.StatusOK, bootOnly{s.boot})
}

// bootOnly is POST /presence's answer.
type bootOnly struct {
	Boot string `json:"boot"`
}

// actionsAnswer is GET /actions's answer.
type actionsAnswer struct {
	Boot    string   `json:"boot"`
	Actions []action `json:"actions"`
}

// handleActions answers with every unacknowledged action, oldest first, as soon as there is one, or with none after
// 30 seconds.
func (s *server) handleActions(w http.ResponseWriter, r *http.Request) {
	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()
	for {
		acts, wake := s.acts.unacknowledged()
		if len(acts) > 0 {
			writeJSON(w, http.StatusOK, actionsAnswer{s.boot, acts})
			return
		}
		select {
		case <-wake:
		case <-timeout.C:
			writeJSON(w, http.StatusOK, actionsAnswer{s.boot, []action{}})
			return
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			writeJSON(w, http.StatusOK, actionsAnswer{s.boot, []action{}})
			return
		}
	}
}

func (s *server) handleAck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		OK     *bool  `json:"ok"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.ID == "" || req.OK == nil {
		writeError(w, http.StatusBadRequest, errors.New(`expected {"id", "ok", "detail"}`))
		return
	}
	s.acts.ack(req.ID, *req.OK, req.Detail)
	writeJSON(w, http.StatusOK, struct{}{})
}

type hostStatus struct {
	Status         string         `json:"status"`
	Boot           string         `json:"boot"`
	Version        string         `json:"version,omitempty"`
	Scorer         string         `json:"scorer"`
	Waiting        int            `json:"lines_waiting"`
	ModelCall      float64        `json:"model_call_seconds"` // how long the model call in progress has run; 0: none
	SlowCalls      int64          `json:"slow_model_calls"`   // calls since start that ran slowCall or longer
	Wandb          *wandbStatus   `json:"wandb"`              // null when no W&B key is set
	Judge          *bedrockStatus `json:"judge"`
	Spend          spendStatus    `json:"spend"`
	Sessions       core.Status    `json:"sessions"`
	Unacknowledged int            `json:"unacknowledged_actions"`
}

// handleStatus is 200 when a bundle is served, otherwise 503, with the same body.
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	h := hostStatus{Status: "ready", Boot: s.boot, Judge: s.br.status(), Spend: s.spend.status(), Unacknowledged: s.acts.count(),
		Waiting: len(s.queue), SlowCalls: s.slowCalls.Load()}
	if t := s.callStart.Load(); t != 0 {
		h.ModelCall = math.Round(time.Since(time.Unix(0, t)).Seconds()*1000) / 1000
	}
	if s.upd != nil {
		h.Wandb = s.upd.status()
	}
	code := http.StatusOK
	s.mu.Lock()
	if s.eng == nil {
		h.Status, code = "no bundle", http.StatusServiceUnavailable
	} else {
		h.Version, h.Sessions, h.Scorer = s.bundle.Version, s.eng.Status(), s.scorerIs
	}
	s.mu.Unlock()
	writeJSON(w, code, h)
}

func (s *server) handleSwitch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Version == "" {
		writeError(w, http.StatusBadRequest, errors.New("expected {\"version\"}"))
		return
	}
	s.switchMu.Lock()
	defer s.switchMu.Unlock()
	if err := s.serve(req.Version, true); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"version": req.Version})
}

// serve switches to an installed bundle: a bundle that fails to load is not activated and the current one keeps
// serving. The caller holds switchMu.
func (s *server) serve(version string, byOperator bool) error {
	s.mu.Lock()
	cur := s.bundle
	s.mu.Unlock()
	if cur != nil && cur.Version == version {
		return nil
	}
	b, err := loadInstalledBundle(s.cfg, version)
	var sc core.Scorer
	if err == nil {
		sc, err = s.scorerFor(b)
	}
	if err != nil {
		slog.Error("bundle failed to load; keeping the current one", "version", version, "err", err)
		return err
	}
	if err := activate(s.cfg, version, byOperator); err != nil {
		if m, ok := sc.(*model.Scorer); ok {
			m.Close()
		}
		return err
	}
	s.use(b, sc)
	return nil
}

func (s *server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if s.upd == nil {
		writeError(w, http.StatusConflict, errors.New("no W&B key is set; set one with sudo ecc-server wandb login"))
		return
	}
	// The host's context, not the request's: an operator who stops waiting does not abort a download.
	res, err := s.upd.check(s.ctx, true)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": res})
}
