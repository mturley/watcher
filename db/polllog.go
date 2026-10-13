package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/mturley/watcher"
)

// PollLogTSFormat is fixed-width so ts sorts and compares lexically (RFC3339Nano
// trims trailing zeros and would not).
const PollLogTSFormat = "2006-01-02T15:04:05.000000Z"

// PollLogEntry is one row of the watcher log. See the watcher log design spec
// in the worktree repo for what each kind means.
type PollLogEntry struct {
	ID         int64
	TS         string // PollLogTSFormat; InsertPollLog fills it when empty
	Source     string // github | jira | slack
	Level      string // info | warn | error
	Kind       string // poll | http | resource
	Message    string
	Method     string
	Endpoint   string
	HTTPStatus int
	DurationMS int
	Bytes      int64
	ErrorBody  string
	Resources  []watcher.Resource // Type + ID only
}

// PollLogQuery filters QueryPollLog. ResourceScoped distinguishes "no resource
// filter" from "filter to this (possibly empty) set".
type PollLogQuery struct {
	Sources        []string // empty = all
	MinLevel       string   // "" or "info" = all; "warn" = warn+error; "error"
	ResourceScoped bool     // true = filter by Resources (even if empty)
	Resources      []watcher.Resource
	AfterID        int64 // > 0: id > AfterID
	BeforeID       int64 // > 0: id < BeforeID
	Limit          int   // <= 0 → 200
}

// SourceForType maps a resource type to the poller (log source) that polls it.
func SourceForType(resourceType string) string {
	if resourceType == "pr" {
		return "github"
	}
	return resourceType
}

func levelsAtLeast(min string) []string {
	switch min {
	case "warn":
		return []string{"warn", "error"}
	case "error":
		return []string{"error"}
	default:
		return nil
	}
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

// InsertPollLog writes one log row and its resource tags in one transaction.
func InsertPollLog(conn *sql.DB, e PollLogEntry) (int64, error) {
	if e.TS == "" {
		e.TS = time.Now().UTC().Format(PollLogTSFormat)
	}
	tx, err := conn.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin poll log insert: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO watcher_poll_log
		(ts, source, level, kind, message, method, endpoint, http_status, duration_ms, bytes, error_body)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TS, e.Source, e.Level, e.Kind, e.Message, nullStr(e.Method), nullStr(e.Endpoint),
		nullInt(int64(e.HTTPStatus)), nullInt(int64(e.DurationMS)), nullInt(e.Bytes), nullStr(e.ErrorBody))
	if err != nil {
		return 0, fmt.Errorf("insert poll log: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, r := range e.Resources {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO watcher_poll_log_resources (log_id, resource_type, resource_id) VALUES (?, ?, ?)`,
			id, r.Type, r.ID); err != nil {
			return 0, fmt.Errorf("insert poll log resource: %w", err)
		}
	}
	return id, tx.Commit()
}

// QueryPollLog returns matching entries newest first, plus whether more rows
// exist beyond Limit in the same direction.
func QueryPollLog(conn *sql.DB, q PollLogQuery) ([]PollLogEntry, bool, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	var where []string
	var args []any
	if len(q.Sources) > 0 {
		where = append(where, "l.source IN ("+placeholders(len(q.Sources))+")")
		for _, s := range q.Sources {
			args = append(args, s)
		}
	}
	if lv := levelsAtLeast(q.MinLevel); lv != nil {
		where = append(where, "l.level IN ("+placeholders(len(lv))+")")
		for _, s := range lv {
			args = append(args, s)
		}
	}
	if q.AfterID > 0 {
		where = append(where, "l.id > ?")
		args = append(args, q.AfterID)
	}
	if q.BeforeID > 0 {
		where = append(where, "l.id < ?")
		args = append(args, q.BeforeID)
	}
	if q.ResourceScoped {
		if len(q.Resources) == 0 {
			return nil, false, nil
		}
		var match []string
		sources := map[string]bool{}
		for _, r := range q.Resources {
			match = append(match, "(resource_type = ? AND resource_id = ?)")
			args = append(args, r.Type, r.ID)
			sources[SourceForType(r.Type)] = true
		}
		var srcList []string
		for s := range sources {
			srcList = append(srcList, s)
		}
		clause := "(l.id IN (SELECT log_id FROM watcher_poll_log_resources WHERE " + strings.Join(match, " OR ") + ")" +
			" OR (l.kind = 'poll' AND l.source IN (" + placeholders(len(srcList)) + ")" +
			" AND NOT EXISTS (SELECT 1 FROM watcher_poll_log_resources x WHERE x.log_id = l.id)))"
		for _, s := range srcList {
			args = append(args, s)
		}
		where = append(where, clause)
	}
	sqlStr := `SELECT l.id, l.ts, l.source, l.level, l.kind, l.message,
		COALESCE(l.method,''), COALESCE(l.endpoint,''), COALESCE(l.http_status,0),
		COALESCE(l.duration_ms,0), COALESCE(l.bytes,0), COALESCE(l.error_body,'')
		FROM watcher_poll_log l`
	if len(where) > 0 {
		sqlStr += " WHERE " + strings.Join(where, " AND ")
	}
	sqlStr += " ORDER BY l.id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := conn.Query(sqlStr, args...)
	if err != nil {
		return nil, false, fmt.Errorf("query poll log: %w", err)
	}
	var out []PollLogEntry
	for rows.Next() {
		var e PollLogEntry
		if err := rows.Scan(&e.ID, &e.TS, &e.Source, &e.Level, &e.Kind, &e.Message,
			&e.Method, &e.Endpoint, &e.HTTPStatus, &e.DurationMS, &e.Bytes, &e.ErrorBody); err != nil {
			rows.Close()
			return nil, false, err
		}
		out = append(out, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	// Ascending queries for AfterID still return newest first (ORDER BY id DESC);
	// callers merge by id. Attach resource tags.
	for i := range out {
		rs, err := conn.Query(`SELECT resource_type, resource_id FROM watcher_poll_log_resources WHERE log_id = ? ORDER BY resource_type, resource_id`, out[i].ID)
		if err != nil {
			return nil, false, err
		}
		for rs.Next() {
			var r watcher.Resource
			if err := rs.Scan(&r.Type, &r.ID); err != nil {
				rs.Close()
				return nil, false, err
			}
			out[i].Resources = append(out[i].Resources, r)
		}
		rs.Close()
	}
	return out, more, nil
}

// PrunePollLog deletes rows older than maxAge, then the oldest rows beyond
// maxRows, along with their resource tags.
func PrunePollLog(conn *sql.DB, maxAge time.Duration, maxRows int) error {
	cutoff := time.Now().UTC().Add(-maxAge).Format(PollLogTSFormat)
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM watcher_poll_log_resources WHERE log_id IN (SELECT id FROM watcher_poll_log WHERE ts < ?)`, []any{cutoff}},
		{`DELETE FROM watcher_poll_log WHERE ts < ?`, []any{cutoff}},
		{`DELETE FROM watcher_poll_log_resources WHERE log_id <= (SELECT id FROM watcher_poll_log ORDER BY id DESC LIMIT 1 OFFSET ?)`, []any{maxRows}},
		{`DELETE FROM watcher_poll_log WHERE id <= (SELECT id FROM watcher_poll_log ORDER BY id DESC LIMIT 1 OFFSET ?)`, []any{maxRows}},
	}
	for _, s := range stmts {
		if _, err := conn.Exec(s.sql, s.args...); err != nil {
			return fmt.Errorf("prune poll log: %w", err)
		}
	}
	return nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
