package db

import (
	"database/sql"
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
