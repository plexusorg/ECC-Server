package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/BurntSushi/toml"

	"ecc-server/core"
)

const replayUsage = `ecc-server replay -config FILE -lines FILE -fold DIR -out DIR [-mode parity|live]

Feeds recorded lines through the decision core on a virtual clock, with the fast model's recorded verdicts and the
judge's saved answers, and writes the record, one line per sitting and a summary to DIR.

  -config  the core's settings (TOML: the [core] table of the server's config)
  -lines   the lines, one JSON event a line: {"type":"stream",...} or {"type":"line",...}
  -fold    question.md, format.md, cases.json, answers.json (sha256 of the prompt -> answer), expected.json
  -mode    parity: sittings and the sitting-end checkpoint as the lines name them, no waiting-time or every-n-th
           checkpoint; checkpoints of sittings not in
           expected.json's judged_sittings are built and not sent, and a prompt with no saved answer stops the replay.
           live: sittings from presence and silence, the waiting-time checkpoint; a prompt with no saved answer is
           counted and its lines stay held.
`

// replayEvent is one line of the replay's input.
type replayEvent struct {
	Type          string   `json:"type"`
	Name          string   `json:"name"`
	Day           string   `json:"day"`
	ID            string   `json:"id"`
	Time          string   `json:"time"`
	Channel       string   `json:"channel"`
	Conversation  string   `json:"conversation"`
	Private       bool     `json:"private"`
	Account       string   `json:"account"`
	Shown         string   `json:"name_shown"`
	TargetAccount string   `json:"target_account"`
	Target        string   `json:"target"`
	Text          string   `json:"text"`
	Sent          int      `json:"sent"`
	Trusted       bool     `json:"trusted"`
	Context       bool     `json:"context"`
	Sitting       string   `json:"sitting"`
	Verdict       string   `json:"verdict"`
	Kinds         []string `json:"kinds"`
}

type savedAnswer struct {
	Checkpoint string   `json:"checkpoint"`
	Ruling     string   `json:"ruling"`
	By         string   `json:"by"`
	Like       []string `json:"like"`
	Sure       *bool    `json:"sure"`
}

type expectedOutcome struct {
	Outcome  string   `json:"outcome"`
	BanLine  *string  `json:"ban_line"`
	Rulings  []string `json:"rulings"`
	Released []string `json:"released"`
}

type expected struct {
	Prompts         map[string]string          `json:"prompts"`
	JudgedSittings  []string                   `json:"judged_sittings"`
	ExampleSittings []string                   `json:"example_sittings"`
	Outcomes        map[string]expectedOutcome `json:"outcomes"`
}

// recordedScorer answers with the verdicts the lines carry, by line id within the current stream.
type recordedScorer struct {
	verdicts map[string]core.Verdict
}

func (s *recordedScorer) Score(l core.Line, _ []core.Line) (core.Verdict, error) {
	v, ok := s.verdicts[l.ID]
	if !ok {
		return core.Verdict{}, fmt.Errorf("no recorded verdict for line %s", l.ID)
	}
	return v, nil
}

var errNoSavedAnswer = errors.New("no saved answer for this prompt")

// savedJudge answers from saved answers by the sha256 of the prompt.
type savedJudge struct {
	answers map[string]savedAnswer
	asked   int
	missing int
}

func (j *savedJudge) Rule(job *core.Job) (core.Answer, error) {
	j.asked++
	a, ok := j.answers[job.Hash]
	if !ok {
		j.missing++
		return core.Answer{}, errNoSavedAnswer
	}
	return core.Answer{Ruling: a.Ruling, By: a.By, Like: a.Like, Sure: a.Sure}, nil
}

// replayRecorder writes the record and keeps the checkpoints and sitting ends for the comparison.
type replayRecorder struct {
	w        *bufio.Writer
	err      error
	sittings []core.SittingRecord
	actions  map[string]int
	reviews  int
	repeats  int
}

