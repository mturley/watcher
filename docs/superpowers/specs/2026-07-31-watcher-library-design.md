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

The library owns these tables, created via `db.Migrate(sqlDB)` inside each consumer's database.

`Migrate` is called on consumer startup, which for handler means it runs on every CLI invocation and on every statusline render (roughly every 10 seconds). It must therefore be a single `SELECT` against `watcher_schema_version` in the common case, issuing DDL only when the version differs. Read-only paths pass a flag that makes a version mismatch an error rather than a migration, so a hot render path never takes a write lock.

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
| expires_at | TEXT | Lease expiry (nullable = permanent). See Subscription Leases. |
| deleted_at | TEXT | Soft-delete timestamp (nullable) |

Indexes: `(resource_type, resource_id, deleted_at, expires_at)` to cover the `ActiveResources` lookup, and `subscriber` to cover lease renewal and revocation.

#### Subscription Leases

Some consumers have ephemeral subscribers. agent-handler subscribes on behalf of Claude Code sessions, which can die without a clean shutdown (crash, `kill -9`, terminal closed) and therefore without soft-deleting their subscriptions. Today handler avoids stale polling by joining `sessions` and filtering `status = 'active'` — a filter the library cannot reproduce, because the library has no concept of sessions and should not acquire one.

Leases make liveness a first-class library concern without leaking consumer semantics into the library:

- `expires_at IS NULL` means a permanent subscription (the worktree CLI's default — a worktree's resources matter until explicitly removed).
- A non-null `expires_at` means the subscriber must periodically renew, or the subscription stops being polled.

`ActiveResources` filters on `deleted_at IS NULL AND (expires_at IS NULL OR expires_at > now)`. Expired rows are not deleted — they remain queryable for history and can be revived by a renewal.

```go
func Subscribe(db *sql.DB, subscriber string, r Resource, opts SubscribeOpts) error

type SubscribeOpts struct {
    TTL time.Duration // zero means permanent (expires_at = NULL)
}

// Renew extends every live lease held by this subscriber.
func Renew(db *sql.DB, subscriber string, ttl time.Duration) error

// Revoke soft-deletes every subscription held by this subscriber.
func Revoke(db *sql.DB, subscriber string) error
```

**Consumer responsibility.** The lease is a backstop, not the primary mechanism. Consumers that already know when a subscriber dies should call `Revoke` promptly; the lease only bounds the damage when that signal is missed. For handler this means `Renew` on the existing session heartbeat and `Revoke` from both archive paths — `SessionEnd` and `handler cleanup`.

Note that these two paths disagree today: `SessionEnd` soft-deletes subscriptions, while `cleanup`'s `ArchiveSessions` only sets `status = 'archived'` and leaves subscriptions untouched. That inconsistency is invisible right now because the `JOIN sessions` filter excludes archived sessions either way. Once the join is gone, both paths must revoke explicitly.

**Handler's TTL: 5 days.** Sessions routinely survive a closed laptop over a weekend and resume afterward, so the TTL has to clear that comfortably.

A long TTL is safe here, for two reasons. Expiry cannot lose events, because change detection is cursor-based rather than diff-against-last-poll: a resource whose lease lapses and is later renewed emits everything that happened in the interim on the next poll. Expiry is also self-healing, because a resumed session re-registers, which flips it back to active and re-subscribes from `.worktree-resources`, reinstating the soft-deleted rows. The worst case is one delayed poll cycle if the watcher happens to run between the laptop waking and the user's first prompt.

A long TTL is also cheap. The only cost of a stale subscription is polling a resource nobody is watching, and the rate-limit headroom that made the library approach viable in the first place applies here too. The lease exists to stop subscriptions accumulating indefinitely over months, not to be precise about the hour a session died — that precision comes from `Revoke`.

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

### `watcher_poller_status`

Last success/error per poller, for health reporting.

