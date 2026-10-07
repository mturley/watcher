# Event Author IDs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Record a stable `author_id` on watcher events (library), then have `worktree` exclude the user's own events from notifications and unread.

**Architecture:** The watcher library gains a nullable `watcher_events.author_id` (schema v5) that the GitHub (GraphQL `databaseId`), Jira (`accountId`) and Slack (user ID) pollers fill, plus identity helpers (`github.ViewerID`, `jira.AccountID`). `worktree` re-pins, stores the user's per-source IDs in a `self_identity` table (`internal/selfid`), and applies one shared SQL condition, `selfid.NotMineSQL`, in the notifier and every unread query. It also checks the same identities in Go for the per-event `unread` flag.

**Tech Stack:** Go, SQLite (`modernc.org/sqlite` in watcher, worktree's existing driver), GitHub GraphQL, Jira REST v3, Slack Web API.

**Spec:** `docs/superpowers/specs/2026-10-07-event-author-ids-design.md` (in `~/git/watcher`)

## Global Constraints

- Repos:
  - Tasks 1–6 run in `~/git/watcher`, on `main` (the library's established practice).
  - Tasks 7–11 run in a new `worktree` worktree, on branch `author-ids` (create it with `cd ~/git/worktree && worktree add author-ids`).
- Schema: `CurrentSchemaVersion` goes 4 → 5. `author_id TEXT` is nullable, with no backfill.
- `author_id` is per source. Matching is always on `(source, author_id)`, never on `author_id` alone.
- GitHub ID = decimal `databaseId`, as a string. Jira ID = `accountId`. Slack ID = message `UserID`.
- `pr_new_commits` is attributed only if the list of commits the event covers (`newCommitsSince`) is non-empty and every entry has the same non-empty `AuthorID`. `pr_closed` and CI events stay authorless.
- A NULL or empty `author_id`, or no `self_identity` row for the source, means the event is never excluded.
- Library release tag: `v0.10.0`. **Pushing `main` and the tag needs Mike's explicit OK at that step** (CLAUDE.md: pushes are not covered by plan approval).
- Commits: `git commit --signoff`, ending with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. `git add` names files explicitly.
- Tests:
  - watcher: `go test ./...`.
  - worktree: `make test`. Use `NONINTERACTIVE=1 make install` to install.

## Review Focus

1. **A database already at schema v4** (worktree's and handler's real DBs) gains `author_id` on the next `Migrate`, without the collision check refusing the narrower table. Test in Task 1.
2. **`pr_new_commits` after a force-push.** The previous head isn't in the recent list, so the event covers all recent commits; it's attributed only if all of them share one author. Test in Task 3.
3. **A GitHub actor without `databaseId`** (Mannequin, deleted account) yields `AuthorID ""` and must never match. Tests in Tasks 2 and 9.
4. **The same `author_id` string under a different source** must not be excluded. Test in Task 7.
5. **"Mark N as read" when the newest unread events are the user's own.** `unread_through_ts` comes from others' newest event, and the user's later event never shows as unread. Test in Task 10.

---

## Part 1 — watcher library (`~/git/watcher`)

### Task 1: `author_id` column, schema v5, `Event.AuthorID`

**Files:**
- Modify: `watcher.go` (`Event` struct)
- Modify: `db/schema.go` (DDL, `managedColumns`, `CurrentSchemaVersion` + comment)
- Modify: `db/migrate.go` (`additiveColumns`)
- Modify: `db/events.go` (`InsertEvent`)
- Modify: `db/subscriptions.go` (the two `SELECT … e.tags` reads, `scanEvents`)
- Test: `db/migrate_test.go`, `db/events_test.go`

**Interfaces:**
- Produces: `watcher.Event.AuthorID *string`; column `watcher_events.author_id`; `db.EventsForResource` / `db.EventsForSubscriberSince` return `AuthorID`.

- [ ] **Step 1: Write the failing tests**

Append to `db/migrate_test.go`:

```go
// A database already at v4 (no author_id) must gain the column on Migrate.
// Without the v5 bump, Migrate would return early and never add it.
func TestMigrateV4AddsEventAuthorID(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(`ALTER TABLE watcher_events DROP COLUMN author_id`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(`UPDATE watcher_schema_version SET version = 4`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(c); err != nil {
		t.Fatalf("Migrate on a v4 DB: %v", err)
	}
	if !hasColumn(t, c, "watcher_events", "author_id") {
		t.Fatal("watcher_events missing author_id after migrating from v4")
	}
	if v, _ := SchemaVersion(c); v != 5 {
		t.Fatalf("version = %d, want 5", v)
	}
}
```

Append to `db/events_test.go`:

```go
func TestInsertEventRoundTripsAuthorID(t *testing.T) {
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	ts := "2026-01-15T12:00:00Z"
	id, login := "583231", "alice"
	res := watcher.Resource{Type: "pr", ID: "owner/repo#1"}
	if err := InsertEvent(c, watcher.Event{ID: "e1", TS: ts, ExternalTS: &ts, Source: "github",
		Type: watcher.EventTypePRComment, Title: "c", Author: &login, AuthorID: &id}, res); err != nil {
		t.Fatal(err)
	}
	if err := InsertEvent(c, watcher.Event{ID: "e2", TS: ts, ExternalTS: &ts, Source: "github",
		Type: watcher.EventTypePRComment, Title: "d"}, res); err != nil {
		t.Fatal(err)
	}
	evs, err := EventsForResource(c, "pr", "owner/repo#1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*string{}
	for _, e := range evs {
		got[e.ID] = e.AuthorID
	}
	if got["e1"] == nil || *got["e1"] != id {
		t.Fatalf("e1 AuthorID = %v, want %q", got["e1"], id)
	}
	if got["e2"] != nil {
		t.Fatalf("e2 AuthorID = %q, want nil", *got["e2"])
	}
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./db/ -run 'V4AddsEventAuthorID|RoundTripsAuthorID'`
Expected: FAIL (`unknown field AuthorID`, and the DROP COLUMN of a missing column).

- [ ] **Step 3: Implement**

- `watcher.go`, in `Event` after `AuthorType`:

  ```go
  	// AuthorID is the author's stable account ID in Source's own namespace
  	// (GitHub databaseId, Jira accountId, Slack user ID); nil when unknown.
  	// Compare it only together with Source.
  	AuthorID *string
  ```

- `db/schema.go`:
  - add `author_id TEXT` after `tags TEXT` in the `watcher_events` DDL;
  - add `"author_id"` to `managedColumns["watcher_events"]`;
  - set `const CurrentSchemaVersion = 5`, and append to the comment above it:

    ```go
    // Bumped to 5 to add watcher_events.author_id (see additiveColumns).
    ```

- `db/migrate.go`, in `additiveColumns`:

  ```go
  	// author_id: the author's stable account ID (schema v5). Nullable, never
  	// backfilled: events recorded before v5 simply have no ID.
  	"watcher_events": {
  		"author_id": "TEXT",
  	},
  ```

- `db/events.go` `InsertEvent`: add `author_id` to the column list and `e.AuthorID` to the values, after `tags` / `e.Tags`.
- `db/subscriptions.go`:
  - in both `SELECT … e.author, e.author_type, e.tags`, append `, e.author_id`;
  - in `scanEvents`, add `authorID sql.NullString`, scan it last, and set `e.AuthorID = &authorID.String` when valid.

- [ ] **Step 4: Run the whole library suite**

Run: `go test ./...`
Expected: PASS. If a test asserts `CurrentSchemaVersion == 4` or counts `watcher_events` columns, update it to v5 / 11 columns and record a ruling.

- [ ] **Step 5: Commit**

```bash
git add watcher.go db/schema.go db/migrate.go db/events.go db/subscriptions.go db/migrate_test.go db/events_test.go
git commit --signoff -m "feat(db): add watcher_events.author_id (schema v5)" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: GitHub author IDs on PR, review, comment and review-comment events

**Files:**
- Modify: `github/graphql.go` (query text, `authorNode`, `PRData`/`Review`/`Comment`/`ReviewComment`, `parsePRNode`)
- Modify: `github/poller.go` (`emitEvent` signature and all callers)
- Test: `github/graphql_test.go`, `github/poller_test.go`

**Interfaces:**
- Consumes: `watcher.Event.AuthorID` (Task 1).
- Produces:
  - `authorNode.DatabaseID *int64` and `func (a authorNode) id() string`;
  - `PRData.AuthorID`, and `Review.AuthorID` / `Comment.AuthorID` / `ReviewComment.AuthorID` (`string`);
  - `emitEvent(conn, t, title, body, externalTS, author, authorType, authorID *string, r)`.

- [ ] **Step 1: Write the failing tests**

Append to `github/graphql_test.go`:

```go
func TestParseGraphQLResponse_AuthorIDs(t *testing.T) {
	raw := `{
		"pr0": {"pullRequest": {
			"number": 1, "state": "OPEN", "title": "T", "updatedAt": "2024-01-01T00:00:00Z",
			"author": {"__typename": "User", "login": "alice", "databaseId": 101},
			"reviews": {"nodes": [{"author": {"__typename": "Bot", "login": "rabbit", "databaseId": 202}, "state": "COMMENTED", "submittedAt": "2024-01-01T00:00:00Z", "body": "b"}]},
			"comments": {"nodes": [{"author": {"__typename": "Mannequin", "login": "ghost"}, "createdAt": "2024-01-01T00:00:00Z", "body": "c"}]},
			"reviewThreads": {"nodes": [{"comments": {"nodes": [{"author": {"__typename": "User", "login": "bob", "databaseId": 303}, "createdAt": "2024-01-01T00:00:00Z", "path": "x.go", "body": "rc"}]}}]},
			"commits": {"totalCount": 0, "nodes": []}
		}},
		"rateLimit": {"remaining": 5000, "limit": 5000}
	}`
	res, _, err := parseGraphQLResponse(json.RawMessage(raw), []PRRef{{Owner: "o", Repo: "r", Number: 1}})
	if err != nil {
		t.Fatal(err)
	}
	pr := res[0]
	if pr.AuthorID != "101" || pr.Reviews[0].AuthorID != "202" || pr.ReviewComments[0].AuthorID != "303" {
		t.Fatalf("ids = %q %q %q", pr.AuthorID, pr.Reviews[0].AuthorID, pr.ReviewComments[0].AuthorID)
	}
	if pr.Comments[0].AuthorID != "" {
		t.Fatalf("an actor without databaseId must yield \"\", got %q", pr.Comments[0].AuthorID)
	}
}

func TestBuildBatchedPRQueryRequestsDatabaseIDs(t *testing.T) {
	q := buildBatchedPRQuery([]PRRef{{Owner: "o", Repo: "r", Number: 1}})
	if n := strings.Count(q, "... on User { databaseId }"); n < 4 {
		t.Fatalf("expected databaseId fragments at every author selection, found %d", n)
	}
}
```

(Add `"strings"` to that file's imports if it is missing.)

Append to `github/poller_test.go`:

```go
func TestProcessPR_CommentCarriesAuthorID(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", prResource, db.SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := processPR(conn, PRData{Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "T",
		UpdatedAt: "2026-06-17T08:00:00Z"}, prResource, "t", false, testLogger()); err != nil {
		t.Fatal(err)
	}
	pr := PRData{Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "T", UpdatedAt: "2026-06-17T10:00:00Z",
		Comments: []Comment{{Author: "alice", AuthorType: "user", AuthorID: "101", CreatedAt: "2026-06-17T09:00:00Z", Body: "hi"}}}
	if _, err := processPR(conn, pr, prResource, "t", false, testLogger()); err != nil {
		t.Fatal(err)
	}
	evs, _ := db.EventsForResource(conn, "pr", prResource.ID)
	for _, e := range evs {
		if e.Type == watcher.EventTypePRComment {
			if e.AuthorID == nil || *e.AuthorID != "101" {
				t.Fatalf("pr_comment AuthorID = %v, want 101", e.AuthorID)
			}
			return
		}
	}
	t.Fatal("no pr_comment emitted")
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./github/ -run 'AuthorIDs|DatabaseIDs|CommentCarriesAuthorID'`
Expected: FAIL (`AuthorID` undefined).

- [ ] **Step 3: Implement**

- In `buildBatchedPRQuery`, change each of the four author selections (PR `author`, review `author`, comment `author`, review-comment `author`) from

  ```graphql
  author {
    __typename
    login
  }
  ```

  to (keep each site's existing indentation)

  ```graphql
  author {
    __typename
    login
    ... on User { databaseId }
    ... on Bot { databaseId }
  }
  ```

- `authorNode`:

  ```go
  type authorNode struct {
  	Typename   string `json:"__typename"`
  	Login      string `json:"login"`
  	DatabaseID *int64 `json:"databaseId"` // absent for actors that aren't a User or Bot
  }

  // id is the actor's stable numeric ID as a decimal string, or "" when
  // GitHub didn't supply one (e.g. a Mannequin or a deleted account).
  func (a authorNode) id() string {
  	if a.DatabaseID == nil {
  		return ""
  	}
  	return strconv.FormatInt(*a.DatabaseID, 10)
  }
  ```

  (import `strconv`)
- Add `AuthorID string` after `AuthorType` in `PRData`, `Review`, `Comment` and `ReviewComment`, and set it in `parsePRNode` with `node.Author.id()`, `r.Author.id()`, `c.Author.id()` and `rc.Author.id()`.
- `github/poller.go`:
  - `emitEvent` gains `authorID *string` after `authorType` and sets `AuthorID: authorID` on the event.
  - At the review / comment / review-comment call sites, pass a pointer to the ID, using this helper:

    ```go
    // optional returns &s, or nil for "", so an unknown ID stores NULL.
    func optional(s string) *string {
    	if s == "" {
    		return nil
    	}
    	return &s
    }
    ```

    e.g. `…, &review.Author, &review.AuthorType, optional(review.AuthorID), resource)`.
  - Every other `emitEvent` call passes `nil` for `authorID`.

- [ ] **Step 4: Run the package tests**

Run: `go test ./github/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add github/graphql.go github/poller.go github/graphql_test.go github/poller_test.go
git commit --signoff -m "feat(github): record author databaseId on PR events" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: GitHub `pr_merged` and `pr_new_commits` attribution

