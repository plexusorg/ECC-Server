package main

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf16"

	"golang.org/x/text/unicode/norm"
)

// duplicateKey is the duplicate rule's key for a message: copies that differ only in case, colour codes, lookalike
// letters, repeated letters or random-looking tokens get the same key. The classifier never sees this form.
func duplicateKey(text string) string {
	folded := colourCodes.ReplaceAllString(strings.ToLower(norm.NFKC.String(text)), "")
	var tokens []string
	for _, token := range strings.FieldsFunc(folded, func(r rune) bool { return strings.ContainsRune(" \t\n\v\f\r", r) }) {
		if randomToken(token) {
			continue
		}
		if mapped := collapse(mapLookalikes(token)); mapped != "" {
			tokens = append(tokens, mapped)
		}
	}
	return strings.Join(tokens, " ")
}

var colourCodes = regexp.MustCompile(`[§&](?:#[0-9a-f]{6}|[0-9a-fk-orx])`)

// randomToken matches hex runs, digit runs, and tokens mixing letters and digits, as raid tools append to dodge
// duplicate filters: [0-9a-f]{8,}|\d{3,}|(?=\S*\d)(?=\S*[a-z])\S{8,} over the whole token, its length counted in
// UTF-16 units.
func randomToken(token string) bool {
	n, hex, digit, hasDigit, hasLetter := 0, true, true, false, false
	for _, r := range token {
		n += utf16.RuneLen(r)
		isDigit := r >= '0' && r <= '9'
		hex = hex && (isDigit || r >= 'a' && r <= 'f')
		digit = digit && isDigit
		hasDigit = hasDigit || isDigit
		hasLetter = hasLetter || r >= 'a' && r <= 'z'
	}
	return hex && n >= 8 || digit && n >= 3 || hasDigit && hasLetter && n >= 8
}

var lookalikes = func() map[rune]rune {
	from := []rune("ᴀʙᴄᴅᴇꜰɢʜɪᴊᴋʟᴍɴᴏᴘǫʀꜱᴛᴜᴠᴡʏᴢаеорсухкмтвніјѕԁɡαορινтκε0134578@$")
	to := []rune("abcdefghijklmnopqrstuvwyzaeopcyxkmtbhijsdgaopivtkeoieastbas")
	m := map[rune]rune{}
	for i, r := range from {
		if _, ok := m[r]; !ok {
			m[r] = to[i]
		}
	}
	return m
}()

// mapLookalikes maps lookalike and leetspeak characters to Latin letters and keeps the other letters and digits of
// the Basic Multilingual Plane.
func mapLookalikes(token string) string {
	var b strings.Builder
	for _, r := range token {
		if l, ok := lookalikes[r]; ok {
			b.WriteRune(l)
		} else if r <= 0xFFFF && (unicode.IsLetter(r) || unicode.Is(unicode.Nd, r)) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func collapse(token string) string {
	var b strings.Builder
	prev := rune(-1)
	for _, r := range token {
		if r != prev {
			b.WriteRune(r)
		}
		prev = r
	}
	return b.String()
}
