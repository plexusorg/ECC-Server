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
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"ecc-server/core"
	"ecc-server/model"
)

const replayUsage = `ecc-server replay -config FILE -lines FILE -fold DIR -out DIR [-mode parity|live]
                    [-judge-delay 15s] [-keep-going] [-dump FILE] [-bundle DIR [-threads 6] [-threshold NAME]]

Feeds recorded lines through the decision core on a virtual clock, with the fast model's recorded verdicts (or, with
-bundle, the bundle's models scoring every line) and the judge's saved answers, and writes the record, one line per
session and a summary to DIR. The core's timers run at their time; a ruling arrives -judge-delay after its checkpoint
(after the call is put, for a call that waited for another), as an event of its own, the lines before it handled
first.

  -config  the core's settings (TOML: a [core] table, with the keys of core.Config)
  -lines   the lines, one JSON event a line: {"type":"stream", "name", "day"}, {"type":"line",...} or
           {"type":"presence", "kind": "join"|"leave", "account", "time"}; text and names prepared
  -fold    question.md, format.md, allowlist.txt, cases.json, answers.json (sha256 of the prompt -> answer), expected.json
  -mode    parity: sessions and the session-end checkpoint as the lines name them, no waiting-time or every-n-th
           checkpoint, rulings at once (as the measured replay ruled); checkpoints of sessions not in
           expected.json's judged_sessions are built and not sent, and a prompt with no saved answer stops the replay
           (with -keep-going it is counted and its lines stay held).
           live: sessions from presence and silence, the waiting-time checkpoint; a prompt with no saved answer is
           counted and its lines stay held.
  -dump    writes every judge call's session, held count, sha256 and the prompt from the new case on, one JSON a line
`

// replayEvent is one line of the replay's input.
type replayEvent struct {
	Type          string  `json:"type"`
	Kind          string  `json:"kind"`
	Name          string  `json:"name"`
	Day           string  `json:"day"`
	CoverageRule  string  `json:"coverage_rule"`
	ModelTextRule string  `json:"model_text_rule"`
	CanonicalLink *string `json:"canonical_link"`
	ID            string  `json:"id"`
	Time          string  `json:"time"`
	Channel       string  `json:"channel"`
	Conversation  string  `json:"conversation"`
	Private       bool    `json:"private"`
	Account       string  `json:"account"`
	Shown         string  `json:"name_shown"`
	TargetAccount string  `json:"target_account"`
	Target        string  `json:"target"`
	Text          string  `json:"text"`
	// Speaker is the speaker's account name as the models know them (a Discord relay: "discord:" and the shown name).
	Speaker    string   `json:"speaker"`
	Sent       int      `json:"sent"`
	Trusted    bool     `json:"trusted"`
	Context    bool     `json:"context"`
	Session    string   `json:"session"`
	Verdict    string   `json:"verdict"`
	Coverage   string   `json:"coverage"`
	Categories []string `json:"categories"`
	// Key is the line's own id, unique in its stream (the rendered id may not be); CopyOf names the line a copy
	// repeats; RawText and RawShown are the text and the shown name before preparation.
	Key      string  `json:"key"`
	CopyOf   string  `json:"copy_of"`
	RawText  *string `json:"raw_text"`
	RawShown *string `json:"raw_shown"`
}

func (ev replayEvent) ref() string {
	if ev.Key != "" {
		return ev.Key
	}
	return ev.ID
}

// coreLine is the input's line: its text and names prepared already. The repeat key is made as the server makes it,
// from the raw text and shown name when the input has them, else from the prepared ones.
func (ev replayEvent) coreLine(t time.Time, session string) core.Line {
	account := strings.TrimPrefix(ev.Speaker, "discord:")
	l := core.Line{ID: ev.ID, Ref: ev.ref(), Time: t, Channel: ev.Channel, Conversation: ev.Conversation,
		Private: ev.Private, Account: ev.Account, Name: ev.Shown, Speaker: ev.Speaker, RawName: ev.Shown,
		TargetAccount: ev.TargetAccount, Target: ev.Target, Text: ev.Text, Raw: ev.Text, Key: ev.Text,
		KeyName: core.KeyName(ev.Channel, ev.Shown, account), Sent: ev.Sent, Context: ev.Context || ev.Trusted || ev.Channel == "discord",
		Session: session}
	if ev.RawText != nil {
		l.Raw = *ev.RawText
		l.Key, _ = core.PrepareText(l.Raw, ev.Channel)
		l.Empty = l.Key == ""
	}
	if ev.RawShown != nil {
		l.RawName = *ev.RawShown
		switch ev.Channel {
		case "chat":
			shown := core.ShownName(l.RawName)
			if shown == "" {
				shown = account
			}
			l.KeyName = core.KeyName(ev.Channel, shown, account)
		case "discord":
			l.KeyName = core.KeyName(ev.Channel, core.DiscordName(l.RawName), account)
		}
	}
	return l
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
	JudgedSessions  []string                   `json:"judged_sessions"`
	ExampleSessions []string                   `json:"example_sessions"`
	Outcomes        map[string]expectedOutcome `json:"outcomes"`
}

