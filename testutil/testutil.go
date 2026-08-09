//go:build golden

package testutil

import (
	"database/sql"
	"testing"

	"github.com/mturley/watcher/db"
	_ "modernc.org/sqlite"
)

// NewTestDB returns an in-memory SQLite DB with all watcher_* tables migrated.
func NewTestDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", "file::memory:?cache=shared&_pragma=busy_timeout(3000)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.Migrate(conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return conn
}