**Files:**
- Modify: `github/graphql.go` (query: `mergedBy`, commit `author { user }`; `prNode.MergedBy`; `PRData.MergedBy*`; `CommitEntry.AuthorLogin/AuthorID`; `commitNode`)
- Modify: `github/poller.go` (merged emission; `newCommitsSince`; `commonCommitAuthor`; new-commits emission; `formatNewCommitsBody` uses `newCommitsSince`)
- Test: `github/graphql_test.go`, `github/poller_test.go`

**Interfaces:**
- Consumes: `authorNode.id()`, `optional`, `emitEvent` (Task 2).
- Produces:
  - `PRData.MergedBy`, `MergedByType`, `MergedByID string`;
  - `CommitEntry.AuthorLogin`, `AuthorID string`;
  - `newCommitsSince(prData PRData, prevSHA string) []CommitEntry`;
  - `commonCommitAuthor(cs []CommitEntry) (login, id string, ok bool)`.

- [ ] **Step 1: Write the failing tests**

Append to `github/graphql_test.go`:

```go
func TestParseGraphQLResponse_MergedByAndCommitAuthors(t *testing.T) {
	raw := `{
		"pr0": {"pullRequest": {
			"number": 1, "state": "MERGED", "title": "T", "updatedAt": "2024-01-01T00:00:00Z",
			"author": {"__typename": "User", "login": "alice", "databaseId": 101},
			"mergedBy": {"__typename": "User", "login": "alice", "databaseId": 101},
			"reviews": {"nodes": []}, "comments": {"nodes": []}, "reviewThreads": {"nodes": []},
			"commits": {"totalCount": 2, "nodes": [
				{"commit": {"oid": "aaa1111", "committedDate": "2024-01-01T00:00:00Z", "messageHeadline": "one", "author": {"user": {"login": "alice", "databaseId": 101}}}},
				{"commit": {"oid": "bbb2222", "committedDate": "2024-01-02T00:00:00Z", "messageHeadline": "two", "author": {"user": null}}}
			]}
		}},
		"rateLimit": {"remaining": 5000, "limit": 5000}
	}`
	res, _, err := parseGraphQLResponse(json.RawMessage(raw), []PRRef{{Owner: "o", Repo: "r", Number: 1}})
	if err != nil {
		t.Fatal(err)
	}
	pr := res[0]
	if pr.MergedBy != "alice" || pr.MergedByID != "101" || pr.MergedByType != "user" {
		t.Fatalf("mergedBy = %q %q %q", pr.MergedBy, pr.MergedByID, pr.MergedByType)
	}
	if pr.Commits.Recent[0].AuthorID != "101" || pr.Commits.Recent[0].AuthorLogin != "alice" {
		t.Fatalf("commit 0 author = %+v", pr.Commits.Recent[0])
	}
	if pr.Commits.Recent[1].AuthorID != "" {
		t.Fatalf("an unlinked commit must have no AuthorID, got %q", pr.Commits.Recent[1].AuthorID)
	}
}
```

