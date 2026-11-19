// Package model scores every covered target with the bundle's classifier. It preserves the bundle's context, input
// strings, token ids and thresholds. OpenVINO runs the graph on the CPU through its C API on Linux.
package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"ecc-server/core"
)

// Spec is what bundle.json states about scoring a line.
type Spec struct {
	ModelText struct {
		Rule string `json:"rule"`
	} `json:"model_text"`
	Coverage struct {
		Rule string `json:"rule"`
	} `json:"coverage"`
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
	Outputs    []string `json:"outputs"`
	Categories []string `json:"categories"`
	OpenVINO   struct {
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
	Classifier struct {
		MaxTokens  int    `json:"max_tokens"`
		Roles      string `json:"roles"`
		LineString string `json:"line_string"`
		InputIDs   string `json:"input_ids"`
	} `json:"classifier"`
	Thresholds struct {
		Hold    map[string]float32 `json:"hold"`
		Default string             `json:"default"`
		// Categories, when the bundle sets them: the classifier's logit for a category at or over its threshold marks
		// the line with it.
		Categories map[string]float32 `json:"categories"`
	} `json:"thresholds"`
}

// RulesSHA256 is the sha256 of the rules bundle.json states in words and patterns, at the paths of Rules, as this
// server implements them (the models' inputs here, the channels, the text preparation and the link rule in core). A
// bundle that states other rules is refused rather than served by the wrong ones.
const RulesSHA256 = "57a28e87a6addbd62c6d73301037ee9fbb4e01233be673a407510e25e65568ad"

// Rules is the paths in bundle.json of the rules RulesSHA256 covers, in order.
var Rules = []string{"context.rule", "classifier.roles", "classifier.line_string", "classifier.input_ids", "channels",
	"text.rule", "text.whitespace", "text.patterns.section", "text.patterns.codes", "text.patterns.url",
	"text.patterns.ipv4", "text.patterns.ipv6", "text.patterns.not_secure", "text.patterns.icons",
	"text.patterns.discord_tags", "text.shown", "text.discord_name", "text.names", "text.repeat", "links.pattern",
	"links.rule", "coverage.rule", "model_text.rule"}

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
	var declaration struct {
		Rules struct {
			Of     []string `json:"of"`
			SHA256 string   `json:"sha256"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(bundleJSON, &declaration); err != nil {
		return fmt.Errorf("bundle.json: %w", err)
	}
	if !slices.Equal(declaration.Rules.Of, Rules) || declaration.Rules.SHA256 != h {
		return errors.New("bundle.json: rules.of or rules.sha256 differs from the implemented rules")
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
	// A threshold given as null would read as 0.
	var given struct {
		Thresholds struct {
			Hold       map[string]*float32 `json:"hold"`
			Categories map[string]*float32 `json:"categories"`
		} `json:"thresholds"`
	}
	if err := json.Unmarshal(raw, &given); err != nil {
		return nil, fmt.Errorf("bundle.json: %w", err)
	}
	for group, values := range map[string]map[string]*float32{"hold": given.Thresholds.Hold,
		"categories": given.Thresholds.Categories} {
		for key, value := range values {
			if value == nil {
				return nil, fmt.Errorf("bundle.json: thresholds.%s.%s must be a number", group, key)
			}
		}
	}
	g, ok := s.Graphs["classifier"]
	if len(s.Graphs) != 1 || !ok || !filepath.IsLocal(g.XML) || len(g.Inputs) != 2 ||
		g.Inputs[0].Name != "input_ids" || g.Inputs[1].Name != "attention_mask" {
		return nil, errors.New("bundle.json: graphs must hold only classifier, with a local xml and the inputs " +
			"input_ids and attention_mask")
	}
	if len(s.Tokenizers) != 1 || !filepath.IsLocal(s.Tokenizers["classifier"].File) {
		return nil, errors.New("bundle.json: tokenizers must hold only classifier, with a local file")
	}
	t := s.Thresholds
	switch {
	case s.ModelText.Rule != core.ModelTextRule:
		return nil, errors.New("bundle.json needs model_text.rule v3_meaning_v1")
	case s.Coverage.Rule != CoverageRule:
		return nil, errors.New("bundle.json needs coverage.rule complete_target_v1")
	case s.Special.BOS == nil || s.Special.EOS == nil:
		return nil, errors.New("bundle.json: special_tokens needs bos and eos")
	case !slices.Equal(s.Outputs, []string{"ask_contact", "give_contact", "ask_personal", "give_personal", "off_server", "sexual", "romantic", "hold"}):
		return nil, errors.New("bundle.json: outputs must contain the seven facts then hold in the supported order")
	case !slices.Equal(s.Categories, s.Outputs[:7]):
		return nil, errors.New("bundle.json: categories must contain the seven facts in the supported order")
	case s.OpenVINO.Device != "CPU" || s.OpenVINO.Batch != 1:
		return nil, errors.New("bundle.json: openvino must name the CPU device and batch 1")
	case s.Context.Lines < 1 || s.Context.Seconds < 1 || s.Context.Private == "":
		return nil, errors.New("bundle.json: context needs lines, seconds and private_channel")
	case s.Classifier.MaxTokens < 3:
		return nil, errors.New("bundle.json: classifier.max_tokens must be at least 3")
	case len(t.Hold) == 0:
		return nil, errors.New("bundle.json: thresholds need hold")
	}
	if _, ok := t.Hold[t.Default]; !ok {
		return nil, fmt.Errorf("bundle.json: thresholds.default %q is not one of thresholds.hold", t.Default)
	}
	for k := range t.Categories {
		if !slices.Contains(s.Categories, k) {
			return nil, fmt.Errorf("bundle.json: thresholds.categories names %s, which is not one of categories", k)
		}
	}
	return &s, nil
}

// Options are the host's settings for the models.
type Options struct {
	// Threads is each graph's INFERENCE_NUM_THREADS.
	Threads int
	// Threshold names the hold threshold of thresholds.hold; "" is the bundle's default.
	Threshold string
}

// Scorer scores lines with a bundle's graphs. Its calls are made one at a time, in the order lines arrive: a line's
// context is the lines Score and See took before it. It implements core.Scorer.
type Scorer struct {
	spec           *Spec
	opt            Options
	core           *ovCore
	classifier     *graph
	tok            *Tokenizer
	threshold      string
	hold           float32
	holdAt         int
	categories     []categoryMark
	public, placed map[string]bool
	bos, eos       int64

	mu sync.Mutex // one call at a time; guards st
	st *stream
}

type categoryMark struct {
	name string
	at   int
	min  float32
}

// Open loads the bundle's classifier graph and tokenizer.
func Open(dir string, spec *Spec, opt Options) (*Scorer, error) {
	if opt.Threads < 1 {
		return nil, errors.New("threads must be at least 1")
	}
	s := &Scorer{spec: spec, opt: opt, threshold: opt.Threshold,
		holdAt: slices.Index(spec.Outputs, "hold"), public: map[string]bool{},
		placed: map[string]bool{}, bos: *spec.Special.BOS, eos: *spec.Special.EOS, st: newStream("")}
	if s.threshold == "" {
		s.threshold = spec.Thresholds.Default
	}
	hold, ok := spec.Thresholds.Hold[s.threshold]
	if !ok {
		return nil, fmt.Errorf("the bundle has no hold threshold %q (it has %v)", s.threshold, keys(spec.Thresholds.Hold))
	}
	s.hold = hold
	for _, k := range spec.Categories {
		if v, ok := spec.Thresholds.Categories[k]; ok {
			s.categories = append(s.categories, categoryMark{k, slices.Index(spec.Outputs, k), v})
		}
	}
	for _, c := range spec.Context.Public {
		s.public[c] = true
	}
	for _, c := range spec.Context.Placed {
		s.placed[c] = true
	}
	var err error
	if s.tok, err = LoadTokenizer(filepath.Join(dir, spec.Tokenizers["classifier"].File)); err != nil {
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
	g := spec.Graphs["classifier"]
	inputs := make([]string, 0, len(g.Inputs))
	for _, in := range g.Inputs {
		inputs = append(inputs, in.Name)
	}
	if s.classifier, err = s.core.load("classifier", filepath.Join(dir, g.XML), inputs, props); err != nil {
		s.Close()
		return nil, err
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
	if s.classifier != nil {
		s.classifier.close()
	}
	s.classifier = nil
	if s.core != nil {
		s.core.close()
		s.core = nil
	}
}

// Threshold is the name of the hold threshold in use.
func (s *Scorer) Threshold() string { return s.threshold }

// MarksCategories tells whether the bundle sets category thresholds.
func (s *Scorer) MarksCategories() bool { return len(s.categories) > 0 }

// ---------------------------------------------------------------- the context

// entry is a line kept as context for later lines; n is its place in the order its stream took lines.
type entry struct {
	id                       string
	n                        int
	time                     int64
	channel, author, partner string
	text                     string
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

// entryData is the line as the classifier sees it: author and partner keys and whole seconds. A line's author key is
// Line.Speaker lowercased as Python's str.lower() does: the speaker's account name, or for a Discord relay "discord:"
// and its prepared name.
func (s *Scorer) entryData(l core.Line) *entry {
	e := &entry{id: l.ID, time: l.Time.Unix(), channel: l.Channel, author: AuthorKey(l.Speaker), text: l.Text}
	if l.Channel == s.spec.Context.Private {
		e.partner = core.Lower(l.Target)
	}
	return e
}

// TakeContext gives the scorer the context of the scorer it replaces (a bundle switch), so that the next line's
// context is the one it had with the old models: each line the old lists keep, taken again in the order it came, with
// this bundle's context settings.
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

// Detail is everything about one line's score.
type Detail struct {
	Verdict core.Verdict
	// Logits is the classifier's output, one logit per bundle.json outputs entry.
	Logits []float32
	// Held tells, by the name of each hold threshold, whether the line is held at it.
	Held       map[string]bool
	ContextIDs []string
	InputIDs   []int64
}

// See keeps a context-only line for later targets without scoring it.
func (s *Scorer) See(l core.Line) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.streamFor(l.Stream)
	s.remember(st, s.entryData(l))
}

// Score scores a line (core.Scorer).
func (s *Scorer) Score(l core.Line) (core.Verdict, error) {
	d, err := s.Detail(l)
	return d.Verdict, err
}

// Detail scores a line and gives everything about its score.
func (s *Scorer) Detail(l core.Line) (Detail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.take(l)
	if err != nil {
		return Detail{}, err
	}
	return s.run(c.e, c.ctx)
}

// Call is a line taken as context for later lines, with the context it had then, for Run to score.
type Call struct {
	s   *Scorer
	e   *entry
	ctx []*entry
}

var errClosed = errors.New("the models are closed")

// Take makes the line context for later lines at once and gives the call that scores it with the context it has now:
// a driver can answer the line before its models end, or score it after later lines, and its score is the same.
func (s *Scorer) Take(l core.Line) (*Call, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.take(l)
}

// take is Take. The caller holds mu.
func (s *Scorer) take(l core.Line) (*Call, error) {
	if s.classifier == nil {
		return nil, errClosed
	}
	st := s.streamFor(l.Stream)
	e := s.entryData(l)
	ctx := s.context(st, e)
	s.remember(st, e)
	return &Call{s, e, ctx}, nil
}

// Run runs the models on the call's line to their end, after the call in progress, if any.
func (c *Call) Run() (Detail, error) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if c.s.classifier == nil {
		return Detail{}, errClosed
	}
	return c.s.run(c.e, c.ctx)
}

// run runs the models on the line e with its context ctx. The caller holds mu.
func (s *Scorer) run(e *entry, ctx []*entry) (Detail, error) {
	var d Detail
	for _, c := range ctx {
		d.ContextIDs = append(d.ContextIDs, c.id)
	}
	d.Held = map[string]bool{}
	var err error
	d.InputIDs = s.inputIDs(e, ctx)
	shape := []int64{1, int64(len(d.InputIDs))}
	inputs := []tensor{{i64: d.InputIDs, shape: shape}, {i64: ones(len(d.InputIDs)), shape: shape}}
	if d.Logits, err = s.classifier.run(inputs); err != nil {
		return d, err
	}
	if len(d.Logits) != len(s.spec.Outputs) {
		return d, fmt.Errorf("the classifier returned %d logits, expected %d", len(d.Logits), len(s.spec.Outputs))
	}
	for _, v := range d.Logits {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return d, errors.New("the classifier returned nonfinite logits")
		}
	}
	hold := float64(d.Logits[s.holdAt])
	d.Verdict.Classifier = &hold
	for k, v := range s.spec.Thresholds.Hold {
		d.Held[k] = d.Logits[s.holdAt] >= v
	}
	d.Verdict.Hold = d.Logits[s.holdAt] >= s.hold
	for _, k := range s.categories {
		if d.Logits[k.at] >= k.min {
			d.Verdict.Categories = append(d.Verdict.Categories, k.name)
		}
	}
	return d, nil
}

// inputIDs builds the same complete context/target input used in inference.
func (s *Scorer) inputIDs(e *entry, ctx []*entry) []int64 {
	var body []int64
	texts := s.inputStrings(e, ctx)
	for k, text := range texts {
		ids := s.tok.Encode(text)
		if k == len(texts)-1 {
			body = append(body, s.eos)
		}
		for _, id := range ids {
			body = append(body, int64(id))
		}
		body = append(body, s.eos)
	}
	if max := s.spec.Classifier.MaxTokens - 1; len(body) > max {
		body = body[len(body)-max:]
	}
	return append([]int64{s.bos}, body...)
}

// inputStrings renders the exact context role walk and final target the classifier reads.
func (s *Scorer) inputStrings(e *entry, ctx []*entry) []string {
	seq := append(slices.Clip(ctx), e)
	roles := map[string]string{e.author: "t"}
	role := func(a string) string {
		r, ok := roles[a]
		if !ok {
			r = fmt.Sprintf("s%d", len(roles))
			roles[a] = r
		}
		return r
	}
	out := make([]string, 0, len(seq))
	for _, c := range seq {
		who := role(c.author)
		tag := ""
		switch {
		case c.channel == s.spec.Context.Private:
			tag = "[msg to " + role(c.partner) + "] "
		case c.channel != "chat":
			tag = "[" + c.channel + "] "
		}
		out = append(out, tag+who+": "+c.text)
	}
	return out
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
