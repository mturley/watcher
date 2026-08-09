// Package fixtures provides canned github.PRData / jira.IssueData
// fixtures for consumers that want to exercise poll-processing logic
// without hitting the network.
//
// github.Poll and jira.Poll fetch data internally (via
// github.FetchPRs / the jira Client), so they cannot be swapped out at
// the Poll layer. MockGitHubPoller and MockJiraPoller instead act as
// canned data sources for tests that drive processing directly (e.g.
// via a consumer's own poll loop built on the fetched data); the
// Sample* constructors below are usually the simpler choice for a
// single fixture.
//
// This lives in its own package, separate from testutil, because
// testutil is imported by github's and jira's own internal test files
// (for testutil.NewTestDB) — importing github/jira back from testutil
// would create an import cycle. Neither github nor jira imports this
// package.
package fixtures

import (
	"github.com/mturley/watcher/github"
	"github.com/mturley/watcher/jira"
)

// MockGitHubPoller holds canned PR data in place of a real GitHub poll.
type MockGitHubPoller struct {
	PRs []github.PRData
}

// FetchPRs returns the poller's canned PR data.
func (m *MockGitHubPoller) FetchPRs() []github.PRData {
	return m.PRs
}

// MockJiraPoller holds canned issue data in place of a real Jira poll.
type MockJiraPoller struct {
	Issues []jira.IssueData
}

// FetchIssues returns the poller's canned issue data.
func (m *MockJiraPoller) FetchIssues() []jira.IssueData {
	return m.Issues
}

// SamplePRData returns a representative github.PRData fixture, with
// sensible defaults that callers can override on the returned value.
func SamplePRData(owner, repo string, number int) github.PRData {
	return github.PRData{
		Number:    number,
		Owner:     owner,
		Repo:      repo,
		State:     "OPEN",
		Title:     "Sample PR",
		UpdatedAt: "2026-08-09T00:00:00Z",
	}
}

// SampleIssueData returns a representative jira.IssueData fixture, with
// sensible defaults that callers can override on the returned value.
func SampleIssueData(key string) jira.IssueData {
	return jira.IssueData{
		Key:       key,
		Summary:   "Sample issue",
		Status:    "In Progress",
		IssueType: "Story",
		CreatedAt: "2026-08-09T00:00:00Z",
		UpdatedAt: "2026-08-09T00:00:00Z",
	}
}
