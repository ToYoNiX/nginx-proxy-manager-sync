package sync

import (
	"sort"
	"strings"
)

// DomainKey returns the identity of a host's domain set: lower-cased,
// de-duplicated and sorted, so that two hosts match when their
// domain_names are equal as sets (spec 4.3).
func DomainKey(domains []string) string {
	seen := make(map[string]bool, len(domains))
	out := make([]string, 0, len(domains))
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// Excluder decides which hosts are excluded by excludeDomains (spec 3.2).
type Excluder struct {
	patterns []string
}

// NewExcluder returns an Excluder for the given shell-style patterns.
func NewExcluder(patterns []string) Excluder {
	ps := make([]string, 0, len(patterns))
	for _, p := range patterns {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			ps = append(ps, p)
		}
	}
	return Excluder{patterns: ps}
}

// Match returns the first pattern that matches any of the domains.
func (e Excluder) Match(domains []string) (pattern string, ok bool) {
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		for _, p := range e.patterns {
			if wildcardMatch(p, d) {
				return p, true
			}
		}
	}
	return "", false
}

// wildcardMatch matches s against a pattern in which '*' matches any run of
// characters (dots included) and '?' matches exactly one character. Both
// arguments must already be lower-cased.
func wildcardMatch(pattern, s string) bool {
	p, n := []rune(pattern), []rune(s)
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(n) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == n[si]):
			pi++
			si++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, si
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
