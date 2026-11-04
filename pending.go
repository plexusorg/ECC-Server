package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// laterLimit bounds a pending player's later lines; older ones are counted in later_dropped.
const laterLimit = 200

// pendingState is what the slow check of a triggered player needs: the trigger, the decision parameters and bundle
// it was judged by, the triggering conversation's transcript up to and including the trigger, the player's own
// recent messages, and in later the player's own lines said after the trigger.
type pendingState struct {
	Player         string         `json:"player"`
	PlayerUUID     string         `json:"player_uuid"`
	TriggeredAt    string         `json:"triggered_at"`
	Trigger        msgRecord      `json:"trigger"`
	Decision       decisionRecord `json:"decision"`
	Conversation   []msgRecord    `json:"conversation"`
	PlayerMessages []msgRecord    `json:"player_messages"`
	Later          []msgRecord    `json:"later"`
	LaterDropped   int            `json:"later_dropped,omitempty"`
	HeldMs         int64          `json:"held_ms,omitempty"`
	// A verdict of the state's slow check was not sure yet, and staff were sent a notice for it.
	Held bool `json:"held,omitempty"`
}

// decisionRecord is the decision parameters of the bundle that scored the trigger.
type decisionRecord struct {
	Version       string  `json:"version"`
	WindowRule    string  `json:"window_rule"`
	Trigger       float64 `json:"trigger"`
	Clearing      float64 `json:"clearing"`
	Corroboration float64 `json:"corroboration"`
}

func (p *pendingState) copy() *pendingState {
	c := *p
	c.Conversation = slices.Clone(p.Conversation)
	c.PlayerMessages = slices.Clone(p.PlayerMessages)
	c.Later = slices.Clone(p.Later)
	return &c
}

// pendingStore holds the players the fast check triggered on until their slow check ends: in memory, and one file
// per player in <data>/safety/pending/<uuid>.json, rewritten atomically on every change. A pending player is muted.
type pendingStore struct {
	dir string

	mu      sync.Mutex
	entries map[string]*pendingState // by player UUID
	unsaved map[string]bool          // a write or delete that failed, retried by flush
}

func openPending(cfg Config) (*pendingStore, error) {
	p := &pendingStore{dir: filepath.Join(cfg.Data, "safety", "pending"), entries: map[string]*pendingState{},
		unsaved: map[string]bool{}}
	if err := os.MkdirAll(p.dir, 0o750); err != nil {
		return nil, err
	}
	files, err := os.ReadDir(p.dir)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		path := filepath.Join(p.dir, f.Name())
		uuid, ok := strings.CutSuffix(f.Name(), ".json")
		if !ok {
			// a write a stopped process left unfinished; the file it was replacing holds the last saved state
			if strings.HasSuffix(f.Name(), ".tmp") {
				os.Remove(path)
			}
			continue
		}
		var st pendingState
		if err := readJSON(path, &st); err != nil {
			return nil, fmt.Errorf("pending state: %w", err)
		}
		if st.PlayerUUID != uuid || st.Later == nil {
			return nil, fmt.Errorf("pending state %s is not the state of player %s with a later list", path, uuid)
		}
		p.entries[uuid] = &st
	}
	return p, nil
}

func (p *pendingStore) contains(uuid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.entries[uuid] != nil
}

type player struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

func (p *pendingStore) players() []player {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]player, 0, len(p.entries))
	for uuid, st := range p.entries {
		out = append(out, player{uuid, st.Player})
	}
	return out
}

// snapshot is a copy of the player's state, or nil when they are not pending.
func (p *pendingStore) snapshot(uuid string) *pendingState {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st := p.entries[uuid]; st != nil {
		return st.copy()
	}
	return nil
}

// add records a trigger; false when the player is already pending.
func (p *pendingStore) add(st *pendingState) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries[st.PlayerUUID] != nil {
		return false
	}
	p.entries[st.PlayerUUID] = st
	p.save(st.PlayerUUID)
	return true
}

// addLater adds a line the player said after the trigger.
func (p *pendingStore) addLater(uuid string, line msgRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.entries[uuid]
	if st == nil {
		return
	}
	st.Later = append(st.Later, line)
	if len(st.Later) > laterLimit {
		st.Later = slices.Delete(st.Later, 0, 1)
		st.LaterDropped++
	}
	p.save(uuid)
}

// setHeld records a verdict that is not sure yet and the time held so far; true at the state's first such verdict.
func (p *pendingStore) setHeld(uuid string, held time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.entries[uuid]
	if st == nil {
		return false
	}
	first := !st.Held
	st.Held, st.HeldMs = true, held.Milliseconds()
	p.save(uuid)
	return first
}

// remove ends the player's pending state, which lifts their mute unless a ban holds it until the kick.
func (p *pendingStore) remove(uuid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries[uuid] != nil {
		delete(p.entries, uuid)
		p.save(uuid)
	}
}

// flush retries every write or delete that failed.
func (p *pendingStore) flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for uuid := range p.unsaved {
		p.save(uuid)
	}
}

// save writes the player's state, or deletes their file when they are no longer pending. The caller holds mu.
func (p *pendingStore) save(uuid string) {
	path := filepath.Join(p.dir, uuid+".json")
	var err error
	if st := p.entries[uuid]; st != nil {
		data, _ := json.Marshal(st)
		err = writeAtomic(path, data)
	} else if err = os.Remove(path); err == nil {
		syncDir(p.dir)
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		if !p.unsaved[uuid] {
			slog.Error("could not save a pending state; retrying every 15 seconds", "player_uuid", uuid, "err", err)
		}
		p.unsaved[uuid] = true
		return
	}
	if p.unsaved[uuid] {
		slog.Info("saved a pending state that failed to save before", "player_uuid", uuid)
		delete(p.unsaved, uuid)
	}
}
