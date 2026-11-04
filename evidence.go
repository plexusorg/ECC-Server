package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The slow check's evidence rules, ported line for line from the pipeline's src/ecc/redteam.py (render,
// ask_evidence's user message, parse_answer, keyed, evidence_check, nrm, match_line, age_statement, validate,
// first_violator, decide), which measured no failures on the red-team set. Kimi reports evidence; this code decides
// who is banned. A change here must match a change there. Python's text predicates are in pytext.go. Python's
// json.loads also reads NaN and Infinity, which Go's JSON rejects, so an answer object holding one is skipped.

// Outcomes and causes, as redteam.py names them.
const (
	roleOffender    = "offender"
	roleAtRisk      = "at_risk"
	roleNotInvolved = "not_involved"
	roleHeld        = "held"
	adultAge        = 18
	mark            = "^"
)

var (
	offendingTags = map[string]bool{"starting": true, "directed": true, "pushing": true, "grooming": true}
	// The only tags a player at risk may have.
	responseTags = map[string]bool{"responding": true, "self": true}
	selfWords    = map[string]bool{"i": true, "im": true, "i'm": true, "me": true, "my": true}
	fence        = regexp.MustCompile("```(?:json)?")
)

// evLine is one line of a transcript: its author's account name, the text, and the fast check's P(ban), nil when
// the line has none.
type evLine struct {
	speaker, text string
	pBan          *float64
}

// violation is a violation Kimi reported; line is its 1-based number in the view, or nil.
type violation struct {
	Quote string   `json:"quote"`
	Line  *big.Int `json:"line"`
	Tag   string   `json:"tag"`
}

// reported is what Kimi reported for one player.
type reported struct {
	Player     string      `json:"-"`
	OwnAge     *big.Int    `json:"own_age"`
	AgeMessage *string     `json:"age_message"`
	Violations []violation `json:"violations"`
}

// quote is a kept quote: the 0-based index of the player's own line it copies, and its tag.
type quote struct {
	index int
	tag   string
}

// kept is one player's validated evidence in one view.
type kept struct {
	player  string
	quotes  []quote
	dropped int
	given   int
	ages    []*big.Int
}

// view is the validated evidence of one call, in the order of the judged players.
type view []kept

func (v view) quotes(player string) []quote {
	for _, k := range v {
		if k.player == player {
			return k.quotes
		}
	}
	return nil
}

func (v view) ages(player string) []*big.Int {
	for _, k := range v {
		if k.player == player {
			return k.ages
		}
	}
	return nil
}

type outcome struct {
	role, cause string
}

// userMessage is ask_evidence's user message.
func userMessage(channel string, lines []evLine) string {
	names := speakers(lines)
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = pyJSONString(n)
	}
	return "Messages in " + channel + ", in order:\n" + render(lines) + "\n\n" +
		"Players to judge (exact speaker names, one JSON string each): [" + strings.Join(quoted, ", ") + "]"
}

// speakers lists each speaker once, in order of their first line.
func speakers(lines []evLine) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range lines {
		if !seen[l.speaker] {
			seen[l.speaker] = true
			out = append(out, l.speaker)
		}
	}
	return out
}

// render is datamarking: whitespace runs in player text become ^, one JSON object per line as Python's json.dumps
// writes it.
func render(lines []evLine) string {
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(`{"line": ` + strconv.Itoa(i+1) + `, "speaker": ` + pyJSONString(l.speaker) + `, "text": ` +
			pyJSONString(markSpaces(pyStrip(l.text))) + "}")
	}
	return b.String()
}

