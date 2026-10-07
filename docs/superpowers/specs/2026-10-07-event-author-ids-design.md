# Event author IDs — design

Date: 2026-10-07
Status: approved in brainstorming, awaiting spec review
Repos: `~/git/watcher` (library) and `~/git/worktree` (consumer)

## Goal

Record a stable account ID for the person behind each watcher event, so a
consumer can tell events the user caused apart from everyone else's.
`worktree` uses that to:

1. never send a notification for an event the user caused, and
2. never count the user's own events as unread.

## Why IDs, not names

`watcher_events.author` holds a display value that varies by source:

| Source | `author` today | Problem with matching on it |
|---|---|---|
| GitHub | login (`mturley`) | logins can be renamed |
| Jira | display name (`Mike Turley`) | not unique; changes with the profile |
| Slack | resolved name (`Mike Turley`, or a username like `andrewballantyne`) | the same person can show either form |

Account IDs are stable and unique: GitHub `databaseId`, Jira `accountId`,
Slack user ID.

## Decisions

- Your own events are **excluded** from notifications and from unread.
  Replying does **not** implicitly read the events before it: others'
  events stay unread until marked read.
- The unread cursor is never moved automatically. "Mine" is a filter
  applied when unread is computed, so there is nothing to mark.
- Two GitHub events that have no author today get one:
  - `pr_merged` gets its author from `mergedBy` (exact);
  - `pr_new_commits` gets one only when **every commit the event lists**
    is linked to the same GitHub user. Otherwise it stays authorless.
- `pr_closed` and CI events stay authorless.
- A missing ID (old rows, authorless events, a source whose identity
  lookup failed) means nothing is excluded, which is today's behavior.

## Part 1 — watcher library

### Schema

- Add a nullable `author_id TEXT` column to `watcher_events`.
  - Add it to `schemaDDL`'s `CREATE TABLE`, to
    `managedColumns["watcher_events"]`, and to
    `additiveColumns["watcher_events"]` (`"TEXT"`), so existing databases
    (worktree's and agent-handler's) gain it on their next `Migrate`.
  - No `CurrentSchemaVersion` bump, matching how `unsubscribed_by_user`
    and `watcher_resource_meta.updated_at` were added.
  - No backfill: existing rows keep `NULL`.
- `watcher.Event` gains `AuthorID *string`; `db.InsertEvent` writes it.
- `author_id` is meaningful only together with `source`: the same string
  in two sources is unrelated.

### GitHub (GraphQL)

- At every author selection (PR author, reviews, comments, review
  comments), request the numeric ID through type fragments, since
  `Actor` itself has no `databaseId`:

  ```graphql
  author { __typename login ... on User { databaseId } ... on Bot { databaseId } }
  ```

  `authorNode` gains `DatabaseID *int64` (`json:"databaseId"`). `Review`,
  `Comment` and `ReviewComment` gain `AuthorID string`, the decimal
  `databaseId`, or `""` when absent (e.g. a Mannequin or a deleted
  account). `emitEvent` passes it through as `AuthorID`.
- `pr_merged`: add `mergedBy { __typename login ... on User { databaseId } ... on Bot { databaseId } }`
  to the PR query, and set `author`, `author_type` and `author_id` on the
  `pr_merged` event from it. `pr_closed` stays authorless: there is no
  equivalent field.
