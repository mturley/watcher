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

func TestSlackReplyDisplayName(t *testing.T) {
	if got := EventTypeSlackReply.DisplayName(); got != "Slack replies" {
		t.Fatalf("EventTypeSlackReply.DisplayName() = %q, want %q", got, "Slack replies")
	}
}
