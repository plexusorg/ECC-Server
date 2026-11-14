package core

import (
	"fmt"
	"regexp"
	"strings"
)

// link matches a web address with or without its scheme: a host of two or more labels, then an optional path.
var link = regexp.MustCompile(`(?i)\b(?:https?://)?((?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,})(/[^\s]*)?`)

// Punctuation that ends a sentence after a link, not part of it.
const linkTrail = ".,!?;:)]}'\""

// Links replaces each link on the plugin's allowlist with the bundle's canonical link before a line is scored. An
// entry is a domain, which covers its subdomains and every path on them, or an exact URL (a host and a path), which
// covers that address only.
type Links struct {
	entries   []linkEntry
	canonical string
}

type linkEntry struct {
	host, path string // path "" for a domain
}

// NewLinks reads the allowlist's entries; an entry that is not a domain or a URL is an error.
func NewLinks(allowed []string, canonical string) (Links, error) {
	l := Links{canonical: canonical}
	for _, a := range allowed {
		m := link.FindStringSubmatch(strings.TrimSpace(a))
		if m == nil || m[0] != strings.TrimSpace(a) {
			return Links{}, fmt.Errorf("allowed link %q is not a domain or a URL", a)
		}
		l.entries = append(l.entries, linkEntry{host: strings.ToLower(m[1]), path: strings.TrimSuffix(m[2], "/")})
	}
	return l, nil
}

func (l Links) allowed(host, path string) bool {
	host = strings.ToLower(host)
	path = strings.TrimSuffix(path, "/")
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
		path := strings.TrimRight(sub[2], linkTrail)
		if !l.allowed(sub[1], path) {
			return m
		}
		return l.canonical + sub[2][len(path):]
	})
}