| Column | Type | Description |
|--------|------|-------------|
| name | TEXT PK | Poller name |
| last_success | TEXT | ISO 8601 (nullable) |
| last_error | TEXT | ISO 8601 (nullable) |
| last_error_message | TEXT | Error details (nullable) |

Named `watcher_poller_status` rather than `watcher_status` deliberately: agent-handler already owns a `watcher_status` table with a similar shape. Reusing the name would make `CREATE TABLE IF NOT EXISTS` silently adopt handler's existing table, hiding any schema drift and leaving old and new binaries sharing one table across a rollback. Avoiding the collision outright is cheaper than detecting it.

### Table Name Collision Check

`Migrate()` fails loudly if it finds a `watcher_*` table it did not create and whose schema does not match its expectation, rather than adopting it. Consumers can already have tables in this namespace — handler does — and a future consumer may too. Silent adoption is the failure mode worth designing against.

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
- `config.GitHub()` / `config.Jira()` / `config.Slack()` — typed per-service accessors, each returning a populated struct or a "not configured" error
- `config.RegisterConsumer(name, dbPath)` — add a consumer to the registry
- `config.InteractiveAuth(service)` — interactive token setup flow

Accessors are typed per service rather than a single `ServiceToken(name) string`, because the services do not share a credential shape: GitHub needs one token, Jira needs host + email + token, Slack will need a bot token and possibly an app token. A lowest-common-denominator accessor would force every caller to reach around it.

Consumers embed the auth flow in their own setup commands (e.g. `handler watcher auth` calls `watcher.InteractiveAuth("github")`). If credentials already exist, setup is skipped.

**Permissions.** The file holds credentials. `Save` writes it `0600` (and its parent directory `0700`). `Load` refuses to read a config that is group- or world-readable, returning an error that names the file and the `chmod` needed to fix it.

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
    id: "slack:C0EXAMPLE2:1700000000.000005"
    url: https://redhat.slack.com/archives/C0EXAMPLE2/p1700000000000005
    label: "Review discussion thread"
```

Primary resources are what the worktree exists for. Related resources are watched for context. Both are subscribed to equally by consumers — the distinction is metadata.

The `label` field is optional, useful for resources with opaque IDs (like Slack threads).

#### Resource ID formats

Each resource type has a canonical `id` — a stable, normalized key that tools match on — and an accompanying `url` that is the human-facing link. Tools key on `id`; the `url` is for the person reading the file.

| Type | `id` format | Example |
|------|-------------|---------|
| `pr` | `owner/repo#number` | `mturley/odh-dashboard#7705` |
| `jira` | issue key | `RHOAIENG-12345` |
| `slack` | `slack:CHANNEL:THREAD_TS` | `slack:C0EXAMPLE2:1700000000.000005` |

The Slack ID is colon-delimited: the literal prefix `slack`, the channel ID (`C…`), and the thread's root timestamp (`conversations.replies`' `thread_ts`, the dotted `1700000000.000005` form). This matches the `channel:threadTs` identity that slack-mini already uses internally as its tab key, so slack-mini can open a thread directly from a `.worktree-resources.yaml` entry, and a future handler Slack watcher can subscribe to the same key.

Note that the Slack `id` and `url` encode the timestamp differently, by necessity: the `id` keeps the dotted `thread_ts` that the Slack API expects, while the permalink `url` uses Slack's `p<digits>` form with the dot removed. Tools derive one from the other; both are stored so neither has to be reconstructed.

**Unknown resource types are not an error.** The example above contains a `slack` entry while the Slack poller is out of scope for v1, and that is the normal case rather than a contrived one: these files are written by hand and by tools that may be newer than the poller set. `Load` returns unknown types unchanged, and pollers ignore resource types they do not handle. A consumer may subscribe to a resource nothing currently polls; it simply receives no events until a poller exists.

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

## Extraction Regression Suite

