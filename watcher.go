// Package watcher polls external resources (GitHub PRs, Jira issues) for
// changes and records them as durable events in a consumer-owned SQLite
// database. It owns events, resource state, subscriptions, and relationships;
// it does not own read-state, a CLI, or a binary.
package watcher

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
}
