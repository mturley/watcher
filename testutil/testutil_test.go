package testutil

import (
	"testing"

	"github.com/mturley/watcher"
	"github.com/mturley/watcher/db"
)

func TestSeedEventsAndEventsForResource(t *testing.T) {
	conn := NewTestDB(t)

	resource := watcher.Resource{Type: "pr", ID: "owner/repo#1", URL: "https://github.com/owner/repo/pull/1"}
	events := []watcher.Event{
		{ID: "evt-1", TS: "2026-08-09T00:00:00Z", Source: "github", Type: watcher.EventTypePRComment, Title: "first comment"},
		{ID: "evt-2", TS: "2026-08-09T00:01:00Z", Source: "github", Type: watcher.EventTypePRComment, Title: "second comment"},
	}

	if err := SeedEvents(conn, events, resource); err != nil {
		t.Fatalf("SeedEvents: %v", err)
	}

	got, err := db.EventsForResource(conn, resource.Type, resource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}
}

func TestSeedSubscriptionsAndActiveResources(t *testing.T) {
	conn := NewTestDB(t)

	resources := []watcher.Resource{
		{Type: "pr", ID: "owner/repo#1", URL: "https://github.com/owner/repo/pull/1"},
	}

	if err := SeedSubscriptions(conn, "test-subscriber", resources); err != nil {
		t.Fatalf("SeedSubscriptions: %v", err)
	}

	active, err := db.ActiveResources(conn, "pr")
	if err != nil {
		t.Fatalf("ActiveResources: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active resource, got %d", len(active))
	}
	if active[0].ID != "owner/repo#1" {
		t.Fatalf("unexpected active resource id: %q", active[0].ID)
	}
}