The library's value proposition is that a fix lands in one place and every consumer inherits it. The extraction itself is the moment that proposition is most at risk: handler's watcher code carries accumulated edge-case fixes that are easy to drop while adapting the code to new table names and signatures. One such fix landed days before this spec was written — batch-submitted GitHub reviews share a single `createdAt`, so timestamp-based dedup silently dropped every inline comment but the first.

Porting handler's existing watcher tests is therefore the **first** task of Phase 1, before any adaptation, and the following behaviors are explicit acceptance criteria with golden fixtures:

| Behavior | Why it breaks |
|----------|---------------|
| Batch-submitted review comments sharing one `createdAt` | Timestamp dedup collapses them; requires title-based dedup |
| CI bundle upsert across multiple poll cycles for one commit SHA | Naive append produces one event per check run |
| New-commit detection via SHA comparison against cached state | Not detectable from timestamps alone |
| First-poll `watch_started` suppression | Without it, subscribing to an old PR floods the inbox with its entire history |
| Jira ADF comment body extraction | Comment bodies are structured documents, not strings |
| Jira epic-link discovery → relationship row | Only automated writer of the relationships table |

Each fixture captures a recorded API response and the exact set of events the poller should emit from it.

## Test Utilities

The `testutil` package provides helpers for consumer test suites:

- `testutil.NewTestDB()` — creates an in-memory SQLite database with WAL mode and all `watcher_*` tables migrated
- `testutil.MockGitHubPoller(responses)` — returns a poller that returns canned API responses instead of hitting GitHub
- `testutil.MockJiraPoller(responses)` — same for Jira
- `testutil.SeedEvents(db, events)` — insert test events for query testing
- `testutil.SeedSubscriptions(db, subs)` — insert test subscriptions

## Agent-Handler Integration

### How Handler Uses the Library

Handler imports the library and runs the library's `Migrate(handlerDB)` on startup, creating `watcher_*` tables alongside handler's existing tables in `handler.db`. This runs in addition to handler's own `runMigrations()` hook; the two are kept disjoint (see Two Migration Systems below).

Handler's watcher commands (`handler watcher run github`, `handler watcher auth`, etc.) become thin wrappers around library calls:

```go
// handler watcher run github
gh, err := config.GitHub()
poller := github.NewPoller(handlerDB, gh)
poller.Poll()

// handler subscribe --resource "pr:owner/repo#42"
watcher.Subscribe(handlerDB, "handler:"+sessionID,
    watcher.Resource{Type: "pr", ID: "owner/repo#42", URL: url},
    watcher.SubscribeOpts{TTL: sessionLeaseTTL})
```

### Query Migration

An earlier draft of this spec estimated "~20 event queries rewritten to UNION ALL." That estimate is now obsolete, in handler's favor. A handler refactor (commits `397ab49`..`6366959`, landed after this spec's first draft) centralized the inbox routing predicate into a single file, `db/inbox_scope.go`. Every unread query — `UnreadForSession`, `UnreadCountForSession`, `UnreadResourcesForSession`, `HumanUnreadCountForSession`, `AutoDeliveredCount`, and the newer `UnreadEventsOfType` — now composes three shared fragments rather than hand-rolling its own joins:

- `inboxJoinSQL` — the `FROM events e` plus `LEFT JOIN`s to `event_recipients`, `event_resources`, and the session's live `subscriptions`
- `inboxWhereSQL` — the routing predicate, including the `s.id IS NOT NULL` branch that admits subscription-routed (watcher) events
- `inboxScopeArgs(session, cursor)` — the ordered argument slice

Because the watcher-relevant routing lives in exactly one place, the migration is localized to that file. The `event_resources` / `subscriptions` joins in the watcher-routed branch are redirected to `watcher_event_resources` / `watcher_subscriptions`, and the branch reads from `watcher_events`. Every caller inherits the change. `DirectCountForSession` stays separate by design (a narrower direct-recipient-only predicate that never sees watcher events) and needs no change.

The new definition of the shared fragments is the UNION of the two routing sources:

```sql
-- Composed inside inboxJoinSQL + inboxWhereSQL. Callers wrap this with their
-- own SELECT / GROUP BY / ORDER BY, exactly as they do today.
SELECT ... FROM (
  -- Agent events (recipient / broadcast / role routing)
  SELECT e.id, e.ts, e.type, e.title, e.body
  FROM events e
  LEFT JOIN event_recipients er ON e.id = er.event_id
  WHERE e.ts > :cursor
    AND e.type NOT IN ('watch_started', 'watcher_error')
    AND NOT EXISTS (SELECT 1 FROM dismissed_events d
                    WHERE d.session_id = :session_id AND d.event_id = e.id)
    AND (
      e.broadcast = 1
      OR (er.recipient_type = 'session' AND er.recipient_value = :session_id)
      OR (er.recipient_type = 'branch'  AND er.recipient_value IN (:branch, :repo_branch))
      OR (er.recipient_type = 'role'    AND er.recipient_value = :role)
    )

  UNION ALL

  -- Watcher events (subscription routing)
  SELECT we.id, we.ts, we.type, we.title, we.body
  FROM watcher_events we
  JOIN watcher_event_resources wer ON we.id = wer.event_id
  JOIN watcher_subscriptions ws ON ws.resource_type = wer.resource_type
    AND ws.resource_id = wer.resource_id
    AND ws.subscriber = :subscriber
    AND ws.deleted_at IS NULL
    AND (ws.expires_at IS NULL OR ws.expires_at > :now)
  WHERE we.ts > :cursor
    AND we.type NOT IN ('watch_started', 'watcher_error')
    AND NOT EXISTS (SELECT 1 FROM dismissed_events d
                    WHERE d.session_id = :session_id AND d.event_id = we.id)
) combined
```

The subscriber for the watcher half is `"handler:" + session_id`. A Go helper builds this UNION so the two halves' column lists stay aligned as handler's `events` table evolves.

### Dismissal Interaction

The same refactor added per-session explicit dismissal: a `dismissed_events(session_id, event_id, dismissed_at)` table, keyed `(session_id, event_id)`, that lets a user drop a single inbox event independently of the cursor. It is excluded via `dismissedExclusionSQL` — `NOT EXISTS (SELECT 1 FROM dismissed_events d WHERE d.session_id = ? AND d.event_id = e.id)`.

This table is handler-owned and stays that way — it is not a `watcher_*` table and the library knows nothing about it. But it interacts with the split in a way that is easy to get wrong: `dismissed_events` stores a bare `event_id` with no source or table discriminator. Before the split, every dismissible event lived in `events`, so the exclusion only ever matched there. After the split, a dismissed PR-comment event lives in `watcher_events`. **The dismissal exclusion must therefore be applied to both halves of the UNION** — the `events` half and the `watcher_events` half — as shown in the SQL above. Wiring it to only the `events` half is the natural mistake, and it silently resurrects dismissed watcher events.

This depends on one invariant: `event_id` values must be unique across `events` and `watcher_events`. Both tables use UUIDs, so this holds, but it is now a load-bearing requirement rather than an incidental fact — a dismissal keyed to an ID that existed in both tables would suppress the wrong event.

### Two Migration Systems, Kept Separate

The refactor also brought handler's own migration hook to life: `runMigrations()` in `db/db.go` now issues `CREATE TABLE IF NOT EXISTS dismissed_events` for databases created before that table existed. This is the first real use of that hook, and it is the established pattern for handler-owned schema evolution.

After integration, two migration systems run against `handler.db`:

- **Handler's `runMigrations()`** owns handler tables (`events`, `dismissed_events`, `subscriptions`, and the rest).
- **The library's `Migrate()`** owns `watcher_*` tables exclusively.

They must not overlap. The collision detection already specified for `Migrate()` — abort on any unexpected `watcher_*` table — is the guard on the library side. On the handler side, `runMigrations()` must never touch a `watcher_*` table; the library is the sole authority there. Handler calls both on startup: its own hook for its tables, the library's `Migrate()` for the watcher tables.

