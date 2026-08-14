package github

import (
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/mturley/watcher"
	"github.com/mturley/watcher/db"
	"github.com/mturley/watcher/testutil"
)

// testLogger returns a logger that discards output.
func testLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

// prResource is the resource under test across cases.
var prResource = watcher.Resource{
	Type: "pr",
	ID:   "owner/repo#123",
	URL:  "https://github.com/owner/repo/pull/123",
}

// TestProcessPR_FirstPoll verifies the first poll (no cursor, no backfill)
// emits only a watch_started marker.
func TestProcessPR_FirstPoll(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", prResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	prData := PRData{
		Number:    123,
		Owner:     "owner",
		Repo:      "repo",
		State:     "OPEN",
		Title:     "Test PR",
		UpdatedAt: "2026-06-17T10:00:00Z",
		Reviews: []Review{
			{Author: "reviewer1", AuthorType: "user", State: "APPROVED", SubmittedAt: "2026-06-17T09:00:00Z", Body: "LGTM"},
		},
		Comments: []Comment{
			{Author: "commenter1", AuthorType: "user", CreatedAt: "2026-06-17T08:00:00Z", Body: "Nice work!"},
		},
	}

	n, err := processPR(conn, prData, prResource, "test-token", false, testLogger())
	if err != nil {
		t.Fatalf("processPR: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 event emitted, got %d", n)
	}

	// EventsForResource excludes bookkeeping types (watch_started), so on a
	// first no-backfill poll no consumer-visible events exist.
	events, err := db.EventsForResource(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 consumer-visible events on first poll, got %d: %+v", len(events), events)
	}

	// The cursor should now be set so the next poll processes normally.
	cursor, err := db.EventCursor(conn, "github", "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventCursor: %v", err)
	}
	if cursor == "" {
		t.Error("expected non-empty cursor after watch_started")
	}
}

// TestProcessPR_SubsequentPoll verifies that once a cursor exists, new
// reviews/comments after the cursor are emitted.
func TestProcessPR_SubsequentPoll(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", prResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Establish a cursor at 2026-06-17T08:00:00Z via a first poll.
	seed := PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T08:00:00Z",
	}
	if _, err := processPR(conn, seed, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("seed processPR: %v", err)
	}

	// Now a later approval + comment arrive.
	prData := PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T10:00:00Z",
		Reviews: []Review{
			{Author: "reviewer1", AuthorType: "user", State: "APPROVED", SubmittedAt: "2026-06-17T09:00:00Z", Body: "LGTM"},
		},
		Comments: []Comment{
			{Author: "commenter1", AuthorType: "user", CreatedAt: "2026-06-17T09:30:00Z", Body: "Nice work!"},
		},
	}
	if _, err := processPR(conn, prData, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR: %v", err)
	}

	events, err := db.EventsForResource(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	types := typeCounts(events)
	if types[watcher.EventTypePRApproved] != 1 {
		t.Errorf("expected 1 pr_approved, got %d", types[watcher.EventTypePRApproved])
	}
	if types[watcher.EventTypePRComment] != 1 {
		t.Errorf("expected 1 pr_comment, got %d", types[watcher.EventTypePRComment])
	}
	// Verify author metadata on the approval.
	for _, e := range events {
		if e.Type == watcher.EventTypePRApproved {
			if e.Author == nil || *e.Author != "reviewer1" {
				t.Errorf("expected author reviewer1, got %v", e.Author)
			}
			if e.AuthorType == nil || *e.AuthorType != "user" {
				t.Errorf("expected author_type user, got %v", e.AuthorType)
			}
		}
	}
}

// TestProcessPR_Dedup verifies that re-processing the same PR data does not
// emit duplicate events.
func TestProcessPR_Dedup(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", prResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Seed a cursor via first poll.
	if _, err := processPR(conn, PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T08:00:00Z",
	}, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("seed processPR: %v", err)
	}

	prData := PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T10:00:00Z",
		Reviews: []Review{
			{Author: "reviewer1", AuthorType: "user", State: "APPROVED", SubmittedAt: "2026-06-17T09:00:00Z", Body: "LGTM"},
		},
	}
	// Process twice.
	if _, err := processPR(conn, prData, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR #1: %v", err)
	}
	if _, err := processPR(conn, prData, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR #2: %v", err)
	}

	events, err := db.EventsForResource(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	if got := typeCounts(events)[watcher.EventTypePRApproved]; got != 1 {
		t.Errorf("expected 1 pr_approved after dedup, got %d", got)
	}
}