- `pr_new_commits`: add `author { user { login databaseId } }` to the
  commit nodes, and record each `CommitEntry`'s `AuthorLogin` and
  `AuthorID`. The event's commit list is the same one
  `formatNewCommitsBody` builds today (the commits after the previous
  head, or all recent commits if the previous head isn't among them).
  The event is attributed only if that list is non-empty and every commit
  in it has the same non-empty `AuthorID`. Factor that list out of
  `formatNewCommitsBody` (`newCommitsSince(prData, prevSHA)`) so the body
  and the attribution use exactly the same list.
- New helper: `github.ViewerID(token string, apiURL ...string) (string, error)`
  queries `{ viewer { databaseId } }` and returns the decimal ID. It
  follows `github.Validate`'s request style.

### Jira

- Decode `author.accountId` alongside `displayName` in `fetchChangelog`
  and `fetchComments`. `ChangelogEntry` and `IssueComment` gain
  `AuthorID`. Every `emitEvent` call that passes an author also passes
  its `AuthorID`.
- New helper: `jira.AccountID(host, email, token string) (string, error)`
  calls `/rest/api/3/myself` and returns `accountId`. It follows
  `jira.Validate`'s request style.

### Slack

- `slack_reply` events get `author_id` = the message's `UserID`. A
  message with no `UserID` (some bot posts) gets none.
- No new helper: `HTTPClient.WhoAmI` already returns the user's ID.

### Release

- Tests with synthetic fixtures (see Testing), then a minor release
  (`v0.10.0`): commit, tag, push `main` and the tag.
- agent-handler needs no code change; its DB gains the column the next
  time it re-pins and migrates.

## Part 2 — worktree

### Re-pin

`go get github.com/mturley/watcher@v0.10.0 && go mod tidy`.

### `internal/selfid`

- A worktree-owned table, created in `internal/db/migrate.go`:

  ```sql
  CREATE TABLE IF NOT EXISTS self_identity (
    source     TEXT PRIMARY KEY,  -- "github", "jira", "slack"
    author_id  TEXT NOT NULL,
    updated_at TEXT NOT NULL
  )
  ```

- `selfid.Set(conn, source, id)` upserts a row. `selfid.Load(conn)`
  returns `map[source]id`.
- `selfid.NotMineSQL`: one SQL fragment, written once, that every query
  appends against its `watcher_events` alias `e`:

  ```sql
  NOT EXISTS (SELECT 1 FROM self_identity si
              WHERE si.source = e.source AND si.author_id = e.author_id)
  ```

  A NULL `author_id` never equals anything, so authorless and old events
  are never excluded.
- `selfid.Resolver` looks up each configured source's ID and stores it:
  GitHub through `github.ViewerID`, Jira through `jira.AccountID`, Slack
  through the Slack client's `WhoAmI`.
  - `worktree ui` runs it at startup. It then retries any source that
    failed every 10 minutes, until all configured sources have succeeded.
  - A failed lookup keeps that source's previous row. An unconfigured
    source is skipped.
- Identities live in the DB rather than in memory, so the CLI's unread
  counts (`worktree resources list`) apply the same exclusion without
  network calls, and a restart doesn't lose them.

### Where "not mine" applies

- **Notifier:** `readNewEvents` adds `AND <NotMineSQL>`. A resource whose
  only new events are the user's gets no notification, and a mixed batch
  counts only others' events (its body uses the newest of those).
- **Unread:**
  - `unread.Counts` / `Summaries` and `unread.SlackCounts` add the
    condition. Unread counts, unread-only filters and `unread_through_ts`
    ("Mark N as read") therefore cover only others' events.
  - The timeline's unread-only clause adds it.
  - The per-event `unread` flag (`unreadIndex.IsUnread`) loads the
    identities once per request and returns false for the user's own
    events. The timeline row read therefore also selects `e.author_id`.
- Display is unchanged: the user's own events still appear in feeds, just
  never boxed as unread.

## Failure modes

| Situation | Behavior |
|---|---|
| Identity lookup fails at startup | Nothing is excluded for that source until a retry succeeds. |
| Lookup fails after an earlier success | The stored row stays; exclusion continues. |
| Event has no `author_id` (old, authorless, unlinked commit) | Treated as someone else's. |
| GitHub stops returning `databaseId` | `author_id` empty; same as authorless. |
| User renames on any source | IDs don't change; still excluded. |

## Testing

Library:

- **Migration:** a DB created at the previous schema (without
  `author_id`) gains the column on `Migrate`; a fresh DB has it; the
  collision check still rejects alien tables.
- **`InsertEvent`:** round-trips `AuthorID`.
- **GitHub:**
  - parsing a GraphQL fixture yields `AuthorID` for a User and a Bot, and
    `""` for an actor without `databaseId`;
  - `pr_merged` carries `mergedBy`;
  - `pr_new_commits` is attributed when every listed commit shares one
    author, and is not attributed when they differ, when one is unlinked,
    or when the list is empty;
  - `ViewerID` parses `viewer.databaseId` from an httptest server.
- **Jira:** fixtures for changelog and comments yield `AuthorID`;
  `AccountID` parses `/myself`.
- **Slack:** a reply event carries the message's `UserID`.

worktree:

- **`selfid`:** `Set`/`Load`; the Resolver stores the IDs from fake
  lookups, skips unconfigured sources, keeps the old row on failure, and
  retries.
- **Notifier:** the user's events are skipped, a mixed batch counts only
  others', and an empty `self_identity` changes nothing.
- **Unread:** counts, summaries, Slack counts, the unread-only clause and
  the `unread` flag all exclude the user's events and leave others' alone.

## Docs

- watcher: document `author_id` alongside the other `watcher_events`
  columns, plus the two identity helpers.
- worktree: describe `internal/selfid` in `.claude/CLAUDE.md`, and the
  "not mine" rule in `docs/web-ui-architecture.md` (the "Unread" and
  "Notifications" sections).
