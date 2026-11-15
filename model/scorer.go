// Package model is the fast model: SH, a small model, on every line (a line encoder whose vector is kept with the
// line, then a context step over the line and its context), and C, a large model, on the lines SH passes. It scores
// as the bundle's bundle.json and README state: the context lists, the features, the input strings and token ids, the
// thresholds. OpenVINO runs the graphs on the CPU, loaded through its C API on Linux.
package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"ecc-server/core"
)

// Spec is what bundle.json states about scoring a line.
type Spec struct {
	Graphs map[string]struct {
		XML    string `json:"xml"`
		Inputs []struct {
			Name string `json:"name"`
		} `json:"inputs"`
	} `json:"graphs"`
	Tokenizers map[string]struct {
		File string `json:"file"`
	} `json:"tokenizers"`
	Special struct {
		BOS *int64 `json:"bos"`
		EOS *int64 `json:"eos"`
	} `json:"special_tokens"`
	Outputs  []string `json:"outputs"`
	Kinds    []string `json:"kinds"`
	OpenVINO struct {
		Version string         `json:"version"`
		Device  string         `json:"device"`
		Compile map[string]any `json:"compile"`
		Batch   int            `json:"batch"`
	} `json:"openvino"`
	Context struct {
		Lines   int      `json:"lines"`
		Seconds int64    `json:"seconds"`
		Public  []string `json:"public_channels"`
		Placed  []string `json:"placed_channels"`
		Private string   `json:"private_channel"`
		Rule    string   `json:"rule"`
	} `json:"context"`
	SH struct {
		LineString string   `json:"line_string"`
		LineTokens string   `json:"line_tokens"`
		Channels   []string `json:"channels"`
		Unknown    *int64   `json:"unknown_channel_id"`
		Slot       string   `json:"slot"`
		AddrNone   int64    `json:"addr_none"`
		Gap        string   `json:"gap"`
		Pos        string   `json:"pos"`
	} `json:"sh"`
	C struct {
		MaxTokens  int    `json:"max_tokens"`
		Roles      string `json:"roles"`
		LineString string `json:"line_string"`
		InputIDs   string `json:"input_ids"`
	} `json:"c"`
	Thresholds struct {
		SHPass  *float32           `json:"sh_pass"`
		Hold    map[string]float32 `json:"hold"`
		Default string             `json:"default"`
		// Kinds, when the bundle sets them: C's logit for a kind at or over its threshold marks the line with it.
		Kinds map[string]float32 `json:"kinds"`
	} `json:"thresholds"`
}

// RulesSHA256 is the sha256 of the rules bundle.json states in words and patterns, at the paths of Rules, as this
// server implements them (the models' inputs here, the channels, the text preparation and the link rule in core). A
// bundle that states other rules is refused rather than served by the wrong ones.
const RulesSHA256 = "09bed914c65a59282c887272dff4b70222f91c78d6f981ceff5b30c164b8e74f"

// Rules is the paths in bundle.json of the rules RulesSHA256 covers, in order.
var Rules = []string{"context.rule", "sh.line_string", "sh.line_tokens", "sh.slot", "sh.gap", "sh.pos", "c.roles",
	"c.line_string", "c.input_ids", "channels", "text.rule", "text.whitespace", "text.patterns.section",
	"text.patterns.codes", "text.patterns.url", "text.patterns.ipv4", "text.patterns.ipv6", "text.patterns.not_secure",
	"text.patterns.icons", "text.patterns.discord_tags", "text.shown", "text.discord_name", "text.names", "text.repeat",
	"links.pattern", "links.rule"}

// The rules that bundle.json states in words, with the numbers in them this package uses: SH keeps the first
// shLineIDs ids of a line string, and a gap is at most maxGap.
const (
	shLineIDs = 46
	maxGap    = 11
)

// RulesHash is the sha256 of the UTF-8 of the values at the paths of Rules in bundle.json, joined by U+0000; a list
// is its items joined by one space, numbers in decimal (bundle.json rules.how).
func RulesHash(bundleJSON []byte) (string, error) {
	d := json.NewDecoder(bytes.NewReader(bundleJSON))
	d.UseNumber()
	var b map[string]any
	if err := d.Decode(&b); err != nil {
		return "", fmt.Errorf("bundle.json: %w", err)
	}
	vals := make([]string, 0, len(Rules))
	for _, p := range Rules {
		var v any = b
		for _, k := range strings.Split(p, ".") {
			m, _ := v.(map[string]any)
			v = m[k]
		}
		s, err := ruleText(v)
		if err != nil {
			return "", fmt.Errorf("bundle.json %s: %w", p, err)
		}
		vals = append(vals, s)
	}
	h := sha256.Sum256([]byte(strings.Join(vals, "\x00")))
	return hex.EncodeToString(h[:]), nil
}

