# Watcher Library Design Spec

A reusable Go library for polling external resources (GitHub PRs, Jira issues, Slack threads) and detecting changes. Extracted from agent-handler's watcher subsystem to serve multiple independent tools without duplicating polling logic.

## Motivation

agent-handler includes a watcher subsystem that polls GitHub and Jira for changes to subscribed resources. Other tools (a worktree UI, a Jira epic visualizer, a Slack thread viewer) need the same capability. Rather than duplicating polling logic across projects, we extract it into a shared Go library.

**Why a library and not a centralized daemon:** Rate limits are not a concern (GitHub: ~6% of budget with batched queries polling 20 PRs every 3 minutes; Jira: ~1.3% polling 20 issues every 5 minutes — even with 3 independent consumers). The strongest argument for centralization was deduplicating API calls, and that argument doesn't hold. A library gives each tool independence without coordination overhead.

**Why not expand agent-handler's scope:** These tools should work independently of Claude Code. A developer who doesn't use Claude Code should be able to install a worktree UI or Jira visualizer on its own.

## Architecture

### The Library: `github.com/mturley/watcher`

```
github.com/mturley/watcher/
├── config/        # Shared config file (~/.config/watcher/config.yaml)
├── db/            # Schema definitions, migrations, query helpers for watcher_* tables
├── github/        # GitHub GraphQL poller
├── jira/          # Jira REST v3 poller
├── slack/         # Slack poller (future, not in v1)
├── resources/     # .worktree-resources.yaml read/write helpers
├── scheduler/     # launchd/cron plist generation
├── testutil/      # Mock pollers, in-memory test DB helpers
└── watcher.go     # Top-level types (Event, Resource, Subscription) and framework
```

### Consumer Integration Model

Each consuming project (handler, worktree CLI, future tools) imports the library as a Go module dependency. The library creates its own `watcher_*` tables inside the consumer's SQLite database. Consumers own their database, their subscription lifecycle, and their notification model.

```
┌─────────────────────┐     ┌─────────────────────┐
│   agent-handler     │     │   worktree CLI       │
│                     │     │                      │
│ ┌─────────────────┐ │     │ ┌──────────────────┐ │
│ │ handler.db      │ │     │ │ worktree.db      │ │
│ │                 │ │     │ │                  │ │
│ │ events          │ │     │ │ watcher_events   │ │
│ │ sessions        │ │     │ │ watcher_*        │ │
│ │ watcher_events  │ │     │ │ (worktree tables)│ │
│ │ watcher_*       │ │     │ │                  │ │
│ └─────────────────┘ │     │ └──────────────────┘ │
│         │           │     │         │            │
│    imports          │     │    imports           │
└────────┬────────────┘     └────────┬─────────────┘
         │                           │
         └─────────┬─────────────────┘
                   │
         ┌─────────▼─────────┐
         │ watcher library   │
         │ (Go module)       │
         │                   │
         │ Pollers, schema,  │
         │ dedup, config,    │
         │ resources helpers │
         └───────────────────┘
```

## Database Schema

The library owns these tables, created via `db.Migrate(sqlDB)` inside each consumer's database:

### `watcher_schema_version`

Tracks the library's schema version for idempotent migrations.

| Column | Type | Description |
|--------|------|-------------|
| version | INTEGER | Current schema version number |
| migrated_at | TEXT | ISO 8601 timestamp of last migration |

### `watcher_events`

Change events detected by pollers.

| Column | Type | Description |
|--------|------|-------------|
| id | TEXT PK | UUID |
| ts | TEXT | Internal timestamp (when the library recorded it) |
| external_ts | TEXT | Timestamp from the source API |
| source | TEXT | Poller name: "github", "jira", "slack" |
| type | TEXT | Event type (see Event Types below) |
| title | TEXT | Human-readable summary |
| body | TEXT | Detailed content (nullable) |
| author | TEXT | Who caused the change (nullable) |
| author_type | TEXT | "user", "bot", "system" (nullable) |
| tags | TEXT | Comma-separated tags, used for upsert matching (e.g. "commit:abc123") |

Indexes: `ts`, `(source, type)`.

### `watcher_event_resources`

Links events to the resources they describe.

| Column | Type | Description |
|--------|------|-------------|
| event_id | TEXT FK | References watcher_events.id |
| resource_type | TEXT | "pr", "jira", "slack" |
| resource_id | TEXT | Resource identifier |
| resource_url | TEXT | URL to the resource (nullable) |

