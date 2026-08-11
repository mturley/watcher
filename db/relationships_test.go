package db

import (
	"testing"

	watcher "github.com/mturley/watcher"
)

func TestSiblingResources(t *testing.T) {
	conn := mem(t) // existing helper in db package tests (see migrate_test.go:10)
	if err := Migrate(conn); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	epic := watcher.Resource{Type: "jira", ID: "EPIC-1"}
	childA := watcher.Resource{Type: "jira", ID: "TASK-1"}
	childB := watcher.Resource{Type: "jira", ID: "TASK-2"}
	unrelated := watcher.Resource{Type: "jira", ID: "TASK-9"}
	otherEpic := watcher.Resource{Type: "jira", ID: "EPIC-2"}

	// TASK-1 and TASK-2 share parent EPIC-1; TASK-9 is under EPIC-2.
	if err := LinkResources(conn, childA, epic, "epic", "test"); err != nil {
		t.Fatalf("link A: %v", err)
	}
	if err := LinkResources(conn, childB, epic, "epic", "test"); err != nil {
		t.Fatalf("link B: %v", err)
	}
	if err := LinkResources(conn, unrelated, otherEpic, "epic", "test"); err != nil {
		t.Fatalf("link unrelated: %v", err)
	}

	got, err := SiblingResources(conn, childA)
	if err != nil {
		t.Fatalf("SiblingResources: %v", err)
	}
	// Expect exactly TASK-2 (shares EPIC-1), NOT TASK-1 (self), NOT TASK-9.
	if len(got) != 1 || got[0].Type != "jira" || got[0].ID != "TASK-2" {
		t.Fatalf("SiblingResources(TASK-1) = %v, want [jira/TASK-2]", got)
	}

	// A resource with no relationships has no siblings.
	none, err := SiblingResources(conn, watcher.Resource{Type: "jira", ID: "NOPE"})
	if err != nil {
		t.Fatalf("SiblingResources(NOPE): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("SiblingResources(NOPE) = %v, want empty", none)
	}
}

// TestLinkResourcesUniqueIndexPreventsDuplicate directly exercises the
// unique index added to close the TOCTOU race in GitHub issue #1: even
// without the ON CONFLICT DO NOTHING clause helping, a second identical
// insert attempt through LinkResources must not create a duplicate row.
func TestLinkResourcesUniqueIndexPreventsDuplicate(t *testing.T) {
	conn := mem(t)
	if err := Migrate(conn); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	child := watcher.Resource{Type: "jira", ID: "TASK-2"}
	parent := watcher.Resource{Type: "jira", ID: "EPIC-2"}

	for i := 0; i < 2; i++ {
		if err := LinkResources(conn, child, parent, "epic", "test"); err != nil {
			t.Fatalf("LinkResources call %d: %v", i, err)
		}
	}

	var count int
	if err := conn.QueryRow(`
		SELECT COUNT(*) FROM watcher_resource_relationships
		WHERE child_type = ? AND child_id = ? AND parent_type = ? AND parent_id = ? AND relationship = ?
	`, child.Type, child.ID, parent.Type, parent.ID, "epic").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1 (unique index should prevent duplicate)", count)
	}
}
