package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/mturley/watcher"
)

// DedupCheck describes an event to check for duplication against
// existing watcher_events. Exactly one of ExternalTS or Title must be
// set: ExternalTS matches on the source's external timestamp; Title
// matches on the event title (used when external_ts alone isn't
// unique, e.g. batch-submitted review comments).
type DedupCheck struct {
	Source       string
	ResourceType string
	ResourceID   string
	Type         watcher.EventType
	ExternalTS   *string
	Title        *string
}

// IsDuplicate reports whether an event matching the check already
// exists. When c.ExternalTS is set, it matches on external_ts; when
// c.Title is set, it matches on title. Exactly one of the two must be
// set, or an error is returned.
func IsDuplicate(conn *sql.DB, c DedupCheck) (bool, error) {
	if (c.ExternalTS == nil) == (c.Title == nil) {
		return false, errors.New("exactly one of ExternalTS or Title must be set")
	}

	var query string
	var matchArg string
	if c.ExternalTS != nil {
		query = `
			SELECT 1
			FROM watcher_events e
			JOIN watcher_event_resources er ON er.event_id = e.id
			WHERE e.source = ? AND er.resource_type = ? AND er.resource_id = ? AND e.type = ? AND e.external_ts = ?
			LIMIT 1
		`
		matchArg = *c.ExternalTS
	} else {
		query = `
			SELECT 1
			FROM watcher_events e
			JOIN watcher_event_resources er ON er.event_id = e.id
			WHERE e.source = ? AND er.resource_type = ? AND er.resource_id = ? AND e.type = ? AND e.title = ?
			LIMIT 1
		`
		matchArg = *c.Title
	}

	var exists int
	err := conn.QueryRow(query, c.Source, c.ResourceType, c.ResourceID, string(c.Type), matchArg).Scan(&exists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("failed to query duplicate: %w", err)
	}

	return true, nil
}
