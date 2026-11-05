package db

import (
	"testing"

	"github.com/mturley/watcher"
)

func TestUpsertCIBundle(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}

	res := watcher.Resource{Type: "pr", ID: "owner/repo#1"}

	if err := UpsertCIBundle(c, "abc", watcher.EventTypeCIPending, "CI running", "", "2026-01-15T12:00:00Z", res, CICheckBundleTypes); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := c.QueryRow(`SELECT COUNT(*) FROM watcher_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}

	if err := UpsertCIBundle(c, "abc", watcher.EventTypeCIPassed, "CI passed", "", "2026-01-15T12:05:00Z", res, CICheckBundleTypes); err != nil {
		t.Fatal(err)
	}

	if err := c.QueryRow(`SELECT COUNT(*) FROM watcher_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count after in-place update = %d, want 1", count)
	}

	var eventType string
	if err := c.QueryRow(`SELECT type FROM watcher_events LIMIT 1`).Scan(&eventType); err != nil {
		t.Fatal(err)
	}
	if eventType != "ci_passed" {
		t.Errorf("type = %q, want %q", eventType, "ci_passed")
	}

	if err := UpsertCIBundle(c, "def", watcher.EventTypeCIPending, "CI running", "", "2026-01-15T12:10:00Z", res, CICheckBundleTypes); err != nil {
		t.Fatal(err)
	}

	if err := c.QueryRow(`SELECT COUNT(*) FROM watcher_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("count after new SHA = %d, want 2", count)
	}
}

// TestUpsertCIBundle_TwoBundleKindsSameCommit verifies the CheckRun and
// StatusContext bundle families are distinct identities under the same commit
// tag: upserting both for one SHA yields two rows, and each family upserts in
// place on repeat rather than colliding with the other.
func TestUpsertCIBundle_TwoBundleKindsSameCommit(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	res := watcher.Resource{Type: "pr", ID: "owner/repo#1"}

	if err := UpsertCIBundle(c, "abc", watcher.EventTypeCIPending, "CI running", "", "2026-01-15T12:00:00Z", res, CICheckBundleTypes); err != nil {
		t.Fatal(err)
	}
	if err := UpsertCIBundle(c, "abc", watcher.EventTypeCIWorkflowsPending, "Workflows running", "", "2026-01-15T12:00:00Z", res, CIWorkflowBundleTypes); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := c.QueryRow(`SELECT COUNT(*) FROM watcher_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("count with both bundle kinds = %d, want 2 (no collision)", count)
	}

	// Each family upserts its own row in place; the other is untouched.
	if err := UpsertCIBundle(c, "abc", watcher.EventTypeCIPassed, "CI checks passed", "", "2026-01-15T12:05:00Z", res, CICheckBundleTypes); err != nil {
		t.Fatal(err)
	}
	if err := UpsertCIBundle(c, "abc", watcher.EventTypeCIWorkflowsPassed, "Workflows passed", "", "2026-01-15T12:06:00Z", res, CIWorkflowBundleTypes); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(`SELECT COUNT(*) FROM watcher_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("count after in-place updates = %d, want 2", count)
	}

	var checkType, wfType string
	if err := c.QueryRow(`SELECT type FROM watcher_events WHERE type LIKE 'ci_passed'`).Scan(&checkType); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(`SELECT type FROM watcher_events WHERE type LIKE 'ci_workflows_passed'`).Scan(&wfType); err != nil {
		t.Fatal(err)
	}
	if checkType != "ci_passed" || wfType != "ci_workflows_passed" {
		t.Errorf("types = %q / %q, want ci_passed / ci_workflows_passed", checkType, wfType)
	}
}

// TestMarkCIBundlesOutOfDate verifies the out-of-date marker prefixes both
// bundle families' titles exactly once, leaves ts/external_ts untouched, is
// idempotent, and doesn't touch a different commit's bundles.
func TestMarkCIBundlesOutOfDate(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	res := watcher.Resource{Type: "pr", ID: "owner/repo#1"}

	if err := UpsertCIBundle(c, "old", watcher.EventTypeCIPassed, "CI checks passed", "", "2026-01-15T12:00:00Z", res, CICheckBundleTypes); err != nil {
		t.Fatal(err)
	}
	if err := UpsertCIBundle(c, "old", watcher.EventTypeCIWorkflowsPending, "Workflows running", "", "2026-01-15T12:00:00Z", res, CIWorkflowBundleTypes); err != nil {
		t.Fatal(err)
	}
	if err := UpsertCIBundle(c, "new", watcher.EventTypeCIPending, "CI running", "", "2026-01-15T12:10:00Z", res, CICheckBundleTypes); err != nil {
		t.Fatal(err)
	}

	// Capture ts/external_ts of an old bundle before marking.
	var tsBefore, extBefore string
	if err := c.QueryRow(`SELECT ts, COALESCE(external_ts,'') FROM watcher_events WHERE type='ci_passed'`).Scan(&tsBefore, &extBefore); err != nil {
		t.Fatal(err)
	}

	if err := MarkCIBundlesOutOfDate(c, "old", res); err != nil {
		t.Fatal(err)
	}

	titles := map[string]string{}
	rows, err := c.Query(`SELECT type, title FROM watcher_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var typ, title string
		if err := rows.Scan(&typ, &title); err != nil {
			t.Fatal(err)
		}
		titles[typ] = title
	}

	if got := titles["ci_passed"]; got != "[out of date] CI checks passed" {
		t.Errorf("ci_passed title = %q, want prefixed", got)
	}
	if got := titles["ci_workflows_pending"]; got != "[out of date] Workflows running" {
		t.Errorf("ci_workflows_pending title = %q, want prefixed", got)
	}
	if got := titles["ci_pending"]; got != "CI running" {
		t.Errorf("different-commit bundle title = %q, want untouched", got)
	}

	// ts/external_ts must be unchanged (no re-notify).
	var tsAfter, extAfter string
	if err := c.QueryRow(`SELECT ts, COALESCE(external_ts,'') FROM watcher_events WHERE type='ci_passed'`).Scan(&tsAfter, &extAfter); err != nil {
		t.Fatal(err)
	}
	if tsAfter != tsBefore || extAfter != extBefore {
		t.Errorf("timestamps changed: ts %q->%q, external_ts %q->%q; must be untouched", tsBefore, tsAfter, extBefore, extAfter)
	}

	// Idempotent: second call must not double-prefix.
	if err := MarkCIBundlesOutOfDate(c, "old", res); err != nil {
		t.Fatal(err)
	}
	var title string
	if err := c.QueryRow(`SELECT title FROM watcher_events WHERE type='ci_passed'`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "[out of date] CI checks passed" {
		t.Errorf("after second mark, title = %q, want no double prefix", title)
	}
}