Append to `github/poller_test.go`:

```go
func TestProcessPR_MergedCarriesMergedBy(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", prResource, db.SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	processPR(conn, PRData{Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "T",
		UpdatedAt: "2026-06-17T08:00:00Z"}, prResource, "t", false, testLogger())
	processPR(conn, PRData{Number: 123, Owner: "owner", Repo: "repo", State: "MERGED", Title: "T",
		UpdatedAt: "2026-06-17T10:00:00Z", MergedBy: "alice", MergedByType: "user", MergedByID: "101"},
		prResource, "t", false, testLogger())
	evs, _ := db.EventsForResource(conn, "pr", prResource.ID)
	for _, e := range evs {
		if e.Type == watcher.EventTypePRMerged {
			if e.AuthorID == nil || *e.AuthorID != "101" || e.Author == nil || *e.Author != "alice" {
				t.Fatalf("pr_merged author = %v / %v", e.Author, e.AuthorID)
			}
			return
		}
	}
	t.Fatal("no pr_merged emitted")
}

// newCommitsEvent runs a seed poll at head "a1", then a poll whose recent
// commits are `recent`, and returns the pr_new_commits event (or nil).
func newCommitsEvent(t *testing.T, recent []CommitEntry) *watcher.Event {
	t.Helper()
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", prResource, db.SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	seed := PRData{Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "T", UpdatedAt: "2026-06-17T08:00:00Z",
		Commits: CommitInfo{TotalCount: 1, LatestSHA: "a1aaaaaa", LatestDate: "2026-06-17T08:00:00Z",
			Recent: []CommitEntry{{SHA: "a1aaaaaa", Date: "2026-06-17T08:00:00Z", MessageHeadline: "seed"}}}}
	if _, err := processPR(conn, seed, prResource, "t", false, testLogger()); err != nil {
		t.Fatal(err)
	}
	last := recent[len(recent)-1]
	next := seed
	next.UpdatedAt = "2026-06-17T10:00:00Z"
	next.Commits = CommitInfo{TotalCount: len(recent), LatestSHA: last.SHA, LatestDate: last.Date, Recent: recent}
	if _, err := processPR(conn, next, prResource, "t", false, testLogger()); err != nil {
		t.Fatal(err)
	}
	evs, _ := db.EventsForResource(conn, "pr", prResource.ID)
	for i := range evs {
		if evs[i].Type == watcher.EventTypePRNewCommits {
			return &evs[i]
		}
	}
	return nil
}

func TestProcessPR_NewCommitsAttribution(t *testing.T) {
	seed := CommitEntry{SHA: "a1aaaaaa", Date: "2026-06-17T08:00:00Z", MessageHeadline: "seed"}
	mine := func(sha, date string) CommitEntry {
		return CommitEntry{SHA: sha, Date: date, MessageHeadline: "m", AuthorLogin: "alice", AuthorID: "101"}
	}
	cases := []struct {
		name   string
		recent []CommitEntry
		wantID string // "" = unattributed
	}{
		{"all new commits share one author", []CommitEntry{seed, mine("b2bbbbbb", "2026-06-17T09:00:00Z"), mine("c3cccccc", "2026-06-17T09:30:00Z")}, "101"},
		{"mixed authors", []CommitEntry{seed, mine("b2bbbbbb", "2026-06-17T09:00:00Z"),
			{SHA: "c3cccccc", Date: "2026-06-17T09:30:00Z", MessageHeadline: "x", AuthorLogin: "bob", AuthorID: "202"}}, ""},
		{"an unlinked commit", []CommitEntry{seed, mine("b2bbbbbb", "2026-06-17T09:00:00Z"),
			{SHA: "c3cccccc", Date: "2026-06-17T09:30:00Z", MessageHeadline: "x"}}, ""},
		// Force-push: the old head is gone, so the event covers every recent
		// commit, and the unlinked seed-like commit below breaks attribution.
		{"force-push, one recent commit unlinked", []CommitEntry{
			{SHA: "z9zzzzzz", Date: "2026-06-17T09:00:00Z", MessageHeadline: "rebased"}, mine("c3cccccc", "2026-06-17T09:30:00Z")}, ""},
		{"force-push, all recent commits mine", []CommitEntry{mine("y8yyyyyy", "2026-06-17T09:00:00Z"), mine("c3cccccc", "2026-06-17T09:30:00Z")}, "101"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := newCommitsEvent(t, c.recent)
			if ev == nil {
				t.Fatal("no pr_new_commits emitted")
			}
			got := ""
			if ev.AuthorID != nil {
				got = *ev.AuthorID
			}
			if got != c.wantID {
				t.Fatalf("AuthorID = %q, want %q", got, c.wantID)
			}
		})
	}
}

func TestCommonCommitAuthorEmptyList(t *testing.T) {
	if _, _, ok := commonCommitAuthor(nil); ok {
		t.Fatal("an empty commit list must not be attributed")
	}
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./github/ -run 'MergedByAndCommitAuthors|MergedCarriesMergedBy|NewCommitsAttribution|CommonCommitAuthor'`
Expected: FAIL (undefined fields and functions).

