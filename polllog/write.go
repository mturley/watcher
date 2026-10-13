package polllog

import (
	"database/sql"
	"log"
	"time"

	"github.com/mturley/watcher"
	"github.com/mturley/watcher/db"
)

const (
	retention = 7 * 24 * time.Hour
	maxRows   = 50_000
)

// Write records a poll-lifecycle (no resources) or per-resource row.
func Write(conn *sql.DB, source, level, message string, rs ...watcher.Resource) error {
	kind := "poll"
	if len(rs) > 0 {
		kind = "resource"
	}
	_, err := db.InsertPollLog(conn, db.PollLogEntry{
		Source: source, Level: level, Kind: kind, Message: SanitizeMessage(message), Resources: rs,
	})
	return err
}

// Prune applies the watcher log's retention policy (7 days, 50k rows).
func Prune(conn *sql.DB, logger *log.Logger) {
	if logger == nil {
		logger = log.Default()
	}
	if err := db.PrunePollLog(conn, retention, maxRows); err != nil {
		logger.Printf("WARNING: %v", err)
	}
}
