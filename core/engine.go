package core

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"
)

// Config is the decision core's configuration.
type Config struct {
	// Schedule is the held-line counts of a sitting that bring a checkpoint; after its last one, every Every-th held
	// line does (0: none).
	Schedule []int `toml:"schedule"`
	Every    int   `toml:"every"`
	// Wait brings a checkpoint when the oldest held line not yet put to the judge has waited this long (0: never).
	Wait time.Duration `toml:"wait"`
	// SittingEnd is how long after the player's leave (or, with no leave known, their last line) the sitting ends.
	SittingEnd time.Duration `toml:"sitting_end"`
	// LastHeldView makes the sitting-end checkpoint show the sitting up to its last held line, not up to its end.
	LastHeldView bool `toml:"last_held_view"`
	// DriverSittings takes sittings from Line.Sitting and ends them on End, in place of presence and SittingEnd.
	DriverSittings bool `toml:"driver_sittings"`
	// TrustedGroups are the groups whose lines are never held or judged, only context.
	TrustedGroups []string `toml:"trusted_groups"`
	// The fast model sees a conversation's last ContextLines lines within ContextAge.
	ContextLines int           `toml:"context_lines"`
	ContextAge   time.Duration `toml:"context_age"`
	Render       Render        `toml:"render"`
	Links        Links         `toml:"links"`
	// The spend cap: a prompt over MaxPromptChars characters is not sent, and no more than MaxCallsPerDay calls are
	// made in a UTC day. 0 is no cap.
	MaxPromptChars int `toml:"max_prompt_chars"`
	MaxCallsPerDay int `toml:"max_calls_per_day"`
	// ReviewHeld puts on the review queue a sitting that ends unbanned with this many held lines.
	ReviewHeld int `toml:"review_held"`
	// Zone is the time zone of the case's times and day.
	Zone *time.Location `toml:"-"`
}

// Job is one checkpoint for the caller to put to the judge and hand back to Engine.Rule.
type Job struct {
	Sitting string
	Account string
	// K is the number of the sitting's held lines the checkpoint shows; Reason is what brought it.
	K      int
	Reason string
	// Line is the last line the checkpoint shows.
	Line   string
	Prompt string
	Hash   string
	Chars  int
	end    int
	day    string
}

// Result is what one call to the engine produced.
type Result struct {
	Verdict string // "deliver" or "hold", for Line
	Jobs    []*Job
	Actions []Action
}

func (r *Result) add(o Result) {
	r.Jobs = append(r.Jobs, o.Jobs...)
	r.Actions = append(r.Actions, o.Actions...)
}

type sitting struct {
	id, account, name string
	st                *stream
	start, last, left time.Time
	online            bool
	first             int
	mine, held        []int
	// included held lines (a prefix of held) have been put to a checkpoint; ruled ones have a ruling.
	included, ruled int
	released        []string
	kept            int
	inFlight        *Job
	waiting         []*Job
	banned          bool
	banLine         string
	warned          bool
	unjudged        bool
	ending          bool
	checkpoints     []CheckpointRecord
}

// Engine is the decision core. It is not safe for concurrent use; the server calls it under one lock.
type Engine struct {
	cfg    Config
	texts  Texts
	scorer Scorer
	rec    Recorder
	now    time.Time
	st     *stream
	convs  map[string][]Line
	open   map[string]*sitting
	live   map[string]*sitting // the account's sitting, live sittings
	online map[string]bool
	left   map[string]time.Time
	closed map[string]bool // sittings of this stream that ended by a ban
	banned map[string]bool // accounts banned in this stream
	calls  map[string]int
	lines  int
}

func NewEngine(cfg Config, texts Texts, scorer Scorer, rec Recorder) *Engine {
	if cfg.Zone == nil {
		cfg.Zone = time.UTC
	}
	return &Engine{cfg: cfg, texts: texts, scorer: scorer, rec: rec, convs: map[string][]Line{},
		open: map[string]*sitting{}, live: map[string]*sitting{}, online: map[string]bool{}, left: map[string]time.Time{},
		closed: map[string]bool{}, banned: map[string]bool{}, calls: map[string]int{}}
}

// Records.

type LineRecord struct {
	Stream       string   `json:"stream"`
	ID           string   `json:"id"`
	Time         string   `json:"time"`
	Channel      string   `json:"channel"`
	Conversation string   `json:"conversation"`
	Account      string   `json:"account"`
	Name         string   `json:"name"`
	Target       string   `json:"target,omitempty"`
	Text         string   `json:"text"`
	Sitting      string   `json:"sitting,omitempty"`
	Context      bool     `json:"context,omitempty"`
	Verdict      string   `json:"verdict"`
	Kinds        []string `json:"kinds,omitempty"`
	Score        float64  `json:"score,omitempty"`
	Why          string   `json:"why,omitempty"`
}