- [ ] **Step 3: Implement**

- **Query.** In `buildBatchedPRQuery`, directly after the PR-level `author { … }` block, add:

  ```graphql
  mergedBy {
    __typename
    login
    ... on User { databaseId }
    ... on Bot { databaseId }
  }
  ```

  Inside `commits(last: 10) { nodes { commit { … } } }`, after `messageHeadline`, add:

  ```graphql
  author {
    user {
      login
      databaseId
    }
  }
  ```

- **Types.**
  - `prNode` gains `MergedBy *authorNode \`json:"mergedBy"\``.
  - `commitNode.Commit` gains:

    ```go
    		Author struct {
    			User *struct {
    				Login      string `json:"login"`
    				DatabaseID *int64 `json:"databaseId"`
    			} `json:"user"`
    		} `json:"author"`
    ```

  - `PRData` gains `MergedBy`, `MergedByType`, `MergedByID string`.
  - `CommitEntry` gains `AuthorLogin`, `AuthorID string`. Both are `""` when the commit isn't linked to a GitHub user.
- **`parsePRNode`.** When `node.MergedBy != nil`, set `MergedBy = node.MergedBy.Login`, `MergedByType = authorType(node.MergedBy.Typename)` and `MergedByID = node.MergedBy.id()`. In the commits loop, when `cn.Commit.Author.User != nil`, set `AuthorLogin` and `AuthorID` (decimal `DatabaseID`, `""` if nil).
- **`github/poller.go`.** Split `formatNewCommitsBody`'s selection out:

  ```go
  // newCommitsSince is the list of commits a pr_new_commits event covers: those
  // after prevSHA in the recent list, or every recent commit when prevSHA isn't
  // in it (a force-push, or more new commits than the query fetches). The body
  // and the event's attribution both use this list, so they always agree.
  func newCommitsSince(prData PRData, prevSHA string) []CommitEntry {
  	var out []CommitEntry
  	foundPrev := false
  	for _, c := range prData.Commits.Recent {
  		if c.SHA == prevSHA {
  			foundPrev = true
  			continue
  		}
  		if foundPrev {
  			out = append(out, c)
  		}
  	}
  	if !foundPrev {
  		return prData.Commits.Recent
  	}
  	return out
  }

  // commonCommitAuthor returns the GitHub user every commit in cs was authored
  // by. ok is false for an empty list, or when any commit is unlinked or has a
  // different author: then nobody can be credited with the push.
  func commonCommitAuthor(cs []CommitEntry) (login, id string, ok bool) {
  	if len(cs) == 0 {
  		return "", "", false
  	}
  	for _, c := range cs {
  		if c.AuthorID == "" || c.AuthorID != cs[0].AuthorID {
  			return "", "", false
  		}
  	}
  	return cs[0].AuthorLogin, cs[0].AuthorID, true
  }
  ```

  `formatNewCommitsBody` starts with `newCommits := newCommitsSince(prData, prevSHA)`, replacing its own loop; the rest is unchanged.
- **New-commits emission.** Replace the `emitEvent(conn, watcher.EventTypePRNewCommits, …, nil, nil, resource)` call with:

  ```go
  				var author, authorType, authorID *string
  				if login, id, ok := commonCommitAuthor(newCommitsSince(prData, prevSHA)); ok {
  					user := "user"
  					author, authorType, authorID = &login, &user, &id
  				}
  				if err := emitEvent(conn, watcher.EventTypePRNewCommits, title, &body, prData.Commits.LatestDate, author, authorType, authorID, resource); err != nil {
  ```

- **Merged emission.** Only for `pr_merged` (`pr_closed` keeps `nil`s):

  ```go
  			var author, authorType, authorID *string
  			if eventType == watcher.EventTypePRMerged && prData.MergedBy != "" {
  				author, authorType, authorID = &prData.MergedBy, &prData.MergedByType, optional(prData.MergedByID)
  			}
  ```

  and pass those three to the existing `emitEvent` call.

- [ ] **Step 4: Run the package tests**

Run: `go test ./github/`
Expected: PASS, including the existing `formatNewCommitsBody` and new-commits tests.

- [ ] **Step 5: Commit**

```bash
git add github/graphql.go github/poller.go github/graphql_test.go github/poller_test.go
git commit --signoff -m "feat(github): attribute pr_merged to mergedBy and single-author pushes" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Jira and Slack author IDs

**Files:**
- Modify: `jira/client.go` (`IssueComment`, `ChangelogEntry`, `fetchChangelog`, `fetchComments`)
- Modify: `jira/poller.go` (`emitEvent` signature and all 7 call sites)
- Modify: `slack/poller.go` (`emitEvent` signature, 3 call sites)
- Test: `jira/poller_test.go`, `slack/poller_test.go`

**Interfaces:**
- Consumes: `watcher.Event.AuthorID` (Task 1).
- Produces: `ChangelogEntry.AuthorID`, `IssueComment.AuthorID`; Slack `slack_reply` events carry `AuthorID`.

- [ ] **Step 1: Write the failing tests**

Append to `jira/poller_test.go`:

```go
func TestFetchIssue_DecodesAccountIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/rest/api/3/issue/X-1":
			json.NewEncoder(w).Encode(map[string]interface{}{"key": "X-1", "fields": map[string]interface{}{
				"summary": "S", "status": map[string]interface{}{"name": "Open"}}})
		case "/rest/api/3/issue/X-1/changelog":
			json.NewEncoder(w).Encode(map[string]interface{}{"startAt": 0, "maxResults": 100, "total": 1, "isLast": true,
				"values": []interface{}{map[string]interface{}{
					"author":  map[string]interface{}{"displayName": "Jane", "accountId": "acc-jane"},
					"created": "2026-06-17T09:00:00.000+0000",
					"items":   []interface{}{map[string]interface{}{"field": "status", "fromString": "A", "toString": "B"}},
				}}})
		case "/rest/api/3/issue/X-1/comment":
			json.NewEncoder(w).Encode(map[string]interface{}{"startAt": 0, "maxResults": 100, "total": 1,
				"comments": []interface{}{map[string]interface{}{
					"author":  map[string]interface{}{"displayName": "Jane", "accountId": "acc-jane"},
					"created": "2026-06-17T09:30:00.000+0000", "body": "plain",
				}}})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer server.Close()
	issue, err := (&Client{BaseURL: server.URL, Email: "e", Token: "t"}).FetchIssue("X-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if issue.Changelog[0].AuthorID != "acc-jane" || issue.Comments[0].AuthorID != "acc-jane" {
		t.Fatalf("ids = %q / %q", issue.Changelog[0].AuthorID, issue.Comments[0].AuthorID)
	}
}

