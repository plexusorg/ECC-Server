package core

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Config is the decision core's configuration.
type Config struct {
	// Schedule is the held-line counts of a session that bring a checkpoint; after its last one, every Every-th held
	// line does (0: none).
	Schedule []int `toml:"schedule"`
	Every    int   `toml:"every"`
	// Wait brings a checkpoint when the oldest held line not yet put to the judge has waited this long (0: never).
	Wait time.Duration `toml:"wait"`
	// SessionEnd is how long after the player's leave (or, with no leave known, their last line) the session ends.
	SessionEnd time.Duration `toml:"session_end"`
	// LastHeldView makes the session-end checkpoint show the session up to its last held line, not up to its end.
	LastHeldView bool `toml:"last_held_view"`
	// DriverSessions takes sessions from Line.Session and ends them on End, in place of presence and SessionEnd.
	DriverSessions bool   `toml:"driver_sessions"`
	Render         Render `toml:"render"`
	// CanonicalLink replaces each link on the plugin's allowlist (AllowLinks) in the text the fast model sees.
	CanonicalLink string `toml:"canonical_link"`
	// The spend cap: a prompt over MaxPromptChars characters is not sent, and no more than MaxCallsPerDay calls are
	// made in a UTC day. 0 is no cap.
	MaxPromptChars int `toml:"max_prompt_chars"`
	MaxCallsPerDay int `toml:"max_calls_per_day"`
	// ReviewHeld puts on the review queue a session that ends unbanned with this many held lines.
	ReviewHeld int `toml:"review_held"`
	// Channels is the channels a line may have (bundle.json `channels`); a line on another channel is held, never
	// scored or shown, and puts its session on the review queue. Empty: any channel.
	Channels []string `toml:"channels"`
	// RetryAfter: a checkpoint the judge could not be reached for (ErrPending) is put again this long after.
	RetryAfter time.Duration `toml:"retry_after"`
}

// Job is one checkpoint for the caller to put to the judge and hand back to Engine.Rule.
type Job struct {
	Session string
	Account string
	// K is the number of the session's held lines the checkpoint shows; Reason is what brought it.
	K      int
	Reason string
	// Line is the last line the checkpoint shows.
	Line   string
	Prompt string
	// Split is the length in bytes of the prompt's part the owner's cases end, the part every prompt shares: the
	// question and the cases, before SepNew.
	Split int
	Hash  string
	Chars int
	// At is when the checkpoint was made.
	At time.Time
	// Mine is the ids of the player's lines the case shows, the lines a hold or ban may name; Cases is how many of the
	// owner's cases the prompt lists.
	Mine  []string
	Cases int
	// ProvisionalBan belongs to the judge driver while this exact job waits for confirmation.
	// It is host state, not model JSON. Dropping the job drops this state.
	ProvisionalBan *Answer `json:"-"`
	end            int
	day            string
}

// Result is what one call to the engine produced.
type Result struct {
	Verdict string // "deliver", "hold" or final nonpunitive "reject", for Line
	Jobs    []*Job
	Actions []Action
}

func (r *Result) add(o Result) {
	r.Jobs = append(r.Jobs, o.Jobs...)
	r.Actions = append(r.Actions, o.Actions...)
}

type session struct {
	// name is the judge's name for the player (their first line's); speaker their account name (Line.Speaker), which
	// actions carry.
	id, account, name, speaker string
	st                         *stream
	start, last, left          time.Time
	online                     bool
	first                      int
	mine, held                 []int
	// unscored is the positions of the player's lines held while they wait to be scored (ErrLater), oldest first; while
	// there is one, no checkpoint is put, and due marks a checkpoint the held count brought meanwhile.
	unscored []int
	due      bool
	// included held lines (a prefix of held) have been put to a checkpoint; ruled ones have a ruling.
	included, ruled int
	released        []string
	kept            int
	inFlight        *Job
	waiting         []*Job
	// parked: the call in flight could not reach the judge; it is put again at retryAt.
	parked  bool
	retryAt time.Time
	// unknown counts the player's lines on channels outside Config.Channels.
	unknown       int
	banned        bool
	banLine       string
	warned        bool
	unjudged      bool
	ending        bool
	reviewReasons []string
	checkpoints   []CheckpointRecord
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
	open   map[string]*session
	live   map[string]*session // the account's session, live sessions
	online map[string]bool
	left   map[string]time.Time
	closed map[string]bool // sessions of this stream that ended by a ban
	banned map[string]bool // accounts banned in this stream
	calls  map[string]int
	lines  int
	// arrived counts every line taken, repeats included: the order a release lists its refs in.
	arrived int
	// copies is, per speaker, channel, target and text, the last line with that text and the time of its last copy,
	// for repeats.
	copies map[copyKey]lastCopy
	// unknown counts the lines taken on each channel outside Config.Channels.
	unknown map[string]int
	// later is the lines waiting to be scored (ErrLater), by Line.Ref.
	later map[string]*entry
}