func ruleText(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case json.Number:
		return x.String(), nil
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			s, err := ruleText(e)
			if err != nil {
				return "", err
			}
			parts[i] = s
		}
		return strings.Join(parts, " "), nil
	}
	return "", errors.New("missing, or not text, a number or a list")
}

// CheckRules refuses a bundle.json whose rules hash differently from RulesSHA256.
func CheckRules(bundleJSON []byte) error {
	h, err := RulesHash(bundleJSON)
	if err != nil {
		return err
	}
	if h != RulesSHA256 {
		return fmt.Errorf("bundle.json states rules this server does not implement (their sha256 is %s, this server's %s)",
			h, RulesSHA256)
	}
	return nil
}

// ReadSpec reads bundle.json's scoring part and checks that this package can score by it.
func ReadSpec(dir string) (*Spec, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
	if err != nil {
		return nil, err
	}
	var s Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("bundle.json: %w", err)
	}
	if err := CheckRules(raw); err != nil {
		return nil, err
	}
	for _, name := range []string{"sh_line_encoder", "sh_context", "c"} {
		g, ok := s.Graphs[name]
		if !ok || g.XML == "" || len(g.Inputs) == 0 {
			return nil, fmt.Errorf("bundle.json: graphs.%s needs xml and inputs", name)
		}
	}
	for _, name := range []string{"sh", "c"} {
		if s.Tokenizers[name].File == "" {
			return nil, fmt.Errorf("bundle.json: tokenizers.%s.file is empty", name)
		}
	}
	t := s.Thresholds
	switch {
	case s.Special.BOS == nil || s.Special.EOS == nil:
		return nil, errors.New("bundle.json: special_tokens needs bos and eos")
	case !slices.Contains(s.Outputs, "hold"):
		return nil, errors.New("bundle.json: outputs has no hold")
	case s.OpenVINO.Device != "CPU" || s.OpenVINO.Batch != 1:
		return nil, errors.New("bundle.json: openvino must name the CPU device and batch 1")
	case s.Context.Lines < 1 || s.Context.Seconds < 1 || s.Context.Private == "":
		return nil, errors.New("bundle.json: context needs lines, seconds and private_channel")
	case len(s.SH.Channels) == 0 || s.SH.Unknown == nil || s.SH.AddrNone < 1:
		return nil, errors.New("bundle.json: sh needs channels, unknown_channel_id and addr_none")
	case s.C.MaxTokens < 2:
		return nil, errors.New("bundle.json: c.max_tokens must be at least 2")
	case t.SHPass == nil || len(t.Hold) == 0:
		return nil, errors.New("bundle.json: thresholds need sh_pass and hold")
	}
	if _, ok := t.Hold[t.Default]; !ok {
		return nil, fmt.Errorf("bundle.json: thresholds.default %q is not one of thresholds.hold", t.Default)
	}
	for _, k := range s.Kinds {
		if !slices.Contains(s.Outputs, k) {
			return nil, fmt.Errorf("bundle.json: kind %s is not one of outputs", k)
		}
	}
	for k := range t.Kinds {
		if !slices.Contains(s.Kinds, k) {
			return nil, fmt.Errorf("bundle.json: thresholds.kinds names %s, which is not one of kinds", k)
		}
	}
	return &s, nil
}

// Options are the host's settings for the models.
type Options struct {
	// Threads is each graph's INFERENCE_NUM_THREADS.
	Threads int
	// CallLimit cancels a model call that takes longer; its line is held. 0: no limit.
	CallLimit time.Duration
	// Threshold names the hold threshold of thresholds.hold; "" is the bundle's default.
	Threshold string
}

// Scorer scores lines with a bundle's graphs. Its calls are made one at a time, in the order lines arrive: a line's
// context is the lines Score and See took before it. It implements core.Scorer.
type Scorer struct {
	spec           *Spec
	opt            Options
	core           *ovCore
	enc, ctx, c    *graph
	shTok, cTok    *Tokenizer
	threshold      string
	shPass, hold   float32
	holdAt         int
	kinds          []kindMark
	chanID         map[string]int64
	public, placed map[string]bool
	bos, eos       int64

	mu sync.Mutex // one call at a time; guards st
	st *stream
}

