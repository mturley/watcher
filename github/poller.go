package github

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mturley/watcher"
	"github.com/mturley/watcher/db"
)

// Poll polls GitHub for PR updates and emits events. Each resource's
// backfill flag is resolved from its live subscriptions.
func Poll(conn *sql.DB, token string, resources []watcher.Resource, logger *log.Logger) error {
	if token == "" {
		return fmt.Errorf("GitHub token not configured")
	}

	// Parse all resource IDs into PRRefs
	var prRefs []PRRef
	resourceMap := make(map[string]watcher.Resource) // key: "owner/repo#number"
	for _, r := range resources {
		ref, err := ParsePRResourceID(r.ID)
		if err != nil {
			logger.Printf("ERROR: failed to parse resource ID %q: %v", r.ID, err)
			// Emit error event for this resource
			errBody := fmt.Sprintf("Failed to parse resource ID: %v", err)
			if err := emitError(conn, "Invalid PR resource ID", &errBody, r); err != nil {
				logger.Printf("ERROR: failed to emit watcher error: %v", err)
			}
			continue
		}
		prRefs = append(prRefs, ref)
		resourceMap[r.ID] = r
	}

	if len(prRefs) == 0 {
		logger.Printf("No valid PRs to poll")
		return nil
	}

	// Fetch PR data
	logger.Printf("Fetching data for %d PRs...", len(prRefs))
	prDataList, rateLimit, err := FetchPRs(token, prRefs)
	if err != nil {
		logger.Printf("ERROR: failed to fetch PRs: %v", err)
		// Emit error events for all resources
		errBody := fmt.Sprintf("Failed to fetch PR data: %v", err)
		for _, r := range resources {
			if err := emitError(conn, "GitHub API error", &errBody, r); err != nil {
				logger.Printf("ERROR: failed to emit watcher error: %v", err)
			}
		}
		if err := db.RecordPollerError(conn, "github", errBody); err != nil {
			logger.Printf("ERROR: failed to record poller error: %v", err)
		}
		return err
	}

	logger.Printf("Rate limit: %d/%d remaining", rateLimit.Remaining, rateLimit.Limit)

	// Process each PR
	eventCount := 0
	for _, prData := range prDataList {
		resourceID := fmt.Sprintf("%s/%s#%d", prData.Owner, prData.Repo, prData.Number)
		resource, ok := resourceMap[resourceID]
		if !ok {
			logger.Printf("WARNING: received data for unknown resource %q", resourceID)
			continue
		}

		backfill, err := db.BackfillFor(conn, resource.Type, resource.ID)
		if err != nil {
			logger.Printf("WARNING: failed to resolve backfill for %s: %v", resourceID, err)
		}

		count, err := processPR(conn, prData, resource, token, backfill, logger)
		if err != nil {
			logger.Printf("ERROR: failed to process PR %s: %v", resourceID, err)
			// Emit error event
			errBody := fmt.Sprintf("Failed to process PR: %v", err)
			if err := emitError(conn, "PR processing error", &errBody, resource); err != nil {
				logger.Printf("ERROR: failed to emit watcher error: %v", err)
			}
			continue
		}
		eventCount += count
	}

	logger.Printf("Emitted %d events", eventCount)
	if err := db.RecordPollerSuccess(conn, "github"); err != nil {
		logger.Printf("ERROR: failed to record poller success: %v", err)
	}
	return nil
}

// emitEvent inserts a watcher event for the given resource, setting the
// ID and recording timestamp that db.InsertEvent does not set.
func emitEvent(conn *sql.DB, t watcher.EventType, title string, body *string, externalTS string, author, authorType *string, r watcher.Resource) error {
	extTS := externalTS
	return db.InsertEvent(conn, watcher.Event{
		ID:         uuid.New().String(),
		TS:         time.Now().UTC().Format(time.RFC3339),
		ExternalTS: &extTS,
		Source:     "github",
		Type:       t,
		Title:      title,
		Body:       body,
		Author:     author,
		AuthorType: authorType,
	}, r)
}

// emitError inserts a watcher_error event, guarded by db.HasPollerError to
// avoid duplicate error spam (mirrors the framework's EmitWatcherError guard).
func emitError(conn *sql.DB, title string, body *string, r watcher.Resource) error {
	if db.HasPollerError(conn, "github") {
		return nil
	}
	return db.InsertEvent(conn, watcher.Event{
		ID:     uuid.New().String(),
		TS:     time.Now().UTC().Format(time.RFC3339),
		Source: "github",
		Type:   watcher.EventTypeWatcherError,
		Title:  title,
		Body:   body,
	}, r)
}