type CheckpointRecord struct {
	Time    string `json:"time"`
	Sitting string `json:"sitting"`
	K       int    `json:"k"`
	Reason  string `json:"reason"`
	Line    string `json:"line"`
	Chars   int    `json:"chars"`
	Hash    string `json:"sha256,omitempty"`
	Sent    bool   `json:"sent"`
	NotSent string `json:"not_sent,omitempty"`
	Ruling  string `json:"ruling,omitempty"`
}

type RulingRecord struct {
	Time    string `json:"time"`
	Sitting string `json:"sitting"`
	K       int    `json:"k"`
	Hash    string `json:"sha256"`
	Answer  Answer `json:"answer"`
	Error   string `json:"error,omitempty"`
	Covers  int    `json:"covers"`
}

type SittingRecord struct {
	Time        string             `json:"time"`
	Stream      string             `json:"stream"`
	Sitting     string             `json:"sitting"`
	Account     string             `json:"account"`
	Start       string             `json:"start"`
	Lines       int                `json:"lines"`
	Held        []string           `json:"held"`
	Outcome     string             `json:"outcome"`
	BanLine     string             `json:"ban_line,omitempty"`
	Released    []string           `json:"released"`
	Unruled     int                `json:"unruled"`
	Checkpoints []CheckpointRecord `json:"checkpoints"`
	Review      string             `json:"review,omitempty"`
}

type ReviewRecord struct {
	Time    string `json:"time"`
	Sitting string `json:"sitting"`
	Account string `json:"account"`
	Name    string `json:"name"`
	Why     string `json:"why"`
}

