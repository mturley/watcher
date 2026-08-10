# Phase 2 Design Spec — watcher v0.2 + agent-handler Integration

Integrate the `github.com/mturley/watcher` library into agent-handler, replacing handler's in-tree `watcher/` subsystem AND its `subscriptions` table. Split into three planned phases: a library growth (v0.2), the handler integration + production data migration, and a gated cleanup.

This spec builds on and refines the Phase 1 design (`2026-07-31-watcher-library-design.md`), whose "Agent-Handler Integration" roadmap sections it supersedes where they conflict — informed by a full surface map of handler's current (post-inbox_scope-refactor) code.

## Motivation

Phase 1 built and shipped the watcher library (v0.1.0). Phase 2 makes agent-handler consume it, deleting handler's duplicated poller/framework code and unifying subscription storage. A surface map revealed handler's `subscriptions` table is far more entangled than the Phase 1 roadmap assumed (SubscribeIfNew, Reinstate, RestoreSubscriptionsForSession, `unsubscribed_by`, branch/session soft-deletes, ~8 read callers). Rather than keep two subscription tables in sync, we generalize the library's subscription lifecycle to absorb handler's semantics as consumer-agnostic concepts, then replace handler's table outright.

**No v0.1 backward-compatibility.** Nothing external consumes the library yet, so v0.2 may change schema and API freely with no upgrade path.

## Phase Structure

- **Phase 2a — watcher v0.2** (library only; nothing touches handler). Tagged `v0.2.0`.
- **Phase 2b — handler integration + data migration** (old tables kept as backup; legacy query path retained behind a gate).
- **Phase 2c — cleanup** (delete legacy code/tables/query-path; delete migrated rows from `events`). Gated on a soak period.

Each phase gets its own implementation plan. Phase 2a completes and tags before 2b begins (2b consumes v0.2 via a local `replace` directive during dev, swapping to the tag once integration is proven). 2c does not begin until its entry gate is satisfied.

## Dependency / Dev Workflow

- During 2b development, handler depends on the library via a `go.mod` `replace` pointing at the local watcher checkout, so both repos can be tweaked together.
- The `replace` is removed and the dependency pinned to `v0.2.0` once integration is proven.
- The watcher repo may be made public at any time; this is orthogonal to the `replace` workflow and only affects frictionless pinned consumption (removes the GOPRIVATE/SSH requirement). Recommended but not blocking.

---

## Phase 2a: watcher v0.2

### Generalized Subscription Lifecycle

The library's `watcher_subscriptions` grows to own subscription lifecycle and tombstone provenance as general concepts, so handler's table can be replaced without leaking handler-specific semantics into the library.

**Schema change** (`watcher_subscriptions`): add `unsubscribed_by_user INTEGER NOT NULL DEFAULT 0` (boolean). Full column set: `id, subscriber, resource_type, resource_id, resource_url, created_at, expires_at, backfill, deleted_at, unsubscribed_by_user`.

The boolean (rather than a free-form actor string) is deliberate: the only distinction any consumer makes at re-subscribe time is "did a human deliberately unwatch this?" — automated tombstones are always fine to revive. The column can be widened later if a concrete multi-actor need arises; no external consumer would break.

**API** (free functions over `*sql.DB`; subscriber is an opaque string the library never interprets):

