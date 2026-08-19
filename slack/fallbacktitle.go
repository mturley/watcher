package slack

import "strings"

const maxTitleLen = 60

// fallbackTitle collapses whitespace, trims, and truncates to maxTitleLen
// runes with a trailing ellipsis — the Go port of ui/src/lib/fallbackTitle.ts
// so the cached thread title matches the live-view title.
func fallbackTitle(text string) string {
	collapsed := strings.Join(strings.Fields(text), " ")
	if collapsed == "" {
		return ""
	}
	r := []rune(collapsed)
	if len(r) <= maxTitleLen {
		return collapsed
	}
	return strings.TrimRight(string(r[:maxTitleLen]), " ") + "…"
}