// PromptHash is the sha256 of a prompt's UTF-8 text, in hex: the key a prompt is recorded and answered by.
func PromptHash(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// Outcomes of a sitting.
const (
	OutcomeBanned   = "banned"   // the judge banned the player
	OutcomeHeld     = "held"     // held lines stayed held: ruled hold, or never ruled
	OutcomeReleased = "released" // every ruled held line was released
	OutcomeNone     = "none"     // no line was held
)

// Start begins a new stream (the game server started): every open sitting ends.
func (e *Engine) Start(name string, at time.Time) Result {
	r := e.advance(at)
	e.now = at
	for _, s := range e.sortedOpen() {
		r.add(e.endSitting(s))
	}
	y, m, d := at.In(e.cfg.Zone).Date()
	e.st = newStream(name, time.Date(y, m, d, 0, 0, 0, 0, e.cfg.Zone))
	e.convs = map[string][]Line{}
	e.closed = map[string]bool{}
	e.banned = map[string]bool{}
	e.online = map[string]bool{}
	e.left = map[string]time.Time{}
	return r
}

// Presence records a join or leave.
func (e *Engine) Presence(account string, join bool, at time.Time) Result {
	r := e.advance(at)
	e.now = at
	e.online[account] = join
	if join {
		delete(e.left, account)
	} else {
		e.left[account] = at
	}
	return r
}

// Tick runs what is due by now: waiting-time checkpoints and sitting ends.
func (e *Engine) Tick(now time.Time) Result {
	r := e.advance(now)
	e.now = now
	return r
}

// End ends a sitting the driver names (Config.DriverSittings).
func (e *Engine) End(id string, at time.Time) Result {
	r := e.advance(at)
	e.now = at
	if s := e.open[id]; s != nil && !s.ending {
		r.add(e.endSitting(s))
	}
	return r
}

// Close ends every open sitting at the given time.
func (e *Engine) Close(at time.Time) Result {
	r := e.advance(at)
	e.now = at
	for _, s := range e.sortedOpen() {
		if !s.ending {
			r.add(e.endSitting(s))
		}
	}
	return r
}

func (e *Engine) sortedOpen() []*sitting {
	var out []*sitting
	for _, s := range e.open {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b *sitting) int {
		if c := a.start.Compare(b.start); c != 0 {
			return c
		}
		if a.first != b.first {
			return a.first - b.first
		}
		return compareStrings(a.id, b.id)
	})
	return out
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (e *Engine) trusted(groups []string) bool {
	for _, g := range groups {
		if slices.Contains(e.cfg.TrustedGroups, g) {
			return true
		}
	}
	return false
}

// context is the conversation's recent lines for the scorer, and the window with this line added.
func (e *Engine) context(l Line) []Line {
	w := e.convs[l.Conversation]
	cut := 0
	for cut < len(w) && l.Time.Sub(w[cut].Time) > e.cfg.ContextAge {
		cut++
	}
	w = w[cut:]
	out := make([]Line, len(w))
	for i, c := range w {
		c.Text = e.cfg.Links.Replace(c.Text)
		out[i] = c
	}
	w = append(w, l)
	if len(w) > e.cfg.ContextLines {
		w = w[len(w)-e.cfg.ContextLines:]
	}
	e.convs[l.Conversation] = w
	return out
}

// Line takes one line and gives its verdict.
func (e *Engine) Line(l Line) Result {
	r := e.advance(l.Time)
	e.now = l.Time
	if e.st == nil {
		y, m, d := l.Time.In(e.cfg.Zone).Date()
		e.st = newStream(stamp(l.Time), time.Date(y, m, d, 0, 0, 0, 0, e.cfg.Zone))
	}
	if l.ID == "" {
		l.ID = "L" + strconv.Itoa(e.st.next+1)
	}
	if l.Sent < 1 {
		l.Sent = 1
	}
	ctx := e.context(l)
	rec := LineRecord{Stream: e.st.name, ID: l.ID, Time: stamp(l.Time), Channel: l.Channel,
		Conversation: l.Conversation, Account: l.Account, Name: l.Name, Target: l.Target, Text: l.Text, Verdict: "deliver"}
	en := &entry{line: l}
	defer func() {
		e.rec.Record("line", rec)
		e.lines++
		if e.lines%256 == 0 {
			e.prune()
		}
	}()
	if l.Context || e.trusted(l.Groups) {
		rec.Context = true
		e.st.add(en)
		r.Verdict = rec.Verdict
		return r
	}
	s := e.sittingFor(l)
	if s == nil {
		// the player's sitting ended with a ban
		en.sitting = l.Sitting
		if !e.cfg.DriverSittings {
			en.sitting = ""
		}
		e.st.add(en)
		rec.Sitting, rec.Verdict, rec.Why = en.sitting, "hold", "banned"
		r.Verdict = rec.Verdict
		return r
	}
	scored := l
	scored.Text = e.cfg.Links.Replace(l.Text)
	v, err := e.scorer.Score(scored, ctx)
	if err != nil {
		v = Verdict{Hold: true}
		rec.Why = "scorer: " + err.Error()
	}
	en.sitting, en.held, en.kinds = s.id, v.Hold, v.Kinds
	e.st.add(en)
	s.mine = append(s.mine, en.pos)
	s.last = l.Time
	rec.Sitting, rec.Kinds, rec.Score = s.id, v.Kinds, v.Score
	if v.Hold {
		rec.Verdict = "hold"
		s.held = append(s.held, en.pos)
		if e.scheduled(len(s.held)) {
			r.add(e.checkpoint(s, "held "+strconv.Itoa(len(s.held)), en.pos))
		}
	}
	r.Verdict = rec.Verdict
	return r
}

func (e *Engine) sittingFor(l Line) *sitting {
	if e.cfg.DriverSittings {
		if l.Sitting == "" {
			panic(fmt.Sprintf("line %s of %s has no sitting", l.ID, l.Account))
		}
		if e.closed[l.Sitting] {
			return nil
		}
		if s := e.open[l.Sitting]; s != nil && !s.ending {
			return s
		}
		return e.newSitting(l.Sitting, l)
	}
	if e.banned[l.Account] {
		return nil
	}
	if s := e.live[l.Account]; s != nil && !s.ending {
		return s
	}
	return e.newSitting(l.Account+"@"+stamp(l.Time), l)
}

func (e *Engine) newSitting(id string, l Line) *sitting {
	s := &sitting{id: id, account: l.Account, name: l.Name, st: e.st, start: l.Time, last: l.Time, first: e.st.next}
	e.open[id] = s
	if !e.cfg.DriverSittings {
		e.live[l.Account] = s
	}
	e.rec.Record("sitting_start", struct {
		Time    string `json:"time"`
		Stream  string `json:"stream"`
		Sitting string `json:"sitting"`
		Account string `json:"account"`
	}{stamp(l.Time), e.st.name, id, l.Account})
	return s
}

func (e *Engine) scheduled(n int) bool {
	if slices.Contains(e.cfg.Schedule, n) {
		return true
	}
	if e.cfg.Every <= 0 || len(e.cfg.Schedule) == 0 {
		return false
	}
	last := slices.Max(e.cfg.Schedule)
	return n > last && (n-last)%e.cfg.Every == 0
}

// checkpoint builds the judge's prompt for the sitting as of stream position end and returns it as a job, unless the
// spend cap stops it.
func (e *Engine) checkpoint(s *sitting, reason string, end int) Result {
	covers := 0
	for covers < len(s.held) && s.held[covers] <= end {
		covers++
	}
	if covers <= s.included {
		return Result{}
	}
	s.included = covers
	v := view{st: s.st, sitting: s.id, end: end, held: s.held[:covers]}
	for _, p := range s.mine {
		if p <= end {
			v.mine = append(v.mine, p)
		}
	}
	cp := CheckpointRecord{Time: stamp(e.now), Sitting: s.id, K: covers, Reason: reason, Line: s.st.get(end).line.ID}
	c, err := e.cfg.Render.render(v)
	day := e.now.UTC().Format(time.DateOnly)
	var job *Job
	switch {
	case err != nil:
		cp.NotSent = err.Error()
	default:
		prompt := e.texts.prompt(c)
		cp.Hash = PromptHash(prompt)
		cp.Chars = utf8.RuneCountInString(prompt)
		switch {
		case e.cfg.MaxPromptChars > 0 && cp.Chars > e.cfg.MaxPromptChars:
			cp.NotSent = "over the size cap"
		case e.cfg.MaxCallsPerDay > 0 && e.calls[day] >= e.cfg.MaxCallsPerDay:
			cp.NotSent = "over the day's calls"
		default:
			cp.Sent = true
			e.calls[day]++
			job = &Job{Sitting: s.id, Account: s.account, K: covers, Reason: reason, Line: cp.Line, Prompt: prompt,
				Hash: cp.Hash, Chars: cp.Chars, end: end, day: day}
		}
	}
	e.rec.Record("checkpoint", cp)
	s.checkpoints = append(s.checkpoints, cp)
	if job == nil {
		s.unjudged = true
		return Result{}
	}
	if s.inFlight != nil {
		s.waiting = append(s.waiting, job)
		return Result{}
	}
	s.inFlight = job
	return Result{Jobs: []*Job{job}}
}

// Rule takes the judge's answer on a job (or the error that stopped it) and carries out the ruling.
func (e *Engine) Rule(job *Job, a Answer, err error) Result {
	s := e.open[job.Sitting]
	if s == nil || s.inFlight != job {
		panic(fmt.Sprintf("ruling on %s#%d, which is not the sitting's call in flight", job.Sitting, job.K))
	}
	var r Result
	rr := RulingRecord{Time: stamp(e.now), Sitting: s.id, K: job.K, Hash: job.Hash, Answer: a}
	ruling := ""
	switch {
	case err != nil:
		rr.Error = err.Error()
		if err == ErrNotSent {
			e.calls[job.day]--
		}
		s.unjudged = true
	case a.Ruling != RulingNothing && a.Ruling != RulingHold && a.Ruling != RulingBan:
		rr.Error = fmt.Sprintf("the judge answered %q", a.Ruling)
		s.unjudged = true
	default:
		ruling = a.Ruling
		covered := s.held[s.ruled:job.K]
		s.ruled = job.K
		rr.Covers = len(covered)
		switch ruling {
		case RulingNothing:
			var ids []string
			for _, p := range covered {
				ids = append(ids, s.st.get(p).line.ID)
			}
			if len(ids) > 0 {
				s.released = append(s.released, ids...)
				r.Actions = append(r.Actions, Action{Kind: ActionRelease, Account: s.account, Name: s.name,
					Sitting: s.id, Lines: ids})
			}
		case RulingHold:
			s.kept += len(covered)
			if !s.warned {
				s.warned = true
				r.Actions = append(r.Actions, Action{Kind: ActionWarn, Account: s.account, Name: s.name, Sitting: s.id})
			}
		case RulingBan:
			s.kept += len(covered)
			s.banned, s.banLine, s.ending = true, job.Line, true
			r.Actions = append(r.Actions, Action{Kind: ActionBan, Account: s.account, Name: s.name, Sitting: s.id})
		}
	}
	for i := range s.checkpoints {
		if s.checkpoints[i].Hash == job.Hash && s.checkpoints[i].K == job.K && s.checkpoints[i].Sent {
			s.checkpoints[i].Ruling = ruling
		}
	}
	e.rec.Record("ruling", rr)
	for _, act := range r.Actions {
		e.rec.Record("action", act)
	}
	s.inFlight = nil
	if s.banned {
		for _, w := range s.waiting {
			e.calls[w.day]--
			e.rec.Record("checkpoint_dropped", struct {
				Sitting string `json:"sitting"`
				K       int    `json:"k"`
				Hash    string `json:"sha256"`
			}{w.Sitting, w.K, w.Hash})
		}
		s.waiting = nil
	}
	if len(s.waiting) > 0 {
		s.inFlight, s.waiting = s.waiting[0], s.waiting[1:]
		r.Jobs = append(r.Jobs, s.inFlight)
		return r
	}
	if s.ending {
		e.closeSitting(s)
	}
	return r
}

// endSitting puts the sitting's held lines not yet put to the judge to a last checkpoint, and closes it once no
// call is in flight.
func (e *Engine) endSitting(s *sitting) Result {
	s.ending = true
	var r Result
	if len(s.held) > s.included {
		end := s.st.next - 1
		if e.cfg.LastHeldView {
			end = s.held[len(s.held)-1]
		}
		r = e.checkpoint(s, "sitting end", end)
	}
	if s.inFlight == nil {
		e.closeSitting(s)
	}
	return r
}

func (e *Engine) closeSitting(s *sitting) {
	delete(e.open, s.id)
	if e.live[s.account] == s {
		delete(e.live, s.account)
	}
	if s.banned && s.st == e.st {
		e.closed[s.id] = true
		e.banned[s.account] = true
	}
	unruled := len(s.held) - s.ruled
	out := OutcomeNone
	switch {
	case s.banned:
		out = OutcomeBanned
	case s.kept > 0 || unruled > 0:
		out = OutcomeHeld
	case len(s.released) > 0:
		out = OutcomeReleased
	}
	review := ""
	switch {
	case s.banned:
		review = "ban"
	case len(s.held) >= e.cfg.ReviewHeld && e.cfg.ReviewHeld > 0:
		review = strconv.Itoa(len(s.held)) + " lines held"
	case s.unjudged:
		review = "the judge was not called on every checkpoint"
	}
	held := make([]string, len(s.held))
	for i, p := range s.held {
		held[i] = s.st.get(p).line.ID
	}
	e.rec.Record("sitting_end", SittingRecord{Time: stamp(e.now), Stream: s.st.name, Sitting: s.id, Account: s.account,
		Start: stamp(s.start), Lines: len(s.mine), Held: held, Outcome: out, BanLine: s.banLine,
		Released: s.released, Unruled: unruled, Checkpoints: s.checkpoints, Review: review})
	if review != "" {
		e.rec.Record("review", ReviewRecord{Time: stamp(e.now), Sitting: s.id, Account: s.account, Name: s.name,
			Why: review})
	}
}

// advance runs the timers due by now, in time order: a held line that has waited Wait, and a sitting's end.
func (e *Engine) advance(now time.Time) Result {
	var r Result
	for {
		var next *sitting
		var at time.Time
		wait := false
		for _, s := range e.sortedOpen() {
			if s.ending {
				continue
			}
			if e.cfg.Wait > 0 && s.inFlight == nil && len(s.held) > s.included {
				due := s.st.get(s.held[s.included]).line.Time.Add(e.cfg.Wait)
				if next == nil || due.Before(at) {
					next, at, wait = s, due, true
				}
			}
			if !e.cfg.DriverSittings && !e.online[s.account] {
				from := s.last
				if t, ok := e.left[s.account]; ok && t.After(from) {
					from = t
				}
				due := from.Add(e.cfg.SittingEnd)
				if next == nil || due.Before(at) {
					next, at, wait = s, due, false
				}
			}
		}
		if next == nil || at.After(now) {
			return r
		}
		e.now = at
		if wait {
			r.add(e.checkpoint(next, "waited", next.st.next-1))
		} else {
			r.add(e.endSitting(next))
		}
	}
}

// prune drops the stream's lines that no open sitting can show and the conversation windows past their age.
func (e *Engine) prune() {
	if e.st == nil {
		return
	}
	back := max(e.cfg.Render.Before, e.cfg.Render.CutBefore)
	from := e.st.next - back
	for _, s := range e.open {
		if s.st == e.st {
			from = min(from, s.first-back)
		}
	}
	e.st.prune(max(0, from), back)
	for k, w := range e.convs {
		if len(w) == 0 || e.now.Sub(w[len(w)-1].Time) > e.cfg.ContextAge {
			delete(e.convs, k)
		}
	}
}
