package jira

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	if err := json.Unmarshal([]byte(buildJiraStateJSON(issue, nil)), &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state["priority_icon_url"] != want {
		t.Errorf("state[\"priority_icon_url\"] = %v, want %q", state["priority_icon_url"], want)
	}
}

// TestStatusCategoryCaptured pins that FetchIssue requests the status field
// (which carries statusCategory nested inside it), keeps its key, and caches
// it in state as status_category.
func TestStatusCategoryCaptured(t *testing.T) {
	var gotFields string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/comment") || strings.HasSuffix(r.URL.Path, "/changelog") {
			w.Write([]byte(`{"comments": [], "values": [], "total": 0, "isLast": true}`))
			return
		}
		gotFields = r.URL.Query().Get("fields")
		w.Write([]byte(`{
			"key": "PROJ-1",
			"fields": {
				"summary": "A bug",
				"status": {"name": "Review", "statusCategory": {"id": 4, "key": "indeterminate", "name": "In Progress"}},
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
	if !strings.Contains(","+gotFields+",", ",status,") {
		t.Errorf("fields = %q, want it to request status", gotFields)
	}
	if issue.StatusCategory != "indeterminate" {
		t.Errorf("StatusCategory = %q, want indeterminate", issue.StatusCategory)
	}
	var state map[string]interface{}
	if err := json.Unmarshal([]byte(buildJiraStateJSON(issue, nil)), &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state["status_category"] != "indeterminate" {
		t.Errorf("state[\"status_category\"] = %v, want indeterminate", state["status_category"])
	}
}

func priorityServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Email: "user@example.com", Token: "token"}
}

func TestFetchPriorityOrderEditmeta(t *testing.T) {
	c := priorityServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/issue/RHOAIENG-1/editmeta" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Write([]byte(`{"fields":{"priority":{"allowedValues":[{"name":"Blocker"},{"name":"Critical"},{"name":"Major"},{"name":"Normal"},{"name":"Minor"},{"name":"Undefined"}]}}}`))
	})
	got, ok, err := c.FetchPriorityOrder("RHOAIENG-1")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	want := map[string]int{"Blocker": 0, "Critical": 1, "Major": 2, "Normal": 3, "Minor": 4, "Undefined": 5}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFetchPriorityOrderNoPermission(t *testing.T) {
	c := priorityServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"fields":{}}`))
	})
	got, ok, err := c.FetchPriorityOrder("RHOAIENG-1")
	if got != nil || ok || err != nil {
		t.Errorf("got %v, %v, %v; want nil, false, nil", got, ok, err)
	}
}

func TestFetchGlobalPriorities(t *testing.T) {
	c := priorityServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/priority" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Write([]byte(`[{"name":"Highest (migrated)"},{"name":"Blocker"}]`))
	})
	got, err := c.FetchGlobalPriorities()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"Highest (migrated)": 0, "Blocker": 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
