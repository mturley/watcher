package db

import (
	"testing"
	"time"

	"github.com/mturley/watcher"
)

func TestSubscribeAndActiveResources(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	r := watcher.Resource{Type: "pr", ID: "owner/repo#1", URL: "https://example.com/1"}
	if err := Subscribe(c, "sub1", r, SubscribeOpts{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}

	resources, err := ActiveResources(c, "pr")
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 || resources[0].ID != r.ID {
		t.Fatalf("got %+v, want one resource %+v", resources, r)
	}
}

func TestActiveResourcesExcludesExpiredLease(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	r := watcher.Resource{Type: "pr", ID: "owner/repo#2"}
	if err := Subscribe(c, "sub1", r, SubscribeOpts{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := c.Exec(`UPDATE watcher_subscriptions SET expires_at = ? WHERE subscriber = ? AND resource_id = ?`, past, "sub1", r.ID); err != nil {
		t.Fatal(err)
	}

	resources, err := ActiveResources(c, "pr")
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 0 {
		t.Fatalf("got %+v, want no active resources (expired lease)", resources)
	}
}

func TestRevokeRemovesFromActiveResources(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	r := watcher.Resource{Type: "pr", ID: "owner/repo#3"}
	if err := Subscribe(c, "sub1", r, SubscribeOpts{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := Revoke(c, "sub1"); err != nil {
		t.Fatal(err)
	}

	resources, err := ActiveResources(c, "pr")
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 0 {
		t.Fatalf("got %+v, want no active resources after revoke", resources)
	}
}

func TestSubscribeReinstatesInsteadOfDuplicating(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	r := watcher.Resource{Type: "pr", ID: "owner/repo#4"}
	if err := Subscribe(c, "sub1", r, SubscribeOpts{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := Revoke(c, "sub1"); err != nil {
		t.Fatal(err)
	}
	if err := Subscribe(c, "sub1", r, SubscribeOpts{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := c.QueryRow(`SELECT COUNT(*) FROM watcher_subscriptions WHERE subscriber = ? AND resource_type = ? AND resource_id = ?`, "sub1", r.Type, r.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("got %d rows, want exactly 1 (reinstated, not duplicated)", count)
	}

	resources, err := ActiveResources(c, "pr")
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 {
		t.Fatalf("got %+v, want one active resource after re-subscribe", resources)
	}
}

func TestUnsubscribeRemovesOnlyThatResource(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	r1 := watcher.Resource{Type: "pr", ID: "owner/repo#5"}
	r2 := watcher.Resource{Type: "pr", ID: "owner/repo#6"}
	if err := Subscribe(c, "sub1", r1, SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := Subscribe(c, "sub1", r2, SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := Unsubscribe(c, "sub1", r1); err != nil {
		t.Fatal(err)
	}

	resources, err := ActiveResources(c, "pr")
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 || resources[0].ID != r2.ID {
		t.Fatalf("got %+v, want only %+v", resources, r2)
	}
}

func TestRenewExtendsLease(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	r := watcher.Resource{Type: "pr", ID: "owner/repo#7"}
	if err := Subscribe(c, "sub1", r, SubscribeOpts{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := c.Exec(`UPDATE watcher_subscriptions SET expires_at = ? WHERE subscriber = ?`, past, "sub1"); err != nil {
		t.Fatal(err)
	}
	if err := Renew(c, "sub1", time.Hour); err != nil {
		t.Fatal(err)
	}

	resources, err := ActiveResources(c, "pr")
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 {
		t.Fatalf("got %+v, want one active resource after renew", resources)
	}
}

func TestEventsForResourceOrderedAndFiltered(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	r := watcher.Resource{Type: "pr", ID: "owner/repo#8"}

	ts1 := "2026-01-15T12:00:00Z"
	ts2 := "2026-01-15T13:00:00Z"
	tsStart := "2026-01-15T11:00:00Z"

	if err := InsertEvent(c, watcher.Event{ID: "e-start", TS: tsStart, Source: "github", Type: watcher.EventTypeWatchStarted, Title: "watch started"}, r); err != nil {
		t.Fatal(err)
	}
	if err := InsertEvent(c, watcher.Event{ID: "e2", TS: ts2, Source: "github", Type: watcher.EventTypePRComment, Title: "second"}, r); err != nil {
		t.Fatal(err)
	}
	if err := InsertEvent(c, watcher.Event{ID: "e1", TS: ts1, Source: "github", Type: watcher.EventTypePRComment, Title: "first"}, r); err != nil {
		t.Fatal(err)
	}

	events, err := EventsForResource(c, r.Type, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (watch_started excluded)", len(events))
	}
	if events[0].ID != "e1" || events[1].ID != "e2" {
		t.Fatalf("got events %+v, want [e1, e2] in ts order", events)
	}
}

func TestEventsForSubscriberSince(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	rSubscribed := watcher.Resource{Type: "pr", ID: "owner/repo#9"}
	rOther := watcher.Resource{Type: "pr", ID: "owner/repo#10"}

	if err := Subscribe(c, "sub1", rSubscribed, SubscribeOpts{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}

	since := "2026-01-15T12:00:00Z"
	tsBefore := "2026-01-15T11:00:00Z"
	tsAfter := "2026-01-15T13:00:00Z"

	if err := InsertEvent(c, watcher.Event{ID: "e-start", TS: tsBefore, Source: "github", Type: watcher.EventTypeWatchStarted, Title: "watch started"}, rSubscribed); err != nil {
		t.Fatal(err)
	}
	if err := InsertEvent(c, watcher.Event{ID: "e-old", TS: tsBefore, Source: "github", Type: watcher.EventTypePRComment, Title: "old"}, rSubscribed); err != nil {
		t.Fatal(err)
	}
	if err := InsertEvent(c, watcher.Event{ID: "e-new", TS: tsAfter, Source: "github", Type: watcher.EventTypePRComment, Title: "new"}, rSubscribed); err != nil {
		t.Fatal(err)
	}
	if err := InsertEvent(c, watcher.Event{ID: "e-unrelated", TS: tsAfter, Source: "github", Type: watcher.EventTypePRComment, Title: "unrelated"}, rOther); err != nil {
		t.Fatal(err)
	}

	events, err := EventsForSubscriberSince(c, "sub1", since)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (only new event for subscribed resource): %+v", len(events), events)
	}
	if events[0].ID != "e-new" {
		t.Fatalf("got event %+v, want e-new", events[0])
	}
}

func TestEventsForSubscriberSinceExcludesRevokedSubscription(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	r := watcher.Resource{Type: "pr", ID: "owner/repo#11"}
	if err := Subscribe(c, "sub1", r, SubscribeOpts{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}

	since := "2026-01-15T12:00:00Z"
	tsAfter := "2026-01-15T13:00:00Z"
	if err := InsertEvent(c, watcher.Event{ID: "e-new", TS: tsAfter, Source: "github", Type: watcher.EventTypePRComment, Title: "new"}, r); err != nil {
		t.Fatal(err)
	}

	if err := Revoke(c, "sub1"); err != nil {
		t.Fatal(err)
	}

	events, err := EventsForSubscriberSince(c, "sub1", since)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("got %+v, want no events after revoke (lease no longer live)", events)
	}

	// The event itself still exists and is still visible via the
	// resource-scoped query; only subscriber-scoped delivery is gated
	// on a live subscription.
	resourceEvents, err := EventsForResource(c, r.Type, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(resourceEvents) != 1 || resourceEvents[0].ID != "e-new" {
		t.Fatalf("got %+v, want the event to still exist via EventsForResource", resourceEvents)
	}
}

func TestEventsForSubscriberSinceExcludesExpiredLease(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	r := watcher.Resource{Type: "pr", ID: "owner/repo#12"}
	if err := Subscribe(c, "sub1", r, SubscribeOpts{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := c.Exec(`UPDATE watcher_subscriptions SET expires_at = ? WHERE subscriber = ? AND resource_id = ?`, past, "sub1", r.ID); err != nil {
		t.Fatal(err)
	}

	since := "2026-01-15T12:00:00Z"
	tsAfter := "2026-01-15T13:00:00Z"
	if err := InsertEvent(c, watcher.Event{ID: "e-new", TS: tsAfter, Source: "github", Type: watcher.EventTypePRComment, Title: "new"}, r); err != nil {
		t.Fatal(err)
	}

	events, err := EventsForSubscriberSince(c, "sub1", since)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("got %+v, want no events for an expired lease", events)
	}
}

func TestListSubscriptionsActiveVsAll(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	r1 := watcher.Resource{Type: "pr", ID: "o/r#1", URL: "u1"}
	r2 := watcher.Resource{Type: "pr", ID: "o/r#2", URL: "u2"}
	if err := Subscribe(c, "handler:session:s1", r1, SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := Subscribe(c, "handler:session:s1", r2, SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	// soft-delete r2
	if err := Unsubscribe(c, "handler:session:s1", r2); err != nil {
		t.Fatal(err)
	}

	active, err := ActiveSubscriptions(c, "handler:session:s1", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Resource.ID != "o/r#1" {
		t.Fatalf("active: want [o/r#1], got %+v", active)
	}
	all, err := AllSubscriptions(c, "handler:session:s1", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all: want 2 rows, got %d", len(all))
	}
	// the deleted one must carry metadata
	var deleted *Subscription
	for i := range all {
		if all[i].Resource.ID == "o/r#2" {
			deleted = &all[i]
		}
	}
	if deleted == nil || deleted.DeletedAt == nil {
		t.Fatalf("deleted row should have DeletedAt set: %+v", deleted)
	}
}

func TestListSubscriptionsPrefix(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	if err := Subscribe(c, "handler:session:s1", watcher.Resource{Type: "pr", ID: "o/r#1"}, SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := Subscribe(c, "handler:session:s2", watcher.Resource{Type: "pr", ID: "o/r#2"}, SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	got, err := ActiveSubscriptions(c, "handler:session:", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("prefix active: want 2, got %d", len(got))
	}
	exact, err := ActiveSubscriptions(c, "handler:session:s1", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(exact) != 1 {
		t.Fatalf("exact active: want 1, got %d", len(exact))
	}
}

func TestSubscribersOf(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	r := watcher.Resource{Type: "pr", ID: "o/r#1"}
	if err := Subscribe(c, "handler:session:s1", r, SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := Subscribe(c, "handler:session:s2", r, SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	subs, err := SubscribersOf(c, "pr", "o/r#1")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 {
		t.Fatalf("want 2 subscribers, got %d", len(subs))
	}
}
