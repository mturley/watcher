package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PRSummary is the minimum needed to render a PR as a row, plus its body
// (which callers scan for links) and its head branch (which callers check
// out, e.g. worktree creating a worktree from a PR).
type PRSummary struct {
	Title       string
	State       string // OPEN | CLOSED | MERGED
	IsDraft     bool
	URL         string
	Body        string
	HeadRefName string
	// HeadRepoOwner and HeadRepoName are the repository the head branch
	// lives in: the base repo for a same-repo PR, the fork otherwise. Both
	// are empty when GitHub no longer has it (a deleted fork).
	HeadRepoOwner string
	HeadRepoName  string
}

// FetchPRSummaries fetches title/state/draft/url/body/head branch and repo for many
// PRs in ONE GraphQL request (one alias per PR). PRs that can't be resolved
// (deleted, private, typo'd) are simply absent from the result; only a
// response with nothing usable at all is an error. A rejected token (401)
// wraps ErrAuth.
func FetchPRSummaries(token string, refs []PRRef, apiURL ...string) (map[PRRef]PRSummary, error) {
	out := map[PRRef]PRSummary{}
	if len(refs) == 0 {
		return out, nil
	}
	endpoint := "https://api.github.com/graphql"
	if len(apiURL) > 0 && apiURL[0] != "" {
		endpoint = apiURL[0]
	}
	var q strings.Builder
	q.WriteString("query {\n")
	for i, r := range refs {
		fmt.Fprintf(&q, "  pr%d: repository(owner: %s, name: %s) { pullRequest(number: %d) { title state isDraft url body headRefName headRepository { name owner { login } } } }\n",
			i, strconv.Quote(r.Owner), strconv.Quote(r.Repo), r.Number)
	}
	q.WriteString("}")

	body, err := postGraphQL(token, endpoint, q.String())
	if err != nil {
		return nil, err
	}
	var result struct {
		Data map[string]*struct {
			PullRequest *struct {
				PRSummary
				HeadRepository *struct {
					Name  string
					Owner struct{ Login string }
				} `json:"headRepository"`
			} `json:"pullRequest"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode graphql: %w", err)
	}
	for i, r := range refs {
		if a := result.Data[fmt.Sprintf("pr%d", i)]; a != nil && a.PullRequest != nil {
			s := a.PullRequest.PRSummary
			if h := a.PullRequest.HeadRepository; h != nil {
				s.HeadRepoOwner, s.HeadRepoName = h.Owner.Login, h.Name
			}
			out[r] = s
		}
	}
	if len(out) == 0 && len(result.Errors) > 0 {
		return nil, fmt.Errorf("github graphql: %s", result.Errors[0].Message)
	}
	return out, nil
}

// postGraphQL POSTs one GraphQL query and returns the raw response body. A
// rejected token (401) wraps ErrAuth; any other non-200 is an error. An
// optional variables map is sent as "variables"; pass user-supplied strings
// that way, never by interpolating them into the query.
func postGraphQL(token, endpoint, query string, variables ...map[string]any) ([]byte, error) {
	reqBody := map[string]any{"query": query}
	if len(variables) > 0 && variables[0] != nil {
		reqBody["variables"] = variables[0]
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("encode graphql: %w", err)
	}
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("github graphql: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("GitHub token rejected: %w", ErrAuth)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned status %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}
