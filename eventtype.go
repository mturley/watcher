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
	// Gated/downstream workflows and third-party status checks surface on the
	// PR's statusCheckRollup as StatusContext nodes (e.g. odh-dashboard's
	// "Cypress E2E Tests", OpenShift "ci/prow/*"), distinct from CheckRun
	// nodes. They are rolled up into their own per-commit bundle so a passing
	// CheckRun bundle ("CI checks passed") doesn't imply gated workflows have
	// also finished.
	EventTypeCIWorkflowsPassed         EventType = "ci_workflows_passed"
	EventTypeCIWorkflowsFailed         EventType = "ci_workflows_failed"
	EventTypeCIWorkflowsPending        EventType = "ci_workflows_pending"
	EventTypeCIWorkflowsPartialFailure EventType = "ci_workflows_partial_failure"
	EventTypeJiraComment               EventType = "jira_comment"
	EventTypeJiraStatusChange          EventType = "jira_status_change"
	EventTypeJiraAssigned              EventType = "jira_assigned"
	EventTypeJiraDescChanged           EventType = "jira_description_changed"
	EventTypeJiraLabelsChanged         EventType = "jira_labels_changed"
	EventTypeSlackReply                EventType = "slack_reply"
	EventTypeWatchStarted              EventType = "watch_started"
	EventTypeWatcherError              EventType = "watcher_error"
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
	EventTypeCIPassed:          "CI checks passed",
	EventTypeCIFailed:          "CI failed",
	EventTypeCIPending:         "CI running",
	EventTypeCIPartialFailure:  "CI failing",

	EventTypeCIWorkflowsPassed:         "workflows passed",
	EventTypeCIWorkflowsFailed:         "workflows failed",
	EventTypeCIWorkflowsPending:        "workflows running",
	EventTypeCIWorkflowsPartialFailure: "workflows failing",

	EventTypeJiraComment:       "Jira comments",
	EventTypeJiraStatusChange:  "status changes",
	EventTypeJiraAssigned:      "assignments",
	EventTypeJiraDescChanged:   "description changes",
	EventTypeJiraLabelsChanged: "label changes",
	EventTypeSlackReply:        "Slack replies",
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
