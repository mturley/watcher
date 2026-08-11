package db

import (
	"database/sql"
	"fmt"
	"testing"

	_ "modernc.org/sqlite"
)

func mem(t *testing.T) *sql.DB {
	t.Helper()
	c, err := sql.Open("sqlite", "file::memory:?cache=shared&_pragma=busy_timeout(3000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestMigrateSetsVersion(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	v, err := SchemaVersion(c)
	if err != nil {
		t.Fatal(err)
	}
	if v != CurrentSchemaVersion {
		t.Errorf("version %d, want %d", v, CurrentSchemaVersion)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(c); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestMigrateAbortsOnAlienWatcherTable(t *testing.T) {
	c := mem(t)
	if _, err := c.Exec(`CREATE TABLE watcher_events (wrong INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(c); err == nil {
		t.Fatal("expected Migrate to abort on a pre-existing watcher_events with unexpected schema")
	}
}

// seedV1Tables creates all watcher_* tables at their v1 shape (no
// idx_watcher_resource_relationships_unique) and records schema version
// 1, mimicking a database migrated by a pre-issue-1-fix build of this
// library. subscriptionsHasUnsubscribedByUser controls whether
// watcher_subscriptions is created with the unsubscribed_by_user column
// (added in 595ff43 without a version bump) or without it (the shape of
// a database migrated before that commit).
func seedV1Tables(t *testing.T, c *sql.DB, subscriptionsHasUnsubscribedByUser bool) {
	t.Helper()

	subscriptionsDDL := `
		CREATE TABLE watcher_subscriptions (
			id TEXT PRIMARY KEY,
			subscriber TEXT NOT NULL,
			resource_type TEXT NOT NULL,
			resource_id TEXT NOT NULL,
			resource_url TEXT,
			created_at TEXT NOT NULL,
			expires_at TEXT,
			backfill INTEGER NOT NULL DEFAULT 0,
			deleted_at TEXT
		);`
	if subscriptionsHasUnsubscribedByUser {
		subscriptionsDDL = `
		CREATE TABLE watcher_subscriptions (
			id TEXT PRIMARY KEY,
			subscriber TEXT NOT NULL,
			resource_type TEXT NOT NULL,
			resource_id TEXT NOT NULL,
			resource_url TEXT,
			created_at TEXT NOT NULL,
			expires_at TEXT,
			backfill INTEGER NOT NULL DEFAULT 0,
			deleted_at TEXT,
			unsubscribed_by_user INTEGER NOT NULL DEFAULT 0
		);`
	}

	stmts := []string{
		`CREATE TABLE watcher_schema_version (version INTEGER, migrated_at TEXT);`,
		`CREATE TABLE watcher_events (
			id TEXT PRIMARY KEY,
			ts TEXT NOT NULL,
			external_ts TEXT,
			source TEXT NOT NULL,
			type TEXT NOT NULL,
			title TEXT NOT NULL,
			body TEXT,
			author TEXT,
			author_type TEXT,
			tags TEXT
		);`,
		`CREATE TABLE watcher_event_resources (
			event_id TEXT NOT NULL,
			resource_type TEXT NOT NULL,
			resource_id TEXT NOT NULL,
			resource_url TEXT
		);`,
		`CREATE TABLE watcher_resource_state (
			resource_type TEXT NOT NULL,
			resource_id TEXT NOT NULL,
			state_json TEXT NOT NULL,
			resource_updated_at TEXT NOT NULL,
			watcher_updated_at TEXT NOT NULL,
			PRIMARY KEY (resource_type, resource_id)
		);`,
		subscriptionsDDL,
		`CREATE TABLE watcher_resource_relationships (
			id TEXT PRIMARY KEY,
			child_type TEXT NOT NULL,
			child_id TEXT NOT NULL,
			child_url TEXT,
			parent_type TEXT NOT NULL,
			parent_id TEXT NOT NULL,
			parent_url TEXT,
			relationship TEXT NOT NULL,
			source TEXT NOT NULL,
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE watcher_poller_status (
			name TEXT PRIMARY KEY,
			last_success TEXT,
			last_error TEXT,
			last_error_message TEXT
		);`,
	}
	for _, stmt := range stmts {
		if _, err := c.Exec(stmt); err != nil {
			t.Fatalf("seed v1 table: %v (stmt: %s)", err, stmt)
		}
	}
	if _, err := c.Exec(`INSERT INTO watcher_schema_version (version, migrated_at) VALUES (1, '2020-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed v1 schema version: %v", err)
	}
}

func hasColumn(t *testing.T, c *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := c.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == column {
			return true
		}
	}
	return false
}

func hasIndex(t *testing.T, c *sql.DB, indexName string) bool {
	t.Helper()
	var count int
	if err := c.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, indexName,
	).Scan(&count); err != nil {
		t.Fatalf("checking for index %q: %v", indexName, err)
	}
	return count > 0
}

// TestMigrateV1NineColumnSubscriptionsUpgrades reproduces the CRITICAL
// finding from review: a database migrated to v1 BEFORE 595ff43 added
// unsubscribed_by_user has a 9-column watcher_subscriptions. Migrate
// must reconcile that gap (via ensureAdditiveColumns) rather than
// rejecting the table as an unexpected-schema collision, and must reach
// v2 with the new unique index in place.
func TestMigrateV1NineColumnSubscriptionsUpgrades(t *testing.T) {
	c := mem(t)
	seedV1Tables(t, c, false)

	if err := Migrate(c); err != nil {
		t.Fatalf("Migrate on pre-595ff43 (9-column) v1 DB: %v", err)
	}

	if !hasColumn(t, c, "watcher_subscriptions", "unsubscribed_by_user") {
		t.Fatal("watcher_subscriptions missing unsubscribed_by_user column after migration")
	}
	if !hasIndex(t, c, "idx_watcher_resource_relationships_unique") {
		t.Fatal("missing idx_watcher_resource_relationships_unique after migration")
	}
	v, err := SchemaVersion(c)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != CurrentSchemaVersion {
		t.Fatalf("version = %d, want %d", v, CurrentSchemaVersion)
	}
}

// TestMigrateV1TenColumnSubscriptionsUpgrades covers the other v1 shape:
// a database migrated to v1 AFTER 595ff43, which already has the
// unsubscribed_by_user column. This is the shape the review confirmed
// exists in the real deployed DB. Migrate must still succeed and reach
// v2 cleanly.
func TestMigrateV1TenColumnSubscriptionsUpgrades(t *testing.T) {
	c := mem(t)
	seedV1Tables(t, c, true)

	if err := Migrate(c); err != nil {
		t.Fatalf("Migrate on post-595ff43 (10-column) v1 DB: %v", err)
	}

	if !hasColumn(t, c, "watcher_subscriptions", "unsubscribed_by_user") {
		t.Fatal("watcher_subscriptions missing unsubscribed_by_user column after migration")
	}
	if !hasIndex(t, c, "idx_watcher_resource_relationships_unique") {
		t.Fatal("missing idx_watcher_resource_relationships_unique after migration")
	}
	v, err := SchemaVersion(c)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != CurrentSchemaVersion {
		t.Fatalf("version = %d, want %d", v, CurrentSchemaVersion)
	}
}

// TestMigrateFailsSafeOnDuplicateRelationships confirms the documented
// fail-safe behavior for the one scenario ensureAdditiveColumns cannot
// help with: pre-existing duplicate watcher_resource_relationships rows
// block the v2 unique index from being created. Migrate should return a
// loud error and leave the database at v1, not corrupt or silently drop
// data.
func TestMigrateFailsSafeOnDuplicateRelationships(t *testing.T) {
	c := mem(t)
	seedV1Tables(t, c, true)

	dup := `INSERT INTO watcher_resource_relationships
		(id, child_type, child_id, child_url, parent_type, parent_id, parent_url, relationship, source, created_at)
		VALUES (?, 'jira', 'TASK-1', NULL, 'jira', 'EPIC-1', NULL, 'epic', 'test', '2020-01-01T00:00:00Z')`
	if _, err := c.Exec(dup, "dup-1"); err != nil {
		t.Fatalf("seed dup 1: %v", err)
	}
	if _, err := c.Exec(dup, "dup-2"); err != nil {
		t.Fatalf("seed dup 2: %v", err)
	}

	if err := Migrate(c); err == nil {
		t.Fatal("expected Migrate to fail when pre-existing duplicate relationships block the unique index")
	}

	v, err := SchemaVersion(c)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != 1 {
		t.Fatalf("version = %d, want 1 (migration should not have advanced)", v)
	}
}

func TestMigrateAddsUnsubscribedByUserColumn(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	rows, err := c.Query(`PRAGMA table_info(watcher_subscriptions)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "unsubscribed_by_user" {
			found = true
		}
	}
	if !found {
		t.Fatal("watcher_subscriptions missing unsubscribed_by_user column")
	}
}
