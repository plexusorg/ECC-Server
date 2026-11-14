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
	"sync"
	"syscall"
	"time"

	"ecc-server/core"
)

// Line ids remembered, so a resend after a lost answer gets the same verdict and is not counted twice.
const seenIDs = 50000

type server struct {
	cfg      Config
	ctx      context.Context // cancelled when the host stops
	rec      *recordLog
	recorder *recorder
	acts     *actionStore
	spend    *spend
	br       *bedrock
	judge    core.Judge
	scorer   core.Scorer
	scorerIs string
	upd      *updater // nil without a W&B key
	switchMu sync.Mutex
	jobs     sync.WaitGroup

	// mu guards the engine and what follows; the engine is not safe for concurrent use.
	mu      sync.Mutex
	eng     *core.Engine // nil until a bundle is served
	bundle  *Bundle
	last    time.Time
	seen    map[string]string
	seenLog []string
}

func run(cfg Config) error {
	ctx, stopped := context.WithCancel(context.Background())
	defer stopped()
	s := &server{cfg: cfg, ctx: ctx, seen: map[string]string{}}
	if cfg.Test.HoldWordsFile == "" {
		return errors.New("no scorer: the fast model's runner is not part of this build; the end-to-end rig sets [test] hold_words_file")
	}
	ws, err := newWordScorer(cfg.Test.HoldWordsFile)
	if err != nil {
		return err
	}
	s.scorer, s.scorerIs = ws, "the test rig's word list (not a model)"
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
		maxOut: cfg.Judge.MaxOutputTokens, timeout: cfg.Judge.Timeout, told: s.budgetUsedUp}
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
		s.use(b)
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
		slog.Info("the judge calls Bedrock", "model", cfg.Judge.Model, "region", cfg.Judge.Region,
			"service_tier", cfg.Judge.ServiceTier, "endpoint", s.br.endpoint)
	}
	go s.tick()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("listening", "addr", cfg.Listen)
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

