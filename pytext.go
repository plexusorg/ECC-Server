package main

import (
	"strings"
	"unicode"

	"golang.org/x/text/cases"
)

// Python 3.13's text predicates as redteam.py uses them, for Unicode 15.1. Go's own classes differ: regexp's \s, \w
// and \d are ASCII only, and the unicode package's tables are Unicode 17.0. Each function here matches Python for
// every code point (checked against Python over all of them; Module-ECC data/audit/go-evidence).

// pySpace is str.isspace and re's \s: Unicode White_Space plus U+001C to U+001F.
func pySpace(r rune) bool {
	return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f
}

// pyStrip is str.strip().
func pyStrip(s string) string {
	return strings.TrimFunc(s, pySpace)
}

// pyWord is re's [^\W_]: a letter or a number (str.isalnum).
func pyWord(r rune) bool {
	return (unicode.IsLetter(r) || unicode.IsNumber(r)) && !after151(r)
}

// pyDecimal is str.isdecimal with the digit's value. Decimal digits come in runs of ten from 0 to 9, so the value is
// the distance from the start of the run, counted in tens.
func pyDecimal(r rune) (int, bool) {
	if !unicode.Is(unicode.Nd, r) || after151(r) {
		return 0, false
	}
	start := r
	for unicode.Is(unicode.Nd, start-1) {
		start--
	}
	return int(r-start) % 10, true
}

// pyDigit is str.isdigit: the decimal digits and the digits that are not decimal (superscripts, circled digits).
func pyDigit(r rune) bool {
	if _, ok := pyDecimal(r); ok {
		return true
	}
	for _, g := range nonDecimalDigits {
		if r >= g[0] && r <= g[1] {
			return true
		}
	}
	return false
}

// Code points str.isdigit accepts that are not decimal digits, which int() then rejects.
var nonDecimalDigits = [][2]rune{{0xb2, 0xb3}, {0xb9, 0xb9}, {0x1369, 0x1371}, {0x19da, 0x19da}, {0x2070, 0x2070},
	{0x2074, 0x2079}, {0x2080, 0x2089}, {0x2460, 0x2468}, {0x2474, 0x247c}, {0x2488, 0x2490}, {0x24ea, 0x24ea},
	{0x24f5, 0x24fd}, {0x24ff, 0x24ff}, {0x2776, 0x277e}, {0x2780, 0x2788}, {0x278a, 0x2792}, {0x10a40, 0x10a43},
	{0x10e60, 0x10e68}, {0x11052, 0x1105a}, {0x1f100, 0x1f10a}}

var folder = cases.Fold()

// pyCasefold is str.casefold: full Unicode case folding, one code point at a time.
func pyCasefold(s string) string {
	var b strings.Builder
	for _, r := range s {
		// x/text folds Cherokee capitals to the small letters; Unicode folds the small letters to the capitals.
		if after151(r) || r >= 0x13A0 && r <= 0x13F5 {
			b.WriteRune(r)
		} else {
			b.WriteString(folder.String(string(r)))
		}
	}
	return b.String()
}

// pyLower is str.lower without the final-sigma rule, which only changes Σ at the end of a word to ς: the text it
// lowers is a quote tag, compared with the fixed ASCII tag names.
func pyLower(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case after151(r):
			b.WriteRune(r)
		case r == 'İ':
			b.WriteString("i̇")
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// after151 reports a code point Unicode 17.0 makes a letter, number or cased character that Unicode 15.1 leaves
// unassigned, so Python 3.13 treats it as unassigned (the code points where Go's tables and Python's disagree).
func after151(r rune) bool {
	if r < after151Ranges[0][0] {
		return false
	}
	for _, g := range after151Ranges {
		if r >= g[0] && r <= g[1] {
			return true
		}
	}
	return false
}

var after151Ranges = [][2]rune{{0x88F, 0x88F}, {0xC5C, 0xC5C}, {0xCDC, 0xCDC}, {0x1C89, 0x1C8A}, {0xA7CB, 0xA7CF},
	{0xA7D2, 0xA7D2}, {0xA7D4, 0xA7D4}, {0xA7DA, 0xA7DC}, {0xA7F1, 0xA7F1}, {0x105C0, 0x105F3}, {0x10940, 0x10959},
	{0x10D40, 0x10D65}, {0x10D6F, 0x10D85}, {0x10EC2, 0x10EC7}, {0x11380, 0x11389}, {0x1138B, 0x1138B},
	{0x1138E, 0x1138E}, {0x11390, 0x113B5}, {0x113B7, 0x113B7}, {0x113D1, 0x113D1}, {0x113D3, 0x113D3},
	{0x116D0, 0x116E3}, {0x11BC0, 0x11BE0}, {0x11BF0, 0x11BF9}, {0x11DB0, 0x11DDB}, {0x11DE0, 0x11DE9},
	{0x13460, 0x143FA}, {0x16100, 0x1611D}, {0x16130, 0x16139}, {0x16D40, 0x16D6C}, {0x16D70, 0x16D79},
	{0x16EA0, 0x16EB8}, {0x16EBB, 0x16ED3}, {0x16FF2, 0x16FF6}, {0x187F8, 0x187FF}, {0x18CFF, 0x18CFF},
	{0x18D09, 0x18D1E}, {0x18D80, 0x18DF2}, {0x1CCF0, 0x1CCF9}, {0x1E5D0, 0x1E5ED}, {0x1E5F0, 0x1E5FA},
	{0x1E6C0, 0x1E6DE}, {0x1E6E0, 0x1E6E2}, {0x1E6E4, 0x1E6E5}, {0x1E6E7, 0x1E6ED}, {0x1E6F0, 0x1E6F4},
	{0x1E6FE, 0x1E6FF}, {0x2B73A, 0x2B73F}, {0x2CEA2, 0x2CEAD}, {0x323B0, 0x33479}}
