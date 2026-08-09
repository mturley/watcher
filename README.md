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
go get github.com/mturley/watcher@v0.1.0
```

(`v0.1.0` is the intended first tag for this library; if you're building
against a pre-release commit, use the commit SHA or a branch instead.)

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

## Config

Config is a single shared YAML file, by default at
`~/.config/watcher/config.yaml`, loaded with `config.Load` and written
with `(*Config).Save` (which enforces `0600` permissions since it holds
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

## Status / scope

`v0.1.0` is the extraction of agent-handler's watcher subsystem into a
standalone, consumer-agnostic library. Deliberately out of scope for
this release:

- Slack polling (resource ID format reserved, no poller implemented)
- A cross-system discovery CLI/UI
- Any integration code for specific consumers (agent-handler, worktree,
  etc.) — those live in their own repos and depend on this library

See the design spec for the full rationale and future-work list:
[`docs/superpowers/specs/2026-07-31-watcher-library-design.md`](docs/superpowers/specs/2026-07-31-watcher-library-design.md).