Indexes: `event_id`, `(resource_type, resource_id)`.

### `watcher_resource_state`

Cached current state of each polled resource.

| Column | Type | Description |
|--------|------|-------------|
| resource_type | TEXT | PK part 1 |
| resource_id | TEXT | PK part 2 |
| state_json | TEXT | JSON blob of current state |
| resource_updated_at | TEXT | Last change time from the source API |
| watcher_updated_at | TEXT | Last poll time |

### `watcher_subscriptions`

Which subscribers care about which resources. Subscribers are opaque strings namespaced by consumer (e.g. `"handler:session-abc"`, `"worktree:odh-dashboard/fix-login"`).

| Column | Type | Description |
|--------|------|-------------|
| id | TEXT PK | UUID |
| subscriber | TEXT | Opaque subscriber identifier |
| resource_type | TEXT | "pr", "jira", "slack" |
| resource_id | TEXT | Resource identifier |
| resource_url | TEXT | URL (nullable) |
| created_at | TEXT | ISO 8601 |
| deleted_at | TEXT | Soft-delete timestamp (nullable) |

Indexes: `(resource_type, resource_id, deleted_at)`.

### `watcher_resource_relationships`

Parent-child links between resources (e.g. Jira epic-to-story).

| Column | Type | Description |
|--------|------|-------------|
| id | TEXT PK | UUID |
| child_type | TEXT | Resource type of the child |
| child_id | TEXT | Resource ID of the child |
| child_url | TEXT | URL (nullable) |
| parent_type | TEXT | Resource type of the parent |
| parent_id | TEXT | Resource ID of the parent |
| parent_url | TEXT | URL (nullable) |
| relationship | TEXT | Relationship type (e.g. "epic", "parent", "blocks") |
| source | TEXT | What discovered this relationship |
| created_at | TEXT | ISO 8601 |

### `watcher_status`

Last success/error per poller, for health reporting.

| Column | Type | Description |
|--------|------|-------------|
| name | TEXT PK | Poller name |
| last_success | TEXT | ISO 8601 (nullable) |
| last_error | TEXT | ISO 8601 (nullable) |
| last_error_message | TEXT | Error details (nullable) |

## Event Types

### GitHub

| Type | Trigger |
|------|---------|
| `pr_comment` | New issue comment on a PR |
| `pr_review_comment` | New inline review comment |
| `pr_review_requested` | Review requested |
| `pr_approved` | PR approved |
| `pr_closed` | PR closed without merge |
| `pr_merged` | PR merged |
| `pr_reopened` | PR reopened |
| `pr_new_commits` | New commits pushed (detected via SHA comparison in resource state) |
| `ci_passed` | All CI checks passed (upserted per commit SHA) |
| `ci_failed` | One or more CI checks failed (upserted per commit SHA) |
| `ci_pending` | CI checks still running (upserted per commit SHA) |
| `ci_partial_failure` | Some checks passed, some failed (upserted per commit SHA) |

### Jira

| Type | Trigger |
|------|---------|
| `jira_comment` | New comment on an issue |
| `jira_status_change` | Status transition |
| `jira_assigned` | Assignee changed |
| `jira_description_changed` | Description updated |
| `jira_labels_changed` | Labels added or removed |

### Internal

| Type | Trigger |
|------|---------|
| `watch_started` | First poll for a newly subscribed resource |
| `watcher_error` | Poller encountered an error |

## Polling Flow

1. Consumer calls `poller.Poll(db)` (or schedules it via `scheduler`)
2. Poller calls `db.ActiveResources(sqlDB, resourceType)` to find resources with active (non-deleted) subscriptions
3. Poller fetches from the external API (GitHub GraphQL batch query, Jira REST per-issue)
4. Poller calls `db.EventCursor(...)` to find the latest `external_ts` for this resource
5. For each change newer than the cursor, poller calls `db.IsDuplicate(...)` with the appropriate match strategy (by timestamp, by title, or composite)
6. New events are written via `db.EmitEvent(...)` or `db.UpsertEvent(...)` (for CI bundling)
7. Resource state is cached via `db.UpsertResourceState(...)`
8. Resource relationships discovered during polling (e.g. Jira epic links) are written via `db.LinkResources(...)`

### Deduplication

