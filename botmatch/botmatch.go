// Package botmatch decides whether an account name is one of the configured
// bot patterns: GitHub logins (config's github.bot_usernames) and Jira
// usernames (jira.bot_usernames) that the service itself does not report as
// bots, such as a CI account that is an ordinary user to GitHub.
//
// A pattern is a glob over the whole name, compared case-insensitively:
// `*` matches any run of characters (including none) and `?` exactly one.
// Every other character matches itself, so a pattern without wildcards is an
// exact (case-insensitive) match. "*ci-bot*" matches "rhods-ci-bot".
package botmatch

import "strings"

// Match reports whether name matches any of patterns. Blank patterns, and a
// blank name, never match.
func Match(patterns []string, name string) bool {
	if name == "" {
		return false
	}
	n := []rune(strings.ToLower(name))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p != "" && glob([]rune(strings.ToLower(p)), n) {
			return true
		}
	}
	return false
}

// glob matches p against all of s, backtracking only to the last `*`.
func glob(p, s []rune) bool {
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == s[si]):
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

// LikePattern converts a pattern to a SQLite LIKE pattern with `\` as the
// ESCAPE character: `*` becomes `%`, `?` becomes `_`, and a literal `%`,
// `_` or `\` is escaped. SQLite's LIKE is case-insensitive for ASCII, as
// Match is. Returns "" for a blank pattern, which callers must skip.
func LikePattern(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range p {
		switch r {
		case '*':
			b.WriteByte('%')
		case '?':
			b.WriteByte('_')
		case '%', '_', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
