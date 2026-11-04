package main

import (
	"container/list"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Chat safety: what Module-ECC sends (POST /messages) goes through the flood and duplicate rule to the fast check,
// and a trigger mutes the player and starts their slow check. Module-ECC reads the actions (GET /actions) and the
// mute set (GET /muted).

const (
	repeatWindow = 60 * time.Second
	floodPlayers = 3
	// Transcript kept for a pending state: more than the classifier's largest window (29 earlier lines).
	historyLines = 40
	historyKeys  = 1024
	// Message ids remembered, so a resend after a lost answer is ignored.
	seenIDs = 50000
)

var errClosed = errors.New("the host is stopping")

type repeat struct {
	m    *message
	time time.Time
}

type flood struct {
	first   *message
	time    time.Time
	players map[string]bool
}

type safety struct {
	sc   *scorer
	pend *pendingStore
	acts *actionStore
	slow *slowChecker
	rec  *recordLog

	// Captures run one at a time, so a conversation's transcript and its window take lines in the same order.
	capture sync.Mutex
	seen    map[string]bool
	seenLog []string

	mu      sync.Mutex
	repeats *ordered[repeat] // in time order of their last update, oldest first
	floods  *ordered[*flood]
	convs   *ordered[[]*message] // transcripts by conversation, least recently used first
	players *ordered[[]*message] // transcripts by player UUID
	closed  bool
}

func openSafety(s *server, cfg Config, rec *recordLog) (*safety, error) {
	pend, err := openPending(cfg)
	if err != nil {
		return nil, err
	}
	acts, err := openActions(cfg, rec, pend.contains)
	if err != nil {
		return nil, err
	}
	sf := &safety{pend: pend, acts: acts, rec: rec, seen: map[string]bool{}, repeats: newOrdered[repeat](),
		floods: newOrdered[*flood](), convs: newOrdered[[]*message](), players: newOrdered[[]*message]()}
	sf.slow = newSlowChecker(s, pend, acts, rec, cfg.HeldCap)
	sf.sc = newScorer(s, cfg.QueueSize, sf.finish)
	for _, p := range pend.players() {
		sf.slow.check(p.UUID, p.Name)
	}
	return sf, nil
}

// close records every message not yet scored as unscored and stops the slow checks; pending state and actions are
// already saved.
func (sf *safety) close() {
	sf.capture.Lock()
	sf.mu.Lock()
	sf.closed = true
	sf.mu.Unlock()
	sf.capture.Unlock()
	sf.sc.close()
	sf.slow.close()
}

// accept takes one message from Module-ECC. A shadowed message reached only its author: it is recorded, kept for a
// pending player's slow check, and neither scored nor added to a window or a shared transcript. A bypass (staff)
// message enters its conversation's window and transcript unscored. Every other message is scored.
func (sf *safety) accept(m *message) error {
	sf.capture.Lock()
	defer sf.capture.Unlock()
	sf.mu.Lock()
	closed := sf.closed
	sf.mu.Unlock()
	if closed {
		return errClosed
	}
	if sf.seen[m.id] {
		return nil
	}
	sf.seen[m.id] = true
	sf.seenLog = append(sf.seenLog, m.id)
	if len(sf.seenLog) > seenIDs {
		delete(sf.seen, sf.seenLog[0])
		sf.seenLog = sf.seenLog[1:]
	}
	if strings.TrimSpace(m.text) == "" {
		return nil
	}
	m.pendingAtCapture = sf.pend.contains(m.speakerUUID)
	switch {
	case m.shadowed:
		sf.finish(m, verdict{decision: decisionShadowed})
	case m.bypass:
		sf.mu.Lock()
		sf.remember(m)
		sf.mu.Unlock()
		sf.sc.windowOnly(m)
		sf.finish(m, verdict{decision: decisionBypass})
	default:
		sf.admit(m)
	}
	// A pending player's line, shadowed or said before their mute took effect, in the order said: it can add
	// evidence, never clear.
	if m.shadowed || m.pendingAtCapture {
		sf.pend.addLater(m.speakerUUID, m.record())
		sf.slow.check(m.speakerUUID, m.speaker)
	}
	return nil
}

// admit applies the flood and duplicate rule, then queues the message to be scored.
func (sf *safety) admit(m *message) {
	sf.mu.Lock()
	sf.remember(m)
	sf.expire(m.time)
	key := m.speakerUUID + "\x1f" + m.conversation
	var original *message
	rule := "flood"
	if r, ok := sf.repeats.remove(key); ok && r.m.text == m.text {
		// A player's exact repeat within 60 seconds of their previous copy reuses that copy's verdict.
		sf.repeats.put(key, repeat{r.m, m.time})
		original, rule = r.m, "repeat"
	} else {
		sf.repeats.put(key, repeat{m, m.time})
		original = sf.flood(m)
	}
	sf.mu.Unlock()
	if original == nil {
		sf.sc.submit(m)
		return
	}
	// Other players saw the copy, so it enters the window, unscored.
	sf.sc.windowOnly(m)
	original.onVerdict(func(v verdict) { sf.finish(m, v.reusedBy(rule, original.id)) })
}

// flood: when three or more players send the same text within 60 seconds of its first copy, later copies reuse the
// first copy's verdict. It returns that first copy, or nil when this message is scored itself. The caller holds mu.
func (sf *safety) flood(m *message) *message {
	key := duplicateKey(m.text)
	if key == "" {
		return nil
	}
	f, ok := sf.floods.get(key)
	if !ok {
		sf.floods.put(key, &flood{m, m.time, map[string]bool{m.speakerUUID: true}})
		return nil
	}
	f.players[m.speakerUUID] = true
	if len(f.players) >= floodPlayers && f.first.speakerUUID != m.speakerUUID {
		return f.first
	}
	return nil
}

// expire forgets repeats and floods older than the repeat window. The caller holds mu.
func (sf *safety) expire(now time.Time) {
	for _, r, ok := sf.repeats.front(); ok && now.Sub(r.time) > repeatWindow; _, r, ok = sf.repeats.front() {
		sf.repeats.popFront()
	}
	for _, f, ok := sf.floods.front(); ok && now.Sub(f.time) > repeatWindow; _, f, ok = sf.floods.front() {
		sf.floods.popFront()
	}
}

// remember keeps the message in its conversation's and its author's transcript for a later pending state, and gives
// it its conversation as it stands now, so a trigger scored late still sees the lines said before it and none after.
// The caller holds mu.
func (sf *safety) remember(m *message) {
	conv := appendHistory(sf.convs, m.conversation, m)
	appendHistory(sf.players, m.speakerUUID, m)
	m.mu.Lock()
	m.context = slices.Clone(conv)
	m.mu.Unlock()
}

func appendHistory(o *ordered[[]*message], key string, m *message) []*message {
	lines, _ := o.remove(key)
	lines = append(lines, m)
	if len(lines) > historyLines {
		lines = slices.Delete(lines, 0, 1)
	}
	o.put(key, lines)
	if o.len() > historyKeys {
		o.popFront()
	}
	return lines
}

type decisionLine struct {
	TS               string  `json:"ts"`
	Decision         string  `json:"decision"`
	Speaker          string  `json:"speaker"`
	SpeakerUUID      string  `json:"speaker_uuid"`
	MessageID        string  `json:"message_id"`
	PBan             float64 `json:"p_ban"`
	Version          string  `json:"version"`
	TriggerThreshold float64 `json:"trigger_threshold"`
}

// finish records the message with its verdict and triggers on it.
func (sf *safety) finish(m *message, v verdict) {
	waiters, ok := m.complete(v)
	if !ok {
		return
	}
	context := m.takeContext()
	sf.rec.append("message", m.record())
	if v.decision == decisionTrigger {
		if context == nil {
			context = []*message{m}
		}
		sf.trigger(m, v, context)
	}
	for _, w := range waiters {
		w(v)
	}
}

// trigger mutes the player and saves what their slow check needs: the conversation up to and including the trigger,
// and in later the player's own lines said after it.
func (sf *safety) trigger(m *message, v verdict, context []*message) {
	sf.mu.Lock()
	closed := sf.closed
	sf.mu.Unlock()
	// A player awaiting a ban's kick is muted and banned already.
	if closed || v.score == nil || sf.pend.contains(m.speakerUUID) || sf.acts.awaitingKickOf(m.speakerUUID) {
		return
	}
	now := time.Now()
	d := v.score.decision
	st := &pendingState{Player: m.speaker, PlayerUUID: m.speakerUUID, TriggeredAt: stamp(now), Trigger: m.record(),
		Decision: decisionRecord{Version: v.score.version, WindowRule: d.WindowRule, Trigger: *d.Trigger,
			Clearing: *d.Clearing, Corroboration: *d.Corroboration}, Conversation: records(context)}
	sf.mu.Lock()
	history, _ := sf.players.get(m.speakerUUID)
	after := sf.saidAfter(m)
	sf.mu.Unlock()
	st.PlayerMessages, st.Later = records(history), records(after)
	if !sf.pend.add(st) {
		return
	}
	sf.rec.append("decision", decisionLine{TS: stamp(now), Decision: decisionTrigger, Speaker: m.speaker,
		SpeakerUUID: m.speakerUUID, MessageID: m.id, PBan: v.score.pBan, Version: v.score.version,
		TriggerThreshold: *d.Trigger})
	sf.acts.mute(m.speakerUUID, m.speaker)
	sf.slow.check(m.speakerUUID, m.speaker)
}

// saidAfter is the trigger's author's lines in its conversation said after it; all of their lines when the trigger
// has already left the kept transcript, since every line kept then is newer. The caller holds mu.
func (sf *safety) saidAfter(trigger *message) []*message {
	lines, _ := sf.convs.get(trigger.conversation)
	after := !slices.Contains(lines, trigger)
	var out []*message
	for _, l := range lines {
		if l == trigger {
			after = true
		} else if after && l.speakerUUID == trigger.speakerUUID {
			out = append(out, l)
		}
	}
	return out
}

// muted is every pending player and every banned player not yet kicked.
func (sf *safety) muted() []player {
	seen := map[string]bool{}
	out := []player{} // the wire format is an array also when nobody is muted; a nil slice encodes as null
	for _, p := range append(sf.pend.players(), sf.acts.awaitingKick()...) {
		if !seen[p.UUID] {
			seen[p.UUID] = true
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b player) int { return strings.Compare(a.UUID, b.UUID) })
	return out
}

// ---------------------------------------------------------------- HTTP

func (s *server) handleMessages(w http.ResponseWriter, r *http.Request) {
	var wm wireMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&wm); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	m, err := wm.message()
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.safety.accept(m); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleActions answers with every unacknowledged action, oldest first, as soon as there is one, or with none after
// 30 seconds.
func (s *server) handleActions(w http.ResponseWriter, r *http.Request) {
	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()
	for {
		acts, wake := s.safety.acts.unacknowledged()
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
	s.safety.acts.ack(req.ID, *req.OK, req.Detail)
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *server) handleMuted(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.safety.muted())
}

// ---------------------------------------------------------------- ordered map

// ordered is a map that keeps its keys in insertion order; put moves a key to the back.
type ordered[V any] struct {
	l *list.List
	m map[string]*list.Element
}

type orderedEntry[V any] struct {
	key string
	val V
}

func newOrdered[V any]() *ordered[V] {
	return &ordered[V]{l: list.New(), m: map[string]*list.Element{}}
}

func (o *ordered[V]) get(key string) (V, bool) {
	if e, ok := o.m[key]; ok {
		return e.Value.(orderedEntry[V]).val, true
	}
	var zero V
	return zero, false
}

func (o *ordered[V]) put(key string, v V) {
	o.remove(key)
	o.m[key] = o.l.PushBack(orderedEntry[V]{key, v})
}

func (o *ordered[V]) remove(key string) (V, bool) {
	v, ok := o.get(key)
	if ok {
		o.l.Remove(o.m[key])
		delete(o.m, key)
	}
	return v, ok
}

func (o *ordered[V]) front() (string, V, bool) {
	if e := o.l.Front(); e != nil {
		en := e.Value.(orderedEntry[V])
		return en.key, en.val, true
	}
	var zero V
	return "", zero, false
}

func (o *ordered[V]) popFront() {
	if e := o.l.Front(); e != nil {
		delete(o.m, e.Value.(orderedEntry[V]).key)
		o.l.Remove(e)
	}
}

func (o *ordered[V]) len() int { return len(o.m) }
