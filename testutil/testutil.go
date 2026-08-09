package testutil

import (
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mturley/watcher"
	"github.com/mturley/watcher/db"
	_ "modernc.org/sqlite"
)

// dbCounter gives each test DB a process-unique name so separate tests do
// not share the same shared-cache in-memory database.
var dbCounter atomic.Int64

// NewTestDB returns an in-memory SQLite DB with all watcher_* tables migrated.
// Each call gets an isolated database keyed by a unique name, so state does
// not leak between tests.
func NewTestDB(t *testing.T) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("%s_%d", strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()), dbCounter.Add(1))
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared&_pragma=busy_timeout(3000)", name)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.Migrate(conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return conn
}

// SeedEvents inserts each of events into conn for resource r, using
// db.InsertEvent. It is a convenience for tests that need pre-existing
// events without going through a poller.
func SeedEvents(conn *sql.DB, events []watcher.Event, r watcher.Resource) error {
	for _, e := range events {
		if err := db.InsertEvent(conn, e, r); err != nil {
			return fmt.Errorf("failed to seed event %s: %w", e.ID, err)
		}
	}
	return nil
}

// SeedSubscriptions subscribes subscriber to each of resources with
// default options (no TTL, no backfill). It is a convenience for tests
// that need pre-existing subscriptions without exercising Subscribe's
// option handling directly.
func SeedSubscriptions(conn *sql.DB, subscriber string, resources []watcher.Resource) error {
	for _, r := range resources {
		if err := db.Subscribe(conn, subscriber, r, db.SubscribeOpts{}); err != nil {
			return fmt.Errorf("failed to seed subscription for %s %s: %w", r.Type, r.ID, err)
		}
	}
	return nil
}

// MockGitHubPoller and MockJiraPoller (canned github.PRData /
// jira.IssueData fixtures) live in the sibling testutil/fixtures
// package rather than here. This package (testutil) is imported by
// github's and jira's own internal tests (for NewTestDB), so it cannot
// also import the github and jira packages without creating an import
// cycle. testutil/fixtures imports github and jira and is meant for
// external consumers (e.g. handler, worktree) that want canned poll
// data without network access; it is not imported back by github or
// jira.
