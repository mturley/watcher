package db

import (
	"database/sql"
	"fmt"

	"github.com/mturley/watcher"
)

// InsertEvent inserts an event into watcher_events and its associated
// resource into watcher_event_resources, in a single transaction.
func InsertEvent(conn *sql.DB, e watcher.Event, r watcher.Resource) error {
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	if _, err := tx.Exec(`
		INSERT INTO watcher_events (id, ts, external_ts, source, type, title, body, author, author_type, tags)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, e.ID, e.TS, e.ExternalTS, e.Source, string(e.Type), e.Title, e.Body, e.Author, e.AuthorType, e.Tags); err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to insert event: %w", err)
	}

	if _, err := tx.Exec(`
		INSERT INTO watcher_event_resources (event_id, resource_type, resource_id, resource_url)
		VALUES (?, ?, ?, ?)
	`, e.ID, r.Type, r.ID, strPtr(r.URL)); err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to insert event resource: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// EventCursor returns the maximum external_ts from events for the given
// source, resource type, and resource ID. Returns "" if no events exist.
func EventCursor(conn *sql.DB, source, resourceType, resourceID string) (string, error) {
	var cursor sql.NullString
	err := conn.QueryRow(`
		SELECT MAX(e.external_ts)
		FROM watcher_events e
		JOIN watcher_event_resources er ON er.event_id = e.id
		WHERE e.source = ? AND er.resource_type = ? AND er.resource_id = ?
	`, source, resourceType, resourceID).Scan(&cursor)
	if err != nil {
		return "", fmt.Errorf("failed to query event cursor: %w", err)
	}

	if !cursor.Valid {
		return "", nil
	}

	return cursor.String, nil
}

// strPtr converts a string to a string pointer. Returns nil if the
// string is empty.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