func (r *replayRecorder) Record(kind string, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	if r.err == nil {
		_, r.err = fmt.Fprintf(r.w, "{\"type\":%q,%s\n", kind, body[1:])
	}
	switch x := v.(type) {
	case core.SittingRecord:
		r.sittings = append(r.sittings, x)
	case core.Action:
		r.actions[x.Kind]++
	case core.ReviewRecord:
		r.reviews++
	case core.RepeatRecord:
		r.repeats++
	}
}

func readReplayEvents(path string) ([]replayEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []replayEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for n := 1; sc.Scan(); n++ {
		var ev replayEvent
		// the event's "name" is the stream's name on a stream event and the shown name on a line event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		if ev.Type == "line" {
			ev.Shown, ev.Name = ev.Name, ""
		}
		out = append(out, ev)
	}
	return out, sc.Err()
}

type replayConfig struct {
	Core core.Config `toml:"core"`
}

func replay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, replayUsage) }
	configPath := fs.String("config", "", "")
	linesPath := fs.String("lines", "", "")
	foldDir := fs.String("fold", "", "")
	outDir := fs.String("out", "", "")
	mode := fs.String("mode", "parity", "")
	_ = fs.Parse(args)
	if *configPath == "" || *linesPath == "" || *foldDir == "" || *outDir == "" || fs.NArg() != 0 ||
		(*mode != "parity" && *mode != "live") {
		fs.Usage()
		os.Exit(2)
	}
	parity := *mode == "parity"

	var rc replayConfig
	md, err := toml.DecodeFile(*configPath, &rc)
	if err != nil {
		return fmt.Errorf("config %s: %w", *configPath, err)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		return fmt.Errorf("config %s: unknown keys %v", *configPath, keys)
	}
	cfg := rc.Core
	cfg.Zone = time.UTC
	if parity {
		cfg.DriverSittings, cfg.LastHeldView, cfg.Wait, cfg.Every = true, true, 0, 0
	} else {
		cfg.DriverSittings, cfg.LastHeldView = false, false
	}

	var texts core.Texts
	q, err := os.ReadFile(filepath.Join(*foldDir, "question.md"))
	if err != nil {
		return err
	}
	f, err := os.ReadFile(filepath.Join(*foldDir, "format.md"))
	if err != nil {
		return err
	}
	texts.Question, texts.Format = string(q), string(f)
	if err := readJSON(filepath.Join(*foldDir, "cases.json"), &texts.Examples); err != nil {
		return err
	}
	answers := map[string]savedAnswer{}
	if err := readJSON(filepath.Join(*foldDir, "answers.json"), &answers); err != nil {
		return err
	}
	var exp expected
	if err := readJSON(filepath.Join(*foldDir, "expected.json"), &exp); err != nil {
		return err
	}
	events, err := readReplayEvents(*linesPath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(*outDir, 0o750); err != nil {
		return err
	}
	rf, err := os.Create(filepath.Join(*outDir, "record.jsonl"))
	if err != nil {
		return err
	}
	defer rf.Close()
	rec := &replayRecorder{w: bufio.NewWriterSize(rf, 1<<20), actions: map[string]int{}}
	scorer := &recordedScorer{}
	judge := &savedJudge{answers: answers}
	eng := core.NewEngine(cfg, texts, scorer, rec)

	judged := map[string]bool{}
	for _, s := range exp.JudgedSittings {
		judged[s] = true
	}
	// parity: the index of each sitting's last line, after which the sitting ends
	lastOf := map[string]int{}
	for i, ev := range events {
		if ev.Type == "line" && ev.Sitting != "" {
			lastOf[ev.Sitting] = i
		}
	}
	var stop error
	notSent := 0
	run := func(r core.Result) {
		jobs := r.Jobs
		for len(jobs) > 0 && stop == nil {
			j := jobs[0]
			jobs = jobs[1:]
			var res core.Result
			if parity && !judged[j.Sitting] {
				notSent++
				res = eng.Rule(j, core.Answer{}, core.ErrNotSent)
			} else {
				a, err := judge.Rule(j)
				if err != nil && parity {
					stop = fmt.Errorf("checkpoint %s#%d (sha256 %s): %w", j.Sitting, j.K, j.Hash, err)
					return
				}
				res = eng.Rule(j, a, err)
			}
			jobs = append(jobs, res.Jobs...)
		}
	}
	var last time.Time
	for i, ev := range events {
		switch ev.Type {
		case "stream":
			day, err := time.ParseInLocation(time.DateOnly, ev.Day, time.UTC)
			if err != nil {
				return fmt.Errorf("stream %s: %w", ev.Name, err)
			}
			scorer.verdicts = map[string]core.Verdict{}
			for _, x := range events[i+1:] {
				if x.Type == "stream" {
					break
				}
				if x.Verdict != "" {
					scorer.verdicts[x.ID] = core.Verdict{Hold: x.Verdict == "hold", Kinds: x.Kinds}
				}
			}
			if day.Before(last) {
				day = last
			}
			run(eng.Start(ev.Name, day))
		case "line":
			t, err := time.Parse(time.RFC3339, ev.Time)
			if err != nil {
				return fmt.Errorf("line %s: %w", ev.ID, err)
			}
			last = t
			sit := ev.Sitting
			if !parity {
				sit = ""
			}
			r := eng.Line(core.Line{ID: ev.ID, Time: t, Channel: ev.Channel, Conversation: ev.Conversation,
				Private: ev.Private, Account: ev.Account, Name: ev.Shown, TargetAccount: ev.TargetAccount,
				Target: ev.Target, Text: ev.Text, Sent: ev.Sent, Context: ev.Context || ev.Trusted, Sitting: sit})
			run(r)
			if parity && ev.Sitting != "" && lastOf[ev.Sitting] == i {
				run(eng.End(ev.Sitting, t))
			}
		default:
			return fmt.Errorf("unknown event type %q", ev.Type)
		}
		if stop != nil {
			return stop
		}
	}
	run(eng.Close(last.Add(24 * time.Hour)))
	if stop != nil {
		return stop
	}
	if err := rec.w.Flush(); err != nil {
		return err
	}
	if rec.err != nil {
		return rec.err
	}

	sf, err := os.Create(filepath.Join(*outDir, "sittings.jsonl"))
	if err != nil {
		return err
	}
	defer sf.Close()
	sw := bufio.NewWriter(sf)
	for _, s := range rec.sittings {
		b, _ := json.Marshal(s)
		sw.Write(append(b, '\n'))
	}
	if err := sw.Flush(); err != nil {
		return err
	}

	sum := summarize(*mode, rec, judge, notSent, exp, answers, parityHeldSets(events))
	b, _ := json.MarshalIndent(sum, "", " ")
	if err := os.WriteFile(filepath.Join(*outDir, "summary.json"), append(b, '\n'), 0o640); err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// parityHeldSets is each input sitting's held lines, keyed as summarize keys a replayed sitting's.
func parityHeldSets(events []replayEvent) map[string]bool {
	by := map[string][]string{}
	stream := map[string]string{}
	cur := ""
	for _, ev := range events {
		if ev.Type == "stream" {
			cur = ev.Name
			continue
		}
		if ev.Verdict == "hold" && !ev.Trusted {
			by[ev.Sitting] = append(by[ev.Sitting], ev.ID)
			stream[ev.Sitting] = cur
		}
	}
	out := map[string]bool{}
	for s, ids := range by {
		out[stream[s]+"/"+fmt.Sprint(ids)] = true
	}
	return out
}

type replaySummary struct {
	Mode           string         `json:"mode"`
	Sittings       int            `json:"sittings"`
	WithHeld       int            `json:"sittings_with_a_held_line"`
	Outcomes       map[string]int `json:"outcomes"`
	Checkpoints    map[string]int `json:"checkpoints_by_reason"`
	PromptsBuilt   int            `json:"prompts_built"`
	Sent           int            `json:"sent"`
	NotSent        int            `json:"built_not_sent"`
	CapStopped     int            `json:"stopped_by_the_cap"`
	NoAnswer       int            `json:"sent_without_a_saved_answer"`
	Actions        map[string]int `json:"actions"`
	Reviews        int            `json:"review_queue"`
	Repeats        int            `json:"repeats"`
	Parity         *parityResult  `json:"parity,omitempty"`
	LiveComparison *liveResult    `json:"live,omitempty"`
}

type parityResult struct {
	PromptsCompared   int      `json:"prompts_compared"`
	PromptsEqual      int      `json:"prompts_equal"`
	PromptsOfExamples int      `json:"prompts_of_example_sittings"`
	PromptDiffs       []string `json:"prompt_differences"`
	MissingCheckpoint []string `json:"python_checkpoints_not_built"`
	SittingsCompared  int      `json:"sittings_compared"`
	SittingsEqual     int      `json:"sittings_equal"`
	SittingDiffs      []string `json:"sitting_differences"`
}

type liveResult struct {
	SameTextAsParity int `json:"checkpoints_with_a_parity_prompt"`
	SameTextAnswered int `json:"of_them_with_a_saved_answer"`
	DifferentText    int `json:"checkpoints_whose_text_differs"`
	WithAnswer       int `json:"checkpoints_with_a_saved_answer"`
	WithoutAnswer    int `json:"checkpoints_without_a_saved_answer"`
	// live sittings with a held line whose held lines are exactly one parity sitting's, and the others
	SameHeldLines  int `json:"sittings_holding_one_parity_sittings_lines"`
	OtherHeldLines int `json:"sittings_holding_other_sets"`
}

func summarize(mode string, rec *replayRecorder, judge *savedJudge, notSent int, exp expected,
	answers map[string]savedAnswer, pyHeld map[string]bool) replaySummary {
	s := replaySummary{Mode: mode, Sittings: len(rec.sittings), Outcomes: map[string]int{},
		Checkpoints: map[string]int{}, Actions: rec.actions, Reviews: rec.reviews, Repeats: rec.repeats, NotSent: notSent,
		NoAnswer: judge.missing}
	for _, st := range rec.sittings {
		s.Outcomes[st.Outcome]++
		if len(st.Held) > 0 {
			s.WithHeld++
		}
		for _, c := range st.Checkpoints {
			reason := c.Reason
			if len(reason) > 5 && reason[:5] == "held " {
				reason = "held count"
			}
			s.Checkpoints[reason]++
			if c.Hash != "" {
				s.PromptsBuilt++
			}
			if c.Sent {
				s.Sent++
			} else if c.NotSent != "" && c.Hash != "" {
				s.CapStopped++
			}
		}
	}
	s.Sent -= notSent
	if mode == "parity" {
		s.Parity = compareParity(rec, exp)
		return s
	}
	parityHashes := map[string]bool{}
	for _, h := range exp.Prompts {
		parityHashes[h] = true
	}
	lr := &liveResult{}
	for _, st := range rec.sittings {
		for _, c := range st.Checkpoints {
			if c.Hash == "" {
				continue
			}
			if parityHashes[c.Hash] {
				lr.SameTextAsParity++
				if _, ok := answers[c.Hash]; ok {
					lr.SameTextAnswered++
				}
			} else {
				lr.DifferentText++
			}
			if _, ok := answers[c.Hash]; ok {
				lr.WithAnswer++
			} else {
				lr.WithoutAnswer++
			}
		}
	}
	held := map[string][]string{}
	for _, st := range rec.sittings {
		if len(st.Held) > 0 {
			held[st.Stream+"/"+fmt.Sprint(st.Held)] = st.Held
		}
	}
	for k := range held {
		if pyHeld[k] {
			lr.SameHeldLines++
		} else {
			lr.OtherHeldLines++
		}
	}
	s.LiveComparison = lr
	return s
}

func compareParity(rec *replayRecorder, exp expected) *parityResult {
	p := &parityResult{}
	examples := map[string]bool{}
	for _, s := range exp.ExampleSittings {
		examples[s] = true
	}
	// Python's checkpoints per sitting, by held-line count
	pyKs := map[string][]int{}
	for id := range exp.Prompts {
		i := len(id) - 1
		for id[i] != '#' {
			i--
		}
		k, _ := strconv.Atoi(id[i+1:])
		pyKs[id[:i]] = append(pyKs[id[:i]], k)
	}
	got := map[string]core.SittingRecord{}
	for _, st := range rec.sittings {
		got[st.Sitting] = st
		if examples[st.Sitting] {
			for _, c := range st.Checkpoints {
				if c.Hash != "" {
					p.PromptsOfExamples++
				}
			}
			continue
		}
		var ks []int
		for _, c := range st.Checkpoints {
			id := st.Sitting + "#" + strconv.Itoa(c.K)
			ks = append(ks, c.K)
			p.PromptsCompared++
			want, ok := exp.Prompts[id]
			switch {
			case !ok:
				p.PromptDiffs = append(p.PromptDiffs, id+": Python has no such checkpoint")
			case c.Hash != want:
				p.PromptDiffs = append(p.PromptDiffs, id+": text differs")
			default:
				p.PromptsEqual++
			}
		}
		want := pyKs[st.Sitting]
		sort.Ints(want)
		// Python built every checkpoint of the sitting; the core stops at a ban
		n := len(want)
		if st.Outcome == core.OutcomeBanned {
			n = len(ks)
		}
		for _, k := range want[:min(n, len(want))] {
			if !slices.Contains(ks, k) {
				p.MissingCheckpoint = append(p.MissingCheckpoint, st.Sitting+"#"+strconv.Itoa(k))
			}
		}
	}
	names := make([]string, 0, len(exp.Outcomes))
	for id := range exp.Outcomes {
		names = append(names, id)
	}
	sort.Strings(names)
	for _, id := range names {
		w := exp.Outcomes[id]
		p.SittingsCompared++
		g, ok := got[id]
		if !ok {
			p.SittingDiffs = append(p.SittingDiffs, id+": no such sitting in the replay")
			continue
		}
		out := g.Outcome
		if out == core.OutcomeNone {
			out = "never reached"
		}
		var rulings []string
		for _, c := range g.Checkpoints {
			if c.Ruling != "" {
				rulings = append(rulings, c.Ruling)
			}
		}
		wantBan := ""
		if w.BanLine != nil {
			wantBan = *w.BanLine
		}
		var diffs []string
		if out != w.Outcome {
			diffs = append(diffs, fmt.Sprintf("outcome %s, online-1 %s", out, w.Outcome))
		}
		if g.BanLine != wantBan {
			diffs = append(diffs, fmt.Sprintf("ban line %q, online-1 %q", g.BanLine, wantBan))
		}
		if !slices.Equal(g.Released, w.Released) && (len(g.Released) > 0 || len(w.Released) > 0) {
			diffs = append(diffs, fmt.Sprintf("released %v, online-1 %v", g.Released, w.Released))
		}
		if !slices.Equal(rulings, w.Rulings) && (len(rulings) > 0 || len(w.Rulings) > 0) {
			diffs = append(diffs, fmt.Sprintf("rulings %v, online-1 %v", rulings, w.Rulings))
		}
		if len(diffs) == 0 {
			p.SittingsEqual++
		} else {
			p.SittingDiffs = append(p.SittingDiffs, fmt.Sprintf("%s: %v", id, diffs))
		}
	}
	return p
}
