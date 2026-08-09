# Watcher Library (Phase 1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extract agent-handler's GitHub/Jira watcher subsystem into a standalone, reusable Go library (`github.com/mturley/watcher`) through a tagged `v0.1.0`, preserving every accumulated change-detection fix.

**Architecture:** The library owns polling (GitHub GraphQL, Jira REST), a self-contained `watcher_*` SQLite schema it migrates into a consumer's database, change detection (cursor + dedup + CI-bundle upsert), resource-state caching, subscriptions with leases, resource relationships, and per-poll first-poll behavior (clean start vs. backfill). It does **not** own read-state, a CLI, or a binary. Consumers pass a `*sql.DB` and call library functions.

**Tech Stack:** Go 1.25+, `modernc.org/sqlite` (pure-Go, no cgo), `github.com/google/uuid`, `gopkg.in/yaml.v3`, stdlib `net/http` and `database/sql`.

## Global Constraints

- Module path: `github.com/mturley/watcher`. Go version floor: `go 1.25.0` (matches source repo).
- SQLite driver: `modernc.org/sqlite` (pure Go). Open DSNs with `_pragma=busy_timeout(3000)` and WAL mode; the library documents that consumers should use WAL.
- The library takes a `*sql.DB` (Go stdlib), NOT agent-handler's `*db.DB` wrapper. All extracted code must be rewritten to use `*sql.DB` directly.
- All library-owned tables are prefixed `watcher_`. The poller status table is `watcher_poller_status` (NOT `watcher_status` — that name collides with a table agent-handler already owns).
- Canonical resource types are `pr`, `jira`, `slack` (spec "Resource ID formats"). The source repo's tests use `github_pr`; do NOT carry that over — standardize on `pr`.
- Timestamps are RFC3339 UTC strings, matching the source (`time.Now().UTC().Format(time.RFC3339)`).
- Preserve these accumulated fixes verbatim in behavior (each has a golden test in Task 2): title-based dedup for batch-submitted review comments (they share one `createdAt`); CI-bundle upsert per commit SHA; new-commit detection via SHA comparison against cached state; first-poll `watch_started` suppression when `Backfill: false`; Jira ADF comment body extraction; Jira epic-link → relationship row.
- No CLI, no `main` package, no web UI in this repo for Phase 1.
- Every event `id` is a UUID (`uuid.New().String()`).

---

## File Structure

```
github.com/mturley/watcher/
├── go.mod
├── watcher.go              # Resource, Event, Subscription types; package doc
├── eventtype.go            # EventType constants + DisplayName
├── db/
│   ├── schema.go           # embedded DDL for all watcher_* tables + indexes
│   ├── migrate.go          # Migrate(), version check, collision detection
│   ├── events.go           # InsertEvent, UpsertEvent, EventsForResource, EventsForSubscriberSince
│   ├── dedup.go            # EventCursor, IsDuplicate(DedupCheck)
│   ├── cibundle.go         # UpsertCIBundle
│   ├── resourcestate.go    # UpsertResourceState, GetResourceState, DeleteResourceState
│   ├── subscriptions.go    # Subscribe, Renew, Revoke, Unsubscribe, ActiveResources
│   ├── relationships.go    # LinkResources, RelationshipsFor
│   └── status.go           # RecordPollerSuccess, RecordPollerError, GetPollerStatus, HasPollerError
├── config/
│   └── config.go           # Load/Save, GitHub()/Jira()/Slack() accessors, 0600 perms, RegisterConsumer
├── github/
│   ├── graphql.go          # FetchPRs, FetchRemainingCheckContexts, PR types (verbatim extract)
│   └── poller.go           # Poll, processPR, state JSON, backfill
├── jira/
│   ├── client.go           # REST client, FetchIssue, ADF extraction (verbatim extract)
│   └── poller.go           # Poll, processIssue, changelog, backfill
├── scheduler/
│   └── scheduler.go        # launchd/cron generation (verbatim extract, deref db.DB)
├── testutil/
│   └── testutil.go         # NewTestDB, mock pollers, SeedEvents, SeedSubscriptions
└── README.md
```

Task order is deliberate: types → golden tests → db layer → config → pollers → scheduler → testutil → README → tag. Task 2 (porting tests) runs **before** the poller adaptation so the extraction is measured against known-good behavior.

---

### Task 1: Module init and core types

**Files:**
- Create: `go.mod`, `watcher.go`, `eventtype.go`
- Test: `eventtype_test.go`

**Interfaces:**
- Produces: `type Resource struct { Type, ID, URL string }`; `type Event struct {...}`; `type EventType string` with all constants; `func (EventType) DisplayName() string`.

- [ ] **Step 1: Initialize the module**

