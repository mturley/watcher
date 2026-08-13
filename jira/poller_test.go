package jira

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
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

// jiraResource is the resource under test across cases.
var jiraResource = watcher.Resource{
	Type: "jira",
	ID:   "RHOAIENG-123",
	URL:  "https://redhat.atlassian.net/browse/RHOAIENG-123",
}

// typeCounts tallies events by type.
func typeCounts(events []watcher.Event) map[watcher.EventType]int {
	out := make(map[watcher.EventType]int)
	for _, e := range events {
		out[e.Type]++
	}
	return out
}

// TestProcessIssue_FirstPoll verifies the first poll (no cursor, no backfill)
// emits only a watch_started marker (which EventsForResource excludes).
func TestProcessIssue_FirstPoll(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", jiraResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	issue := IssueData{
		Key:     "RHOAIENG-123",
		Summary: "Test Issue",
		Status:  "In Progress",
		Comments: []IssueComment{
			{Author: "Jane Smith", CreatedAt: "2026-06-17T09:00:00.000+0000", Body: "hello"},
		},
		Changelog: []ChangelogEntry{
			{Author: "John Doe", CreatedAt: "2026-06-17T08:00:00.000+0000", Field: "status", From: "To Do", To: "In Progress"},
		},
	}

	n, err := processIssue(conn, JiraAuth{}, issue, jiraResource, false, testLogger())
	if err != nil {
		t.Fatalf("processIssue: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 event emitted (watch_started), got %d", n)
	}

	// watch_started is a bookkeeping type excluded by EventsForResource.
	events, err := db.EventsForResource(conn, "jira", jiraResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 consumer-visible events on first poll, got %d: %+v", len(events), events)
	}

	// A cursor should now be set so the next poll processes normally.
	cursor, err := db.EventCursor(conn, "jira", "jira", jiraResource.ID)
	if err != nil {
		t.Fatalf("EventCursor: %v", err)
	}
	if cursor == "" {
		t.Error("expected non-empty cursor after watch_started")
	}
}