// TestUpsertCIBundle_SameTypeKeepsTS verifies that re-upserting a bundle whose
// type is unchanged (e.g. a bot re-running a check on an already-passing
// commit) refreshes its content but does not move ts, so the event is not
// bumped to "now" and made unread again. A type change still moves ts.
func TestUpsertCIBundle_SameTypeKeepsTS(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	res := watcher.Resource{Type: "pr", ID: "owner/repo#1"}

	if err := UpsertCIBundle(c, "abc", watcher.EventTypeCIPassed, "CI checks passed (2/2)", "", "2026-01-15T12:00:00Z", res, CICheckBundleTypes); err != nil {
		t.Fatal(err)
	}
	const oldTS = "2000-01-01T00:00:00Z"
	if _, err := c.Exec(`UPDATE watcher_events SET ts = ?`, oldTS); err != nil {
		t.Fatal(err)
	}

	if err := UpsertCIBundle(c, "abc", watcher.EventTypeCIPassed, "CI checks passed (3/3)", "b", "2026-01-15T13:00:00Z", res, CICheckBundleTypes); err != nil {
		t.Fatal(err)
	}
	var ts, title, extTS string
	if err := c.QueryRow(`SELECT ts, title, external_ts FROM watcher_events`).Scan(&ts, &title, &extTS); err != nil {
		t.Fatal(err)
	}
	if ts != oldTS {
		t.Errorf("same-type upsert moved ts to %q, want it kept at %q", ts, oldTS)
	}
	if title != "CI checks passed (3/3)" || extTS != "2026-01-15T13:00:00Z" {
		t.Errorf("same-type upsert did not refresh content: title=%q external_ts=%q", title, extTS)
	}

	if err := UpsertCIBundle(c, "abc", watcher.EventTypeCIFailed, "CI failed", "", "2026-01-15T14:00:00Z", res, CICheckBundleTypes); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(`SELECT ts FROM watcher_events`).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	if ts == oldTS {
		t.Errorf("type change did not move ts")
	}
}