The library supports multiple dedup strategies because different event sources have different uniqueness semantics:

- **By external timestamp**: Default. Works for most events where each has a unique timestamp.
- **By title**: For events that share timestamps (e.g. batch-submitted GitHub review comments that all have the same `createdAt` but unique file-path-based titles).
- **By composite key**: For future use where neither alone is sufficient.

```go
type DedupCheck struct {
    Source       string
    ResourceType string
    ResourceID   string
    Type         string
    ExternalTS   *time.Time  // match by timestamp
    Title        *string     // match by title
}

func IsDuplicate(db *sql.DB, check DedupCheck) (bool, error)
```

### Event Upsert

CI check events are bundled per commit: the first check completion creates an event tagged `commit:<sha>`, and subsequent completions for the same commit update that event in place rather than creating new ones. This prevents inbox noise from 20+ individual check-run events.

```go
func UpsertEvent(db *sql.DB, match UpsertMatch, event Event) error

type UpsertMatch struct {
    Source       string
    ResourceType string
    ResourceID   string
    Types        []string  // any of these types match
    Tags         string    // exact tag match (e.g. "commit:abc123")
}
```

## Shared Configuration

The library provides a shared config file at `~/.config/watcher/config.yaml`:

```yaml
services:
  github:
    token: ghp_...
  jira:
    host: redhat.atlassian.net
    email: mturley@redhat.com
    token: ...
  slack:
    token: xoxb-...  # future

# Jira custom field IDs (instance-specific)
jira_custom_fields:
  blocked: customfield_10517
  blocked_reason: customfield_10483
  epic_key: customfield_10014
  flagged: customfield_10021
  story_points: customfield_10028
  git_pull_request: customfield_10875

# Consumer DB registry (for cross-system discovery, read-only)
consumers:
  handler:
    db: ~/.agent-handler/handler.db
  worktree:
    db: ~/.config/worktree/worktree.db
```

The library provides:
- `config.Load()` / `config.Save()` — read/write the config file
- `config.ServiceToken(service)` — get credentials, clear error if not configured
- `config.RegisterConsumer(name, dbPath)` — add a consumer to the registry
- `config.InteractiveAuth(service)` — interactive token setup flow

Consumers embed the auth flow in their own setup commands (e.g. `handler watcher auth` calls `watcher.InteractiveAuth("github")`). If credentials already exist, setup is skipped.

## `.worktree-resources.yaml` Format

The library provides a `resources` package for reading and writing `.worktree-resources.yaml` files — the convention for declaring which external resources matter in a git worktree.

```yaml
primary:
  - type: pr
    id: "mturley/odh-dashboard#7705"
    url: https://github.com/mturley/odh-dashboard/pull/7705
  - type: jira
    id: RHOAIENG-12345
    url: https://redhat.atlassian.net/browse/RHOAIENG-12345

related:
  - type: pr
    id: "mturley/odh-dashboard#7700"
    url: https://github.com/mturley/odh-dashboard/pull/7700
  - type: slack
    id: "C0123ABC/p1234567890"
    url: https://redhat.slack.com/archives/C0123ABC/p1234567890
    label: "Review discussion thread"
```

Primary resources are what the worktree exists for. Related resources are watched for context. Both are subscribed to equally by consumers — the distinction is metadata.

The `label` field is optional, useful for resources with opaque IDs (like Slack threads).

API:
- `resources.Load(path) ([]Resource, error)` — reads YAML
- `resources.Save(path, []Resource) error` — writes YAML
- `resources.Add(path, Resource) error` — appends with dedup and primary-demotion logic
- `resources.Remove(path, resourceType, resourceID) error` — removes an entry

## Scheduler Helpers

The library provides helpers for setting up periodic polling via the OS scheduler:

- **macOS**: Generates launchd plist files at `~/Library/LaunchAgents/`
- **Linux**: Generates cron entries

Consumers specify which pollers to run and at what interval. The scheduler helpers generate the appropriate configuration that invokes the consumer's own binary (not a library binary — the library has no binary).

Example: handler would schedule `handler watcher run github` every 3 minutes, just as it does today. The worktree UI might schedule `worktree poll github` every 5 minutes. Each runs independently.

## Test Utilities

The `testutil` package provides helpers for consumer test suites:

