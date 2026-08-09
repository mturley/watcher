package db

import (
	"testing"

	"github.com/mturley/watcher"
)

func TestUpsertAndGetResourceState(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}

	if err := UpsertResourceState(c, "pr", "owner/repo#1", `{"state":"open"}`, "2026-01-15T12:00:00Z", "2026-01-15T12:01:00Z"); err != nil {
		t.Fatal(err)
	}

	rs, err := GetResourceState(c, "pr", "owner/repo#1")
	if err != nil {
		t.Fatal(err)
	}
	if rs == nil {
		t.Fatal("expected resource state, got nil")
	}
	if rs.StateJSON != `{"state":"open"}` {
		t.Errorf("StateJSON = %q, want %q", rs.StateJSON, `{"state":"open"}`)
	}

	if err := UpsertResourceState(c, "pr", "owner/repo#1", `{"state":"closed"}`, "2026-01-15T13:00:00Z", "2026-01-15T13:01:00Z"); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := c.QueryRow(`SELECT COUNT(*) FROM watcher_resource_state`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}

	rs, err = GetResourceState(c, "pr", "owner/repo#1")
	if err != nil {
		t.Fatal(err)
	}
	if rs.StateJSON != `{"state":"closed"}` {
		t.Errorf("StateJSON after overwrite = %q, want %q", rs.StateJSON, `{"state":"closed"}`)
	}

	if err := DeleteResourceState(c, "pr", "owner/repo#1"); err != nil {
		t.Fatal(err)
	}
	rs, err = GetResourceState(c, "pr", "owner/repo#1")
	if err != nil {
		t.Fatal(err)
	}
	if rs != nil {
		t.Errorf("expected nil resource state after delete, got %+v", rs)
	}
}

func TestLinkResourcesIdempotent(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}

	child := watcher.Resource{Type: "commit", ID: "owner/repo@abc"}
	parent := watcher.Resource{Type: "pr", ID: "owner/repo#1"}

	if err := LinkResources(c, child, parent, "belongs_to", "github"); err != nil {
		t.Fatal(err)
	}
	if err := LinkResources(c, child, parent, "belongs_to", "github"); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := c.QueryRow(`SELECT COUNT(*) FROM watcher_resource_relationships`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestPollerStatus(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}

	if err := RecordPollerError(c, "github-poller", "boom"); err != nil {
		t.Fatal(err)
	}
	if !HasPollerError(c, "github-poller") {
		t.Error("expected HasPollerError true after error")
	}

	if err := RecordPollerSuccess(c, "github-poller"); err != nil {
		t.Fatal(err)
	}
	if HasPollerError(c, "github-poller") {
		t.Error("expected HasPollerError false after success")
	}
}
