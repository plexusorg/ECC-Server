package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"time"

	"ecc-server/core"
	"ecc-server/model"
)

const scoreUsage = `ecc-server score -bundle DIR -in FILE -out FILE [-threads 6] [-threshold NAME] [-ids]

Scores a stream of chat lines with a bundle's models as the server does, and writes one record per line in the
format of the pipeline's reference scorer (ecc.reference), to check the server against it.

  -in   one JSON record a line, in arrival order: {"stream", "id", "time" (whole seconds), "channel", "author",
        "partner" (msg only), "text", "context_only" (optional)}
  -out  one record per input record: {"stream", "id", "sh", "passed", "c", "held_<threshold>"..., "verdict",
        "kinds"} or
        {"stream", "id", "context_only": true}; -ids adds the context's ids, SH's input ids and features and C's
        input ids
`

type scoreIn struct {
	Stream      string `json:"stream"`
	ID          string `json:"id"`
	Time        int64  `json:"time"`
	Channel     string `json:"channel"`
	Author      string `json:"author"`
	Partner     string `json:"partner"`
	Text        string `json:"text"`
	ContextOnly bool   `json:"context_only"`
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
	_ = fs.Parse(args)
	if *dir == "" || *in == "" || *out == "" || fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}
	spec, err := model.ReadSpec(*dir)
	if err != nil {
		return err
	}
	t0 := time.Now()
	sc, err := model.Open(*dir, spec, model.Options{Threads: *threads, Threshold: *threshold})
	if err != nil {
		return err
	}
	defer sc.Close()
	fmt.Fprintf(os.Stderr, "models loaded in %.1f s\n", time.Since(t0).Seconds())
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
	for r.Scan() {
		var x scoreIn
		if err := json.Unmarshal(r.Bytes(), &x); err != nil {
			return fmt.Errorf("%s:%d: %w", *in, n+1, err)
		}
		n++
		l := core.Line{ID: x.ID, Stream: x.Stream, Time: time.Unix(x.Time, 0), Channel: x.Channel, Account: x.Author,
			Name: x.Author, Speaker: x.Author, Target: x.Partner, Text: x.Text}
		rec := map[string]any{"stream": x.Stream, "id": x.ID}
		if x.ContextOnly {
			sc.See(l)
			rec["context_only"] = true
		} else {
			t1 := time.Now()
			d, err := sc.ScoreBy(l, time.Time{})
			if err != nil {
				return fmt.Errorf("line %s/%s: %w", x.Stream, x.ID, err)
			}
			times = append(times, float64(time.Since(t1).Microseconds())/1000)
			verdict := "deliver"
			if d.Verdict.Hold {
				verdict = "hold"
			}
			rec["sh"], rec["passed"], rec["verdict"], rec["kinds"] = d.SH, d.Passed, verdict, nonNil(d.Verdict.Kinds)
			rec["c"] = nil
			if d.C != nil {
				rec["c"] = d.C
				cRuns++
			}
			for k, v := range d.Held {
				rec["held_"+k] = v
			}
			if *ids {
				rec["context_ids"] = nonNil(d.ContextIDs)
				rec["sh_slot"], rec["sh_addr"], rec["sh_chan"], rec["sh_gap"] = d.Slot, d.Addr, d.Chan, d.Gap
				rec["sh_input_ids"] = d.SHIDs
				if d.CIDs != nil {
					rec["c_input_ids"] = d.CIDs
				}
			}
		}
		b, _ := json.Marshal(rec)
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
