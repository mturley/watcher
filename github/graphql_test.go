package github

import (
	"encoding/json"
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
