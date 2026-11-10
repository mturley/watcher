package github

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
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
	// Note: we don't assert on ts here — a pending→pending refresh keeps the
	// bundle's type, so UpsertCIBundle deliberately leaves ts alone. The
	// title content change above (overwriting the sentinel) is sufficient
	// proof that UpsertCIBundle ran its UPDATE path again.
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

// wfEventCount counts events whose type is one of the workflow bundle types.
func wfEventCount(events []watcher.Event) int {
	n := 0
	for _, e := range events {
		switch e.Type {
		case watcher.EventTypeCIWorkflowsPassed, watcher.EventTypeCIWorkflowsFailed,
			watcher.EventTypeCIWorkflowsPending, watcher.EventTypeCIWorkflowsPartialFailure:
			n++
		}
	}
	return n
}

// findEventTitleByType returns the title of the first event with the given type.
func findEventTitleByType(t *testing.T, events []watcher.Event, typ watcher.EventType) string {
	t.Helper()
	for _, e := range events {
		if e.Type == typ {
			return e.Title
		}
	}
	t.Fatalf("no event of type %q found", typ)
	return ""
}

// TestProcessPR_DualBundlesCoexist reproduces the PR #9393 bug: all CheckRuns
// pass while a gated workflow (StatusContext) is still PENDING. The CheckRun
// bundle must report "CI checks passed" AND a separate workflow bundle must
// report the pending gate — both tagged commit:<sha> as distinct rows.
func TestProcessPR_DualBundlesCoexist(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", prResource, db.SubscribeOpts{Backfill: true}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	const sha = "966c7e8abc123"
	data := PRData{
		Number: 9393, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T09:00:00Z",
		Commits:   CommitInfo{LatestSHA: sha, LatestDate: "2026-06-17T08:00:00Z"},
		CheckRuns: []CheckRun{
			{Name: "lint", Status: "COMPLETED", Conclusion: "SUCCESS", CompletedAt: "2026-06-17T08:30:00Z"},
			{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS", CompletedAt: "2026-06-17T08:40:00Z"},
		},
		StatusContexts: []StatusContext{
			{Name: "Cypress E2E Tests", State: "PENDING"},
			{Name: "ci/prow/images", State: "SUCCESS"},
		},
	}
	if _, err := processPR(conn, data, prResource, "test-token", true, testLogger()); err != nil {
		t.Fatalf("processPR: %v", err)
	}

	events, err := db.EventsForResource(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	if got := ciEventCount(events); got != 1 {
		t.Errorf("expected 1 CheckRun bundle, got %d", got)
	}
	if got := wfEventCount(events); got != 1 {
		t.Errorf("expected 1 workflow bundle, got %d", got)
	}
	types := typeCounts(events)
	if types[watcher.EventTypeCIPassed] != 1 {
		t.Errorf("expected ci_passed (checks passed), got %+v", types)
	}
	if types[watcher.EventTypeCIWorkflowsPending] != 1 {
		t.Errorf("expected ci_workflows_pending, got %+v", types)
	}
	if got := findEventTitleByType(t, events, watcher.EventTypeCIPassed); !strings.Contains(got, "CI checks passed") {
		t.Errorf("CheckRun bundle title = %q, want 'CI checks passed'", got)
	}
	wfTitle := findEventTitleByType(t, events, watcher.EventTypeCIWorkflowsPending)
	if !strings.Contains(wfTitle, "Workflows running") {
		t.Errorf("workflow bundle title = %q, want 'Workflows running'", wfTitle)
	}
}

// TestProcessPR_WorkflowBundleRefreshesOnStateChange verifies the workflow
// bundle is recorded on first poll, not churned when the StatusContext set is
// unchanged, and refreshed in place when a context's state transitions (even
// with no change to what's pending).
func TestProcessPR_WorkflowBundleRefreshesOnStateChange(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", prResource, db.SubscribeOpts{Backfill: true}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	const sha = "wfsha123456"
	base := func(updatedAt string, ctxs []StatusContext) PRData {
		return PRData{
			Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
			UpdatedAt: updatedAt,
			Commits:   CommitInfo{LatestSHA: sha, LatestDate: "2026-06-17T08:00:00Z"},
			// A completed CheckRun keeps the check bundle out of the way and
			// gives processing a real cursor.
			CheckRuns:      []CheckRun{{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS", CompletedAt: "2026-06-17T08:30:00Z"}},
			StatusContexts: ctxs,
		}
	}

	// Poll 1: E2E pending.
	if _, err := processPR(conn, base("2026-06-17T09:00:00Z", []StatusContext{{Name: "Cypress E2E Tests", State: "PENDING"}}), prResource, "test-token", true, testLogger()); err != nil {
		t.Fatalf("poll1: %v", err)
	}
	events, _ := db.EventsForResource(conn, "pr", prResource.ID)
	if got := wfEventCount(events); got != 1 {
		t.Fatalf("expected 1 workflow bundle after poll1, got %d", got)
	}

	// Sentinel the workflow bundle so we can detect whether poll 2 rewrote it.
	const sentinel = "SENTINEL-WF-DO-NOT-OVERWRITE"
	sentinelizeWorkflowBundle(t, conn, sha, sentinel)

	// Poll 2: SAME context set → must not refresh (sentinel survives).
	if _, err := processPR(conn, base("2026-06-17T09:05:00Z", []StatusContext{{Name: "Cypress E2E Tests", State: "PENDING"}}), prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("poll2: %v", err)
	}
	events, _ = db.EventsForResource(conn, "pr", prResource.ID)
	if got := findEventTitleByType(t, events, watcher.EventTypeCIWorkflowsPending); got != sentinel {
		t.Errorf("unchanged context set refreshed the bundle: title = %q, want sentinel", got)
	}

	// Poll 3: E2E flips PENDING→SUCCESS → must refresh in place.
	if _, err := processPR(conn, base("2026-06-17T09:10:00Z", []StatusContext{{Name: "Cypress E2E Tests", State: "SUCCESS"}}), prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("poll3: %v", err)
	}
	events, _ = db.EventsForResource(conn, "pr", prResource.ID)
	if got := wfEventCount(events); got != 1 {
		t.Fatalf("expected still 1 workflow bundle after poll3, got %d", got)
	}
	if typeCounts(events)[watcher.EventTypeCIWorkflowsPassed] != 1 {
		t.Errorf("expected workflow bundle to flip to ci_workflows_passed, got %+v", typeCounts(events))
	}
}

// TestProcessPR_MarksPrevCommitOutOfDate verifies that when a new commit
// appears, the previous commit's bundles get the [out of date] title prefix
// (both families), the new commit's do not, and re-polling doesn't double-prefix.
func TestProcessPR_MarksPrevCommitOutOfDate(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", prResource, db.SubscribeOpts{Backfill: true}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	const shaA = "commitAAAA111"
	const shaB = "commitBBBB222"

	// Poll commit A: both a CheckRun bundle and a workflow bundle.
	dataA := PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T09:00:00Z",
		Commits:   CommitInfo{LatestSHA: shaA, LatestDate: "2026-06-17T08:00:00Z"},
		CheckRuns: []CheckRun{{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS", CompletedAt: "2026-06-17T08:30:00Z"}},
		StatusContexts: []StatusContext{
			{Name: "Cypress E2E Tests", State: "SUCCESS"},
		},
	}
	if _, err := processPR(conn, dataA, prResource, "test-token", true, testLogger()); err != nil {
		t.Fatalf("poll A: %v", err)
	}

	// Poll commit B: new latest SHA.
	dataB := PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T10:00:00Z",
		Commits:   CommitInfo{LatestSHA: shaB, LatestDate: "2026-06-17T09:30:00Z", Recent: []CommitEntry{{SHA: shaB, MessageHeadline: "new work"}}},
		CheckRuns: []CheckRun{{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS", CompletedAt: "2026-06-17T09:45:00Z"}},
		StatusContexts: []StatusContext{
			{Name: "Cypress E2E Tests", State: "PENDING"},
		},
	}
	if _, err := processPR(conn, dataB, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("poll B: %v", err)
	}

	titlesFor := func(commitSHA string) []string {
		t.Helper()
		rows, err := conn.Query(`
			SELECT e.title FROM watcher_events e
			JOIN watcher_event_resources er ON e.id = er.event_id
			WHERE er.resource_id = ? AND e.tags = ?`, prResource.ID, "commit:"+commitSHA)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}

	aTitles := titlesFor(shaA)
	if len(aTitles) != 2 {
		t.Fatalf("expected 2 bundles for commit A, got %d: %v", len(aTitles), aTitles)
	}
	for _, tt := range aTitles {
		if !strings.HasPrefix(tt, "[out of date] ") {
			t.Errorf("commit A bundle not marked out of date: %q", tt)
		}
	}
	for _, tt := range titlesFor(shaB) {
		if strings.HasPrefix(tt, "[out of date] ") {
			t.Errorf("commit B bundle should NOT be out of date: %q", tt)
		}
	}

	// Re-poll B (SHA unchanged from stored state) → A must not double-prefix.
	if _, err := processPR(conn, dataB, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("re-poll B: %v", err)
	}
	for _, tt := range titlesFor(shaA) {
		if strings.HasPrefix(tt, "[out of date] [out of date] ") {
			t.Errorf("commit A bundle double-prefixed: %q", tt)
		}
	}
}

// sentinelizeWorkflowBundle overwrites the workflow bundle's title for a commit,
// mirroring sentinelizeCIBundle but for the ci_workflows_* family.
func sentinelizeWorkflowBundle(t *testing.T, conn *sql.DB, commitSHA, sentinelTitle string) {
	t.Helper()
	tag := "commit:" + commitSHA
	res, err := conn.Exec(`
		UPDATE watcher_events
		SET title = ?
		WHERE id IN (
			SELECT e.id FROM watcher_events e
			JOIN watcher_event_resources er ON e.id = er.event_id
			WHERE e.source = 'github'
			  AND e.type IN ('ci_workflows_passed', 'ci_workflows_failed', 'ci_workflows_pending', 'ci_workflows_partial_failure')
			  AND er.resource_type = ? AND er.resource_id = ?
			  AND e.tags = ?
		)
	`, sentinelTitle, prResource.Type, prResource.ID, tag)
	if err != nil {
		t.Fatalf("sentinelizeWorkflowBundle: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("sentinelizeWorkflowBundle RowsAffected: %v", err)
	}
	if n != 1 {
		t.Fatalf("sentinelizeWorkflowBundle: expected to update exactly 1 row, updated %d", n)
	}
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
		AuthorID:   "42",
		CreatedAt:  "2023-12-01T00:00:00Z",
		UpdatedAt:  "2024-01-01T00:00:00Z",
	}

	stateJSON := buildPRStateJSON(prData, "", "")

	var state map[string]interface{}
	if err := json.Unmarshal([]byte(stateJSON), &state); err != nil {
		t.Fatalf("failed to unmarshal state JSON: %v", err)
	}

	if got, want := state["author"], "alice"; got != want {
		t.Errorf("state[\"author\"] = %v, want %v", got, want)
	}
	// Consumers match author_id against the viewer to tell "my PRs" apart,
	// and sort by created_at.
	if got, want := state["author_id"], "42"; got != want {
		t.Errorf("state[\"author_id\"] = %v, want %v", got, want)
	}
	if got, want := state["created_at"], "2023-12-01T00:00:00Z"; got != want {
		t.Errorf("state[\"created_at\"] = %v, want %v", got, want)
	}
}

func TestParsePRNode_CreatedAt(t *testing.T) {
	got := parsePRNode(&prNode{Number: 1, CreatedAt: "2023-12-01T00:00:00Z"}, "o", "r")
	if got.CreatedAt != "2023-12-01T00:00:00Z" {
		t.Errorf("CreatedAt = %q", got.CreatedAt)
	}
}

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
	// Twice: the first poll only emits watch_started and returns before it
	// stores the head SHA that the next poll compares against.
	for i := 0; i < 2; i++ {
		if _, err := processPR(conn, seed, prResource, "t", false, testLogger()); err != nil {
			t.Fatal(err)
		}
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

// TestProcessPR_ReviewCommentTag: a review comment event carries its GitHub
// database id as a "comment:<id>" tag; one without a known id has no tag.
func TestProcessPR_ReviewCommentTag(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", prResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := processPR(conn, PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T08:00:00Z",
	}, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("seed processPR: %v", err)
	}
	if _, err := processPR(conn, PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T10:00:00Z",
		ReviewComments: []ReviewComment{
			{Author: "r", AuthorType: "user", CreatedAt: "2026-06-17T09:00:00Z", Path: "a.go", Body: "x", DatabaseID: 4242},
			{Author: "r", AuthorType: "user", CreatedAt: "2026-06-17T09:00:00Z", Path: "b.go", Body: "y"},
		},
	}, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR: %v", err)
	}
	events, err := db.EventsForResourceByType(conn, "pr", prResource.ID, string(watcher.EventTypePRReviewComment))
	if err != nil || len(events) != 2 {
		t.Fatalf("events = %d, err = %v", len(events), err)
	}
	tags := map[string]*string{}
	for _, e := range events {
		tags[e.Title] = e.Tags
	}
	if p := tags["Review comment by r on a.go"]; p == nil || *p != "comment:4242" {
		t.Errorf("a.go tags = %v, want comment:4242", p)
	}
	if p := tags["Review comment by r on b.go"]; p != nil {
		t.Errorf("b.go tags = %q, want nil", *p)
	}
}

func TestPRReviewDecision(t *testing.T) {
	rev := func(author, state, at string) Review {
		return Review{Author: author, State: state, SubmittedAt: at}
	}
	tests := []struct {
		name string
		pr   PRData
		want string
	}{
		{"GitHub's decision wins", PRData{ReviewDecision: "REVIEW_REQUIRED", Reviews: []Review{rev("a", "APPROVED", "1")}}, "REVIEW_REQUIRED"},
		{"no reviews", PRData{}, "NONE"},
		{"comment only is not review required", PRData{Reviews: []Review{rev("a", "COMMENTED", "1")}}, "NONE"},
		{"approval plus another's comment", PRData{Reviews: []Review{rev("a", "APPROVED", "1"), rev("b", "COMMENTED", "2")}}, "APPROVED"},
		{"later comment keeps approval", PRData{Reviews: []Review{rev("a", "APPROVED", "1"), rev("a", "COMMENTED", "2")}}, "APPROVED"},
		{"any changes requested", PRData{Reviews: []Review{rev("a", "APPROVED", "1"), rev("b", "CHANGES_REQUESTED", "2")}}, "CHANGES_REQUESTED"},
		{"re-approval clears changes requested", PRData{Reviews: []Review{rev("a", "CHANGES_REQUESTED", "1"), rev("a", "APPROVED", "2")}}, "APPROVED"},
		{"dismissed approval", PRData{Reviews: []Review{rev("a", "DISMISSED", "1")}}, "NONE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := prReviewDecision(tt.pr); got != tt.want {
				t.Errorf("prReviewDecision() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParsePRNode_ReviewDecision(t *testing.T) {
	d := "CHANGES_REQUESTED"
	if got := parsePRNode(&prNode{ReviewDecision: &d}, "o", "r"); got.ReviewDecision != d {
		t.Errorf("ReviewDecision = %q, want %q", got.ReviewDecision, d)
	}
	if got := parsePRNode(&prNode{}, "o", "r"); got.ReviewDecision != "" {
		t.Errorf("null reviewDecision = %q, want empty", got.ReviewDecision)
	}
}

func TestBuildPRStateJSONRequestedReviewers(t *testing.T) {
	got := buildPRStateJSON(PRData{RequestedReviewerIDs: []string{"123", "456"}}, "", "")
	if !strings.Contains(got, `"requested_reviewer_ids":["123","456"]`) {
		t.Errorf("missing requested ids: %s", got)
	}
	got = buildPRStateJSON(PRData{}, "", "")
	if !strings.Contains(got, `"requested_reviewer_ids":[]`) {
		t.Errorf("nil should marshal as []: %s", got)
	}
}

func TestParsePRNodeRequestedReviewers(t *testing.T) {
	var node prNode
	raw := `{"number":1,"reviewRequests":{"nodes":[{"requestedReviewer":{"databaseId":123}},{"requestedReviewer":{}},{"requestedReviewer":null}]}}`
	if err := json.Unmarshal([]byte(raw), &node); err != nil {
		t.Fatal(err)
	}
	got := parsePRNode(&node, "o", "r").RequestedReviewerIDs
	if len(got) != 1 || got[0] != "123" {
		t.Errorf("got %v, want [123] (team/unknown reviewers skipped)", got)
	}
}

func TestResolveMergeable(t *testing.T) {
	tests := []struct {
		name string
		prev map[string]interface{}
		in   string
		want string
	}{
		{"fresh conflicting", nil, "CONFLICTING", "CONFLICTING"},
		{"fresh mergeable", nil, "MERGEABLE", "MERGEABLE"},
		{"unknown, no prev", nil, "UNKNOWN", ""},
		{"unknown, same head", map[string]interface{}{"mergeable": "CONFLICTING", "mergeable_sha": "sha2"}, "UNKNOWN", "CONFLICTING"},
		{"unknown, head moved", map[string]interface{}{"mergeable": "CONFLICTING", "mergeable_sha": "sha1"}, "UNKNOWN", ""},
		{"empty treated as unknown", map[string]interface{}{"mergeable": "MERGEABLE", "mergeable_sha": "sha2"}, "", "MERGEABLE"},
		{"legacy state", map[string]interface{}{"latest_commit_sha": "sha2"}, "UNKNOWN", ""},
		{"wrong types", map[string]interface{}{"mergeable": 1, "mergeable_sha": 2}, "UNKNOWN", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pd := PRData{Mergeable: tt.in, Commits: CommitInfo{LatestSHA: "sha2"}}
			if got := resolveMergeable(tt.prev, pd); got != tt.want {
				t.Errorf("resolveMergeable() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildPRStateJSON_Mergeable(t *testing.T) {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(buildPRStateJSON(PRData{Commits: CommitInfo{LatestSHA: "sha2"}}, "CONFLICTING", "")), &m); err != nil {
		t.Fatal(err)
	}
	if m["mergeable"] != "CONFLICTING" || m["mergeable_sha"] != "sha2" {
		t.Errorf("got mergeable=%v mergeable_sha=%v", m["mergeable"], m["mergeable_sha"])
	}
}

// needsRebaseSeed subscribes prResource and seeds the event cursor with a
// first (watch_started) poll, so later polls process normally.
func needsRebaseSeed(t *testing.T) *sql.DB {
	t.Helper()
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", prResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := processPR(conn, PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: "OPEN", Title: "Test PR",
		UpdatedAt: "2026-06-17T07:00:00Z",
	}, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("seed processPR: %v", err)
	}
	return conn
}

const (
	nrSHA1  = "85a4638deadbeef"
	nrSHA2  = "99b7777cafef00d"
	nrDate1 = "2026-06-17T08:00:00Z"
	nrDate2 = "2026-06-17T12:00:00Z"
)

// needsRebasePoll runs one poll of an OPEN PR at the given head and mergeable.
func needsRebasePoll(t *testing.T, conn *sql.DB, state, sha, date, mergeable, updatedAt string) {
	t.Helper()
	if _, err := processPR(conn, PRData{
		Number: 123, Owner: "owner", Repo: "repo", State: state, Title: "Test PR",
		UpdatedAt: updatedAt,
		Mergeable: mergeable,
		Commits:   CommitInfo{LatestSHA: sha, LatestDate: date},
	}, prResource, "test-token", false, testLogger()); err != nil {
		t.Fatalf("processPR: %v", err)
	}
}

func needsRebaseEvents(t *testing.T, conn *sql.DB) []watcher.Event {
	t.Helper()
	events, err := db.EventsForResource(conn, "pr", prResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	var out []watcher.Event
	for _, e := range events {
		if e.Type == watcher.EventTypePRNeedsRebase {
			out = append(out, e)
		}
	}
	return out
}

func cachedMergeable(t *testing.T, conn *sql.DB) string {
	t.Helper()
	rs, err := db.GetResourceState(conn, "pr", prResource.ID)
	if err != nil || rs == nil {
		t.Fatalf("GetResourceState: %v (nil=%v)", err, rs == nil)
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(rs.StateJSON), &m); err != nil {
		t.Fatal(err)
	}
	s, _ := m["mergeable"].(string)
	return s
}

const (
	nrTitle1 = "Needs rebase: PR #123 @ 85a4638"
	nrTitle2 = "Needs rebase: PR #123 @ 99b7777"
	nrStale  = "[out of date] "
)

func TestProcessPR_NeedsRebase_Emits(t *testing.T) {
	conn := needsRebaseSeed(t)
	needsRebasePoll(t, conn, "OPEN", nrSHA1, nrDate1, "CONFLICTING", "2026-06-17T09:00:00Z")

	evs := needsRebaseEvents(t, conn)
	if len(evs) != 1 {
		t.Fatalf("expected 1 pr_needs_rebase, got %d", len(evs))
	}
	e := evs[0]
	if e.Title != nrTitle1 {
		t.Errorf("title = %q, want %q", e.Title, nrTitle1)
	}
	if e.Tags == nil || *e.Tags != "commit:"+nrSHA1 {
		t.Errorf("tags = %v, want commit:%s", e.Tags, nrSHA1)
	}
	if e.ExternalTS == nil || *e.ExternalTS != nrDate1 {
		t.Errorf("external_ts = %v, want %s", e.ExternalTS, nrDate1)
	}
	if e.Author != nil {
		t.Errorf("author = %q, want nil", *e.Author)
	}
	if e.Body == nil || *e.Body != "PR #123: Test PR" {
		t.Errorf("body = %v", e.Body)
	}
}

func TestProcessPR_NeedsRebase_NoRepeatSameHead(t *testing.T) {
	conn := needsRebaseSeed(t)
	needsRebasePoll(t, conn, "OPEN", nrSHA1, nrDate1, "CONFLICTING", "2026-06-17T09:00:00Z")
	needsRebasePoll(t, conn, "OPEN", nrSHA1, nrDate1, "CONFLICTING", "2026-06-17T10:00:00Z")
	needsRebasePoll(t, conn, "OPEN", nrSHA1, nrDate1, "CONFLICTING", "2026-06-17T11:00:00Z")
	if got := len(needsRebaseEvents(t, conn)); got != 1 {
		t.Errorf("expected 1 pr_needs_rebase, got %d", got)
	}
}

func TestProcessPR_NeedsRebase_UnknownIsNoChange(t *testing.T) {
	conn := needsRebaseSeed(t)
	needsRebasePoll(t, conn, "OPEN", nrSHA1, nrDate1, "CONFLICTING", "2026-06-17T09:00:00Z")
	needsRebasePoll(t, conn, "OPEN", nrSHA1, nrDate1, "UNKNOWN", "2026-06-17T10:00:00Z")
	evs := needsRebaseEvents(t, conn)
	if len(evs) != 1 {
		t.Fatalf("expected 1 pr_needs_rebase, got %d", len(evs))
	}
	if evs[0].Title != nrTitle1 {
		t.Errorf("title = %q, want un-prefixed %q", evs[0].Title, nrTitle1)
	}
	if got := cachedMergeable(t, conn); got != "CONFLICTING" {
		t.Errorf("cached mergeable = %q, want CONFLICTING", got)
	}
}

func TestProcessPR_NeedsRebase_ClearedMarksOutOfDate(t *testing.T) {
	conn := needsRebaseSeed(t)
	needsRebasePoll(t, conn, "OPEN", nrSHA1, nrDate1, "CONFLICTING", "2026-06-17T09:00:00Z")
	before := needsRebaseEvents(t, conn)
	if len(before) != 1 {
		t.Fatalf("expected 1 pr_needs_rebase, got %d", len(before))
	}
	needsRebasePoll(t, conn, "OPEN", nrSHA1, nrDate1, "MERGEABLE", "2026-06-17T10:00:00Z")
	evs := needsRebaseEvents(t, conn)
	if len(evs) != 1 {
		t.Fatalf("expected 1 pr_needs_rebase, got %d", len(evs))
	}
	if want := nrStale + nrTitle1; evs[0].Title != want {
		t.Errorf("title = %q, want %q", evs[0].Title, want)
	}
	if evs[0].TS != before[0].TS {
		t.Errorf("ts changed: %q -> %q", before[0].TS, evs[0].TS)
	}
}

func TestProcessPR_NeedsRebase_PushThenUnknown(t *testing.T) {
	conn := needsRebaseSeed(t)
	needsRebasePoll(t, conn, "OPEN", nrSHA1, nrDate1, "CONFLICTING", "2026-06-17T09:00:00Z")
	needsRebasePoll(t, conn, "OPEN", nrSHA2, nrDate2, "UNKNOWN", "2026-06-17T13:00:00Z")
	evs := needsRebaseEvents(t, conn)
	if len(evs) != 1 {
		t.Fatalf("expected only the sha1 pr_needs_rebase, got %d", len(evs))
	}
	if want := nrStale + nrTitle1; evs[0].Title != want {
		t.Errorf("title = %q, want %q", evs[0].Title, want)
	}
	if got := cachedMergeable(t, conn); got != "" {
		t.Errorf("cached mergeable = %q, want empty", got)
	}
}

func TestProcessPR_NeedsRebase_PushStillConflicting(t *testing.T) {
	conn := needsRebaseSeed(t)
	needsRebasePoll(t, conn, "OPEN", nrSHA1, nrDate1, "CONFLICTING", "2026-06-17T09:00:00Z")
	needsRebasePoll(t, conn, "OPEN", nrSHA2, nrDate2, "CONFLICTING", "2026-06-17T13:00:00Z")
	evs := needsRebaseEvents(t, conn)
	if len(evs) != 2 {
		t.Fatalf("expected 2 pr_needs_rebase, got %d", len(evs))
	}
	titles := map[string]bool{}
	for _, e := range evs {
		titles[e.Title] = true
	}
	if !titles[nrStale+nrTitle1] || !titles[nrTitle2] {
		t.Errorf("titles = %v, want %q and %q", titles, nrStale+nrTitle1, nrTitle2)
	}
}

func TestProcessPR_NeedsRebase_ReconflictSameHead(t *testing.T) {
	conn := needsRebaseSeed(t)
	needsRebasePoll(t, conn, "OPEN", nrSHA1, nrDate1, "CONFLICTING", "2026-06-17T09:00:00Z")
	needsRebasePoll(t, conn, "OPEN", nrSHA1, nrDate1, "MERGEABLE", "2026-06-17T10:00:00Z")
	needsRebasePoll(t, conn, "OPEN", nrSHA1, nrDate1, "CONFLICTING", "2026-06-17T11:00:00Z")
	evs := needsRebaseEvents(t, conn)
	if len(evs) != 2 {
		t.Fatalf("expected 2 pr_needs_rebase, got %d", len(evs))
	}
	var stale, fresh int
	for _, e := range evs {
		switch e.Title {
		case nrStale + nrTitle1:
			stale++
		case nrTitle1:
			fresh++
		default:
			t.Errorf("unexpected title %q", e.Title)
		}
	}
	if stale != 1 || fresh != 1 {
		t.Errorf("stale=%d fresh=%d, want 1 and 1", stale, fresh)
	}
}

func TestProcessPR_NeedsRebase_NotOpen(t *testing.T) {
	conn := needsRebaseSeed(t)
	needsRebasePoll(t, conn, "MERGED", nrSHA1, nrDate1, "CONFLICTING", "2026-06-17T09:00:00Z")
	if got := len(needsRebaseEvents(t, conn)); got != 0 {
		t.Errorf("expected no pr_needs_rebase, got %d", got)
	}
}

func TestProcessPR_NeedsRebase_NoSHA(t *testing.T) {
	conn := needsRebaseSeed(t)
	needsRebasePoll(t, conn, "OPEN", "", nrDate1, "CONFLICTING", "2026-06-17T09:00:00Z")
	if got := len(needsRebaseEvents(t, conn)); got != 0 {
		t.Errorf("expected no pr_needs_rebase, got %d", got)
	}
}

func TestProcessPR_NeedsRebase_DoesNotAdvanceCursor(t *testing.T) {
	conn := needsRebaseSeed(t) // seed external_ts 07:00, before nrDate1
	needsRebasePoll(t, conn, "OPEN", nrSHA1, nrDate1, "CONFLICTING", "2026-06-17T09:00:00Z")
	if got := len(needsRebaseEvents(t, conn)); got != 1 {
		t.Fatalf("expected 1 pr_needs_rebase, got %d", got)
	}
	cursor, err := db.EventCursor(conn, "github", prResource.Type, prResource.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != nrDate1 {
		t.Errorf("cursor = %q, want %q (the head commit date, not poll time)", cursor, nrDate1)
	}
}

func TestNeedsRebaseTitle(t *testing.T) {
	if got := needsRebaseTitle(7, "abc"); got != "Needs rebase: PR #7 @ abc" {
		t.Errorf("short sha: %q", got)
	}
	if got := needsRebaseTitle(123, nrSHA1); got != nrTitle1 {
		t.Errorf("long sha: %q", got)
	}
}

// --- pr_description_changed ---------------------------------------------

var descResource = watcher.Resource{
	Type: "pr",
	ID:   "o/r#1",
	URL:  "https://github.com/o/r/pull/1",
}

// descVersion is one synthetic userContentEdits node. A nil body is a
// version whose history entry was deleted (GitHub returns diff: null).
type descVersion struct {
	editedAt string
	body     *string
	editor   string // GraphQL editor JSON, or "null"
}

func strp(s string) *string { return &s }

const (
	descUser = `{"__typename":"User","login":"alice","databaseId":101}`
	descBot  = `{"__typename":"Bot","login":"helper-bot","databaseId":202}`
)

// descVersions returns V0 (original) … V(n-1), oldest first, an hour apart
// starting at 08:00, each with body "body vN".
func descVersions(n int) []descVersion {
	var vs []descVersion
	for i := 0; i < n; i++ {
		vs = append(vs, descVersion{
			editedAt: fmt.Sprintf("2026-06-17T%02d:00:00Z", 8+i),
			body:     strp(fmt.Sprintf("body v%d", i)),
			editor:   descUser,
		})
	}
	return vs
}

// descServer serves pullRequest.userContentEdits newest first, pageSize
// nodes per page (ignoring the requested page size), from versions given
// oldest first. status != 200 fails every request. It counts requests.
type descServer struct {
	srv      *httptest.Server
	requests atomic.Int32
}

func newDescServer(t *testing.T, versions []descVersion, pageSize int, status int) *descServer {
	t.Helper()
	ds := &descServer{}
	newest := make([]descVersion, len(versions))
	for i, v := range versions {
		newest[len(versions)-1-i] = v
	}
	ds.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ds.requests.Add(1)
		if status != http.StatusOK {
			w.WriteHeader(status)
			io.WriteString(w, "boom")
			return
		}
		var req struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if !strings.Contains(req.Query, "userContentEdits") {
			t.Errorf("unexpected query: %s", req.Query)
		}
		start := 0
		if after, _ := req.Variables["after"].(string); after != "" {
			fmt.Sscanf(after, "c%d", &start)
		}
		end := start + pageSize
		if end > len(newest) {
			end = len(newest)
		}
		var nodes []string
		for _, v := range newest[start:end] {
			diff := "null"
			if v.body != nil {
				b, _ := json.Marshal(*v.body)
				diff = string(b)
			}
			nodes = append(nodes, fmt.Sprintf(`{"editedAt":%q,"diff":%s,"editor":%s}`, v.editedAt, diff, v.editor))
		}
		fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"userContentEdits":{"pageInfo":{"hasNextPage":%t,"endCursor":"c%d"},"nodes":[%s]}}}}}`,
			end < len(newest), end, strings.Join(nodes, ","))
	}))
	t.Cleanup(ds.srv.Close)
	return ds
}

// descSeed subscribes descResource and sets the cursor to cursor via a
// first (watch_started) poll.
func descSeed(t *testing.T, cursor string) *sql.DB {
	t.Helper()
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", descResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if cursor != "" {
		descPollOpts(t, conn, "http://127.0.0.1:1", cursor, "", false, PollOptions{})
	}
	return conn
}

func descPollOpts(t *testing.T, conn *sql.DB, url, updatedAt, lastEditedAt string, backfill bool, opts PollOptions) {
	t.Helper()
	prData := PRData{
		Number: 1, Owner: "o", Repo: "r", State: "OPEN", Title: "T",
		CreatedAt: "2026-06-17T08:00:00Z", UpdatedAt: updatedAt, LastEditedAt: lastEditedAt,
	}
	if _, err := processPRWith(conn, defaultGHHTTP(url), prData, descResource, "tok", backfill, testLogger(), opts); err != nil {
		t.Fatalf("processPRWith: %v", err)
	}
}

func descPoll(t *testing.T, conn *sql.DB, url, lastEditedAt string) {
	t.Helper()
	descPollOpts(t, conn, url, "2026-06-17T23:00:00Z", lastEditedAt, false, PollOptions{})
}

func descEvents(t *testing.T, conn *sql.DB) []watcher.Event {
	t.Helper()
	events, err := db.EventsForResource(conn, "pr", descResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	var out []watcher.Event
	for _, e := range events {
		if e.Type == watcher.EventTypePRDescChanged {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return *out[i].ExternalTS < *out[j].ExternalTS })
	return out
}

func descEditedAtState(t *testing.T, conn *sql.DB) string {
	t.Helper()
	rs, err := db.GetResourceState(conn, "pr", descResource.ID)
	if err != nil || rs == nil {
		t.Fatalf("GetResourceState: %v (nil=%v)", err, rs == nil)
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(rs.StateJSON), &m); err != nil {
		t.Fatal(err)
	}
	s, _ := m["description_edited_at"].(string)
	return s
}

func descTextChange(t *testing.T, conn *sql.DB, e watcher.Event) *watcher.TextChange {
	t.Helper()
	tc, err := db.EventTextChange(conn, e.ID)
	if err != nil {
		t.Fatalf("EventTextChange: %v", err)
	}
	return tc
}

func TestDescriptionEdits_PagesAndPairing(t *testing.T) {
	vs := descVersions(5)
	conn := descSeed(t, "2026-06-17T09:30:00Z") // between V1 and V2
	ds := newDescServer(t, vs, 2, http.StatusOK)

	descPoll(t, conn, ds.srv.URL, vs[4].editedAt)

	// Pages: [V4 V3] [V2 V1] [V0]. V1 is at or before the cursor, so the
	// third page is never requested.
	if got := ds.requests.Load(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
	evs := descEvents(t, conn)
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3", len(evs))
	}
	for k, e := range evs {
		i := k + 2
		if *e.ExternalTS != vs[i].editedAt {
			t.Errorf("event %d external_ts = %q, want %q", k, *e.ExternalTS, vs[i].editedAt)
		}
		if e.Title != "o/r#1 description changed" {
			t.Errorf("title = %q", e.Title)
		}
		if e.Body != nil {
			t.Errorf("body = %q, want nil", *e.Body)
		}
		if e.Author == nil || *e.Author != "alice" || e.AuthorType == nil || *e.AuthorType != "user" || e.AuthorID == nil || *e.AuthorID != "101" {
			t.Errorf("author = %v/%v/%v", e.Author, e.AuthorType, e.AuthorID)
		}
		tc := descTextChange(t, conn, e)
		want := watcher.TextChange{Old: *vs[i-1].body, New: *vs[i].body}
		if tc == nil || *tc != want {
			t.Errorf("event %d text change = %+v, want %+v", k, tc, want)
		}
	}
	if got := descEditedAtState(t, conn); got != vs[4].editedAt {
		t.Errorf("description_edited_at = %q, want %q", got, vs[4].editedAt)
	}
}

func TestDescriptionEdits_OriginalNeverEmitted(t *testing.T) {
	vs := descVersions(3)
	conn := descSeed(t, "")
	ds := newDescServer(t, vs, 50, http.StatusOK)

	descPollOpts(t, conn, ds.srv.URL, "2026-06-17T23:00:00Z", vs[2].editedAt, true, PollOptions{})

	evs := descEvents(t, conn)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2", len(evs))
	}
	for _, e := range evs {
		if *e.ExternalTS == vs[0].editedAt {
			t.Errorf("emitted an event for the original body")
		}
	}
}

func TestDescriptionEdits_OnlyWhenLastEditedAtMoves(t *testing.T) {
	vs := descVersions(2)
	conn := descSeed(t, "2026-06-17T08:30:00Z")
	ds := newDescServer(t, vs, 50, http.StatusOK)

	descPoll(t, conn, ds.srv.URL, vs[1].editedAt)
	if got := ds.requests.Load(); got != 1 {
		t.Fatalf("first poll requests = %d, want 1", got)
	}
	descPoll(t, conn, ds.srv.URL, vs[1].editedAt)
	if got := ds.requests.Load(); got != 1 {
		t.Errorf("unchanged lastEditedAt made %d more requests", got-1)
	}
	if n := len(descEvents(t, conn)); n != 1 {
		t.Errorf("got %d events, want 1", n)
	}
}

func TestDescriptionEdits_NeverEditedMakesNoQuery(t *testing.T) {
	conn := descSeed(t, "2026-06-17T08:30:00Z")
	ds := newDescServer(t, nil, 50, http.StatusOK)

	descPoll(t, conn, ds.srv.URL, "")
	if got := ds.requests.Load(); got != 0 {
		t.Errorf("requests = %d, want 0", got)
	}
}

func TestDescriptionEdits_FailureDoesNotAdvance(t *testing.T) {
	vs := descVersions(2)
	conn := descSeed(t, "2026-06-17T08:30:00Z")
	failing := newDescServer(t, vs, 50, http.StatusInternalServerError)

	descPoll(t, conn, failing.srv.URL, vs[1].editedAt)
	if n := len(descEvents(t, conn)); n != 0 {
		t.Errorf("got %d events after failure, want 0", n)
	}
	// Not advanced to lastEditedAt: it holds the bound the retry starts from
	// (the cursor, as nothing was saved before).
	if got := descEditedAtState(t, conn); got != "2026-06-17T08:30:00Z" {
		t.Errorf("description_edited_at = %q after failure, want the cursor 2026-06-17T08:30:00Z", got)
	}

	ok := newDescServer(t, vs, 50, http.StatusOK)
	descPoll(t, conn, ok.srv.URL, vs[1].editedAt)
	if got := ok.requests.Load(); got != 1 {
		t.Errorf("retry requests = %d, want 1", got)
	}
	if n := len(descEvents(t, conn)); n != 1 {
		t.Errorf("got %d events after retry, want 1", n)
	}
	if got := descEditedAtState(t, conn); got != vs[1].editedAt {
		t.Errorf("description_edited_at = %q, want %q", got, vs[1].editedAt)
	}
}

// descPollWithComment polls with a comment at commentAt, an event that
// moves the cursor past the description edits in the same poll.
func descPollWithComment(t *testing.T, conn *sql.DB, url, lastEditedAt, commentAt string) {
	t.Helper()
	prData := PRData{
		Number: 1, Owner: "o", Repo: "r", State: "OPEN", Title: "T",
		CreatedAt: "2026-06-17T08:00:00Z", UpdatedAt: "2026-06-17T23:00:00Z", LastEditedAt: lastEditedAt,
		Comments: []Comment{{Author: "carol", AuthorType: "user", CreatedAt: commentAt, Body: "a comment"}},
	}
	if _, err := processPRWith(conn, defaultGHHTTP(url), prData, descResource, "tok", false, testLogger(), PollOptions{}); err != nil {
		t.Fatalf("processPRWith: %v", err)
	}
}

// A failed edits fetch in a poll that also emits a later event (moving the
// cursor past the edit) must not lose the edit on the retry.
func TestDescriptionEdits_RetryAfterCursorMoves(t *testing.T) {
	vs := descVersions(3)
	commentAt := "2026-06-17T11:00:00Z" // after V2's edit

	check := func(t *testing.T, conn *sql.DB, wantNew int) {
		t.Helper()
		evs := descEvents(t, conn)
		if len(evs) == 0 || *evs[len(evs)-1].ExternalTS != vs[2].editedAt {
			t.Fatalf("edit V2 not emitted; events: %d", len(evs))
		}
		tc := descTextChange(t, conn, evs[len(evs)-1])
		want := watcher.TextChange{Old: *vs[1].body, New: *vs[2].body}
		if tc == nil || *tc != want {
			t.Errorf("text change = %+v, want %+v", tc, want)
		}
		if len(evs) != wantNew {
			t.Errorf("got %d description events, want %d", len(evs), wantNew)
		}
		if got := descEditedAtState(t, conn); got != vs[2].editedAt {
			t.Errorf("description_edited_at = %q, want %q", got, vs[2].editedAt)
		}
	}

	t.Run("after an earlier successful fetch", func(t *testing.T) {
		conn := descSeed(t, "2026-06-17T08:30:00Z")
		descPoll(t, conn, newDescServer(t, vs[:2], 50, http.StatusOK).srv.URL, vs[1].editedAt)
		descPollWithComment(t, conn, newDescServer(t, vs, 50, http.StatusInternalServerError).srv.URL, vs[2].editedAt, commentAt)
		descPoll(t, conn, newDescServer(t, vs, 50, http.StatusOK).srv.URL, vs[2].editedAt)
		check(t, conn, 2)
	})

	t.Run("on the first fetch", func(t *testing.T) {
		conn := descSeed(t, "2026-06-17T09:30:00Z") // between V1 and V2
		descPollWithComment(t, conn, newDescServer(t, vs, 50, http.StatusInternalServerError).srv.URL, vs[2].editedAt, commentAt)
		descPoll(t, conn, newDescServer(t, vs, 50, http.StatusOK).srv.URL, vs[2].editedAt)
		check(t, conn, 1)
	})
}

func TestDescriptionEdits_LineEndingsOnly(t *testing.T) {
	vs := descVersions(2)
	vs[0].body = strp("line one\nline two")
	vs[1].body = strp("line one\r\nline two")
	conn := descSeed(t, "2026-06-17T08:30:00Z")
	ds := newDescServer(t, vs, 50, http.StatusOK)

	descPoll(t, conn, ds.srv.URL, vs[1].editedAt)
	if n := len(descEvents(t, conn)); n != 0 {
		t.Errorf("got %d events for a line-ending-only edit, want 0", n)
	}
}

func TestDescriptionEdits_NullDiff(t *testing.T) {
	vs := descVersions(2)
	vs[0].body = nil
	conn := descSeed(t, "2026-06-17T08:30:00Z")
	ds := newDescServer(t, vs, 50, http.StatusOK)

	descPoll(t, conn, ds.srv.URL, vs[1].editedAt)
	evs := descEvents(t, conn)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if tc := descTextChange(t, conn, evs[0]); tc != nil {
		t.Errorf("text change = %+v, want nil", tc)
	}
}

func TestDescriptionEdits_NullEditor(t *testing.T) {
	vs := descVersions(2)
	vs[1].editor = "null"
	conn := descSeed(t, "2026-06-17T08:30:00Z")
	ds := newDescServer(t, vs, 50, http.StatusOK)

	descPoll(t, conn, ds.srv.URL, vs[1].editedAt)
	evs := descEvents(t, conn)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	e := evs[0]
	if (e.Author != nil && *e.Author != "") || e.AuthorType == nil || *e.AuthorType != "user" || e.AuthorID != nil {
		t.Errorf("author = %v/%v/%v, want empty/user/nil", e.Author, e.AuthorType, e.AuthorID)
	}
}

func TestDescriptionEdits_BotAuthor(t *testing.T) {
	vs := descVersions(2)
	vs[1].editor = descBot

	t.Run("kept by default", func(t *testing.T) {
		conn := descSeed(t, "2026-06-17T08:30:00Z")
		ds := newDescServer(t, vs, 50, http.StatusOK)
		descPoll(t, conn, ds.srv.URL, vs[1].editedAt)
		evs := descEvents(t, conn)
		if len(evs) != 1 {
			t.Fatalf("got %d events, want 1", len(evs))
		}
		e := evs[0]
		if e.Author == nil || *e.Author != "helper-bot" || e.AuthorType == nil || *e.AuthorType != "bot" || e.AuthorID == nil || *e.AuthorID != "202" {
			t.Errorf("author = %v/%v/%v", e.Author, e.AuthorType, e.AuthorID)
		}
	})

	t.Run("dropped on request", func(t *testing.T) {
		conn := descSeed(t, "2026-06-17T08:30:00Z")
		ds := newDescServer(t, vs, 50, http.StatusOK)
		descPollOpts(t, conn, ds.srv.URL, "2026-06-17T23:00:00Z", vs[1].editedAt, false, PollOptions{DropBotDescriptionEdits: true})
		if n := len(descEvents(t, conn)); n != 0 {
			t.Errorf("got %d events, want 0", n)
		}
		if got := descEditedAtState(t, conn); got != vs[1].editedAt {
			t.Errorf("description_edited_at = %q, want %q", got, vs[1].editedAt)
		}
	})
}

func TestDescriptionEdits_Dedup(t *testing.T) {
	vs := descVersions(3)
	conn := descSeed(t, "2026-06-17T08:30:00Z")
	ds := newDescServer(t, vs, 50, http.StatusOK)
	prData := PRData{Number: 1, Owner: "o", Repo: "r", LastEditedAt: vs[2].editedAt}

	// Run the same edit processing twice against the same (stale) cursor, so
	// the second run sees the same edits as new and only dedup stops them.
	for run, want := range []int{2, 0} {
		n, err := processDescriptionEdits(conn, defaultGHHTTP(ds.srv.URL), prData, descResource, "tok", "2026-06-17T08:30:00Z", PollOptions{}, testLogger())
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if n != want {
			t.Errorf("run %d emitted %d events, want %d", run, n, want)
		}
	}
	if n := len(descEvents(t, conn)); n != 2 {
		t.Errorf("got %d events, want 2", n)
	}
}

func TestBuildPRStateJSONHeadBase(t *testing.T) {
	got := buildPRStateJSON(PRData{HeadRefName: "feat", BaseRefName: "main", HeadRepoOwner: "o", HeadRepoName: "r"}, "", "")
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"head_ref_name": "feat", "base_ref_name": "main", "head_repo_owner": "o", "head_repo_name": "r"} {
		if m[k] != want {
			t.Errorf("%s = %v, want %q", k, m[k], want)
		}
	}
}