Run:
```bash
cd ~/git/watcher
go mod init github.com/mturley/watcher
go get github.com/google/uuid@v1.6.0
go get modernc.org/sqlite@latest
go get gopkg.in/yaml.v3@v3.0.1
```

- [ ] **Step 2: Write core types**

Create `watcher.go`:
```go
// Package watcher polls external resources (GitHub PRs, Jira issues) for
// changes and records them as durable events in a consumer-owned SQLite
// database. It owns events, resource state, subscriptions, and relationships;
// it does not own read-state, a CLI, or a binary.
package watcher

// Resource identifies an external resource. ID is the canonical key
// (see the spec's Resource ID formats); URL is the human-facing link.
type Resource struct {
	Type string
	ID   string
	URL  string
}

// Event is a change detected by a poller.
type Event struct {
	ID         string
	TS         string  // RFC3339 UTC, when the library recorded it
	ExternalTS *string // RFC3339 UTC from the source API; nil for some internal events
	Source     string  // "github", "jira", "slack"
	Type       EventType
	Title      string
	Body       *string
	Author     *string
	AuthorType *string
	Tags       *string // e.g. "commit:<sha>" for CI bundles
}
```

- [ ] **Step 3: Write event types**

Create `eventtype.go` — copy every constant from `~/git/agent-ledger/watcher/event_types.go` verbatim, including `EventTypeCICheckPassed`/`EventTypeCICheckFailed` (used internally by CI classification) and the `DisplayName()` method. Remove the handler-only entries from the display map (`status`, `message`, `reminder`, `blocked`, `unblocked`, `milestone`, `decision`, `followup`, `handoff`) — those are handler event types, not watcher types.

- [ ] **Step 4: Write the failing test**

Create `eventtype_test.go`:
```go
package watcher

import "testing"

func TestDisplayNameKnown(t *testing.T) {
	if got := EventTypePRComment.DisplayName(); got != "PR comments" {
		t.Errorf("got %q, want %q", got, "PR comments")
	}
}

func TestDisplayNameFallsBackToRaw(t *testing.T) {
	if got := EventType("nonexistent").DisplayName(); got != "nonexistent" {
		t.Errorf("got %q, want %q", got, "nonexistent")
	}
}
```

- [ ] **Step 5: Run tests**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum watcher.go eventtype.go eventtype_test.go
git commit -s -m "feat: module init, core types, event types"
```

---

### Task 2: Port poller golden-fixture tests (behavior baseline)

Port the existing poller tests **before** adapting the pollers, so the extraction has a known-good target. These tests currently exercise `processPR`/`processIssue` against a real test DB; they encode the accumulated fixes.

**Files:**
- Create: `github/poller_test.go` (from `~/git/agent-ledger/watcher/github/poller_test.go`)
- Create: `jira/poller_test.go` (from `~/git/agent-ledger/watcher/jira/poller_test.go`)
- Create: `testutil/testutil.go` (minimal `NewTestDB` needed by these tests; expanded in Task 9)

**Interfaces:**
- Consumes: `db.Migrate` (Task 4), the poller entry points (Tasks 7–8). Because those don't exist yet, this task writes the tests and leaves them failing-to-compile until their dependencies land; mark them with a build tag `//go:build golden` so `go test ./...` stays green in the interim, and remove the tag in Task 8.
- Produces: the golden fixtures the poller tasks must satisfy.

- [ ] **Step 1: Create minimal test DB helper**

Create `testutil/testutil.go`:
```go
package testutil

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
	"github.com/mturley/watcher/db"
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
```

- [ ] **Step 2: Port the GitHub poller test**

Copy `~/git/agent-ledger/watcher/github/poller_test.go` into `github/poller_test.go`. Apply these mechanical changes: add `//go:build golden` at the top; replace `github_pr` resource types with `pr`; replace `*db.DB` test helper usage with `testutil.NewTestDB`; replace `watcher.Resource{ResourceType:..., ResourceID:..., ResourceURL:...}` with `watcher.Resource{Type:..., ID:..., URL:...}`. Keep every assertion about emitted events unchanged — especially the batch-review-comment case (multiple comments, same timestamp, all emitted) and the CI-bundle case (one event per commit, updated in place).

- [ ] **Step 3: Port the Jira poller test**

Copy `~/git/agent-ledger/watcher/jira/poller_test.go` into `jira/poller_test.go` with the same mechanical changes. Keep the ADF-body-extraction and epic-link-relationship assertions unchanged.

- [ ] **Step 4: Verify tests compile under the golden tag but are skipped normally**

Run: `go test ./...`
Expected: PASS (golden-tagged tests are excluded)

Run: `go vet -tags golden ./...`
Expected: compile errors ONLY for not-yet-existing symbols (poller entry points, db funcs) — this is the target list for later tasks.

- [ ] **Step 5: Commit**

