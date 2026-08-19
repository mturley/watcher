package db

import (
	"database/sql"
	"fmt"

	"github.com/mturley/watcher"
)

// ResourceMeta holds user-supplied presentation metadata for a resource.
// Both fields are commonly empty; consumers fall back to the platform's own
// name (e.g. PR/Jira title, or a Slack thread's first message) when empty.
type ResourceMeta struct {
	CustomName        string
	CustomDescription string
}

// SetResourceMeta upserts the custom name/description for a resource. Empty
// strings are stored as-is (passing "" clears a field). Keyed per resource
// (resource_type, resource_id), independent of any subscriber.
func SetResourceMeta(conn *sql.DB, r watcher.Resource, name, description string) error {
	_, err := conn.Exec(`
		INSERT INTO watcher_resource_meta (resource_type, resource_id, custom_name, custom_description)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(resource_type, resource_id) DO UPDATE SET
			custom_name = excluded.custom_name,
			custom_description = excluded.custom_description
	`, r.Type, r.ID, name, description)
	if err != nil {
		return fmt.Errorf("failed to upsert resource meta: %w", err)
	}
	return nil
}

// GetResourceMeta returns the custom metadata for a resource, or nil if no
// row exists (never set).
func GetResourceMeta(conn *sql.DB, resourceType, resourceID string) (*ResourceMeta, error) {
	var m ResourceMeta
	err := conn.QueryRow(`
		SELECT custom_name, custom_description
		FROM watcher_resource_meta
		WHERE resource_type = ? AND resource_id = ?
	`, resourceType, resourceID).Scan(&m.CustomName, &m.CustomDescription)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get resource meta: %w", err)
	}
	return &m, nil
}
