package db

import (
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
func UpsertCIBundle(conn *sql.DB, commitSHA string, t watcher.EventType, title, body, externalTS string, r watcher.Resource) error {
	tag := "commit:" + commitSHA
	now := time.Now().UTC().Format(time.RFC3339)

	var existingID string
	err := conn.QueryRow(`
		SELECT e.id FROM watcher_events e
		JOIN watcher_event_resources er ON e.id = er.event_id
		WHERE e.source = 'github'
		  AND e.type IN ('ci_passed', 'ci_failed', 'ci_pending', 'ci_partial_failure')
		  AND er.resource_type = ? AND er.resource_id = ?
		  AND e.tags = ?
		LIMIT 1
	`, r.Type, r.ID, tag).Scan(&existingID)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("failed to query existing CI bundle: %w", err)
	}

	if existingID != "" {
		if _, err := conn.Exec(`
			UPDATE watcher_events SET type = ?, title = ?, body = ?, ts = ?, external_ts = ?
			WHERE id = ?
		`, string(t), title, body, now, externalTS, existingID); err != nil {
			return fmt.Errorf("failed to update CI bundle: %w", err)
		}
		return nil
	}

	id := uuid.New().String()

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	if _, err := tx.Exec(`
		INSERT INTO watcher_events (id, ts, external_ts, source, type, title, body, tags)
		VALUES (?, ?, ?, 'github', ?, ?, ?, ?)
	`, id, now, externalTS, string(t), title, body, tag); err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to insert CI bundle event: %w", err)
	}

	if _, err := tx.Exec(`
		INSERT INTO watcher_event_resources (event_id, resource_type, resource_id, resource_url)
		VALUES (?, ?, ?, ?)
	`, id, r.Type, r.ID, strPtr(r.URL)); err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to insert CI bundle event resource: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}
