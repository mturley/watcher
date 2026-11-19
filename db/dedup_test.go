package db

import (
	"testing"

	"github.com/mturley/watcher"
)

// TestIsDuplicateMatchTypeOnly reproduces and proves the fix for
// duplicate "PR merged" events. The bug: the terminal PR-state dedup
// keyed on the PR's mutable GitHub updatedAt field, which post-merge
// activity (comments, label changes, CI) continues to bump. So a
// later poll, seeing a new updatedAt, found no matching event and
// re-emitted "PR merged". MatchTypeOnly fixes this by dedup'ing purely
// on source+resource_type+resource_id+type, ignoring the timestamp.
func TestIsDuplicateMatchTypeOnly(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}

	t1 := "2026-01-15T12:00:00Z"
	ev := watcher.Event{
		ID:         "e1",
		TS:         t1,
		ExternalTS: &t1,
		Source:     "github",
		Type:       watcher.EventTypePRMerged,
		Title:      "PR MERGED",
	}
	res := watcher.Resource{Type: "pr", ID: "o/r#1"}
	if err := InsertEvent(c, ev, res); err != nil {
		t.Fatal(err)
	}

	// 1. MatchTypeOnly with no ExternalTS/Title set at all -> dup found.
	dup, err := IsDuplicate(c, DedupCheck{
		Source:        "github",
		ResourceType:  "pr",
		ResourceID:    "o/r#1",
		Type:          watcher.EventTypePRMerged,
		MatchTypeOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !dup {
		t.Error("expected MatchTypeOnly (no ExternalTS) to find the existing pr_merged event")
	}

	// 2. THE BUG, proven: without MatchTypeOnly, a different
	// external_ts (simulating the PR's updatedAt having been bumped by
	// post-merge activity) is treated as NOT a duplicate — this is
	// exactly what caused repeated "PR merged" events in production.
	t2 := "2026-01-15T12:30:00Z"
	dup, err = IsDuplicate(c, DedupCheck{
		Source:       "github",
		ResourceType: "pr",
		ResourceID:   "o/r#1",
		Type:         watcher.EventTypePRMerged,
		ExternalTS:   &t2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Error("expected non-MatchTypeOnly check with a different external_ts to NOT find a duplicate (this documents the bug's mechanism)")
	}

	// 3. THE FIX, proven: with MatchTypeOnly true, that same changed
	// external_ts (t2) is ignored entirely, and the merged event is
	// still correctly recognized as a duplicate.
	dup, err = IsDuplicate(c, DedupCheck{
		Source:        "github",
		ResourceType:  "pr",
		ResourceID:    "o/r#1",
		Type:          watcher.EventTypePRMerged,
		ExternalTS:    &t2,
		MatchTypeOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !dup {
		t.Error("expected MatchTypeOnly to find a duplicate even with a changed external_ts set on the check")
	}
}

// TestIsDuplicateMatchTypeOnlyDifferentResourceOrType verifies
// MatchTypeOnly still correctly scopes to source+resource+type, and
// doesn't just always return true.
func TestIsDuplicateMatchTypeOnlyDifferentResourceOrType(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}

	ts := "2026-01-15T12:00:00Z"
	ev := watcher.Event{ID: "e1", TS: ts, ExternalTS: &ts, Source: "github", Type: watcher.EventTypePRMerged, Title: "PR MERGED"}
	res := watcher.Resource{Type: "pr", ID: "o/r#1"}
	if err := InsertEvent(c, ev, res); err != nil {
		t.Fatal(err)
	}

	// Different resource ID -> no duplicate.
	dup, err := IsDuplicate(c, DedupCheck{Source: "github", ResourceType: "pr", ResourceID: "o/r#2", Type: watcher.EventTypePRMerged, MatchTypeOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Error("expected no duplicate for a different resource ID")
	}

	// Different event type (pr_closed vs pr_merged) -> no duplicate.
	dup, err = IsDuplicate(c, DedupCheck{Source: "github", ResourceType: "pr", ResourceID: "o/r#1", Type: watcher.EventTypePRClosed, MatchTypeOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Error("expected no duplicate for a different event type")
	}
}

func TestUpdateEventBody(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	ts := "1.5"
	old := "Bob: short…"
	res := watcher.Resource{Type: "slack", ID: "C1:1.0"}
	if err := InsertEvent(c, watcher.Event{
		ID: "e1", TS: "2026-01-15T12:00:00Z", ExternalTS: &ts, Source: "slack",
		Type: watcher.EventTypeSlackReply, Title: "New reply", Body: &old,
	}, res); err != nil {
		t.Fatal(err)
	}
	check := DedupCheck{Source: "slack", ResourceType: "slack", ResourceID: "C1:1.0",
		Type: watcher.EventTypeSlackReply, ExternalTS: &ts}

	changed, err := UpdateEventBody(c, check, "Bob: the whole message")
	if err != nil || !changed {
		t.Fatalf("UpdateEventBody = %v, %v; want true, nil", changed, err)
	}
	var body string
	if err := c.QueryRow(`SELECT body FROM watcher_events WHERE id = 'e1'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if body != "Bob: the whole message" {
		t.Errorf("body = %q", body)
	}
	if changed, err := UpdateEventBody(c, check, "Bob: the whole message"); err != nil || changed {
		t.Errorf("unchanged body: UpdateEventBody = %v, %v; want false, nil", changed, err)
	}
	other := "9.9"
	check.ExternalTS = &other
	if changed, err := UpdateEventBody(c, check, "x"); err != nil || changed {
		t.Errorf("no match: UpdateEventBody = %v, %v; want false, nil", changed, err)
	}
}