// TestProcessPR_MergedNoDuplicateAcrossUpdatedAtChange reproduces the
// duplicate "PR merged" bug: prData.UpdatedAt is GitHub's mutable
// updatedAt field, which keeps changing after merge due to unrelated
// activity (comments, label changes, CI finishing). A second poll that
// observes a new UpdatedAt on an already-merged PR must NOT re-emit
// pr_merged.
func TestProcessPR_MergedNoDuplicateAcrossUpdatedAtChange(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", prResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Seed a cursor via first poll while still open.
	if _, err := processPR(conn, PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T08:00:00Z",
	}, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("seed processPR: %v", err)
	}

	// Poll #1: PR is now merged.
	merged := PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "MERGED", Title: "Test PR",
		UpdatedAt: "2026-06-17T10:00:00Z",
	}
	if _, err := processPR(conn, merged, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR #1 (merged): %v", err)
	}

	// Poll #2 (~30 min later): PR is still merged, but UpdatedAt has been
	// bumped by unrelated post-merge activity (e.g. a bot comment).
	mergedLater := merged
	mergedLater.UpdatedAt = "2026-06-17T10:30:00Z"
	if _, err := processPR(conn, mergedLater, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR #2 (merged, later updatedAt): %v", err)
	}

	// Poll #3, for good measure, another 30 min later with yet another
	// UpdatedAt bump.
	mergedEvenLater := merged
	mergedEvenLater.UpdatedAt = "2026-06-17T11:00:00Z"
	if _, err := processPR(conn, mergedEvenLater, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR #3 (merged, even later updatedAt): %v", err)
	}

	events, err := db.EventsForResource(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	if got := typeCounts(events)[watcher.EventTypePRMerged]; got != 1 {
		t.Errorf("expected exactly 1 pr_merged event across 3 polls with changing UpdatedAt, got %d: %+v", got, events)
	}
}

// TestProcessPR_Backfill verifies that a first poll with backfill enabled
// emits all history rather than only a watch_started marker.
func TestProcessPR_Backfill(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", prResource, db.SubscribeOpts{Backfill: true}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	backfill, err := db.BackfillFor(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("BackfillFor: %v", err)
	}
	if !backfill {
		t.Fatal("expected BackfillFor to report true")
	}

	prData := PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T10:00:00Z",
		Reviews: []Review{
			{Author: "reviewer1", AuthorType: "user", State: "APPROVED", SubmittedAt: "2026-06-17T09:00:00Z", Body: "LGTM"},
		},
		Comments: []Comment{
			{Author: "commenter1", AuthorType: "user", CreatedAt: "2026-06-17T08:00:00Z", Body: "Nice work!"},
		},
	}
	if _, err := processPR(conn, prData, prResource, "test-token", backfill, testLogger()); err != nil {
		t.Fatalf("processPR: %v", err)
	}

	events, err := db.EventsForResource(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	types := typeCounts(events)
	if types[watcher.EventTypePRApproved] != 1 {
		t.Errorf("expected 1 pr_approved from backfill, got %d", types[watcher.EventTypePRApproved])
	}
	if types[watcher.EventTypePRComment] != 1 {
		t.Errorf("expected 1 pr_comment from backfill, got %d", types[watcher.EventTypePRComment])
	}
}

