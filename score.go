package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"ecc-server/core"
	"ecc-server/model"
)

const scoreUsage = `ecc-server score -bundle DIR -in FILE -out FILE [-threads 6] [-threshold NAME] [-ids] [-allow FILE]

Takes a file of lines as the plugin sends them, handles each as the server does (the text and names prepared, a
channel outside the bundle's refused, an emptied text dropped, copies folded, links on the allowlist replaced, the
bundle's models scoring the rest), and writes one record per line in the format of the pipeline's reference scorer
(ecc.reference), to check the server against it.

  -in     one JSON record a line, in arrival order: the fields of POST /line and the stream and time the server would
          give it: {"stream", "time" (whole seconds), "id", "channel", "speaker_uuid", "speaker", "shown",
          "target_uuid", "target", "text", "trusted"}
  -out    one record per input record: {"stream", "id", "refused": "unknown channel", "verdict": "hold"},
          {"stream", "id", "dropped": true, "verdict": "deliver"}, {"stream", "id", "folded_into": "<id>"},
          {"stream", "id", "context_only": true}, or {"stream", "id", "sh", "passed", "c", "held_<threshold>"...,
          "verdict", "kinds"}; -ids adds the prepared text, the text the models read, the author key and the repeat
          key of every line kept, and the context's ids, SH's input ids and features and C's input ids of a scored one
  -allow  a file of allowed links, one host or host/prefix a line (# comments)
`

type scoreIn struct {
	wireLine
	Stream string `json:"stream"`
	Time   int64  `json:"time"`
}

// repeatRecorder keeps the line a copy was folded into.
type repeatRecorder struct{ of string }

func (r *repeatRecorder) Record(kind string, v any) {
	if x, ok := v.(core.RepeatRecord); ok && kind == "repeat" {
		r.of = x.Of
	}
}

func readAllow(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		l, _, _ = strings.Cut(l, "#")
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

func scoreLines(args []string) error {
	fs := flag.NewFlagSet("score", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, scoreUsage) }
	dir := fs.String("bundle", "", "")
	in := fs.String("in", "", "")
	out := fs.String("out", "", "")
	threads := fs.Int("threads", 6, "")
	threshold := fs.String("threshold", "", "")
	ids := fs.Bool("ids", false, "")
	allowPath := fs.String("allow", "", "")
	_ = fs.Parse(args)
	if *dir == "" || *in == "" || *out == "" || fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}
	b, err := loadBundle(*dir)
	if err != nil {
		return err
	}
	if b.specErr != nil {
		return b.specErr
	}
	allow, err := readAllow(*allowPath)
	if err != nil {
		return err
	}
	t0 := time.Now()
	sc, err := model.Open(*dir, b.spec, model.Options{Threads: *threads, Threshold: *threshold})
	if err != nil {
		return err
	}
	defer sc.Close()
	fmt.Fprintf(os.Stderr, "models loaded in %.1f s\n", time.Since(t0).Seconds())
	// The server's core with no checkpoint and no sitting end: it prepares, folds and keeps the lines as the server
	// does; the models score the lines it would score.
	rec := &repeatRecorder{}
	eng := core.NewEngine(core.Config{Channels: b.Channels, CanonicalLink: b.Links.Canonical,
		SittingEnd: 100 * 365 * 24 * time.Hour}, core.Texts{}, sc, rec)
	if err := eng.AllowLinks(allow); err != nil {
		return err
	}
	links, _ := core.NewLinks(allow, b.Links.Canonical)

	f, err := os.Open(*in)
	if err != nil {
		return err
	}
	defer f.Close()
	o, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer o.Close()
	w := bufio.NewWriterSize(o, 1<<20)
	r := bufio.NewScanner(f)
	r.Buffer(make([]byte, 1<<20), 1<<24)
	var times []float64
	n, cRuns := 0, 0
	start := time.Now()
	// a stream's times are whole seconds of its own; each stream is moved after the last one, since the core's clock
	// only goes forward
	stream, seen := "", map[string]bool{}
	var offset, last int64
	for r.Scan() {
		var x scoreIn
		if err := json.Unmarshal(r.Bytes(), &x); err != nil {
			return fmt.Errorf("%s:%d: %w", *in, n+1, err)
		}
		n++
		if err := x.check(); err != nil {
			return fmt.Errorf("%s:%d: %w", *in, n, err)
		}
		if x.Stream != stream {
			if seen[x.Stream] {
				return fmt.Errorf("%s:%d: stream %s comes back after another; give each stream's lines together", *in, n, x.Stream)
			}
			seen[x.Stream], stream = true, x.Stream
			offset = max(0, last+86400-x.Time)
			eng.Start(x.Stream, time.Unix(x.Time+offset, 0))
		}
		at := time.Unix(x.Time+offset, 0)
		last = max(last, x.Time+offset)
		l := x.coreLine(at)
		l.ID = x.ID
		out := map[string]any{"stream": x.Stream, "id": x.ID}
		keep := func() {
			if *ids {
				k := core.RepeatKey(l)
				out["prepared"], out["text"], out["author"], out["repeat_key"] = l.Text, links.Replace(l.Text),
					model.AuthorKey(l.Speaker), k[:]
			}
		}
		switch scored, score := eng.Prepare(l); {
		case !eng.Known(l.Channel):
			eng.Line(l)
			out["refused"], out["verdict"] = "unknown channel", "hold"
		case l.Empty:
			eng.Line(l)
			out["dropped"], out["verdict"] = true, "deliver"
		case !score:
			rec.of = ""
			eng.Line(l)
			keep()
			switch {
			case rec.of != "":
				out["folded_into"] = rec.of
			case l.Context:
				out["context_only"] = true
			default:
				return fmt.Errorf("line %s/%s was neither scored, folded nor context", x.Stream, x.ID)
			}
		default:
			t1 := time.Now()
			d, err := sc.ScoreBy(scored, time.Time{})
			if err != nil {
				return fmt.Errorf("line %s/%s: %w", x.Stream, x.ID, err)
			}
			eng.LineScored(l, d.Verdict, nil)
			times = append(times, float64(time.Since(t1).Microseconds())/1000)
			keep()
			if *ids && out["text"] != scored.Text {
				return errors.New("the allowlist replaced links differently in the scorer's line")
			}
			verdict := "deliver"
			if d.Verdict.Hold {
				verdict = "hold"
			}
			out["sh"], out["passed"], out["verdict"], out["kinds"] = d.SH, d.Passed, verdict, nonNil(d.Verdict.Kinds)
			out["c"] = nil
			if d.C != nil {
				out["c"] = d.C
				cRuns++
			}
			for k, v := range d.Held {
				out["held_"+k] = v
			}
			if *ids {
				out["context_ids"] = nonNil(d.ContextIDs)
				out["sh_slot"], out["sh_addr"], out["sh_chan"], out["sh_gap"] = d.Slot, d.Addr, d.Chan, d.Gap
				out["sh_input_ids"] = d.SHIDs
				if d.CIDs != nil {
					out["c_input_ids"] = d.CIDs
				}
			}
		}
		b, _ := json.Marshal(out)
		w.Write(append(b, '\n'))
	}
	if err := r.Err(); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	slices.Sort(times)
	q := func(p float64) float64 {
		if len(times) == 0 {
			return 0
		}
		return times[min(len(times)-1, int(p*float64(len(times))))]
	}
	fmt.Fprintf(os.Stderr, "%d lines (%d scored, C on %d) in %.1f s; a scored line: median %.1f ms, p95 %.1f, p99 %.1f\n",
		n, len(times), cRuns, time.Since(start).Seconds(), q(0.5), q(0.95), q(0.99))
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
