package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseGraphQLResponse_Author verifies that the PR-level author is
// parsed out of the GraphQL response into PRData.Author/AuthorType, so it
// can be cached in the poller's state JSON.
func TestParseGraphQLResponse_Author(t *testing.T) {
	raw := `{
		"pr0": {
			"pullRequest": {
				"number": 123,
				"state": "OPEN",
				"title": "Test PR",
				"updatedAt": "2024-01-01T00:00:00Z",
				"author": {
					"__typename": "User",
					"login": "alice"
				},
				"reviews": {"nodes": []},
				"comments": {"nodes": []},
				"reviewThreads": {"nodes": []},
				"commits": {"totalCount": 0, "nodes": []}
			}
		},
		"rateLimit": {"remaining": 5000, "limit": 5000}
	}`

	prs := []PRRef{{Owner: "owner", Repo: "repo", Number: 123}}

	result, _, err := parseGraphQLResponse(json.RawMessage(raw), prs)
	if err != nil {
		t.Fatalf("parseGraphQLResponse: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 PR, got %d", len(result))
	}

	if got, want := result[0].Author, "alice"; got != want {
		t.Errorf("Author = %q, want %q", got, want)
	}
	if got, want := result[0].AuthorType, "user"; got != want {
		t.Errorf("AuthorType = %q, want %q", got, want)
	}
}

// TestFetchPRsPartialFailure pins GraphQL's partial-success semantics: when
// one repo in a batch cannot be resolved, GitHub returns that alias as null
// AND populates data for every other alias. Discarding the whole response
// (the old behaviour) stripped enrichment from every PR in the batch because
// a single bogus subscription existed.
func TestFetchPRsPartialFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"data": {
				"rateLimit": {"remaining": 4999, "limit": 5000},
				"pr0": null,
				"pr1": {"pullRequest": {"number": 9097, "title": "Real PR", "state": "OPEN", "url": "https://github.com/o/r/pull/9097", "author": {"login": "octocat"}}}
			},
			"errors": [{"message": "Could not resolve to a Repository with the name 'mturley/myrepo'."}]
		}`))
	}))
	defer srv.Close()

	prs := []PRRef{
		{Owner: "mturley", Repo: "myrepo", Number: 42},
		{Owner: "o", Repo: "r", Number: 9097},
	}
	got, rl, err := FetchPRs("tok", prs, srv.URL)
	if err != nil {
		t.Fatalf("a partial failure must not fail the batch: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want the 1 resolvable PR, got %d: %+v", len(got), got)
	}
	if got[0].Number != 9097 {
		t.Errorf("wrong PR survived: %+v", got[0])
	}
	if rl == nil || rl.Remaining != 4999 {
		t.Errorf("rate limit lost: %+v", rl)
	}
}

// TestFetchPRsTotalFailure ensures a response where nothing could be parsed
// still surfaces an error, rather than silently reporting success with no PRs.
func TestFetchPRsTotalFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"data": {"rateLimit": {"remaining": 4999, "limit": 5000}, "pr0": null},
			"errors": [{"message": "Could not resolve to a Repository with the name 'mturley/myrepo'."}]
		}`))
	}))
	defer srv.Close()

	_, _, err := FetchPRs("tok", []PRRef{{Owner: "mturley", Repo: "myrepo", Number: 42}}, srv.URL)
	if err == nil {
		t.Fatal("expected an error when no PR in the batch could be resolved")
	}
	if !strings.Contains(err.Error(), "myrepo") {
		t.Errorf("error should name the failing repo, got: %v", err)
	}
}

func TestParseGraphQLResponse_AuthorIDs(t *testing.T) {
	raw := `{
		"pr0": {"pullRequest": {
			"number": 1, "state": "OPEN", "title": "T", "updatedAt": "2024-01-01T00:00:00Z",
			"author": {"__typename": "User", "login": "alice", "databaseId": 101},
			"reviews": {"nodes": [{"author": {"__typename": "Bot", "login": "rabbit", "databaseId": 202}, "state": "COMMENTED", "submittedAt": "2024-01-01T00:00:00Z", "body": "b"}]},
			"comments": {"nodes": [{"author": {"__typename": "Mannequin", "login": "ghost"}, "createdAt": "2024-01-01T00:00:00Z", "body": "c"}]},
			"reviewThreads": {"nodes": [{"comments": {"nodes": [{"author": {"__typename": "User", "login": "bob", "databaseId": 303}, "createdAt": "2024-01-01T00:00:00Z", "path": "x.go", "body": "rc"}]}}]},
			"commits": {"totalCount": 0, "nodes": []}
		}},
		"rateLimit": {"remaining": 5000, "limit": 5000}
	}`
	res, _, err := parseGraphQLResponse(json.RawMessage(raw), []PRRef{{Owner: "o", Repo: "r", Number: 1}})
	if err != nil {
		t.Fatal(err)
	}
	pr := res[0]
	if pr.AuthorID != "101" || pr.Reviews[0].AuthorID != "202" || pr.ReviewComments[0].AuthorID != "303" {
		t.Fatalf("ids = %q %q %q", pr.AuthorID, pr.Reviews[0].AuthorID, pr.ReviewComments[0].AuthorID)
	}
	if pr.Comments[0].AuthorID != "" {
		t.Fatalf("an actor without databaseId must yield \"\", got %q", pr.Comments[0].AuthorID)
	}
}

func TestBuildBatchedPRQueryRequestsDatabaseIDs(t *testing.T) {
	q := buildBatchedPRQuery([]PRRef{{Owner: "o", Repo: "r", Number: 1}})
	if n := strings.Count(q, "... on User { databaseId }"); n < 4 {
		t.Fatalf("expected databaseId fragments at every author selection, found %d", n)
	}
}

func TestParseGraphQLResponse_MergedByAndCommitAuthors(t *testing.T) {
	raw := `{
		"pr0": {"pullRequest": {
			"number": 1, "state": "MERGED", "title": "T", "updatedAt": "2024-01-01T00:00:00Z",
			"author": {"__typename": "User", "login": "alice", "databaseId": 101},
			"mergedBy": {"__typename": "User", "login": "alice", "databaseId": 101},
			"reviews": {"nodes": []}, "comments": {"nodes": []}, "reviewThreads": {"nodes": []},
			"commits": {"totalCount": 2, "nodes": [
				{"commit": {"oid": "aaa1111", "committedDate": "2024-01-01T00:00:00Z", "messageHeadline": "one", "author": {"user": {"login": "alice", "databaseId": 101}}}},
				{"commit": {"oid": "bbb2222", "committedDate": "2024-01-02T00:00:00Z", "messageHeadline": "two", "author": {"user": null}}}
			]}
		}},
		"rateLimit": {"remaining": 5000, "limit": 5000}
	}`
	res, _, err := parseGraphQLResponse(json.RawMessage(raw), []PRRef{{Owner: "o", Repo: "r", Number: 1}})
	if err != nil {
		t.Fatal(err)
	}
	pr := res[0]
	if pr.MergedBy != "alice" || pr.MergedByID != "101" || pr.MergedByType != "user" {
		t.Fatalf("mergedBy = %q %q %q", pr.MergedBy, pr.MergedByID, pr.MergedByType)
	}
	if pr.Commits.Recent[0].AuthorID != "101" || pr.Commits.Recent[0].AuthorLogin != "alice" {
		t.Fatalf("commit 0 author = %+v", pr.Commits.Recent[0])
	}
	if pr.Commits.Recent[1].AuthorID != "" {
		t.Fatalf("an unlinked commit must have no AuthorID, got %q", pr.Commits.Recent[1].AuthorID)
	}
}
