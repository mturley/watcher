package db

import (
	"testing"

	"github.com/mturley/watcher"
)

func TestMarkBotAuthors(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	res := watcher.Resource{Type: "pr", ID: "owner/repo#1"}
	add := func(id, source, author, authorType string) {
		t.Helper()
		ev := watcher.Event{ID: id, TS: "2026-01-15T12:00:00Z", Source: source, Type: watcher.EventTypePRComment, Title: id}
		if author != "" {
			ev.Author = &author
		}
		if authorType != "" {
			ev.AuthorType = &authorType
		}
		if err := InsertEvent(c, ev, res); err != nil {
			t.Fatal(err)
		}
	}
	add("ci", "github", "rhods-ci-bot", "user")
	add("ci-upper", "github", "Other-CI-Bot", "user")
	add("human", "github", "alice", "user")
	add("under", "github", "ci_bot", "user") // `_` must not act as a wildcard
	add("jira-ci", "jira", "rhods-ci-bot", "human")
	add("authorless", "github", "", "")
	add("already", "github", "dependabot", "bot")

	n, err := MarkBotAuthors(c, "github", []string{" ", "*ci-bot*"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("changed %d rows, want 2", n)
	}
	want := map[string]string{
		"ci": "bot", "ci-upper": "bot", "human": "user", "under": "user",
		"jira-ci": "human", "authorless": "", "already": "bot",
	}
	for id, w := range want {
		var got string
		if err := c.QueryRow(`SELECT COALESCE(author_type,'') FROM watcher_events WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != w {
			t.Errorf("%s: author_type %q, want %q", id, got, w)
		}
	}
	// Idempotent, and no patterns change nothing.
	if n, _ := MarkBotAuthors(c, "github", []string{"*ci-bot*"}); n != 0 {
		t.Errorf("second run changed %d rows", n)
	}
	if n, _ := MarkBotAuthors(c, "github", nil); n != 0 {
		t.Errorf("no patterns changed %d rows", n)
	}
}
