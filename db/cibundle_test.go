package db

import (
	"testing"

	"github.com/mturley/watcher"
)

func TestUpsertCIBundle(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}

	res := watcher.Resource{Type: "pr", ID: "owner/repo#1"}

	if err := UpsertCIBundle(c, "abc", watcher.EventTypeCIPending, "CI running", "", "2026-01-15T12:00:00Z", res); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := c.QueryRow(`SELECT COUNT(*) FROM watcher_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}

	if err := UpsertCIBundle(c, "abc", watcher.EventTypeCIPassed, "CI passed", "", "2026-01-15T12:05:00Z", res); err != nil {
		t.Fatal(err)
	}

	if err := c.QueryRow(`SELECT COUNT(*) FROM watcher_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count after in-place update = %d, want 1", count)
	}

	var eventType string
	if err := c.QueryRow(`SELECT type FROM watcher_events LIMIT 1`).Scan(&eventType); err != nil {
		t.Fatal(err)
	}
	if eventType != "ci_passed" {
		t.Errorf("type = %q, want %q", eventType, "ci_passed")
	}

	if err := UpsertCIBundle(c, "def", watcher.EventTypeCIPending, "CI running", "", "2026-01-15T12:10:00Z", res); err != nil {
		t.Fatal(err)
	}

	if err := c.QueryRow(`SELECT COUNT(*) FROM watcher_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("count after new SHA = %d, want 2", count)
	}
}
