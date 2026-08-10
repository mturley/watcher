# watcher

`watcher` is a Go library that polls external resources — GitHub pull
requests and Jira issues today, Slack threads in the future — and records
every change it detects as a durable event in a SQLite database.

It is **not**:

- a ledger's read-state (dismissal, cursors, "unread" tracking) — that's
  the consumer's job, layered on top of the events this library writes
- a CLI or daemon — it's a library you embed in your own binary
- a hosted service — each consumer supplies and owns its own `*sql.DB`

`watcher` owns a small set of `watcher_*` tables: events, per-resource
cached state, subscriptions (with lease/TTL semantics), cross-resource
relationships (e.g. Jira issue → epic), and poller status. Everything
else — how you read those tables, what "caught up" means for your
consumer, how you dismiss or acknowledge events — is up to you.

See the full design rationale in
[`docs/superpowers/specs/2026-07-31-watcher-library-design.md`](docs/superpowers/specs/2026-07-31-watcher-library-design.md).

## Install

```bash
go get github.com/mturley/watcher@v0.2.0
```

(if you're building against a pre-release commit, use the commit SHA or
a branch instead.)

**Note on upgrading from v0.1:** There is no in-place v0.1→v0.2 database upgrade path. If you have an existing v0.1 watcher database, you must delete it and let `db.Migrate` create a fresh v0.2 schema (the schema added a column and migration aborts if the table structure doesn't match).

## Quickstart

```go
package main

import (
	"database/sql"
	"log"
	"os"

	"github.com/mturley/watcher"
	"github.com/mturley/watcher/db"
	"github.com/mturley/watcher/github"

	_ "modernc.org/sqlite"
)

func main() {
	// You own the *sql.DB. Enable WAL so readers (your app) and the
	// poller (this call, or a scheduled process) don't block each other.
	conn, err := sql.Open("sqlite", "file:mytool.db?_pragma=journal_mode(WAL)")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	// Migrate creates/upgrades the watcher_* tables. Safe to call on
	// every startup.
	if err := db.Migrate(conn); err != nil {
		log.Fatal(err)
	}

	pr := watcher.Resource{
		Type: "pr",
		ID:   "owner/repo#42",
		URL:  "https://github.com/owner/repo/pull/42",
	}

	// Subscribe your consumer to the resource. Backfill: false means the
	// first poll only records a "watch_started" marker, not history.
	if err := db.Subscribe(conn, "mytool", pr, db.SubscribeOpts{}); err != nil {
		log.Fatal(err)
	}

	// Poll GitHub for the subscribed resources (run this on a schedule,
	// see the Scheduler section below).
	token := os.Getenv("GITHUB_TOKEN")
	logger := log.New(os.Stderr, "", log.LstdFlags)
	resources, err := db.ActiveResources(conn, "pr")
	if err != nil {
		log.Fatal(err)
	}
	if err := github.Poll(conn, token, resources, logger); err != nil {
		log.Fatal(err)
	}

	// Read events back — by resource, or by everything a subscriber
	// hasn't seen since some point in time.
	events, err := db.EventsForResource(conn, "pr", "owner/repo#42")
	if err != nil {
		log.Fatal(err)
	}
	for _, e := range events {
		log.Printf("[%s] %s: %s", e.TS, e.Type, e.Title)
	}

	since := "2026-01-01T00:00:00Z"
	newEvents, err := db.EventsForSubscriberSince(conn, "mytool", since)
	if err != nil {
		log.Fatal(err)
	}
	_ = newEvents
}
```

## Consumer integration model

- **You own your `*sql.DB`.** Open it however fits your app (in-process
  SQLite via `modernc.org/sqlite`, a shared file, etc.) and pass it into
  every `db.*`, `github.Poll`, and `jira.Poll` call.
- **The library owns its tables.** `db.Migrate` creates
  `watcher_schema_version`, `watcher_events`, `watcher_event_resources`,
  `watcher_resource_state`, `watcher_subscriptions`,
  `watcher_resource_relationships`, and `watcher_poller_status`. If any
  of those table names already exist in your DB with a different schema,
  `Migrate` refuses to proceed rather than risk corrupting your data.
- **You own your read-state.** Cursors, "seen"/"dismissed" flags, and
  any other consumer-specific bookkeeping should be stored in your own
  tables, keyed off `watcher_events.id` or `ts`. The library only tracks
  what happened, not what you've done with it.
- **Enable WAL mode** (`_pragma=journal_mode(WAL)` in the DSN, or
  `PRAGMA journal_mode=WAL` after opening) if a poller and a reader
  process might touch the DB concurrently — this is the standard SQLite
  setup for a single-writer/multi-reader workload.

## First-poll behavior

Each subscription carries a `Backfill` flag (`db.SubscribeOpts.Backfill`).
On a resource's first poll (no events recorded yet for it):

- **`Backfill: false`** (the default) — the poller emits a single
  `watch_started` event and does *not* flood the DB with the resource's
  entire history. This is the right default for "start watching this PR
  from now on."
- **`Backfill: true`** — the poller emits every historical event it can
  see (all comments, reviews, status changes, etc.) as if you'd been
  watching the resource all along.

`db.BackfillFor` resolves this per-resource from live subscriptions, so
if any subscriber to a resource asked for backfill, the poller will do a
full backfill for everyone.

Source-specific caveats:

- **GitHub**: check-run/CI data is paginated; the poller follows
  pagination via `FetchRemainingCheckContexts` to assemble a complete
  CI bundle per commit.
- **Jira**: the issue changelog is fully paginated by the client before
  the poller processes it, so no partial-history edge cases there.
- **Slack**: not yet implemented (see Status below). The `slack:...`
  resource ID format is reserved but there is no poller yet.

## Subscription lifecycle

A subscription is keyed by `(subscriber, resource_type, resource_id)`;
`db` maintains at most one row per key and tracks its state through a
tombstone model rather than deleting rows outright:

- **`db.Subscribe(conn, subscriber, resource, db.SubscribeOpts{TTL, Backfill, IfAbsent})`**
  creates a subscription, or reinstates/refreshes an existing one, with
  one exception: if the existing row was soft-deleted via
  `UserUnsubscribe` (`unsubscribed_by_user = 1`), `Subscribe` leaves it
  tombstoned — only `Reinstate` can revive a user-removed subscription.
  A non-user tombstone (from `Unsubscribe`, `Revoke`/`RevokePrefix`, or
  lease expiry) is reinstated normally.
  `SubscribeOpts.IfAbsent`, when true, makes `Subscribe` a no-op if a
  *live* row already exists (it won't refresh `url`/`expires_at`/
  `backfill`), but a non-user tombstone still counts as "absent" and
  gets reinstated even with `IfAbsent` set.
- **`db.Unsubscribe(conn, subscriber, resource)`** soft-deletes the
  subscription (revivable by a later `Subscribe` or `Reinstate`).
- **`db.UserUnsubscribe(conn, subscriber, resource)`** soft-deletes the
  subscription *and* sets `unsubscribed_by_user`, protecting it from
  being silently reinstated by `Subscribe` — use this when the removal
  reflects explicit user intent (e.g. an "unsubscribe" button) as
  opposed to internal bookkeeping.
- **`db.Reinstate(conn, subscriber, resource)`** clears both
  `deleted_at` and `unsubscribed_by_user`, unconditionally reviving the
  subscription regardless of how it was removed.
- **`db.Renew(conn, subscriber, ttl)`** / **`db.RenewPrefix(conn, subscriberPrefix, ttl)`**
  extend the lease (`expires_at`) on all of a subscriber's live
  subscriptions, matching the subscriber exactly or by prefix
  (`subscriber LIKE prefix||'%'`) respectively.
- **`db.Revoke(conn, subscriber)`** / **`db.RevokePrefix(conn, subscriberPrefix)`**
  soft-delete all of a subscriber's live subscriptions (exact or
  prefix match); neither sets the user tombstone flag.
