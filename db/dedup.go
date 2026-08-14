package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/mturley/watcher"
)

// DedupCheck describes an event to check for duplication against
// existing watcher_events. At least one of ExternalTS or Title must be
// set:
//   - ExternalTS only: matches on the source's external timestamp.
//   - Title only: matches on the event title (used when external_ts
//     alone isn't unique, e.g. batch-submitted review comments that
//     share a createdAt but differ by file path in the title).
//   - Both set: matches only when BOTH the title AND external_ts equal
//     the existing event's. Required for review comments, where neither
//     field alone is a safe key: title alone collapses distinct
//     comments by the same author on the same file across different
//     reviews (GitHub issue: repeat CodeRabbit comments dropped), and
//     external_ts alone collapses distinct comments within one
//     batch-submitted review that share a createdAt.
//
// MatchTypeOnly, when true, overrides the above: the check matches
// purely on source+resource_type+resource_id+type, ignoring
// ExternalTS/Title entirely (they may be left unset). This is for
// terminal, once-per-resource events like PR merged/closed, whose
// natural "timestamp" (e.g. the PR's mutable updatedAt) can change
// after the event has already fired due to unrelated later activity
// (comments, label changes, CI) — keying on it would let it re-fire.
type DedupCheck struct {
	Source        string
	ResourceType  string
	ResourceID    string
	Type          watcher.EventType
	ExternalTS    *string
	Title         *string
	MatchTypeOnly bool
}

// IsDuplicate reports whether an event matching the check already
// exists. The matched columns depend on which of c.ExternalTS and
// c.Title are set (see DedupCheck), unless c.MatchTypeOnly is true, in
// which case only source+resource_type+resource_id+type are matched.
// When MatchTypeOnly is false, at least one of ExternalTS or Title
// must be set, or an error is returned.
func IsDuplicate(conn *sql.DB, c DedupCheck) (bool, error) {
	if !c.MatchTypeOnly && c.ExternalTS == nil && c.Title == nil {
		return false, errors.New("at least one of ExternalTS or Title must be set")
	}

	conds := []string{
		"e.source = ?",
		"er.resource_type = ?",
		"er.resource_id = ?",
		"e.type = ?",
	}
	argsList := []interface{}{c.Source, c.ResourceType, c.ResourceID, string(c.Type)}

	if !c.MatchTypeOnly {
		if c.ExternalTS != nil {
			conds = append(conds, "e.external_ts = ?")
			argsList = append(argsList, *c.ExternalTS)
		}
		if c.Title != nil {
			conds = append(conds, "e.title = ?")
			argsList = append(argsList, *c.Title)
		}
	}

	query := `
		SELECT 1
		FROM watcher_events e
		JOIN watcher_event_resources er ON er.event_id = e.id
		WHERE ` + strings.Join(conds, " AND ") + `
		LIMIT 1
	`

	var exists int
	err := conn.QueryRow(query, argsList...).Scan(&exists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("failed to query duplicate: %w", err)
	}

	return true, nil
}