- `testutil.NewTestDB()` — creates an in-memory SQLite database with WAL mode and all `watcher_*` tables migrated
- `testutil.MockGitHubPoller(responses)` — returns a poller that returns canned API responses instead of hitting GitHub
- `testutil.MockJiraPoller(responses)` — same for Jira
- `testutil.SeedEvents(db, events)` — insert test events for query testing
- `testutil.SeedSubscriptions(db, subs)` — insert test subscriptions

## Agent-Handler Integration

### How Handler Uses the Library

Handler imports the library and runs `db.Migrate(handlerDB)` on startup, creating `watcher_*` tables alongside handler's existing tables in `handler.db`.

Handler's watcher commands (`handler watcher run github`, `handler watcher auth`, etc.) become thin wrappers around library calls:

```go
// handler watcher run github
poller := github.NewPoller(handlerDB, config.ServiceToken("github"))
poller.Poll()

// handler subscribe --resource "pr:owner/repo#42"
watcher.Subscribe(handlerDB, "handler:"+sessionID, "pr", "owner/repo#42", url)
```

### Query Migration

Handler's ~20 event queries that currently read from a single `events` table are rewritten to UNION ALL with `watcher_events`. The pattern:

```sql
SELECT id, ts, type, title, body FROM (
  -- Agent events (recipients/broadcast routing)
  SELECT e.id, e.ts, e.type, e.title, e.body
  FROM events e
  LEFT JOIN event_recipients er ON e.id = er.event_id
  WHERE e.ts > :cursor AND (
    e.broadcast = 1
    OR (er.recipient_type = 'session' AND er.recipient_value = :session_id)
    OR (er.recipient_type = 'branch' AND er.recipient_value = :branch)
  )

  UNION ALL

  -- Watcher events (subscription routing)
  SELECT we.id, we.ts, we.type, we.title, we.body
  FROM watcher_events we
  JOIN watcher_event_resources wer ON we.id = wer.event_id
  JOIN watcher_subscriptions ws ON ws.resource_type = wer.resource_type
    AND ws.resource_id = wer.resource_id
  WHERE ws.subscriber = :subscriber AND we.ts > :cursor
    AND we.type NOT IN ('watch_started', 'watcher_error')
) combined
ORDER BY ts DESC
```

A Go helper function builds these UNION ALL queries to avoid misaligned column lists when handler's events table changes.

### Data Migration

A one-time migration command (`handler setup --migrate-watcher`) with the following runbook:

**Pre-migration:**
1. Back up the database: `cp ~/.agent-handler/handler.db ~/.agent-handler/handler.db.backup-YYYYMMDD`
2. Stop watchers: `handler watcher stop`
3. Verify current state: `handler health`, `handler watching`, note current unread counts

**Migration steps:**
1. Run `handler setup --migrate-watcher`
2. This creates `watcher_*` tables via `db.Migrate()`
3. Copies watcher events from `events` → `watcher_events` (filtered by `source IN ('github', 'jira')`)
4. Copies `event_resources` rows for those events → `watcher_event_resources`
5. Copies `resource_state` → `watcher_resource_state`
6. Copies `subscriptions` → `watcher_subscriptions` (with `subscriber = "handler:" + session_id`)
7. Copies `watcher_status` → `watcher_status` (same table name, library-owned now)
8. Copies `resource_relationships` → `watcher_resource_relationships`
9. Copies credentials from `~/.agent-handler/config.yaml` → `~/.config/watcher/config.yaml`

**Post-migration verification:**
1. `handler health` — check database health
2. `handler watching` — verify subscriptions are intact
3. `handler status` — verify unread counts match pre-migration values
4. `handler log --global` — verify timeline includes both agent and watcher events
5. Start watchers: `handler watcher start`
6. Wait for one poll cycle, check that new events appear

**Rollback:**
1. Stop watchers: `handler watcher stop`
2. Restore backup: `cp ~/.agent-handler/handler.db.backup-YYYYMMDD ~/.agent-handler/handler.db`
3. Reinstall previous handler binary: `cd agent-handler && git checkout <previous-commit> && make build && make install`
4. Start watchers: `handler watcher start`

