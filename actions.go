package main

import (
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// What Module-ECC is told to do, in the order the actions were made.
const (
	actionMute   = "mute"
	actionUnmute = "unmute"
	actionBan    = "ban"
)

// action is one action as GET /actions returns it. Role and KickAt are set on a ban only.
type action struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	UUID   string `json:"uuid"`
	Name   string `json:"name"`
	Role   string `json:"role,omitempty"`
	KickAt string `json:"kick_at,omitempty"`
}

// kick is a banned player who stays muted until kickAt, when they are unmuted.
type kick struct {
	UUID   string    `json:"uuid"`
	Name   string    `json:"name"`
	KickAt time.Time `json:"kick_at"`
}

// actionStore holds every action Module-ECC has not acknowledged, and the banned players awaiting their kick, in
// <data>/safety/actions.json, rewritten atomically on every change, so a restart of either program resends what was
// not done.
type actionStore struct {
	path             string
	rec              *recordLog
	kickMin, kickMax time.Duration
	pending          func(uuid string) bool // a pending player stays muted after a kick time

	mu      sync.Mutex
	actions []action
	kicks   []kick
	wake    chan struct{} // closed and replaced when an action is added
	unsaved bool
}

func openActions(cfg Config, rec *recordLog, pending func(uuid string) bool) (*actionStore, error) {
	a := &actionStore{path: filepath.Join(cfg.Data, "safety", "actions.json"), rec: rec, kickMin: cfg.KickDelayMin,
		kickMax: cfg.KickDelayMax, pending: pending, wake: make(chan struct{})}
	var saved struct {
		Actions []action `json:"actions"`
		Kicks   []kick   `json:"kicks"`
	}
	if err := readJSON(a.path, &saved); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("actions: %w", err)
	}
	a.actions, a.kicks = saved.Actions, saved.Kicks
	for _, k := range a.kicks {
		time.AfterFunc(time.Until(k.KickAt), a.kicked)
	}
	return a, nil
}

// kicked unmutes every banned player whose kick time has come.
func (a *actionStore) kicked() {
	now := time.Now()
	a.mu.Lock()
	var due []kick
	a.kicks = slices.DeleteFunc(a.kicks, func(k kick) bool {
		if now.Before(k.KickAt) {
			return false
		}
		due = append(due, k)
		return true
	})
	a.mu.Unlock()
	for _, k := range due {
		if !a.pending(k.UUID) {
			a.emit(action{Type: actionUnmute, UUID: k.UUID, Name: k.Name}, "", "kicked", "")
		}
	}
}

// unacknowledged returns every action not yet acknowledged, oldest first, and a channel closed when one is added.
func (a *actionStore) unacknowledged() ([]action, <-chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.actions), a.wake
}

func (a *actionStore) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.actions)
}

// awaitingKick lists the banned players whose kick time has not passed.
func (a *actionStore) awaitingKick() []player {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []player
	for _, k := range a.kicks {
		if time.Now().Before(k.KickAt) {
			out = append(out, player{k.UUID, k.Name})
		}
	}
	return out
}

// actionLine is an action's record line. Cause and Detail say why: the verdict's cause, or for an unmute after the
// held cap how long the player was held.
type actionLine struct {
	TS         string `json:"ts"`
	ID         string `json:"id,omitempty"`
	Action     string `json:"action"`
	Player     string `json:"player"`
	PlayerUUID string `json:"player_uuid"`
	Role       string `json:"role,omitempty"`
	Cause      string `json:"cause,omitempty"`
	KickAt     string `json:"kick_at,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

func (a *actionStore) mute(uuid, name string) {
	a.emit(action{Type: actionMute, UUID: uuid, Name: name}, "", "", "")
}

func (a *actionStore) unmute(uuid, name, role, cause, detail string) {
	a.emit(action{Type: actionUnmute, UUID: uuid, Name: name}, role, cause, detail)
}

// ban bans an offender or a player at risk: Module-ECC writes the ban entry and kicks them at a random time in the
// kick delay range; they stay muted until then. A player banned already and not yet kicked is not banned again.
func (a *actionStore) ban(uuid, name, role, cause string) {
	now := time.Now()
	a.mu.Lock()
	for _, k := range a.kicks {
		if k.UUID == uuid && now.Before(k.KickAt) {
			a.mu.Unlock()
			a.rec.append("action", actionLine{TS: stamp(now), Action: "already_banned", Player: name, PlayerUUID: uuid,
				Role: role, Cause: cause})
			return
		}
	}
	at := now.Add(a.kickMin + rand.N(a.kickMax-a.kickMin+time.Millisecond)).Truncate(time.Millisecond)
	a.kicks = append(a.kicks, kick{uuid, name, at})
	a.mu.Unlock()
	time.AfterFunc(time.Until(at), a.kicked)
	a.emit(action{Type: actionBan, UUID: uuid, Name: name, Role: role, KickAt: stamp(at)}, role, cause, "")
}

func (a *actionStore) emit(act action, role, cause, detail string) {
	act.ID = newUUID()
	a.mu.Lock()
	a.actions = append(a.actions, act)
	a.save()
	close(a.wake)
	a.wake = make(chan struct{})
	a.mu.Unlock()
	a.rec.append("action", actionLine{TS: stamp(time.Now()), ID: act.ID, Action: act.Type, Player: act.Name,
		PlayerUUID: act.UUID, Role: role, Cause: cause, KickAt: act.KickAt, Detail: detail})
}

type ackLine struct {
	TS         string `json:"ts"`
	ID         string `json:"id"`
	OK         bool   `json:"ok"`
	Detail     string `json:"detail,omitempty"`
	Known      bool   `json:"known"` // an unacknowledged action has this id
	Action     string `json:"action,omitempty"`
	PlayerUUID string `json:"player_uuid,omitempty"`
}

// ack records Module-ECC's acknowledgement. Only a successful one ends the action; after a failed one the action
// stays and is served again, and Module-ECC retries it. A repeated or unknown id is recorded and changes nothing.
func (a *actionStore) ack(id string, ok bool, detail string) {
	line := ackLine{TS: stamp(time.Now()), ID: id, OK: ok, Detail: detail}
	a.mu.Lock()
	if i := slices.IndexFunc(a.actions, func(act action) bool { return act.ID == id }); i >= 0 {
		line.Known, line.Action, line.PlayerUUID = true, a.actions[i].Type, a.actions[i].UUID
		if ok {
			a.actions = slices.Delete(a.actions, i, i+1)
			a.save()
		}
	}
	a.mu.Unlock()
	a.rec.append("ack", line)
}

// flush retries a write that failed.
func (a *actionStore) flush() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.unsaved {
		a.save()
	}
}

// save writes the actions and kicks. A failed write keeps them in memory, where GET /actions still serves them,
// and is retried every 15 seconds. The caller holds mu.
func (a *actionStore) save() {
	data, _ := json.Marshal(struct {
		Actions []action `json:"actions"`
		Kicks   []kick   `json:"kicks"`
	}{a.actions, a.kicks})
	if err := writeAtomic(a.path, data); err != nil {
		if !a.unsaved {
			slog.Error("could not save the actions; retrying every 15 seconds", "err", err)
		}
		a.unsaved = true
		return
	}
	if a.unsaved {
		slog.Info("saved the actions that failed to save before")
		a.unsaved = false
	}
}

// newUUID is a random (version 4) UUID.
func newUUID() string {
	var b [16]byte
	crand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
