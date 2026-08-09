package testutil

import (
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mturley/watcher/db"
	_ "modernc.org/sqlite"
)

// dbCounter gives each test DB a process-unique name so separate tests do
// not share the same shared-cache in-memory database.
var dbCounter atomic.Int64

// NewTestDB returns an in-memory SQLite DB with all watcher_* tables migrated.
// Each call gets an isolated database keyed by a unique name, so state does
// not leak between tests.
func NewTestDB(t *testing.T) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("%s_%d", strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()), dbCounter.Add(1))
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared&_pragma=busy_timeout(3000)", name)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.Migrate(conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return conn
}
