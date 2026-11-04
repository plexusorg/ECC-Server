package main

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// Decisions a message's record line carries.
const (
	decisionOK      = "ok"
	decisionTrigger = "trigger"
	// recorded without a score
	decisionShadowed = "shadowed"
	decisionBypass   = "bypass"
	decisionDropped  = "dropped"  // the player's queue was full
	decisionUnscored = "unscored" // still waiting when the host stopped
	decisionNoScore  = "no_score" // scored, but neither answer token was among the top logprobs
)

// wireMessage is one message as POST /messages carries it.
type wireMessage struct {
	ID           string  `json:"id"`
	Time         string  `json:"time"`
	Channel      string  `json:"channel"`
	Conversation string  `json:"conversation"`
	SpeakerUUID  string  `json:"speaker_uuid"`
	Speaker      string  `json:"speaker"`
	TargetUUID   *string `json:"target_uuid"`
	Target       *string `json:"target"`
	Text         string  `json:"text"`
	Shadowed     bool    `json:"shadowed"`
	Bypass       bool    `json:"bypass"`
}

// canonicalUUID is a UUID in its 8-4-4-4-12 hex form, lowercased.
func canonicalUUID(s string) (string, bool) {
	if len(s) != 36 {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return "", false
			}
		case c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F':
		default:
			return "", false
		}
	}
	return strings.ToLower(s), true
}

// message validates a wire message once, at ingress.
func (w wireMessage) message() (*message, error) {
	id, ok := canonicalUUID(w.ID)
	if !ok {
		return nil, errors.New("id must be a UUID")
	}
	at, err := time.Parse(time.RFC3339Nano, w.Time)
	if err != nil {
		return nil, errors.New("time must be RFC 3339")
	}
	speaker, ok := canonicalUUID(w.SpeakerUUID)
	if !ok {
		return nil, errors.New("speaker_uuid must be a UUID")
	}
	if w.Channel == "" || w.Conversation == "" || w.Speaker == "" {
		return nil, errors.New("channel, conversation and speaker must not be empty")
	}
	m := &message{id: id, time: at.UTC(), channel: w.Channel, conversation: w.Conversation, speakerUUID: speaker,
		speaker: w.Speaker, text: w.Text, shadowed: w.Shadowed, bypass: w.Bypass}
	if w.TargetUUID != nil {
		if m.targetUUID, ok = canonicalUUID(*w.TargetUUID); !ok {
			return nil, errors.New("target_uuid must be a UUID or null")
		}
	}
	if w.Target != nil {
		m.target = *w.Target
	}
	return m, nil
}

// message is one captured message. Its verdict is set once: when it is scored, reuses another message's verdict, or
// is recorded without a score.
type message struct {
	id, channel, conversation string
	speakerUUID, speaker      string
	targetUUID, target        string
	text                      string
	time                      time.Time
	shadowed, bypass          bool
	pendingAtCapture          bool // its speaker was pending when it arrived

	mu      sync.Mutex
	verdict *verdict
	waiters []func(verdict)
	// Its conversation up to and including it, as it stood when it was said; dropped once the message is finished,
	// so finished messages never hold one another.
	context []*message
}

// verdict is what became of a message. score is set when the message was scored or reused a scored message's
// verdict.
type verdict struct {
	decision   string
	score      *score
	rule       string // "repeat" or "flood" for a reused verdict
	reusedFrom string
}

type score struct {
	pBan, margin float64
	version      string
	decision     Decision // of the bundle that scored it
}

func (v verdict) reusedBy(rule, original string) verdict {
	v.rule, v.reusedFrom = rule, original
	return v
}

// complete sets the verdict once and returns the callbacks waiting for it; false when it was already set.
func (m *message) complete(v verdict) ([]func(verdict), bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.verdict != nil {
		return nil, false
	}
	m.verdict = &v
	w := m.waiters
	m.waiters = nil
	return w, true
}

// onVerdict calls f with the verdict, at once when it is set, otherwise when it is.
func (m *message) onVerdict(f func(verdict)) {
	m.mu.Lock()
	if m.verdict == nil {
		m.waiters = append(m.waiters, f)
		m.mu.Unlock()
		return
	}
	v := *m.verdict
	m.mu.Unlock()
	f(v)
}

func (m *message) takeContext() []*message {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.context
	m.context = nil
	return c
}

// msgRecord is a message and, once known, its verdict: the shape of a record line and of a pending state's
// transcript entry.
type msgRecord struct {
	ID           string   `json:"id"`
	TS           string   `json:"ts"`
	Channel      string   `json:"channel"`
	Conversation string   `json:"conversation"`
	Speaker      string   `json:"speaker"`
	SpeakerUUID  string   `json:"speaker_uuid"`
	Target       string   `json:"target,omitempty"`
	TargetUUID   string   `json:"target_uuid,omitempty"`
	Text         string   `json:"text"`
	Shadowed     bool     `json:"shadowed"`
	Bypass       bool     `json:"bypass"`
	Decision     string   `json:"decision,omitempty"`
	PBan         *float64 `json:"p_ban,omitempty"`
	Margin       *float64 `json:"margin,omitempty"`
	Version      string   `json:"version,omitempty"`
	Rule         string   `json:"rule,omitempty"`
	ReusedFrom   string   `json:"reused_from,omitempty"`
}

func (m *message) record() msgRecord {
	r := msgRecord{ID: m.id, TS: stamp(m.time), Channel: m.channel, Conversation: m.conversation, Speaker: m.speaker,
		SpeakerUUID: m.speakerUUID, Target: m.target, TargetUUID: m.targetUUID, Text: m.text, Shadowed: m.shadowed,
		Bypass: m.bypass}
	m.mu.Lock()
	v := m.verdict
	m.mu.Unlock()
	if v != nil {
		r.Decision, r.Rule, r.ReusedFrom = v.decision, v.rule, v.reusedFrom
		if s := v.score; s != nil {
			p, mg := s.pBan, s.margin
			r.PBan, r.Margin, r.Version = &p, &mg, s.version
		}
	}
	return r
}

func records(lines []*message) []msgRecord {
	out := make([]msgRecord, len(lines))
	for i, l := range lines {
		out[i] = l.record()
	}
	return out
}
