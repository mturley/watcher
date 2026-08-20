package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mturley/watcher"
)

// CICheckBundleTypes are the event types that identify a CheckRun rollup
// bundle. CIWorkflowBundleTypes identify the parallel StatusContext
// (gated-workflow) bundle. The two families share the "commit:<sha>" tag but
// have disjoint types so UpsertCIBundle can key each bundle's identity on its
// own family and the two never collide under the same commit tag.
var (
	CICheckBundleTypes = []watcher.EventType{
		watcher.EventTypeCIPassed,
		watcher.EventTypeCIFailed,
		watcher.EventTypeCIPending,
		watcher.EventTypeCIPartialFailure,
	}
	CIWorkflowBundleTypes = []watcher.EventType{
		watcher.EventTypeCIWorkflowsPassed,
		watcher.EventTypeCIWorkflowsFailed,
		watcher.EventTypeCIWorkflowsPending,
		watcher.EventTypeCIWorkflowsPartialFailure,
	}
)

// inClause builds a SQL "(?, ?, …)" placeholder list and matching []any args
// from a slice of event types.
func inClause(types []watcher.EventType) (string, []any) {
	placeholders := make([]string, len(types))
	args := make([]any, len(types))
	for i, t := range types {
		placeholders[i] = "?"
		args[i] = string(t)
	}
	return "(" + strings.Join(placeholders, ", ") + ")", args
}

// UpsertCIBundle finds or creates a CI bundle event for a specific commit
// on a resource. Identity is source='github', one of identityTypes, the given
// resource, and tags = "commit:<commitSHA>". If a matching bundle exists it
// updates the event's type, title, body, and timestamps in place; otherwise it
// inserts a new event with its associated resource row. identityTypes selects
// which bundle family this call owns (CICheckBundleTypes vs
// CIWorkflowBundleTypes) so the CheckRun and StatusContext bundles for the same
// commit don't collide.
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
func UpsertCIBundle(conn *sql.DB, commitSHA string, t watcher.EventType, title, body, externalTS string, r watcher.Resource, identityTypes []watcher.EventType) error {
	tag := "commit:" + commitSHA
	now := time.Now().UTC().Format(time.RFC3339)
	ctx := context.Background()
	typeIn, typeArgs := inClause(identityTypes)

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
	// commitFailed handles a failed COMMIT. The connection's transaction
	// state is unknown at that point (it may still be mid-transaction),
	// so best-effort ROLLBACK it and then poison the pinned *sql.Conn so
	// database/sql discards it from the pool on Close instead of handing
	// a possibly-still-in-transaction connection to the next caller.
	commitFailed := func(commitErr error) error {
		_, _ = c.ExecContext(ctx, "ROLLBACK")
		_ = c.Raw(func(driverConn any) error { return driver.ErrBadConn })
		return fmt.Errorf("failed to commit transaction: %w", commitErr)
	}

	var existingID string
	selectArgs := append(typeArgs, r.Type, r.ID, tag)
	err = c.QueryRowContext(ctx, `
		SELECT e.id FROM watcher_events e
		JOIN watcher_event_resources er ON e.id = er.event_id
		WHERE e.source = 'github'
		  AND e.type IN `+typeIn+`
		  AND er.resource_type = ? AND er.resource_id = ?
		  AND e.tags = ?
		LIMIT 1
	`, selectArgs...).Scan(&existingID)
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
			return commitFailed(err)
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
		return commitFailed(err)
	}

	return nil
}

// outOfDatePrefix is prepended to a bundle's title when the commit it covers is
// no longer the PR's latest, so a consumer that still has the event in its inbox
// doesn't mistake it for the current CI state.
const outOfDatePrefix = "[out of date] "

// MarkCIBundlesOutOfDate prepends outOfDatePrefix to the title of every CI/
// workflow bundle event (both families) for the given commit on a resource.
//
// It touches ONLY the title: ts and external_ts are deliberately left unchanged
// so the update does not re-surface a stale event in a consumer's inbox — the
// point is to flag an already-delivered event as superseded, not to re-notify.
// The NOT LIKE guard makes it idempotent: re-running never double-prefixes, so
// the poller can call it on every poll where the commit remains superseded.
// Zero matched rows is not an error (the previous commit may have had no
// bundles). A single UPDATE with no check-then-act means no BEGIN IMMEDIATE is
// needed.
func MarkCIBundlesOutOfDate(conn *sql.DB, commitSHA string, r watcher.Resource) error {
	tag := "commit:" + commitSHA
	allTypes := append(append([]watcher.EventType{}, CICheckBundleTypes...), CIWorkflowBundleTypes...)
	typeIn, typeArgs := inClause(allTypes)

	args := append([]any{outOfDatePrefix}, typeArgs...)
	args = append(args, r.Type, r.ID, tag, outOfDatePrefix+"%")

	_, err := conn.Exec(`
		UPDATE watcher_events
		SET title = ? || title
		WHERE id IN (
			SELECT e.id FROM watcher_events e
			JOIN watcher_event_resources er ON e.id = er.event_id
			WHERE e.source = 'github'
			  AND e.type IN `+typeIn+`
			  AND er.resource_type = ? AND er.resource_id = ?
			  AND e.tags = ?
			  AND e.title NOT LIKE ?
		)
	`, args...)
	if err != nil {
		return fmt.Errorf("failed to mark CI bundles out of date: %w", err)
	}
	return nil
}