- **`db.ActiveSubscriptions(conn, subscriberOrPrefix, prefix bool)`**
  returns only live, non-expired subscriptions (subscriber exact match
  when `prefix` is false, prefix match when true) as `[]db.Subscription`.
- **`db.AllSubscriptions(conn, subscriberOrPrefix, prefix bool)`**
  returns every subscription for that subscriber/prefix, including
  soft-deleted and lease-expired ones. Each `db.Subscription` carries
  `DeletedAt *string`, `ExpiresAt *string`, and `UnsubscribedByUser bool`
  alongside `ID`, `Subscriber`, `Resource`, `CreatedAt`, and `Backfill`,
  so a consumer can show *why* a subscription is inactive (explicitly
  unsubscribed by the user vs. revoked vs. lease-expired) instead of
  just that it's gone.
- **`db.SubscribersOf(conn, resourceType, resourceID)`** is the reverse
  lookup: every subscription (any state) for a given resource, useful
  for "who's watching this" tooling.

## Config

Config is a single shared YAML credentials file, by default at
`~/.config/watcher/auth.yaml` (`config.DefaultPath()`, overridable via
the `WATCHER_HOME` env var), loaded with `config.Load` and written with
`(*Config).Save` (which enforces `0600` permissions since it holds
credentials — `Load` refuses to read a group/world-readable file).

```yaml
services:
  github:
    token: ghp_xxx
  jira:
    host: https://yourcompany.atlassian.net
    email: you@yourcompany.com
    token: xxx
jira_custom_fields:
  epic_key: customfield_10014
consumers:
  mytool:
    db: /Users/you/.local/share/mytool/mytool.db
```

Typed accessors return an error if a service isn't configured, rather
than zero-value credentials:

```go
cfg, err := config.Load(config.DefaultPath())
ghCreds, err := cfg.GitHub()   // config.GitHubCreds{Token}
jiraCreds, err := cfg.Jira()   // config.JiraCreds{Host, Email, Token, CustomFields}
```

