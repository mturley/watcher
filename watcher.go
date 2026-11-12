// Package watcher polls external resources (GitHub PRs, Jira issues) for
// changes and records them as durable events in a consumer-owned SQLite
// database. It owns events, resource state, subscriptions, and relationships;
// it does not own read-state, a CLI, or a binary.
package watcher

import "strings"

// Resource identifies an external resource. ID is the canonical key
// (see the spec's Resource ID formats); URL is the human-facing link.
type Resource struct {
	Type string
	ID   string
	URL  string
}

// Event is a change detected by a poller.
type Event struct {
	ID         string
	TS         string  // RFC3339 UTC, when the library recorded it
	ExternalTS *string // RFC3339 UTC from the source API; nil for some internal events
	Source     string  // "github", "jira", "slack"
	Type       EventType
	Title      string
	Body       *string
	Author     *string
	AuthorType *string
	Tags       *string // e.g. "commit:<sha>" for CI bundles
	// AuthorID is the author's stable account ID in Source's own namespace
	// (GitHub databaseId, Jira accountId, Slack user ID); nil when unknown.
	// Compare it only together with Source.
	AuthorID *string
	// OutOfDate is true once the commit the event covers is no longer the
	// PR's latest (a CI bundle or needs-rebase event; see db.MarkOutOfDate).
	// The title also carries an "[out of date] " prefix, for display.
	OutOfDate bool
	// TextChange holds the before/after text of an edit (a description
	// change); nil when the event carries no text. Stored exactly as
	// received, in watcher_event_text_change.
	TextChange *TextChange
}

// TextChange is the text before and after an edit.
type TextChange struct {
	Old, New string
}

// SameText reports whether a and b are equal once "\r\n" is normalized
// to "\n". Pollers use it to skip edits that only change line endings.
func SameText(a, b string) bool {
	return strings.ReplaceAll(a, "\r\n", "\n") == strings.ReplaceAll(b, "\r\n", "\n")
}
