package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/mturley/watcher"
)

// ResourceMeta holds user-supplied presentation metadata for a resource.
// Both fields are commonly empty; consumers fall back to the platform's own
// name (e.g. PR/Jira title, or a Slack thread's first message) when empty.
type ResourceMeta struct {
	CustomName        string
	CustomDescription string
	// UpdatedAt is the RFC3339 UTC timestamp of the last name/description
	// write. It drives newest-wins conflict resolution when a name is
	// replicated between separate watcher databases (e.g. worktree ↔
	// handler). "" only for rows never rewritten since before the column
	// existed; such rows sort oldest.
	UpdatedAt string
}

// SetResourceMetaAt upserts the custom name/description for a resource with an
// explicit updated_at timestamp. Use this for REPLICATION — pass the origin
// database's timestamp so both sides converge to an identical (name,
// updated_at) pair instead of ping-ponging. Empty strings are stored as-is
// (passing "" clears a field). Keyed per resource (resource_type,
// resource_id), independent of any subscriber.
func SetResourceMetaAt(conn *sql.DB, r watcher.Resource, name, description, updatedAt string) error {
	_, err := conn.Exec(`
		INSERT INTO watcher_resource_meta (resource_type, resource_id, custom_name, custom_description, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(resource_type, resource_id) DO UPDATE SET
			custom_name = excluded.custom_name,
			custom_description = excluded.custom_description,
			updated_at = excluded.updated_at
	`, r.Type, r.ID, name, description, updatedAt)
	if err != nil {
		return fmt.Errorf("failed to upsert resource meta: %w", err)
	}
	return nil
}

// SetResourceMeta upserts the custom name/description, stamping updated_at with
// the current time. Use this for interactive/local edits; use
// SetResourceMetaAt to preserve an origin timestamp during replication.
func SetResourceMeta(conn *sql.DB, r watcher.Resource, name, description string) error {
	return SetResourceMetaAt(conn, r, name, description, time.Now().UTC().Format(time.RFC3339))
}

// GetResourceMeta returns the custom metadata for a resource, or nil if no
// row exists (never set).
func GetResourceMeta(conn *sql.DB, resourceType, resourceID string) (*ResourceMeta, error) {
	var m ResourceMeta
	err := conn.QueryRow(`
		SELECT custom_name, custom_description, COALESCE(updated_at, '')
		FROM watcher_resource_meta
		WHERE resource_type = ? AND resource_id = ?
	`, resourceType, resourceID).Scan(&m.CustomName, &m.CustomDescription, &m.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get resource meta: %w", err)
	}
	return &m, nil
}
