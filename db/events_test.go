package db

import (
	"testing"

	"github.com/mturley/watcher"
)

func TestEventCursorAndDedup(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	ts := "2026-01-15T12:00:00Z"
	ev := watcher.Event{ID: "e1", TS: ts, ExternalTS: &ts, Source: "github", Type: watcher.EventTypePRComment, Title: "Comment by alice"}
	res := watcher.Resource{Type: "pr", ID: "owner/repo#1"}
	if err := InsertEvent(c, ev, res); err != nil {
		t.Fatal(err)
	}

	cur, err := EventCursor(c, "github", "pr", "owner/repo#1")
	if err != nil {
		t.Fatal(err)
	}
	if cur != ts {
		t.Errorf("cursor %q, want %q", cur, ts)
	}

	dup, _ := IsDuplicate(c, DedupCheck{Source: "github", ResourceType: "pr", ResourceID: "owner/repo#1", Type: watcher.EventTypePRComment, ExternalTS: &ts})
	if !dup {
		t.Error("expected ts duplicate")
	}

	title := "Comment by alice"
	byTitle, _ := IsDuplicate(c, DedupCheck{Source: "github", ResourceType: "pr", ResourceID: "owner/repo#1", Type: watcher.EventTypePRComment, Title: &title})
	if !byTitle {
		t.Error("expected title duplicate")
	}
}

func TestIsDuplicateFalseCases(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	ts := "2026-01-15T12:00:00Z"
	ev := watcher.Event{ID: "e1", TS: ts, ExternalTS: &ts, Source: "github", Type: watcher.EventTypePRComment, Title: "Comment by alice"}
	res := watcher.Resource{Type: "pr", ID: "owner/repo#1"}
	if err := InsertEvent(c, ev, res); err != nil {
		t.Fatal(err)
	}

	otherTS := "2026-01-15T13:00:00Z"
	dup, err := IsDuplicate(c, DedupCheck{Source: "github", ResourceType: "pr", ResourceID: "owner/repo#1", Type: watcher.EventTypePRComment, ExternalTS: &otherTS})
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Error("expected no duplicate for different external_ts")
	}

	dup, err = IsDuplicate(c, DedupCheck{Source: "github", ResourceType: "pr", ResourceID: "owner/repo#1", Type: watcher.EventTypePRReviewComment, ExternalTS: &ts})
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Error("expected no duplicate for different event type")
	}

	dup, err = IsDuplicate(c, DedupCheck{Source: "github", ResourceType: "pr", ResourceID: "owner/repo#2", Type: watcher.EventTypePRComment, ExternalTS: &ts})
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Error("expected no duplicate for different resource")
	}
}

func TestIsDuplicateRequiresExactlyOneMatcher(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}

	if _, err := IsDuplicate(c, DedupCheck{Source: "github", ResourceType: "pr", ResourceID: "owner/repo#1", Type: watcher.EventTypePRComment}); err == nil {
		t.Error("expected error when neither ExternalTS nor Title is set")
	}

	ts := "2026-01-15T12:00:00Z"
	title := "Comment by alice"
	if _, err := IsDuplicate(c, DedupCheck{Source: "github", ResourceType: "pr", ResourceID: "owner/repo#1", Type: watcher.EventTypePRComment, ExternalTS: &ts, Title: &title}); err == nil {
		t.Error("expected error when both ExternalTS and Title are set")
	}
}
