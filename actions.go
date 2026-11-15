package main

import (
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"ecc-server/core"
)

// What Module-ECC is told to do, in the order the actions were made.
const (
	actionBan     = "ban"     // ban the player indefinitely and kick them at once; drop their held lines
	actionWarn    = "warn"    // show the player the warning; their held lines stay held
	actionRelease = "release" // deliver these held lines, in order
	actionNotice  = "notice"  // show staff with the alert permission Text
)

// action is one action as GET /actions returns it. UUID and Account are the player's UUID and account name, as the
// plugin sent them as speaker_uuid and speaker (ban, warn, release); IDs are the plugin's ids of the lines a release
// delivers; Kind and Text are a notice's.
type action struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	UUID    string   `json:"uuid,omitempty"`
	Account string   `json:"account,omitempty"`
	IDs     []string `json:"ids,omitempty"`
	Kind    string   `json:"kind,omitempty"`
	Text    string   `json:"text,omitempty"`
}

// fromCore is the action the plugin carries out for one of the core's; a release whose lines have no plugin id has
// nothing for the plugin to deliver.
func fromCore(a core.Action) (action, bool) {
	out := action{Type: a.Kind, UUID: a.Account, Account: a.Speaker}
	switch a.Kind {
	case core.ActionBan, core.ActionWarn:
	case core.ActionRelease:
		out.IDs = a.Refs
		if len(out.IDs) == 0 {
			return out, false
		}
	default:
		panic("unknown action " + a.Kind)
	}
	return out, true
}

// actionStore holds every action Module-ECC has not acknowledged in <data>/actions.json, rewritten atomically on
// every change, so a restart of either program resends what was not done.
type actionStore struct {
	path string
	rec  *recordLog

	mu      sync.Mutex
	actions []action
	wake    chan struct{} // closed and replaced when an action is added
	unsaved bool
}

func openActions(cfg Config, rec *recordLog) (*actionStore, error) {
	a := &actionStore{path: filepath.Join(cfg.Data, "actions.json"), rec: rec, wake: make(chan struct{})}
	if err := readJSON(a.path, &a.actions); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("actions: %w", err)
	}
	if len(a.actions) > 0 {
		slog.Info("resending the actions Module-ECC has not acknowledged", "count", len(a.actions))
	}
	return a, nil
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

func (a *actionStore) add(act action) {
	act.ID = newUUID()
	a.mu.Lock()
	a.actions = append(a.actions, act)
	a.save()
	close(a.wake)
	a.wake = make(chan struct{})
	a.mu.Unlock()
	a.rec.append("action_sent", struct {
		TS      string   `json:"ts"`
		ID      string   `json:"id"`
		Action  string   `json:"action"`
		UUID    string   `json:"uuid,omitempty"`
		Account string   `json:"account,omitempty"`
		IDs     []string `json:"ids,omitempty"`
		Kind    string   `json:"kind,omitempty"`
	}{stamp(time.Now()), act.ID, act.Type, act.UUID, act.Account, act.IDs, act.Kind})
	slog.Info("action", "type", act.Type, "account", act.Account, "uuid", act.UUID, "lines", len(act.IDs), "kind", act.Kind,
		"id", act.ID)
}

type ackLine struct {
	TS     string `json:"ts"`
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Known  bool   `json:"known"` // an unacknowledged action has this id
	Action string `json:"action,omitempty"`
	UUID   string `json:"uuid,omitempty"`
}

// ack records Module-ECC's acknowledgement. Only a successful one ends the action; after a failed one the action
// stays and is served again, and Module-ECC retries it. A repeated or unknown id is recorded and changes nothing.
func (a *actionStore) ack(id string, ok bool, detail string) {
	line := ackLine{TS: stamp(time.Now()), ID: id, OK: ok, Detail: detail}
	a.mu.Lock()
	if i := slices.IndexFunc(a.actions, func(act action) bool { return act.ID == id }); i >= 0 {
		line.Known, line.Action, line.UUID = true, a.actions[i].Type, a.actions[i].UUID
		if ok {
			a.actions = slices.Delete(a.actions, i, i+1)
			a.save()
		}
	}
	a.mu.Unlock()
	a.rec.append("ack", line)
	slog.Info("acknowledged", "type", line.Action, "uuid", line.UUID, "ok", ok, "id", id, "known", line.Known)
}

// save writes the actions. A failed write keeps them in memory, where GET /actions still serves them, and is retried
// on the next change. The caller holds mu.
func (a *actionStore) save() {
	data, _ := json.Marshal(a.actions)
	if err := writeAtomic(a.path, data); err != nil {
		if !a.unsaved {
			slog.Error("could not save the actions; retrying on the next change", "err", err)
		}
		a.unsaved = true
		return
	}
	a.unsaved = false
}

// newUUID is a random (version 4) UUID.
func newUUID() string {
	var b [16]byte
	crand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