```go
type SubscribeOpts struct {
    TTL      time.Duration // 0 = permanent (expires_at NULL)
    Backfill bool          // emit pre-subscription history on first poll
    IfAbsent bool          // no-op if a live row exists; do NOT reinstate a tombstone
}

// Subscribe creates or reinstates. It reinstates a soft-deleted row EXCEPT when
// unsubscribed_by_user is set (then it stays deleted). IfAbsent makes it a no-op
// when a live row already exists and never revives a tombstone.
func Subscribe(db *sql.DB, subscriber string, r Resource, opts SubscribeOpts) error

// Unsubscribe soft-deletes (revivable). UserUnsubscribe sets unsubscribed_by_user=1 (protected).
func Unsubscribe(db *sql.DB, subscriber string, r Resource) error
func UserUnsubscribe(db *sql.DB, subscriber string, r Resource) error

// Reinstate force-revives a tombstoned row regardless of unsubscribed_by_user.
func Reinstate(db *sql.DB, subscriber string, r Resource) error

// Lease management, by exact subscriber or by prefix (for bulk/cleanup sweeps).
func Renew(db *sql.DB, subscriber string, ttl time.Duration) error
func RenewPrefix(db *sql.DB, subscriberPrefix string, ttl time.Duration) error
func Revoke(db *sql.DB, subscriber string) error
func RevokePrefix(db *sql.DB, subscriberPrefix string) error

// Listing: Active = live (not deleted, lease not expired). All = every row with
// metadata (DeletedAt, UnsubscribedByUser, ExpiresAt) so a consumer can render WHY
// a subscription is inactive. Both accept an exact subscriber or a prefix.
func ActiveSubscriptions(db *sql.DB, subscriberOrPrefix string, prefix bool) ([]Subscription, error)
func AllSubscriptions(db *sql.DB, subscriberOrPrefix string, prefix bool) ([]Subscription, error)

// Reverse lookup: which subscribers watch a resource (returns subscriber strings + metadata).
func SubscribersOf(db *sql.DB, resourceType, resourceID string) ([]Subscription, error)
```

`ActiveResources(db, resourceType)` (the poller's fetch-list query) stays, now naturally driven by the live-lease predicate.

The `Subscription` struct gains `UnsubscribedByUser bool` alongside the existing fields.

### Config → `auth.yaml` Rename

The shared config file becomes `~/.config/watcher/auth.yaml` (auth/credentials only). Per-consumer *behavior* configuration is each consumer's own concern and stays in the consumer's own config. The library's config package reads/writes `auth.yaml` with the same 0600/0700 permission enforcement and typed accessors (`GitHub()`, `Jira()`, `Slack()`), plus the consumer DB registry. `DefaultPath()` → `~/.config/watcher/auth.yaml` (honoring `WATCHER_HOME`).

### Loop Foreground Runner

Add an optional in-process run mode so consumers aren't forced onto OS scheduling:

```go
// Loop calls pollFn every interval until ctx is cancelled. No OS scheduler,
// no per-cycle process cold-start. For consumers that keep a process alive
// (a `watcher loop` foreground command, or embedded as a goroutine in a server).
func Loop(ctx context.Context, interval time.Duration, pollFn func(context.Context) error) error
```

Three run modes are then available to any consumer, consumer's choice:
1. **One-off** — `Poll` once and exit (existing primitive; good for cron/launchd/manual).
2. **Foreground loop** — `Loop` in a long-running command.
3. **Embedded** — `Loop` as a goroutine inside a consumer that already runs a server.

The OS `scheduler` package (from v0.1) remains for consumers that want background services.

### Phase 2a Deliverables

- `watcher_subscriptions` schema + the lifecycle API above, with tests (reinstate-unless-user, IfAbsent, prefix revoke/renew, active-vs-all metadata, SubscribersOf).
- `auth.yaml` rename.
- `Loop` helper with a test (ticker fires, ctx cancellation stops it).
- Updated README.
- Tag `v0.2.0`.

---

## Phase 2b: Handler Integration + Data Migration

### Subscription Table Replacement

Handler's `subscriptions` table is dropped; `watcher_subscriptions` becomes the single store. `sessions.session_id` PK is UNCHANGED (no reformatting). The subscriber string embeds the id: `handler:session:<session_id>`. No FK from `watcher_subscriptions` to `sessions` — integrity is covered by lease expiry + explicit Revoke (the FK's cascade was never load-bearing; handler archives sessions, never hard-deletes).

**Performance:** losing the FK costs nothing. FKs provide integrity, not query speed; joins need indexes, which the library provides on `subscriber` and `(resource_type, resource_id)`. Per-session lookups become exact-match on the indexed `subscriber` column (faster than the old join); reverse lookups use `SubscribersOf` then parse the id out of the subscriber string.

