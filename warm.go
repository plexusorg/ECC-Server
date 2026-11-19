package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Bedrock keeps the judge's cached prompt part (the question and the owner's cases) for at least 30 minutes after it
// was last used, and a call that finds it gone writes all of it to the cache again at the cache-write price instead of
// reading it: about $0.36 more for 138,000 tokens on the flex tier. So after a judge call the host keeps that part
// cached with a warming call every warmEvery: the cached part, a one-line ask, and warmMaxTokens of output, about
// $0.015. Warming stops once the warming calls since the last judge call have cost what one call that finds the cache
// gone pays over a cached one, and starts again with the next judge call, so a quiet stretch costs at most twice what
// knowing its length in advance would.
const (
	warmEvery     = 25 * time.Minute
	warmMaxTokens = 16
	warmAsk       = "\n\nNo new case. Answer OK."
)

// warmer keeps the judge's cached prompt part warm between judge calls.
type warmer struct {
	j      *bedrockJudge
	wake   chan struct{}
	mu     sync.Mutex
	prefix string    // the cached part of the last judge call's prompt
	next   time.Time // when the next warming call is due
	worth  float64   // what a call that finds the cache gone pays over a cached one
	spent  float64   // what warming has cost since the last judge call
}

func newWarmer(j *bedrockJudge) *warmer {
	return &warmer{j: j, wake: make(chan struct{}, 1)}
}

// used tells the warmer a judge call was answered: prefix is its prompt's cached part, u its usage.
func (w *warmer) used(prefix string, u tokenUsage) {
	s := w.j.spend
	w.mu.Lock()
	// The cached part's tokens: what Bedrock read from the cache, or, on a call that found it gone, the whole input.
	tokens := u.Cached
	if tokens == 0 {
		tokens = u.Input
	}
	w.prefix, w.next, w.spent = prefix, time.Now().Add(warmEvery), 0
	w.worth = float64(tokens) * (max(s.in, s.cw) - s.cr)
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// loop makes the warming calls until the host stops.
func (w *warmer) loop(ctx context.Context) {
	t := time.NewTimer(time.Hour)
	t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-t.C:
			w.warm(ctx)
		}
		w.mu.Lock()
		due, on := w.next, w.prefix != "" && w.spent < w.worth
		w.mu.Unlock()
		t.Stop()
		if on {
			t.Reset(time.Until(due))
		}
	}
}

// warmCall records a warming call in the private record.
type warmCall struct {
	Time        string      `json:"time"`
	PromptBytes int         `json:"prompt_bytes"`
	Status      string      `json:"status,omitempty"`
	Usage       *tokenUsage `json:"usage,omitempty"`
	USD         float64     `json:"usd"`
	Seconds     float64     `json:"seconds"`
	Error       string      `json:"error,omitempty"`
}

// warm makes one warming call when one is due and warming is still worth its cost; the next is due warmEvery later.
// The call is charged to the budget, but a budget it does not fit leaves it unmade without telling staff.
func (w *warmer) warm(ctx context.Context) {
	w.mu.Lock()
	prefix, next, left := w.prefix, w.next, w.worth-w.spent
	if prefix == "" || left <= 0 || time.Now().Before(next) {
		w.mu.Unlock()
		return
	}
	w.next = time.Now().Add(warmEvery)
	w.mu.Unlock()
	prompt := prefix + warmAsk
	sp := w.j.spend
	r, err := sp.reserveQuiet(sp.worst(len(prompt), warmMaxTokens))
	if err != nil {
		w.mu.Lock()
		w.spent = w.worth // over budget: wait for the next judge call
		w.mu.Unlock()
		return
	}
	cctx, cancel := context.WithTimeout(ctx, judgeTimeout)
	defer cancel()
	start := time.Now()
	res, err := w.j.br.respond(cctx, prompt, len(prefix), warmMaxTokens, judgeReasoningEffort, judgeServiceTier)
	cost := r.usd
	var be *bedrockError
	switch {
	case res != nil:
		cost = sp.cost(res.Usage)
	case errors.As(err, &be) && be.Status != 200:
		cost = 0
	}
	sp.settle(r, cost)
	rec := warmCall{Time: stamp(time.Now()), PromptBytes: len(prompt), USD: cost, Seconds: time.Since(start).Seconds()}
	w.mu.Lock()
	if w.prefix == prefix { // a judge call in the meantime starts a new count
		w.spent += cost
	}
	w.mu.Unlock()
	if res != nil {
		rec.Status, rec.Usage = res.Status, &res.Usage
		slog.Info("warming call", "cached_tokens", res.Usage.Cached, "input_tokens", res.Usage.Input,
			"usd", cost)
	} else {
		rec.Error = err.Error()
		slog.Warn("warming call failed", "err", err.Error(), "usd", cost)
		if !passing(err) {
			w.mu.Lock()
			w.spent = w.worth // refused: wait for the next judge call
			w.mu.Unlock()
		}
	}
	w.j.record("judge_warm", rec)
}
