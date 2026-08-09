package watcher

import "testing"

func TestDisplayNameKnown(t *testing.T) {
	if got := EventTypePRComment.DisplayName(); got != "PR comments" {
		t.Errorf("got %q, want %q", got, "PR comments")
	}
}

func TestDisplayNameFallsBackToRaw(t *testing.T) {
	if got := EventType("nonexistent").DisplayName(); got != "nonexistent" {
		t.Errorf("got %q, want %q", got, "nonexistent")
	}
}
