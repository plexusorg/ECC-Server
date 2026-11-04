package main

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// Waiting window-only lines one conversation may hold while scoring is paused; past this its oldest one is
// dropped and never enters the window.
const windowOnlyLimit = 64

// item is one line waiting for its conversation's window: scored, or added to the window only (a staff line, a
// copy that reuses another message's verdict, a line dropped from a full queue).
type item struct {
	m     *message
	score bool
}

// scorer is the fast check: one bounded queue of lines to score per player, served in turn, and one worker that
// scores each line as its conversation's next one. Every line of a conversation reaches the window in the order it
// was said; a window-only line waits only for the lines before it. While no bundle is served or llama-server fails,
// the worker keeps retrying the line it holds and the others wait.
type scorer struct {
	s      *server
	finish func(*message, verdict)
	size   int
	ctx    context.Context
	stop   context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	cond   *sync.Cond
	queues map[string][]*item // lines to score by player UUID
	turns  []string           // players with lines to score, in turn
	convs  map[string][]*item // every waiting line by conversation, in the order said
	ready  []string           // conversations whose first waiting line is window-only
	isRdy  map[string]bool
	scored int // lines to score waiting
	held   *item
	closed bool
	down   bool // scoring is paused
	reason string
}

func newScorer(s *server, size int, finish func(*message, verdict)) *scorer {
	ctx, stop := context.WithCancel(context.Background())
	sc := &scorer{s: s, finish: finish, size: size, ctx: ctx, stop: stop, done: make(chan struct{}),
		queues: map[string][]*item{}, convs: map[string][]*item{}, isRdy: map[string]bool{}}
	sc.cond = sync.NewCond(&sc.mu)
	go sc.run()
	return sc
}

// submit queues a line to score. When the player's queue is full their oldest waiting line is dropped from scoring;
// it still enters the window in its place.
func (sc *scorer) submit(m *message) {
	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		sc.finish(m, verdict{decision: decisionUnscored})
		return
	}
	it := &item{m: m, score: true}
	q := sc.queues[m.speakerUUID]
	var dropped *item
	if len(q) == 0 {
		sc.turns = append(sc.turns, m.speakerUUID)
	} else if len(q) >= sc.size {
		dropped, q = q[0], q[1:]
		dropped.score = false
		sc.scored--
		sc.markReady(dropped.m.conversation)
	}
	sc.queues[m.speakerUUID] = append(q, it)
	sc.convs[m.conversation] = append(sc.convs[m.conversation], it)
	sc.scored++
	sc.cond.Broadcast()
	sc.mu.Unlock()
	if dropped != nil {
		sc.finish(dropped.m, verdict{decision: decisionDropped})
	}
}

// windowOnly queues a line that enters its conversation's window unscored.
func (sc *scorer) windowOnly(m *message) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.closed {
		return
	}
	l := append(sc.convs[m.conversation], &item{m: m})
	waiting := 0
	for _, it := range l {
		if !it.score {
			waiting++
		}
	}
	if waiting > windowOnlyLimit {
		i := slices.IndexFunc(l, func(it *item) bool { return !it.score })
		l = slices.Delete(l, i, i+1)
	}
	sc.convs[m.conversation] = l
	sc.markReady(m.conversation)
	sc.cond.Broadcast()
}

// markReady lists a conversation whose first waiting line is window-only. The caller holds mu.
func (sc *scorer) markReady(conv string) {
	if l := sc.convs[conv]; len(l) > 0 && !l[0].score && !sc.isRdy[conv] {
		sc.isRdy[conv] = true
		sc.ready = append(sc.ready, conv)
	}
}

// popFirst takes a conversation's first waiting line. The caller holds mu.
func (sc *scorer) popFirst(conv string) *item {
	l := sc.convs[conv]
	it := l[0]
	if len(l) == 1 {
		delete(sc.convs, conv)
	} else {
		sc.convs[conv] = l[1:]
		sc.markReady(conv)
	}
	return it
}

