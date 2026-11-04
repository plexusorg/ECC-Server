package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// recordLog is the day's record: one JSON line per message, decision, slow-check call, verdict, action and
// acknowledgement, in <data>/record/YYYY-MM-DD.jsonl (UTC days). At midnight the day's file is compressed to
// YYYY-MM-DD.jsonl.zst and compressed files past retention are deleted. Lines are written in the order they are
// appended, each flushed at once, so a crash loses at most the line being written.
type recordLog struct {
	dir       string
	server    string
	retention int

	mu  sync.Mutex
	day string // the open file's day, "" when none is open
	f   *os.File
	w   *bufio.Writer
}

var recordFile = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})\.jsonl(\.zst)?$`)

// stamp is a record time: UTC with milliseconds.
func stamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// openRecord compresses the files an earlier run left, applies retention, and rotates at each midnight until ctx
// ends.
func openRecord(ctx context.Context, cfg Config) (*recordLog, error) {
	r := &recordLog{dir: filepath.Join(cfg.Data, "record"), server: cfg.Server, retention: cfg.RecordRetentionDays}
	if err := os.MkdirAll(r.dir, 0o750); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.rotate()
	r.mu.Unlock()
	go func() {
		for {
			now := time.Now().UTC()
			next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 1, 0, time.UTC)
			select {
			case <-ctx.Done():
				return
			case <-time.After(next.Sub(now)):
			}
			r.mu.Lock()
			r.rotate()
			r.mu.Unlock()
		}
	}()
	return r, nil
}

// append writes one line of the given type; v is a struct whose fields follow "type" and "server".
func (r *recordLog) append(typ string, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("a record line does not encode", "type", typ, "err", err)
		return
	}
	head, _ := json.Marshal(struct {
		Type   string `json:"type"`
		Server string `json:"server"`
	}{typ, r.server})
	line := head[:len(head)-1]
	if len(body) > 2 {
		line = append(append(line, ','), body[1:]...)
	} else {
		line = append(line, '}')
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	today := time.Now().UTC().Format(time.DateOnly)
	if r.day != today {
		r.rotate()
		f, err := os.OpenFile(filepath.Join(r.dir, today+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		if err != nil {
			slog.Error("could not open the day's record; the line is lost", "type", typ, "err", err)
			return
		}
		r.f, r.w, r.day = f, bufio.NewWriter(f), today
	}
	r.w.Write(line)
	r.w.WriteByte('\n')
	if err := r.w.Flush(); err != nil {
		slog.Error("could not write the record", "type", typ, "err", err)
		r.closeFile()
	}
}

// close writes what is buffered and closes the day's file.
func (r *recordLog) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeFile()
}

func (r *recordLog) closeFile() {
	if r.f == nil {
		return
	}
	if err := r.w.Flush(); err != nil {
		slog.Error("could not write the record", "err", err)
	}
	if err := r.f.Close(); err != nil {
		slog.Error("could not close the record", "err", err)
	}
	r.f, r.w, r.day = nil, nil, ""
}

// rotate closes a file of an earlier day, compresses every day file before today, and deletes compressed files
// past retention. The caller holds mu.
func (r *recordLog) rotate() {
	now := time.Now().UTC()
	today := now.Format(time.DateOnly)
	if r.day != "" && r.day != today {
		r.closeFile()
	}
	oldest := time.Date(now.Year(), now.Month(), now.Day()-r.retention, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		slog.Error("could not list the records", "dir", r.dir, "err", err)
		return
	}
	for _, e := range entries {
		m := recordFile.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if _, err := time.Parse(time.DateOnly, m[1]); err != nil {
			continue // shaped like a record but not a calendar date
		}
		path := filepath.Join(r.dir, e.Name())
		switch {
		case m[2] != "" && m[1] < oldest:
			if err := os.Remove(path); err != nil {
				slog.Error("could not delete a record past retention", "file", path, "err", err)
			}
		case m[2] == "" && m[1] < today:
			if err := compress(path); err != nil {
				slog.Error("could not compress a record", "file", path, "err", err)
			}
		}
	}
}

// compress writes path.zst through a .part file, then deletes path.
func compress(path string) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	part := path + ".zst.part"
	out, err := os.Create(part)
	if err != nil {
		return err
	}
	enc, _ := zstd.NewWriter(out)
	_, err = io.Copy(enc, in)
	if cerr := enc.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(part, path+".zst")
	}
	if err != nil {
		os.Remove(part)
		return err
	}
	in.Close()
	syncDir(filepath.Dir(path))
	return os.Remove(path)
}

// writeAtomic replaces path with data: a temporary file, fsynced, renamed over it.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir makes a rename in dir durable on Linux; Windows cannot fsync a directory.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil {
		if err := d.Sync(); err != nil {
			slog.Warn("could not sync a directory", "dir", dir, "err", err)
		}
		d.Close()
	}
}
