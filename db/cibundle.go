package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/mturley/watcher"
)

// UpsertCIBundle finds or creates a CI bundle event for a specific commit
// on a resource. If a bundle already exists (matched by source='github',
// a CI event type, the given resource, and tags = "commit:<commitSHA>"),
// it updates the event's type, title, body, and timestamps in place.
// Otherwise it inserts a new event with its associated resource row.
//
// A CI bundle's identity (source + a set of CI event types + a resource
// join + a tags value) isn't expressible as a single-table UNIQUE
// constraint the way LinkResources's natural key is, so instead the
// entire check-then-act (SELECT, then UPDATE or INSERT+INSERT) runs on
// one pinned connection wrapped in BEGIN IMMEDIATE/COMMIT. BEGIN
// IMMEDIATE takes SQLite's write lock up front rather than lazily on
// first write (the default DEFERRED behavior of a plain conn.Begin()
// would still leave a race window between the SELECT and the write),
// which closes the check-then-insert race described in GitHub issue #1:
// a concurrent writer serializes behind this transaction instead of
// racing it. database/sql's *sql.Tx has no portable way to request
// BEGIN IMMEDIATE (the modernc.org/sqlite driver only offers it via a
// DSN-wide _txlock option, which would affect every transaction on the
// connection, not just this one), so we grab a single *sql.Conn from
// the pool and issue BEGIN IMMEDIATE / COMMIT / ROLLBACK as plain SQL on
// it directly.
func UpsertCIBundle(conn *sql.DB, commitSHA string, t watcher.EventType, title, body, externalTS string, r watcher.Resource) error {
	tag := "commit:" + commitSHA
	now := time.Now().UTC().Format(time.RFC3339)
	ctx := context.Background()

	c, err := conn.Conn(ctx)
	if err != nil {
		return fmt.Errorf("failed to acquire connection: %w", err)
	}
	defer c.Close()

	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	rollback := func(cause error) error {
		if _, rbErr := c.ExecContext(ctx, "ROLLBACK"); rbErr != nil {
			return fmt.Errorf("%w (rollback also failed: %v)", cause, rbErr)
		}
		return cause
	}

	var existingID string
	err = c.QueryRowContext(ctx, `
		SELECT e.id FROM watcher_events e
		JOIN watcher_event_resources er ON e.id = er.event_id
		WHERE e.source = 'github'
		  AND e.type IN ('ci_passed', 'ci_failed', 'ci_pending', 'ci_partial_failure')
		  AND er.resource_type = ? AND er.resource_id = ?
		  AND e.tags = ?
		LIMIT 1
	`, r.Type, r.ID, tag).Scan(&existingID)
	if err != nil && err != sql.ErrNoRows {
		return rollback(fmt.Errorf("failed to query existing CI bundle: %w", err))
	}

	if existingID != "" {
		if _, err := c.ExecContext(ctx, `
			UPDATE watcher_events SET type = ?, title = ?, body = ?, ts = ?, external_ts = ?
			WHERE id = ?
		`, string(t), title, body, now, externalTS, existingID); err != nil {
			return rollback(fmt.Errorf("failed to update CI bundle: %w", err))
		}
		if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
			return fmt.Errorf("failed to commit transaction: %w", err)
		}
		return nil
	}

	id := uuid.New().String()

	if _, err := c.ExecContext(ctx, `
		INSERT INTO watcher_events (id, ts, external_ts, source, type, title, body, tags)
		VALUES (?, ?, ?, 'github', ?, ?, ?, ?)
	`, id, now, externalTS, string(t), title, body, tag); err != nil {
		return rollback(fmt.Errorf("failed to insert CI bundle event: %w", err))
	}

	if _, err := c.ExecContext(ctx, `
		INSERT INTO watcher_event_resources (event_id, resource_type, resource_id, resource_url)
		VALUES (?, ?, ?, ?)
	`, id, r.Type, r.ID, strPtr(r.URL)); err != nil {
		return rollback(fmt.Errorf("failed to insert CI bundle event resource: %w", err))
	}

	if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}