func markSpaces(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if pySpace(r) {
			if !space {
				b.WriteString(mark)
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// pyJSONString is json.dumps of a string with ensure_ascii=False: only quotes, backslashes and control characters
// are escaped.
func pyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// jvalue is a parsed JSON value that keeps an object's keys in order, as a Python dict does: a repeated key keeps
// its first position and its last value.
type jvalue struct {
	kind   byte // 'o' object, 'a' array, 's' string, 'n' number, 'b' boolean, 'z' null
	keys   []string
	values map[string]*jvalue
	items  []*jvalue
	str    string // a string, or a number's literal
	b      bool
}

func (v *jvalue) get(key string) *jvalue {
	if v == nil || v.kind != 'o' {
		return nil
	}
	return v.values[key]
}

// parseAnswer is parse_answer: the last top-level JSON object in the text that json.loads accepts, or nil.
func parseAnswer(raw string) *jvalue {
	text := fence.ReplaceAllString(raw, "")
	var found *jvalue
	depth, start := 0, 0
	inString, escaped := false, false
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch {
		case ch == '"':
			inString = true
		case ch == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case ch == '}' && depth > 0:
			depth--
			if depth == 0 {
				if v, err := parseJSON(text[start : i+1]); err == nil {
					found = v
				}
			}
		}
	}
	return found
}

func parseJSON(text string) (*jvalue, error) {
	if !json.Valid([]byte(text)) {
		return nil, errors.New("invalid JSON")
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	return readValue(dec)
}

func readValue(dec *json.Decoder) (*jvalue, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '{' {
			v := &jvalue{kind: 'o', values: map[string]*jvalue{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key := kt.(string)
				item, err := readValue(dec)
				if err != nil {
					return nil, err
				}
				if _, seen := v.values[key]; !seen {
					v.keys = append(v.keys, key)
				}
				v.values[key] = item
			}
			_, err := dec.Token()
			return v, err
		}
		v := &jvalue{kind: 'a'}
		for dec.More() {
			item, err := readValue(dec)
			if err != nil {
				return nil, err
			}
			v.items = append(v.items, item)
		}
		_, err := dec.Token()
		return v, err
	case string:
		return &jvalue{kind: 's', str: t}, nil
	case json.Number:
		return &jvalue{kind: 'n', str: string(t)}, nil
	case bool:
		return &jvalue{kind: 'b', b: t}, nil
	case nil:
		return &jvalue{kind: 'z'}, nil
	}
	return nil, io.ErrUnexpectedEOF
}

// errUnparseable is an answer Kimi gave that cannot be used (Python's ValueError, TypeError, KeyError or
// AttributeError); the call is retried.
type errUnparseable struct{ reason string }

func (e *errUnparseable) Error() string { return e.reason }

func unparseable(format string, args ...any) error {
	return &errUnparseable{fmt.Sprintf(format, args...)}
}

// keyed is keyed: each judged player's entry, matched by casefolded name.
func keyed(answer *jvalue, players []string) ([]*jvalue, error) {
	if answer == nil || answer.kind != 'o' {
		return nil, unparseable("no JSON object")
	}
	obj := answer
	if nested := obj.get("players"); nested != nil && nested.kind == 'o' {
		named := false
		for _, p := range players {
			if obj.get(p) != nil {
				named = true
			}
		}
		if !named {
			obj = nested
		}
	}
	lower := map[string]*jvalue{}
	for _, k := range obj.keys {
		lower[pyCasefold(k)] = obj.values[k]
	}
	out := make([]*jvalue, len(players))
	for i, p := range players {
		v, ok := lower[pyCasefold(p)]
		if !ok {
			return nil, unparseable("missing player %s", pyRepr(p))
		}
		out[i] = v
	}
	return out, nil
}

// evidenceCheck is evidence_check: Kimi's answer for each judged player, in their order.
func evidenceCheck(answer *jvalue, players []string) ([]reported, error) {
	entries, err := keyed(answer, players)
	if err != nil {
		return nil, err
	}
	out := make([]reported, len(players))
	for i, v := range entries {
		list := v.get("violations")
		if v.kind != 'o' || list != nil && list.kind != 'a' {
			return nil, unparseable("bad entry for %s", pyRepr(players[i]))
		}
		r := reported{Player: players[i], Violations: []violation{}}
		if list != nil {
			for _, item := range list.items {
				q, err := readViolation(item)
				if err != nil {
					return nil, err
				}
				r.Violations = append(r.Violations, q)
			}
		}
		if r.OwnAge, err = ownAge(v.get("own_age")); err != nil {
			return nil, err
		}
		if m := v.get("age_message"); m != nil && m.kind == 's' {
			r.AgeMessage = &m.str
		}
		out[i] = r
	}
	return out, nil
}

func readViolation(item *jvalue) (violation, error) {
	if item.kind == 's' {
		return violation{Quote: item.str}, nil
	}
	q := item.get("quote")
	if item.kind != 'o' || q == nil || q.kind != 's' {
		return violation{}, unparseable("bad violation")
	}
	line, err := lineNumber(item.get("line"))
	if err != nil {
		return violation{}, err
	}
	tag := ""
	if t := item.get("tag"); t != nil {
		tag = pyLower(pyStrip(pyStr(t)))
	}
	return violation{Quote: q.str, Line: line, Tag: tag}, nil
}

// lineNumber is int(line) if str(line).isdigit() else None: a non-negative integer, or a string of digits.
func lineNumber(v *jvalue) (*big.Int, error) {
	switch {
	case v == nil:
		return nil, nil
	case v.kind == 'n':
		n, ok := new(big.Int).SetString(v.str, 10)
		if !ok || n.Sign() < 0 {
			return nil, nil
		}
		return n, nil
	case v.kind == 's':
		return digits(v.str)
	}
	return nil, nil
}

// ownAge is own_age: int() of a number (a boolean is 1 or 0) or of a string that holds only digits; else nil.
func ownAge(v *jvalue) (*big.Int, error) {
	switch {
	case v == nil:
		return nil, nil
	case v.kind == 'b':
		if v.b {
			return big.NewInt(1), nil
		}
		return big.NewInt(0), nil
	case v.kind == 'n':
		if n, ok := new(big.Int).SetString(v.str, 10); ok {
			return n, nil
		}
		// Python reads any other number as a float and truncates it.
		f, err := strconv.ParseFloat(v.str, 64)
		if err != nil || math.IsInf(f, 0) {
			return nil, unparseable("own_age %s is not a finite number", v.str)
		}
		n, _ := big.NewFloat(f).Int(nil)
		return n, nil
	case v.kind == 's':
		return digits(pyStrip(v.str))
	}
	return nil, nil
}

// digits is int(text) when text.isdigit(), else nil. Digits that are not decimal fail int(), as in Python.
func digits(text string) (*big.Int, error) {
	if text == "" {
		return nil, nil
	}
	for _, r := range text {
		if !pyDigit(r) {
			return nil, nil
		}
	}
	n := new(big.Int)
	for _, r := range text {
		d, ok := pyDecimal(r)
		if !ok {
			return nil, unparseable("invalid literal for int() with base 10: %s", pyRepr(text))
		}
		n.Mul(n, big.NewInt(10)).Add(n, big.NewInt(int64(d)))
	}
	return n, nil
}

// pyStr is str() of a parsed JSON value as Python prints it, for a tag that is not a string.
func pyStr(v *jvalue) string {
	switch v.kind {
	case 's':
		return v.str
	case 'z':
		return "None"
	case 'b':
		if v.b {
			return "True"
		}
		return "False"
	case 'n':
		if n, ok := new(big.Int).SetString(v.str, 10); ok {
			return n.String()
		}
		f, _ := strconv.ParseFloat(v.str, 64)
		return pyFloat(f)
	case 'a':
		parts := make([]string, len(v.items))
		for i, item := range v.items {
			parts[i] = pyReprValue(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	parts := make([]string, len(v.keys))
	for i, k := range v.keys {
		parts[i] = pyRepr(k) + ": " + pyReprValue(v.values[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func pyReprValue(v *jvalue) string {
	if v.kind == 's' {
		return pyRepr(v.str)
	}
	return pyStr(v)
}

// pyFloat is repr() of a float: the shortest digits that read back, positional from 1e-4 up to 1e16.
func pyFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	exp := 0
	if f != 0 {
		exp = int(math.Floor(math.Log10(math.Abs(f))))
		// Log10 can be one off at a power of ten; the shortest representation settles it.
		e := strconv.FormatFloat(f, 'e', -1, 64)
		exp, _ = strconv.Atoi(e[strings.IndexByte(e, 'e')+1:])
	}
	if exp < -4 || exp >= 16 {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		mant, ex, _ := strings.Cut(s, "e")
		n, _ := strconv.Atoi(ex)
		sign := "+"
		if n < 0 {
			sign, n = "-", -n
		}
		return fmt.Sprintf("%se%s%02d", mant, sign, n)
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".") {
		s += ".0"
	}
	return s
}

// pyRepr is repr() of a string, for error messages and for a tag that is a list or an object.
func pyRepr(s string) string {
	q := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		q = '"'
	}
	var b bytes.Buffer
	b.WriteByte(q)
	for _, r := range s {
		switch {
		case r == rune(q) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(q)
	return b.String()
}

// nrm is nrm: the casefolded words (runs of letters and numbers) of the text, joined by single spaces.
func nrm(text string) string {
	folded := pyCasefold(strings.ReplaceAll(text, mark, " "))
	var words []string
	var word strings.Builder
	for _, r := range folded {
		if pyWord(r) {
			word.WriteRune(r)
		} else if word.Len() > 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	if word.Len() > 0 {
		words = append(words, word.String())
	}
	return strings.Join(words, " ")
}

// squash is match_line's squash: nrm with each run of one repeated word counted as one, since a model cannot copy
// "lol lol lol" spam to the exact count. It equals re.sub(r"\b(\w+)(?: \1\b)+", r"\1", nrm(s)), as nrm leaves only
// whole words.
func squash(text string) string {
	var out []string
	for _, w := range strings.Fields(nrm(text)) {
		if len(out) == 0 || out[len(out)-1] != w {
			out = append(out, w)
		}
	}
	return strings.Join(out, " ")
}

type ownLine struct {
	index int
	text  string
}

// matchLine is match_line: the index of the player's own line that the quote copies, or -1.
func matchLine(q string, lineNo *big.Int, own []ownLine) int {
	wanted := squash(q)
	if wanted == "" {
		return -1
	}
	partial := utf8.RuneCountInString(wanted) >= 4
	var hits []int
	for _, l := range own {
		text := squash(l.text)
		if wanted == text || partial && strings.Contains(text, wanted) {
			hits = append(hits, l.index)
		}
	}
	if len(hits) == 0 {
		return -1
	}
	if lineNo != nil && lineNo.IsInt64() {
		for _, h := range hits {
			if int64(h) == lineNo.Int64()-1 {
				return h
			}
		}
	}
	return hits[0]
}

// ageStatement is age_statement, on an nrm'd text.
func ageStatement(normalized string) bool {
	words := strings.Fields(normalized)
	if len(words) <= 3 {
		return true
	}
	for _, w := range words {
		if selfWords[w] {
			return true
		}
	}
	return false
}

func hasWord(normalized, word string) bool {
	for _, w := range strings.Fields(normalized) {
		if w == word {
			return true
		}
	}
	return false
}

// validate is validate: the kept evidence of one call, for the view lines[0..cut]. triggerOf maps each triggered
// player to their trigger's index; an age counts only when stated before it (before the cut for others).
func validate(answer []reported, lines []evLine, cut int, triggerOf map[string]int) view {
	out := make(view, 0, len(answer))
	for _, r := range answer {
		var own []ownLine
		for i := 0; i <= cut; i++ {
			if lines[i].speaker == r.Player {
				own = append(own, ownLine{i, lines[i].text})
			}
		}
		k := kept{player: r.Player, given: len(r.Violations)}
		for _, v := range r.Violations {
			if i := matchLine(v.Quote, v.Line, own); i < 0 {
				k.dropped++
			} else {
				k.quotes = append(k.quotes, quote{i, v.Tag})
			}
		}
		trigger, ok := triggerOf[r.Player]
		if !ok {
			trigger = cut
		}
		k.ages = statedAge(r, lines, own, trigger)
		out = append(out, k)
	}
	return out
}

// statedAge is validate's age rule: the age counts once, when one of the player's own lines before their trigger
// holds the stated age message, and holds the number in a statement about themselves, or is a short confirmation
// ("yea") of the nearest earlier line by someone else that holds it ("ur 11 right").
func statedAge(r reported, lines []evLine, own []ownLine, trigger int) []*big.Int {
	if r.OwnAge == nil || r.AgeMessage == nil || *r.AgeMessage == "" {
		return nil
	}
	said := nrm(*r.AgeMessage)
	number := r.OwnAge.String()
	for _, l := range own {
		nt := nrm(l.text)
		if l.index >= trigger || said == "" || !(strings.Contains(nt, said) || strings.Contains(said, nt)) {
			continue
		}
		prev := ""
		for j := l.index - 1; j >= 0; j-- {
			if lines[j].speaker != r.Player {
				prev = nrm(lines[j].text)
				break
			}
		}
		if hasWord(nt, number) && ageStatement(nt) || len(strings.Fields(nt)) <= 3 && hasWord(prev, number) {
			return []*big.Int{r.OwnAge}
		}
	}
	return nil
}

// firstViolator is first_violator: the player whose kept quote is the earliest line in the view (the first name
// on a tie), or "".
func firstViolator(v view) string {
	first, earliest := "", -1
	for _, k := range v {
		for _, q := range k.quotes {
			if earliest < 0 || q.index < earliest || q.index == earliest && k.player < first {
				first, earliest = k.player, q.index
			}
		}
	}
	return first
}

// decideUntriggered is decide for a player who never triggered in the transcript: an offender only with
// corroboration, a kept quote tagged starting, directed, pushing or grooming in the whole view and P(ban) at or above
// the corroboration threshold on at least one of their kept quoted lines.
func decideUntriggered(player string, full view, lines []evLine, corroboration float64) outcome {
	offending, corroborated := false, false
	for _, q := range full.quotes(player) {
		offending = offending || offendingTags[q.tag]
		p := lines[q.index].pBan
		corroborated = corroborated || p != nil && *p >= corroboration
	}
	if offending && corroborated {
		return outcome{roleOffender, "corroborated"}
	}
	return outcome{roleNotInvolved, "not triggered"}
}

// decideTriggered is decide for a triggered player, from the whole view and the view cut at their trigger. Any kept
// quote makes an offender, unless the player is a minor by a validated age, every tag is responding or self, and they
// are the first violator in neither view. With no kept quote they are cleared only when every line of theirs before
// the trigger has a P(ban) below the clearing threshold; otherwise they are held.
func decideTriggered(player string, trigger int, full, cut view, lines []evLine, clearing float64) outcome {
	views := []view{full, cut}
	var quotes []quote
	for _, v := range views {
		quotes = append(quotes, v.quotes(player)...)
	}
	if len(quotes) > 0 {
		responses := true
		for _, q := range quotes {
			responses = responses && responseTags[q.tag]
		}
		first := false
		var ages []*big.Int
		for _, v := range views {
			first = first || firstViolator(v) == player
			ages = append(ages, v.ages(player)...)
		}
		minor := len(ages) > 0
		for _, a := range ages {
			minor = minor && a.Cmp(big.NewInt(adultAge)) < 0
		}
		if minor && !first && responses {
			return outcome{roleAtRisk, "kimi"}
		}
		return outcome{roleOffender, "kimi"}
	}
	for i := 0; i < trigger; i++ {
		if l := lines[i]; l.speaker == player && (l.pBan == nil || *l.pBan >= clearing) {
			return outcome{roleHeld, "fast"}
		}
	}
	return outcome{roleNotInvolved, "cleared"}
}
