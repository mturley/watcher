// Package polllog records watcher HTTP and poll activity to the watcher log
// (watcher_poll_log). It never stores query strings, headers, or full bodies.
package polllog

import (
	"html"
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxRunes = 500

var (
	titleRe   = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	commentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	blockRe   = regexp.MustCompile(`(?is)<(style|script)[^>]*>.*?</(style|script)>`)
	tagRe     = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRe   = regexp.MustCompile(`\s+`)
)

func isHTML(s, contentType string) bool {
	if strings.Contains(strings.ToLower(contentType), "html") {
		return true
	}
	t := strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(t, "<!doctype") || strings.HasPrefix(t, "<html")
}

func collapse(s string) string { return strings.TrimSpace(spaceRe.ReplaceAllString(s, " ")) }

func capRunes(s string) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxRunes-1]) + "…"
}

// Sanitize reduces a response body to a short, single-line, human-readable
// excerpt: an HTML page's <title> (or its tag-stripped text), otherwise the
// body with whitespace collapsed; capped at 500 runes.
func Sanitize(body []byte, contentType string) string {
	s := strings.ToValidUTF8(string(body), "�")
	if isHTML(s, contentType) {
		if m := titleRe.FindStringSubmatch(s); m != nil {
			if t := collapse(html.UnescapeString(m[1])); t != "" {
				return capRunes(t)
			}
		}
		s = commentRe.ReplaceAllString(s, " ")
		s = blockRe.ReplaceAllString(s, " ")
		s = tagRe.ReplaceAllString(s, " ")
		s = html.UnescapeString(s)
	}
	return capRunes(collapse(s))
}

// SanitizeMessage sanitizes an error message that may embed a raw HTML
// response body after a prefix (e.g. "...returned status 504: <!DOCTYPE html>...").
func SanitizeMessage(msg string) string {
	lower := strings.ToLower(msg)
	idx := -1
	for _, marker := range []string{"<!doctype", "<html"} {
		if i := strings.Index(lower, marker); i >= 0 && (idx < 0 || i < idx) {
			idx = i
		}
	}
	if idx < 0 {
		return capRunes(collapse(strings.ToValidUTF8(msg, "�")))
	}
	return capRunes(collapse(msg[:idx]) + " " + Sanitize([]byte(msg[idx:]), "text/html"))
}