type kindMark struct {
	name string
	at   int
	min  float32
}

// Open loads a bundle's graphs and tokenizers. The models take about 4 GB of memory, 2.7 GB of it the graphs' weights
// mapped from the bundle's files.
func Open(dir string, spec *Spec, opt Options) (*Scorer, error) {
	if opt.Threads < 1 {
		return nil, errors.New("threads must be at least 1")
	}
	s := &Scorer{spec: spec, opt: opt, threshold: opt.Threshold, shPass: *spec.Thresholds.SHPass,
		holdAt: slices.Index(spec.Outputs, "hold"), chanID: map[string]int64{}, public: map[string]bool{},
		placed: map[string]bool{}, bos: *spec.Special.BOS, eos: *spec.Special.EOS, st: newStream("")}
	if s.threshold == "" {
		s.threshold = spec.Thresholds.Default
	}
	hold, ok := spec.Thresholds.Hold[s.threshold]
	if !ok {
		return nil, fmt.Errorf("the bundle has no hold threshold %q (it has %v)", s.threshold, keys(spec.Thresholds.Hold))
	}
	s.hold = hold
	for _, k := range spec.Kinds {
		if v, ok := spec.Thresholds.Kinds[k]; ok {
			s.kinds = append(s.kinds, kindMark{k, slices.Index(spec.Outputs, k), v})
		}
	}
	for i, c := range spec.SH.Channels {
		s.chanID[c] = int64(i)
	}
	for _, c := range spec.Context.Public {
		s.public[c] = true
	}
	for _, c := range spec.Context.Placed {
		s.placed[c] = true
	}
	var err error
	if s.shTok, err = LoadTokenizer(filepath.Join(dir, spec.Tokenizers["sh"].File)); err != nil {
		return nil, err
	}
	if s.cTok, err = LoadTokenizer(filepath.Join(dir, spec.Tokenizers["c"].File)); err != nil {
		return nil, err
	}
	if s.core, err = newCore(); err != nil {
		return nil, err
	}
	have, err := runtimeVersion()
	if err == nil && release(have) != release(spec.OpenVINO.Version) {
		err = fmt.Errorf("the bundle was exported with OpenVINO %s; the runtime in %s is %s", spec.OpenVINO.Version,
			RuntimeDir, have)
	}
	if err != nil {
		s.core.close()
		return nil, err
	}
	props := map[string]string{}
	for k, v := range spec.OpenVINO.Compile {
		props[k] = fmt.Sprint(v)
	}
	props["INFERENCE_NUM_THREADS"] = fmt.Sprint(opt.Threads)
	for name, dst := range map[string]**graph{"sh_line_encoder": &s.enc, "sh_context": &s.ctx, "c": &s.c} {
		g := spec.Graphs[name]
		var inputs []string
		for _, in := range g.Inputs {
			inputs = append(inputs, in.Name)
		}
		if *dst, err = s.core.load(name, filepath.Join(dir, g.XML), inputs, props); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// release is an OpenVINO build number up to its build count: "2026.4.1-22982" of "2026.4.1-22982-e213a14...".
func release(v string) string {
	parts := strings.SplitN(v, "-", 3)
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "-" + parts[1]
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Close frees the graphs once the call in progress, if any, has finished.
func (s *Scorer) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range []*graph{s.enc, s.ctx, s.c} {
		if g != nil {
			g.close()
		}
	}
	s.enc, s.ctx, s.c = nil, nil, nil
	if s.core != nil {
		s.core.close()
		s.core = nil
	}
}

// Threshold is the name of the hold threshold in use.
func (s *Scorer) Threshold() string { return s.threshold }

// MarksKinds tells whether the bundle sets kind thresholds.
func (s *Scorer) MarksKinds() bool { return len(s.kinds) > 0 }

// ---------------------------------------------------------------- the context

// entry is a line kept as context for later lines; n is its place in the order its stream took lines.
type entry struct {
	id                       string
	n                        int
	time                     int64
	channel, author, partner string
	text                     string
	toks                     []int64   // SH's input ids
	vec                      []float32 // SH's line vector, nil until encoded
}

// stream is one stream's lists of earlier lines: the public list, each pair's private messages, each author's placed
// lines. Each list keeps its last Context.Lines entries; a list whose last line is older than Context.Seconds is
// dropped. That bounds the memory to the lists active within the context's age.
type stream struct {
	name   string
	public []*entry
	pairs  map[[2]string][]*entry
	placed map[string][]*entry
	adds   int
	newest int64
}

func newStream(name string) *stream {
	return &stream{name: name, pairs: map[[2]string][]*entry{}, placed: map[string][]*entry{}}
}

func pair(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

// list is the list a line takes its context from.
func (s *Scorer) list(st *stream, e *entry) []*entry {
	switch {
	case e.channel == s.spec.Context.Private:
		return st.pairs[pair(e.author, e.partner)]
	case s.placed[e.channel]:
		return st.placed[e.author]
	}
	return st.public
}

// context is the line's context, oldest first: the last Context.Lines of its list, then those within
// Context.Seconds of it.
func (s *Scorer) context(st *stream, e *entry) []*entry {
	l := s.list(st, e)
	l = l[max(0, len(l)-s.spec.Context.Lines):]
	var out []*entry
	for _, c := range l {
		if e.time-c.time <= s.spec.Context.Seconds {
			out = append(out, c)
		}
	}
	return out
}

func trim(l []*entry, n int) []*entry {
	if len(l) > 2*n {
		l = append([]*entry(nil), l[len(l)-n:]...)
	}
	return l
}

// remember appends the line to its list: a private message to its pair's, a placed line to its author's, a public
// channel's line to the public list; any other channel's line to none.
func (s *Scorer) remember(st *stream, e *entry) {
	e.n = st.adds
	n := s.spec.Context.Lines
	switch {
	case e.channel == s.spec.Context.Private:
		k := pair(e.author, e.partner)
		st.pairs[k] = trim(append(st.pairs[k], e), n)
	case s.placed[e.channel]:
		st.placed[e.author] = trim(append(st.placed[e.author], e), n)
	case s.public[e.channel]:
		st.public = trim(append(st.public, e), n)
	}
	st.newest = max(st.newest, e.time)
	if st.adds++; st.adds%256 == 0 {
		old := func(l []*entry) bool { return len(l) == 0 || st.newest-l[len(l)-1].time > s.spec.Context.Seconds }
		for k, l := range st.pairs {
			if old(l) {
				delete(st.pairs, k)
			}
		}
		for k, l := range st.placed {
			if old(l) {
				delete(st.placed, k)
			}
		}
	}
}

// AuthorKey is a speaker's key lowercased: the account name, or "discord:" and the lowercased name (lowered alone, as
// the training pipeline lowered it: the prefix would change a final sigma's context).
func AuthorKey(speaker string) string {
	if name, ok := strings.CutPrefix(speaker, "discord:"); ok {
		return "discord:" + core.Lower(name)
	}
	return core.Lower(speaker)
}

// entryOf is the line as the models see it: author and partner keys, whole seconds, SH's input ids. A line's author
// key is Line.Speaker lowercased as Python's str.lower() does: the speaker's account name, or for a Discord relay "discord:"
// and its prepared name.
func (s *Scorer) entryOf(l core.Line) *entry {
	e := &entry{id: l.ID, time: l.Time.Unix(), channel: l.Channel, author: AuthorKey(l.Speaker),
		text: l.Text}
	if l.Channel == s.spec.Context.Private {
		e.partner = core.Lower(l.Target)
	}
	s.encode(e)
	return e
}

// encode gives the entry SH's input ids.
func (s *Scorer) encode(e *entry) {
	str := e.text
	if e.channel != "chat" {
		str = "[" + e.channel + "] " + e.text
	}
	ids := s.shTok.Encode(str)
	if len(ids) > shLineIDs {
		ids = ids[:shLineIDs]
	}
	e.toks = make([]int64, 0, len(ids)+2)
	e.toks = append(e.toks, s.bos)
	for _, id := range ids {
		e.toks = append(e.toks, int64(id))
	}
	e.toks = append(e.toks, s.eos)
}

// TakeContext gives the scorer the context of the scorer it replaces (a bundle switch), so that the next line's
// context is the one it had with the old models: each line the old lists keep, taken again in the order it came, with
// this bundle's input ids. A line's vector is made when a later line first needs it.
func (s *Scorer) TakeContext(old *Scorer) {
	old.mu.Lock()
	src := old.st
	all := slices.Clone(src.public)
	for _, l := range src.pairs {
		all = append(all, l...)
	}
	for _, l := range src.placed {
		all = append(all, l...)
	}
	old.mu.Unlock()
	slices.SortFunc(all, func(a, b *entry) int { return a.n - b.n })
	s.mu.Lock()
	defer s.mu.Unlock()
	st := newStream(src.name)
	for _, o := range all {
		e := &entry{id: o.id, time: o.time, channel: o.channel, author: o.author, partner: o.partner, text: o.text}
		s.encode(e)
		s.remember(st, e)
	}
	s.st = st
}

// streamFor is the state of the line's stream; a new stream starts with no context.
func (s *Scorer) streamFor(name string) *stream {
	if s.st.name != name {
		s.st = newStream(name)
	}
	return s.st
}

// ---------------------------------------------------------------- scoring

// ErrBusy is the error for a line whose time to be scored ran out before its models ran (core.ErrLater): the error
// ScoreBy gives is then a *Later, which ScoreLater scores.
var ErrBusy = core.ErrLater

// Later is a line whose time to be scored ran out: the line and the context it had when it came, for ScoreLater.
type Later struct {
	s   *Scorer
	e   *entry
	ctx []*entry
}

func (p *Later) Error() string        { return ErrBusy.Error() }
func (p *Later) Is(target error) bool { return target == ErrBusy }

// Detail is everything about one line's score.
type Detail struct {
	Verdict    core.Verdict
	SH         []float32
	Passed     bool
	C          []float32 // nil when C did not run
	Held       map[string]bool
	ContextIDs []string
	SHIDs      []int64
	Slot, Addr []int64
	Chan, Gap  []int64
	CIDs       []int64
}

// See takes a line that is context only: kept for later lines, never scored. Its vector is made when a later line
// first needs it.
func (s *Scorer) See(l core.Line) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.streamFor(l.Stream)
	s.remember(st, s.entryOf(l))
}

// Score scores a line with no time limit but the per-call one.
func (s *Scorer) Score(l core.Line) (core.Verdict, error) {
	d, err := s.ScoreBy(l, time.Time{})
	return d.Verdict, err
}

// ScoreBy scores a line within the deadline (none when zero): each model call is cancelled at the deadline or after
// Options.CallLimit, whichever is sooner. The line becomes context for later lines whether or not its models ran.
// When the deadline cuts it, the error is a *Later (errors.Is ErrBusy): ScoreLater scores the line then with the
// context it has now.
func (s *Scorer) ScoreBy(l core.Line, deadline time.Time) (Detail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enc == nil {
		return Detail{}, errors.New("the models are closed")
	}
	st := s.streamFor(l.Stream)
	e := s.entryOf(l)
	ctx := s.context(st, e)
	s.remember(st, e)
	d, err := s.run(e, ctx, deadline)
	if errors.Is(err, errDeadline) {
		err = &Later{s, e, ctx}
	}
	return d, err
}

// ScoreLater scores a line whose time ran out (ScoreBy's *Later), with the context it had when it came and the
// per-call limit only. A line of models since replaced (a bundle switch) is not scored.
func (s *Scorer) ScoreLater(p *Later) (Detail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.s != s || s.enc == nil {
		return Detail{}, errors.New("the models that took the line were replaced before it was scored")
	}
	return s.run(p.e, p.ctx, time.Time{})
}

// errDeadline is run's error when the deadline, not the per-call limit, stopped the models.
var errDeadline = errors.New("the deadline passed")

// run runs the models on the line e with its context ctx. The caller holds mu.
func (s *Scorer) run(e *entry, ctx []*entry, deadline time.Time) (Detail, error) {
	var d Detail
	d.SHIDs = e.toks
	for _, c := range ctx {
		d.ContextIDs = append(d.ContextIDs, c.id)
	}
	// limit is the next call's time: the per-call limit, or less when the deadline comes first (cut reports which)
	cut := false
	limit := func() (time.Duration, error) {
		lim := s.opt.CallLimit
		cut = false
		if !deadline.IsZero() {
			left := time.Until(deadline)
			if left <= 0 {
				return 0, errDeadline
			}
			if lim <= 0 || left < lim {
				lim, cut = left, true
			}
		}
		return lim, nil
	}
	failed := func(err error) error {
		if cut && errors.Is(err, errTimeout) {
			return errDeadline
		}
		return err
	}
	seq := append(slices.Clip(ctx), e)
	n := len(seq)
	for _, c := range seq {
		if c.vec != nil {
			continue
		}
		lim, err := limit()
		if err != nil {
			return d, err
		}
		shape := []int64{1, int64(len(c.toks))}
		v, err := s.enc.run([]tensor{{i64: c.toks, shape: shape}, {i64: ones(len(c.toks)), shape: shape}}, lim)
		if err != nil {
			return d, failed(err)
		}
		c.vec = v
	}

	// SH: slot, addr, chan, gap, pos, mask
	names := map[string]int64{e.author: 0}
	slotMax := s.spec.SH.AddrNone - 1
	num := func(a string) int64 {
		v, ok := names[a]
		if !ok {
			v = int64(len(names))
			names[a] = v
		}
		return min(v, slotMax)
	}
	var f [6][]int64
	for k := range f {
		f[k] = make([]int64, n)
	}
	vec := make([]float32, 0, n*len(e.vec))
	for k, c := range seq {
		if k < n-1 {
			f[0][k] = num(c.author)
		}
		f[1][k] = s.spec.SH.AddrNone
		if c.channel == s.spec.Context.Private {
			f[1][k] = num(c.partner)
		}
		f[2][k] = *s.spec.SH.Unknown
		if id, ok := s.chanID[c.channel]; ok {
			f[2][k] = id
		}
		if g := e.time - c.time; g > 0 {
			f[3][k] = int64(min(maxGap, bits.Len64(uint64(g))))
		}
		f[4][k] = int64(n - 1 - k)
		f[5][k] = 1
		vec = append(vec, c.vec...)
	}
	d.Slot, d.Addr, d.Chan, d.Gap = f[0], f[1], f[2], f[3]
	lim, err := limit()
	if err != nil {
		return d, err
	}
	in := []tensor{{f32: vec, shape: []int64{1, int64(n), int64(len(e.vec))}}}
	for _, x := range f {
		in = append(in, tensor{i64: x, shape: []int64{1, int64(n)}})
	}
	if d.SH, err = s.ctx.run(in, lim); err != nil {
		return d, failed(err)
	}
	sh := float64(d.SH[s.holdAt])
	d.Verdict.SH = &sh
	d.Passed = d.SH[s.holdAt] >= s.shPass
	d.Held = map[string]bool{}
	for k := range s.spec.Thresholds.Hold {
		d.Held[k] = false
	}
	if !d.Passed {
		return d, nil
	}

	// C: the role names, each line's string, the input ids
	roles := map[string]string{e.author: "t"}
	role := func(a string) string {
		r, ok := roles[a]
		if !ok {
			r = fmt.Sprintf("s%d", len(roles))
			roles[a] = r
		}
		return r
	}
	var body []int64
	for k, c := range seq {
		who := role(c.author)
		tag := ""
		switch {
		case c.channel == s.spec.Context.Private:
			tag = "[msg to " + role(c.partner) + "] "
		case c.channel != "chat":
			tag = "[" + c.channel + "] "
		}
		ids := s.cTok.Encode(tag + who + ": " + c.text)
		if k == n-1 {
			body = append(body, s.eos)
		}
		for _, id := range ids {
			body = append(body, int64(id))
		}
		body = append(body, s.eos)
	}
	if max := s.spec.C.MaxTokens - 1; len(body) > max {
		body = body[len(body)-max:]
	}
	d.CIDs = append([]int64{s.bos}, body...)
	if lim, err = limit(); err != nil {
		return d, err
	}
	shape := []int64{1, int64(len(d.CIDs))}
	if d.C, err = s.c.run([]tensor{{i64: d.CIDs, shape: shape}, {i64: ones(len(d.CIDs)), shape: shape}}, lim); err != nil {
		return d, failed(err)
	}
	c := float64(d.C[s.holdAt])
	d.Verdict.C = &c
	for k, v := range s.spec.Thresholds.Hold {
		d.Held[k] = d.C[s.holdAt] >= v
	}
	d.Verdict.Hold = d.C[s.holdAt] >= s.hold
	for _, k := range s.kinds {
		if d.C[k.at] >= k.min {
			d.Verdict.Kinds = append(d.Verdict.Kinds, k.name)
		}
	}
	return d, nil
}

func ones(n int) []int64 {
	m := make([]int64, n)
	for i := range m {
		m[i] = 1
	}
	return m
}

// tensor is one input of a graph: int64 or float32 values and the shape.
type tensor struct {
	i64   []int64
	f32   []float32
	shape []int64
}

var errTimeout = errors.New("timed out")