// TestProcessIssue_SubsequentPoll verifies that once a cursor exists, new
// comments and changelog entries after the cursor are emitted with the
// correct event types, titles, and author metadata.
func TestProcessIssue_SubsequentPoll(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", jiraResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Establish a cursor via a first poll (uses latest ts = 2026-06-17T08:00).
	seed := IssueData{
		Key: "RHOAIENG-123", Summary: "Test Issue", Status: "To Do",
		Changelog: []ChangelogEntry{
			{Author: "John Doe", CreatedAt: "2026-06-17T08:00:00.000+0000", Field: "status", From: "Backlog", To: "To Do"},
		},
	}
	if _, err := processIssue(conn, JiraAuth{}, seed, jiraResource, false, testLogger()); err != nil {
		t.Fatalf("seed processIssue: %v", err)
	}

	// Now newer activity arrives, all after the cursor.
	issue := IssueData{
		Key: "RHOAIENG-123", Summary: "Test Issue", Status: "Done",
		Comments: []IssueComment{
			{Author: "Jane Smith", CreatedAt: "2026-06-17T09:00:00.000+0000", Body: "Looks good"},
		},
		Changelog: []ChangelogEntry{
			{Author: "John Doe", CreatedAt: "2026-06-17T10:00:00.000+0000", Field: "status", From: "In Progress", To: "Done"},
			{Author: "John Doe", CreatedAt: "2026-06-17T10:30:00.000+0000", Field: "assignee", From: "", To: "Alice"},
			{Author: "John Doe", CreatedAt: "2026-06-17T11:00:00.000+0000", Field: "description", From: "old", To: "new"},
			{Author: "John Doe", CreatedAt: "2026-06-17T11:30:00.000+0000", Field: "labels", From: "bug", To: "bug urgent"},
		},
	}
	if _, err := processIssue(conn, JiraAuth{}, issue, jiraResource, false, testLogger()); err != nil {
		t.Fatalf("processIssue: %v", err)
	}

	events, err := db.EventsForResource(conn, "jira", jiraResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	types := typeCounts(events)
	if types[watcher.EventTypeJiraComment] != 1 {
		t.Errorf("expected 1 jira_comment, got %d", types[watcher.EventTypeJiraComment])
	}
	if types[watcher.EventTypeJiraStatusChange] != 1 {
		t.Errorf("expected 1 jira_status_change, got %d", types[watcher.EventTypeJiraStatusChange])
	}
	if types[watcher.EventTypeJiraAssigned] != 1 {
		t.Errorf("expected 1 jira_assigned, got %d", types[watcher.EventTypeJiraAssigned])
	}
	if types[watcher.EventTypeJiraDescChanged] != 1 {
		t.Errorf("expected 1 jira_description_changed, got %d", types[watcher.EventTypeJiraDescChanged])
	}
	if types[watcher.EventTypeJiraLabelsChanged] != 1 {
		t.Errorf("expected 1 jira_labels_changed, got %d", types[watcher.EventTypeJiraLabelsChanged])
	}

	// Verify the status change title and author metadata.
	for _, e := range events {
		if e.Type == watcher.EventTypeJiraStatusChange {
			if e.Title != "RHOAIENG-123: In Progress → Done" {
				t.Errorf("expected status change title 'RHOAIENG-123: In Progress → Done', got %q", e.Title)
			}
			if e.Author == nil || *e.Author != "John Doe" {
				t.Errorf("expected author 'John Doe', got %v", e.Author)
			}
			if e.AuthorType == nil || *e.AuthorType != "human" {
				t.Errorf("expected author_type 'human', got %v", e.AuthorType)
			}
		}
	}
}

// TestProcessIssue_ADFCommentExtraction is a regression test: a comment whose
// body was an ADF document must surface as extracted plain text via the
// jira_comment event. (Extraction happens in the client; here we assert the
// poller carries the already-extracted body through unchanged.)
func TestProcessIssue_ADFCommentExtraction(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", jiraResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Simulate the ADF extraction the client performs: a nested ADF document
	// reduced to plain text.
	adfBody := map[string]interface{}{
		"type":    "doc",
		"version": 1,
		"content": []interface{}{
			map[string]interface{}{
				"type": "paragraph",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "Hello "},
					map[string]interface{}{"type": "text", "text": "world"},
				},
			},
		},
	}
	extracted := extractADFText(adfBody)
	if extracted == "" || extracted != "Hello world" {
		t.Fatalf("client ADF extraction produced unexpected text: %q", extracted)
	}

	// Seed a cursor so the comment is emitted (not swallowed by first-poll).
	if _, err := processIssue(conn, JiraAuth{}, IssueData{
		Key: "RHOAIENG-123", Summary: "Test Issue", Status: "Open",
		Changelog: []ChangelogEntry{
			{Author: "x", CreatedAt: "2026-06-17T00:00:00.000+0000", Field: "status", From: "a", To: "b"},
		},
	}, jiraResource, false, testLogger()); err != nil {
		t.Fatalf("seed processIssue: %v", err)
	}

	issue := IssueData{
		Key: "RHOAIENG-123", Summary: "Test Issue", Status: "Open",
		Comments: []IssueComment{
			{Author: "Jane Smith", CreatedAt: "2026-06-17T09:00:00.000+0000", Body: extracted},
		},
	}
	if _, err := processIssue(conn, JiraAuth{}, issue, jiraResource, false, testLogger()); err != nil {
		t.Fatalf("processIssue: %v", err)
	}

	events, err := db.EventsForResource(conn, "jira", jiraResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Type == watcher.EventTypeJiraComment {
			found = true
			if e.Body == nil || *e.Body != "Hello world" {
				t.Errorf("expected jira_comment body 'Hello world' (extracted ADF text), got %v", e.Body)
			}
		}
	}
	if !found {
		t.Error("expected a jira_comment event, none found")
	}
}