```bash
git add github/poller_test.go jira/poller_test.go testutil/testutil.go
git commit -s -m "test: port poller golden fixtures (build-tagged until deps land)"
```

---

### Task 3: Schema and migration with collision detection

**Files:**
- Create: `db/schema.go`, `db/migrate.go`
- Test: `db/migrate_test.go`

**Interfaces:**
- Produces:
  - `func Migrate(conn *sql.DB) error` — idempotent; creates all `watcher_*` tables; sets `watcher_schema_version`; aborts on an unexpected pre-existing `watcher_*` table.
  - `func SchemaVersion(conn *sql.DB) (int, error)` — returns 0 if unmigrated.
  - `const CurrentSchemaVersion = 1`

- [ ] **Step 1: Write the schema DDL**

Create `db/schema.go` with a `const schemaDDL` string containing `CREATE TABLE IF NOT EXISTS` for: `watcher_schema_version(version INTEGER, migrated_at TEXT)`; `watcher_events(id TEXT PRIMARY KEY, ts TEXT NOT NULL, external_ts TEXT, source TEXT NOT NULL, type TEXT NOT NULL, title TEXT NOT NULL, body TEXT, author TEXT, author_type TEXT, tags TEXT)`; `watcher_event_resources(event_id TEXT NOT NULL, resource_type TEXT NOT NULL, resource_id TEXT NOT NULL, resource_url TEXT)`; `watcher_resource_state(resource_type TEXT NOT NULL, resource_id TEXT NOT NULL, state_json TEXT NOT NULL, resource_updated_at TEXT NOT NULL, watcher_updated_at TEXT NOT NULL, PRIMARY KEY(resource_type, resource_id))`; `watcher_subscriptions(id TEXT PRIMARY KEY, subscriber TEXT NOT NULL, resource_type TEXT NOT NULL, resource_id TEXT NOT NULL, resource_url TEXT, created_at TEXT NOT NULL, expires_at TEXT, deleted_at TEXT)`; `watcher_resource_relationships(id TEXT PRIMARY KEY, child_type TEXT NOT NULL, child_id TEXT NOT NULL, child_url TEXT, parent_type TEXT NOT NULL, parent_id TEXT NOT NULL, parent_url TEXT, relationship TEXT NOT NULL, source TEXT NOT NULL, created_at TEXT NOT NULL)`; `watcher_poller_status(name TEXT PRIMARY KEY, last_success TEXT, last_error TEXT, last_error_message TEXT)`. Add indexes: `watcher_events(ts)`, `watcher_events(source, type)`, `watcher_event_resources(event_id)`, `watcher_event_resources(resource_type, resource_id)`, `watcher_subscriptions(resource_type, resource_id, deleted_at, expires_at)`, `watcher_subscriptions(subscriber)`.

Also define:
```go
const CurrentSchemaVersion = 1

// managedTables is the exact set of tables Migrate owns.
var managedTables = []string{
	"watcher_schema_version", "watcher_events", "watcher_event_resources",
	"watcher_resource_state", "watcher_subscriptions",
	"watcher_resource_relationships", "watcher_poller_status",
}
```

- [ ] **Step 2: Write the failing migration test**

Create `db/migrate_test.go`:
```go
package db

import (
	"database/sql"
	"testing"
	_ "modernc.org/sqlite"
)

func mem(t *testing.T) *sql.DB {
	t.Helper()
	c, err := sql.Open("sqlite", "file::memory:?cache=shared&_pragma=busy_timeout(3000)")
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { c.Close() })
	return c
}

func TestMigrateSetsVersion(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil { t.Fatal(err) }
	v, err := SchemaVersion(c)
	if err != nil { t.Fatal(err) }
	if v != CurrentSchemaVersion { t.Errorf("version %d, want %d", v, CurrentSchemaVersion) }
}

func TestMigrateIsIdempotent(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil { t.Fatal(err) }
	if err := Migrate(c); err != nil { t.Fatalf("second migrate: %v", err) }
}

func TestMigrateAbortsOnAlienWatcherTable(t *testing.T) {
	c := mem(t)
	if _, err := c.Exec(`CREATE TABLE watcher_events (wrong INTEGER)`); err != nil { t.Fatal(err) }
	if err := Migrate(c); err == nil {
		t.Fatal("expected Migrate to abort on a pre-existing watcher_events with unexpected schema")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./db/ -run TestMigrate -v`
Expected: FAIL (undefined `Migrate`/`SchemaVersion`)

- [ ] **Step 4: Implement migrate.go**

