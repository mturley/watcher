package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/mturley/watcher"
)

// bookkeepingEventTypes lists event types that are internal to the
// watcher library and should never be surfaced to consumers via the
// read queries in this file.
var bookkeepingEventTypes = []string{"watch_started", "watcher_error"}

// SubscribeOpts controls how Subscribe creates or reinstates a
// subscription.
type SubscribeOpts struct {
	// TTL is how long the subscription's lease lasts before it expires.
	// If zero, the subscription never expires.
	TTL time.Duration
	// Backfill indicates the poller should emit historical events on
	// its first poll of this subscription, instead of only new ones.
	Backfill bool
}

// Subscribe creates a subscription for subscriber to resource r, or
// reinstates/updates an existing (possibly soft-deleted) subscription
// for the same subscriber+resource so at most one row exists for that
// pair.
func Subscribe(conn *sql.DB, subscriber string, r watcher.Resource, opts SubscribeOpts) error {
	now := time.Now().UTC().Format(time.RFC3339)
	var expiresAt *string
	if opts.TTL > 0 {
		e := time.Now().UTC().Add(opts.TTL).Format(time.RFC3339)
		expiresAt = &e
	}
	backfill := 0
	if opts.Backfill {
		backfill = 1
	}

	var existingID string
	err := conn.QueryRow(`
		SELECT id FROM watcher_subscriptions
		WHERE subscriber = ? AND resource_type = ? AND resource_id = ?
	`, subscriber, r.Type, r.ID).Scan(&existingID)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("failed to check for existing subscription: %w", err)
	}

	if existingID != "" {
		if _, err := conn.Exec(`
			UPDATE watcher_subscriptions
			SET resource_url = ?, expires_at = ?, backfill = ?, deleted_at = NULL
			WHERE id = ?
		`, strPtr(r.URL), expiresAt, backfill, existingID); err != nil {
			return fmt.Errorf("failed to reinstate subscription: %w", err)
		}
		return nil
	}

	id := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO watcher_subscriptions (id, subscriber, resource_type, resource_id, resource_url, created_at, expires_at, backfill, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)
	`, id, subscriber, r.Type, r.ID, strPtr(r.URL), now, expiresAt, backfill); err != nil {
		return fmt.Errorf("failed to insert subscription: %w", err)
	}
	return nil
}

// Renew extends the lease on all of subscriber's live subscriptions by
// setting expires_at to now+ttl.
func Renew(conn *sql.DB, subscriber string, ttl time.Duration) error {
	expiresAt := time.Now().UTC().Add(ttl).Format(time.RFC3339)
	_, err := conn.Exec(`
		UPDATE watcher_subscriptions SET expires_at = ?
		WHERE subscriber = ? AND deleted_at IS NULL
	`, expiresAt, subscriber)
	if err != nil {
		return fmt.Errorf("failed to renew subscriptions: %w", err)
	}
	return nil
}

// Revoke soft-deletes all of subscriber's live subscriptions.
func Revoke(conn *sql.DB, subscriber string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := conn.Exec(`
		UPDATE watcher_subscriptions SET deleted_at = ?
		WHERE subscriber = ? AND deleted_at IS NULL
	`, now, subscriber)
	if err != nil {
		return fmt.Errorf("failed to revoke subscriptions: %w", err)
	}
	return nil
}

// Unsubscribe soft-deletes subscriber's subscription to resource r.
func Unsubscribe(conn *sql.DB, subscriber string, r watcher.Resource) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := conn.Exec(`
		UPDATE watcher_subscriptions SET deleted_at = ?
		WHERE subscriber = ? AND resource_type = ? AND resource_id = ? AND deleted_at IS NULL
	`, now, subscriber, r.Type, r.ID)
	if err != nil {
		return fmt.Errorf("failed to unsubscribe: %w", err)
	}
	return nil
}

// ActiveResources returns the distinct set of resources of resourceType
// with at least one live (not soft-deleted, not lease-expired)
// subscription.
func ActiveResources(conn *sql.DB, resourceType string) ([]watcher.Resource, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := conn.Query(`
		SELECT DISTINCT resource_type, resource_id, resource_url
		FROM watcher_subscriptions
		WHERE resource_type = ? AND deleted_at IS NULL
		  AND (expires_at IS NULL OR expires_at > ?)
	`, resourceType, now)
	if err != nil {
		return nil, fmt.Errorf("failed to query active resources: %w", err)
	}
	defer rows.Close()

	var out []watcher.Resource
	for rows.Next() {
		var r watcher.Resource
		var url sql.NullString
		if err := rows.Scan(&r.Type, &r.ID, &url); err != nil {
			return nil, fmt.Errorf("failed to scan resource: %w", err)
		}
		if url.Valid {
			r.URL = url.String
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating active resources: %w", err)
	}
	return out, nil
}

// EventsForResource returns all non-bookkeeping events recorded for the
// given resource, ordered by ts ascending.
func EventsForResource(conn *sql.DB, resourceType, resourceID string) ([]watcher.Event, error) {
	rows, err := conn.Query(`
		SELECT e.id, e.ts, e.external_ts, e.source, e.type, e.title, e.body, e.author, e.author_type, e.tags
		FROM watcher_events e
		JOIN watcher_event_resources er ON er.event_id = e.id
		WHERE er.resource_type = ? AND er.resource_id = ?
		  AND e.type NOT IN (?, ?)
		ORDER BY e.ts ASC
	`, resourceType, resourceID, bookkeepingEventTypes[0], bookkeepingEventTypes[1])
	if err != nil {
		return nil, fmt.Errorf("failed to query events for resource: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// EventsForSubscriberSince returns all non-bookkeeping events with
// ts > since that were recorded for a resource the subscriber has a
// live (not soft-deleted, not lease-expired) subscription to, ordered
// by ts ascending.
func EventsForSubscriberSince(conn *sql.DB, subscriber, since string) ([]watcher.Event, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := conn.Query(`
		SELECT DISTINCT e.id, e.ts, e.external_ts, e.source, e.type, e.title, e.body, e.author, e.author_type, e.tags
		FROM watcher_events e
		JOIN watcher_event_resources er ON er.event_id = e.id
		JOIN watcher_subscriptions s ON s.resource_type = er.resource_type AND s.resource_id = er.resource_id
		WHERE s.subscriber = ? AND s.deleted_at IS NULL
		  AND (s.expires_at IS NULL OR s.expires_at > ?)
		  AND e.ts > ?
		  AND e.type NOT IN (?, ?)
		ORDER BY e.ts ASC
	`, subscriber, now, since, bookkeepingEventTypes[0], bookkeepingEventTypes[1])
	if err != nil {
		return nil, fmt.Errorf("failed to query events for subscriber: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// scanEvents scans rows in the id, ts, external_ts, source, type, title,
// body, author, author_type, tags column order into watcher.Event values.
func scanEvents(rows *sql.Rows) ([]watcher.Event, error) {
	var out []watcher.Event
	for rows.Next() {
		var e watcher.Event
		var externalTS, body, author, authorType, tags sql.NullString
		var typ string
		if err := rows.Scan(&e.ID, &e.TS, &externalTS, &e.Source, &typ, &e.Title, &body, &author, &authorType, &tags); err != nil {
			return nil, fmt.Errorf("failed to scan event: %w", err)
		}
		e.Type = watcher.EventType(typ)
		if externalTS.Valid {
			e.ExternalTS = &externalTS.String
		}
		if body.Valid {
			e.Body = &body.String
		}
		if author.Valid {
			e.Author = &author.String
		}
		if authorType.Valid {
			e.AuthorType = &authorType.String
		}
		if tags.Valid {
			e.Tags = &tags.String
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating events: %w", err)
	}
	return out, nil
}