// processPR processes a single PR and emits events.
// Returns the count of events emitted. When cursor is empty and backfill
// is true, all history is emitted; when backfill is false, only a
// watch_started event is emitted on the first poll.
func processPR(conn *sql.DB, prData PRData, resource watcher.Resource, token string, backfill bool, logger *log.Logger) (int, error) {
	eventCount := 0

	// Get cursor (last seen external timestamp)
	cursor, err := db.EventCursor(conn, "github", resource.Type, resource.ID)
	if err != nil {
		return eventCount, fmt.Errorf("failed to get event cursor: %w", err)
	}

	// First poll without backfill: emit watch_started event and return.
	// With backfill, fall through to normal processing so all history is
	// emitted (the item<=cursor guards are false for any real timestamp
	// vs "", and the CI block already handles cursor=="").
	if cursor == "" && !backfill {
		title := fmt.Sprintf("Started watching PR: %s", prData.Title)
		body := fmt.Sprintf("PR #%d in %s/%s\nState: %s", prData.Number, prData.Owner, prData.Repo, prData.State)
		if err := emitEvent(conn, watcher.EventTypeWatchStarted, title, &body, prData.UpdatedAt, nil, nil, resource); err != nil {
			return eventCount, fmt.Errorf("failed to emit watch_started event: %w", err)
		}
		eventCount++
		logger.Printf("Emitted watch_started for %s", resource.ID)
		return eventCount, nil
	}

	// Process reviews. APPROVED and CHANGES_REQUESTED map to their own
	// event types; every other submitted review state (COMMENTED, and
	// defensively any future state) is emitted as a review event too, so
	// top-level review summaries — e.g. CodeRabbit and other bots, which
	// submit COMMENTED reviews rather than APPROVED/CHANGES_REQUESTED —
	// reach subscribers instead of being silently dropped. DISMISSED and
	// PENDING reviews are skipped (no submittedAt / not surfaced to users).
	for _, review := range prData.Reviews {
		if review.SubmittedAt <= cursor {
			continue
		}
		if review.State == "DISMISSED" || review.State == "PENDING" {
			continue
		}

		eventType := reviewEventType(review.State)

		// Skip duplicate events (dedup by the review's submittedAt, which
		// is unique per review).
		dup, err := db.IsDuplicate(conn, db.DedupCheck{
			Source:       "github",
			ResourceType: resource.Type,
			ResourceID:   resource.ID,
			Type:         eventType,
			ExternalTS:   &review.SubmittedAt,
		})
		if err != nil {
			return eventCount, fmt.Errorf("failed to check review duplicate: %w", err)
		}
		if dup {
			continue
		}

		var title string
		switch review.State {
		case "APPROVED":
			title = fmt.Sprintf("PR approved by %s", review.Author)
		case "CHANGES_REQUESTED":
			title = fmt.Sprintf("Changes requested by %s", review.Author)
		default: // COMMENTED and any other submitted state
			title = fmt.Sprintf("PR review by %s", review.Author)
		}

		if err := emitEvent(conn, eventType, title, &review.Body, review.SubmittedAt, &review.Author, &review.AuthorType, resource); err != nil {
			return eventCount, fmt.Errorf("failed to emit %s event: %w", eventType, err)
		}
		eventCount++
		logger.Printf("Emitted %s for %s by %s", eventType, resource.ID, review.Author)
	}

	// Process comments
	for _, comment := range prData.Comments {
		if comment.CreatedAt <= cursor {
			continue
		}

		dup, err := db.IsDuplicate(conn, db.DedupCheck{
			Source:       "github",
			ResourceType: resource.Type,
			ResourceID:   resource.ID,
			Type:         watcher.EventTypePRComment,
			ExternalTS:   &comment.CreatedAt,
		})
		if err != nil {
			return eventCount, fmt.Errorf("failed to check comment duplicate: %w", err)
		}
		if dup {
			continue
		}

		title := fmt.Sprintf("Comment by %s", comment.Author)
		if err := emitEvent(conn, watcher.EventTypePRComment, title, &comment.Body, comment.CreatedAt, &comment.Author, &comment.AuthorType, resource); err != nil {
			return eventCount, fmt.Errorf("failed to emit pr_comment event: %w", err)
		}
		eventCount++
		logger.Printf("Emitted pr_comment for %s by %s", resource.ID, comment.Author)
	}

	// Process review comments (inline code comments).
	// Dedup on BOTH title and createdAt: neither alone is safe. Title
	// alone (author+path) collapses distinct comments by the same author
	// on the same file across different reviews (e.g. a bot re-commenting
	// on a file it already flagged), permanently suppressing the later
	// ones. Timestamp alone collapses distinct comments within one
	// batch-submitted review, which all share a createdAt. Requiring both
	// to match distinguishes both cases.
	for _, reviewComment := range prData.ReviewComments {
		if reviewComment.CreatedAt <= cursor {
			continue
		}

		title := fmt.Sprintf("Review comment by %s on %s", reviewComment.Author, reviewComment.Path)
		dup, err := db.IsDuplicate(conn, db.DedupCheck{
			Source:       "github",
			ResourceType: resource.Type,
			ResourceID:   resource.ID,
			Type:         watcher.EventTypePRReviewComment,
			Title:        &title,
			ExternalTS:   &reviewComment.CreatedAt,
		})
		if err != nil {
			return eventCount, fmt.Errorf("failed to check review comment duplicate: %w", err)
		}
		if dup {
			continue
		}

		if err := emitEvent(conn, watcher.EventTypePRReviewComment, title, &reviewComment.Body, reviewComment.CreatedAt, &reviewComment.Author, &reviewComment.AuthorType, resource); err != nil {
			return eventCount, fmt.Errorf("failed to emit pr_review_comment event: %w", err)
		}
		eventCount++
		logger.Printf("Emitted pr_review_comment for %s by %s on %s", resource.ID, reviewComment.Author, reviewComment.Path)
	}

	// Read previous resource state once, used both by the CI-pending-refresh
	// check below and by the new-commits detection further down.
	prevState, _ := db.GetResourceState(conn, "pr", resource.ID)
	var prevStateMap map[string]interface{}
	if prevState != nil {
		if json.Unmarshal([]byte(prevState.StateJSON), &prevStateMap) != nil {
			// Malformed state_json: treat as "no prior state" rather than
			// risking a partially-populated map (matches the old inline
			// check's behavior of skipping cleanly on unmarshal error).
			prevStateMap = nil
		}
	}

	// Fetch remaining check contexts if paginated
	if prData.CheckRunsHasMore && prData.CheckRunsEndCursor != "" {
		moreRuns, moreCtx, err := FetchRemainingCheckContexts(token, prData.Owner, prData.Repo, prData.Number, prData.CheckRunsEndCursor)
		if err != nil {
			logger.Printf("WARNING: failed to paginate check contexts for %s: %v", resource.ID, err)
		} else {
			prData.CheckRuns = append(prData.CheckRuns, moreRuns...)
			prData.StatusContexts = append(prData.StatusContexts, moreCtx...)
		}
	}

	// Process check runs as a bundle per commit
	if len(prData.CheckRuns) > 0 && prData.Commits.LatestSHA != "" {
		hasNewChecks := false
		for _, cr := range prData.CheckRuns {
			if cr.CompletedAt != "" && cr.CompletedAt > cursor {
				hasNewChecks = true
				break
			}
		}

		// A purely-pending CI state never produces a newly-completed check,
		// so hasNewChecks stays false and the bundle would otherwise only be
		// recorded once (on the first/backfill poll) and never refresh while
		// checks remain pending (GitHub issue #2). Detect that the set of
		// pending checks changed since the last poll and treat that as a
		// trigger too, so the bundle stays live without re-emitting on every
		// poll when nothing has actually changed.
		curFP := pendingCIFingerprint(prData)
		prevFP, _ := prevStateMap["ci_pending_fingerprint"].(string)
		pendingChanged := curFP != "" && curFP != prevFP

		if hasNewChecks || cursor == "" || pendingChanged {
			passed, failed, pending := 0, 0, 0
			var latestTS string
			var failedNames, pendingNames, passedNames []string

			for _, cr := range prData.CheckRuns {
				if cr.Status == "COMPLETED" {
					_, isCI := checkRunEventType(cr.Conclusion)
					if !isCI {
						continue
					}
					if cr.CompletedAt > latestTS {
						latestTS = cr.CompletedAt
					}
					switch cr.Conclusion {
					case "SUCCESS", "NEUTRAL", "SKIPPED":
						passed++
						passedNames = append(passedNames, fmt.Sprintf("✓ %s", cr.Name))
					default:
						failed++
						failedNames = append(failedNames, fmt.Sprintf("✗ %s: %s", cr.Name, cr.Conclusion))
					}
				} else if cr.Status == "IN_PROGRESS" || cr.Status == "QUEUED" {
					pending++
					pendingNames = append(pendingNames, fmt.Sprintf("⧖ %s", cr.Name))
				}
			}

			total := passed + failed + pending
			if total == 0 {
				goto skipCIBundle
			}

			shortSHA := prData.Commits.LatestSHA
			if len(shortSHA) > 7 {
				shortSHA = shortSHA[:7]
			}

			var eventType watcher.EventType
			var title string
			prLabel := fmt.Sprintf("PR #%d", prData.Number)

			if failed > 0 && pending > 0 {
				eventType = watcher.EventTypeCIPartialFailure
				title = fmt.Sprintf("CI failing for %s @ %s (%d failed, %d/%d passed)", prLabel, shortSHA, failed, passed, total)
			} else if failed > 0 {
				eventType = watcher.EventTypeCIFailed
				title = fmt.Sprintf("CI failed for %s @ %s (%d failed, %d passed)", prLabel, shortSHA, failed, passed)
			} else if pending > 0 {
				eventType = watcher.EventTypeCIPending
				title = fmt.Sprintf("CI running for %s @ %s (%d/%d passed)", prLabel, shortSHA, passed, total)
			} else {
				eventType = watcher.EventTypeCIPassed
				title = fmt.Sprintf("CI checks passed for %s @ %s (%d/%d checks)", prLabel, shortSHA, passed, total)
			}

			// Build body: failures first, then pending, then passes
			var bodyLines []string
			bodyLines = append(bodyLines, failedNames...)
			bodyLines = append(bodyLines, pendingNames...)
			bodyLines = append(bodyLines, passedNames...)
			body := strings.Join(bodyLines, "\n")

			if latestTS == "" {
				latestTS = time.Now().UTC().Format(time.RFC3339)
			}

			if err := db.UpsertCIBundle(conn, prData.Commits.LatestSHA, eventType, title, body, latestTS, resource, db.CICheckBundleTypes); err != nil {
				return eventCount, fmt.Errorf("failed to upsert CI bundle: %w", err)
			}
			eventCount++
			logger.Printf("Upserted CI bundle for %s (%s): %s", resource.ID, prData.Commits.LatestSHA[:8], title)
		}
	}
skipCIBundle:

	// Process StatusContexts (gated workflows / external CI like ci/prow) as a
	// separate per-commit bundle, independent of the CheckRun bundle above. A
	// StatusContext (e.g. "Cypress E2E Tests", triggered after the Test
	// workflow) can still be PENDING while every CheckRun has passed, so folding
	// it into the CheckRun bundle would let "CI checks passed" hide a
	// still-running gated workflow.
	if len(prData.StatusContexts) > 0 && prData.Commits.LatestSHA != "" {
		// StatusContexts carry no completion timestamp, so there is no
		// newly-completed signal to compare against the cursor. Drive refresh
		// off (a) the first/backfill poll and (b) any change to the full set of
		// (name,state) pairs — this catches SUCCESS→FAILURE transitions that
		// don't change the pending set, as well as pending appearing/clearing.
		curWF := workflowFingerprint(prData)
		prevWF, _ := prevStateMap["ci_workflow_fingerprint"].(string)
		workflowChanged := curWF != "" && curWF != prevWF

		if cursor == "" || workflowChanged {
			passed, failed, pending := 0, 0, 0
			var failedNames, pendingNames, passedNames []string

			for _, sc := range prData.StatusContexts {
				switch sc.State {
				case "SUCCESS", "EXPECTED", "NEUTRAL":
					passed++
					passedNames = append(passedNames, fmt.Sprintf("✓ %s", sc.Name))
				case "FAILURE", "ERROR":
					failed++
					failedNames = append(failedNames, fmt.Sprintf("✗ %s: %s", sc.Name, sc.State))
				default: // PENDING and any unknown state, treated as in-flight
					pending++
					pendingNames = append(pendingNames, fmt.Sprintf("⧖ %s", sc.Name))
				}
			}

			total := passed + failed + pending
			if total > 0 {
				shortSHA := prData.Commits.LatestSHA
				if len(shortSHA) > 7 {
					shortSHA = shortSHA[:7]
				}

				var eventType watcher.EventType
				var title string
				prLabel := fmt.Sprintf("PR #%d", prData.Number)

				if failed > 0 && pending > 0 {
					eventType = watcher.EventTypeCIWorkflowsPartialFailure
					title = fmt.Sprintf("Workflows failing for %s @ %s (%d failed, %d/%d passed)", prLabel, shortSHA, failed, passed, total)
				} else if failed > 0 {
					eventType = watcher.EventTypeCIWorkflowsFailed
					title = fmt.Sprintf("Workflows failed for %s @ %s (%d failed, %d passed)", prLabel, shortSHA, failed, passed)
				} else if pending > 0 {
					eventType = watcher.EventTypeCIWorkflowsPending
					title = fmt.Sprintf("Workflows running for %s @ %s (%d/%d passed)", prLabel, shortSHA, passed, total)
				} else {
					eventType = watcher.EventTypeCIWorkflowsPassed
					title = fmt.Sprintf("Workflows passed for %s @ %s (%d/%d)", prLabel, shortSHA, passed, total)
				}

				var bodyLines []string
				bodyLines = append(bodyLines, failedNames...)
				bodyLines = append(bodyLines, pendingNames...)
				bodyLines = append(bodyLines, passedNames...)
				body := strings.Join(bodyLines, "\n")

				// StatusContexts have no per-context timestamp; use now.
				externalTS := time.Now().UTC().Format(time.RFC3339)

				if err := db.UpsertCIBundle(conn, prData.Commits.LatestSHA, eventType, title, body, externalTS, resource, db.CIWorkflowBundleTypes); err != nil {
					return eventCount, fmt.Errorf("failed to upsert workflow bundle: %w", err)
				}
				eventCount++
				logger.Printf("Upserted workflow bundle for %s (%s): %s", resource.ID, prData.Commits.LatestSHA[:8], title)
			}
		}
	}

	// Check PR state
	if prData.State == "MERGED" || prData.State == "CLOSED" {
		eventType := watcher.EventTypePRMerged
		if prData.State == "CLOSED" {
			eventType = watcher.EventTypePRClosed
		}

		// Terminal PR state (merged/closed) is once-per-resource: dedup on
		// source+resource+type only. prData.UpdatedAt is the PR's mutable
		// GitHub updatedAt field, which post-merge activity (comments,
		// label changes, CI) continues to bump — keying dedup on it would
		// let the merged/closed event re-fire on every such change.
		dup, err := db.IsDuplicate(conn, db.DedupCheck{
			Source:        "github",
			ResourceType:  resource.Type,
			ResourceID:    resource.ID,
			Type:          eventType,
			MatchTypeOnly: true,
		})
		if err != nil {
			return eventCount, fmt.Errorf("failed to check PR state duplicate: %w", err)
		}
		if !dup {
			title := fmt.Sprintf("PR %s", prData.State)
			body := fmt.Sprintf("PR #%d: %s", prData.Number, prData.Title)
			if err := emitEvent(conn, eventType, title, &body, prData.UpdatedAt, nil, nil, resource); err != nil {
				return eventCount, fmt.Errorf("failed to emit %s event: %w", eventType, err)
			}
			eventCount++
			logger.Printf("Emitted %s for %s", eventType, resource.ID)
		}

		// Don't auto-unsubscribe — the terminal event needs to be delivered
		// to subscribers. Let subscribers or users unsubscribe explicitly,
		// or let the subscription go idle.
	}

	// Detect new commits by comparing latest SHA against previous state
	// (reuses prevStateMap read near the top of this function).
	if prData.Commits.LatestSHA != "" && prevStateMap != nil {
		prevSHA, _ := prevStateMap["latest_commit_sha"].(string)
		if prevSHA != "" && prevSHA != prData.Commits.LatestSHA {
			// The previous commit's CI/workflow bundles now describe a
			// superseded commit. Flag them out of date (title-only, no
			// timestamp bump) so a consumer that still holds them in its inbox
			// doesn't treat them as the current CI state. Idempotent, so
			// re-running on later polls while the SHA remains superseded is
			// harmless. Only the immediately-previous SHA is known here (same
			// limitation as new-commit detection), so commits skipped between
			// polls are not marked.
			if err := db.MarkCIBundlesOutOfDate(conn, prevSHA, resource); err != nil {
				logger.Printf("WARNING: failed to mark CI bundles out of date for %s @ %s: %v", resource.ID, prevSHA, err)
			}

			dup, err := db.IsDuplicate(conn, db.DedupCheck{
				Source:       "github",
				ResourceType: resource.Type,
				ResourceID:   resource.ID,
				Type:         watcher.EventTypePRNewCommits,
				ExternalTS:   &prData.Commits.LatestDate,
			})
			if err != nil {
				logger.Printf("WARNING: failed to check new commits duplicate: %v", err)
			} else if !dup {
				title := fmt.Sprintf("New commits pushed to PR #%d", prData.Number)
				body := formatNewCommitsBody(prData, prevSHA)
				if err := emitEvent(conn, watcher.EventTypePRNewCommits, title, &body, prData.Commits.LatestDate, nil, nil, resource); err != nil {
					logger.Printf("WARNING: failed to emit new commits event: %v", err)
				} else {
					eventCount++
					logger.Printf("Emitted pr_new_commits for %s", resource.ID)
				}
			}
		}
	}

	// Write resource state
	stateJSON := buildPRStateJSON(prData)
	now := time.Now().UTC().Format(time.RFC3339)
	if err := db.UpsertResourceState(conn, "pr", resource.ID, stateJSON, prData.UpdatedAt, now); err != nil {
		logger.Printf("WARNING: failed to upsert resource state for %s: %v", resource.ID, err)
	}

	return eventCount, nil
}