Create `db/migrate.go`. `Migrate` should: (1) read `SchemaVersion`; if it equals `CurrentSchemaVersion`, return nil (fast path — single SELECT); (2) run a collision check — for each `watcher_*` table that already exists (query `sqlite_master`), verify its columns match the managed schema, else return an error naming the table; (3) exec `schemaDDL`; (4) upsert `watcher_schema_version` to `CurrentSchemaVersion` with `migrated_at = now`. `SchemaVersion` returns 0 when the `watcher_schema_version` table is absent or empty.

For the collision check on an existing table, compare `PRAGMA table_info(<name>)` column names against the expected set; the test's `watcher_events (wrong INTEGER)` must fail this.

- [ ] **Step 5: Run tests**

Run: `go test ./db/ -run TestMigrate -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add db/schema.go db/migrate.go db/migrate_test.go
git commit -s -m "feat(db): watcher_* schema, idempotent migrate, collision detection"
```

---

### Task 4: Event insert and dedup

**Files:**
- Create: `db/events.go`, `db/dedup.go`
- Test: `db/events_test.go`

**Interfaces:**
- Consumes: `Migrate` (Task 3); `watcher.Event`, `watcher.Resource`, `watcher.EventType` (Task 1).
- Produces:
  - `func InsertEvent(conn *sql.DB, e watcher.Event, r watcher.Resource) error` — inserts into `watcher_events` + `watcher_event_resources` in one transaction.
  - `func EventCursor(conn *sql.DB, source, resourceType, resourceID string) (string, error)` — MAX(external_ts) or "".
  - `type DedupCheck struct { Source, ResourceType, ResourceID string; Type watcher.EventType; ExternalTS *string; Title *string }`
  - `func IsDuplicate(conn *sql.DB, c DedupCheck) (bool, error)` — matches on ExternalTS when set, on Title when set.

- [ ] **Step 1: Write the failing test**

Create `db/events_test.go` covering: insert then `EventCursor` returns the `external_ts`; `IsDuplicate` by `ExternalTS` true for same ts/type/resource, false for different ts, different type, different resource; `IsDuplicate` by `Title` true for same title even when timestamps would differ (the batch-review-comment fix). Use `watcher.Resource{Type:"pr", ID:"owner/repo#1"}`.

```go
package db

import (
	"testing"
	"github.com/mturley/watcher"
)

func TestEventCursorAndDedup(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil { t.Fatal(err) }
	ts := "2026-01-15T12:00:00Z"
	ev := watcher.Event{ID: "e1", TS: ts, ExternalTS: &ts, Source: "github", Type: watcher.EventTypePRComment, Title: "Comment by alice"}
	res := watcher.Resource{Type: "pr", ID: "owner/repo#1"}
	if err := InsertEvent(c, ev, res); err != nil { t.Fatal(err) }

	cur, err := EventCursor(c, "github", "pr", "owner/repo#1")
	if err != nil { t.Fatal(err) }
	if cur != ts { t.Errorf("cursor %q, want %q", cur, ts) }

	dup, _ := IsDuplicate(c, DedupCheck{Source: "github", ResourceType: "pr", ResourceID: "owner/repo#1", Type: watcher.EventTypePRComment, ExternalTS: &ts})
	if !dup { t.Error("expected ts duplicate") }

	title := "Comment by alice"
	byTitle, _ := IsDuplicate(c, DedupCheck{Source: "github", ResourceType: "pr", ResourceID: "owner/repo#1", Type: watcher.EventTypePRComment, Title: &title})
	if !byTitle { t.Error("expected title duplicate") }
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./db/ -run TestEventCursorAndDedup -v`
Expected: FAIL (undefined symbols)

- [ ] **Step 3: Implement events.go and dedup.go**

`InsertEvent` adapts `EmitWatcherEvent`'s body (framework.go:104) to `*sql.DB` + a transaction: `BEGIN`, insert into `watcher_events`, insert one `watcher_event_resources` row, `COMMIT`. `EventCursor` adapts framework.go:55 against `watcher_events`/`watcher_event_resources`. `IsDuplicate` merges `IsDuplicate` + `IsDuplicateByTitle` (framework.go:73, 89): if `c.ExternalTS != nil` match on `external_ts`; if `c.Title != nil` match on `title`; exactly one must be set.

- [ ] **Step 4: Run tests**

Run: `go test ./db/ -run TestEventCursorAndDedup -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add db/events.go db/dedup.go db/events_test.go
git commit -s -m "feat(db): event insert, cursor, unified dedup"
```

---

### Task 5: CI-bundle upsert, resource state, relationships, poller status

**Files:**
- Create: `db/cibundle.go`, `db/resourcestate.go`, `db/relationships.go`, `db/status.go`
- Test: `db/cibundle_test.go`, `db/resourcestate_test.go`

