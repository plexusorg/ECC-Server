package core

import (
	"fmt"
	"regexp"
	"strings"
)

// link matches a web address as the training pipeline reads one: an optional scheme, a host of two or more labels
// (the last one 2 to 24 letters), an optional port, then an optional path up to the next space.
var link = regexp.MustCompile(`(?i)(?:https?://)?((?:[a-z0-9-]+\.)+[a-z]{2,24})(?::\d+)?(/[^\s]*)?`)

// Punctuation that ends a sentence after a link. An exact URL is compared without it.
const linkTrail = ".,!?;:)]}'\""

// Links replaces each link on the plugin's allowlist with the bundle's canonical link before a line is scored: the
// whole match, scheme, port, path and any punctuation after it included, as the models were trained. An entry is a
// domain, which covers its subdomains and every path on them, or an exact URL (a host and a path), which covers that
// address only. Hosts and paths compare without regard to case, and a scheme is never needed.
type Links struct {
	entries   []linkEntry
	canonical string
}

type linkEntry struct {
	host, path string // lower case; path "" for a domain
}

// NewLinks reads the allowlist's entries; an entry that is not a domain or a URL is an error.
func NewLinks(allowed []string, canonical string) (Links, error) {
	l := Links{canonical: canonical}
	for _, a := range allowed {
		a = strings.TrimSpace(a)
		m := link.FindStringSubmatch(a)
		if m == nil || m[0] != a {
			return Links{}, fmt.Errorf("allowed link %q is not a domain or a URL", a)
		}
		l.entries = append(l.entries, linkEntry{host: strings.ToLower(m[1]), path: exactPath(m[2])})
	}
	return l, nil
}

// exactPath is a path as an exact URL compares it: lower case, without the punctuation after it or a last "/".
func exactPath(path string) string {
	return strings.TrimSuffix(strings.TrimRight(strings.ToLower(path), linkTrail), "/")
}

func (l Links) allowed(host, path string) bool {
	host, path = strings.ToLower(host), exactPath(path)
	for _, e := range l.entries {
		if e.path == "" {
			if host == e.host || strings.HasSuffix(host, "."+e.host) {
				return true
			}
		} else if host == e.host && path == e.path {
			return true
		}
	}
	return false
}

// Replace is the text with every allowlisted link replaced by the canonical link.
func (l Links) Replace(text string) string {
	if len(l.entries) == 0 {
		return text
	}
	return link.ReplaceAllStringFunc(text, func(m string) string {
		sub := link.FindStringSubmatch(m)
		if l.allowed(sub[1], sub[2]) {
			return l.canonical
		}
		return m
	})
}
