package db

import (
	"database/sql"
	"fmt"
)

// ResourceState represents cached state of an external resource.
type ResourceState struct {
	ResourceType      string
	ResourceID        string
	StateJSON         string
	ResourceUpdatedAt string
	WatcherUpdatedAt  string
}

// UpsertResourceState inserts or updates the cached state for a resource.
func UpsertResourceState(conn *sql.DB, resourceType, resourceID, stateJSON, resourceUpdatedAt, watcherUpdatedAt string) error {
	_, err := conn.Exec(`
		INSERT INTO watcher_resource_state (resource_type, resource_id, state_json, resource_updated_at, watcher_updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(resource_type, resource_id) DO UPDATE SET
			state_json = excluded.state_json,
			resource_updated_at = excluded.resource_updated_at,
			watcher_updated_at = excluded.watcher_updated_at
	`, resourceType, resourceID, stateJSON, resourceUpdatedAt, watcherUpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to upsert resource state: %w", err)
	}
	return nil
}

// GetResourceState returns the cached state for a resource, or nil if none exists.
func GetResourceState(conn *sql.DB, resourceType, resourceID string) (*ResourceState, error) {
	var rs ResourceState
	err := conn.QueryRow(`
		SELECT resource_type, resource_id, state_json, resource_updated_at, watcher_updated_at
		FROM watcher_resource_state
		WHERE resource_type = ? AND resource_id = ?
	`, resourceType, resourceID).Scan(&rs.ResourceType, &rs.ResourceID, &rs.StateJSON, &rs.ResourceUpdatedAt, &rs.WatcherUpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get resource state: %w", err)
	}
	return &rs, nil
}

// DeleteResourceState removes the cached state for a resource.
func DeleteResourceState(conn *sql.DB, resourceType, resourceID string) error {
	_, err := conn.Exec(`DELETE FROM watcher_resource_state WHERE resource_type = ? AND resource_id = ?`,
		resourceType, resourceID)
	if err != nil {
		return fmt.Errorf("failed to delete resource state: %w", err)
	}
	return nil
}
