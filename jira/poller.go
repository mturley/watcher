package jira

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

// JiraAuth holds the credentials and configuration needed to poll Jira.
type JiraAuth struct {
	URL          string
	Email        string
	Token        string
	CustomFields map[string]string
	BotUsernames []string
}

// Poll polls Jira for issue updates and emits events. Each resource's
// backfill flag is resolved from its live subscriptions.
func Poll(conn *sql.DB, cfg JiraAuth, resources []watcher.Resource, logger *log.Logger) error {
	if cfg.Token == "" {
		return fmt.Errorf("Jira token not configured")
	}

	client := &Client{
		BaseURL: normalizeBaseURL(cfg.URL),
		Email:   cfg.Email,
		Token:   cfg.Token,
	}

	customFields := cfg.CustomFields
	if customFields == nil {
		customFields = make(map[string]string)
	}

	eventCount := 0
	for _, resource := range resources {
		// The resource ID for a Jira issue is just the issue key.
		issueKey := resource.ID

		logger.Printf("Fetching issue %s...", issueKey)
		issueData, err := client.FetchIssue(issueKey, customFields)
		if err != nil {
			logger.Printf("ERROR: failed to fetch issue %s: %v", issueKey, err)
			errBody := fmt.Sprintf("Failed to fetch issue: %v", err)
			if err := emitError(conn, fmt.Sprintf("Failed to fetch %s", issueKey), &errBody, resource); err != nil {
				logger.Printf("ERROR: failed to emit watcher error: %v", err)
			}
			if err := db.RecordPollerError(conn, "jira", errBody); err != nil {
				logger.Printf("ERROR: failed to record poller error: %v", err)
			}
			continue
		}

		backfill, err := db.BackfillFor(conn, resource.Type, resource.ID)
		if err != nil {
			logger.Printf("WARNING: failed to resolve backfill for %s: %v", issueKey, err)
		}

		count, err := processIssue(conn, cfg, *issueData, resource, backfill, logger)
		if err != nil {
			logger.Printf("ERROR: failed to process issue %s: %v", issueKey, err)
			errBody := fmt.Sprintf("Failed to process issue: %v", err)
			if err := emitError(conn, fmt.Sprintf("Error processing %s", issueKey), &errBody, resource); err != nil {
				logger.Printf("ERROR: failed to emit watcher error: %v", err)
			}
			continue
		}
		eventCount += count

		// Write resource state
		stateJSON := buildJiraStateJSON(issueData)
		now := time.Now().UTC().Format(time.RFC3339)
		if err := db.UpsertResourceState(conn, "jira", issueKey, stateJSON, issueData.UpdatedAt, now); err != nil {
			logger.Printf("WARNING: failed to upsert resource state for %s: %v", issueKey, err)
		}
	}

	logger.Printf("Emitted %d events", eventCount)
	if err := db.RecordPollerSuccess(conn, "jira"); err != nil {
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
		Source:     "jira",
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
	if db.HasPollerError(conn, "jira") {
		return nil
	}
	return db.InsertEvent(conn, watcher.Event{
		ID:     uuid.New().String(),
		TS:     time.Now().UTC().Format(time.RFC3339),
		Source: "jira",
		Type:   watcher.EventTypeWatcherError,
		Title:  title,
		Body:   body,
	}, r)
}

// processIssue processes a single Jira issue and emits events.
// Returns the count of events emitted. When cursor is empty and backfill
// is true, all history is emitted; when backfill is false, only a
// watch_started event is emitted on the first poll.
func processIssue(conn *sql.DB, cfg JiraAuth, issue IssueData, resource watcher.Resource, backfill bool, logger *log.Logger) (int, error) {
	eventCount := 0

	// Discover epic link and record the relationship. This is the only
	// automated relationship writer.
	if epicKey := epicKeyFromCustomFields(issue.CustomFields); epicKey != "" {
		parent := watcher.Resource{Type: "jira", ID: epicKey}
		if err := db.LinkResources(conn, resource, parent, "epic", "jira"); err != nil {
			logger.Printf("WARNING: failed to link %s to epic %s: %v", resource.ID, epicKey, err)
		} else {
			logger.Printf("Linked %s to epic %s", resource.ID, epicKey)
		}
	}

	// Get cursor (last seen external timestamp)
	cursor, err := db.EventCursor(conn, "jira", resource.Type, resource.ID)
	if err != nil {
		return eventCount, fmt.Errorf("failed to get event cursor: %w", err)
	}

	// First poll without backfill: emit watch_started event and return.
	// With backfill, fall through to normal processing so all history is
	// emitted (the item<=cursor guards are false for any real timestamp
	// vs "").
	if cursor == "" && !backfill {
		title := fmt.Sprintf("Started watching issue: %s", issue.Summary)
		body := fmt.Sprintf("%s\nStatus: %s", issue.Key, issue.Status)
		// Use the most recent timestamp from the issue (latest changelog or comment)
		latestTS := latestTimestamp(issue)
		if err := emitEvent(conn, watcher.EventTypeWatchStarted, title, &body, latestTS, nil, nil, resource); err != nil {
			return eventCount, fmt.Errorf("failed to emit watch_started event: %w", err)
		}
		eventCount++
		logger.Printf("Emitted watch_started for %s", resource.ID)
		return eventCount, nil
	}

	// Process new comments
	for _, comment := range issue.Comments {
		if comment.CreatedAt <= cursor {
			continue
		}

		dup, err := db.IsDuplicate(conn, db.DedupCheck{
			Source:       "jira",
			ResourceType: resource.Type,
			ResourceID:   resource.ID,
			Type:         watcher.EventTypeJiraComment,
			ExternalTS:   &comment.CreatedAt,
		})
		if err != nil {
			return eventCount, fmt.Errorf("failed to check comment duplicate: %w", err)
		}
		if dup {
			continue
		}

		title := fmt.Sprintf("Comment by %s on %s", comment.Author, issue.Key)
		authorType := authorTypeFromUsername(cfg.BotUsernames, comment.Author)
		body := comment.Body
		if err := emitEvent(conn, watcher.EventTypeJiraComment, title, &body, comment.CreatedAt, &comment.Author, &authorType, resource); err != nil {
			return eventCount, fmt.Errorf("failed to emit jira_comment event: %w", err)
		}
		eventCount++
		logger.Printf("Emitted jira_comment for %s by %s", resource.ID, comment.Author)
	}

	// Process changelog entries
	for _, entry := range issue.Changelog {
		if entry.CreatedAt <= cursor {
			continue
		}

		switch entry.Field {
		case "status":
			dup, err := db.IsDuplicate(conn, db.DedupCheck{
				Source: "jira", ResourceType: resource.Type, ResourceID: resource.ID,
				Type: watcher.EventTypeJiraStatusChange, ExternalTS: &entry.CreatedAt,
			})
			if err != nil {
				return eventCount, fmt.Errorf("failed to check status change duplicate: %w", err)
			}
			if dup {
				continue
			}
			title := fmt.Sprintf("%s: %s → %s", issue.Key, entry.From, entry.To)
			authorType := authorTypeFromUsername(cfg.BotUsernames, entry.Author)
			author := entry.Author
			if err := emitEvent(conn, watcher.EventTypeJiraStatusChange, title, nil, entry.CreatedAt, &author, &authorType, resource); err != nil {
				return eventCount, fmt.Errorf("failed to emit jira_status_change event: %w", err)
			}
			eventCount++
			logger.Printf("Emitted jira_status_change for %s: %s → %s", resource.ID, entry.From, entry.To)

		case "assignee":
			dup, err := db.IsDuplicate(conn, db.DedupCheck{
				Source: "jira", ResourceType: resource.Type, ResourceID: resource.ID,
				Type: watcher.EventTypeJiraAssigned, ExternalTS: &entry.CreatedAt,
			})
			if err != nil {
				return eventCount, fmt.Errorf("failed to check assignee change duplicate: %w", err)
			}
			if dup {
				continue
			}
			title := fmt.Sprintf("%s assigned to %s", issue.Key, entry.To)
			authorType := authorTypeFromUsername(cfg.BotUsernames, entry.Author)
			author := entry.Author
			if err := emitEvent(conn, watcher.EventTypeJiraAssigned, title, nil, entry.CreatedAt, &author, &authorType, resource); err != nil {
				return eventCount, fmt.Errorf("failed to emit jira_assigned event: %w", err)
			}
			eventCount++
			logger.Printf("Emitted jira_assigned for %s: %s", resource.ID, entry.To)

		case "description":
			dup, err := db.IsDuplicate(conn, db.DedupCheck{
				Source: "jira", ResourceType: resource.Type, ResourceID: resource.ID,
				Type: watcher.EventTypeJiraDescChanged, ExternalTS: &entry.CreatedAt,
			})
			if err != nil {
				return eventCount, fmt.Errorf("failed to check description change duplicate: %w", err)
			}
			if dup {
				continue
			}
			title := fmt.Sprintf("%s description changed", issue.Key)
			authorType := authorTypeFromUsername(cfg.BotUsernames, entry.Author)
			author := entry.Author
			if err := emitEvent(conn, watcher.EventTypeJiraDescChanged, title, nil, entry.CreatedAt, &author, &authorType, resource); err != nil {
				return eventCount, fmt.Errorf("failed to emit jira_description_changed event: %w", err)
			}
			eventCount++
			logger.Printf("Emitted jira_description_changed for %s", resource.ID)

		case "labels":
			dup, err := db.IsDuplicate(conn, db.DedupCheck{
				Source: "jira", ResourceType: resource.Type, ResourceID: resource.ID,
				Type: watcher.EventTypeJiraLabelsChanged, ExternalTS: &entry.CreatedAt,
			})
			if err != nil {
				return eventCount, fmt.Errorf("failed to check labels change duplicate: %w", err)
			}
			if dup {
				continue
			}
			title := labelChangeTitle(issue.Key, entry.From, entry.To)
			authorType := authorTypeFromUsername(cfg.BotUsernames, entry.Author)
			author := entry.Author
			if err := emitEvent(conn, watcher.EventTypeJiraLabelsChanged, title, nil, entry.CreatedAt, &author, &authorType, resource); err != nil {
				return eventCount, fmt.Errorf("failed to emit jira_labels_changed event: %w", err)
			}
			eventCount++
			logger.Printf("Emitted jira_labels_changed for %s", resource.ID)

		default:
			// Skip other fields
			continue
		}
	}

	// Don't auto-unsubscribe on terminal status — the terminal event needs
	// to be delivered to subscribers via the subscription join. Let
	// subscribers or users unsubscribe explicitly.

	return eventCount, nil
}

// epicKeyFromCustomFields returns the epic issue key discovered from an
// issue's custom fields, or "" if none is present. The epic link is
// conventionally exposed under the "epic_key" custom field display name.
func epicKeyFromCustomFields(customFields map[string]interface{}) string {
	v, ok := customFields["epic_key"]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// authorTypeFromUsername determines if a username is a bot.
func authorTypeFromUsername(botUsernames []string, username string) string {
	for _, bot := range botUsernames {
		if username == bot {
			return "bot"
		}
	}
	return "human"
}

// labelChangeTitle creates a title for label changes showing +added and -removed.
func labelChangeTitle(issueKey, from, to string) string {
	fromLabels := parseLabels(from)
	toLabels := parseLabels(to)

	var added, removed []string
	fromSet := make(map[string]bool)
	toSet := make(map[string]bool)

	for _, label := range fromLabels {
		fromSet[label] = true
	}
	for _, label := range toLabels {
		toSet[label] = true
	}

	for _, label := range toLabels {
		if !fromSet[label] {
			added = append(added, label)
		}
	}
	for _, label := range fromLabels {
		if !toSet[label] {
			removed = append(removed, label)
		}
	}

	var parts []string
	if len(added) > 0 {
		parts = append(parts, "+"+strings.Join(added, " +"))
	}
	if len(removed) > 0 {
		parts = append(parts, "-"+strings.Join(removed, " -"))
	}

	if len(parts) == 0 {
		return fmt.Sprintf("%s labels changed", issueKey)
	}

	return fmt.Sprintf("%s labels: %s", issueKey, strings.Join(parts, ", "))
}

// parseLabels parses space-separated labels from a string.
func parseLabels(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}

// latestTimestamp returns the most recent timestamp from an issue's comments and changelog.
func latestTimestamp(issue IssueData) string {
	latest := ""
	for _, comment := range issue.Comments {
		if comment.CreatedAt > latest {
			latest = comment.CreatedAt
		}
	}
	for _, entry := range issue.Changelog {
		if entry.CreatedAt > latest {
			latest = entry.CreatedAt
		}
	}
	// If no comments or changelog, use a default timestamp
	if latest == "" {
		latest = "2000-01-01T00:00:00.000+0000"
	}
	return latest
}

// buildJiraStateJSON builds a JSON representation of a Jira issue's current state.
func buildJiraStateJSON(issue *IssueData) string {
	state := map[string]interface{}{
		"summary":    issue.Summary,
		"status":     issue.Status,
		"priority":   issue.Priority,
		"assignee":   issue.Assignee,
		"reporter":   issue.Reporter,
		"issue_type": issue.IssueType,
		// Cached so the UI can render Jira's own icon for the type. Requires
		// auth to fetch, so consumers proxy it rather than using it directly.
		"issue_type_id":       issue.IssueTypeID,
		"issue_type_icon_url": issue.IssueTypeIconURL,
		"labels":              issue.Labels,
		"created_at":          issue.CreatedAt,
		"updated_at":          issue.UpdatedAt,
	}
	for k, v := range issue.CustomFields {
		state[k] = v
	}
	data, _ := json.Marshal(state)
	return string(data)
}