**Interfaces:**
- Produces:
  - `func UpsertCIBundle(conn *sql.DB, commitSHA string, t watcher.EventType, title, body, externalTS string, r watcher.Resource) error`
  - `func UpsertResourceState(conn *sql.DB, resourceType, resourceID, stateJSON, resourceUpdatedAt, watcherUpdatedAt string) error`
  - `func GetResourceState(conn *sql.DB, resourceType, resourceID string) (*ResourceState, error)` with `type ResourceState struct { ResourceType, ResourceID, StateJSON, ResourceUpdatedAt, WatcherUpdatedAt string }`
  - `func LinkResources(conn *sql.DB, child, parent watcher.Resource, relationship, source string) error`
  - `func RecordPollerSuccess(conn *sql.DB, name string) error`, `RecordPollerError(conn, name, msg)`, `GetPollerStatus(conn, name) (*PollerStatus, error)`, `HasPollerError(conn, name) bool`

- [ ] **Step 1: Write failing CI-bundle test**

`db/cibundle_test.go`: call `UpsertCIBundle` for `commitSHA "abc"` with `EventTypeCIPending`; assert one row in `watcher_events`. Call again same SHA with `EventTypeCIPassed`; assert STILL one row, now `type='ci_passed'` (the in-place update, from framework.go:138). Call with a different SHA; assert two rows.

- [ ] **Step 2: Run to verify fail**

Run: `go test ./db/ -run TestUpsertCIBundle -v`
Expected: FAIL

- [ ] **Step 3: Implement the four files**

Adapt `UpsertCIBundle` (framework.go:138), `resource_state.go` functions (verbatim, deref to `*sql.DB`), and `watcher_status.go` functions (rename table→`watcher_poller_status`, type→`PollerStatus`, funcs→`RecordPollerSuccess`/`RecordPollerError`/`GetPollerStatus`/`HasPollerError`). `LinkResources` inserts a `watcher_resource_relationships` row with a `uuid` id; make it idempotent (skip if an identical child/parent/relationship row exists).

- [ ] **Step 4: Write and run resource-state test**

`db/resourcestate_test.go`: upsert state, get it back, assert `state_json` matches; upsert again with new json, assert overwrite. Run: `go test ./db/ -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add db/cibundle.go db/resourcestate.go db/relationships.go db/status.go db/cibundle_test.go db/resourcestate_test.go
git commit -s -m "feat(db): CI-bundle upsert, resource state, relationships, poller status"
```

---

### Task 6: Subscriptions with leases, and the two consumer read queries

**Files:**
- Create: `db/subscriptions.go`
- Test: `db/subscriptions_test.go`

**Interfaces:**
- Produces:
  - `func Subscribe(conn *sql.DB, subscriber string, r watcher.Resource, opts SubscribeOpts) error` with `type SubscribeOpts struct { TTL time.Duration; Backfill bool }` — reinstates a soft-deleted row; sets `expires_at = now+TTL` when `TTL>0`, else NULL.
  - `func Renew(conn *sql.DB, subscriber string, ttl time.Duration) error` — extends `expires_at` on all live rows for the subscriber.
  - `func Revoke(conn *sql.DB, subscriber string) error` — soft-deletes all the subscriber's rows.
  - `func Unsubscribe(conn *sql.DB, subscriber string, r watcher.Resource) error`
  - `func ActiveResources(conn *sql.DB, resourceType string) ([]watcher.Resource, error)` — `deleted_at IS NULL AND (expires_at IS NULL OR expires_at > now)`, DISTINCT by resource.
  - `func EventsForResource(conn *sql.DB, resourceType, resourceID string) ([]watcher.Event, error)` — all events for a resource, ordered by ts.
  - `func EventsForSubscriberSince(conn *sql.DB, subscriber, since string) ([]watcher.Event, error)` — events routed to the subscriber's live subscriptions with `ts > since`.

Note: `Backfill` is stored on the subscription so the poller can read it on first poll (Task 7). Add `backfill` — reuse an existing column? No: add nothing to schema; instead the poller decides backfill by checking whether the subscription's first poll has run. Simpler: store `Backfill` intent by having the poller treat "no cursor yet" + a per-subscription flag. To keep schema v1 stable, add a `backfill INTEGER NOT NULL DEFAULT 0` column to `watcher_subscriptions` in Task 3's DDL. **Action:** go back and add that column to `db/schema.go` now (it is part of v1), and set it in `Subscribe`.

- [ ] **Step 1: Add the `backfill` column to schema**

Modify `db/schema.go`: add `backfill INTEGER NOT NULL DEFAULT 0` to `watcher_subscriptions`. (Still schema v1 — nothing has shipped.)

- [ ] **Step 2: Write the failing test**

