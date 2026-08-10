package db

import (
	"database/sql"
	"fmt"
	"strings"
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
	// IfAbsent, when true, makes Subscribe a no-op if a live subscription
	// already exists for this subscriber+resource, rather than refreshing
	// it. A soft-deleted subscription is not considered live: a non-user
	// tombstone is still reinstated, while a user tombstone is still left
	// alone (see Subscribe's doc comment).
	IfAbsent bool
}

// Subscribe creates a subscription for subscriber to resource r, or
// reinstates/updates an existing (possibly soft-deleted) subscription
// for the same subscriber+resource so at most one row exists for that
// pair.
//
// Behavior when a row for (subscriber, resource) already exists:
//   - live (deleted_at IS NULL): if opts.IfAbsent, no-op; otherwise refresh
//     url/expires_at/backfill and keep it live.
//   - soft-deleted with unsubscribed_by_user = 1 (a user-initiated
//     tombstone): left tombstoned; Reinstate is the only way to revive it.
//   - soft-deleted otherwise (a non-user tombstone, e.g. from Unsubscribe
//     or lease expiry): reinstated (deleted_at cleared, url/expires_at/
//     backfill refreshed), even when opts.IfAbsent is set, since such a
//     row is not live.
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
	var deletedAt sql.NullString
	var unsubUser int
	err := conn.QueryRow(`
		SELECT id, deleted_at, unsubscribed_by_user FROM watcher_subscriptions
		WHERE subscriber = ? AND resource_type = ? AND resource_id = ?
	`, subscriber, r.Type, r.ID).Scan(&existingID, &deletedAt, &unsubUser)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("failed to check for existing subscription: %w", err)
	}

	if existingID == "" {
		id := uuid.New().String()
		if _, err := conn.Exec(`
			INSERT INTO watcher_subscriptions (id, subscriber, resource_type, resource_id, resource_url, created_at, expires_at, backfill, deleted_at, unsubscribed_by_user)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, 0)
		`, id, subscriber, r.Type, r.ID, strPtr(r.URL), now, expiresAt, backfill); err != nil {
			return fmt.Errorf("failed to insert subscription: %w", err)
		}
		return nil
	}

	live := !deletedAt.Valid
	if live && opts.IfAbsent {
		return nil // don't disturb a live row
	}
	if !live && unsubUser == 1 {
		return nil // protected tombstone; use Reinstate to override
	}
	// live (non-IfAbsent) → refresh; or non-user tombstone → reinstate
	if _, err := conn.Exec(`
		UPDATE watcher_subscriptions
		SET resource_url = ?, expires_at = ?, backfill = ?, deleted_at = NULL
		WHERE id = ?
	`, strPtr(r.URL), expiresAt, backfill, existingID); err != nil {
		return fmt.Errorf("failed to reinstate/update subscription: %w", err)
	}
	return nil
}

// UserUnsubscribe soft-deletes subscriber's live subscription to resource
// r and marks the tombstone as user-initiated, so a later Subscribe will
// NOT auto-reinstate it (only Reinstate can).
func UserUnsubscribe(conn *sql.DB, subscriber string, r watcher.Resource) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := conn.Exec(`
		UPDATE watcher_subscriptions SET deleted_at = ?, unsubscribed_by_user = 1
		WHERE subscriber = ? AND resource_type = ? AND resource_id = ? AND deleted_at IS NULL
	`, now, subscriber, r.Type, r.ID)
	if err != nil {
		return fmt.Errorf("failed to user-unsubscribe: %w", err)
	}
	return nil
}