**Cleanup (after confirming migration is stable):**
1. Remove migrated watcher events from the `events` table (optional, reduces DB size)
2. Remove old `event_resources` rows for migrated events
3. Remove old `subscriptions`, `resource_state`, `watcher_status` tables (once handler's code no longer references them)
4. Remove credentials from `~/.agent-handler/config.yaml`

### Backwards Compatibility During Development

During the transition period, handler supports both code paths:
- If `watcher_events` table exists and has data → use UNION ALL queries
- If not → use legacy single-table queries

This allows incremental development on a feature branch without breaking the installed production binary.

## Worktree CLI Integration

The worktree CLI (`github.com/mturley/worktree`) adopts the library for two things:

1. **`.worktree-resources.yaml` helpers**: Replace `internal/resources/` with `watcher/resources` package import
2. **Shared credentials**: Read Jira credentials from `~/.config/watcher/config.yaml` instead of `~/.config/worktree/config.yaml` (with fallback to the worktree-specific config for users who don't have the watcher library's config)

The worktree CLI does not run pollers or manage subscriptions in v1. It writes `.worktree-resources.yaml` files that handler (or future tools) read and act on.

## Known Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| UNION ALL query performance | Negligible at handler's scale. Ensure matching indexes on `watcher_events`. |
| UNION ALL column alignment drift | Go helper function builds queries; compile-time guarantee of column match. |
| Schema versioning across consumers | `watcher_schema_version` table enables idempotent `Migrate()`. Consumers can report their version. |
| SQLite write contention | Already exists in handler today. Library documents WAL mode requirement. |
| Cursor clock skew between processes | Theoretically possible, practically irrelevant on a single machine. Same risk as today. |
| Handler data migration failure | Full backup + rollback procedure documented above. Backwards-compatible code paths during transition. |

## Scope

### In Scope (v1)

- GitHub PR poller (GraphQL, batched, all change detection)
- Jira issue poller (REST v3, changelog, comments, custom fields)
- `watcher_*` database schema and idempotent migrations
- Schema version tracking
- Subscription management (subscribe, unsubscribe, soft-delete, active resource queries)
- Resource state caching
- Resource relationship tracking (`watcher_resource_relationships`)
- Event emission with upsert support (CI bundling)
- Dedup framework (by external_ts, by title, composite)
- Shared config file with credential management and consumer DB registry
- `.worktree-resources.yaml` read/write helpers
- Scheduler helpers (launchd/cron)
- Test utilities (mock pollers, test DB, seed helpers)

### Out of Scope (future work)

- Slack poller
- Cross-system discovery CLI
- Any CLI or web UI in the watcher project itself
- Agent-handler integration (separate effort after library is stable)
- Worktree CLI integration (separate effort)

## Release and Integration Roadmap

### Phase 1: Build the Library

1. Initialize `github.com/mturley/watcher` Go module
2. Extract and adapt code from agent-handler's `watcher/` package
3. Implement `db` package with schema, migrations, query helpers
4. Implement `config` package with shared config file
5. Implement `resources` package with YAML helpers
6. Implement `scheduler` package
7. Implement `testutil` package
8. Write tests against mock pollers and test DBs
9. Tag `v0.1.0` and push

### Phase 2: Worktree CLI Integration

1. Manually migrate existing `.worktree-resources` files to `.worktree-resources.yaml`
2. Update worktree CLI to import `watcher/resources`
3. Update worktree CLI to read Jira credentials from shared config (with fallback)
4. Tag a worktree CLI release

### Phase 3: Agent-Handler Integration

1. Create a handler worktree for the integration work
2. Add `github.com/mturley/watcher` dependency (use `replace` directive for local development)
3. Add `db.Migrate()` call on startup
4. Rewrite watcher commands as thin wrappers around library calls
5. Rewrite event queries to use UNION ALL (with backwards-compatible fallback)
6. Implement data migration command
7. Test thoroughly against a test database
8. Remove `replace` directive, pin to library version
9. Tag a handler release

### Phase 4: Production Migration

1. Back up handler database
2. Stop watchers
3. Install new handler binary
4. Run `handler setup --migrate-watcher`
5. Verify (health, watching, status, log, unread counts)
6. Start watchers
7. Monitor for one day
8. Clean up old tables (optional)

### Version Tagging Convention

- Library: `v0.x.y` until API is stable across both consumers, then `v1.0.0`
- During development: consumers use `replace` directives for local library checkout
- For releases: consumers pin to tagged library versions (`go get github.com/mturley/watcher@v0.1.0`)
- Bump library minor version for new features, patch for bugfixes
- Consumers update library dependency and re-test before releasing
