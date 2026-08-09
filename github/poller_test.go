package github

import (
	"io"
	"log"
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
