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
	"strconv"
	"sync"
	"syscall"
	"time"
)

// A conversation idle this long drops its window; its next message starts a new one.
const idleWindow = 30 * time.Minute

// model is one served bundle with its llama-server. A swap replaces it whole, so a score, its bundle version and the
// decision parameters always come from the same bundle.
type model struct {
	bundle   *Bundle
	llama    *llama
	ban, ok  int           // token ids of the two answers
	header   int           // tokens of the header
	slots    chan struct{} // one per llama-server slot
	inflight sync.WaitGroup
}

func startModel(cfg Config, b *Bundle) (*model, error) {
	l, err := startLlama(cfg, b)
	if err != nil {
		return nil, err
	}
	m, err := newModel(b, l)
	if err != nil {
		l.stop()
		return nil, err
	}
	slog.Info("serving bundle", "version", b.Version, "llama", l.url)
	return m, nil
}

func newModel(b *Bundle, l *llama) (*model, error) {
	m := &model{bundle: b, llama: l, slots: make(chan struct{}, parallelSlots(b.LlamaServerArgs))}
	ids := make([]int, 2)
	for i, a := range b.Prompt.Answers {
		t, err := l.tokenize(a)
		if err != nil {
			return nil, err
		}
		if len(t) != 1 {
			return nil, fmt.Errorf("bundle %s: answer %q is %d tokens, not one", b.Version, a, len(t))
		}
		ids[i] = t[0]
	}
	m.ban, m.ok = ids[0], ids[1]
	h, err := l.tokenize(b.Prompt.Header)
	m.header = len(h)
	return m, err
}

// parallelSlots is llama-server's slot count from its arguments (-np, --parallel; 1 when unset).
func parallelSlots(args []string) int {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-np" || args[i] == "--parallel" {
			if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
				return n
			}
		}
	}
	return 1
}

// complete holds one llama-server slot for the call, released even if it panics.
func (m *model) complete(prompt string, topLogprobs int) (*completion, error) {
	m.slots <- struct{}{}
	defer func() { <-m.slots }()
	return m.llama.complete(prompt, topLogprobs)
}

func (m *model) count(text string) (int, error) {
	t, err := m.llama.tokenize(text)
	return len(t), err
}

// conversation holds one conversation's window. Its messages take turns in arrival order: each waits for the
// previous one's turn channel to close. Only the turn holder touches gen and win.
type conversation struct {
	tail    chan struct{}
	pending int
	last    time.Time
	gen     *model
	win     window
}

type server struct {
	cfg      Config
	ctx      context.Context // cancelled when the host stops
	mu       sync.Mutex
	cur      *model
	convs    map[string]*conversation
	switchMu sync.Mutex
	upd      *updater // nil without a W&B key
	br       *bedrock
}

type scoreRequest struct {
	Conversation string `json:"conversation"`
	Speaker      string `json:"speaker"`
	Text         string `json:"text"`
}

type scoreResponse struct {
	PBan      float64 `json:"p_ban"`
	Margin    float64 `json:"margin"`
	Version   string  `json:"version"`
	Earlier   int     `json:"earlier"`   // earlier lines in the prompt
	Tokens    int     `json:"tokens"`    // prompt tokens
	Evaluated int     `json:"evaluated"` // prompt tokens llama-server decoded (the rest came from its cache)
}

// errNoAnswer: neither answer token is among the top logprobs, so the message has no score.
var errNoAnswer = errors.New("neither answer token is among the top logprobs")

