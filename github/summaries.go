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
// (which callers scan for links).
type PRSummary struct {
	Title   string
	State   string // OPEN | CLOSED | MERGED
	IsDraft bool
	URL     string
	Body    string
}

// FetchPRSummaries fetches title/state/draft/url/body for many PRs in ONE
// GraphQL request (one alias per PR). PRs that can't be resolved (deleted,
// private, typo'd) are simply absent from the result; only a response with
// nothing usable at all is an error.
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
		fmt.Fprintf(&q, "  pr%d: repository(owner: %s, name: %s) { pullRequest(number: %d) { title state isDraft url body } }\n",
			i, strconv.Quote(r.Owner), strconv.Quote(r.Repo), r.Number)
	}
	q.WriteString("}")

	payload, _ := json.Marshal(map[string]string{"query": q.String()})
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
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned status %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		Data map[string]*struct {
			PullRequest *PRSummary `json:"pullRequest"`
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
			out[r] = *a.PullRequest
		}
	}
	if len(out) == 0 && len(result.Errors) > 0 {
		return nil, fmt.Errorf("github graphql: %s", result.Errors[0].Message)
	}
	return out, nil
}