// reviewEventType maps a PR review state to an event type. APPROVED gets
// its own type; CHANGES_REQUESTED, COMMENTED, and any other submitted
// state map to the generic review-comment type.
func reviewEventType(state string) watcher.EventType {
	if state == "APPROVED" {
		return watcher.EventTypePRApproved
	}
	return watcher.EventTypePRReviewComment
}

func checkRunEventType(conclusion string) (watcher.EventType, bool) {
	switch conclusion {
	case "SUCCESS", "NEUTRAL", "SKIPPED":
		return watcher.EventTypeCICheckPassed, true
	case "FAILURE", "TIMED_OUT", "ACTION_REQUIRED", "CANCELLED", "STALE":
		return watcher.EventTypeCICheckFailed, true
	default:
		return "", false
	}
}

// derivePRReviewDecision computes the overall review decision based on latest review per author.
func derivePRReviewDecision(reviews []Review) string {
	latestByAuthor := make(map[string]Review)
	for _, r := range reviews {
		if r.State == "DISMISSED" {
			continue
		}
		existing, ok := latestByAuthor[r.Author]
		if !ok || r.SubmittedAt > existing.SubmittedAt {
			latestByAuthor[r.Author] = r
		}
	}

	if len(latestByAuthor) == 0 {
		return "NONE"
	}

	for _, r := range latestByAuthor {
		if r.State == "CHANGES_REQUESTED" {
			return "CHANGES_REQUESTED"
		}
	}

	allApproved := true
	for _, r := range latestByAuthor {
		if r.State != "APPROVED" {
			allApproved = false
			break
		}
	}
	if allApproved {
		return "APPROVED"
	}

	return "REVIEW_REQUIRED"
}

