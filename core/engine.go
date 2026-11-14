package core

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	DriverSittings bool   `toml:"driver_sittings"`
	Render         Render `toml:"render"`
	// CanonicalLink replaces each link on the plugin's allowlist (AllowLinks) in the text the fast model sees.
	CanonicalLink string `toml:"canonical_link"`
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
	// Mine is the ids of the player's lines the case shows, the lines a hold or ban may name; Cases is how many of the
	// owner's cases the prompt lists.
	Mine  []string
	Cases int
	end   int
	day   string
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
	allow  []string
	links  Links
	scorer Scorer
	rec    Recorder
	now    time.Time
	st     *stream
	open   map[string]*sitting
	live   map[string]*sitting // the account's sitting, live sittings
	online map[string]bool
	left   map[string]time.Time
	closed map[string]bool // sittings of this stream that ended by a ban
	banned map[string]bool // accounts banned in this stream
	calls  map[string]int
	lines  int
	// copies is each speaker's last line in each conversation and the time of its last copy, for repeats.
	copies map[copyKey]lastCopy
}

type copyKey struct{ speaker, conversation string }

type lastCopy struct {
	en *entry
	at time.Time
}

func NewEngine(cfg Config, texts Texts, scorer Scorer, rec Recorder) *Engine {
	if cfg.Zone == nil {
		cfg.Zone = time.UTC
	}
	return &Engine{cfg: cfg, texts: texts, scorer: scorer, rec: rec,
		open: map[string]*sitting{}, live: map[string]*sitting{}, online: map[string]bool{}, left: map[string]time.Time{},
		closed: map[string]bool{}, banned: map[string]bool{}, calls: map[string]int{}, copies: map[copyKey]lastCopy{}}
}

// Swap replaces the configuration, the judge's texts and the scorer (a new bundle). Open sittings keep their lines
// and rulings; their later checkpoints use the new texts and settings.
func (e *Engine) Swap(cfg Config, texts Texts, scorer Scorer) {
	if cfg.Zone == nil {
		cfg.Zone = time.UTC
	}
	e.cfg, e.texts, e.scorer = cfg, texts, scorer
	e.links, _ = NewLinks(e.allow, cfg.CanonicalLink)
}

// AllowLinks takes the plugin's allowlist of links: each link it covers is replaced by Config.CanonicalLink in the
// text the fast model sees. Until it is called, no link is replaced.
func (e *Engine) AllowLinks(allowed []string) error {
	l, err := NewLinks(allowed, e.cfg.CanonicalLink)
	if err != nil {
		return err
	}
	e.allow, e.links = allowed, l
	return nil
}

// Status is a count of the engine's state.
type Status struct {
	Stream   string `json:"stream"`
	Sittings int    `json:"open_sittings"`
	// Unruled counts the held lines of open sittings that no ruling covers yet; InFlight the judge calls running.
	Unruled  int `json:"held_unruled"`
	InFlight int `json:"judge_calls_in_flight"`
}