`db/subscriptions_test.go`: Subscribe with `TTL: time.Hour`; `ActiveResources("pr")` returns it. Subscribe another with an already-past expiry (insert directly, or Subscribe then manually age it); `ActiveResources` excludes it. `Revoke(subscriber)`; `ActiveResources` empty. Re-`Subscribe` same resource; row reinstated (count of rows for that resource stays 1). Insert two events for a resource, assert `EventsForResource` returns both ordered; assert `EventsForSubscriberSince` returns only events after `since`.

- [ ] **Step 3: Run to verify fail**

Run: `go test ./db/ -run TestSubscription -v`
Expected: FAIL

- [ ] **Step 4: Implement subscriptions.go**

Adapt from `~/git/agent-ledger/db/subscriptions.go` and framework.go's `ActiveResources`, but keyed on `subscriber` (opaque string) instead of `session_id`, with the lease predicate. `EventsForResource`/`EventsForSubscriberSince` join `watcher_events`→`watcher_event_resources`(→`watcher_subscriptions` for the subscriber variant), excluding `watch_started`/`watcher_error`. Compute `now` and `expires_at` as RFC3339 strings so string comparison in SQL is valid.

- [ ] **Step 5: Run tests**

Run: `go test ./db/ -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add db/schema.go db/subscriptions.go db/subscriptions_test.go
git commit -s -m "feat(db): subscriptions with leases, backfill flag, consumer read queries"
```

---

### Task 7: GitHub poller (fetch layer + processing)

**Files:**
- Create: `github/graphql.go` (verbatim extract), `github/poller.go`
- Test: relies on `github/poller_test.go` from Task 2

**Interfaces:**
- Consumes: all of `db/*` (Tasks 3–6); `config.GitHub()` (Task 9 — but the poller takes a token string, not the config, to stay decoupled).
- Produces:
  - `func Poll(conn *sql.DB, token string, resources []watcher.Resource, logger *log.Logger) error`
  - `func processPR(conn *sql.DB, prData PRData, r watcher.Resource, token string, backfill bool, logger *log.Logger) (int, error)`

- [ ] **Step 1: Extract graphql.go verbatim**

Copy `~/git/agent-ledger/watcher/github/graphql.go` to `github/graphql.go`. It has no `db.DB` or `config` dependency (pure HTTP + types), so only the package clause and any agent-handler imports change. Keep `PRData`, `PRRef`, `Review`, `CheckRun`, `CommitEntry`, `FetchPRs`, `FetchRemainingCheckContexts`, `ParsePRResourceID` exactly.

- [ ] **Step 2: Adapt poller.go**

Copy `~/git/agent-ledger/watcher/github/poller.go` to `github/poller.go` and rewrite: `*db.DB` → `*sql.DB`; `watcher.EmitWatcherEvent(...)` → `db.InsertEvent(conn, watcher.Event{...}, r)`; `watcher.IsDuplicate(...)`/`IsDuplicateByTitle(...)` → `db.IsDuplicate(conn, db.DedupCheck{...})`; `watcher.UpsertCIBundle` → `db.UpsertCIBundle`; `watcher.EventCursor` → `db.EventCursor`; `d.GetResourceState`/`d.UpsertResourceState`/`d.RecordWatcherSuccess`/`d.RecordWatcherError` → the `db.*` equivalents; `watcher.EmitWatcherError` → a local helper that calls `db.InsertEvent` with `EventTypeWatcherError` + the `HasPollerError` dedup guard. Resource type is `"pr"`. Add the `backfill bool` param to `processPR`: when `cursor == ""`, if `backfill` fetch+emit history (see Step 3), else emit `watch_started` and return (existing behavior at poller.go:101).

- [ ] **Step 3: Implement backfill branch**

In `processPR`, when `cursor == "" && backfill`: instead of the early `watch_started` return, fall through to the normal review/comment/reviewComment/CI/commit processing with `cursor = ""` so everything is treated as new. The existing loops already emit all items when `cursor` is empty (their `item <= cursor` guards are false for any real timestamp vs `""`). The CI block already handles `cursor == ""` (poller.go:199). This means backfill is mostly "skip the early return"; verify the golden GitHub test's backfill case passes.

- [ ] **Step 4: Un-tag the GitHub golden test and run**

Remove `//go:build golden` from `github/poller_test.go`. Run: `go test ./github/ -v`
Expected: PASS (all ported assertions, including batch-review-comment and CI-bundle)

- [ ] **Step 5: Commit**

```bash
git add github/graphql.go github/poller.go github/poller_test.go
git commit -s -m "feat(github): poller extracted to library, backfill support"
```

---

### Task 8: Jira poller (client + processing)

**Files:**
- Create: `jira/client.go` (verbatim extract), `jira/poller.go`
- Test: relies on `jira/poller_test.go` from Task 2