### Data Migration

A one-time migration command (`handler setup --migrate-watcher`) with the following runbook:

**Pre-migration:**
1. Back up the database: `cp ~/.agent-handler/handler.db ~/.agent-handler/handler.db.backup-YYYYMMDD`
2. Stop watchers: `handler watcher stop`
3. Verify current state: `handler health`, `handler watching`, note current unread counts

**Migration steps:**
1. Run `handler setup --migrate-watcher`
2. Creates `watcher_*` tables via `db.Migrate()`, which aborts if it finds an unexpected table in its namespace
3. Copies watcher events from `events` → `watcher_events`, filtered by `source IN ('github', 'jira')`
4. Copies `event_resources` rows for those events → `watcher_event_resources`
5. Copies `resource_state` → `watcher_resource_state`
6. Copies `subscriptions` → `watcher_subscriptions` (with `subscriber = "handler:" + session_id`), setting `expires_at` for sessions that are still active and soft-deleting the rest
7. Copies handler's `watcher_status` → `watcher_poller_status`, leaving the original in place for rollback
8. Copies `resource_relationships` → `watcher_resource_relationships`
9. Copies credentials from `~/.agent-handler/config.yaml` → `~/.config/watcher/config.yaml`
10. Sets the schema-version marker that switches handler's read path to the UNION ALL queries

The source filter must be an explicit allowlist of watcher sources, not a negation. Handler's `events` table currently holds four sources — `agent` (475 rows), `handler` (492), `github` (1816), `jira` (573) — and only the last two carry `event_resources`. Verified against the live database: the `agent`/`handler` and `github`/`jira` partitions have zero overlap, so the split is clean, but `handler` is easy to overlook when writing the filter.

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

During the transition period, handler supports both code paths, selected by the `watcher_schema_version` marker set in the final migration step:

- Marker present → UNION ALL queries
- Marker absent → legacy single-table queries

The predicate must never be "does `watcher_events` contain rows." Immediately after migrating a database with no watcher history, that table is legitimately empty, so a row-count check would send reads down the legacy path while the poller writes to `watcher_events` — new events would land in a table nothing reads. The failure is silent and presents as "the watcher stopped working."

This allows incremental development on a feature branch without breaking the installed production binary.

### Cursor Advance Correctness

Handler currently advances cursors to `time.Now()` formatted as RFC3339, which is second-granularity, rather than to the maximum `ts` of the events actually returned. Any event inserted between the read and the cursor write — or merely within the same wall-clock second — is skipped permanently, because the unread predicate is `e.ts > cursor`.

This is a pre-existing bug rather than something the split introduces, but the migration touches every one of these call sites, so it is cheap to fix here: advance to `max(ts)` over the returned event set, and leave the cursor untouched when the set is empty.

## Worktree CLI Integration

The worktree CLI (`github.com/mturley/worktree`) adopts the library for two things:

1. **`.worktree-resources.yaml` helpers**: Replace `internal/resources/` with `watcher/resources` package import
2. **Shared credentials**: Read Jira credentials from `~/.config/watcher/config.yaml` instead of `~/.config/worktree/config.yaml` (with fallback to the worktree-specific config for users who don't have the watcher library's config)

The worktree CLI does not run pollers or manage subscriptions in v1. It writes `.worktree-resources.yaml` files that handler (or future tools) read and act on.

