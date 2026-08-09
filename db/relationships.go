package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/mturley/watcher"
)

// LinkResources records a relationship between a child and parent resource.
// It is idempotent: if an identical child+parent+relationship row already
// exists, it does nothing.
func LinkResources(conn *sql.DB, child, parent watcher.Resource, relationship, source string) error {
	var exists int
	err := conn.QueryRow(`
		SELECT 1 FROM watcher_resource_relationships
		WHERE child_type = ? AND child_id = ? AND parent_type = ? AND parent_id = ? AND relationship = ?
		LIMIT 1
	`, child.Type, child.ID, parent.Type, parent.ID, relationship).Scan(&exists)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("failed to query existing relationship: %w", err)
	}
	if exists == 1 {
		return nil
	}

	id := uuid.New().String()
	createdAt := time.Now().UTC().Format(time.RFC3339)

	if _, err := conn.Exec(`
		INSERT INTO watcher_resource_relationships
			(id, child_type, child_id, child_url, parent_type, parent_id, parent_url, relationship, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, child.Type, child.ID, strPtr(child.URL), parent.Type, parent.ID, strPtr(parent.URL), relationship, source, createdAt); err != nil {
		return fmt.Errorf("failed to insert relationship: %w", err)
	}

	return nil
}