`cfg.RegisterConsumer(name, dbPath)` / `cfg.Consumers()` manage the
`consumers` registry, letting a single config file coordinate multiple
consumer databases.

**Jira epic-link note:** the Jira poller records an `epic` relationship
(via `db.LinkResources`) from an issue to its parent epic, but it can
only do so if `jira_custom_fields` maps the display name `epic_key` to
your Jira instance's actual custom field ID (e.g.
`customfield_10014` — this varies per Jira instance). Without that
entry, epic-link relationships are silently skipped. This mapping is a
deferred, per-instance concern — there's no way to discover it
generically from the API.

### Behavior config (config.yaml)

Separate from `auth.yaml`, the library also reads an optional behavior
config file, by default at `~/.config/watcher/config.yaml`
(`config.ConfigDefaultPath()`, also overridable via `WATCHER_HOME`),
loaded with `config.LoadConfig`. Unlike `auth.yaml`, it holds no
credentials, so it isn't subject to the same permission checks, and a
missing file simply loads as empty defaults (not an error).

```yaml
jira:
  bot_usernames:
    - dependabot
    - jira-automation
  custom_fields:
    epic_key: customfield_10014
    story_points: customfield_10028
```

`bot_usernames` lists Jira usernames to classify as bots when
attributing comment/changelog authors. `custom_fields` maps friendly
names to Jira custom field IDs so the poller can fetch and cache extra
issue data (epic link, story points, blocked status, etc.). Both are
configuration rather than credentials, which is why they live here in
`config.yaml` and not in `auth.yaml`. Nil-safe accessors:

```go
cfg, err := config.LoadConfig(config.ConfigDefaultPath())
bots := cfg.JiraBotUsernames()   // []string, or nil if unset
fields := cfg.JiraCustomFields() // map[string]string, or nil if unset
```

## Resource ID formats

| Type    | ID format                    | Example                            |
|---------|-------------------------------|-------------------------------------|
| `pr`    | `owner/repo#number`           | `owner/repo#42`                     |
| `jira`  | issue key                     | `PROJ-123`                          |
| `slack` | `slack:CHANNEL:THREAD_TS`     | `slack:C0123ABC:1699999999.000100`  |

The `slack` format is reserved for future use; there is no Slack poller
yet (see Status below).

## Scheduler

`scheduler.Install(scheduler.ScheduleConfig{...})` generates and installs
OS-level periodic scheduling to run *your* command (not this library's —
you supply the full argv, e.g. `[]string{"/usr/local/bin/mytool", "poll",
"github"}`) on an interval: a launchd plist on macOS, or a crontab entry
on Linux. `scheduler.Uninstall`, `Start`, `Stop`, `IsInstalled`, and
`IsRunning` round out lifecycle management.

## Run modes

There are three ways to drive polling, depending on how your consumer
is deployed:

- **One-off `Poll`** (`github.Poll`, `jira.Poll`): call it yourself,
  e.g. from a CLI subcommand invoked by cron/launchd, or once at
  startup. This is the building block the other two modes wrap.
- **Foreground `Loop`**: for a long-running process (a `watch loop`
  command, or a goroutine inside a server), use the library's own
  in-process scheduler instead of OS-level scheduling:

  ```go
  err := watcher.Loop(ctx, 5*time.Minute, func(ctx context.Context) error {
      resources, err := db.ActiveResources(conn, "pr")
      if err != nil {
          return err
      }
      return github.Poll(conn, token, resources, logger)
  })
  ```

  `Loop(ctx, interval, pollFn)` runs `pollFn` immediately, then again
  every `interval`, until `ctx` is cancelled (at which point it returns
  `ctx.Err()`). A `pollFn` error does not stop the loop — a transient
  poll failure shouldn't end watching — so log or record errors inside
  `pollFn` if you need to observe them.
- **OS scheduler** (`scheduler.Install`, see above): let launchd/cron
  invoke your own poll command on an interval, for consumers that
  don't want a long-running process.

## Status / scope

`v0.1.0` was the extraction of agent-handler's watcher subsystem into a
standalone, consumer-agnostic library. `v0.2.0` generalizes the
subscription lifecycle (tombstones, `IfAbsent`, `UserUnsubscribe`/
`Reinstate`, prefix-based renew/revoke), renames the config file to
`auth.yaml`, and adds the foreground `Loop` runner. Deliberately out of
scope so far:

- Slack polling (resource ID format reserved, no poller implemented)
- A cross-system discovery CLI/UI
- Any integration code for specific consumers (agent-handler, worktree,
  etc.) — those live in their own repos and depend on this library

See the design spec for the full rationale and future-work list:
[`docs/superpowers/specs/2026-07-31-watcher-library-design.md`](docs/superpowers/specs/2026-07-31-watcher-library-design.md).
The v0.2 subscription-lifecycle generalization is documented in
[`docs/superpowers/specs/2026-08-09-phase2-handler-integration-design.md`](docs/superpowers/specs/2026-08-09-phase2-handler-integration-design.md).
