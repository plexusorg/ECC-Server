package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"ecc-server/core"
)

// recorder is the core's Recorder: every record line goes to the day's record, and each review entry also to
// <data>/review/queue.jsonl, the owner's review queue.
type recorder struct {
	rec   *recordLog
	queue string
}

func newRecorder(cfg Config, rec *recordLog) (*recorder, error) {
	dir := filepath.Join(cfg.Data, "review")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &recorder{rec: rec, queue: filepath.Join(dir, "queue.jsonl")}, nil
}

func (r *recorder) Record(kind string, v any) {
	r.rec.append(kind, v)
	if kind != "review" {
		return
	}
	line, err := json.Marshal(v)
	if err == nil {
		var f *os.File
		if f, err = os.OpenFile(r.queue, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640); err == nil {
			_, err = f.Write(append(line, '\n'))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err != nil {
		slog.Error("could not write to the review queue; the entry is in the record", "err", err)
	}
}

// restartEnd is the end the restart scan records for a session the previous run left open.
type restartEnd struct {
	Time    string `json:"time"`
	Stream  string `json:"stream"`
	Session string `json:"session"`
	Account string `json:"account"`
	Start   string `json:"start"`
	Held    int    `json:"held"`
	Unruled int    `json:"unruled"`
	Outcome string `json:"outcome"`
	Review  string `json:"review,omitempty"`
	EndedBy string `json:"ended_by"`
}

// scanRestart ends the sessions the previous run left open. It recovers host outcomes and review reasons from the
// last two days' record. Historical rulings without an effective outcome need durable action evidence; otherwise
// their lines stay held for review. The scan records each end and sends no actions.
func scanRestart(r *recorder, dir string, now time.Time) error {
	type open struct {
		start                    map[string]any
		name                     string
		held, ruled, kept, freed int
		banned                   bool
		review                   []string
		// Only a durable host action can establish an old ruling's missing effect.
		legacyPending, legacyUnknown int
		heldLines                    map[string]string
		released                     map[string]bool
	}
	sessions := map[string]*open{}
	var order []string
	for _, day := range []time.Time{now.AddDate(0, 0, -1), now} {
		name := filepath.Join(dir, day.UTC().Format(time.DateOnly)+".jsonl")
		lines, err := readRecordDay(name)
		if err != nil {
			return err
		}
		for _, l := range lines {
			id, _ := l["session"].(string)
			switch l["type"] {
			case "session_start":
				sessions[id] = &open{start: l, heldLines: map[string]string{}, released: map[string]bool{}}
				order = append(order, id)
			case "session_end":
				delete(sessions, id)
			case "line":
				if s := sessions[id]; s != nil {
					s.name, _ = l["name"].(string)
					if l["verdict"] == "hold" {
						s.held++
						line, _ := l["id"].(string)
						s.heldLines[line] = "hold"
					}
				}
			case "rescored":
				if s := sessions[id]; s != nil {
					line, _ := l["id"].(string)
					if _, held := s.heldLines[line]; held {
						verdict, _ := l["verdict"].(string)
						s.heldLines[line] = verdict
					}
				}
			case "ruling":
				if s := sessions[id]; s != nil {
					covers, _ := l["covers"].(float64)
					n := int(covers)
					s.ruled += n
					s.legacyPending = 0
					if why, _ := l["review"].(string); why != "" && !slices.Contains(s.review, why) {
						s.review = append(s.review, why)
					}
					switch l["effective"] {
					case core.RulingHold:
						s.kept += n
					case core.RulingNothing:
						s.freed += n
					case core.RulingBan:
						s.banned = true
					default:
						// A diagnostic answer does not prove that a line was released or a ban applied.
						s.kept += n
						s.legacyPending = n
						s.legacyUnknown += n
					}
				}
			case "action":
				if s := sessions[id]; s != nil {
					switch l["kind"] {
					case core.ActionBan:
						s.banned = true
					case core.ActionRelease:
						lines, _ := l["lines"].([]any)
						n := 0
						for _, value := range lines {
							line, ok := value.(string)
							if !ok || line == "" || s.released[line] {
								continue
							}
							s.released[line] = true
							if s.heldLines[line] == "deliver" {
								// Retire a temporary scoring hold only after its durable release.
								delete(s.heldLines, line)
								s.held--
								s.freed++
							} else {
								n++
							}
						}
						n = min(n, s.legacyPending)
						s.kept -= n
						s.freed += n
						s.legacyPending -= n
						s.legacyUnknown -= n
					case core.ActionWarn:
						s.legacyUnknown -= s.legacyPending
						s.legacyPending = 0
					}
				}
			}
		}
	}
	at := stamp(now)
	for _, id := range order {
		s := sessions[id]
		if s == nil {
			continue
		}
		sessions[id] = nil
		str := func(k string) string { v, _ := s.start[k].(string); return v }
		e := restartEnd{Time: at, Stream: str("stream"), Session: id, Account: str("account"), Start: str("time"),
			Held: s.held, Unruled: s.held - s.ruled, EndedBy: "restart"}
		switch {
		case s.banned:
			e.Outcome = core.OutcomeBanned
		case e.Unruled > 0 || s.kept > 0:
			e.Outcome = core.OutcomeHeld
		case s.freed > 0:
			e.Outcome = core.OutcomeReleased
		default:
			e.Outcome = core.OutcomeNone
		}
		why := slices.Clone(s.review)
		if s.banned {
			why = append(why, "ban")
		} else {
			if e.Unruled > 0 {
				why = append(why, "the server restarted with "+strconv.Itoa(e.Unruled)+" held lines no ruling covered")
			}
			if s.legacyUnknown > 0 {
				why = append(why, "the historical record has no host outcome for "+strconv.Itoa(s.legacyUnknown)+" held lines")
			}
		}
		e.Review = strings.Join(why, "; ")
		r.Record("session_end", e)
		if e.Review != "" {
			r.Record("review", core.ReviewRecord{Time: at, Session: id, Account: e.Account, Name: s.name, Why: e.Review})
			slog.Warn("a session the previous run left open goes on the review queue", "session", id,
				"unruled", e.Unruled)
		}
	}
	return nil
}

// readRecordDay reads one day's record, the open file or its compressed copy; a day with neither has no lines.
func readRecordDay(name string) ([]map[string]any, error) {
	var in io.Reader
	f, err := os.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		f, err = os.Open(name + ".zst")
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		defer f.Close()
		dec, err := zstd.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer dec.Close()
		in = dec
	} else if err != nil {
		return nil, err
	} else {
		defer f.Close()
		in = f
	}
	var out []map[string]any
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		var l map[string]any
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue // a line cut off by a crash
		}
		if t, _ := l["type"].(string); slices.Contains([]string{"session_start", "session_end", "line", "rescored", "ruling", "action"}, t) {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}