Mapping of handler operations:

| Handler concept | Becomes |
|---|---|
| `subscriptions.session_id` FK | subscriber `handler:session:<id>` (parsed back out; no FK) |
| `SubscribeIfNew(s)` | `Subscribe{IfAbsent:true}` |
| `Subscribe(s)` (reinstate-aware) | `Subscribe{}` |
| `Unsubscribe(...)` (`unsubscribed_by='user'`) | `UserUnsubscribe(...)` |
| `Reinstate(...)` | `Reinstate(...)` |
| `RestoreSubscriptionsForSession(id)` | reinstate over prefix `handler:session:<id>`, honoring `unsubscribed_by_user` |
| `SoftDeleteSubscriptionsForSession(id)` | `RevokePrefix("handler:session:<id>")` |
| `SoftDeleteSubscriptionsForBranch(branch)` | handler resolves branch→session ids, then `RevokePrefix` each (library never sees "branch") |
| `ListSubscriptions(id, includeDeleted)` | `ActiveSubscriptions`/`AllSubscriptions("handler:session:<id>", prefix=true)` |
| `SessionsForResource` / `FindRelatedSessions` | `SubscribersOf(t,id)` → parse session ids |
| the ~8 read callers | via the above, parsing ids from subscriber strings |

`db/subscriptions.go` and handler's `subscriptions` table are removed; `db/resources.go`'s `FindRelatedSessions`/`SessionsForResource` reimplement on `SubscribersOf`; `ResourceHistory` reads `watcher_events`/`watcher_event_resources` (or the library's `EventsForResource`).

### Lease Wiring

- `register.go` (session-start subscribe from `.worktree-resources`) + statusline restore → `Subscribe{IfAbsent:true, TTL:5d}` / reinstate-prefix honoring `unsubscribed_by_user`.
- Session heartbeat (statusline, ~10s) → `RenewPrefix("handler:session:<id>", 5d)`.
- `unregister.go` (SessionEnd) → `RevokePrefix("handler:session:<id>")` (replaces `SoftDeleteSubscriptionsForSession`).
- **`cleanup.go` — new revoke**: it currently archives sessions but does NOT revoke their subscriptions. Add `RevokePrefix` per archived session, so both archive paths (SessionEnd and cleanup) finally agree. (This fixes a latent inconsistency the surface map found.)

5-day TTL rationale (from Phase 1 spec): sessions routinely survive a closed laptop over a weekend; expiry cannot lose events (cursor-based detection re-emits on renewal) and a resumed session re-subscribes anyway.

### Watcher Command Wrappers

`cmd/watcher/*` become thin wrappers over library calls:
- `run <name>` → resolve active resources via the library, call `github.Poll` / `jira.Poll` with creds from `auth.yaml`.
- `auth` → write to `auth.yaml` via the library config API.
- `install`/`stop`/`start`/`uninstall`/`list` → keep handler's EXISTING launchd/cron scheduling for now (invoking `handler watcher run <name>`), to minimize integration risk. Migrating to the library `scheduler` package and/or the `Loop`/embedded modes is deferred (optional future work, not part of 2b).

Credentials: migrate handler's `~/.agent-handler/config.yaml` tokens to `~/.config/watcher/auth.yaml` (one-time, part of the migration command). Jira `custom_fields`/`bot_usernames` move too.

### Migration Systems Coexist

Handler's own `runMigrations()` (owns `events`, `dismissed_events`, `sessions`, etc.) and the library's `Migrate()` (owns `watcher_*` exclusively) both run on startup, kept disjoint. The library's collision check aborts on any unexpected `watcher_*` table; handler's hook must never touch a `watcher_*` table.

### Inbox UNION + Dismissal

`db/inbox_scope.go` is rewritten so its composed query UNIONs two halves:
- **Agent half**: `events` matched via `event_recipients`/broadcast/branch/role (handler's own event types) — unchanged.
- **Watcher half**: `watcher_events` → `watcher_event_resources` → `watcher_subscriptions` where `subscriber = 'handler:session:<id>'`, lease live, `ts > cursor`, excluding bookkeeping types.

**Dismissal on BOTH halves.** `dismissed_events` stays handler-owned and stores a bare `event_id` with no source discriminator. After the split a dismissed PR-comment event lives in `watcher_events`, so `dismissedExclusionSQL` must be applied to both halves (keyed on each half's id column). Wiring it to only the `events` half silently resurrects dismissed watcher events — a real trap. Requires `event_id` uniqueness across `events` and `watcher_events` (both UUIDs — holds, now load-bearing).

**Gated on `watcher_schema_version` marker, never row counts.** Reads use the UNION path once the marker is set (final migration step), legacy single-table path before. A row-count predicate would strand poller writes into an empty-looking table; must key on the marker.

### Cursor-Advance Fix

While touching every cursor call site (the migration necessarily does): advance the cursor to `max(ts)` of the events actually returned, not `time.Now()`. The current second-granularity `time.Now()` advance permanently skips any event written in the same wall-clock second. Pre-existing bug, cheap to fix here.

### watcher_status → watcher_poller_status Repoint

The 5 reader files (`cmd/watching.go`, `cmd/triage.go`, `cmd/status.go`, `cmd/statusline.go`, `cmd/api/resources.go`) switch from handler's `GetWatcherStatus`/`HasWatcherError` (reading `watcher_status`) to the library's `GetPollerStatus`/`HasPollerError` (reading `watcher_poller_status`). Handler's `db/watcher_status.go` is deleted in 2b; the old `watcher_status` table is dropped in 2c.

### Production Data Migration

One-time command `handler setup --migrate-watcher`.

**Pre-migration (automatic + gated):**
1. Refuse if watchers active — instruct `handler watcher stop` first.
2. Auto-backup `handler.db` → `handler.db.backup-<timestamp>`; print path; abort on failure.
3. Snapshot + print verification counts (watcher event count, subscription count, resource_state count).

**Migration steps:**
1. `watcher.Migrate(handlerDB)` — create `watcher_*` (aborts on unexpected collision).
2. Copy `events WHERE source IN ('github','jira')` → `watcher_events` (+ their `event_resources` → `watcher_event_resources`). `agent`/`handler` sources stay in `events`. (The `source` filter is an explicit allowlist — handler is a third source that must NOT move.)
3. Copy `resource_state` → `watcher_resource_state`.
4. Copy `subscriptions` → `watcher_subscriptions`: subscriber `handler:session:<session_id>`; map `deleted_at`; `unsubscribed_by='user'` → `unsubscribed_by_user=1`; `expires_at`=now+5d for rows whose session is active, else leave to age out.
5. Copy `resource_relationships` → `watcher_resource_relationships`.
6. Copy handler's `watcher_status` → `watcher_poller_status` (leave original in place for rollback).
7. Migrate credentials `~/.agent-handler/config.yaml` → `~/.config/watcher/auth.yaml`.
8. Set `watcher_schema_version` marker (flips reads to the UNION path).

**Post-migration verification (automatic):** re-count in new tables, compare to snapshot, print pass/fail. Then manual: `handler watching`, `handler status`, `handler log --global`, confirm unread counts, start watchers, wait one cycle.

**Rollback:** stop watchers; restore `handler.db.backup-<timestamp>`; reinstall previous handler binary (git checkout prior commit + build + install); start watchers. Clean because 2b keeps old tables and the old binary reads the backup.

### Phase 2b Deliverables

- Library imported via `replace`; `watcher.Migrate()` on startup alongside `runMigrations()`.
- Subscription read/write layer reimplemented on the library (table above).
- Lease wiring at all four seams (including the new cleanup revoke).
- `cmd/watcher/*` thin wrappers; credentials via `auth.yaml`.
- inbox_scope UNION rewrite; dismissal on both halves; gated on schema marker.
- Cursor-advance fix.
- watcher_status readers repointed; `db/watcher_status.go` deleted.
- Migration command with backup/verify/rollback.
- `replace` removed, pinned to `v0.2.0`, handler release tagged.
- Old tables and legacy query path RETAINED (not deleted) — that's 2c.

---

## Phase 2c: Cleanup (Gated)

Explicitly part of this plan so it is not lost, but gated so it cannot fire prematurely.

**Entry gate (all must hold before 2c begins):**
- Handler has run on the new (UNION) path for a soak period of at least 7 days.
- `handler watching`, `handler status`, `handler log --global`, and inbox unread counts have been verified correct on the new path.
- The pre-migration backup is retained somewhere outside the working tree.

**Pre-cleanup safety:** take a fresh `handler.db` backup before any destructive step (cleanup is reversible too).

**Cleanup steps:**
1. Delete handler's `watcher/`, `watcher/github/`, `watcher/jira/` packages.
2. Remove the legacy single-table query path from `db/inbox_scope.go` and the cursor/unread functions (the UNION path becomes the only path). Retire only handler's *read-path gate* (the `if marker set → UNION` branch in handler's query code); do NOT remove the library's `watcher_schema_version` table itself — the library still uses it for its own migrations.
3. Delete migrated watcher rows from `events` (`source IN ('github','jira')`) and their `event_resources` rows.
4. Drop handler tables: `subscriptions`, `resource_state`, `resource_relationships`, `watcher_status`.
5. Delete now-dead db code: `db/subscriptions.go`, `db/resource_state.go`, `db/watcher_status.go`, and the watcher-specific parts of `db/resources.go` superseded by the library.
6. Tag a handler release.

### Phase 2c Deliverables

- Legacy watcher code and tables removed; single (UNION) read path.
- Migrated rows purged from `events`.
- Handler release tagged.

---

## Known Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Subscription-layer rewrite is the most entangled change | Every handler op maps to a named library call (table above); library grown in 2a first and tagged, so 2b builds on a tested API. |
| Dismissed watcher events silently reappear | Apply `dismissedExclusionSQL` to BOTH UNION halves; relies on `event_id` uniqueness across `events`/`watcher_events` (UUIDs). |
| Read path/write path disagree post-migration | Path selection keyed to `watcher_schema_version` marker, never row counts. |
| Handler + library migration hooks collide | Disjoint ownership; library collision check aborts on unexpected `watcher_*`; handler hook never touches `watcher_*`. |
| Production data loss | Auto-backup before migration; automatic count verification; documented+tested rollback; old tables retained through 2b. |
| `source` filter moves handler's own events | Explicit allowlist `('github','jira')`; `agent`/`handler` sources stay in `events`. |
| Losing the subscriptions↔sessions FK | Integrity via lease expiry + explicit Revoke; no hard session deletes; leases age out orphans. |
| cleanup.go never revoked subscriptions (latent bug) | 2b adds `RevokePrefix` to cleanup so both archive paths agree. |
| 2c cleanup fires prematurely / loses data | Explicit entry gate (soak ≥7d + verified views + retained backup) and a pre-cleanup backup. |
| Cursor second-granularity skip (pre-existing) | Advance to `max(ts)` of returned events. |

## Scope

### In scope
- Phase 2a: library v0.2 (subscription lifecycle, auth.yaml, Loop), tagged.
- Phase 2b: handler integration, subscription-table replacement, inbox UNION, migration command, watcher_status repoint.
- Phase 2c: gated cleanup of legacy code/tables/query-path.

### Out of scope (future work)
- Migrating handler onto the library `scheduler` package or Loop/embedded run modes (handler keeps launchd/cron + `watcher run` for now).
- Slack poller (still unimplemented; format reserved).
- Worktree integration (its own later effort; the generalized subscription lifecycle and tombstone model built here directly serve its auto-discovery UX).
- The two v0.1 follow-up tickets (watcher #1 TOCTOU, #2 pending-only CI bundle) — tracked separately; #1 becomes more relevant once a second concurrent writer exists.