**Interfaces:**
- Produces:
  - `func Poll(conn *sql.DB, cfg JiraAuth, resources []watcher.Resource, logger *log.Logger) error` with `type JiraAuth struct { URL, Email, Token string; CustomFields map[string]string; BotUsernames []string }`
  - `func processIssue(conn *sql.DB, issue IssueData, r watcher.Resource, backfill bool, logger *log.Logger) (int, error)`

- [ ] **Step 1: Extract client.go verbatim**

Copy `~/git/agent-ledger/watcher/jira/client.go` to `jira/client.go`, changing only package/imports. Preserve the ADF body-extraction logic and the `FetchIssue` signature/behavior. **Fix while extracting:** if `FetchIssue` uses `expand=changelog`, switch it to the paginated `/issue/{key}/changelog` endpoint (spec: `expand=changelog` silently caps at 100). If it already paginates correctly, leave it. Note the change in the commit message.

- [ ] **Step 2: Adapt poller.go**

Copy `~/git/agent-ledger/watcher/jira/poller.go` to `jira/poller.go` with the same `*db.DB`→`*sql.DB` and `watcher.*`→`db.*` rewrites as Task 7. Resource type is `"jira"`. Preserve epic-link discovery → `db.LinkResources(conn, child, parent, "epic", "jira")` (was `resource_relationships` insert at poller.go ~277). Add the `backfill bool` param with the same "skip the early `watch_started` return" semantics.

- [ ] **Step 3: Un-tag the Jira golden test and run**

Remove `//go:build golden` from `jira/poller_test.go`. Run: `go test ./jira/ -v`
Expected: PASS (including ADF extraction and epic-link relationship assertions)

