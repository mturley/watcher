package github

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
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

	// Process reviews
	for _, review := range prData.Reviews {
		if review.SubmittedAt <= cursor {
			continue
		}

		// Skip duplicate events
		dup, err := db.IsDuplicate(conn, db.DedupCheck{
			Source:       "github",
			ResourceType: resource.Type,
			ResourceID:   resource.ID,
			Type:         reviewEventType(review.State),
			ExternalTS:   &review.SubmittedAt,
		})
		if err != nil {
			return eventCount, fmt.Errorf("failed to check review duplicate: %w", err)
		}
		if dup {
			continue
		}

		// Emit event based on review state
		if review.State == "APPROVED" {
			title := fmt.Sprintf("PR approved by %s", review.Author)
			if err := emitEvent(conn, watcher.EventTypePRApproved, title, &review.Body, review.SubmittedAt, &review.Author, &review.AuthorType, resource); err != nil {
				return eventCount, fmt.Errorf("failed to emit pr_approved event: %w", err)
			}
			eventCount++
			logger.Printf("Emitted pr_approved for %s by %s", resource.ID, review.Author)
		} else if review.State == "CHANGES_REQUESTED" {
			title := fmt.Sprintf("Changes requested by %s", review.Author)
			if err := emitEvent(conn, watcher.EventTypePRReviewComment, title, &review.Body, review.SubmittedAt, &review.Author, &review.AuthorType, resource); err != nil {
				return eventCount, fmt.Errorf("failed to emit pr_review_comment event: %w", err)
			}
			eventCount++
			logger.Printf("Emitted pr_review_comment for %s by %s", resource.ID, review.Author)
		}
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
	// Batch-submitted reviews have multiple comments with the same createdAt.
	// Use title-based dedup (which includes the file path) instead of
	// timestamp-only dedup.
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

	// Fetch remaining check contexts if paginated
	if prData.CheckRunsHasMore && prData.CheckRunsEndCursor != "" {
		moreRuns, err := FetchRemainingCheckContexts(token, prData.Owner, prData.Repo, prData.Number, prData.CheckRunsEndCursor)
		if err != nil {
			logger.Printf("WARNING: failed to paginate check contexts for %s: %v", resource.ID, err)
		} else {
			prData.CheckRuns = append(prData.CheckRuns, moreRuns...)
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

		if hasNewChecks || cursor == "" {
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
				title = fmt.Sprintf("CI passed for %s @ %s (%d/%d checks)", prLabel, shortSHA, passed, total)
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

			if err := db.UpsertCIBundle(conn, prData.Commits.LatestSHA, eventType, title, body, latestTS, resource); err != nil {
				return eventCount, fmt.Errorf("failed to upsert CI bundle: %w", err)
			}
			eventCount++
			logger.Printf("Upserted CI bundle for %s (%s): %s", resource.ID, prData.Commits.LatestSHA[:8], title)
		}
	}
skipCIBundle:

	// Check PR state
	if prData.State == "MERGED" || prData.State == "CLOSED" {
		eventType := watcher.EventTypePRMerged
		if prData.State == "CLOSED" {
			eventType = watcher.EventTypePRClosed
		}

		dup, err := db.IsDuplicate(conn, db.DedupCheck{
			Source:       "github",
			ResourceType: resource.Type,
			ResourceID:   resource.ID,
			Type:         eventType,
			ExternalTS:   &prData.UpdatedAt,
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
	if prData.Commits.LatestSHA != "" {
		prevState, _ := db.GetResourceState(conn, "pr", resource.ID)
		if prevState != nil {
			var prev map[string]interface{}
			if json.Unmarshal([]byte(prevState.StateJSON), &prev) == nil {
				prevSHA, _ := prev["latest_commit_sha"].(string)
				if prevSHA != "" && prevSHA != prData.Commits.LatestSHA {
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
		"state":                        prData.State,
		"review_decision":              derivePRReviewDecision(prData.Reviews),
		"has_new_commits_since_review": hasNewCommitsSinceReview(prData),
		"ci_status":                    deriveCIStatus(prData.CheckRuns),
		"latest_commit_sha":            prData.Commits.LatestSHA,
	}
	data, _ := json.Marshal(state)
	return string(data)
}
