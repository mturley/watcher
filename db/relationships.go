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
//
// This relies on the unique index on (child_type, child_id, parent_type,
// parent_id, relationship) in schemaDDL to make the insert atomic: a
// single INSERT ... ON CONFLICT DO NOTHING closes the check-then-insert
// race that a separate SELECT-then-INSERT would have under concurrent
// writers (see GitHub issue #1).
func LinkResources(conn *sql.DB, child, parent watcher.Resource, relationship, source string) error {
	id := uuid.New().String()
	createdAt := time.Now().UTC().Format(time.RFC3339)

	if _, err := conn.Exec(`
		INSERT INTO watcher_resource_relationships
			(id, child_type, child_id, child_url, parent_type, parent_id, parent_url, relationship, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (child_type, child_id, parent_type, parent_id, relationship) DO NOTHING
	`, id, child.Type, child.ID, strPtr(child.URL), parent.Type, parent.ID, strPtr(parent.URL), relationship, source, createdAt); err != nil {
		return fmt.Errorf("failed to insert relationship: %w", err)
	}

	return nil
}

// SiblingResources returns the distinct child resources that share at least one
// parent with the given resource, excluding the resource itself. Used to find
// "related" resources (e.g. Jira issues under the same epic) without exposing
// the relationship table's shape to consumers.
func SiblingResources(conn *sql.DB, resource watcher.Resource) ([]watcher.Resource, error) {
	rows, err := conn.Query(`
		SELECT DISTINCT rr_other.child_type, rr_other.child_id
		FROM watcher_resource_relationships rr_mine
		JOIN watcher_resource_relationships rr_other
		  ON rr_other.parent_type = rr_mine.parent_type AND rr_other.parent_id = rr_mine.parent_id
		WHERE rr_mine.child_type = ? AND rr_mine.child_id = ?
		  AND (rr_other.child_type != rr_mine.child_type OR rr_other.child_id != rr_mine.child_id)
	`, resource.Type, resource.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to query sibling resources of %s/%s: %w", resource.Type, resource.ID, err)
	}
	defer rows.Close()

	var siblings []watcher.Resource
	for rows.Next() {
		var t, id string
		if err := rows.Scan(&t, &id); err != nil {
			return nil, fmt.Errorf("failed to scan sibling resource: %w", err)
		}
		siblings = append(siblings, watcher.Resource{Type: t, ID: id})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating sibling resources: %w", err)
	}
	return siblings, nil
}