// TestProcessIssue_EpicLinkRelationship is a regression test: an issue with an
// epic-link custom field must record a watcher_resource_relationships row
// linking the issue (child) to its epic (parent) with relationship "epic".
func TestProcessIssue_EpicLinkRelationship(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", jiraResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	issue := IssueData{
		Key: "RHOAIENG-123", Summary: "Test Issue", Status: "Open",
		CustomFields: map[string]interface{}{
			"epic_key": "RHOAIENG-100",
		},
	}
	if _, err := processIssue(conn, JiraAuth{}, issue, jiraResource, false, testLogger()); err != nil {
		t.Fatalf("processIssue: %v", err)
	}

	var childType, childID, parentType, parentID, relationship, source string
	err := conn.QueryRow(`
		SELECT child_type, child_id, parent_type, parent_id, relationship, source
		FROM watcher_resource_relationships
		WHERE child_id = ? AND parent_id = ?
	`, "RHOAIENG-123", "RHOAIENG-100").Scan(&childType, &childID, &parentType, &parentID, &relationship, &source)
	if err != nil {
		t.Fatalf("expected an epic relationship row, query failed: %v", err)
	}
	if childType != "jira" || childID != "RHOAIENG-123" {
		t.Errorf("unexpected child: %s/%s", childType, childID)
	}
	if parentType != "jira" || parentID != "RHOAIENG-100" {
		t.Errorf("unexpected parent: %s/%s", parentType, parentID)
	}
	if relationship != "epic" {
		t.Errorf("expected relationship 'epic', got %q", relationship)
	}
	if source != "jira" {
		t.Errorf("expected source 'jira', got %q", source)
	}
}

