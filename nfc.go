package main

import (
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// nfc is NFC exactly as the pipeline's Python 3.13 unicodedata.normalize("NFC") gives it. x/text differs from it in
// two ways, and both change prompt bytes:
//   - It follows the Stream-Safe Text Format: after 30 non-starters in a row it inserts U+034F COMBINING GRAPHEME
//     JOINER and reorders each run of 30 on its own. Python reorders the whole run and inserts nothing. Chat has
//     such runs ("zalgo" text).
//   - Its tables are Unicode 17.0; Python 3.13's are 15.1, which leaves the characters in unicode151Unassigned
//     unassigned and therefore unnormalized.
//
// Strings without either case take x/text's result; the rest go through UAX #15 directly.
func nfc(s string) string {
	out := norm.NFC.String(s)
	if strings.Count(out, cgj) == strings.Count(s, cgj) && !hasUnassigned(s) && !hasUnassigned(out) {
		return out
	}
	// canonical decomposition, one character at a time so no stream-safe limit applies
	d := make([]rune, 0, len(s))
	for _, r := range s {
		if unassigned(r) {
			d = append(d, r)
		} else {
			d = append(d, []rune(norm.NFD.String(string(r)))...)
		}
	}
	// canonical ordering: each run of non-starters sorted by combining class, keeping order within a class
	for i := 0; i < len(d); {
		j := i
		for j < len(d) && ccc(d[j]) != 0 {
			j++
		}
		slices.SortStableFunc(d[i:j], func(a, b rune) int { return int(ccc(a)) - int(ccc(b)) })
		i = max(j, i+1)
	}
	// canonical composition: a character joins the last starter unless a character between them has a combining
	// class of 0 or at least its own
	c := make([]rune, 0, len(d))
	starter := -1
	var last uint8
	for _, r := range d {
		class := ccc(r)
		if starter >= 0 && (starter == len(c)-1 || last < class) {
			if p, ok := compose(c[starter], r); ok {
				c[starter] = p
				continue
			}
		}
		if class == 0 {
			starter = len(c)
		}
		last = class
		c = append(c, r)
	}
	return string(c)
}

const cgj = "͏"

func ccc(r rune) uint8 {
	if unassigned(r) {
		return 0
	}
	return norm.NFD.PropertiesString(string(r)).CCC()
}

// compose returns the primary composite of a starter and a character, if there is one.
func compose(a, b rune) (rune, bool) {
	if unassigned(a) || unassigned(b) {
		return 0, false
	}
	s := norm.NFC.String(string(a) + string(b))
	r, n := utf8.DecodeRuneInString(s)
	return r, n == len(s) && !unassigned(r)
}

// unicode151Unassigned: the characters whose normalization differs between x/text's Unicode 17.0 tables and
// Unicode 15.1, all assigned after 15.1 (found by comparing every code point's combining class, NFD, NFC and
// backward composition in both).
var unicode151Unassigned = [][2]rune{
	{0x897, 0x897}, {0x1ACF, 0x1ADD}, {0x1AE0, 0x1AEB}, {0x105C9, 0x105C9}, {0x105E4, 0x105E4},
	{0x10D69, 0x10D6D}, {0x10EFA, 0x10EFB}, {0x11383, 0x11383}, {0x11385, 0x11385}, {0x1138E, 0x1138E},
	{0x11391, 0x11391}, {0x113B8, 0x113B8}, {0x113BB, 0x113BB}, {0x113C2, 0x113C2}, {0x113C5, 0x113C5},
	{0x113C7, 0x113C9}, {0x113CE, 0x113D0}, {0x1611E, 0x16129}, {0x1612F, 0x1612F}, {0x16D67, 0x16D6A},
	{0x1E5EE, 0x1E5EF}, {0x1E6E3, 0x1E6E3}, {0x1E6E6, 0x1E6E6}, {0x1E6EE, 0x1E6EF}, {0x1E6F5, 0x1E6F5},
}

func unassigned(r rune) bool {
	if r < 0x897 {
		return false
	}
	for _, g := range unicode151Unassigned {
		if r >= g[0] && r <= g[1] {
			return true
		}
	}
	return false
}

func hasUnassigned(s string) bool {
	for _, r := range s {
		if unassigned(r) {
			return true
		}
	}
	return false
}