// take returns the next line: a window-only line at the front of its conversation, otherwise the line of the player
// whose turn it is, after the lines said before it in that conversation. Nil once the scorer is closed.
func (sc *scorer) take() *item {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for {
		if sc.closed {
			return nil
		}
		for len(sc.ready) > 0 {
			conv := sc.ready[0]
			sc.ready = sc.ready[1:]
			delete(sc.isRdy, conv)
			if l := sc.convs[conv]; len(l) > 0 && !l[0].score {
				sc.held = sc.popFirst(conv)
				return sc.held
			}
		}
		if len(sc.turns) > 0 {
			player := sc.turns[0]
			sc.turns = sc.turns[1:]
			it := sc.popFirst(sc.queues[player][0].m.conversation)
			// ready is empty, so the conversation's first waiting line is one to score
			sc.held = it
			author := it.m.speakerUUID
			q := slices.DeleteFunc(sc.queues[author], func(x *item) bool { return x == it })
			if len(q) == 0 {
				delete(sc.queues, author)
				sc.turns = slices.DeleteFunc(sc.turns, func(p string) bool { return p == author })
			} else {
				sc.queues[author] = q
			}
			// The player whose turn it was keeps it when an earlier line of their conversation was served instead.
			if author != player {
				sc.turns = append([]string{player}, sc.turns...)
			} else if len(q) > 0 {
				sc.turns = append(sc.turns, player)
			}
			sc.scored--
			return it
		}
		sc.cond.Wait()
	}
}

func (sc *scorer) run() {
	defer close(sc.done)
	for {
		it := sc.take()
		if it == nil {
			return
		}
		v, ok := sc.process(it)
		sc.mu.Lock()
		sc.held = nil
		sc.mu.Unlock()
		if !ok {
			if it.score {
				sc.finish(it.m, verdict{decision: decisionUnscored})
			}
			return
		}
		if it.score {
			sc.finish(it.m, v)
		}
	}
}

// process scores the line, or adds it to the window, retrying while no bundle is served or llama-server fails: a
// line that failed was not added to the window. False when the scorer closed first.
func (sc *scorer) process(it *item) (verdict, bool) {
	retry := time.Second
	for {
		var v verdict
		err := sc.s.inTurn(it.m.conversation, it.m.time, func(m *model, c *conversation, idle bool) error {
			if !it.score {
				return sc.s.appendLine(m, c, idle, it.m.speaker, it.m.text)
			}
			res, err := sc.s.score(m, c, idle, scoreRequest{Conversation: it.m.conversation, Speaker: it.m.speaker,
				Text: it.m.text})
			if err != nil {
				return err
			}
			d := m.bundle.Decision
			v = verdict{decision: decisionOK, score: &score{pBan: res.PBan, margin: res.Margin, version: res.Version,
				decision: d}}
			if res.PBan >= *d.Trigger {
				v.decision = decisionTrigger
			}
			return nil
		})
		if err == nil || errors.Is(err, errNoAnswer) {
			sc.up()
			if err != nil {
				slog.Warn("a message has no score: neither answer token is among the top logprobs", "message_id", it.m.id)
				v = verdict{decision: decisionNoScore}
			}
			return v, true
		}
		sc.pause(err)
		select {
		case <-sc.ctx.Done():
			return verdict{}, false
		case <-time.After(retry):
		}
		retry = min(retry*2, 30*time.Second)
	}
}

func (sc *scorer) pause(err error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if !sc.down {
		slog.Warn("scoring paused; messages wait in their queues and are scored in order when it resumes", "err", err)
	}
	sc.down, sc.reason = true, err.Error()
}

func (sc *scorer) up() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.down {
		slog.Info("scoring resumed", "waiting", sc.scored)
	}
	sc.down, sc.reason = false, ""
}

// state is the backlog (lines waiting to be scored, and the one being scored) and why scoring is paused, or "".
func (sc *scorer) state() (int, string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	n := sc.scored
	if sc.held != nil && sc.held.score {
		n++
	}
	return n, sc.reason
}

// close stops the worker and records every line it did not score as unscored.
func (sc *scorer) close() {
	sc.mu.Lock()
	sc.closed = true
	var unscored []*message
	for _, q := range sc.queues {
		for _, it := range q {
			unscored = append(unscored, it.m)
		}
	}
	sc.queues, sc.convs, sc.turns, sc.ready, sc.isRdy, sc.scored = nil, nil, nil, nil, nil, 0
	sc.cond.Broadcast()
	sc.mu.Unlock()
	sc.stop()
	select {
	case <-sc.done:
	case <-time.After(5 * time.Second):
		slog.Warn("the scorer did not stop within 5 seconds; the message it holds is not recorded")
	}
	for _, m := range unscored {
		sc.finish(m, verdict{decision: decisionUnscored})
	}
}