// recordedScorer answers with the verdicts the lines carry, by line id within the current stream.
type recordedScorer struct {
	verdicts map[string]core.Verdict
	rejects  map[string]bool
}

func (s *recordedScorer) See(core.Line) {}

func (s *recordedScorer) RejectReason(l core.Line) string {
	if s.rejects[l.Ref] {
		return "recorded complete-target rejection"
	}
	return ""
}

func (s *recordedScorer) Score(l core.Line) (core.Verdict, error) {
	v, ok := s.verdicts[l.Ref]
	if !ok {
		return core.Verdict{}, fmt.Errorf("no recorded verdict for line %s", l.Ref)
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

// replayRecorder writes the record and keeps the checkpoints and session ends for the comparison.
type replayRecorder struct {
	w        *bufio.Writer
	err      error
	sessions []core.SessionRecord
	actions  map[string]int
	reviews  int
	repeats  int
	folded   map[string]string // a repeat's ref -> the ref of the line it repeats
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
	case core.SessionRecord:
		r.sessions = append(r.sessions, x)
	case core.Action:
		r.actions[x.Kind]++
	case core.ReviewRecord:
		r.reviews++
	case core.RepeatRecord:
		r.repeats++
		r.folded[x.Ref] = x.OfRef
	}
}

func readReplayEvents(path string) ([]replayEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []replayEvent
	stream := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for n := 1; sc.Scan(); n++ {
		var ev replayEvent
		// the event's "name" is the stream's name on a stream event and the shown name on a line event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		if ev.Type == "stream" {
			if ev.CoverageRule != model.CoverageRule || ev.CanonicalLink == nil || ev.ModelTextRule != core.ModelTextRule {
				return nil, fmt.Errorf("%s:%d: stream requires model_text_rule %s, canonical_link and coverage_rule %s",
					path, n, core.ModelTextRule, model.CoverageRule)
			}
			stream = true
		}
		if ev.Type == "line" {
			if !stream || ev.RawText == nil {
				return nil, fmt.Errorf("%s:%d: line requires a coverage stream and raw_text", path, n)
			}
			switch ev.Coverage {
			case model.CoverageScored, model.CoverageRejected, "context", "fold", "dropped":
			default:
				return nil, fmt.Errorf("%s:%d: line has invalid coverage %q", path, n, ev.Coverage)
			}
			if (ev.Verdict == "reject") != (ev.Coverage == model.CoverageRejected) {
				return nil, fmt.Errorf("%s:%d: reject verdict and coverage disagree", path, n)
			}
			if ev.Verdict != "" && ev.Verdict != "deliver" && ev.Verdict != "hold" && ev.Verdict != "reject" {
				return nil, fmt.Errorf("%s:%d: invalid verdict %q", path, n, ev.Verdict)
			}
			context := ev.Context || ev.Trusted || ev.Channel == "discord"
			if context && ev.Coverage != "context" && ev.Coverage != "fold" && ev.Coverage != "dropped" {
				return nil, fmt.Errorf("%s:%d: context line has scoring coverage", path, n)
			}
			if !context && (ev.Coverage == "context" || (ev.Coverage == model.CoverageScored && ev.Verdict == "")) {
				return nil, fmt.Errorf("%s:%d: scoring line has no recorded verdict", path, n)
			}
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
	bundle := fs.String("bundle", "", "")
	threads := fs.Int("threads", 6, "")
	threshold := fs.String("threshold", "", "")
	judgeDelay := fs.Duration("judge-delay", 15*time.Second, "")
	keepGoing := fs.Bool("keep-going", false, "")
	dumpPath := fs.String("dump", "", "")
	_ = fs.Parse(args)
	if *configPath == "" || *linesPath == "" || *foldDir == "" || *outDir == "" || fs.NArg() != 0 ||
		(*mode != "parity" && *mode != "live") || *judgeDelay < 0 {
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
	delay := *judgeDelay
	if parity {
		cfg.DriverSessions, cfg.LastHeldView, cfg.Wait, cfg.Every = true, true, 0, 0
		delay = 0
	} else {
		cfg.DriverSessions, cfg.LastHeldView = false, false
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
	for _, ev := range events {
		if ev.Type == "stream" && *ev.CanonicalLink != cfg.CanonicalLink {
			return fmt.Errorf("stream %s: canonical_link differs from replay config", ev.Name)
		}
	}

	if err := os.MkdirAll(*outDir, 0o750); err != nil {
		return err
	}
	rf, err := os.Create(filepath.Join(*outDir, "record.jsonl"))
	if err != nil {
		return err
	}
	defer rf.Close()
	rec := &replayRecorder{w: bufio.NewWriterSize(rf, 1<<20), actions: map[string]int{}, folded: map[string]string{}}
	scorer := &recordedScorer{}
	var sc core.Scorer = scorer
	if *bundle != "" {
		var metadata Bundle
		if err := readJSON(filepath.Join(*bundle, "bundle.json"), &metadata); err != nil {
			return err
		}
		if metadata.Links.Canonical != cfg.CanonicalLink {
			return fmt.Errorf("bundle canonical link differs from replay config")
		}
		spec, err := model.ReadSpec(*bundle)
		if err != nil {
			return err
		}
		m, err := model.Open(*bundle, spec, model.Options{Threads: *threads, Threshold: *threshold})
		if err != nil {
			return err
		}
		defer m.Close()
		sc = m
	}
	judge := &savedJudge{answers: answers}
	var dump *json.Encoder
	if *dumpPath != "" {
		df, err := os.Create(*dumpPath)
		if err != nil {
			return err
		}
		defer df.Close()
		dump = json.NewEncoder(df)
	}
	eng := core.NewEngine(cfg, texts, sc, rec)
	allowed, err := readAllow(filepath.Join(*foldDir, "allowlist.txt"))
	if err != nil {
		return err
	}
	if err := eng.AllowLinks(allowed); err != nil {
		return err
	}

	judged := map[string]bool{}
	for _, s := range exp.JudgedSessions {
		judged[s] = true
	}
	// parity: the index of each session's last line, after which the session ends
	lastOf := map[string]int{}
	for i, ev := range events {
		if ev.Type == "line" && ev.Session != "" {
			lastOf[ev.Session] = i
		}
	}

	// A judge call put at time t is answered at the later of t and its checkpoint's time plus the delay, as an event of
	// its own: the lines and timers before then are handled first.
	type ruling struct {
		job *core.Job
		due time.Time
		a   core.Answer
		err error
	}
	var pending []ruling
	var stop error
	notSent := 0
	put := func(r core.Result, at time.Time) {
		for _, j := range r.Jobs {
			p := ruling{job: j, due: later(j.At, at).Add(delay)}
			if dump != nil {
				dump.Encode(map[string]any{"session": j.Session, "k": j.K, "sha256": j.Hash, "case": j.Prompt[j.Split:]})
			}
			if parity && !judged[j.Session] {
				notSent++
				p.due, p.err = later(j.At, at), core.ErrNotSent
			} else {
				p.a, p.err = judge.Rule(j)
				if p.err != nil && parity && !*keepGoing && stop == nil {
					stop = fmt.Errorf("checkpoint %s#%d (sha256 %s): %w", j.Session, j.K, j.Hash, p.err)
				}
			}
			i := sort.Search(len(pending), func(i int) bool { return pending[i].due.After(p.due) })
			pending = slices.Insert(pending, i, p)
		}
	}
	// until runs the rulings and timers due by t, in time order, a ruling before a timer of the same time.
	until := func(t time.Time) {
		for stop == nil {
			due, timer := eng.NextDue()
			switch {
			case len(pending) > 0 && !pending[0].due.After(t) && (!timer || !pending[0].due.After(due)):
				p := pending[0]
				pending = pending[1:]
				put(eng.Tick(p.due), p.due)
				put(eng.Rule(p.job, p.a, p.err), p.due)
			case timer && !due.After(t):
				put(eng.Tick(due), due)
			default:
				return
			}
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
			scorer.rejects = map[string]bool{}
			for _, x := range events[i+1:] {
				if x.Type == "stream" {
					break
				}
				if x.Verdict == "reject" {
					scorer.rejects[x.ref()] = true
				} else if x.Verdict != "" {
					scorer.verdicts[x.ref()] = core.Verdict{Hold: x.Verdict == "hold", Categories: x.Categories}
				}
			}
			day = later(day, last)
			until(day)
			put(eng.Start(ev.Name, day), day)
		case "presence":
			t, err := time.Parse(time.RFC3339, ev.Time)
			if err != nil {
				return fmt.Errorf("presence of %s: %w", ev.Account, err)
			}
			if ev.Kind != "join" && ev.Kind != "leave" {
				return fmt.Errorf("presence of %s: kind %q is not join or leave", ev.Account, ev.Kind)
			}
			last = later(last, t)
			until(t)
			put(eng.Presence(ev.Account, ev.Kind == "join", t), t)
		case "line":
			t, err := time.Parse(time.RFC3339, ev.Time)
			if err != nil {
				return fmt.Errorf("line %s: %w", ev.ID, err)
			}
			last = later(last, t)
			if *bundle != "" && ev.Speaker == "" {
				return fmt.Errorf("line %s has no speaker, which the models need", ev.ID)
			}
			sit := ev.Session
			if !parity {
				sit = ""
			}
			until(t)
			line := ev.coreLine(t, sit)
			if line.Empty != (ev.Coverage == "dropped") {
				return fmt.Errorf("line %s: actual and recorded empty text disagree", ev.ref())
			}
			result := eng.Line(line)
			if *bundle == "" {
				if (result.Verdict == "reject") != (ev.Coverage == model.CoverageRejected) {
					return fmt.Errorf("line %s: actual and recorded rejection disagree", ev.ref())
				}
				_, folded := rec.folded[ev.ref()]
				if folded != (ev.Coverage == "fold") {
					return fmt.Errorf("line %s: actual and recorded folding disagree", ev.ref())
				}
			}
			put(result, t)
			if parity && ev.Session != "" && lastOf[ev.Session] == i {
				until(t)
				put(eng.End(ev.Session, t), t)
			}
		default:
			return fmt.Errorf("unknown event type %q", ev.Type)
		}
		if stop != nil {
			return stop
		}
	}
	end := last.Add(24 * time.Hour)
	until(end)
	put(eng.Close(end), end)
	until(end.Add(24 * time.Hour))
	if stop != nil {
		return stop
	}
	if len(pending) > 0 {
		return fmt.Errorf("%d judge calls left unanswered at the end", len(pending))
	}
	if err := rec.w.Flush(); err != nil {
		return err
	}
	if rec.err != nil {
		return rec.err
	}

	sf, err := os.Create(filepath.Join(*outDir, "sessions.jsonl"))
	if err != nil {
		return err
	}
	defer sf.Close()
	sw := bufio.NewWriter(sf)
	for _, s := range rec.sessions {
		b, _ := json.Marshal(s)
		sw.Write(append(b, '\n'))
	}
	if err := sw.Flush(); err != nil {
		return err
	}

	sum := summarize(*mode, rec, judge, notSent, exp, answers, parityHeldSets(events))
	sum.Copies = checkCopies(events, rec.folded)
	b, _ := json.MarshalIndent(sum, "", " ")
	if err := os.WriteFile(filepath.Join(*outDir, "summary.json"), append(b, '\n'), 0o640); err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// parityHeldSets is each input session's held lines, keyed as summarize keys a replayed session's.
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
			by[ev.Session] = append(by[ev.Session], ev.ID)
			stream[ev.Session] = cur
		}
	}
	out := map[string]bool{}
	for s, ids := range by {
		out[stream[s]+"/"+fmt.Sprint(ids)] = true
	}
	return out
}

type replaySummary struct {
	Mode         string         `json:"mode"`
	Sessions     int            `json:"sessions"`
	WithHeld     int            `json:"sessions_with_a_held_line"`
	Outcomes     map[string]int `json:"outcomes"`
	Checkpoints  map[string]int `json:"checkpoints_by_reason"`
	PromptsBuilt int            `json:"prompts_built"`
	Sent         int            `json:"sent"`
	NotSent      int            `json:"built_not_sent"`
	CapStopped   int            `json:"stopped_by_the_cap"`
	NoAnswer     int            `json:"sent_without_a_saved_answer"`
	Actions      map[string]int `json:"actions"`
	Reviews      int            `json:"review_queue"`
	Repeats      int            `json:"repeats"`
	// Copies: the input's lines that name the line they repeat (copy_of), and how the core folded them.
	Copies         *copyCheck    `json:"copies,omitempty"`
	Parity         *parityResult `json:"parity,omitempty"`
	LiveComparison *liveResult   `json:"live,omitempty"`
}

type copyCheck struct {
	InInput       int `json:"in_input"`
	FoldedAsNamed int `json:"folded_into_the_line_named"`
	FoldedOther   int `json:"folded_into_another_line"`
	NotFolded     int `json:"not_folded"`
	Rejected      int `json:"rejected_before_fold"`
	FoldedUnnamed int `json:"folded_without_copy_of"`
}

func checkCopies(events []replayEvent, folded map[string]string) *copyCheck {
	c := &copyCheck{}
	named := map[string]bool{}
	for _, ev := range events {
		if ev.Type != "line" || ev.CopyOf == "" {
			continue
		}
		c.InInput++
		named[ev.ref()] = true
		switch of, ok := folded[ev.ref()]; {
		case ev.Coverage == model.CoverageRejected:
			c.Rejected++
		case !ok:
			c.NotFolded++
		case of == ev.CopyOf:
			c.FoldedAsNamed++
		default:
			c.FoldedOther++
		}
	}
	for ref := range folded {
		if !named[ref] {
			c.FoldedUnnamed++
		}
	}
	if c.InInput == 0 && len(folded) == 0 {
		return nil
	}
	return c
}

type parityResult struct {
	PromptsCompared   int      `json:"prompts_compared"`
	PromptsEqual      int      `json:"prompts_equal"`
	PromptsOfExamples int      `json:"prompts_of_example_sessions"`
	PromptDiffs       []string `json:"prompt_differences"`
	MissingCheckpoint []string `json:"python_checkpoints_not_built"`
	SessionsCompared  int      `json:"sessions_compared"`
	SessionsEqual     int      `json:"sessions_equal"`
	SessionDiffs      []string `json:"session_differences"`
}

type liveResult struct {
	SameTextAsParity int `json:"checkpoints_with_a_parity_prompt"`
	SameTextAnswered int `json:"of_them_with_a_saved_answer"`
	DifferentText    int `json:"checkpoints_whose_text_differs"`
	WithAnswer       int `json:"checkpoints_with_a_saved_answer"`
	WithoutAnswer    int `json:"checkpoints_without_a_saved_answer"`
	// live sessions with a held line whose held lines are exactly one parity session's, and the others
	SameHeldLines  int `json:"sessions_holding_one_parity_sessions_lines"`
	OtherHeldLines int `json:"sessions_holding_other_sets"`
}

func summarize(mode string, rec *replayRecorder, judge *savedJudge, notSent int, exp expected,
	answers map[string]savedAnswer, pyHeld map[string]bool) replaySummary {
	s := replaySummary{Mode: mode, Sessions: len(rec.sessions), Outcomes: map[string]int{},
		Checkpoints: map[string]int{}, Actions: rec.actions, Reviews: rec.reviews, Repeats: rec.repeats, NotSent: notSent,
		NoAnswer: judge.missing}
	for _, st := range rec.sessions {
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
	for _, st := range rec.sessions {
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
	for _, st := range rec.sessions {
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
	for _, s := range exp.ExampleSessions {
		examples[s] = true
	}
	// Python's checkpoints per session, by held-line count
	pyKs := map[string][]int{}
	for id := range exp.Prompts {
		i := len(id) - 1
		for id[i] != '#' {
			i--
		}
		k, _ := strconv.Atoi(id[i+1:])
		pyKs[id[:i]] = append(pyKs[id[:i]], k)
	}
	got := map[string]core.SessionRecord{}
	for _, st := range rec.sessions {
		got[st.Session] = st
		if examples[st.Session] {
			for _, c := range st.Checkpoints {
				if c.Hash != "" {
					p.PromptsOfExamples++
				}
			}
			continue
		}
		var ks []int
		for _, c := range st.Checkpoints {
			id := st.Session + "#" + strconv.Itoa(c.K)
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
		want := pyKs[st.Session]
		sort.Ints(want)
		// Python built every checkpoint of the session; the core stops at a ban
		n := len(want)
		if st.Outcome == core.OutcomeBanned {
			n = len(ks)
		}
		for _, k := range want[:min(n, len(want))] {
			if !slices.Contains(ks, k) {
				p.MissingCheckpoint = append(p.MissingCheckpoint, st.Session+"#"+strconv.Itoa(k))
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
		p.SessionsCompared++
		g, ok := got[id]
		if !ok {
			p.SessionDiffs = append(p.SessionDiffs, id+": no such session in the replay")
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
			p.SessionsEqual++
		} else {
			p.SessionDiffs = append(p.SessionDiffs, fmt.Sprintf("%s: %v", id, diffs))
		}
	}
	return p
}
