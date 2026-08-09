package db

import (
	"database/sql"
	"fmt"
	"sort"
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

	if err := checkForCollisions(conn); err != nil {
		return err
	}

	if _, err := conn.Exec(schemaDDL); err != nil {
		return fmt.Errorf("watcher: creating schema: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := conn.Exec(`DELETE FROM watcher_schema_version`); err != nil {
		return fmt.Errorf("watcher: clearing schema version: %w", err)
	}
	if _, err := conn.Exec(
		`INSERT INTO watcher_schema_version (version, migrated_at) VALUES (?, ?)`,
		CurrentSchemaVersion, now,
	); err != nil {
		return fmt.Errorf("watcher: setting schema version: %w", err)
	}

	return nil
}

// SchemaVersion returns the schema version currently recorded in conn,
// or 0 if the database has not been migrated yet (the
// watcher_schema_version table is absent or empty).
func SchemaVersion(conn *sql.DB) (int, error) {
	exists, err := tableExists(conn, "watcher_schema_version")
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}

	var version int
	err = conn.QueryRow(`SELECT version FROM watcher_schema_version LIMIT 1`).Scan(&version)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("watcher: reading schema version: %w", err)
	}
	return version, nil
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