func (e *Engine) Status() Status {
	var st Status
	if e.st != nil {
		st.Stream = e.st.name
	}
	for _, s := range e.open {
		st.Sittings++
		st.Unruled += len(s.held) - s.ruled
		if s.inFlight != nil {
			st.InFlight++
		}
	}
	return st
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
	SH           *float64 `json:"sh,omitempty"`
	C            *float64 `json:"c,omitempty"`
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
	e.copies = map[copyKey]lastCopy{}
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

// streamFor makes the first stream when a line comes before any Start.
func (e *Engine) streamFor(at time.Time) {
	if e.st == nil {
		y, m, d := at.In(e.cfg.Zone).Date()
		e.st = newStream(stamp(at), time.Date(y, m, d, 0, 0, 0, 0, e.cfg.Zone))
	}
}

func copyKeyOf(l Line) copyKey {
	key := copyKey{l.Account, l.Conversation}
	if l.Account == "" {
		key.speaker = "name:" + l.Name
	}
	return key
}

// repeatOf is the line l repeats, or nil.
func (e *Engine) repeatOf(l Line) *entry {
	if c, ok := e.copies[copyKeyOf(l)]; ok && c.en.line.Text == l.Text && l.Time.Sub(c.at) <= RepeatWindow {
		return c.en
	}
	return nil
}

// barred tells whether the line's player is banned in this stream (their lines are held unscored).
func (e *Engine) barred(l Line) bool {
	if e.cfg.DriverSittings {
		return e.closed[l.Sitting]
	}
	return e.banned[l.Account]
}

// Prepare tells whether Line would score l now, and gives the line as the scorer gets it: its stream and number set,
// each allowlisted link replaced by the canonical one. A driver that scores outside the engine's lock scores that line
// and hands the verdict to LineScored; for a line Prepare does not score it calls Line. No line may be taken between
// a Prepare and its LineScored.
func (e *Engine) Prepare(l Line) (Line, bool) {
	e.streamFor(l.Time)
	if l.Context || e.repeatOf(l) != nil || e.barred(l) {
		return Line{}, false
	}
	return e.scorerLine(l), true
}

func (e *Engine) scorerLine(l Line) Line {
	if l.ID == "" {
		l.ID = "L" + strconv.Itoa(e.st.next+1)
	}
	l.Stream = e.st.name
	l.Text = e.links.Replace(l.Text)
	return l
}

// Line takes one line and gives its verdict, scoring it with the scorer.
func (e *Engine) Line(l Line) Result { return e.line(l, nil) }

// LineScored takes a line Prepare said to score, with the verdict (or the error) the driver got for it from the
// scorer.
func (e *Engine) LineScored(l Line, v Verdict, err error) Result { return e.line(l, &scored{v, err}) }

type scored struct {
	v   Verdict
	err error
}

func (e *Engine) line(l Line, given *scored) Result {
	r := e.advance(l.Time)
	e.now = l.Time
	e.streamFor(l.Time)
	key := copyKeyOf(l)
	if en := e.repeatOf(l); en != nil {
		r.Verdict = e.repeat(key, en, l)
		return r
	}
	if l.ID == "" {
		l.ID = "L" + strconv.Itoa(e.st.next+1)
	}
	if l.Sent < 1 {
		l.Sent = 1
	}
	rec := LineRecord{Stream: e.st.name, ID: l.ID, Time: stamp(l.Time), Channel: l.Channel,
		Conversation: l.Conversation, Account: l.Account, Name: l.Name, Target: l.Target, Text: l.Text, Verdict: "deliver"}
	en := &entry{line: l}
	if l.Ref != "" {
		en.refs = []string{l.Ref}
	}
	defer func() {
		en.verdict = rec.Verdict
		e.copies[key] = lastCopy{en, l.Time}
		e.rec.Record("line", rec)
		e.lines++
		if e.lines%256 == 0 {
			e.prune()
		}
	}()
	if l.Context {
		if given == nil {
			e.scorer.See(e.scorerLine(l))
		}
		rec.Context = true
		e.st.add(en)
		r.Verdict = rec.Verdict
		return r
	}
	s := e.sittingFor(l)
	if s == nil {
		// the player's sitting ended with a ban
		if given == nil {
			e.scorer.See(e.scorerLine(l))
		}
		en.sitting = l.Sitting
		if !e.cfg.DriverSittings {
			en.sitting = ""
		}
		e.st.add(en)
		rec.Sitting, rec.Verdict, rec.Why = en.sitting, "hold", "banned"
		r.Verdict = rec.Verdict
		return r
	}
	if given == nil {
		v, err := e.scorer.Score(e.scorerLine(l))
		given = &scored{v, err}
	}
	v := given.v
	if given.err != nil {
		v.Hold, v.Kinds = true, nil
		rec.Why = "scorer: " + given.err.Error()
	}
	en.sitting, en.held, en.kinds = s.id, v.Hold, v.Kinds
	e.st.add(en)
	s.mine = append(s.mine, en.pos)
	s.last = l.Time
	rec.Sitting, rec.Kinds, rec.SH, rec.C = s.id, v.Kinds, v.SH, v.C
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

// RepeatRecord is a line taken as a repeat of an earlier one: the earlier line's id, its count of sends with this
// one, and the verdict this copy got.
type RepeatRecord struct {
	Stream  string `json:"stream"`
	Of      string `json:"of"`
	Time    string `json:"time"`
	Account string `json:"account"`
	Ref     string `json:"ref,omitempty"`
	Sent    int    `json:"sent"`
	Verdict string `json:"verdict"`
}

// repeat counts l as one more send of the line en and gives it that line's verdict: a held line's copy is held with
// it and released with it, a copy of a line a ruling released is delivered. A repeat is not scored, adds no line to
// the stream or a conversation's window, and is no new held line for the checkpoints.
func (e *Engine) repeat(key copyKey, en *entry, l Line) string {
	en.line.Sent++
	e.copies[key] = lastCopy{en, l.Time}
	verdict := en.verdict
	if en.released {
		verdict = "deliver"
	}
	if verdict == "hold" && l.Ref != "" {
		en.refs = append(en.refs, l.Ref)
	}
	if s := e.open[en.sitting]; s != nil && !s.ending && l.Time.After(s.last) {
		s.last = l.Time
	}
	e.rec.Record("repeat", RepeatRecord{Stream: e.st.name, Of: en.line.ID, Time: stamp(l.Time), Account: l.Account,
		Ref: l.Ref, Sent: en.line.Sent, Verdict: verdict})
	return verdict
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
		prompt := e.texts.prompt(c.text)
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
				Hash: cp.Hash, Chars: cp.Chars, Mine: c.mine, Cases: len(e.texts.Examples), end: end, day: day}
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
		if errors.Is(err, ErrNotSent) {
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
			var ids, refs []string
			for _, p := range covered {
				en := s.st.get(p)
				en.released = true
				ids = append(ids, en.line.ID)
				refs = append(refs, en.refs...)
			}
			if len(ids) > 0 {
				s.released = append(s.released, ids...)
				r.Actions = append(r.Actions, Action{Kind: ActionRelease, Account: s.account, Name: s.name,
					Sitting: s.id, Lines: ids, Refs: refs})
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

	for k, c := range e.copies {
		if e.now.Sub(c.at) > RepeatWindow {
			delete(e.copies, k)
		}
	}
}