// copyKey is what a repeat shares with the line it repeats: the speaker's shown name (lower case), the channel, the
// target and the text, as the training pipeline's dossiers fold repeats.
type copyKey struct{ speaker, channel, target, text string }

type lastCopy struct {
	en *entry
	at time.Time
}

func NewEngine(cfg Config, texts Texts, scorer Scorer, rec Recorder) *Engine {
	return &Engine{cfg: cfg, texts: texts, scorer: scorer, rec: rec,
		open: map[string]*session{}, live: map[string]*session{}, online: map[string]bool{}, left: map[string]time.Time{},
		closed: map[string]bool{}, banned: map[string]bool{}, calls: map[string]int{}, copies: map[copyKey]lastCopy{},
		unknown: map[string]int{}, later: map[string]*entry{}}
}

// Swap replaces the configuration, the judge's texts and the scorer (a new bundle). Open sessions keep their lines
// and rulings; their later checkpoints use the new texts and settings.
func (e *Engine) Swap(cfg Config, texts Texts, scorer Scorer) {
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
	Sessions int    `json:"open_sessions"`
	// Unruled counts the held lines of open sessions that no ruling covers yet; InFlight the judge calls running;
	// Pending the checkpoints waiting to be put again after the judge could not be reached.
	Unruled int `json:"held_unruled"`
	// Unscored counts the held lines waiting to be scored (ErrLater).
	Unscored int `json:"held_unscored"`
	InFlight int `json:"judge_calls_in_flight"`
	Pending  int `json:"checkpoints_pending"`
	// UnknownChannels counts, by channel, the lines taken on channels outside the bundle's.
	UnknownChannels map[string]int `json:"unknown_channels"`
}

func (e *Engine) Status() Status {
	st := Status{UnknownChannels: map[string]int{}}
	if e.st != nil {
		st.Stream = e.st.name
	}
	for _, s := range e.open {
		st.Sessions++
		st.Unruled += len(s.held) - s.ruled
		st.Unscored += len(s.unscored)
		switch {
		case s.parked:
			st.Pending++
		case s.inFlight != nil:
			st.InFlight++
		}
	}
	for c, n := range e.unknown {
		st.UnknownChannels[c] = n
	}
	return st
}

// Known tells whether a line may have this channel (Config.Channels).
func (e *Engine) Known(channel string) bool {
	return len(e.cfg.Channels) == 0 || slices.Contains(e.cfg.Channels, channel)
}

// at moves the engine's clock to t, never back.
func (e *Engine) at(t time.Time) {
	if t.After(e.now) {
		e.now = t
	}
}

// Records.

type LineRecord struct {
	Stream       string   `json:"stream"`
	ID           string   `json:"id"`
	Ref          string   `json:"ref,omitempty"`
	Time         string   `json:"time"`
	Channel      string   `json:"channel"`
	Conversation string   `json:"conversation"`
	Account      string   `json:"account"`
	Name         string   `json:"name"`
	RawName      string   `json:"raw_name,omitempty"`
	Target       string   `json:"target,omitempty"`
	Text         string   `json:"text"`
	Raw          string   `json:"raw"`
	Session      string   `json:"session,omitempty"`
	Context      bool     `json:"context,omitempty"`
	Verdict      string   `json:"verdict"`
	Categories   []string `json:"categories,omitempty"`
	SH           *float64 `json:"sh,omitempty"`
	C            *float64 `json:"c,omitempty"`
	Why          string   `json:"why,omitempty"`
}

type CheckpointRecord struct {
	Time    string `json:"time"`
	Session string `json:"session"`
	K       int    `json:"k"`
	Reason  string `json:"reason"`
	Line    string `json:"line"`
	LineRef string `json:"line_ref,omitempty"`
	Chars   int    `json:"chars"`
	Hash    string `json:"sha256,omitempty"`
	Sent    bool   `json:"sent"`
	NotSent string `json:"not_sent,omitempty"`
	Ruling  string `json:"ruling,omitempty"`
}

