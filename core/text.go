package core

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// The text preparation of bundle.json `text`: what the training pipeline's dossiers did to a line's text and names
// before the models and the judge read them (ecc.inputs clean, redact, shown, discord_name). It follows Python's
// regular expressions as the pipeline wrote them: lengths count code points, whitespace is str.isspace(), \d is any
// Unicode decimal digit, \w any letter, digit or number and "_", and a letter compares without case as re.IGNORECASE
// does, which also lets U+0130, U+0131, U+017F and U+212A stand for i, i, s and k.

// The limits of step 6 and of a Discord name.
const (
	LimitBook    = 1000
	LimitNick    = 80
	LimitText    = 400
	LimitDiscord = 60
	// Step 5: a link longer than URLOver code points keeps its first URLKeep and URLCut.
	URLOver = 60
	URLKeep = 50
	URLCut  = "[...]"
	// Step 6: a text longer than its limit keeps its first limit code points and TextCut.
	TextCut = " [...]"
	// Step 7: an IPv4 or IPv6 address becomes IPMark.
	IPMark = "[ip]"
)

// Limit is the step-6 limit of a channel's text.
func Limit(channel string) int {
	switch channel {
	case "book":
		return LimitBook
	case "nick":
		return LimitNick
	}
	return LimitText
}

// Whitespace is the code points Python's str.isspace() counts as whitespace, as ranges.
var Whitespace = [][2]rune{{0x09, 0x0d}, {0x1c, 0x20}, {0x85, 0x85}, {0xa0, 0xa0}, {0x1680, 0x1680},
	{0x2000, 0x200a}, {0x2028, 0x2029}, {0x202f, 0x202f}, {0x205f, 0x205f}, {0x3000, 0x3000}}

func isSpace(r rune) bool {
	for _, w := range Whitespace {
		if r >= w[0] && r <= w[1] {
			return true
		}
	}
	return false
}

// isDigit is Python's \d: a Unicode decimal digit.
func isDigit(r rune) bool { return unicode.Is(unicode.Nd, r) }

// isWord is Python's \w: str.isalnum() or "_".
func isWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