// deriveCIStatus computes the overall CI status based on check runs.
func deriveCIStatus(checkRuns []CheckRun) string {
	if len(checkRuns) == 0 {
		return "NONE"
	}
	hasPending := false
	for _, cr := range checkRuns {
		switch cr.Conclusion {
		case "FAILURE", "TIMED_OUT", "ACTION_REQUIRED", "CANCELLED":
			return "FAILURE"
		case "":
			hasPending = true
		}
	}
	if hasPending {
		return "PENDING"
	}
	return "SUCCESS"
}

// hasNewCommitsSinceReview checks if there are commits after the latest review.
func hasNewCommitsSinceReview(prData PRData) bool {
	if prData.Commits.LatestDate == "" {
		return false
	}
	latestReviewDate := ""
	for _, r := range prData.Reviews {
		if r.SubmittedAt > latestReviewDate {
			latestReviewDate = r.SubmittedAt
		}
	}
	if latestReviewDate == "" {
		return false
	}
	return prData.Commits.LatestDate > latestReviewDate
}

// formatNewCommitsBody builds the body text for a pr_new_commits event.
func formatNewCommitsBody(prData PRData, prevSHA string) string {
	// Find commits newer than prevSHA
	var newCommits []CommitEntry
	foundPrev := false
	for _, c := range prData.Commits.Recent {
		if c.SHA == prevSHA {
			foundPrev = true
			continue
		}
		if foundPrev {
			newCommits = append(newCommits, c)
		}
	}
	// If we didn't find prevSHA in the recent list, show all recent commits
	if !foundPrev {
		newCommits = prData.Commits.Recent
	}

	if len(newCommits) == 0 {
		return fmt.Sprintf("Latest commit: %s", prData.Commits.LatestSHA[:7])
	}

	var lines []string
	for _, c := range newCommits {
		msg := c.MessageHeadline
		if len(msg) > 72 {
			msg = msg[:69] + "..."
		}
		lines = append(lines, fmt.Sprintf("• %s %s", c.SHA[:7], msg))
	}
	return strings.Join(lines, "\n")
}

