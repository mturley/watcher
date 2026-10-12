package jira

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIssueTypeIconCaptured pins that we keep the issue type's iconUrl, not
// just its name. agent-handler removed this exact capture in 2026-08 because
// the URLs 401 in a browser <img>; the answer is a server-side proxy that can
// re-attach Basic auth, which needs the URL to survive this far in the first
// place.
func TestIssueTypeIconCaptured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"key": "PROJ-1",
			"fields": {
				"summary": "A bug",
				"status": {"name": "In Progress"},
				"priority": {"name": "High"},
				"issuetype": {
					"id": "10004",
					"name": "Bug",
					"iconUrl": "https://example.atlassian.net/rest/api/2/universal_avatar/view/type/issuetype/avatar/10303?size=medium"
				},
				"labels": [],
				"created": "2026-01-01T00:00:00.000+0000",
				"updated": "2026-01-02T00:00:00.000+0000"
			}
		}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Email: "user@example.com", Token: "token"}
	issue, err := c.FetchIssue("PROJ-1", nil)
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}
	if issue.IssueType != "Bug" {
		t.Errorf("IssueType = %q, want Bug", issue.IssueType)
	}
	if issue.IssueTypeID != "10004" {
		t.Errorf("IssueTypeID = %q, want 10004", issue.IssueTypeID)
	}
	if issue.IssueTypeIconURL == "" {
		t.Fatal("IssueTypeIconURL not captured")
	}
	if !strings.Contains(issue.IssueTypeIconURL, "universal_avatar") {
		t.Errorf("unexpected icon url: %q", issue.IssueTypeIconURL)
	}
}

// TestPriorityIconCaptured pins that we keep the priority's iconUrl and cache
// it in state. It cannot be derived from the name: custom priorities point at
// universal_avatar URLs rather than /images/icons/priorities/<name>.svg.
func TestPriorityIconCaptured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"key": "PROJ-1",
			"fields": {
				"summary": "A bug",
				"status": {"name": "In Progress"},
				"priority": {
					"name": "P1 - Critical",
					"iconUrl": "https://example.atlassian.net/rest/api/2/universal_avatar/view/type/priority/avatar/10557?size=medium"
				},
				"issuetype": {"id": "10004", "name": "Bug"},
				"labels": [],
				"created": "2026-01-01T00:00:00.000+0000",
				"updated": "2026-01-02T00:00:00.000+0000"
			}
		}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Email: "user@example.com", Token: "token"}
	issue, err := c.FetchIssue("PROJ-1", nil)
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}
	want := "https://example.atlassian.net/rest/api/2/universal_avatar/view/type/priority/avatar/10557?size=medium"
	if issue.PriorityIconURL != want {
		t.Errorf("PriorityIconURL = %q, want %q", issue.PriorityIconURL, want)
	}
	var state map[string]interface{}
	if err := json.Unmarshal([]byte(buildJiraStateJSON(issue)), &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state["priority_icon_url"] != want {
		t.Errorf("state[\"priority_icon_url\"] = %v, want %q", state["priority_icon_url"], want)
	}
}
