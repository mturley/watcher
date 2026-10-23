package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/mturley/watcher"
)

func insertTestEvent(t *testing.T, c *sql.DB, id, ts, extTS string, typ watcher.EventType, r watcher.Resource) {
	t.Helper()
	var ext *string
	if extTS != "" {
		ext = &extTS
	}
	if err := InsertEvent(c, watcher.Event{ID: id, TS: ts, ExternalTS: ext, Source: "test", Type: typ, Title: "title " + id}, r); err != nil {
		t.Fatal(err)
	}
}

func eventIDs(events []watcher.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}

func TestRecentEventsForResource(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	r := watcher.Resource{Type: "pr", ID: "owner/repo#1"}
	other := watcher.Resource{Type: "pr", ID: "owner/repo#2"}
	insertTestEvent(t, c, "e1", "2026-01-01T00:00:01Z", "", watcher.EventTypePRComment, r)
	insertTestEvent(t, c, "e2", "2026-01-01T00:00:02Z", "", watcher.EventTypePRComment, r)
	insertTestEvent(t, c, "e3", "2026-01-01T00:00:03Z", "", watcher.EventTypeWatchStarted, r)
	insertTestEvent(t, c, "e4", "2026-01-01T00:00:04Z", "", watcher.EventTypePRComment, r)
	insertTestEvent(t, c, "e5", "2026-01-01T00:00:05Z", "", watcher.EventTypePRComment, other)

	got, err := RecentEventsForResource(c, "pr", r.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if ids := eventIDs(got); len(ids) != 2 || ids[0] != "e4" || ids[1] != "e2" {
		t.Errorf("got %v, want [e4 e2] (newest first, bookkeeping and other resources excluded)", ids)
	}

	got, err = RecentEventsForResource(c, "pr", "owner/repo#none", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want none", eventIDs(got))
	}
}

func TestGetEvent(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	r := watcher.Resource{Type: "pr", ID: "owner/repo#1"}
	insertTestEvent(t, c, "e1", "2026-01-01T00:00:01Z", "2026-01-01T00:00:00Z", watcher.EventTypePRComment, r)
	insertTestEvent(t, c, "ws", "2026-01-01T00:00:02Z", "", watcher.EventTypeWatchStarted, r)

	e, err := GetEvent(c, "e1")
	if err != nil {
		t.Fatal(err)
	}
	if e == nil || e.ID != "e1" || e.Title != "title e1" || e.ExternalTS == nil || *e.ExternalTS != "2026-01-01T00:00:00Z" {
		t.Errorf("got %+v, want e1 with its fields", e)
	}

	// Bookkeeping events are still returned by id.
	if e, err := GetEvent(c, "ws"); err != nil || e == nil || e.Type != watcher.EventTypeWatchStarted {
		t.Errorf("GetEvent(ws) = %+v, %v; want the watch_started event", e, err)
	}

	e, err = GetEvent(c, "missing")
	if err != nil || e != nil {
		t.Errorf("GetEvent(missing) = %+v, %v; want nil, nil", e, err)
	}
}

func TestEventByExternalTS(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	thread := watcher.Resource{Type: "slack_thread", ID: "C123:1700000000.000100"}
	otherThread := watcher.Resource{Type: "slack_thread", ID: "C123:1700000000.000200"}
	otherChannel := watcher.Resource{Type: "slack_thread", ID: "C999:1700000000.000100"}
	// "C1_3:" would match "C123:" if the prefix were a LIKE pattern.
	wildcardTrap := watcher.Resource{Type: "slack_thread", ID: "C1X3:1700000000.000100"}
	msgTS := "1700000001.000300"

	insertTestEvent(t, c, "old", "2026-01-01T00:00:01Z", msgTS, watcher.EventTypeSlackReply, thread)
	insertTestEvent(t, c, "new", "2026-01-01T00:00:02Z", msgTS, watcher.EventTypeSlackReply, otherThread)
	insertTestEvent(t, c, "book", "2026-01-01T00:00:09Z", msgTS, watcher.EventTypeWatchStarted, thread)
	insertTestEvent(t, c, "elsewhere", "2026-01-01T00:00:08Z", msgTS, watcher.EventTypeSlackReply, otherChannel)
	insertTestEvent(t, c, "trap", "2026-01-01T00:00:07Z", msgTS, watcher.EventTypeSlackReply, wildcardTrap)
	insertTestEvent(t, c, "othertype", "2026-01-01T00:00:06Z", msgTS, watcher.EventTypePRComment,
		watcher.Resource{Type: "pr", ID: "C123:x"})

	e, err := EventByExternalTS(c, "slack_thread", "C123:", msgTS)
	if err != nil {
		t.Fatal(err)
	}
	if e == nil || e.ID != "new" {
		t.Errorf("got %+v, want event new (newest non-bookkeeping match under C123:)", e)
	}

	e, err = EventByExternalTS(c, "slack_thread", "C1_3:", msgTS)
	if err != nil {
		t.Fatal(err)
	}
	if e != nil {
		t.Errorf("prefix with _ matched %q; it must be literal", e.ID)
	}

	e, err = EventByExternalTS(c, "slack_thread", "C123:", "1700000009.000000")
	if err != nil || e != nil {
		t.Errorf("unmatched ts = %+v, %v; want nil, nil", e, err)
	}
}

func TestResourceURL(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	id := "owner/repo#1"

	// Nothing records a URL yet.
	if got, err := ResourceURL(c, "pr", id); err != nil || got != "" {
		t.Fatalf("empty DB: got %q, %v; want \"\"", got, err)
	}

	// Event rows only: lexically first non-empty.
	insertTestEvent(t, c, "e1", "2026-01-01T00:00:01Z", "", watcher.EventTypePRComment, watcher.Resource{Type: "pr", ID: id, URL: "https://ev/b"})
	insertTestEvent(t, c, "e2", "2026-01-01T00:00:02Z", "", watcher.EventTypePRComment, watcher.Resource{Type: "pr", ID: id, URL: "https://ev/a"})
	insertTestEvent(t, c, "e3", "2026-01-01T00:00:03Z", "", watcher.EventTypePRComment, watcher.Resource{Type: "pr", ID: id})
	if got, err := ResourceURL(c, "pr", id); err != nil || got != "https://ev/a" {
		t.Fatalf("events only: got %q, %v; want https://ev/a", got, err)
	}

	// A dead subscription beats event rows.
	if err := Subscribe(c, "dead", watcher.Resource{Type: "pr", ID: id, URL: "https://sub/0-dead"}, SubscribeOpts{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := Revoke(c, "dead"); err != nil {
		t.Fatal(err)
	}
	if got, err := ResourceURL(c, "pr", id); err != nil || got != "https://sub/0-dead" {
		t.Fatalf("dead sub: got %q, %v; want https://sub/0-dead", got, err)
	}

	// Live subscriptions beat dead ones even when they sort later; URL-less
	// live rows are skipped.
	for _, s := range []struct{ sub, url string }{{"b", "https://sub/b"}, {"a", "https://sub/a"}, {"none", ""}} {
		if err := Subscribe(c, s.sub, watcher.Resource{Type: "pr", ID: id, URL: s.url}, SubscribeOpts{TTL: time.Hour}); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := ResourceURL(c, "pr", id); err != nil || got != "https://sub/a" {
		t.Fatalf("live subs: got %q, %v; want https://sub/a", got, err)
	}
}
