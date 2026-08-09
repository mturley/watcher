package db

import "database/sql"

// PollerStatus tracks the last success/error state of a poller.
type PollerStatus struct {
	Name             string
	LastSuccess      string
	LastError        string
	LastErrorMessage string
}

// GetPollerStatus returns the status for the named poller, or nil if none exists.
func GetPollerStatus(conn *sql.DB, name string) (*PollerStatus, error) {
	var ps PollerStatus
	var lastSuccess, lastError, lastErrorMsg sql.NullString
	err := conn.QueryRow(`
		SELECT name, last_success, last_error, last_error_message
		FROM watcher_poller_status WHERE name = ?
	`, name).Scan(&ps.Name, &lastSuccess, &lastError, &lastErrorMsg)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if lastSuccess.Valid {
		ps.LastSuccess = lastSuccess.String
	}
	if lastError.Valid {
		ps.LastError = lastError.String
	}
	if lastErrorMsg.Valid {
		ps.LastErrorMessage = lastErrorMsg.String
	}
	return &ps, nil
}

// RecordPollerSuccess records that the named poller succeeded just now.
func RecordPollerSuccess(conn *sql.DB, name string) error {
	_, err := conn.Exec(`
		INSERT INTO watcher_poller_status (name, last_success)
		VALUES (?, datetime('now'))
		ON CONFLICT(name) DO UPDATE SET last_success = datetime('now')
	`, name)
	return err
}

// RecordPollerError records that the named poller errored just now with the given message.
func RecordPollerError(conn *sql.DB, name, message string) error {
	_, err := conn.Exec(`
		INSERT INTO watcher_poller_status (name, last_error, last_error_message)
		VALUES (?, datetime('now'), ?)
		ON CONFLICT(name) DO UPDATE SET last_error = datetime('now'), last_error_message = ?
	`, name, message, message)
	return err
}

// HasPollerError returns true if the poller's last_error is more recent than last_success.
func HasPollerError(conn *sql.DB, name string) bool {
	ps, err := GetPollerStatus(conn, name)
	if err != nil || ps == nil {
		return false
	}
	if ps.LastError == "" {
		return false
	}
	if ps.LastSuccess == "" {
		return true
	}
	return ps.LastError > ps.LastSuccess
}