func buildPRStateJSON(prData PRData) string {
	state := map[string]interface{}{
		"title":                        prData.Title,
		"author":                       prData.Author,
		"state":                        prData.State,
		"review_decision":              derivePRReviewDecision(prData.Reviews),
		"has_new_commits_since_review": hasNewCommitsSinceReview(prData),
		"ci_status":                    deriveCIStatus(prData.CheckRuns),
		"latest_commit_sha":            prData.Commits.LatestSHA,
		"ci_pending_fingerprint":       pendingCIFingerprint(prData),
		"ci_workflow_fingerprint":      workflowFingerprint(prData),
	}
	data, _ := json.Marshal(state)
	return string(data)
}

// pendingCIFingerprint computes a stable fingerprint of the currently
// pending (IN_PROGRESS or QUEUED) check runs for the latest commit, so
// callers can detect when the set of pending checks changes between polls
// even though no check has newly completed (see GitHub issue #2: a
// purely-pending CI bundle would otherwise never refresh). Returns "" when
// there are no pending checks, so callers can distinguish "nothing pending"
// from "pending set unchanged".
//
// Each sorted name is encoded as "<len>:<name>" and segments are joined
// with "\n". GitHub check names can contain commas, spaces, and even
// newlines, so a plain delimiter-joined string (e.g. comma-separated)
// could let two different check sets collide onto the same fingerprint
// (e.g. {"a,b"} vs {"a", "b"}). Length-prefixing each name makes the
// encoding unambiguous regardless of what characters appear in a name,
// since the length tells the reader exactly where each name ends.
func pendingCIFingerprint(prData PRData) string {
	var names []string
	for _, cr := range prData.CheckRuns {
		if cr.Status == "IN_PROGRESS" || cr.Status == "QUEUED" {
			names = append(names, cr.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	segments := make([]string, len(names))
	for i, n := range names {
		segments[i] = fmt.Sprintf("%d:%s", len(n), n)
	}
	return prData.Commits.LatestSHA + "|" + strings.Join(segments, "\n")
}

// workflowFingerprint computes a stable fingerprint of the FULL StatusContext
// set (name + state) for the latest commit, so the workflow bundle refreshes on
// any state transition — not just when the pending set changes. A gated
// workflow flipping SUCCESS→FAILURE with no change to what's pending must still
// refresh the bundle, so unlike pendingCIFingerprint this hashes every context
// and includes its state. Returns "" when there are no StatusContexts, so
// callers can distinguish "no workflows" from "workflow set unchanged".
//
// Uses the same length-prefixed, sorted encoding as pendingCIFingerprint to
// stay collision-free regardless of characters in context names (see its doc).
func workflowFingerprint(prData PRData) string {
	if len(prData.StatusContexts) == 0 {
		return ""
	}
	entries := make([]string, 0, len(prData.StatusContexts))
	for _, sc := range prData.StatusContexts {
		entries = append(entries, fmt.Sprintf("%d:%s=%s", len(sc.Name), sc.Name, sc.State))
	}
	sort.Strings(entries)
	return prData.Commits.LatestSHA + "|" + strings.Join(entries, "\n")
}
