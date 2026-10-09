package jira

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// issueJSON renders a minimal issue for a fake Jira. parent may be "".
func issueJSON(key, typ string, level int, parent string) string {
	p := "null"
	if parent != "" {
		p = fmt.Sprintf(`{"key":%q,"fields":{"summary":"S %s"}}`, parent, parent)
	}
	return fmt.Sprintf(`{"key":%q,"fields":{"summary":"S %s",
		"status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}},
		"issuetype":{"name":%q,"iconUrl":"https://example.atlassian.net/icon/%s","hierarchyLevel":%d},
		"parent":%s}}`, key, key, typ, typ, level, p)
}

func TestFetchLinkGraph(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/rest/api/3/issue/PROJ-1":
			if !strings.Contains(r.URL.Query().Get("fields"), "customfield_99") {
				t.Errorf("git PR field not requested: %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"key":"PROJ-1","fields":{"summary":"Story one",
				"status":{"name":"Review","statusCategory":{"key":"indeterminate"}},
				"issuetype":{"name":"Story","iconUrl":"https://example.atlassian.net/icon/story","hierarchyLevel":0},
				"parent":{"key":"PROJ-10","fields":{"summary":"Epic"}},
				"subtasks":[{"key":"PROJ-2","fields":{"summary":"Sub","status":{"name":"New","statusCategory":{"key":"new"}},"issuetype":{"name":"Sub-task","hierarchyLevel":-1}}}],
				"issuelinks":[
					{"type":{"inward":"is blocked by","outward":"blocks"},"outwardIssue":{"key":"PROJ-3","fields":{"summary":"Blocked one","status":{"name":"New"},"issuetype":{"name":"Bug"}}}},
					{"type":{"inward":"is cloned by","outward":"clones"},"inwardIssue":{"key":"PROJ-4","fields":{"summary":"Clone","status":{"name":"Done"},"issuetype":{"name":"Story"}}}}
				],
				"description":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"inlineCard","attrs":{"url":"https://example.atlassian.net/browse/PROJ-5"}}]}]},
				"customfield_99":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"inlineCard","attrs":{"url":"https://github.com/example/repo/pull/9"}}]}]}
			}}`)
		case "/rest/api/3/issue/PROJ-10":
			fmt.Fprint(w, issueJSON("PROJ-10", "Epic", 1, "OTHER-20"))
		case "/rest/api/3/issue/OTHER-20":
			fmt.Fprint(w, issueJSON("OTHER-20", "Feature", 2, ""))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Email: "u@example.com", Token: "t"}
	g, err := c.FetchLinkGraph("PROJ-1", "customfield_99")
	if err != nil {
		t.Fatal(err)
	}
	if g.Issue.Key != "PROJ-1" || g.Issue.IssueType != "Story" || g.Issue.StatusCategory != "indeterminate" {
		t.Errorf("issue = %+v", g.Issue)
	}
	if len(g.Ancestors) != 2 || g.Ancestors[0].Key != "PROJ-10" || g.Ancestors[1].Key != "OTHER-20" || g.Ancestors[1].HierarchyLevel != 2 {
		t.Errorf("ancestors = %+v", g.Ancestors)
	}
	if len(g.Subtasks) != 1 || g.Subtasks[0].Key != "PROJ-2" || g.Subtasks[0].HierarchyLevel != -1 {
		t.Errorf("subtasks = %+v", g.Subtasks)
	}
	if len(g.Links) != 2 || g.Links[0].Label != "blocks" || g.Links[0].Issue.Key != "PROJ-3" ||
		g.Links[1].Label != "is cloned by" || g.Links[1].Issue.Key != "PROJ-4" {
		t.Errorf("links = %+v", g.Links)
	}
	if len(g.DescriptionURLs) != 1 || len(g.GitPRURLs) != 1 || g.GitPRURLs[0] != "https://github.com/example/repo/pull/9" {
		t.Errorf("urls: desc=%v gitpr=%v", g.DescriptionURLs, g.GitPRURLs)
	}
	if len(g.DescriptionLinks) != len(g.DescriptionURLs) || len(g.GitPRLinks) != 1 ||
		g.GitPRLinks[0].URL != g.GitPRURLs[0] || g.GitPRLinks[0].Label == "" {
		t.Errorf("links: desc=%+v gitpr=%+v", g.DescriptionLinks, g.GitPRLinks)
	}
}

func TestFetchLinkGraphNoGitPRField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, issueJSON("PROJ-1", "Story", 0, ""))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	g, err := c.FetchLinkGraph("PROJ-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if g.GitPRURLs != nil {
		t.Errorf("GitPRURLs must be nil when the field is unconfigured, got %v", g.GitPRURLs)
	}
	if len(g.Ancestors) != 0 {
		t.Errorf("ancestors = %v", g.Ancestors)
	}
}

func TestFetchLinkGraphStopsOnCycleAndHopCap(t *testing.T) {
	// A → B → A ... must terminate.
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		key := strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/")
		next := map[string]string{"PROJ-1": "PROJ-2", "PROJ-2": "PROJ-1"}[key]
		fmt.Fprint(w, issueJSON(key, "Story", 0, next))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	g, err := c.FetchLinkGraph("PROJ-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Ancestors) != 1 || g.Ancestors[0].Key != "PROJ-2" {
		t.Errorf("cycle: ancestors = %+v", g.Ancestors)
	}
	if hits > 3 {
		t.Errorf("cycle walk made %d requests", hits)
	}

	// An endless chain stops at MaxAncestorHops.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/")
		var n int
		fmt.Sscanf(key, "PROJ-%d", &n)
		fmt.Fprint(w, issueJSON(key, "Story", 0, fmt.Sprintf("PROJ-%d", n+1)))
	}))
	defer srv2.Close()
	g2, err := (&Client{BaseURL: srv2.URL}).FetchLinkGraph("PROJ-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(g2.Ancestors) != MaxAncestorHops {
		t.Errorf("hop cap: got %d ancestors", len(g2.Ancestors))
	}
}

func TestFetchLinkGraphAncestorErrorKeepsPartialChain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "PROJ-1") {
			fmt.Fprint(w, issueJSON("PROJ-1", "Story", 0, "SECRET-1"))
			return
		}
		http.Error(w, `{"errorMessages":["no permission"]}`, http.StatusNotFound)
	}))
	defer srv.Close()
	g, err := (&Client{BaseURL: srv.URL}).FetchLinkGraph("PROJ-1", "")
	if err != nil {
		t.Fatalf("an unreadable parent must not fail the graph: %v", err)
	}
	// The parent ref embedded in the issue still names it.
	if len(g.Ancestors) != 1 || g.Ancestors[0].Key != "SECRET-1" {
		t.Errorf("ancestors = %+v", g.Ancestors)
	}
}

func TestSearchSummaries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/search/jql" {
			t.Errorf("path %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("jql") != "parent = PROJ-10" || q.Get("maxResults") != "3" {
			t.Errorf("query %v", q)
		}
		fmt.Fprintf(w, `{"issues":[%s,%s,%s],"isLast":true}`,
			issueJSON("PROJ-1", "Story", 0, ""), issueJSON("PROJ-2", "Story", 0, ""), issueJSON("PROJ-3", "Story", 0, ""))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	got, more, err := c.SearchSummaries("parent = PROJ-10", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !more || got[0].Key != "PROJ-1" {
		t.Errorf("got %d issues more=%v: %+v", len(got), more, got)
	}
}
