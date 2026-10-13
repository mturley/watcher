package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mturley/watcher"
)

// ResourcePollDetail is the JSON stored in last_detail: what the most recent
// poll of one resource saw. For a batched GitHub call, DurationMS and
// HTTPStatus are the batch's, shared by every PR in it.
type ResourcePollDetail struct {
	OK            bool   `json:"ok"`
	DurationMS    int    `json:"duration_ms,omitempty"`
	HTTPStatus    int    `json:"http_status,omitempty"`
	EventsEmitted int    `json:"events_emitted"`
	Note          string `json:"note,omitempty"`
}

type ResourceKey struct{ Type, ID string }

type ResourcePollStatus struct {
	Type, ID         string
	LastAttempt      string
	LastSuccess      string
	LastError        string
	LastErrorMessage string
	Detail           *ResourcePollDetail
}

// Failing reports whether the last error is newer than the last success,
// the same rule as HasPollerError, applied per resource.
func (s ResourcePollStatus) Failing() bool {
	return s.LastError != "" && s.LastError > s.LastSuccess
}

// RecordResourcePoll records one resource's poll outcome. The previous error
// message is kept after a success so the UI can still say when it last failed.
func RecordResourcePoll(conn *sql.DB, r watcher.Resource, ok bool, message string, d ResourcePollDetail) error {
	d.OK = ok
	now := time.Now().UTC().Format(PollLogTSFormat)
	detail, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if ok {
		_, err = conn.Exec(`INSERT INTO watcher_resource_poll_status
			(resource_type, resource_id, last_attempt, last_success, last_detail) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(resource_type, resource_id) DO UPDATE SET
			last_attempt = excluded.last_attempt, last_success = excluded.last_success, last_detail = excluded.last_detail`,
			r.Type, r.ID, now, now, string(detail))
	} else {
		_, err = conn.Exec(`INSERT INTO watcher_resource_poll_status
			(resource_type, resource_id, last_attempt, last_error, last_error_message, last_detail) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(resource_type, resource_id) DO UPDATE SET
			last_attempt = excluded.last_attempt, last_error = excluded.last_error,
			last_error_message = excluded.last_error_message, last_detail = excluded.last_detail`,
			r.Type, r.ID, now, now, message, string(detail))
	}
	if err != nil {
		return fmt.Errorf("record resource poll: %w", err)
	}
	return nil
}

// GetResourcePollStatuses returns status rows for the given resources; a
// resource that has never been polled is absent from the map.
func GetResourcePollStatuses(conn *sql.DB, rs []watcher.Resource) (map[ResourceKey]ResourcePollStatus, error) {
	out := make(map[ResourceKey]ResourcePollStatus, len(rs))
	for _, r := range rs {
		var st ResourcePollStatus
		var attempt, success, lastErr, msg, detail sql.NullString
		err := conn.QueryRow(`SELECT resource_type, resource_id, last_attempt, last_success, last_error, last_error_message, last_detail
			FROM watcher_resource_poll_status WHERE resource_type = ? AND resource_id = ?`, r.Type, r.ID).
			Scan(&st.Type, &st.ID, &attempt, &success, &lastErr, &msg, &detail)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get resource poll status: %w", err)
		}
		st.LastAttempt, st.LastSuccess, st.LastError, st.LastErrorMessage = attempt.String, success.String, lastErr.String, msg.String
		if detail.Valid {
			var d ResourcePollDetail
			if json.Unmarshal([]byte(detail.String), &d) == nil {
				st.Detail = &d
			}
		}
		out[ResourceKey{r.Type, r.ID}] = st
	}
	return out, nil
}