// TestProcessIssue_Dedup verifies that re-processing the same issue data does
// not emit duplicate events.
func TestProcessIssue_Dedup(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", jiraResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Seed a cursor before the comment timestamp.
	if _, err := processIssue(conn, JiraAuth{}, IssueData{
		Key: "RHOAIENG-123", Summary: "Test Issue", Status: "Open",
		Changelog: []ChangelogEntry{
			{Author: "x", CreatedAt: "2026-06-17T00:00:00.000+0000", Field: "status", From: "a", To: "b"},
		},
	}, jiraResource, false, testLogger()); err != nil {
		t.Fatalf("seed processIssue: %v", err)
	}

	issue := IssueData{
		Key: "RHOAIENG-123", Summary: "Test Issue", Status: "Open",
		Comments: []IssueComment{
			{Author: "Jane Smith", CreatedAt: "2026-06-17T09:00:00.000+0000", Body: "hi"},
		},
	}
	if _, err := processIssue(conn, JiraAuth{}, issue, jiraResource, false, testLogger()); err != nil {
		t.Fatalf("processIssue #1: %v", err)
	}
	if _, err := processIssue(conn, JiraAuth{}, issue, jiraResource, false, testLogger()); err != nil {
		t.Fatalf("processIssue #2: %v", err)
	}

	events, err := db.EventsForResource(conn, "jira", jiraResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	if got := typeCounts(events)[watcher.EventTypeJiraComment]; got != 1 {
		t.Errorf("expected exactly 1 jira_comment after dedup, got %d", got)
	}
}

// TestProcessIssue_BotAuthorType verifies that a comment by a configured bot
// username is labeled author_type "bot".
func TestProcessIssue_BotAuthorType(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", jiraResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	cfg := JiraAuth{BotUsernames: []string{"Automation Bot"}}

	// Seed a cursor.
	if _, err := processIssue(conn, cfg, IssueData{
		Key: "RHOAIENG-123", Summary: "Test", Status: "Open",
		Changelog: []ChangelogEntry{
			{Author: "x", CreatedAt: "2026-06-17T00:00:00.000+0000", Field: "status", From: "a", To: "b"},
		},
	}, jiraResource, false, testLogger()); err != nil {
		t.Fatalf("seed processIssue: %v", err)
	}

	issue := IssueData{
		Key: "RHOAIENG-123", Summary: "Test", Status: "Open",
		Comments: []IssueComment{
			{Author: "Automation Bot", CreatedAt: "2026-06-17T09:00:00.000+0000", Body: "auto"},
		},
	}
	if _, err := processIssue(conn, cfg, issue, jiraResource, false, testLogger()); err != nil {
		t.Fatalf("processIssue: %v", err)
	}

	events, err := db.EventsForResource(conn, "jira", jiraResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	for _, e := range events {
		if e.Type == watcher.EventTypeJiraComment {
			if e.AuthorType == nil || *e.AuthorType != "bot" {
				t.Errorf("expected author_type 'bot', got %v", e.AuthorType)
			}
		}
	}
}

// TestProcessIssue_Backfill verifies that a first poll with backfill enabled
// emits all history (comments and changelog entries) rather than only a
// watch_started marker. This is the symmetric case to
// TestProcessIssue_FirstPoll, which asserts the contrasting backfill=false
// behavior (watch_started only). This must fail if processIssue is changed
// to return early on first poll regardless of the backfill flag.
func TestProcessIssue_Backfill(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", jiraResource, db.SubscribeOpts{Backfill: true}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	backfill, err := db.BackfillFor(conn, "jira", jiraResource.ID)
	if err != nil {
		t.Fatalf("BackfillFor: %v", err)
	}
	if !backfill {
		t.Fatal("expected BackfillFor to report true")
	}

	issue := IssueData{
		Key:     "RHOAIENG-123",
		Summary: "Test Issue",
		Status:  "In Progress",
		Comments: []IssueComment{
			{Author: "Jane Smith", CreatedAt: "2026-06-17T09:00:00.000+0000", Body: "hello"},
		},
		Changelog: []ChangelogEntry{
			{Author: "John Doe", CreatedAt: "2026-06-17T08:00:00.000+0000", Field: "status", From: "To Do", To: "In Progress"},
		},
	}

	n, err := processIssue(conn, JiraAuth{}, issue, jiraResource, backfill, testLogger())
	if err != nil {
		t.Fatalf("processIssue: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 events emitted from backfill (comment + status change), got %d", n)
	}

	events, err := db.EventsForResource(conn, "jira", jiraResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	types := typeCounts(events)
	if types[watcher.EventTypeJiraComment] != 1 {
		t.Errorf("expected 1 jira_comment from backfill, got %d", types[watcher.EventTypeJiraComment])
	}
	if types[watcher.EventTypeJiraStatusChange] != 1 {
		t.Errorf("expected 1 jira_status_change from backfill, got %d", types[watcher.EventTypeJiraStatusChange])
	}

	// EventsForResource excludes bookkeeping types, so query the raw table
	// directly to confirm no watch_started was emitted alongside the
	// backfilled history.
	var watchStartedCount int
	if err := conn.QueryRow(`
		SELECT COUNT(*) FROM watcher_events e
		JOIN watcher_event_resources er ON er.event_id = e.id
		WHERE er.resource_type = ? AND er.resource_id = ? AND e.type = ?
	`, "jira", jiraResource.ID, string(watcher.EventTypeWatchStarted)).Scan(&watchStartedCount); err != nil {
		t.Fatalf("query watch_started count: %v", err)
	}
	if watchStartedCount != 0 {
		t.Errorf("expected no watch_started event when backfilling, got %d", watchStartedCount)
	}
}

// TestFetchIssue_ChangelogPagination is a client-level regression test for the
// changelog pagination fix. A fake Jira server returns the changelog across two
// pages (isLast=false then isLast=true). FetchIssue must return ALL entries
// from both pages, proving the old expand=changelog 100-entry cap is gone.
func TestFetchIssue_ChangelogPagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/rest/api/3/issue/RHOAIENG-123":
			// Fields-only issue GET.
			json.NewEncoder(w).Encode(map[string]interface{}{
				"key": "RHOAIENG-123",
				"fields": map[string]interface{}{
					"summary": "Paginated Issue",
					"status":  map[string]interface{}{"name": "Open"},
				},
			})
		case r.URL.Path == "/rest/api/3/issue/RHOAIENG-123/changelog":
			startAt := r.URL.Query().Get("startAt")
			if startAt == "" || startAt == "0" {
				// Page 1: 100 entries, not last.
				json.NewEncoder(w).Encode(changelogPage(0, 150, false, 100))
			} else {
				// Page 2: remaining 50 entries, last.
				json.NewEncoder(w).Encode(changelogPage(100, 150, true, 50))
			}
		case r.URL.Path == "/rest/api/3/issue/RHOAIENG-123/comment":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"startAt": 0, "maxResults": 100, "total": 0, "comments": []interface{}{},
			})
		default:
			http.Error(w, "not found: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, Email: "e@x.com", Token: "t"}
	issue, err := client.FetchIssue("RHOAIENG-123", nil)
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}

	// 150 histories, each with exactly 1 item -> 150 changelog entries.
	if len(issue.Changelog) != 150 {
		t.Errorf("expected 150 changelog entries across both pages, got %d", len(issue.Changelog))
	}
	// Verify the last entry (from page 2) is present, proving nothing was dropped.
	last := issue.Changelog[len(issue.Changelog)-1]
	if last.To != "status-149" {
		t.Errorf("expected last changelog entry To='status-149', got %q", last.To)
	}
}