## Known Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Extraction silently drops an accumulated edge-case fix | Port handler's watcher tests first; golden fixtures for the six behaviors listed in the regression suite. |
| Dead subscribers keep resources polled forever | `Revoke` on both handler archive paths is the primary mechanism; a 5-day lease bounds the cases that miss it. Over-polling is cheap; expiry cannot lose events. |
| Library adopts a consumer's same-named table | `watcher_poller_status` avoids the one known collision; `Migrate()` aborts on any unexpected `watcher_*` table. |
| Read path and write path disagree post-migration | Path selection keyed to `watcher_schema_version`, never to row counts. |
| Dismissed watcher events silently reappear | Apply `dismissedExclusionSQL` to both halves of the inbox UNION, not just the `events` half. Relies on `event_id` uniqueness across `events` and `watcher_events` (both UUIDs). |
| Handler and library migration hooks collide | `runMigrations()` owns handler tables only; `Migrate()` owns `watcher_*` only. Collision check aborts if either strays. |
| UNION ALL query performance | Negligible at handler's scale. Ensure matching indexes on `watcher_events`. |
| UNION ALL column alignment drift | Go helper function builds queries; compile-time guarantee of column match. |
| Schema versioning across consumers | `watcher_schema_version` table enables idempotent `Migrate()`. Consumers can report their version. |
| `Migrate()` on a hot path takes write locks | Version check is a single SELECT; read-only callers error rather than migrate. |
| Credentials in a shared config file | `0600` on write; `Load` refuses group- or world-readable files. |
| SQLite write contention | Already exists in handler today. Library documents WAL mode requirement. |
| Cursor clock skew between processes | Theoretically possible, practically irrelevant on a single machine. Same risk as today. |
| Handler data migration failure | Full backup + rollback procedure documented above. Backwards-compatible code paths during transition. |

## Scope

### In Scope (v1)

- GitHub PR poller (GraphQL, batched, all change detection)
- Jira issue poller (REST v3, changelog, comments, custom fields)
- `watcher_*` database schema and idempotent migrations, with namespace collision detection
- Schema version tracking
- Subscription management (subscribe, unsubscribe, soft-delete, leases, active resource queries)
- Resource state caching
- Resource relationship tracking (`watcher_resource_relationships`)
- Event emission with upsert support (CI bundling)
- Dedup framework (by external_ts, by title, composite)
- Extraction regression suite with golden fixtures
- Shared config file with typed accessors, credential management, and consumer DB registry
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
2. Port agent-handler's existing watcher tests and build the golden-fixture regression suite — before any adaptation, so the extraction has something to be measured against
3. Extract and adapt code from agent-handler's `watcher/` package
4. Implement `db` package with schema, migrations, collision detection, query helpers
5. Implement `config` package with typed accessors and permission enforcement
6. Implement `resources` package with YAML helpers
7. Implement `scheduler` package
8. Implement `testutil` package
9. Write tests against mock pollers and test DBs
10. Write comprehensive README documenting library purpose, installation, API, consumer integration, and configuration
11. Tag `v0.1.0` and push

### Phase 2: Worktree CLI Integration

1. Manually migrate existing `.worktree-resources` files to `.worktree-resources.yaml`
2. Update worktree CLI to import `watcher/resources`
3. Update worktree CLI to read Jira credentials from shared config (with fallback)
4. Tag a worktree CLI release

### Phase 3: Agent-Handler Integration

1. Create a handler worktree for the integration work
2. Add `github.com/mturley/watcher` dependency (use `replace` directive for local development)
3. Add the library's `Migrate()` call on startup alongside handler's existing `runMigrations()`, keeping the two hooks disjoint (handler tables vs `watcher_*`); read-only callers use the non-migrating variant
4. Rewrite watcher commands as thin wrappers around library calls
5. Wire lease management: `Renew` on session heartbeat with a 5-day TTL, `Revoke` on both archive paths (`SessionEnd` and `cleanup`, which currently disagree)
6. Redirect the watcher-routed branch of `db/inbox_scope.go` (join + where + args) to the `watcher_*` tables via the UNION, gated on the schema-version marker
7. Apply `dismissedExclusionSQL` to the watcher half of the UNION, not just the `events` half
8. Fix cursor advance to use `max(ts)` of returned events
9. Implement data migration command
10. Test thoroughly against a test database
11. Remove `replace` directive, pin to library version
12. Tag a handler release

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