func run(cfg Config) error {
	if err := ownChildren(); err != nil {
		return err
	}
	ctx, stopped := context.WithCancel(context.Background())
	defer stopped()
	s := &server{cfg: cfg, ctx: ctx, convs: map[string]*conversation{}}
	brKey, err := credential(bedrockCredential)
	if err != nil {
		return fmt.Errorf("reading the Bedrock key credential: %w", err)
	}
	s.br = newBedrock(brKey, cfg.BedrockRegion, cfg.BedrockModel)
	key, err := credential(wandbCredential)
	if err != nil {
		return fmt.Errorf("reading the W&B key credential: %w", err)
	}
	if key != "" {
		s.upd = newUpdater(s, key)
	} else if err := pruneStaging(cfg, ""); err != nil {
		// downloads from before the key was removed
		return err
	}
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
		if s.cur, err = startModel(cfg, b); err != nil {
			return err
		}
	} else if s.upd != nil {
		slog.Warn("no bundle installed; not ready until one is fetched from W&B")
	} else {
		slog.Warn("no bundle installed; not ready until `ecc-server use` installs one")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /score", s.handleScore)
	mux.HandleFunc("GET /bundle", s.handleBundle)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /switch", s.handleSwitch)
	mux.HandleFunc("POST /update", s.handleUpdate)
	srv := &http.Server{Addr: cfg.Listen, Handler: mux}
	go s.dropIdle()
	if s.upd != nil {
		slog.Info("checking W&B for the production bundle", "artifact", s.upd.name(), "every", cfg.CheckInterval.String())
		go s.upd.loop(ctx)
	} else {
		slog.Info("no W&B key is set; serving the bundle on disk (set one with sudo ecc-server wandb login)")
	}
	if brKey == "" {
		slog.Warn("no Bedrock key is set; slow checks fail until one is set with sudo ecc-server bedrock login")
	} else {
		slog.Info("slow checks call Bedrock", "model", cfg.BedrockModel, "region", cfg.BedrockRegion)
	}

	died := make(chan struct{})
	go s.watchLlama(died)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("listening", "addr", cfg.Listen)
	select {
	case err = <-errc:
	case <-died:
		err = errors.New("llama-server exited; stopping so the service manager restarts the host")
	case sig := <-stop:
		slog.Info("stopping", "signal", sig.String())
	}
	stopped()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	s.mu.Lock()
	m := s.cur
	s.mu.Unlock()
	if m != nil {
		m.llama.stop()
	}
	return err
}

// watchLlama closes died when the serving llama-server exits on its own. The windows live in the host, but the
// host does not restart llama-server itself: it exits and the service manager restarts it.
func (s *server) watchLlama(died chan<- struct{}) {
	for {
		s.mu.Lock()
		m := s.cur
		s.mu.Unlock()
		if m == nil {
			time.Sleep(time.Second)
			continue
		}
		<-m.llama.exited
		s.mu.Lock()
		swapped := s.cur != m
		s.mu.Unlock()
		if !swapped {
			close(died)
			return
		}
	}
}