type RulingRecord struct {
	Time      string `json:"time"`
	Session   string `json:"session"`
	K         int    `json:"k"`
	Hash      string `json:"sha256"`
	Answer    Answer `json:"answer"`
	Effective string `json:"effective,omitempty"`
	Review    string `json:"review,omitempty"`
	Error     string `json:"error,omitempty"`
	// Pending: the judge could not be reached; the checkpoint is put again at Retry.
	Pending bool   `json:"pending,omitempty"`
	Retry   string `json:"retry,omitempty"`
	Covers  int    `json:"covers"`
}

type SessionRecord struct {
	Time        string             `json:"time"`
	Stream      string             `json:"stream"`
	Session     string             `json:"session"`
	Account     string             `json:"account"`
	Start       string             `json:"start"`
	Lines       int                `json:"lines"`
	Held        []string           `json:"held"`
	HeldRefs    []string           `json:"held_refs"`
	Outcome     string             `json:"outcome"`
	BanLine     string             `json:"ban_line,omitempty"`
	Released    []string           `json:"released"`
	Unruled     int                `json:"unruled"`
	Checkpoints []CheckpointRecord `json:"checkpoints"`
	Review      string             `json:"review,omitempty"`
}

type ReviewRecord struct {
	Time    string `json:"time"`
	Session string `json:"session"`
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

// Outcomes of a session.
const (
	OutcomeBanned   = "banned"   // the judge banned the player
	OutcomeHeld     = "held"     // held lines stayed held: ruled hold, or never ruled
	OutcomeReleased = "released" // every ruled held line was released
	OutcomeNone     = "none"     // no line was held
)

// Start begins a new stream (the game server started): every open session ends.
func (e *Engine) Start(name string, at time.Time) Result {
	r := e.advance(at)
	e.at(at)
	for _, s := range e.sortedOpen() {
		r.add(e.endSession(s))
	}
	e.st = newStream(name)
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
	e.at(at)
	e.online[account] = join
	if join {
		delete(e.left, account)
	} else {
		e.left[account] = at
	}
	return r
}

// Tick runs what is due by now: waiting-time checkpoints and session ends.
func (e *Engine) Tick(now time.Time) Result {
	r := e.advance(now)
	e.at(now)
	return r
}

// NextDue is the time of the earliest timer the engine will run (a waiting-time checkpoint, a session's end, a
// checkpoint to put again), if any.
func (e *Engine) NextDue() (time.Time, bool) {
	s, at, _ := e.due()
	return at, s != nil
}

// End ends a session the driver names (Config.DriverSessions).
func (e *Engine) End(id string, at time.Time) Result {
	r := e.advance(at)
	e.at(at)
	if s := e.open[id]; s != nil && !s.ending {
		r.add(e.endSession(s))
	}
	return r
}

// Close ends every open session at the given time.
func (e *Engine) Close(at time.Time) Result {
	r := e.advance(at)
	e.at(at)
	for _, s := range e.sortedOpen() {
		if !s.ending {
			r.add(e.endSession(s))
		}
	}
	return r
}

func (e *Engine) sortedOpen() []*session {
	var out []*session
	for _, s := range e.open {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b *session) int {
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
		e.st = newStream(stamp(at))
	}
}

// RepeatKey is a line's repeat key as bundle.json text.repeat writes it: the name lowercased, the channel (with
// ">" and the target where the target counts), the text after step 6.
func RepeatKey(l Line) [3]string {
	k := copyKeyOf(l)
	ch := k.channel
	if k.target != "" {
		ch += ">" + k.target
	}
	return [3]string{k.speaker, ch, k.text}
}

// copyKeyOf is a line's repeat key (bundle.json text.repeat): the target counts on msg and mail ("unknown" when there
// is none, as the dossiers wrote it) and on nick, on no other channel.
func copyKeyOf(l Line) copyKey {
	target := ""
	switch l.Channel {
	case "msg", "mail":
		target = l.Target
		if target == "" {
			target = "unknown"
		}
	case "nick":
		target = l.Target
	}
	return copyKey{Lower(l.KeyName), l.Channel, target, l.Key}
}

// withinRepeat tells whether a line at t is within RepeatWindow of a copy at last, in whole seconds as the logs time
// lines.
func withinRepeat(t, last time.Time) bool {
	return t.Unix()-last.Unix() <= int64(RepeatWindow/time.Second)
}

// repeatOf is the line l repeats, or nil.
func (e *Engine) repeatOf(l Line) *entry {
	if c, ok := e.copies[copyKeyOf(l)]; ok && withinRepeat(l.Time, c.at) {
		return c.en
	}
	return nil
}

// barred tells whether the line's player is banned in this stream (their lines are held unscored).
func (e *Engine) barred(l Line) bool {
	if e.cfg.DriverSessions {
		return e.closed[l.Session]
	}
	return e.banned[l.Account]
}

// Prepare tells whether Line would score l now, and gives the line as the scorer gets it: its stream and number set,
// each allowlisted link replaced by the canonical one. A driver that scores outside the engine's lock scores that line
// and hands the verdict to LineScored; for a line Prepare does not score it calls Line. No line may be taken between
// a Prepare and its LineScored.
func (e *Engine) Prepare(l Line) (Line, bool) {
	e.streamFor(l.Time)
	if l.Context || !e.Known(l.Channel) || l.Empty || e.barred(l) {
		return Line{}, false
	}
	if e.rejectReason(l) != "" || e.repeatOf(l) != nil {
		return Line{}, false
	}
	return e.scorerLine(l), true
}

func (e *Engine) rejectReason(l Line) string {
	if l.Context {
		return ""
	}
	if SourceShortened(l.Raw, l.Channel) {
		return "text preparation would discard content"
	}
	if sc, ok := e.scorer.(CoverageScorer); ok {
		return sc.RejectReason(e.scorerLine(l))
	}
	return ""
}

func (e *Engine) scorerLine(l Line) Line {
	if l.ID == "" {
		l.ID = "L" + strconv.Itoa(e.st.next+1)
	}
	l.Stream = e.st.name
	l.Text = e.links.Replace(ModelText(l.Text))
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
	e.at(l.Time)
	e.streamFor(l.Time)
	e.arrived++
	if !e.Known(l.Channel) {
		r.Verdict = e.unknownLine(l)
		return r
	}
	// LineScored follows a successful Prepare; its scorer already remembered the validated target.
	if given == nil {
		if why := e.rejectReason(l); why != "" {
			e.rec.Record("line", LineRecord{Stream: e.st.name, Ref: l.Ref, Time: stamp(l.Time), Channel: l.Channel,
				Conversation: l.Conversation, Account: l.Account, Name: l.Name, RawName: l.RawName, Target: l.Target,
				Text: l.Text, Raw: l.Raw, Verdict: "reject", Why: why})
			r.Verdict = "reject"
			return r
		}
	}
	if l.Empty {
		// preparation emptied the text: delivered, not scored, no line of the stream
		e.rec.Record("line", LineRecord{Stream: e.st.name, Ref: l.Ref, Time: stamp(l.Time), Channel: l.Channel,
			Conversation: l.Conversation, Account: l.Account, Name: l.Name, RawName: l.RawName, Target: l.Target,
			Raw: l.Raw, Context: l.Context, Verdict: "deliver", Why: "empty after preparation"})
		r.Verdict = "deliver"
		return r
	}
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
	rec := LineRecord{Stream: e.st.name, ID: l.ID, Ref: l.Ref, Time: stamp(l.Time), Channel: l.Channel,
		Conversation: l.Conversation, Account: l.Account, Name: l.Name, RawName: l.RawName, Target: l.Target,
		Text: l.Text, Raw: l.Raw, Verdict: "deliver"}
	en := &entry{line: l}
	if l.Ref != "" {
		en.refs = []lineRef{{l.Ref, e.arrived}}
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
	s := e.sessionFor(l)
	if s == nil {
		// the player's session ended with a ban
		if given == nil {
			e.scorer.See(e.scorerLine(l))
		}
		en.session = l.Session
		if !e.cfg.DriverSessions {
			en.session = ""
		}
		e.st.add(en)
		rec.Session, rec.Verdict, rec.Why = en.session, "hold", "banned"
		r.Verdict = rec.Verdict
		return r
	}
	if given == nil {
		v, err := e.scorer.Score(e.scorerLine(l))
		given = &scored{v, err}
	}
	v := given.v
	later := errors.Is(given.err, ErrLater) && l.Ref != ""
	if given.err != nil {
		v.Hold, v.Categories = true, nil
		rec.Why = "scorer: " + given.err.Error()
	}
	en.session, en.held, en.categories = s.id, v.Hold && !later, v.Categories
	e.st.add(en)
	s.mine = append(s.mine, en.pos)
	s.last = l.Time
	rec.Session, rec.Categories, rec.SH, rec.C = s.id, v.Categories, v.SH, v.C
	switch {
	case later:
		rec.Verdict = "hold"
		en.unscored = true
		s.unscored = append(s.unscored, en.pos)
		e.later[l.Ref] = en
	case v.Hold:
		rec.Verdict = "hold"
		r.add(e.hold(s, en.pos))
	}
	r.Verdict = rec.Verdict
	return r
}

// hold makes the line at pos one of the session's held lines, in stream order, and puts the checkpoint the held count
// brings; while a line of the session waits to be scored, that checkpoint waits for it.
func (e *Engine) hold(s *session, pos int) Result {
	i, _ := slices.BinarySearch(s.held, pos)
	s.held = slices.Insert(s.held, i, pos)
	if e.scheduled(len(s.held)) {
		s.due = true
	}
	if !s.due || len(s.unscored) > 0 || s.banned {
		return Result{}
	}
	s.due = false
	return e.checkpoint(s, "held "+strconv.Itoa(len(s.held)), s.held[len(s.held)-1])
}

// Unscored tells whether the line with this ref is held waiting to be scored.
func (e *Engine) Unscored(ref string) bool { return e.later[ref] != nil }

// RescoredRecord is the verdict of a line held while it waited to be scored.
type RescoredRecord struct {
	Time       string   `json:"time"`
	Stream     string   `json:"stream"`
	Session    string   `json:"session"`
	ID         string   `json:"id"`
	Ref        string   `json:"ref"`
	Verdict    string   `json:"verdict"`
	Categories []string `json:"categories,omitempty"`
	SH         *float64 `json:"sh,omitempty"`
	C          *float64 `json:"c,omitempty"`
	Why        string   `json:"why,omitempty"`
}

// Rescored takes the verdict (or the error) the driver got for a line held while it waited to be scored: a line to
// hold becomes one of its session's held lines, with its categories; a line to deliver, and its held repeats, are
// released. A line its session already settled (Wait or the session's end came first) stays held unscored.
func (e *Engine) Rescored(ref string, v Verdict, err error) Result {
	en := e.later[ref]
	if en == nil {
		return Result{}
	}
	delete(e.later, ref)
	s := e.open[en.session]
	if s == nil {
		return Result{}
	}
	en.unscored = false
	s.unscored = slices.DeleteFunc(s.unscored, func(p int) bool { return p == en.pos })
	rec := RescoredRecord{Time: stamp(e.now), Stream: s.st.name, Session: s.id, ID: en.line.ID, Ref: ref,
		Verdict: "hold", SH: v.SH, C: v.C}
	if err != nil {
		v.Hold, v.Categories = true, nil
		rec.Why = "scorer: " + err.Error()
	}
	var r Result
	if v.Hold || s.banned {
		en.held, en.categories, rec.Categories = true, v.Categories, v.Categories
		e.rec.Record("rescored", rec)
		r = e.hold(s, en.pos)
	} else {
		rec.Verdict = "deliver"
		e.rec.Record("rescored", rec)
		en.verdict, en.released = "deliver", true
		refs := slices.Clone(en.refs)
		slices.SortFunc(refs, func(a, b lineRef) int { return a.n - b.n })
		act := Action{Kind: ActionRelease, Account: s.account, Speaker: s.speaker, Session: s.id, Lines: []string{en.line.ID}}
		for _, x := range refs {
			act.Refs = append(act.Refs, x.ref)
		}
		r.Actions = append(r.Actions, act)
		if s.due && len(s.unscored) == 0 {
			s.due = false
			r.add(e.checkpoint(s, "held "+strconv.Itoa(len(s.held)), s.held[len(s.held)-1]))
		}
	}
	for _, act := range r.Actions {
		e.rec.Record("action", act)
	}
	return r
}

// settle makes the session's lines still waiting to be scored held lines without a category, for a checkpoint that
// cannot wait for them (the session waited Wait, or ends).
func (e *Engine) settle(s *session) {
	for _, p := range s.unscored {
		en := s.st.get(p)
		en.unscored, en.held = false, true
		delete(e.later, en.line.Ref)
		i, _ := slices.BinarySearch(s.held, p)
		s.held = slices.Insert(s.held, i, p)
		e.rec.Record("unscored", struct {
			Time    string `json:"time"`
			Session string `json:"session"`
			ID      string `json:"id"`
			Ref     string `json:"ref"`
		}{stamp(e.now), s.id, en.line.ID, en.line.Ref})
	}
	s.unscored, s.due = nil, false
}

// RepeatRecord is a line taken as a repeat of an earlier one: the earlier line's id, its count of sends with this
// one, and the verdict this copy got.
type RepeatRecord struct {
	Stream  string `json:"stream"`
	Of      string `json:"of"`
	OfRef   string `json:"of_ref,omitempty"`
	Time    string `json:"time"`
	Account string `json:"account"`
	Ref     string `json:"ref,omitempty"`
	Raw     string `json:"raw"`
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
		en.refs = append(en.refs, lineRef{l.Ref, e.arrived})
	}
	if s := e.open[en.session]; s != nil && !s.ending && l.Time.After(s.last) {
		s.last = l.Time
	}
	e.rec.Record("repeat", RepeatRecord{Stream: e.st.name, Of: en.line.ID, OfRef: en.line.Ref, Time: stamp(l.Time), Account: l.Account,
		Ref: l.Ref, Raw: l.Raw, Sent: en.line.Sent, Verdict: verdict})
	return verdict
}

// unknownLine holds a line on a channel outside Config.Channels: never scored, no line of the stream, and the
// player's session goes on the review queue when it ends.
func (e *Engine) unknownLine(l Line) string {
	e.unknown[l.Channel]++
	rec := LineRecord{Stream: e.st.name, Ref: l.Ref, Time: stamp(l.Time), Channel: l.Channel, Conversation: l.Conversation,
		Account: l.Account, Name: l.Name, RawName: l.RawName, Target: l.Target, Text: l.Text, Raw: l.Raw,
		Verdict: "hold", Why: "a channel this server does not know"}
	if l.Account != "" && (!e.cfg.DriverSessions || l.Session != "") {
		if s := e.sessionFor(l); s != nil {
			s.unknown++
			s.last = l.Time
			rec.Session = s.id
		}
	}
	e.rec.Record("line", rec)
	return "hold"
}

func (e *Engine) sessionFor(l Line) *session {
	if e.cfg.DriverSessions {
		if l.Session == "" {
			panic(fmt.Sprintf("line %s of %s has no session", l.ID, l.Account))
		}
		if e.closed[l.Session] {
			return nil
		}
		if s := e.open[l.Session]; s != nil && !s.ending {
			return s
		}
		return e.newSession(l.Session, l)
	}
	if e.banned[l.Account] {
		return nil
	}
	if s := e.live[l.Account]; s != nil && !s.ending {
		return s
	}
	return e.newSession(l.Account+"@"+stamp(l.Time), l)
}

func (e *Engine) newSession(id string, l Line) *session {
	s := &session{id: id, account: l.Account, name: l.Name, speaker: l.Speaker, st: e.st, start: l.Time, last: l.Time, first: e.st.next}
	e.open[id] = s
	if !e.cfg.DriverSessions {
		e.live[l.Account] = s
	}
	e.rec.Record("session_start", struct {
		Time    string `json:"time"`
		Stream  string `json:"stream"`
		Session string `json:"session"`
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

// checkpoint builds the judge's prompt for the session as of stream position end and returns it as a job, unless the
// spend cap stops it.
func (e *Engine) checkpoint(s *session, reason string, end int) Result {
	covers := 0
	for covers < len(s.held) && s.held[covers] <= end {
		covers++
	}
	if covers <= s.included {
		return Result{}
	}
	s.included = covers
	v := view{st: s.st, session: s.id, end: end, held: s.held[:covers]}
	for _, p := range s.mine {
		if p <= end {
			v.mine = append(v.mine, p)
		}
	}
	cp := CheckpointRecord{Time: stamp(e.now), Session: s.id, K: covers, Reason: reason, Line: s.st.get(end).line.ID,
		LineRef: s.st.get(end).line.Ref}
	c, err := e.cfg.Render.render(v)
	day := e.now.UTC().Format(time.DateOnly)
	var job *Job
	switch {
	case err != nil:
		cp.NotSent = err.Error()
	default:
		prompt, split := e.texts.prompt(c.text)
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
			job = &Job{Session: s.id, Account: s.account, K: covers, Reason: reason, Line: cp.Line, Prompt: prompt,
				Split: split, Hash: cp.Hash, Chars: cp.Chars, Mine: c.mine,
				Cases: len(e.texts.Examples), At: e.now, end: end, day: day}
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

// ErrPending is the error a driver hands to Engine.Rule when the judge could not be reached for its retry time (failed
// calls: throttling, a server error, a timeout, the network). The checkpoint stays the session's call and is put
// again Config.RetryAfter later, until the session ends.
var ErrPending = errors.New("the judge could not be reached")

// Rule takes the judge's answer on a job (or the error that stopped it) and carries out the ruling.
func (e *Engine) Rule(job *Job, a Answer, err error) Result {
	s := e.open[job.Session]
	if s == nil || s.inFlight != job || s.parked {
		panic(fmt.Sprintf("ruling on %s#%d, which is not the session's call in flight", job.Session, job.K))
	}
	var r Result
	rr := RulingRecord{Time: stamp(e.now), Session: s.id, K: job.K, Hash: job.Hash, Answer: a}
	if err != nil && errors.Is(err, ErrPending) && !s.ending {
		s.parked, s.retryAt = true, e.now.Add(e.cfg.RetryAfter)
		rr.Error, rr.Pending, rr.Retry = err.Error(), true, stamp(s.retryAt)
		e.rec.Record("ruling", rr)
		return r
	}
	ruling := ""
	switch {
	case err != nil:
		rr.Error = err.Error()
		if errors.Is(err, ErrNotSent) && a.RequestID == "" {
			e.calls[job.day]--
		}
		s.unjudged = true
	case a.Ruling != RulingNothing && a.Ruling != RulingHold && a.Ruling != RulingBan:
		rr.Error = fmt.Sprintf("the judge answered %q", a.Ruling)
		s.unjudged = true
	default:
		ruling, rr.Review = a.effect(job)
		rr.Effective = ruling
		if rr.Review != "" {
			if !slices.Contains(s.reviewReasons, rr.Review) {
				s.reviewReasons = append(s.reviewReasons, rr.Review)
			}
			e.rec.Record("review", ReviewRecord{Time: stamp(e.now), Session: s.id, Account: s.account,
				Name: s.name, Why: rr.Review})
		}
		covered := s.held[s.ruled:job.K]
		s.ruled = job.K
		rr.Covers = len(covered)
		switch ruling {
		case RulingNothing:
			var ids []string
			var arrived []lineRef
			for _, p := range covered {
				en := s.st.get(p)
				en.released = true
				ids = append(ids, en.line.ID)
				arrived = append(arrived, en.refs...)
			}
			// a repeat sent after a later held line is delivered after it
			slices.SortFunc(arrived, func(a, b lineRef) int { return a.n - b.n })
			var refs []string
			for _, x := range arrived {
				refs = append(refs, x.ref)
			}
			if len(ids) > 0 {
				s.released = append(s.released, ids...)
				r.Actions = append(r.Actions, Action{Kind: ActionRelease, Account: s.account, Speaker: s.speaker,
					Session: s.id, Lines: ids, Refs: refs})
			}
		case RulingHold:
			s.kept += len(covered)
			if rr.Review == "" && !s.warned {
				s.warned = true
				r.Actions = append(r.Actions, Action{Kind: ActionWarn, Account: s.account, Speaker: s.speaker, Session: s.id})
			}
		case RulingBan:
			s.kept += len(covered)
			s.banned, s.banLine, s.ending = true, job.Line, true
			r.Actions = append(r.Actions, Action{Kind: ActionBan, Account: s.account, Speaker: s.speaker, Session: s.id})
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
	r.add(e.nextCall(s))
	return r
}

// nextCall clears the session's call and starts the next waiting one, or closes an ending session once none is left.
// A ban drops the waiting calls.
func (e *Engine) nextCall(s *session) Result {
	s.inFlight, s.parked = nil, false
	if s.banned {
		for _, w := range s.waiting {
			e.calls[w.day]--
			e.rec.Record("checkpoint_dropped", struct {
				Session string `json:"session"`
				K       int    `json:"k"`
				Hash    string `json:"sha256"`
			}{w.Session, w.K, w.Hash})
		}
		s.waiting = nil
	}
	if len(s.waiting) > 0 {
		s.inFlight, s.waiting = s.waiting[0], s.waiting[1:]
		return Result{Jobs: []*Job{s.inFlight}}
	}
	if s.ending {
		e.closeSession(s)
	}
	return Result{}
}

// endSession puts the session's held lines not yet put to the judge to a last checkpoint, and closes it once no
// call is in flight. A checkpoint waiting to be put again is given up: the last checkpoint covers its lines.
func (e *Engine) endSession(s *session) Result {
	s.ending = true
	e.settle(s)
	var r Result
	if len(s.held) > s.included {
		end := s.st.next - 1
		if e.cfg.LastHeldView {
			end = s.held[len(s.held)-1]
		}
		r = e.checkpoint(s, "session end", end)
	}
	if s.parked {
		job := s.inFlight
		s.unjudged = true
		e.rec.Record("ruling", RulingRecord{Time: stamp(e.now), Session: s.id, K: job.K, Hash: job.Hash,
			Error: "the judge could not be reached before the session ended"})
		r.add(e.nextCall(s))
		return r
	}
	if s.inFlight == nil {
		e.closeSession(s)
	}
	return r
}

func (e *Engine) closeSession(s *session) {
	e.settle(s)
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
	case s.kept > 0 || unruled > 0 || s.unknown > 0:
		out = OutcomeHeld
	case len(s.released) > 0:
		out = OutcomeReleased
	}
	why := slices.Clone(s.reviewReasons)
	switch {
	case s.banned:
		why = append(why, "ban")
	case len(s.held) >= e.cfg.ReviewHeld && e.cfg.ReviewHeld > 0:
		why = append(why, "lines held: "+strconv.Itoa(len(s.held)))
	}
	if s.unjudged {
		why = append(why, "the judge was not called on every checkpoint")
	}
	if s.unknown > 0 {
		why = append(why, "lines on a channel this server does not know: "+strconv.Itoa(s.unknown))
	}
	review := strings.Join(why, "; ")
	held, refs := make([]string, len(s.held)), make([]string, len(s.held))
	for i, p := range s.held {
		held[i], refs[i] = s.st.get(p).line.ID, s.st.get(p).line.Ref
	}
	e.rec.Record("session_end", SessionRecord{Time: stamp(e.now), Stream: s.st.name, Session: s.id, Account: s.account,
		Start: stamp(s.start), Lines: len(s.mine), Held: held, HeldRefs: refs, Outcome: out, BanLine: s.banLine,
		Released: s.released, Unruled: unruled, Checkpoints: s.checkpoints, Review: review})
	if review != "" {
		e.rec.Record("review", ReviewRecord{Time: stamp(e.now), Session: s.id, Account: s.account, Name: s.name,
			Why: review})
	}
}

// The timers a session runs.
const (
	timerWait = iota
	timerEnd
	timerRetry
)

// due is the earliest timer of the open sessions: a held line that has waited Wait, a session's end, a checkpoint to
// put again.
func (e *Engine) due() (next *session, at time.Time, kind int) {
	for _, s := range e.sortedOpen() {
		if s.ending {
			continue
		}
		if s.parked && (next == nil || s.retryAt.Before(at)) {
			next, at, kind = s, s.retryAt, timerRetry
		}
		if e.cfg.Wait > 0 && s.inFlight == nil && (len(s.held) > s.included || len(s.unscored) > 0) {
			// the oldest held line not yet put to the judge, a line waiting to be scored included
			var oldest time.Time
			if len(s.held) > s.included {
				oldest = s.st.get(s.held[s.included]).line.Time
			}
			if len(s.unscored) > 0 {
				if t := s.st.get(s.unscored[0]).line.Time; oldest.IsZero() || t.Before(oldest) {
					oldest = t
				}
			}
			due := oldest.Add(e.cfg.Wait)
			if next == nil || due.Before(at) {
				next, at, kind = s, due, timerWait
			}
		}
		if !e.cfg.DriverSessions && !e.online[s.account] {
			from := s.last
			if t, ok := e.left[s.account]; ok && t.After(from) {
				from = t
			}
			due := from.Add(e.cfg.SessionEnd)
			if next == nil || due.Before(at) {
				next, at, kind = s, due, timerEnd
			}
		}
	}
	return next, at, kind
}

// advance runs the timers due by now, in time order. A timer that came due while the engine was not called runs at
// the engine's present time.
func (e *Engine) advance(now time.Time) Result {
	var r Result
	for {
		s, at, kind := e.due()
		if s == nil || at.After(now) {
			return r
		}
		e.at(at)
		switch kind {
		case timerWait:
			e.settle(s)
			r.add(e.checkpoint(s, "waited", s.st.next-1))
		case timerEnd:
			r.add(e.endSession(s))
		case timerRetry:
			s.parked = false
			e.rec.Record("checkpoint_again", struct {
				Time    string `json:"time"`
				Session string `json:"session"`
				K       int    `json:"k"`
				Hash    string `json:"sha256"`
			}{stamp(e.now), s.id, s.inFlight.K, s.inFlight.Hash})
			r.Jobs = append(r.Jobs, s.inFlight)
		}
	}
}

// prune drops the stream's lines that no open session can show and the conversation windows past their age.
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
		if !withinRepeat(e.now, c.at) {
			delete(e.copies, k)
		}
	}
}
