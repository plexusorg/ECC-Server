package core

import (
	"fmt"
	"regexp"
	"strings"
)

// LinkPattern is bundle.json links.pattern, the training pipeline's link (truth2.LINK), as Python reads it.
const LinkPattern = `(?i)(?:https?://)?((?:[a-z0-9-]+\.)+[a-z]{2,24})(?::\d+)?(/[^\s]*)?`

// link is LinkPattern in Go's syntax, matching what Python's matches: re.IGNORECASE lets U+0130, U+0131, U+017F and
// U+212A stand for i, i, s and k in [a-z] and s in "https", \d is any Unicode decimal digit and \s is str.isspace().
// Go's leftmost-first matching picks the match Python's backtracking does.
var link = regexp.MustCompile(`(?:[hH][tT][tT][pP][sS\x{17f}]?://)?` +
	`((?:[a-zA-Z0-9\x{130}\x{131}\x{17f}\x{212a}-]+\.)+[a-zA-Z\x{130}\x{131}\x{17f}\x{212a}]{2,24})` +
	`(?::\p{Nd}+)?(/[^\t-\r\x{1c}-\x{20}\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]*)?`)

// host is an allowlist entry's host as a link's host: two or more labels, the last 2 to 24 letters.
var host = regexp.MustCompile(`^(?:[a-z0-9-]+\.)+[a-z]{2,24}$`)

// Links replaces each link on the plugin's allowlist with the bundle's canonical link before a line is scored: the
// whole match, scheme, port, path and any punctuation after it included, as the models were trained (truth2.allowed).
// An entry is host[/prefix]: it allows a link whose lowercased host is the host or ends in "." and the host, and whose
// lowercased path, punctuation after it kept, starts with "/" and the prefix.
type Links struct {
	entries   []linkEntry
	canonical string
}

type linkEntry struct {
	host, prefix string // lower case; prefix "" or "/..."
}

// NewLinks reads the allowlist's entries; an entry that is not host[/prefix] is an error.
func NewLinks(allowed []string, canonical string) (Links, error) {
	l := Links{canonical: canonical}
	for _, a := range allowed {
		e := Lower(strings.TrimSpace(a))
		h, prefix, slash := strings.Cut(e, "/")
		if !host.MatchString(h) {
			return Links{}, fmt.Errorf("allowed link %q is not a host with an optional path prefix (host[/prefix])", a)
		}
		if slash {
			prefix = "/" + prefix
		}
		l.entries = append(l.entries, linkEntry{host: h, prefix: prefix})
	}
	return l, nil
}

func (l Links) allowed(host, path string) bool {
	host, path = Lower(host), Lower(path)
	for _, e := range l.entries {
		if (host == e.host || strings.HasSuffix(host, "."+e.host)) && strings.HasPrefix(path, e.prefix) {
			return true
		}
	}
	return false
}

// Replace is the prepared text with every allowlisted link replaced by the canonical link.
func (l Links) Replace(text string) string {
	if len(l.entries) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, m := range link.FindAllStringSubmatchIndex(text, -1) {
		path := ""
		if m[4] >= 0 {
			path = text[m[4]:m[5]]
		}
		if l.allowed(text[m[2]:m[3]], path) {
			b.WriteString(text[last:m[0]])
			b.WriteString(l.canonical)
			last = m[1]
		}
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}