func isHex(r rune) bool { return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F' }

// foldASCII is the ASCII letter r stands for under re.IGNORECASE, lower case, or 0.
func foldASCII(r rune) rune {
	switch {
	case r >= 'a' && r <= 'z':
		return r
	case r >= 'A' && r <= 'Z':
		return r + 'a' - 'A'
	case r == 0x130 || r == 0x131:
		return 'i'
	case r == 0x17f:
		return 's'
	case r == 0x212a:
		return 'k'
	}
	return 0
}

// isLetterAZ is Python's (?i)[a-z].
func isLetterAZ(r rune) bool { return foldASCII(r) != 0 }

// PrepareText applies steps 1-7 to a line's raw text: key is the text after step 6, which the repeat key compares;
// text is the text after step 7, which the models and the judge read. An empty key is a text that is dropped.
func PrepareText(raw, channel string) (key, text string) {
	key = clean(raw, Limit(channel))
	return key, Redact(key)
}

// ShownName is a chat name as players saw it, before step 7: steps 1 and 2, a leading "[Not Secure]" mark and the
// whitespace after it removed, the icon codes removed, then step 4.
func ShownName(display string) string {
	s := []rune(codes(section(display)))
	if strings.HasPrefix(string(s), "[Not Secure]") {
		s = s[len("[Not Secure]"):]
		for len(s) > 0 && isSpace(s[0]) {
			s = s[1:]
		}
	}
	return collapse(icons(s))
}

// DiscordName is a Discord relay line's name before step 7: its <...> tags removed, then steps 1-6 with the limit
// LimitDiscord.
func DiscordName(raw string) string { return clean(tags(raw), LimitDiscord) }

// KeyName is the name a repeat is keyed by (bundle.json text.repeat), as the dossiers keyed it: on chat the shown name
// before step 7, followed by "=" and the account when it does not contain the account in any case; on discord the
// Discord name before step 7; on every other channel the account.
func KeyName(channel, shown, account string) string {
	switch channel {
	case "discord":
		return shown
	case "chat":
		if shown == "" {
			shown = account
		}
		if strings.Contains(Lower(shown), Lower(account)) {
			return shown
		}
		return shown + "=" + account
	}
	return account
}

func beforeCuts(raw string) []rune {
	s := strings.ReplaceAll(strings.ReplaceAll(codes(section(raw)), "\r", " "), "\n", " / ")
	return []rune(collapse([]rune(s)))
}

// SourceShortened reports content that preparation would discard, before any URL or character cut.
func SourceShortened(raw, channel string) bool {
	r := beforeCuts(raw)
	return len(r) > Limit(channel) || string(cutLinks(r)) != string(r)
}

// clean is steps 1-6.
func clean(raw string, limit int) string {
	r := cutLinks(beforeCuts(raw))
	if len(r) > limit {
		r = append(r[:limit:limit], []rune(TextCut)...)
	}
	return string(r)
}

// section is step 1: each "§" removed with the character after it (Python's "§.?" with DOTALL).
func section(s string) string {
	if !strings.ContainsRune(s, '§') {
		return s
	}
	var b strings.Builder
	skip := false
	for _, r := range s {
		switch {
		case skip:
			skip = false
		case r == '§':
			skip = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// codes is step 2: Python's (?i)[§&](?:#[0-9a-f]{6}|[0-9a-fk-orx]) removed.
func codes(s string) string {
	if !strings.ContainsAny(s, "&§") {
		return s
	}
	r := []rune(s)
	out := make([]rune, 0, len(r))
	for i := 0; i < len(r); i++ {
		if (r[i] == '&' || r[i] == '§') && i+1 < len(r) {
			if r[i+1] == '#' && i+7 < len(r) {
				hex := true
				for _, c := range r[i+2 : i+8] {
					hex = hex && isHex(c)
				}
				if hex {
					i += 7
					continue
				}
			}
			c := r[i+1]
			f := foldASCII(c)
			if isHex(c) || f >= 'k' && f <= 'o' && c != 0x130 && c != 0x131 && c != 0x17f || f == 'r' && c < 0x80 ||
				f == 'x' && c < 0x80 {
				i++
				continue
			}
		}
		out = append(out, r[i])
	}
	return string(out)
}

// collapse is step 4: whitespace stripped at both ends, each run of it made one space.
func collapse(r []rune) string {
	var b strings.Builder
	space := false
	for _, c := range r {
		if isSpace(c) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(c)
	}
	return b.String()
}

// cutLinks is step 5: Python's (?i)https?://\S+ over URLOver code points cut to its first URLKeep and URLCut.
func cutLinks(r []rune) []rune {
	var out []rune
	for i := 0; i < len(r); i++ {
		if n := scheme(r, i); n > 0 {
			j := i + n
			for j < len(r) && !isSpace(r[j]) {
				j++
			}
			if j > i+n {
				if out == nil {
					out = append(make([]rune, 0, len(r)), r[:i]...)
				}
				if j-i > URLOver {
					out = append(append(out, r[i:i+URLKeep]...), []rune(URLCut)...)
				} else {
					out = append(out, r[i:j]...)
				}
				i = j - 1
				continue
			}
		}
		if out != nil {
			out = append(out, r[i])
		}
	}
	if out == nil {
		return r
	}
	return out
}

// scheme is the length of Python's (?i)https?:// at r[i:], or 0.
func scheme(r []rune, i int) int {
	if i+7 > len(r) || foldASCII(r[i]) != 'h' || r[i] > 0x7f || foldASCII(r[i+1]) != 't' || r[i+1] > 0x7f ||
		foldASCII(r[i+2]) != 't' || r[i+2] > 0x7f || foldASCII(r[i+3]) != 'p' || r[i+3] > 0x7f {
		return 0
	}
	j := i + 4
	if foldASCII(r[j]) == 's' && (r[j] < 0x80 || r[j] == 0x17f) {
		if j+3 < len(r) && r[j+1] == ':' && r[j+2] == '/' && r[j+3] == '/' {
			return j + 4 - i
		}
	}
	if j+2 < len(r) && r[j] == ':' && r[j+1] == '/' && r[j+2] == '/' {
		return j + 3 - i
	}
	return 0
}

// Redact is step 7: IPv4 addresses, then IPv6 addresses, replaced by IPMark (ecc.inputs IPV4 and IPV6).
func Redact(s string) string {
	if !strings.ContainsAny(s, ".:") {
		return s
	}
	return replaceAt([]rune(replaceAt([]rune(s), ipv4)), ipv6)
}

// replaceAt replaces, scanning left to right, each match of m (m(r, i) is the length of the match at i, or 0) by
// IPMark, as Python's re.sub does.
func replaceAt(r []rune, m func([]rune, int) int) string {
	var b strings.Builder
	for i := 0; i < len(r); i++ {
		if n := m(r, i); n > 0 {
			b.WriteString(IPMark)
			i += n - 1
			continue
		}
		b.WriteRune(r[i])
	}
	return b.String()
}

// ipv4 is Python's (?<![\d.])(?:\d{1,3}\.){3}\d{1,3}(?![\d.]) at r[i:]: each group of digits is a whole run (a
// shorter one is followed by a digit), so the match is four runs of one to three digits joined by single dots.
func ipv4(r []rune, i int) int {
	if i > 0 && (isDigit(r[i-1]) || r[i-1] == '.') {
		return 0
	}
	j := i
	for g := 0; g < 4; g++ {
		k := j
		for k < len(r) && isDigit(r[k]) {
			k++
		}
		if k == j || k-j > 3 {
			return 0
		}
		j = k
		if g < 3 {
			if j >= len(r) || r[j] != '.' {
				return 0
			}
			j++
		}
	}
	if j < len(r) && (isDigit(r[j]) || r[j] == '.') {
		return 0
	}
	return j - i
}

// hexRun is the length of the run of hex digits at r[j:].
func hexRun(r []rune, j int) int {
	k := j
	for k < len(r) && isHex(r[k]) {
		k++
	}
	return k - j
}

func endOK(r []rune, j int) bool { return j >= len(r) || !isWord(r[j]) && r[j] != ':' }

// ipv6 is Python's
//
//	(?i)(?<![\w:])(?:[0-9a-f]{1,4}:){3,7}[0-9a-f]{1,4}(?![\w:])|
//	(?<![\w:])(?=(?:[0-9a-f]*:){2,})(?:[0-9a-f]{1,4}:)+:(?:[0-9a-f]{1,4}:?)+(?![\w:])
//
// at r[i:]. In the first form each group is a whole run of hex digits followed by one colon, and the last group the
// last run; in the second the groups before "::" are whole runs, and after it the longest run of hex digits and single
// colons is taken (backtracking only shortens it to a point followed by a hex digit or a colon, which the end test
// refuses). The lookahead of the second form holds whenever its groups match.
func ipv6(r []rune, i int) int {
	if i > 0 && (isWord(r[i-1]) || r[i-1] == ':') {
		return 0
	}
	// the first form
	j, groups := i, 0
	for {
		n := hexRun(r, j)
		if n < 1 || n > 4 {
			break
		}
		if j+n < len(r) && r[j+n] == ':' {
			groups++
			j += n + 1
			continue
		}
		if groups >= 3 && groups <= 7 && endOK(r, j+n) {
			return j + n - i
		}
		break
	}
	// the second form
	j, groups = i, 0
	for {
		n := hexRun(r, j)
		if n < 1 || n > 4 || j+n >= len(r) || r[j+n] != ':' {
			break
		}
		groups++
		j += n + 1
	}
	if groups == 0 || j >= len(r) || r[j] != ':' {
		return 0
	}
	j++
	if j >= len(r) || !isHex(r[j]) {
		return 0
	}
	for j < len(r) && (isHex(r[j]) || r[j] == ':' && r[j-1] != ':') {
		j++
	}
	if !endOK(r, j) {
		return 0
	}
	return j - i
}

// icons removes Python's \[(?:item|block)/[^\]]*\]|\|\|\| from a name.
func icons(r []rune) []rune {
	s := string(r)
	if !strings.Contains(s, "[item/") && !strings.Contains(s, "[block/") && !strings.Contains(s, "|||") {
		return r
	}
	out := make([]rune, 0, len(r))
	for i := 0; i < len(r); i++ {
		rest := string(r[i:])
		if strings.HasPrefix(rest, "[item/") || strings.HasPrefix(rest, "[block/") {
			if k := strings.IndexRune(rest, ']'); k >= 0 {
				i += utf8.RuneCountInString(rest[:k])
				continue
			}
		}
		if strings.HasPrefix(rest, "|||") {
			i += 2
			continue
		}
		out = append(out, r[i])
	}
	return out
}

// tags removes Python's <[^>]*> from a Discord name.
func tags(s string) string {
	if !strings.Contains(s, "<") {
		return s
	}
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			break
		}
		k := strings.IndexByte(s[i:], '>')
		if k < 0 {
			break
		}
		b.WriteString(s[:i])
		s = s[i+k+1:]
	}
	b.WriteString(s)
	return b.String()
}

// Lower is Python's str.lower(): each character's simple lower case, except U+0130, which becomes "i" and U+0307, and
// a capital sigma in the final-sigma context, which becomes ς.
func Lower(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return strings.ToLower(s)
	}
	r := []rune(s)
	var b strings.Builder
	for i, c := range r {
		switch c {
		case 0x130:
			b.WriteString("i̇")
		case 0x3a3:
			if finalSigma(r, i) {
				b.WriteRune(0x3c2)
			} else {
				b.WriteRune(0x3c3)
			}
		default:
			b.WriteRune(unicode.ToLower(c))
		}
	}
	return b.String()
}

// finalSigma is CPython's test: a cased letter before r[i] (case-ignorable ones skipped), and none after it.
func finalSigma(r []rune, i int) bool {
	j := i - 1
	for j >= 0 && caseIgnorable(r[j]) {
		j--
	}
	if j < 0 || !cased(r[j]) {
		return false
	}
	j = i + 1
	for j < len(r) && caseIgnorable(r[j]) {
		j++
	}
	return j == len(r) || !cased(r[j])
}

func cased(r rune) bool {
	return unicode.IsLower(r) || unicode.IsUpper(r) || unicode.IsTitle(r) || unicode.Is(unicode.Other_Lowercase, r) ||
		unicode.Is(unicode.Other_Uppercase, r)
}

// Word_Break MidLetter, MidNumLet and Single_Quote: with Mn, Me, Cf, Lm and Sk, the case-ignorable characters.
var midLetter = []rune{'\'', '.', ':', 0xb7, 0x387, 0x55f, 0x5f4, 0x2018, 0x2019, 0x2024, 0x2027, 0xfe13, 0xfe52,
	0xfe55, 0xff07, 0xff0e, 0xff1a}

func caseIgnorable(r rune) bool {
	if unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk) {
		return true
	}
	for _, m := range midLetter {
		if r == m {
			return true
		}
	}
	return false
}
