package db

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Migrate brings conn up to CurrentSchemaVersion. It is safe to call on
// every application startup: if the database is already migrated, it
// performs a single SELECT and returns. Otherwise it verifies that any
// pre-existing watcher_* tables match the schema this package manages,
// aborting with an error if not, then creates any missing tables/indexes
// and records the current schema version.
func Migrate(conn *sql.DB) error {
	version, err := SchemaVersion(conn)
	if err != nil {
		return fmt.Errorf("watcher: reading schema version: %w", err)
	}
	if version == CurrentSchemaVersion {
		return nil
	}

	if err := ensureAdditiveColumns(conn); err != nil {
		return err
	}

	if err := checkForCollisions(conn); err != nil {
		return err
	}

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("watcher: starting migration transaction: %w", err)
	}

	if _, err := tx.Exec(schemaDDL); err != nil {
		tx.Rollback()
		return fmt.Errorf("watcher: creating schema: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	// Backfill updated_at for rows that predate the column (added nullable by
	// ensureAdditiveColumns above). New rows always carry a timestamp.
	if _, err := tx.Exec(
		`UPDATE watcher_resource_meta SET updated_at = ? WHERE updated_at IS NULL OR updated_at = ''`,
		now,
	); err != nil {
		tx.Rollback()
		return fmt.Errorf("watcher: backfilling resource meta updated_at: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM watcher_schema_version`); err != nil {
		tx.Rollback()
		return fmt.Errorf("watcher: clearing schema version: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO watcher_schema_version (version, migrated_at) VALUES (?, ?)`,
		CurrentSchemaVersion, now,
	); err != nil {
		tx.Rollback()
		return fmt.Errorf("watcher: setting schema version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("watcher: committing migration: %w", err)
	}

	return nil
}

// SchemaVersion returns the schema version currently recorded in conn,
// or 0 if the database has not been migrated yet (the
// watcher_schema_version table is absent or empty).
func SchemaVersion(conn *sql.DB) (int, error) {
	var version int
	err := conn.QueryRow(`SELECT version FROM watcher_schema_version LIMIT 1`).Scan(&version)
	if err == nil {
		return version, nil
	}
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if strings.Contains(err.Error(), "no such table") {
		return 0, nil
	}
	return 0, fmt.Errorf("watcher: reading schema version: %w", err)
}

// additiveColumns lists columns that were added to a managed table's
// schema (both managedColumns and schemaDDL's CREATE TABLE) without a
// corresponding CurrentSchemaVersion bump at the time they were
// introduced — e.g. unsubscribed_by_user was added to
// watcher_subscriptions in this way. Because schemaDDL only ever issues
// CREATE TABLE IF NOT EXISTS (it cannot ALTER an existing table),
// databases that were already migrated before such a column existed
// have a narrower table than managedColumns now expects.
//
// ensureAdditiveColumns reconciles this gap with an explicit ALTER
// TABLE ADD COLUMN, and MUST run before checkForCollisions: otherwise a
// legitimately-historical (but now stale) table shape would be reported
// as an "unexpected schema" collision and Migrate would refuse to run,
// even though the only problem is a column this package itself knows
// how to add. Only columns listed here are auto-added; a table with any
// other mismatch is still rejected by checkForCollisions as before, so
// TestMigrateAbortsOnAlienWatcherTable-style protection against
// genuinely alien tables is unaffected.
var additiveColumns = map[string]map[string]string{
	"watcher_subscriptions": {
		"unsubscribed_by_user": "INTEGER NOT NULL DEFAULT 0",
	},
	// updated_at is nullable (SQLite ALTER ADD COLUMN cannot add a NOT NULL
	// column without a constant default, and we want the migration to
	// backfill existing rows with the migration timestamp instead). Reads
	// COALESCE NULL to "" (see GetResourceMeta), and Migrate backfills
	// pre-existing rows below.
	"watcher_resource_meta": {
		"updated_at": "TEXT",
	},
}

// ensureAdditiveColumns adds any column in additiveColumns that is
// missing from an existing managed table. It is a no-op for tables that
// don't exist yet (schemaDDL will create them with the column already
// present) and for columns that are already present.
func ensureAdditiveColumns(conn *sql.DB) error {
	for table, cols := range additiveColumns {
		exists, err := tableExists(conn, table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}

		actual, err := tableColumns(conn, table)
		if err != nil {
			return err
		}
		have := make(map[string]bool, len(actual))
		for _, c := range actual {
			have[c] = true
		}

		for col, def := range cols {
			if have[col] {
				continue
			}
			if _, err := conn.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, col, def)); err != nil {
				return fmt.Errorf("watcher: adding column %q to table %q: %w", col, table, err)
			}
		}
	}
	return nil
}

// checkForCollisions verifies that any managed table already present in
// conn has exactly the columns this package expects. It returns an error
// naming the first mismatched table it finds.
func checkForCollisions(conn *sql.DB) error {
	for _, table := range managedTables {
		exists, err := tableExists(conn, table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}

		actual, err := tableColumns(conn, table)
		if err != nil {
			return err
		}

		expected := append([]string(nil), managedColumns[table]...)
		sort.Strings(expected)
		sort.Strings(actual)

		if !equalStrings(expected, actual) {
			return fmt.Errorf(
				"watcher: table %q already exists with an unexpected schema (columns %v, want %v); refusing to migrate",
				table, actual, managedColumns[table],
			)
		}
	}
	return nil
}

func tableExists(conn *sql.DB, name string) (bool, error) {
	var count int
	err := conn.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("watcher: checking for table %q: %w", name, err)
	}
	return count > 0, nil
}

func tableColumns(conn *sql.DB, name string) ([]string, error) {
	rows, err := conn.Query(fmt.Sprintf(`PRAGMA table_info(%q)`, name))
	if err != nil {
		return nil, fmt.Errorf("watcher: inspecting table %q: %w", name, err)
	}
	defer rows.Close()

	var columns []string
	for rows.Next() {
		var (
			cid        int
			colName    string
			colType    string
			notNull    int
			dfltValue  sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &colName, &colType, &notNull, &dfltValue, &primaryKey); err != nil {
			return nil, fmt.Errorf("watcher: inspecting table %q: %w", name, err)
		}
		columns = append(columns, colName)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("watcher: inspecting table %q: %w", name, err)
	}
	return columns, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
