package core

import (
	"regexp"
	"strings"
)

// link matches a web address with or without its scheme: a host of two or more labels, then an optional path.
var link = regexp.MustCompile(`(?i)\b(?:https?://)?((?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,})(/[^\s]*)?`)

// Links replaces each link on the allowlist with the placeholder before a line is scored. An entry is a host, which
// covers its subdomains, or a host and path, which covers the paths under it.
type Links struct {
	Allowed     []string `toml:"allowed"`
	Placeholder string   `toml:"placeholder"`
}

func (l Links) allowed(host, path string) bool {
	host = strings.ToLower(host)
	for _, a := range l.Allowed {
		a = strings.ToLower(a)
		h, p, _ := strings.Cut(a, "/")
		if host != h && !strings.HasSuffix(host, "."+h) {
			continue
		}
		if p == "" || strings.HasPrefix(strings.ToLower(strings.TrimPrefix(path, "/")), p) {
			return true
		}
	}
	return false
}

// Replace is the text with every allowlisted link replaced by the placeholder.
func (l Links) Replace(text string) string {
	if len(l.Allowed) == 0 {
		return text
	}
	return link.ReplaceAllStringFunc(text, func(m string) string {
		sub := link.FindStringSubmatch(m)
		if l.allowed(sub[1], sub[2]) {
			return l.Placeholder
		}
		return m
	})
}