func TestProcessIssue_EventsCarryAuthorID(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", jiraResource, db.SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	processIssue(conn, JiraAuth{}, IssueData{Key: "RHOAIENG-123", Summary: "S", Status: "To Do",
		Changelog: []ChangelogEntry{{Author: "J", CreatedAt: "2026-06-17T08:00:00.000+0000", Field: "status", From: "A", To: "B"}}},
		jiraResource, false, testLogger())
	issue := IssueData{Key: "RHOAIENG-123", Summary: "S", Status: "Done",
		Comments:  []IssueComment{{Author: "Jane", AuthorID: "acc-jane", CreatedAt: "2026-06-17T09:00:00.000+0000", Body: "c"}},
		Changelog: []ChangelogEntry{{Author: "Jane", AuthorID: "acc-jane", CreatedAt: "2026-06-17T09:30:00.000+0000", Field: "status", From: "B", To: "C"}}}
	if _, err := processIssue(conn, JiraAuth{}, issue, jiraResource, false, testLogger()); err != nil {
		t.Fatal(err)
	}
	evs, _ := db.EventsForResource(conn, "jira", jiraResource.ID)
	seen := 0
	for _, e := range evs {
		if e.Type == watcher.EventTypeJiraComment || e.Type == watcher.EventTypeJiraStatusChange {
			if e.AuthorID == nil || *e.AuthorID != "acc-jane" {
				t.Fatalf("%s AuthorID = %v", e.Type, e.AuthorID)
			}
			seen++
		}
	}
	if seen != 2 {
		t.Fatalf("checked %d events, want 2", seen)
	}
}
```

(Reuse the file's existing imports. `testutil`, `db` and `watcher` are already used by its other tests. If the issue GET in this repo also requests other paths, add matching `case`s from `TestFetchIssue_ChangelogPagination`.)

In `slack/poller_test.go`'s `TestPoll_NewReplyEmitsSlackReplyWithVerbatimTS`, after the `ExternalTS` assertion, add:

```go
	if ev.AuthorID == nil || *ev.AuthorID != "U2" {
		t.Errorf("AuthorID = %v, want the replier's user ID U2", ev.AuthorID)
	}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./jira/ ./slack/ -run 'DecodesAccountIDs|EventsCarryAuthorID|NewReplyEmitsSlackReply'`
Expected: FAIL

- [ ] **Step 3: Implement**

- `jira/client.go`:
  - `IssueComment` and `ChangelogEntry` gain `AuthorID string`.
  - In both page structs, `Author` gains `AccountID string \`json:"accountId"\``.
  - Set `AuthorID: history.Author.AccountID` and `AuthorID: cm.Author.AccountID`.
- `jira/poller.go`:
  - `emitEvent` gains `authorID *string` after `authorType`, and sets `AuthorID`.
  - The comment call site passes `optional(comment.AuthorID)`, and the five changelog call sites pass `optional(entry.AuthorID)`.
  - The remaining call (watch_started or error) passes `nil`.
  - Add the same `optional` helper as in Task 2; the packages don't share code.
- `slack/poller.go`:
  - `emitEvent` gains `authorID *string` and sets `AuthorID`.
  - The `slack_reply` call passes `optional(m.UserID)`; the other two calls pass `nil`.
  - Add `optional` here too.

- [ ] **Step 4: Run the suite**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add jira/client.go jira/poller.go jira/poller_test.go slack/poller.go slack/poller_test.go
git commit --signoff -m "feat(jira,slack): record author account IDs on events" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Identity helpers `github.ViewerID` and `jira.AccountID`

**Files:**
- Modify: `github/validate.go`, `jira/validate.go`
- Test: `github/validate_test.go`, `jira/validate_test.go`

**Interfaces:**
- Produces:
  - `github.ViewerID(token string, apiURL ...string) (string, error)`: the decimal `viewer.databaseId`;
  - `jira.AccountID(host, email, token string) (string, error)`: the `/myself` `accountId`.

- [ ] **Step 1: Write the failing tests**

`github/validate_test.go`:

```go
func TestViewerID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"data":{"viewer":{"databaseId":583231}}}`))
	}))
	defer srv.Close()
	id, err := ViewerID("tok", srv.URL)
	if err != nil || id != "583231" {
		t.Fatalf("ViewerID = %q, %v", id, err)
	}
	if _, err := ViewerID("bad", srv.URL); !errors.Is(err, ErrAuth) {
		t.Fatalf("bad token err = %v, want ErrAuth", err)
	}
}
```

`jira/validate_test.go`:

```go
func TestAccountID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/myself" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"accountId":"acc-me","displayName":"Me"}`))
	}))
	defer srv.Close()
	id, err := AccountID(srv.URL, "e", "t")
	if err != nil || id != "acc-me" {
		t.Fatalf("AccountID = %q, %v", id, err)
	}
}
```

(Add any missing imports: `errors`, `net/http`, `net/http/httptest`.)

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./github/ ./jira/ -run 'ViewerID|AccountID'`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement**

`github/validate.go`:

```go
// ViewerID returns the authenticated user's numeric GitHub ID (databaseId)
// as a decimal string: the value pollers store in watcher_events.author_id.
func ViewerID(token string, apiURL ...string) (string, error) {
	endpoint := "https://api.github.com/graphql"
	if len(apiURL) > 0 && apiURL[0] != "" {
		endpoint = apiURL[0]
	}
	body, _ := json.Marshal(map[string]string{"query": "{ viewer { databaseId } }"})
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("github viewer: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("github viewer request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("invalid GitHub token: %w", ErrAuth)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github API status %d", resp.StatusCode)
	}
	var out struct {
		Data   struct{ Viewer struct{ DatabaseID int64 `json:"databaseId"` } }
		Errors []struct{ Message string }
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("github viewer parse: %w", err)
	}
	if len(out.Errors) > 0 {
		return "", fmt.Errorf("github GraphQL error: %s", out.Errors[0].Message)
	}
	if out.Data.Viewer.DatabaseID == 0 {
		return "", fmt.Errorf("github viewer: empty databaseId")
	}
	return strconv.FormatInt(out.Data.Viewer.DatabaseID, 10), nil
}
```

(import `strconv`)

`jira/validate.go`:

```go
// AccountID returns the authenticated user's Jira accountId: the value
// pollers store in watcher_events.author_id.
func AccountID(host, email, token string) (string, error) {
	base := strings.TrimRight(host, "/")
	req, err := http.NewRequest(http.MethodGet, base+"/rest/api/3/myself", nil)
	if err != nil {
		return "", fmt.Errorf("jira myself: %w", err)
	}
	req.SetBasicAuth(email, token)
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("jira myself request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("invalid Jira credentials: %w", ErrAuth)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("jira API status %d", resp.StatusCode)
	}
	var out struct {
		AccountID string `json:"accountId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("jira myself parse: %w", err)
	}
	if out.AccountID == "" {
		return "", fmt.Errorf("jira myself: empty accountId")
	}
	return out.AccountID, nil
}
```

- [ ] **Step 4: Run the suite**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add github/validate.go github/validate_test.go jira/validate.go jira/validate_test.go
git commit --signoff -m "feat: add github.ViewerID and jira.AccountID identity helpers" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: Library docs and the v0.10.0 release

**Files:**
- Modify: `README.md` (or the doc that lists `watcher_events` columns; find it with `grep -rn "author_type" README.md docs/ --include=*.md | grep -v superpowers`)

- [ ] **Step 1: Document**

Next to the existing `author` / `author_type` column description, add:
- `author_id`: the author's stable account ID in the source's namespace (GitHub `databaseId`, Jira `accountId`, Slack user ID). It is NULL for events before schema v5 and for authorless events, and is compared only together with `source`.
- `pr_merged` is attributed to `mergedBy`. `pr_new_commits` is attributed only when every commit it covers shares one linked author.
- The `github.ViewerID` and `jira.AccountID` helpers.

If no such doc exists, add a short "Event authors" section to `README.md`.

- [ ] **Step 2: Verify and commit**

Run: `go vet ./... && go test ./...`
Expected: PASS

```bash
git add README.md
git commit --signoff -m "docs: document event author IDs" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
git tag v0.10.0
```

- [ ] **Step 3: STOP — ask Mike before pushing**

Ask for approval, then run `git push origin main && git push origin v0.10.0`. Part 2 can't start until the tag is pushed, because `go get` resolves it from GitHub.

---

## Part 2 — worktree (`~/git/worktree`, branch `author-ids` in a new worktree)

### Task 7: Re-pin, the `self_identity` table, and `internal/selfid`

**Files:**
- Modify: `go.mod`, `go.sum` (re-pin)
- Modify: `internal/db/migrate.go` (table)
- Create: `internal/selfid/selfid.go`, `internal/selfid/selfid_test.go`

**Interfaces:**
- Produces:
  - `selfid.Set(conn *sql.DB, source, id string) error`;
  - `selfid.Load(conn *sql.DB) (map[string]string, error)`;
  - `selfid.NotMineSQL` (string const, written against alias `e`);
  - `selfid.IsMine(ids map[string]string, source, authorID string) bool`.

- [ ] **Step 1: Create the worktree and re-pin**

```bash
cd ~/git/worktree && worktree add author-ids
cd ~/.worktrees/worktree/author-ids
go get github.com/mturley/watcher@v0.10.0 && go mod tidy && go build ./... && go test ./... 2>&1 | tail -5
```

Expected: builds and passes. The library's new column is invisible to existing code.

- [ ] **Step 2: Write the failing tests**

`internal/selfid/selfid_test.go`:

```go
package selfid