// TestProcessPR_BatchReviewComments is a regression test: two review comments
// submitted in the same batch share one createdAt but differ by file path.
// Title-based dedup must let ALL of them through.
func TestProcessPR_BatchReviewComments(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", prResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Seed a cursor before the batch timestamp.
	if _, err := processPR(conn, PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T08:00:00Z",
	}, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("seed processPR: %v", err)
	}

	const sharedTS = "2026-06-17T09:00:00Z"
	prData := PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T10:00:00Z",
		ReviewComments: []ReviewComment{
			{Author: "reviewer1", AuthorType: "user", CreatedAt: sharedTS, Path: "main.go", Body: "fix this"},
			{Author: "reviewer1", AuthorType: "user", CreatedAt: sharedTS, Path: "util.go", Body: "and this"},
			{Author: "reviewer1", AuthorType: "user", CreatedAt: sharedTS, Path: "api.go", Body: "also this"},
		},
	}
	if _, err := processPR(conn, prData, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR: %v", err)
	}

	events, err := db.EventsForResource(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	// All three batch comments (distinct paths -> distinct titles) must emit,
	// despite sharing a single createdAt.
	if got := typeCounts(events)[watcher.EventTypePRReviewComment]; got != 3 {
		t.Errorf("expected 3 pr_review_comment events (title-based dedup), got %d", got)
	}

	// Re-process the same batch: dedup by title should suppress all of them.
	if _, err := processPR(conn, prData, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR #2: %v", err)
	}
	events, err = db.EventsForResource(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	if got := typeCounts(events)[watcher.EventTypePRReviewComment]; got != 3 {
		t.Errorf("expected still 3 pr_review_comment events after re-poll, got %d", got)
	}
}

// TestProcessPR_RepeatReviewCommentSameFile is a regression test for the bug
// where a second review comment by the same author on the same file path was
// permanently suppressed because dedup used the title (author+path) alone.
// CodeRabbit routinely re-comments on a file it has already flagged; the later
// comment has a distinct createdAt and must emit.
func TestProcessPR_RepeatReviewCommentSameFile(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", prResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Seed a cursor before any comments.
	if _, err := processPR(conn, PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-08-10T14:00:00Z",
	}, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("seed processPR: %v", err)
	}

	const path = "packages/foo/Bar.tsx"

	// Poll 1: first comment on the file.
	first := PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-08-10T15:37:12Z",
		ReviewComments: []ReviewComment{
			{Author: "coderabbitai", AuthorType: "bot", CreatedAt: "2026-08-10T15:37:12Z", Path: path, Body: "first pass"},
		},
	}
	if _, err := processPR(conn, first, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR first: %v", err)
	}

	// Poll 2: SECOND comment by the same author on the SAME file, later ts.
	second := PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-08-11T17:35:34Z",
		ReviewComments: []ReviewComment{
			{Author: "coderabbitai", AuthorType: "bot", CreatedAt: "2026-08-11T17:35:34Z", Path: path, Body: "second pass"},
		},
	}
	if _, err := processPR(conn, second, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR second: %v", err)
	}

	events, err := db.EventsForResource(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	// Both comments must emit — same title, distinct createdAt.
	if got := typeCounts(events)[watcher.EventTypePRReviewComment]; got != 2 {
		t.Errorf("expected 2 pr_review_comment events (repeat comment on same file must not be deduped), got %d", got)
	}

	// Re-poll the second batch: same title AND same ts -> must be deduped.
	if _, err := processPR(conn, second, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR second re-poll: %v", err)
	}
	events, err = db.EventsForResource(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	if got := typeCounts(events)[watcher.EventTypePRReviewComment]; got != 2 {
		t.Errorf("expected still 2 pr_review_comment events after re-poll (title+ts dedup), got %d", got)
	}
}

// TestProcessPR_CommentedReview is a regression test for the bug where a review
// submitted with state COMMENTED (as CodeRabbit and other bots do) emitted no
// event at all — only APPROVED and CHANGES_REQUESTED were handled.
func TestProcessPR_CommentedReview(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", prResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Seed a cursor before the review.
	if _, err := processPR(conn, PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-08-11T17:00:00Z",
	}, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("seed processPR: %v", err)
	}

	prData := PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-08-11T17:35:35Z",
		Reviews: []Review{
			{Author: "coderabbitai", AuthorType: "bot", State: "COMMENTED", SubmittedAt: "2026-08-11T17:35:35Z", Body: "Render the missing-configuration state."},
		},
	}
	if _, err := processPR(conn, prData, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR: %v", err)
	}

	events, err := db.EventsForResource(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	if got := typeCounts(events)[watcher.EventTypePRReviewComment]; got != 1 {
		t.Errorf("expected 1 pr_review_comment event for a COMMENTED review, got %d", got)
	}

	// Re-poll: dedup by submittedAt must suppress the repeat.
	if _, err := processPR(conn, prData, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR re-poll: %v", err)
	}
	events, err = db.EventsForResource(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	if got := typeCounts(events)[watcher.EventTypePRReviewComment]; got != 1 {
		t.Errorf("expected still 1 pr_review_comment event after re-poll, got %d", got)
	}
}

// TestProcessPR_CIBundleUpsert is a regression test: processing a PR with CI
// pending for a commit and then again with CI passed for the SAME commit SHA
// must upsert a single ci_* event whose type flips to ci_passed.
func TestProcessPR_CIBundleUpsert(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", prResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Seed a cursor before any CI activity.
	if _, err := processPR(conn, PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T07:00:00Z",
	}, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("seed processPR: %v", err)
	}

	const sha = "abc1234def5678"

	// Poll 1: CI running for the commit — one check completed, one still
	// in progress. A completed check gives the bundle a real timestamp
	// (later polls compare against it), and the pending check makes the
	// bundle type ci_pending.
	pending := PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T09:00:00Z",
		Commits:   CommitInfo{LatestSHA: sha, LatestDate: "2026-06-17T08:00:00Z"},
		CheckRuns: []CheckRun{
			{Name: "lint", Status: "COMPLETED", Conclusion: "SUCCESS", CompletedAt: "2026-06-17T08:30:00Z"},
			{Name: "build", Status: "IN_PROGRESS"},
		},
	}
	if _, err := processPR(conn, pending, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR pending: %v", err)
	}

	events, err := db.EventsForResource(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	if got := ciEventCount(events); got != 1 {
		t.Fatalf("expected 1 CI event after pending poll, got %d", got)
	}
	if typeCounts(events)[watcher.EventTypeCIPending] != 1 {
		t.Errorf("expected the CI event to be ci_pending, got %+v", typeCounts(events))
	}

	// Poll 2: CI passed for the SAME commit SHA.
	passed := PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T11:00:00Z",
		Commits:   CommitInfo{LatestSHA: sha, LatestDate: "2026-06-17T08:00:00Z"},
		CheckRuns: []CheckRun{
			{Name: "lint", Status: "COMPLETED", Conclusion: "SUCCESS", CompletedAt: "2026-06-17T08:30:00Z"},
			{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS", CompletedAt: "2026-06-17T10:00:00Z"},
		},
	}
	if _, err := processPR(conn, passed, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR passed: %v", err)
	}

	events, err = db.EventsForResource(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	// Exactly ONE ci_* event for the resource: upserted in place.
	if got := ciEventCount(events); got != 1 {
		t.Errorf("expected exactly 1 CI event after upsert, got %d", got)
	}
	types := typeCounts(events)
	if types[watcher.EventTypeCIPassed] != 1 {
		t.Errorf("expected the CI event type to flip to ci_passed, got %+v", types)
	}
	if types[watcher.EventTypeCIPending] != 0 {
		t.Errorf("expected no lingering ci_pending event, got %d", types[watcher.EventTypeCIPending])
	}
}

// TestProcessPR_PendingCIBundleRefreshesOnSetChange is a regression test for
// GitHub issue #2: a purely-pending CI bundle (no completed checks) must be
// recorded on first poll, must NOT be refreshed on a later poll where the
// pending set is unchanged (to avoid churn), and MUST be refreshed in place
// when the pending set changes.
func TestProcessPR_PendingCIBundleRefreshesOnSetChange(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", prResource, db.SubscribeOpts{Backfill: true}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	const sha = "deadbeef1234"

	// Poll 1: purely-pending CI (no completed checks at all) on the very
	// first poll for this resource (cursor == "", backfill enabled so
	// processing falls through instead of short-circuiting to
	// watch_started), so the existing "cursor == ''" trigger records the
	// bundle — this is the "first/backfill poll" the issue describes.
	pending1 := PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T09:00:00Z",
		Commits:   CommitInfo{LatestSHA: sha, LatestDate: "2026-06-17T08:00:00Z"},
		CheckRuns: []CheckRun{
			{Name: "build", Status: "IN_PROGRESS"},
			{Name: "test", Status: "QUEUED"},
		},
	}
	if _, err := processPR(conn, pending1, prResource, "test-token", true, testLogger()); err != nil {
		t.Fatalf("processPR pending1: %v", err)
	}

	events, err := db.EventsForResource(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	if got := ciEventCount(events); got != 1 {
		t.Fatalf("expected 1 CI event after first pending poll, got %d", got)
	}
	types := typeCounts(events)
	if types[watcher.EventTypeCIPending] != 1 {
		t.Fatalf("expected ci_pending after first poll, got %+v", types)
	}
	firstTitle := findCIEventTitle(t, events)

	// Directly mutate the stored bundle's title in the DB to a sentinel
	// value before poll 2. This makes the no-churn assertion genuinely
	// discriminating: comparing ts (second-resolution, same-second polls)
	// or comparing recomputed title/body (identical inputs recompute to
	// the identical string either way) can't tell "skipped" from "regressed
	// to always-refresh" apart. But if poll 2 calls UpsertCIBundle at all,
	// it will overwrite the sentinel with the freshly computed title — so
	// asserting the sentinel survives poll 2 proves UpsertCIBundle did NOT
	// run, i.e. the pendingChanged guard correctly skipped it.
	const sentinelTitle = "SENTINEL-DO-NOT-OVERWRITE"
	sentinelizeCIBundle(t, conn, sha, sentinelTitle)

	// Poll 2: SAME pending set for the same commit. hasNewChecks is false
	// (nothing completed), cursor is no longer empty, and the pending
	// fingerprint is unchanged, so the bundle must NOT be refreshed.
	pending2 := PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T09:05:00Z",
		Commits:   CommitInfo{LatestSHA: sha, LatestDate: "2026-06-17T08:00:00Z"},
		CheckRuns: []CheckRun{
			{Name: "build", Status: "IN_PROGRESS"},
			{Name: "test", Status: "QUEUED"},
		},
	}
	if _, err := processPR(conn, pending2, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR pending2: %v", err)
	}

	events, err = db.EventsForResource(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	if got := ciEventCount(events); got != 1 {
		t.Fatalf("expected still 1 CI event after unchanged pending poll, got %d", got)
	}
	if got := findCIEventTitle(t, events); got != sentinelTitle {
		t.Errorf("expected unchanged pending set to NOT refresh the bundle (title should still be sentinel %q), got %q — UpsertCIBundle ran when it shouldn't have", sentinelTitle, got)
	}

	// Poll 3: CHANGED pending set (a new check "lint" appears) for the same
	// commit. The pending fingerprint now differs from what was cached, so
	// the bundle MUST be refreshed in place: still exactly one CI event for
	// the resource (UPDATE, not INSERT), but its title/body reflect the new
	// set.
	pending3 := PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T09:10:00Z",
		Commits:   CommitInfo{LatestSHA: sha, LatestDate: "2026-06-17T08:00:00Z"},
		CheckRuns: []CheckRun{
			{Name: "build", Status: "IN_PROGRESS"},
			{Name: "test", Status: "QUEUED"},
			{Name: "lint", Status: "IN_PROGRESS"},
		},
	}
	if _, err := processPR(conn, pending3, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR pending3: %v", err)
	}

	events, err = db.EventsForResource(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	// Still exactly one CI bundle event: the change must UPDATE the existing
	// row (via UpsertCIBundle's match-by-commit-tag), not insert a new one.
	if got := ciEventCount(events); got != 1 {
		t.Fatalf("expected still 1 CI event after changed pending poll (updated in place), got %d", got)
	}
	types = typeCounts(events)
	if types[watcher.EventTypeCIPending] != 1 {
		t.Fatalf("expected the refreshed bundle to still be ci_pending, got %+v", types)
	}
	newTitle := findCIEventTitle(t, events)
	newBody := findCIEventBody(t, events)
	if newTitle == sentinelTitle {
		t.Errorf("expected the changed pending set to overwrite the sentinel title, but it survived: %q — UpsertCIBundle did not run when it should have", newTitle)
	}
	if newTitle == firstTitle {
		t.Errorf("expected the title to reflect the new pending set (3/3 pending vs 2/2), got unchanged title %q", newTitle)
	}
	if !strings.Contains(newBody, "lint") {
		t.Errorf("expected the refreshed body to mention the new pending check %q, got %q", "lint", newBody)
	}
	// Note: we don't assert that ts advanced here — UpsertCIBundle stamps
	// ts with second-resolution RFC3339, so two polls in the same test run
	// can legitimately land in the same second. The title content change
	// above (overwriting the sentinel) is sufficient proof that
	// UpsertCIBundle ran its UPDATE path again.
}

// sentinelizeCIBundle directly overwrites the title of the CI bundle event
// for the given commit SHA on prResource, bypassing UpsertCIBundle. This
// gives the no-churn test a real discriminator: if a later poll calls
// UpsertCIBundle at all, it will overwrite this sentinel back to a
// freshly computed title. Comparing recomputed values (ts, or title/body
// derived from identical inputs) can't distinguish "skipped" from
// "regressed to always-refresh", but the sentinel can.
func sentinelizeCIBundle(t *testing.T, conn *sql.DB, commitSHA, sentinelTitle string) {
	t.Helper()
	tag := "commit:" + commitSHA
	res, err := conn.Exec(`
		UPDATE watcher_events
		SET title = ?
		WHERE id IN (
			SELECT e.id FROM watcher_events e
			JOIN watcher_event_resources er ON e.id = er.event_id
			WHERE e.source = 'github'
			  AND e.type IN ('ci_passed', 'ci_failed', 'ci_pending', 'ci_partial_failure')
			  AND er.resource_type = ? AND er.resource_id = ?
			  AND e.tags = ?
		)
	`, sentinelTitle, prResource.Type, prResource.ID, tag)
	if err != nil {
		t.Fatalf("sentinelizeCIBundle: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("sentinelizeCIBundle RowsAffected: %v", err)
	}
	if n != 1 {
		t.Fatalf("sentinelizeCIBundle: expected to update exactly 1 row, updated %d", n)
	}
}

// findCIEventTitle returns the title of the (single) CI bundle event.
func findCIEventTitle(t *testing.T, events []watcher.Event) string {
	t.Helper()
	for _, e := range events {
		if isCIEventType(e.Type) {
			return e.Title
		}
	}
	t.Fatal("no CI bundle event found")
	return ""
}

// findCIEventBody returns the body of the (single) CI bundle event.
func findCIEventBody(t *testing.T, events []watcher.Event) string {
	t.Helper()
	for _, e := range events {
		if isCIEventType(e.Type) {
			if e.Body == nil {
				return ""
			}
			return *e.Body
		}
	}
	t.Fatal("no CI bundle event found")
	return ""
}

func isCIEventType(t watcher.EventType) bool {
	switch t {
	case watcher.EventTypeCIPassed, watcher.EventTypeCIFailed, watcher.EventTypeCIPending, watcher.EventTypeCIPartialFailure:
		return true
	}
	return false
}

// typeCounts tallies events by type.
func typeCounts(events []watcher.Event) map[watcher.EventType]int {
	out := make(map[watcher.EventType]int)
	for _, e := range events {
		out[e.Type]++
	}
	return out
}

// ciEventCount counts events whose type is one of the CI bundle types.
func ciEventCount(events []watcher.Event) int {
	n := 0
	for _, e := range events {
		switch e.Type {
		case watcher.EventTypeCIPassed, watcher.EventTypeCIFailed, watcher.EventTypeCIPending, watcher.EventTypeCIPartialFailure:
			n++
		}
	}
	return n
}

// TestBuildPRStateJSON_Author verifies the cached PR state JSON includes the
// PR author's login, so downstream consumers (e.g. the worktree UI) can
// display who opened the PR.
func TestBuildPRStateJSON_Author(t *testing.T) {
	prData := PRData{
		Number:     123,
		Owner:      "owner",
		Repo:       "repo",
		State:      "OPEN",
		Title:      "Test PR",
		Author:     "alice",
		AuthorType: "User",
		UpdatedAt:  "2024-01-01T00:00:00Z",
	}

	stateJSON := buildPRStateJSON(prData)

	var state map[string]interface{}
	if err := json.Unmarshal([]byte(stateJSON), &state); err != nil {
		t.Fatalf("failed to unmarshal state JSON: %v", err)
	}

	if got, want := state["author"], "alice"; got != want {
		t.Errorf("state[\"author\"] = %v, want %v", got, want)
	}
}
