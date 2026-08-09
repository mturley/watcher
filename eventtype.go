package watcher

// EventType is a typed string for watcher event types.
type EventType string

const (
	EventTypePRComment         EventType = "pr_comment"
	EventTypePRReviewComment   EventType = "pr_review_comment"
	EventTypePRReviewRequested EventType = "pr_review_requested"
	EventTypePRApproved        EventType = "pr_approved"
	EventTypePRClosed          EventType = "pr_closed"
	EventTypePRMerged          EventType = "pr_merged"
	EventTypePRReopened        EventType = "pr_reopened"
	EventTypePRNewCommits      EventType = "pr_new_commits"
	EventTypeCICheckPassed     EventType = "ci_check_passed"
	EventTypeCICheckFailed     EventType = "ci_check_failed"
	EventTypeCIPassed          EventType = "ci_passed"
	EventTypeCIFailed          EventType = "ci_failed"
	EventTypeCIPending         EventType = "ci_pending"
	EventTypeCIPartialFailure  EventType = "ci_partial_failure"
	EventTypeJiraComment       EventType = "jira_comment"
	EventTypeJiraStatusChange  EventType = "jira_status_change"
	EventTypeJiraAssigned      EventType = "jira_assigned"
	EventTypeJiraDescChanged   EventType = "jira_description_changed"
	EventTypeJiraLabelsChanged EventType = "jira_labels_changed"
	EventTypeWatchStarted      EventType = "watch_started"
	EventTypeWatcherError      EventType = "watcher_error"
)

// eventTypeDisplayNames maps each EventType to a human-readable label.
// When adding a new EventType constant above, add its display name here too.
var eventTypeDisplayNames = map[EventType]string{
	EventTypePRComment:         "PR comments",
	EventTypePRReviewComment:   "review comments",
	EventTypePRReviewRequested: "review requests",
	EventTypePRApproved:        "approvals",
	EventTypePRClosed:          "PR closed",
	EventTypePRMerged:          "PR merged",
	EventTypePRReopened:        "PR reopened",
	EventTypePRNewCommits:      "new commits",
	EventTypeCICheckPassed:     "CI passed",
	EventTypeCICheckFailed:     "CI failed",
	EventTypeCIPassed:          "CI passed",
	EventTypeCIFailed:          "CI failed",
	EventTypeCIPending:         "CI running",
	EventTypeCIPartialFailure:  "CI failing",
	EventTypeJiraComment:       "Jira comments",
	EventTypeJiraStatusChange:  "status changes",
	EventTypeJiraAssigned:      "assignments",
	EventTypeJiraDescChanged:   "description changes",
	EventTypeJiraLabelsChanged: "label changes",
	EventTypeWatchStarted:      "watch started",
	EventTypeWatcherError:      "watcher errors",
}

// DisplayName returns the human-readable label for an EventType,
// falling back to the raw type string if no mapping exists.
func (t EventType) DisplayName() string {
	if name, ok := eventTypeDisplayNames[t]; ok {
		return name
	}
	return string(t)
}