import (
	"database/sql"
	"path/filepath"
	"testing"

	wdb "github.com/mturley/worktree/internal/db"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := wdb.OpenAt(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestSetAndLoad(t *testing.T) {
	conn := testDB(t)
	if err := Set(conn, "github", "101"); err != nil {
		t.Fatal(err)
	}
	if err := Set(conn, "github", "102"); err != nil { // upsert
		t.Fatal(err)
	}
	Set(conn, "slack", "U1")
	got, err := Load(conn)
	if err != nil {
		t.Fatal(err)
	}
	if got["github"] != "102" || got["slack"] != "U1" || len(got) != 2 {
		t.Fatalf("Load = %v", got)
	}
}

func TestNotMineSQLMatchesSourceAndID(t *testing.T) {
	conn := testDB(t)
	Set(conn, "github", "101")
	ins := func(id, source, authorID string) {
		var a any
		if authorID != "" {
			a = authorID
		}
		if _, err := conn.Exec(`INSERT INTO watcher_events (id, ts, source, type, title, author_id)
			VALUES (?, '2026-01-01T00:00:00Z', ?, 'pr_comment', 't', ?)`, id, source, a); err != nil {
			t.Fatal(err)
		}
	}
	ins("mine", "github", "101")
	ins("other", "github", "202")
	ins("same-id-other-source", "jira", "101")
	ins("authorless", "github", "")
	rows, err := conn.Query(`SELECT e.id FROM watcher_events e WHERE ` + NotMineSQL + ` ORDER BY e.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		got = append(got, id)
	}
	want := "authorless,other,same-id-other-source"
	if j := join(got); j != want {
		t.Fatalf("not-mine rows = %s, want %s", j, want)
	}
}

func TestIsMine(t *testing.T) {
	ids := map[string]string{"github": "101"}
	if !IsMine(ids, "github", "101") || IsMine(ids, "jira", "101") || IsMine(ids, "github", "") || IsMine(nil, "github", "101") {
		t.Fatal("IsMine wrong")
	}
}

func join(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}
```

- [ ] **Step 3: Run them to make sure they fail**

Run: `go test ./internal/selfid/`
Expected: FAIL (undefined `Set`, …).

- [ ] **Step 4: Implement**

In `internal/db/migrate.go`, append to `stmts` after `worktree_notify`:

```go
		// The user's own account ID per source (internal/selfid), so queries
		// can leave out events the user caused. Written by worktree ui at
		// startup; read by every unread and notification query.
		`CREATE TABLE IF NOT EXISTS self_identity (
			source     TEXT PRIMARY KEY,
			author_id  TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
```

`internal/selfid/selfid.go`:

```go
// Package selfid records the user's own account ID per source, so events
// the user caused can be left out of notifications and unread. IDs come from
// the watcher library (watcher_events.author_id): GitHub databaseId, Jira
// accountId, Slack user ID. Stored in the DB so the CLI applies the same rule
// without network calls.
package selfid

import (
	"database/sql"
	"time"
)

// NotMineSQL is the condition "this event was not caused by the user", over
// a watcher_events row aliased e. Matched on source AND id: IDs are only
// unique within a source. A NULL author_id never matches, so authorless and
// pre-v5 events always count as someone else's.
const NotMineSQL = `NOT EXISTS (SELECT 1 FROM self_identity si
	WHERE si.source = e.source AND si.author_id = e.author_id)`

// Set stores the user's ID for source, replacing any previous one.
func Set(conn *sql.DB, source, id string) error {
	_, err := conn.Exec(
		`INSERT INTO self_identity (source, author_id, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT (source) DO UPDATE SET author_id = excluded.author_id, updated_at = excluded.updated_at`,
		source, id, time.Now().UTC().Format(time.RFC3339))
	return err
}

// Load returns the stored IDs keyed by source.
func Load(conn *sql.DB) (map[string]string, error) {
	rows, err := conn.Query(`SELECT source, author_id FROM self_identity`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var src, id string
		if err := rows.Scan(&src, &id); err != nil {
			return nil, err
		}
		out[src] = id
	}
	return out, rows.Err()
}

// IsMine is NotMineSQL's Go twin, for code that already holds an event.
func IsMine(ids map[string]string, source, authorID string) bool {
	return authorID != "" && ids[source] == authorID
}
```

- [ ] **Step 5: Run the tests, then commit**

Run: `go test ./internal/selfid/ ./internal/db/`
Expected: PASS

```bash
git add go.mod go.sum internal/db/migrate.go internal/selfid/selfid.go internal/selfid/selfid_test.go
git commit --signoff -m "feat(selfid): store the user's per-source account IDs; re-pin watcher v0.10.0" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: Resolve identities at `worktree ui` startup, with retry

**Files:**
- Create: `internal/selfid/resolver.go`, `internal/selfid/resolver_test.go`
- Modify: `internal/webui/server.go` (start the resolver), `cmd/ui.go`

**Interfaces:**
- Consumes: `selfid.Set`; `wgithub.ViewerID`, `wjira.AccountID`; `(*Server).whoAmI` (`internal/webui/slack.go`).
- Produces:
  - `type Lookup func(ctx context.Context) (string, error)`;
  - `type Resolver struct { DB *sql.DB; Lookups map[string]Lookup; Logf func(string, ...any) }`, with `func (r *Resolver) ResolveOnce(ctx) (pending int)`;
  - `func (r *Resolver) Run(ctx context.Context, retry time.Duration)`;
  - `func (s *Server) StartSelfIdentity(retry time.Duration) (stop func())`.

- [ ] **Step 1: Write the failing tests**

`internal/selfid/resolver_test.go`:

```go
package selfid

import (
	"context"
	"errors"
	"testing"
)

func TestResolveOnceStoresAndCountsPending(t *testing.T) {
	conn := testDB(t)
	fail := true
	r := &Resolver{DB: conn, Lookups: map[string]Lookup{
		"github": func(context.Context) (string, error) { return "101", nil },
		"jira": func(context.Context) (string, error) {
			if fail {
				return "", errors.New("down")
			}
			return "acc-me", nil
		},
	}}
	if pending := r.ResolveOnce(context.Background()); pending != 1 {
		t.Fatalf("pending = %d, want 1 (jira failed)", pending)
	}
	ids, _ := Load(conn)
	if ids["github"] != "101" || ids["jira"] != "" {
		t.Fatalf("after first pass: %v", ids)
	}
	fail = false
	if pending := r.ResolveOnce(context.Background()); pending != 0 {
		t.Fatalf("pending = %d, want 0", pending)
	}
	ids, _ = Load(conn)
	if ids["jira"] != "acc-me" {
		t.Fatalf("after retry: %v", ids)
	}
}

func TestResolveOnceKeepsOldRowOnFailure(t *testing.T) {
	conn := testDB(t)
	Set(conn, "slack", "U1")
	r := &Resolver{DB: conn, Lookups: map[string]Lookup{
		"slack": func(context.Context) (string, error) { return "", errors.New("expired") },
	}}
	r.ResolveOnce(context.Background())
	if ids, _ := Load(conn); ids["slack"] != "U1" {
		t.Fatalf("a failed lookup must keep the stored ID, got %v", ids)
	}
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./internal/selfid/ -run Resolve`
Expected: FAIL (undefined `Resolver`).

- [ ] **Step 3: Implement**

`internal/selfid/resolver.go`:

```go
package selfid

import (
	"context"
	"database/sql"
	"time"
)

// Lookup returns the user's ID for one source.
type Lookup func(ctx context.Context) (string, error)

// Resolver looks up the user's ID for each configured source and stores it.
// Only configured sources get a Lookup; a source that fails keeps whatever
// was stored before, so a transient outage never stops exclusion.
type Resolver struct {
	DB      *sql.DB
	Lookups map[string]Lookup
	Logf    func(format string, args ...any)
}

// ResolveOnce tries every source not yet resolved in this Resolver's
// lifetime and returns how many are still pending.
func (r *Resolver) ResolveOnce(ctx context.Context) (pending int) {
	for src, look := range r.Lookups {
		id, err := look(ctx)
		if err == nil && id != "" {
			err = Set(r.DB, src, id)
		}
		if err != nil || id == "" {
			pending++
			if r.Logf != nil {
				r.Logf("self identity: %s: %v", src, err)
			}
			continue
		}
		delete(r.Lookups, src)
	}
	return pending
}

// Run resolves now, then retries the pending sources every retry interval
// until all have succeeded or ctx is done.
func (r *Resolver) Run(ctx context.Context, retry time.Duration) {
	if r.ResolveOnce(ctx) == 0 {
		return
	}
	t := time.NewTicker(retry)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if r.ResolveOnce(ctx) == 0 {
				return
			}
		}
	}
}
```

(`ResolveOnce` deletes from the map it ranges over, which Go allows. That deletion is what makes a later pass skip resolved sources.)

`internal/webui/server.go` (or a new `internal/webui/selfid.go`):

```go
// StartSelfIdentity resolves the user's per-source IDs in the background (see
// internal/selfid) and retries failures every retry interval.
func (s *Server) StartSelfIdentity(retry time.Duration) (stop func()) {
	lookups := map[string]selfid.Lookup{}
	if cfg, err := wconfig.Load(wconfig.DefaultPath()); err == nil {
		if gh, err := cfg.GitHub(); err == nil {
			lookups["github"] = func(context.Context) (string, error) { return wgithub.ViewerID(gh.Token) }
		}
		if jc, err := cfg.Jira(); err == nil {
			lookups["jira"] = func(context.Context) (string, error) { return wjira.AccountID(jc.Host, jc.Email, jc.Token) }
		}
	}
	if s.SlackClient != nil {
		lookups["slack"] = s.whoAmI
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &selfid.Resolver{DB: s.DB, Lookups: lookups, Logf: s.logger().Printf}
	go r.Run(ctx, retry)
	return cancel
}
```

(Imports: `context`, `selfid`, plus `wconfig`, `wgithub`, `wjira` as `poller.go` imports them.)

`cmd/ui.go`, after the notifier start:

```go
	// The user's own account IDs, so their events never notify or count as
	// unread (internal/selfid).
	stopSelf := srv.StartSelfIdentity(10 * time.Minute)
	defer stopSelf()
```

- [ ] **Step 4: Run the tests and build**

Run: `go test ./internal/selfid/ ./internal/webui/ && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/selfid/resolver.go internal/selfid/resolver_test.go internal/webui/server.go cmd/ui.go
git commit --signoff -m "feat(selfid): resolve the user's IDs at ui startup with retry" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

(Add `internal/webui/selfid.go` instead of `server.go` if that's where `StartSelfIdentity` went.)

---

### Task 9: Notifier skips the user's own events

**Files:**
- Modify: `internal/webui/notify_scan.go` (`readNewEvents` query)
- Test: `internal/webui/notify_scan_test.go`

**Interfaces:**
- Consumes: `selfid.NotMineSQL`, `selfid.Set`.

- [ ] **Step 1: Write the failing test**

```go
func TestReadNewEventsSkipsMyOwnEvents(t *testing.T) {
	conn := unreadTestDB(t)
	selfid.Set(conn, "github", "101")
	c, _ := initNotifyCursor(conn)
	insertTypedEvent(t, conn, "mine", "2026-01-01T00:00:01Z", "pr_comment", "x", "pr", "o/r#1")
	insertTypedEvent(t, conn, "theirs", "2026-01-01T00:00:02Z", "pr_comment", "x", "pr", "o/r#1")
	insertTypedEvent(t, conn, "ghost", "2026-01-01T00:00:03Z", "pr_comment", "x", "pr", "o/r#1")
	conn.Exec(`UPDATE watcher_events SET author_id = '101' WHERE id = 'mine'`)
	conn.Exec(`UPDATE watcher_events SET author_id = '202' WHERE id = 'theirs'`)
	// "ghost" has no author_id, like an actor without a databaseId.
	evs, _, err := readNewEvents(conn, c)
	if err != nil {
		t.Fatal(err)
	}
	if got := newEventIDs(evs); len(got) != 2 || got[0] != "theirs" || got[1] != "ghost" {
		t.Fatalf("got %v, want [theirs ghost]", got)
	}
}
```

(Import `github.com/mturley/worktree/internal/selfid`.)

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test ./internal/webui/ -run SkipsMyOwnEvents`
Expected: FAIL (`mine` is included).

- [ ] **Step 3: Implement**

In `readNewEvents`'s query, after the `e.type NOT IN (…)` condition, add `AND ` + `selfid.NotMineSQL`. Because the user's own events are never read, a mixed batch's count and newest title cover only others' events, with no further change.

- [ ] **Step 4: Run and commit**

Run: `go test ./internal/webui/`
Expected: PASS

```bash
git add internal/webui/notify_scan.go internal/webui/notify_scan_test.go
git commit --signoff -m "feat(notify): never notify for the user's own events" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 10: Unread excludes the user's own events

**Files:**
- Modify: `internal/unread/unread.go` (`Summaries`, `SlackCounts`)
- Modify: `internal/webui/timeline.go` (`unreadOnlyClause`; the global timeline's `SELECT` and `Scan`; the worktree timeline's conversion from `watcher.Event`; `TimelineEvent`)
- Modify: `internal/webui/unread.go` (`unreadIndex` loads identities; `IsUnread` signature)
- Test: `internal/unread/unread_test.go`, `internal/webui/unread_test.go`

**Interfaces:**
- Consumes: `selfid.NotMineSQL`, `selfid.Load`, `selfid.IsMine`; `watcher.Event.AuthorID`.
- Produces: `TimelineEvent.source`/`authorID` (unexported, `json:"-"`); `(*unreadIndex).IsUnread(resType, id, ts, externalTS, source, authorID string) bool`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/unread/unread_test.go` (it has its own DB and event helpers; build on them, and insert with an explicit `author_id`):

```go
func TestSummariesExcludeMyOwnEvents(t *testing.T) {
	conn := testDB(t) // use this file's existing DB helper
	selfid.Set(conn, "github", "101")
	EnsureCursor(conn, "pr", "o/r#1")
	conn.Exec(`UPDATE resource_read_cursor SET last_read_ts = '2026-01-01T00:00:00Z'`)
	ins := func(id, ts, authorID string) {
		conn.Exec(`INSERT INTO watcher_events (id, ts, source, type, title, author_id) VALUES (?, ?, 'github', 'pr_comment', 't', ?)`, id, ts, authorID)
		conn.Exec(`INSERT INTO watcher_event_resources (event_id, resource_type, resource_id) VALUES (?, 'pr', 'o/r#1')`, id)
	}
	ins("theirs", "2026-01-01T00:00:05Z", "202")
	ins("mine", "2026-01-01T00:00:10Z", "101")
	sums, err := Summaries(conn)
	if err != nil {
		t.Fatal(err)
	}
	s := sums[Key("pr", "o/r#1")]
	if s.Count != 1 || s.NewestTS != "2026-01-01T00:00:05Z" {
		t.Fatalf("summary = %+v, want 1 unread through the other person's event", s)
	}
}
```

(If `EnsureCursor` seeds the cursor at "now", the explicit `UPDATE` above moves it back so both events are newer. Adjust to the file's existing helpers if they already do this.)

Append to `internal/webui/unread_test.go`:

```go
func TestIsUnreadIgnoresMyOwnEvents(t *testing.T) {
	conn := unreadTestDB(t)
	selfid.Set(conn, "github", "101")
	ix := &unreadIndex{cursors: map[string]string{unread.Key("pr", "o/r#1"): "2026-01-01T00:00:00Z"},
		mine: map[string]string{"github": "101"}}
	if ix.IsUnread("pr", "o/r#1", "2026-01-01T00:00:05Z", "", "github", "101") {
		t.Fatal("the user's own event must not be unread")
	}
	if !ix.IsUnread("pr", "o/r#1", "2026-01-01T00:00:05Z", "", "github", "202") {
		t.Fatal("another person's newer event must be unread")
	}
	if !ix.IsUnread("pr", "o/r#1", "2026-01-01T00:00:05Z", "", "jira", "101") {
		t.Fatal("the same id under another source is someone else")
	}
}
```

Also extend the existing `TestGlobalTimelineUnreadOnlyMatchesIsUnread` and `TestWorktreeTimelineUnreadOnlyMatchesIsUnread` fixtures. Give one of the unread events `author_id = '101'` with `self_identity(github, 101)` set, and assert that it is absent from the unread-only page and has `unread:false` in the full page. That keeps the SQL and Go implementations pinned together.

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./internal/unread/ ./internal/webui/ -run 'ExcludeMyOwnEvents|IgnoresMyOwnEvents|UnreadOnlyMatchesIsUnread'`
Expected: FAIL (compile error on `mine` / the 6-arg `IsUnread`, and the counts).

- [ ] **Step 3: Implement**

- `internal/unread/unread.go`: in `Summaries` and in `SlackCounts`, add `AND ` + `selfid.NotMineSQL` after the `e.type NOT IN (…)` line. Extend the doc comments: the user's own events (per `internal/selfid`) don't count.
- `internal/webui/timeline.go`:
  - `unreadOnlyClause`: wrap it so the whole clause also requires `selfid.NotMineSQL`, i.e. `AND ( … existing … ) AND ` + `selfid.NotMineSQL` + ` `.
  - `TimelineEvent` gains `authorID string \`json:"-"\``. `Source` is already there.
  - In the global timeline's `base` `SELECT`, add `COALESCE(e.author_id,'')` after `COALESCE(e.author,'')`, and scan it into `&te.authorID` in the same position.
  - In the worktree-timeline path that converts a `watcher.Event`, set `te.authorID = *ev.AuthorID` when non-nil.
  - Change the `IsUnread` call to `e.unread.IsUnread(te.ResourceType, te.ResourceID, te.TS, te.ExternalTS, te.Source, te.authorID)`.
- `internal/webui/unread.go`:
  - `unreadIndex` gains `mine map[string]string`.
  - `newUnreadIndex` loads it with `selfid.Load(s.DB)`, logging and leaving it empty on error.
  - `IsUnread` takes `source, authorID string` and returns false first when `selfid.IsMine(ix.mine, source, authorID)`.
  - Update the four existing `IsUnread` calls in `unread_test.go` to pass `"slack", ""`.

- [ ] **Step 4: Run the whole suite**

Run: `make test`
Expected: PASS, including the two "UnreadOnlyMatchesIsUnread" tests.

- [ ] **Step 5: Commit**

```bash
git add internal/unread/unread.go internal/unread/unread_test.go internal/webui/timeline.go internal/webui/unread.go internal/webui/unread_test.go internal/webui/unread_only_test.go
git commit --signoff -m "feat(unread): the user's own events never count as unread" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 11: Docs, merge, install

**Files:**
- Modify: `.claude/CLAUDE.md` (package list: `selfid`; note the watcher pin is now v0.10.0 where versions are mentioned)
- Modify: `docs/web-ui-architecture.md` ("Unread" and "Notifications" sections)

- [ ] **Step 1: Document**

`.claude/CLAUDE.md`, after the `notifyprefs` bullet:

```markdown
  - `selfid` — the user's own account ID per source (`self_identity`:
    GitHub databaseId, Jira accountId, Slack user ID), resolved by
    `worktree ui` at startup with a 10-minute retry for failures. Every
    unread and notification query appends `selfid.NotMineSQL` so the
    user's own events never count; `IsMine` is its Go twin for the
    per-event unread flag. Requires watcher ≥ v0.10.0
    (`watcher_events.author_id`).
```

`docs/web-ui-architecture.md`:
- In "Unread", add a paragraph. Events whose `(source, author_id)` matches `self_identity` never count as unread and are never flagged unread. The cursor is not moved; this is a query-time filter. `unread_through_ts` therefore covers only others' events. Events with no `author_id` (before watcher v0.10.0, or authorless) always count.
- In "Notifications", add that `readNewEvents` applies the same filter.

- [ ] **Step 2: Verify, commit**

Run: `make test && make build`
Expected: PASS, and `bin/worktree` builds.

```bash
git add .claude/CLAUDE.md docs/web-ui-architecture.md
git commit --signoff -m "docs: document self identity and the not-mine rule" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

- [ ] **Step 3: STOP — ask Mike how to integrate**

Merging to `main`, deleting the worktree and branch, pushing, and installing all need his explicit OK.