// Reinstate force-revives subscriber's subscription to resource r
// regardless of how or why it was removed, clearing both deleted_at and
// the user tombstone flag.
func Reinstate(conn *sql.DB, subscriber string, r watcher.Resource) error {
	_, err := conn.Exec(`
		UPDATE watcher_subscriptions SET deleted_at = NULL, unsubscribed_by_user = 0
		WHERE subscriber = ? AND resource_type = ? AND resource_id = ?
	`, subscriber, r.Type, r.ID)
	if err != nil {
		return fmt.Errorf("failed to reinstate subscription: %w", err)
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

// BackfillFor reports whether any live (not soft-deleted) subscription for
// the given resource requests backfill. Used by pollers to decide, on the
// first poll of a resource, whether to emit historical events or just a
// watch_started marker.
func BackfillFor(conn *sql.DB, resourceType, resourceID string) (bool, error) {
	var backfill sql.NullInt64
	err := conn.QueryRow(`
		SELECT MAX(backfill) FROM watcher_subscriptions
		WHERE resource_type = ? AND resource_id = ? AND deleted_at IS NULL
	`, resourceType, resourceID).Scan(&backfill)
	if err != nil {
		return false, fmt.Errorf("failed to query backfill flag: %w", err)
	}
	return backfill.Valid && backfill.Int64 == 1, nil
}

// bookkeepingExclusionClause builds a "NOT IN (?, ?, ...)" SQL fragment
// sized to len(bookkeepingEventTypes), plus the matching args, so that
// adding a bookkeeping type to that slice automatically extends every
// query that calls this helper without any other code changes.
func bookkeepingExclusionClause() (string, []any) {
	placeholders := make([]string, len(bookkeepingEventTypes))
	args := make([]any, len(bookkeepingEventTypes))
	for i, t := range bookkeepingEventTypes {
		placeholders[i] = "?"
		args[i] = t
	}
	clause := "e.type NOT IN (" + strings.Join(placeholders, ", ") + ")"
	return clause, args
}

// EventsForResource returns all non-bookkeeping events recorded for the
// given resource, ordered by ts ascending.
func EventsForResource(conn *sql.DB, resourceType, resourceID string) ([]watcher.Event, error) {
	exclClause, exclArgs := bookkeepingExclusionClause()
	args := append([]any{resourceType, resourceID}, exclArgs...)
	rows, err := conn.Query(`
		SELECT e.id, e.ts, e.external_ts, e.source, e.type, e.title, e.body, e.author, e.author_type, e.tags
		FROM watcher_events e
		JOIN watcher_event_resources er ON er.event_id = e.id
		WHERE er.resource_type = ? AND er.resource_id = ?
		  AND `+exclClause+`
		ORDER BY e.ts ASC
	`, args...)
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
	exclClause, exclArgs := bookkeepingExclusionClause()
	args := append([]any{subscriber, now, since}, exclArgs...)
	rows, err := conn.Query(`
		SELECT DISTINCT e.id, e.ts, e.external_ts, e.source, e.type, e.title, e.body, e.author, e.author_type, e.tags
		FROM watcher_events e
		JOIN watcher_event_resources er ON er.event_id = e.id
		JOIN watcher_subscriptions s ON s.resource_type = er.resource_type AND s.resource_id = er.resource_id
		WHERE s.subscriber = ? AND s.deleted_at IS NULL
		  AND (s.expires_at IS NULL OR s.expires_at > ?)
		  AND e.ts > ?
		  AND `+exclClause+`
		ORDER BY e.ts ASC
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query events for subscriber: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// Subscription is a row of watcher_subscriptions with lifecycle metadata.
type Subscription struct {
	ID                 string
	Subscriber         string
	Resource           watcher.Resource
	CreatedAt          string
	ExpiresAt          *string
	Backfill           bool
	DeletedAt          *string
	UnsubscribedByUser bool
}

// subscriberPredicate returns the WHERE fragment and arg for matching a
// subscriber exactly or by prefix.
func subscriberPredicate(subscriberOrPrefix string, prefix bool) (string, any) {
	if prefix {
		return "subscriber LIKE ?", subscriberOrPrefix + "%"
	}
	return "subscriber = ?", subscriberOrPrefix
}

const subscriptionColumns = `id, subscriber, resource_type, resource_id, resource_url, created_at, expires_at, backfill, deleted_at, unsubscribed_by_user`

func scanSubscriptions(rows *sql.Rows) ([]Subscription, error) {
	var out []Subscription
	for rows.Next() {
		var s Subscription
		var url, expiresAt, deletedAt sql.NullString
		var backfill, unsubUser int
		if err := rows.Scan(&s.ID, &s.Subscriber, &s.Resource.Type, &s.Resource.ID,
			&url, &s.CreatedAt, &expiresAt, &backfill, &deletedAt, &unsubUser); err != nil {
			return nil, fmt.Errorf("failed to scan subscription: %w", err)
		}
		if url.Valid {
			s.Resource.URL = url.String
		}
		if expiresAt.Valid {
			s.ExpiresAt = &expiresAt.String
		}
		if deletedAt.Valid {
			s.DeletedAt = &deletedAt.String
		}
		s.Backfill = backfill == 1
		s.UnsubscribedByUser = unsubUser == 1
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating subscriptions: %w", err)
	}
	return out, nil
}

// ActiveSubscriptions returns live (not soft-deleted, not lease-expired)
// subscriptions matching subscriber exactly, or by prefix when prefix is true.
func ActiveSubscriptions(conn *sql.DB, subscriberOrPrefix string, prefix bool) ([]Subscription, error) {
	pred, arg := subscriberPredicate(subscriberOrPrefix, prefix)
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := conn.Query(`SELECT `+subscriptionColumns+`
		FROM watcher_subscriptions
		WHERE `+pred+` AND deleted_at IS NULL AND (expires_at IS NULL OR expires_at > ?)
		ORDER BY created_at`, arg, now)
	if err != nil {
		return nil, fmt.Errorf("failed to query active subscriptions: %w", err)
	}
	defer rows.Close()
	return scanSubscriptions(rows)
}

// AllSubscriptions returns every subscription (including soft-deleted and
// lease-expired) matching subscriber exactly, or by prefix when prefix is true.
// Callers inspect DeletedAt / ExpiresAt / UnsubscribedByUser to see why a row
// is inactive.
func AllSubscriptions(conn *sql.DB, subscriberOrPrefix string, prefix bool) ([]Subscription, error) {
	pred, arg := subscriberPredicate(subscriberOrPrefix, prefix)
	rows, err := conn.Query(`SELECT `+subscriptionColumns+`
		FROM watcher_subscriptions
		WHERE `+pred+`
		ORDER BY created_at`, arg)
	if err != nil {
		return nil, fmt.Errorf("failed to query all subscriptions: %w", err)
	}
	defer rows.Close()
	return scanSubscriptions(rows)
}

// SubscribersOf returns all subscriptions (any state) for a given resource.
func SubscribersOf(conn *sql.DB, resourceType, resourceID string) ([]Subscription, error) {
	rows, err := conn.Query(`SELECT `+subscriptionColumns+`
		FROM watcher_subscriptions
		WHERE resource_type = ? AND resource_id = ?
		ORDER BY created_at`, resourceType, resourceID)
	if err != nil {
		return nil, fmt.Errorf("failed to query subscribers of resource: %w", err)
	}
	defer rows.Close()
	return scanSubscriptions(rows)
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
