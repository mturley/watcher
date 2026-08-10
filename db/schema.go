// Package db contains the SQLite schema and migration logic for the
// watcher library. All tables owned by this package are prefixed
// watcher_ so they can safely coexist with tables owned by the host
// application in the same database.
package db

// CurrentSchemaVersion is the schema version this build of the library
// expects. Migrate creates/upgrades the database to this version.
const CurrentSchemaVersion = 1

// managedTables is the exact set of tables Migrate owns.
var managedTables = []string{
	"watcher_schema_version", "watcher_events", "watcher_event_resources",
	"watcher_resource_state", "watcher_subscriptions",
	"watcher_resource_relationships", "watcher_poller_status",
}

// managedColumns maps each managed table to its expected set of column
// names, used by the collision check in Migrate to detect a pre-existing
// table that doesn't match the schema this package manages.
var managedColumns = map[string][]string{
	"watcher_schema_version": {"version", "migrated_at"},
	"watcher_events": {
		"id", "ts", "external_ts", "source", "type", "title", "body",
		"author", "author_type", "tags",
	},
	"watcher_event_resources": {
		"event_id", "resource_type", "resource_id", "resource_url",
	},
	"watcher_resource_state": {
		"resource_type", "resource_id", "state_json", "resource_updated_at",
		"watcher_updated_at",
	},
	"watcher_subscriptions": {
		"id", "subscriber", "resource_type", "resource_id", "resource_url",
		"created_at", "expires_at", "backfill", "deleted_at", "unsubscribed_by_user",
	},
	"watcher_resource_relationships": {
		"id", "child_type", "child_id", "child_url", "parent_type",
		"parent_id", "parent_url", "relationship", "source", "created_at",
	},
	"watcher_poller_status": {
		"name", "last_success", "last_error", "last_error_message",
	},
}

// schemaDDL creates all watcher_* tables and indexes if they don't
// already exist. Timestamps are stored as RFC3339 UTC strings.
const schemaDDL = `
CREATE TABLE IF NOT EXISTS watcher_schema_version (
	version INTEGER,
	migrated_at TEXT
);

CREATE TABLE IF NOT EXISTS watcher_events (
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
);

CREATE TABLE IF NOT EXISTS watcher_event_resources (
	event_id TEXT NOT NULL,
	resource_type TEXT NOT NULL,
	resource_id TEXT NOT NULL,
	resource_url TEXT
);

CREATE TABLE IF NOT EXISTS watcher_resource_state (
	resource_type TEXT NOT NULL,
	resource_id TEXT NOT NULL,
	state_json TEXT NOT NULL,
	resource_updated_at TEXT NOT NULL,
	watcher_updated_at TEXT NOT NULL,
	PRIMARY KEY (resource_type, resource_id)
);

CREATE TABLE IF NOT EXISTS watcher_subscriptions (
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
);

CREATE TABLE IF NOT EXISTS watcher_resource_relationships (
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
);

CREATE TABLE IF NOT EXISTS watcher_poller_status (
	name TEXT PRIMARY KEY,
	last_success TEXT,
	last_error TEXT,
	last_error_message TEXT
);

CREATE INDEX IF NOT EXISTS idx_watcher_events_ts ON watcher_events (ts);
CREATE INDEX IF NOT EXISTS idx_watcher_events_source_type ON watcher_events (source, type);
CREATE INDEX IF NOT EXISTS idx_watcher_event_resources_event_id ON watcher_event_resources (event_id);
CREATE INDEX IF NOT EXISTS idx_watcher_event_resources_resource ON watcher_event_resources (resource_type, resource_id);
CREATE INDEX IF NOT EXISTS idx_watcher_subscriptions_resource ON watcher_subscriptions (resource_type, resource_id, deleted_at, expires_at);
CREATE INDEX IF NOT EXISTS idx_watcher_subscriptions_subscriber ON watcher_subscriptions (subscriber);
`