- [ ] **Step 4: Run the whole suite**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add jira/client.go jira/poller.go jira/poller_test.go
git commit -s -m "feat(jira): poller extracted to library, changelog pagination, backfill"
```

---

### Task 9: Config package (typed accessors, 0600 perms, consumer registry) and testutil expansion

**Files:**
- Create: `config/config.go`
- Test: `config/config_test.go`
- Modify: `testutil/testutil.go` (add mock pollers + seed helpers)

**Interfaces:**
- Produces:
  - `func Load(path string) (*Config, error)` — refuses group/world-readable files.
  - `func (c *Config) Save(path string) error` — writes `0600`, parent dir `0700`.
  - `func (c *Config) GitHub() (GitHubCreds, error)` / `Jira() (JiraCreds, error)` / `Slack() (SlackCreds, error)` — each returns creds or a "not configured" error.
  - `func (c *Config) RegisterConsumer(name, dbPath string)` and `Consumers() map[string]string`
  - `func DefaultPath() string` → `~/.config/watcher/config.yaml` (honor `WATCHER_HOME` for tests).
- Produces in testutil: `MockGitHubPoller`, `MockJiraPoller`, `SeedEvents(conn, []watcher.Event, watcher.Resource)`, `SeedSubscriptions(conn, subscriber, []watcher.Resource)`.

- [ ] **Step 1: Write the failing config test**

`config/config_test.go`: Save a config with GitHub token to a temp path; stat it and assert mode `0600`; `Load` it back and assert `GitHub()` returns the token. `Jira()` on a config without Jira returns an error. Write a file `0644` and assert `Load` refuses it with an error mentioning `chmod`.

- [ ] **Step 2: Run to verify fail**

Run: `go test ./config/ -v`
Expected: FAIL

- [ ] **Step 3: Implement config.go**

Model the YAML on the spec's Shared Configuration section: `services.github.token`, `services.jira.{host,email,token}`, `services.slack.token`, `jira_custom_fields`, `consumers.<name>.db`. Base the struct on `~/git/agent-ledger/config/config.go` but note Jira uses `host` in the new spec (source uses `URL`); use `Host` in the YAML tag `host`. `Load`: `os.Stat`, reject if `mode & 0077 != 0`. `Save`: `os.MkdirAll(dir, 0700)` then `os.WriteFile(path, data, 0600)`. Accessors return typed structs or `fmt.Errorf("github not configured in %s", path)`.

- [ ] **Step 4: Run config tests**

Run: `go test ./config/ -v`
Expected: PASS

- [ ] **Step 5: Expand testutil**

Add `SeedEvents`, `SeedSubscriptions`, and `MockGitHubPoller`/`MockJiraPoller` (structs returning canned `PRData`/`IssueData` so consumers can test without network). Add a small test `testutil/testutil_test.go` that seeds two events and asserts `db.EventsForResource` returns them.

- [ ] **Step 6: Commit**

```bash
git add config/config.go config/config_test.go testutil/testutil.go testutil/testutil_test.go
git commit -s -m "feat(config): typed accessors, 0600 perms, consumer registry; expand testutil"
```

---

### Task 10: Scheduler

**Files:**
- Create: `scheduler/scheduler.go`
- Test: `scheduler/scheduler_test.go`

**Interfaces:**
- Produces: `func Install(cfg ScheduleConfig) error`, `Uninstall`, `Stop`, `Start`, `IsInstalled`, `IsRunning` — with `type ScheduleConfig struct { Name, Command string; Interval time.Duration }`. Generates launchd plist (macOS) / cron (Linux).

- [ ] **Step 1: Extract scheduler.go**

Copy `~/git/agent-ledger/watcher/scheduler.go` to `scheduler/scheduler.go`. It generates OS scheduler config and shells out; it has no `db.DB` dependency. Change: it must invoke the **consumer's** command (passed in `ScheduleConfig.Command`), not a hardcoded `handler watcher run`. Parameterize the command and label prefix.

- [ ] **Step 2: Write a plist/cron generation test**

`scheduler/scheduler_test.go`: call the internal plist/cron string builder with a known `ScheduleConfig` and assert the output contains the command and the interval in seconds. Do NOT actually install to the OS in the test — test the string generation only (refactor the builder into a pure function if needed).

- [ ] **Step 3: Run**

Run: `go test ./scheduler/ -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add scheduler/scheduler.go scheduler/scheduler_test.go
git commit -s -m "feat(scheduler): OS scheduler generation, consumer-command parameterized"
```

---

### Task 11: README, full-suite green, tag v0.1.0

**Files:**
- Create/replace: `README.md`

- [ ] **Step 1: Write the README**

Replace the stub `README.md`. Cover: what the library is (event+state store + pollers; not a ledger's read-state, not a CLI); install (`go get github.com/mturley/watcher@v0.1.0`); quickstart (open `*sql.DB`, `db.Migrate`, `Subscribe`, run a poller); the consumer integration model (own your DB, own your read-state, WAL mode); config file location and shape; the resource ID format table (`pr`/`jira`/`slack`); a note that Slack polling is not yet implemented. Link to the design spec.

- [ ] **Step 2: Full suite + vet + build**

Run:
```bash
go build ./...
go vet ./...
go test ./...
```
Expected: all PASS, no vet warnings.

- [ ] **Step 3: Commit and tag**

```bash
git add README.md
git commit -s -m "docs: comprehensive README for v0.1.0"
git tag v0.1.0
git push origin main
git push origin v0.1.0
```

- [ ] **Step 4: Verify the module is fetchable**

Run (in a temp dir outside the repo):
```bash
cd $(mktemp -d) && go mod init probe && GOFLAGS=-mod=mod go get github.com/mturley/watcher@v0.1.0 && echo OK
```
Expected: `OK`

---

## Self-Review

**Spec coverage:**
- GitHub poller → Task 7. Jira poller → Task 8. Schema + migrations + collision → Task 3. Schema version → Task 3. Subscriptions + leases + active queries → Task 6. First-poll backfill/clean-start → Tasks 6 (flag) + 7/8 (behavior). Per-resource / per-subscriber queries → Task 6. Resource state cache → Task 5. Relationships → Task 5. Upsert/CI bundling → Task 5. Dedup framework → Task 4. Extraction regression suite → Task 2 (+ un-tagged in 7/8). Config typed accessors + 0600 → Task 9. Consumer DB registry → Task 9. Scheduler → Task 10. Testutil → Tasks 2 (min) + 9 (full). README → Task 11. Tag v0.1.0 → Task 11.
- Deliberately excluded (per spec Scope): `.worktree-resources` YAML package, Slack poller, CLI/UI, handler and worktree integrations. `slack` appears only as a resource-ID format in the README.
- `watcher_poller_status` rename and collision check both covered (Task 3, Global Constraints).

**Placeholder scan:** No TBD/TODO. Every code step shows code or an exact adaptation instruction against a named source file+line. Verbatim-extract tasks (graphql.go, client.go, scheduler.go, resource_state.go) name the source path and the specific rewrites.

**Type consistency:** `watcher.Resource{Type,ID,URL}` used consistently (renamed from source's `ResourceType/ResourceID/ResourceURL`). `db.IsDuplicate(DedupCheck)` used in Tasks 7/8 as defined in Task 4. `db.UpsertCIBundle` signature matches between Task 5 and Task 7. `PollerStatus`/`RecordPollerSuccess` naming consistent (Task 5, referenced in 7/8). The `backfill` column is added to schema in Task 6 Step 1 with a note that it's still v1 (caught during writing — Task 3's DDL and Task 6 must agree; Task 6 Step 1 explicitly amends schema.go).

**One flagged risk for the executor:** the source `github/graphql.go` (618 lines) and `jira/client.go` (272 lines) are extracted verbatim but not independently unit-tested in this plan — their behavior is covered transitively by the ported golden poller tests (Task 2). If FetchPRs/FetchIssue need direct tests, add them, but the golden tests are the contract.