// use serves a bundle's judge texts and settings: the engine is made with the first bundle and takes later ones in
// place, keeping its sittings.
func (s *server) use(b *Bundle) {
	cc := coreConfig(s.cfg, b)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eng == nil {
		s.eng = core.NewEngine(cc, b.texts, s.scorer, s.recorder)
	} else {
		s.eng.Swap(cc, b.texts, s.scorer)
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
	if err != nil {
		slog.Warn("checkpoint not ruled; its lines stay held", "sitting", j.Sitting, "k", j.K, "err", err.Error())
	} else {
		slog.Info("ruling", "sitting", j.Sitting, "k", j.K, "ruling", a.Ruling, "by", a.By)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now()
	s.dispatch(s.eng.Rule(j, a, err))
}

// budgetUsedUp tells staff that a budget stops the judge's calls, once per day or month.
func (s *server) budgetUsedUp(e *errOverBudget) {
	until := "the next UTC day"
	if e.period == "monthly" {
		until = "the next UTC month"
	}
	slog.Error("the judge's budget is used up; held lines wait for review", "budget", e.period,
		"usd", fmt.Sprintf("%.2f", e.budget))
	s.acts.add(action{Type: actionNotice, Kind: "budget",
		Text: fmt.Sprintf("ECC: the judge's %s budget of $%.2f is used up. Until %s, held lines stay held and their "+
			"sittings go to the review queue.", e.period, e.budget, until)})
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

// lineRequest is one line from the plugin. A line with no speaker_uuid (a Discord relay) is context only.
type lineRequest struct {
	ID           string   `json:"id"`
	Channel      string   `json:"channel"`
	Conversation string   `json:"conversation"`
	Private      bool     `json:"private"`
	SpeakerUUID  string   `json:"speaker_uuid"`
	Speaker      string   `json:"speaker"`
	TargetUUID   string   `json:"target_uuid"`
	Target       string   `json:"target"`
	Text         string   `json:"text"`
	Groups       []string `json:"groups"`
}

// handleLine gives a line its verdict, timed by the host's clock on arrival.
func (s *server) handleLine(w http.ResponseWriter, r *http.Request) {
	var req lineRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.ID == "" || req.Channel == "" || req.Conversation == "" || req.Speaker == "" {
		writeError(w, http.StatusBadRequest, errors.New("id, channel, conversation and speaker are required"))
		return
	}
	s.mu.Lock()
	verdict, seen := s.seen[req.ID]
	if !seen && s.eng != nil {
		res := s.eng.Line(core.Line{Ref: req.ID, Time: s.now(), Channel: req.Channel, Conversation: req.Conversation,
			Private: req.Private, Account: req.SpeakerUUID, Name: req.Speaker, TargetAccount: req.TargetUUID,
			Target: req.Target, Text: req.Text, Groups: req.Groups, Context: req.SpeakerUUID == ""})
		s.dispatch(res)
		verdict = res.Verdict
		s.seen[req.ID] = verdict
		s.seenLog = append(s.seenLog, req.ID)
		if len(s.seenLog) > seenIDs {
			delete(s.seen, s.seenLog[0])
			s.seenLog = s.seenLog[1:]
		}
	}
	s.mu.Unlock()
	if verdict == "" {
		writeError(w, http.StatusServiceUnavailable, errNoBundle)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"verdict": verdict})
}

// handlePresence takes the plugin's start (a new stream: every open sitting ends) and players' joins and leaves.
func (s *server) handlePresence(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind string `json:"kind"`
		UUID string `json:"uuid"`
		Name string `json:"name"`
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
	now := s.now()
	switch req.Kind {
	case "start":
		slog.Info("the game server started: a new stream, every open sitting ends")
		s.dispatch(s.eng.Start(stamp(now), now))
	default:
		s.dispatch(s.eng.Presence(req.UUID, req.Kind == "join", now))
	}
	s.rec.append("presence", struct {
		Time string `json:"time"`
		Kind string `json:"kind"`
		UUID string `json:"uuid,omitempty"`
		Name string `json:"name,omitempty"`
	}{stamp(now), req.Kind, req.UUID, req.Name})
	writeJSON(w, http.StatusOK, struct{}{})
}

// handleActions answers with every unacknowledged action, oldest first, as soon as there is one, or with none after
// 30 seconds.
func (s *server) handleActions(w http.ResponseWriter, r *http.Request) {
	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()
	for {
		acts, wake := s.acts.unacknowledged()
		if len(acts) > 0 {
			writeJSON(w, http.StatusOK, acts)
			return
		}
		select {
		case <-wake:
		case <-timeout.C:
			writeJSON(w, http.StatusOK, []action{})
			return
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			writeJSON(w, http.StatusOK, []action{})
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
	Version        string         `json:"version,omitempty"`
	Scorer         string         `json:"scorer"`
	Wandb          *wandbStatus   `json:"wandb"` // null when no W&B key is set
	Judge          *bedrockStatus `json:"judge"`
	Spend          spendStatus    `json:"spend"`
	Sittings       core.Status    `json:"sittings"`
	Unacknowledged int            `json:"unacknowledged_actions"`
}

// handleStatus is 200 when a bundle is served, otherwise 503, with the same body.
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	h := hostStatus{Status: "ready", Scorer: s.scorerIs, Judge: s.br.status(), Spend: s.spend.status(),
		Unacknowledged: s.acts.count()}
	if s.upd != nil {
		h.Wandb = s.upd.status()
	}
	code := http.StatusOK
	s.mu.Lock()
	if s.eng == nil {
		h.Status, code = "no bundle", http.StatusServiceUnavailable
	} else {
		h.Version, h.Sittings = s.bundle.Version, s.eng.Status()
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
	if err != nil {
		slog.Error("bundle failed to load; keeping the current one", "version", version, "err", err)
		return err
	}
	if err := activate(s.cfg, version, byOperator); err != nil {
		return err
	}
	s.use(b)
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
