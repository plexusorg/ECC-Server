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

// restartEnd is the end the restart scan records for a sitting the previous run left open.
type restartEnd struct {
	Time    string `json:"time"`
	Stream  string `json:"stream"`
	Sitting string `json:"sitting"`
	Account string `json:"account"`
	Start   string `json:"start"`
	Held    int    `json:"held"`
	Unruled int    `json:"unruled"`
	Outcome string `json:"outcome"`
	Review  string `json:"review,omitempty"`
	EndedBy string `json:"ended_by"`
}

// scanRestart ends the sittings the previous run left open: it reads the last two days' record for a sitting_start
// with no sitting_end, puts each one with held lines no ruling covered on the review queue, and records its end.
// Their held lines stay held; the plugin delivers a held line only on a release.
func scanRestart(r *recorder, dir string, now time.Time) error {
	type open struct {
		start                    map[string]any
		name                     string
		held, ruled, kept, freed int
	}
	sittings := map[string]*open{}
	var order []string
	for _, day := range []time.Time{now.AddDate(0, 0, -1), now} {
		name := filepath.Join(dir, day.UTC().Format(time.DateOnly)+".jsonl")
		lines, err := readRecordDay(name)
		if err != nil {
			return err
		}
		for _, l := range lines {
			id, _ := l["sitting"].(string)
			switch l["type"] {
			case "sitting_start":
				sittings[id] = &open{start: l}
				order = append(order, id)
			case "sitting_end":
				delete(sittings, id)
			case "line":
				if s := sittings[id]; s != nil {
					s.name, _ = l["name"].(string)
					if l["verdict"] == "hold" {
						s.held++
					}
				}
			case "ruling":
				if s := sittings[id]; s != nil {
					covers, _ := l["covers"].(float64)
					s.ruled += int(covers)
					a, _ := l["answer"].(map[string]any)
					switch a["ruling"] {
					case core.RulingHold:
						s.kept++
					case core.RulingNothing:
						s.freed++
					}
				}
			}
		}
	}
	at := stamp(now)
	for _, id := range order {
		s := sittings[id]
		if s == nil {
			continue
		}
		sittings[id] = nil
		str := func(k string) string { v, _ := s.start[k].(string); return v }
		e := restartEnd{Time: at, Stream: str("stream"), Sitting: id, Account: str("account"), Start: str("time"),
			Held: s.held, Unruled: s.held - s.ruled, EndedBy: "restart"}
		switch {
		case e.Unruled > 0 || s.kept > 0:
			e.Outcome = core.OutcomeHeld
		case s.freed > 0:
			e.Outcome = core.OutcomeReleased
		default:
			e.Outcome = core.OutcomeNone
		}
		if e.Unruled > 0 {
			e.Review = "the server restarted with " + strconv.Itoa(e.Unruled) + " held lines no ruling covered"
		}
		r.Record("sitting_end", e)
		if e.Review != "" {
			r.Record("review", core.ReviewRecord{Time: at, Sitting: id, Account: e.Account, Name: s.name, Why: e.Review})
			slog.Warn("a sitting the previous run left open goes on the review queue", "sitting", id,
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
		if t, _ := l["type"].(string); slices.Contains([]string{"sitting_start", "sitting_end", "line", "ruling"}, t) {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}