func (s *server) dropIdle() {
	for range time.Tick(time.Minute) {
		s.mu.Lock()
		for id, c := range s.convs {
			if c.pending == 0 && time.Since(c.last) >= idleWindow {
				delete(s.convs, id)
			}
		}
		s.mu.Unlock()
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *server) handleScore(w http.ResponseWriter, r *http.Request) {
	var req scoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Conversation == "" {
		writeError(w, http.StatusBadRequest, errors.New("expected {\"conversation\", \"speaker\", \"text\"}"))
		return
	}
	s.mu.Lock()
	if s.cur == nil {
		s.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, errors.New("no bundle is being served"))
		return
	}
	c := s.convs[req.Conversation]
	if c == nil {
		c = &conversation{tail: make(chan struct{})}
		close(c.tail)
		s.convs[req.Conversation] = c
	}
	prev, mine := c.tail, make(chan struct{})
	c.tail = mine
	c.pending++
	s.mu.Unlock()

	<-prev
	s.mu.Lock()
	m := s.cur
	m.inflight.Add(1)
	idle := time.Since(c.last) >= idleWindow
	s.mu.Unlock()
	res, err := func() (*scoreResponse, error) {
		// Released even if score panics, so the conversation's later messages and a bundle switch never wait on it.
		defer func() {
			m.inflight.Done()
			s.mu.Lock()
			c.pending--
			c.last = time.Now()
			s.mu.Unlock()
			close(mine)
		}()
		return s.score(m, c, idle, req)
	}()

	switch {
	case errors.Is(err, errNoAnswer):
		writeError(w, http.StatusUnprocessableEntity, err)
	case err != nil:
		slog.Error("scoring failed", "conversation", req.Conversation, "err", err)
		writeError(w, http.StatusBadGateway, err)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

// score scores the message as its conversation's next line and appends it to the window. A message that fails to
// score is not appended, so Module-ECC's retry of it takes its place in the conversation.
func (s *server) score(m *model, c *conversation, idle bool, req scoreRequest) (*scoreResponse, error) {
	b := m.bundle
	if c.gen != m || idle {
		c.gen, c.win = m, window{used: m.header}
	}
	cost, err := m.count(promptLine(req.Speaker, req.Text))
	if err != nil {
		return nil, err
	}
	c.win.fit(b, m.header, cost)
	prompt, tokens, err := fittedPrompt(b.Prompt.Header, c.win.lines, req.Speaker, req.Text, b.Prompt.WindowTokens, m.count)
	if err != nil {
		return nil, err
	}
	out, err := m.complete(prompt, b.Prompt.TopLogprobs)
	if err != nil {
		return nil, err
	}
	if len(out.Probs) == 0 {
		return nil, errors.New("llama-server returned no probabilities")
	}
	earlier := len(c.win.lines)
	c.win.add(line{speaker: req.Speaker, text: req.Text, cost: cost})

	// log P(ban) - log P(ok) of the exact answer tokens; an answer missing from the top logprobs gets the lowest
	// listed one (Module-ECC evaluate.classifier_margin).
	got := map[int]float64{}
	floor := math.Inf(1)
	for _, t := range out.Probs[0].TopLogprobs {
		got[t.ID] = t.Logprob
		floor = min(floor, t.Logprob)
	}
	lb, okB := got[m.ban]
	lo, okO := got[m.ok]
	if !okB && !okO {
		return nil, errNoAnswer
	}
	if !okB {
		lb = floor
	}
	if !okO {
		lo = floor
	}
	margin := lb - lo
	return &scoreResponse{
		PBan:      1 / (1 + math.Exp(-(b.Platt.A*margin + b.Platt.B))),
		Margin:    margin,
		Version:   b.Version,
		Earlier:   earlier,
		Tokens:    tokens,
		Evaluated: out.Timings.PromptN,
	}, nil
}

func (s *server) handleBundle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	m := s.cur
	s.mu.Unlock()
	if m == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("no bundle is being served"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": m.bundle.Version, "decision": m.bundle.Decision})
}

type health struct {
	Status  string         `json:"status"`
	Version string         `json:"version,omitempty"`
	Wandb   *wandbStatus   `json:"wandb"` // null when no W&B key is set
	Bedrock *bedrockStatus `json:"bedrock"`
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	m := s.cur
	s.mu.Unlock()
	h := health{Status: "ready", Bedrock: s.br.status()}
	if s.upd != nil {
		h.Wandb = s.upd.status()
	}
	code := http.StatusServiceUnavailable
	switch {
	case m == nil:
		h.Status = "no bundle"
	case !m.llama.healthy():
		h.Status, h.Version = "llama-server unhealthy", m.bundle.Version
	default:
		h.Version, code = m.bundle.Version, http.StatusOK
	}
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

// serve switches to an installed bundle without a gap in scoring: it starts and health-checks the new llama-server,
// switches scoring and the served decision parameters over together, then stops the old one once its in-flight
// messages finish. A bundle that fails to start is not activated and the current one keeps serving. The caller holds
// switchMu.
func (s *server) serve(version string, byOperator bool) error {
	s.mu.Lock()
	old := s.cur
	s.mu.Unlock()
	if old != nil && old.bundle.Version == version {
		return nil
	}
	b, err := loadBundle(bundleDir(s.cfg, version))
	if err != nil {
		return err
	}
	m, err := startModel(s.cfg, b)
	if err != nil {
		slog.Error("bundle failed to start; keeping the current one", "version", version, "err", err)
		return err
	}
	if err := activate(s.cfg, version, byOperator); err != nil {
		m.llama.stop()
		return err
	}
	s.mu.Lock()
	s.cur = m
	s.mu.Unlock()
	if old != nil {
		old.inflight.Wait()
		old.llama.stop()
		slog.Info("stopped the previous bundle's llama-server", "version", old.bundle.Version)
	}
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
