package db

import (
	"testing"

	"github.com/mturley/watcher"
)

// TestResourceMetaUpdatedAtColumn verifies the column exists after Migrate.
func TestResourceMetaUpdatedAtColumn(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	rows, err := c.Query(`PRAGMA table_info(watcher_resource_meta)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "updated_at" {
			found = true
		}
	}
	if !found {
		t.Fatal("watcher_resource_meta is missing the updated_at column")
	}
}

// TestSetResourceMetaAtPreservesTimestamp verifies the explicit-timestamp
// setter stores the given ts verbatim (needed so cross-DB replication
// converges instead of ping-ponging).
func TestSetResourceMetaAtPreservesTimestamp(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	r := watcher.Resource{Type: "slack", ID: "C1:1.2"}
	const ts = "2020-01-02T03:04:05Z"
	if err := SetResourceMetaAt(c, r, "Name", "Desc", ts); err != nil {
		t.Fatal(err)
	}
	m, err := GetResourceMeta(c, r.Type, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("meta is nil")
	}
	if m.CustomName != "Name" || m.CustomDescription != "Desc" {
		t.Errorf("name/desc = %q/%q", m.CustomName, m.CustomDescription)
	}
	if m.UpdatedAt != ts {
		t.Errorf("updated_at = %q, want %q", m.UpdatedAt, ts)
	}
}

// TestSetResourceMetaStampsNow verifies the convenience setter stamps a
// non-empty timestamp.
func TestSetResourceMetaStampsNow(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	r := watcher.Resource{Type: "slack", ID: "C1:9.9"}
	if err := SetResourceMeta(c, r, "N", "D"); err != nil {
		t.Fatal(err)
	}
	m, err := GetResourceMeta(c, r.Type, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil || m.UpdatedAt == "" {
		t.Fatalf("expected non-empty updated_at, got %+v", m)
	}
}

// TestMigrateBackfillsUpdatedAtOnExistingRow simulates a pre-v4 database
// (watcher_resource_meta without updated_at, holding a row) and verifies
// Migrate adds the column and backfills the existing row to a non-empty ts.
func TestMigrateBackfillsUpdatedAtOnExistingRow(t *testing.T) {
	c := mem(t)
	// Build the minimal pre-v4 shape: version table + old meta table + a row.
	stmts := []string{
		`CREATE TABLE watcher_schema_version (version INTEGER NOT NULL, migrated_at TEXT NOT NULL)`,
		`INSERT INTO watcher_schema_version (version, migrated_at) VALUES (3, '2020-01-01T00:00:00Z')`,
		`CREATE TABLE watcher_resource_meta (
			resource_type TEXT NOT NULL,
			resource_id TEXT NOT NULL,
			custom_name TEXT,
			custom_description TEXT,
			PRIMARY KEY (resource_type, resource_id)
		)`,
		`INSERT INTO watcher_resource_meta (resource_type, resource_id, custom_name, custom_description)
			VALUES ('slack', 'C1:1.1', 'Old', '')`,
	}
	for _, s := range stmts {
		if _, err := c.Exec(s); err != nil {
			t.Fatalf("setup exec: %v", err)
		}
	}
	if err := Migrate(c); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	m, err := GetResourceMeta(c, "slack", "C1:1.1")
	if err != nil {
		t.Fatal(err)
	}
	if m == nil || m.CustomName != "Old" {
		t.Fatalf("row lost in migration: %+v", m)
	}
	if m.UpdatedAt == "" {
		t.Error("existing row was not backfilled with updated_at")
	}
}