// TestFetchIssue_Reporter verifies that the issue's reporter display name is
// parsed into IssueData.Reporter, so it can be cached in the poller's state
// JSON for downstream consumers (e.g. the worktree UI).
func TestFetchIssue_Reporter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/rest/api/3/issue/RHOAIENG-456":
			// Verify reporter is requested in the explicit fields list.
			if !strings.Contains(r.URL.Query().Get("fields"), "reporter") {
				t.Errorf("expected fields query param to include \"reporter\", got %q", r.URL.Query().Get("fields"))
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"key": "RHOAIENG-456",
				"fields": map[string]interface{}{
					"summary":  "Issue with reporter",
					"status":   map[string]interface{}{"name": "Open"},
					"reporter": map[string]interface{}{"displayName": "Bob Reporter"},
				},
			})
		case r.URL.Path == "/rest/api/3/issue/RHOAIENG-456/changelog":
			json.NewEncoder(w).Encode(changelogPage(0, 0, true, 0))
		case r.URL.Path == "/rest/api/3/issue/RHOAIENG-456/comment":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"startAt": 0, "maxResults": 100, "total": 0, "comments": []interface{}{},
			})
		default:
			http.Error(w, "not found: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, Email: "e@x.com", Token: "t"}
	issue, err := client.FetchIssue("RHOAIENG-456", nil)
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}

	if issue.Reporter == nil {
		t.Fatal("expected Reporter to be set, got nil")
	}
	if got, want := *issue.Reporter, "Bob Reporter"; got != want {
		t.Errorf("Reporter = %q, want %q", got, want)
	}
}

// TestBuildJiraStateJSON_Reporter verifies the cached Jira state JSON
// includes the issue reporter's display name.
func TestBuildJiraStateJSON_Reporter(t *testing.T) {
	reporter := "Bob Reporter"
	issue := &IssueData{
		Key:      "RHOAIENG-456",
		Summary:  "Issue with reporter",
		Status:   "Open",
		Reporter: &reporter,
	}

	stateJSON := buildJiraStateJSON(issue)

	var state map[string]interface{}
	if err := json.Unmarshal([]byte(stateJSON), &state); err != nil {
		t.Fatalf("failed to unmarshal state JSON: %v", err)
	}

	if got, want := state["reporter"], reporter; got != want {
		t.Errorf("state[\"reporter\"] = %v, want %v", got, want)
	}
}

// TestNormalizeBaseURL is a regression test: a bare host (as documented in
// the README config example, e.g. "redhat.atlassian.net") must get an
// "https://" scheme prepended, since request URLs are built via
// fmt.Sprintf("%s/rest/api/3/...", BaseURL) and a schemeless host produces a
// malformed request. An already-schemed URL must be left unchanged.
func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"redhat.atlassian.net", "https://redhat.atlassian.net"},
		{"https://redhat.atlassian.net", "https://redhat.atlassian.net"},
		{"http://localhost:8080", "http://localhost:8080"},
	}
	for _, c := range cases {
		if got := normalizeBaseURL(c.in); got != c.want {
			t.Errorf("normalizeBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// changelogPage builds a fake /changelog page response containing histories
// for indices [start, total), capped at count entries, with the given isLast.
func changelogPage(start, total int, isLast bool, count int) map[string]interface{} {
	var values []interface{}
	for i := start; i < start+count && i < total; i++ {
		values = append(values, map[string]interface{}{
			"author":  map[string]interface{}{"displayName": "John Doe"},
			"created": fmt.Sprintf("2026-06-17T%02d:00:00.000+0000", i%24),
			"items": []interface{}{
				map[string]interface{}{
					"field":      "status",
					"fromString": "prev",
					"toString":   fmt.Sprintf("status-%d", i),
				},
			},
		})
	}
	return map[string]interface{}{
		"startAt":    start,
		"maxResults": count,
		"total":      total,
		"isLast":     isLast,
		"values":     values,
	}
}
