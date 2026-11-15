package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"sync"
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
	words    *wordScorer // the rig's scorer, in place of the bundle's models
	upd      *updater    // nil without a W&B key
	switchMu sync.Mutex
	// scoreMu is held while the scoring worker takes a line, so a bundle switch comes between two lines.
	scoreMu sync.Mutex
	jobs    sync.WaitGroup
	// noticed is when a notice of each kind was last sent (notifyStop).
	noticeMu sync.Mutex
	noticed  map[string]time.Time
	// queue holds the lines waiting for the scoring worker, in arrival order.
	queue chan *lineJob

	// mu guards the engine and what follows; the engine is not safe for concurrent use.
	mu       sync.Mutex
	eng      *core.Engine // nil until a bundle is served
	bundle   *Bundle
	scorer   core.Scorer
	models   *model.Scorer // the bundle's models; nil with the rig's scorer
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
	if cfg.Test.HoldWordsFile != "" {
		if s.words, err = newWordScorer(cfg.Test.HoldWordsFile); err != nil {
			return err
		}
	} else if goruntime.GOOS != "linux" {
		return errors.New("the models run on Linux hosts only; on this system only the end-to-end rig's scorer runs ([test] hold_words_file)")
	}
	token, err := loadOrCreateToken(cfg)
	if err != nil {
		return fmt.Errorf("the token: %w", err)
	}
	brKey, err := credential(bedrockCredential)
	if err != nil {
		return fmt.Errorf("reading the Bedrock key credential: %w", err)
	}
	s.br = newBedrock(brKey, cfg)
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
		return fmt.Errorf("reading the record for sittings left open: %w", err)
	}
	if s.spend, err = openSpend(cfg); err != nil {
		return err
	}
	if s.acts, err = openActions(cfg, s.rec); err != nil {
		return err
	}
	s.judge = &bedrockJudge{ctx: ctx, br: s.br, spend: s.spend, tier: cfg.Judge.ServiceTier,
		effort: cfg.Judge.ReasoningEffort, maxOut: cfg.Judge.MaxOutputTokens, timeout: cfg.Judge.Timeout,
		stopped: s.notifyStop, record: s.rec.append, backoff: orDefault(cfg.Test.JudgeBackoff, judgeBackoff),
		retryFor: orDefault(cfg.Test.JudgeRetryFor, judgeRetryFor)}
	st, err := readState(cfg)
	if err != nil {
		return err
	}
	if err := removeStaleInstalls(cfg); err != nil {
		return err
	}
	if st.Current != "" {
		b, err := loadBundle(bundleDir(cfg, st.Current))
		if err != nil {
			return err
		}
		sc, err := s.scorerFor(b)
		if err != nil {
			return err
		}
		s.use(b, sc)
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
		slog.Info("checking W&B for the production bundle", "artifact", s.upd.name(), "every", cfg.CheckInterval.String())
		go s.upd.loop(ctx)
	} else {
		slog.Info("no W&B key is set; serving the bundle on disk (set one with sudo ecc-server wandb login)")
	}
	if brKey == "" {
		slog.Warn("no Bedrock key is set; checkpoints are not judged until one is set with sudo ecc-server bedrock login")
	} else {
		slog.Info("the judge calls Bedrock's Responses API", "model", cfg.Judge.Model, "region", cfg.Judge.Region,
			"service_tier", cfg.Judge.ServiceTier, "reasoning_effort", cfg.Judge.ReasoningEffort, "endpoint", s.br.endpoint)
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
	// Judge calls in flight end with the host's context; their sittings are ended by the next start's scan.
	done := make(chan struct{})
	go func() { s.jobs.Wait(); close(done) }()
	select {
	case <-done:
	case <-shutdown.Done():
	}
	return err
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// scorerFor loads the bundle's models, or gives the rig's scorer when one is set.
func (s *server) scorerFor(b *Bundle) (core.Scorer, error) {
	if s.words != nil {
		return s.words, nil
	}
	if b.specErr != nil {
		return nil, fmt.Errorf("bundle %s: %w", b.Version, b.specErr)
	}
	m := s.cfg.Model
	t0 := time.Now()
	sc, err := model.Open(b.dir, b.spec, model.Options{Threads: m.Threads, CallLimit: m.CallTimeout, Threshold: m.Threshold})
	if err != nil {
		return nil, fmt.Errorf("bundle %s: the models: %w", b.Version, err)
	}
	slog.Info("models loaded", "bundle", b.Version, "seconds", fmt.Sprintf("%.1f", time.Since(t0).Seconds()),
		"threads", m.Threads, "threshold", sc.Threshold())
	if !sc.MarksKinds() {
		slog.Warn("the bundle sets no kind thresholds; no line is marked with a kind", "bundle", b.Version)
	}
	return sc, nil
}

// use serves a bundle's judge texts, settings and scorer: the engine is made with the first bundle and takes later
// ones in place, keeping its sittings. The switch waits for the line being scored, if any; the new models take the
// previous models' context, and the previous models are freed.
func (s *server) use(b *Bundle, sc core.Scorer) {
	cc := coreConfig(s.cfg, b)
	s.scoreMu.Lock()
	defer s.scoreMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.models
	s.scorer, s.models = sc, nil
	s.scorerIs = "the test rig's word list (not a model)"
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

// tick runs the waiting-time checkpoints and sitting ends once a second, on the host's clock.
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
		slog.Warn("the judge could not be reached; the checkpoint is put again later", "sitting", j.Sitting, "k", j.K,
			"err", err.Error())
	case err != nil:
		slog.Warn("checkpoint not ruled; its lines stay held", "sitting", j.Sitting, "k", j.K, "err", err.Error())
	default:
		slog.Info("ruling", "sitting", j.Sitting, "k", j.K, "ruling", a.Ruling, "by", a.By)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now()
	s.dispatch(s.eng.Rule(j, a, err))
}

// A notice of one cause is sent at most this often.
const noticeEvery = time.Hour

// notifyStop tells staff with the alert permission that judging stopped, and why: a notice of each kind at most once
// per [test] notice_every (an hour). The budget's notice comes once per UTC day or month (spend), within that.
func (s *server) notifyStop(kind, text string) {
	now := time.Now()
	s.noticeMu.Lock()
	last, ok := s.noticed[kind]
	send := !ok || now.Sub(last) >= orDefault(s.cfg.Test.NoticeEvery, noticeEvery)
	if send {
		s.noticed[kind] = now
	}
	s.noticeMu.Unlock()
	if !send {
		return
	}
	slog.Error("judging stopped; staff are told", "cause", kind)
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
// context is the lines before it and one model call runs at a time. A line that has waited [model] max_wait is held
// (its model call is cut off at that point; a call is also cut after [model] call_timeout, and its line held): it is
// context for later lines at once, and is scored, with the context it had, when no line is waiting (rescore). The
// engine's lock is not held while the models run.
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

// laterLine is a line held when its time to be scored ran out, to score when no line is waiting.
type laterLine struct {
	line core.Line
	p    *model.Later
}

// rescore scores a line held when its time ran out and hands its verdict to the engine, unless its sitting settled it
// meanwhile.
func (s *server) rescore(l laterLine) {
	s.scoreMu.Lock()
	defer s.scoreMu.Unlock()
	s.mu.Lock()
	m := s.models
	waiting := s.eng.Unscored(l.line.Ref)
	s.mu.Unlock()
	if !waiting {
		return
	}
	var d model.Detail
	err := errors.New("the models are closed")
	if m != nil {
		d, err = m.ScoreLater(l.p)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now()
	s.dispatch(s.eng.Rescored(l.line.Ref, d.Verdict, err))
}

// scoreLine gives one queued line its verdict; ok when the line is held to be scored later.
func (s *server) scoreLine(j *lineJob) (later laterLine, ok bool) {
	s.scoreMu.Lock()
	req := j.req
	l := req.coreLine(j.arrived)
	s.mu.Lock()
	if !s.eng.Known(l.Channel) {
		slog.Error("a line on a channel this server does not know is held; its sitting goes on the review queue",
			"channel", l.Channel, "id", req.ID)
	}
	var res core.Result
	scored, score := s.eng.Prepare(l)
	if m := s.models; score && m != nil {
		s.mu.Unlock()
		d, err := m.ScoreBy(scored, j.arrived.Add(s.cfg.Model.MaxWait))
		s.mu.Lock()
		res = s.eng.LineScored(l, d.Verdict, err)
		var p *model.Later
		if errors.As(err, &p) && s.eng.Unscored(l.Ref) {
			later, ok = laterLine{scored, p}, true
		}
	} else {
		res = s.eng.Line(l)
	}
	s.dispatch(res)
	j.verdict = res.Verdict
	s.seen[req.ID] = res.Verdict
	s.seenLog = append(s.seenLog, req.ID)
	if len(s.seenLog) > seenIDs {
		delete(s.seen, s.seenLog[0])
		s.seenLog = s.seenLog[1:]
	}
	delete(s.pending, req.ID)
	s.mu.Unlock()
	s.scoreMu.Unlock()
	close(j.done)
	return later, ok
}

// handlePresence takes the plugin's start (a new stream: every open sitting ends; the allowlist of links) and
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
		slog.Info("the game server started: a new stream, every open sitting ends", "allowed_links", len(req.AllowedLinks))
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
	Wandb          *wandbStatus   `json:"wandb"` // null when no W&B key is set
	Judge          *bedrockStatus `json:"judge"`
	Spend          spendStatus    `json:"spend"`
	Sittings       core.Status    `json:"sittings"`
	Unacknowledged int            `json:"unacknowledged_actions"`
}

// handleStatus is 200 when a bundle is served, otherwise 503, with the same body.
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	h := hostStatus{Status: "ready", Boot: s.boot, Judge: s.br.status(), Spend: s.spend.status(), Unacknowledged: s.acts.count(),
		Waiting: len(s.queue)}
	if s.upd != nil {
		h.Wandb = s.upd.status()
	}
	code := http.StatusOK
	s.mu.Lock()
	if s.eng == nil {
		h.Status, code = "no bundle", http.StatusServiceUnavailable
	} else {
		h.Version, h.Sittings, h.Scorer = s.bundle.Version, s.eng.Status(), s.scorerIs
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
	b, err := loadBundle(bundleDir(s.cfg, version))
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
