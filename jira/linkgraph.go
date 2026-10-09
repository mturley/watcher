package jira

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// MaxAncestorHops bounds the parent walk. Real hierarchies are at most
// Outcome → Feature → Epic → Story → Sub-task; the cap only matters for
// misconfigured or cyclic data.
const MaxAncestorHops = 6

// IssueSummary is the minimum needed to render an issue as a row.
type IssueSummary struct {
	Key              string
	Summary          string
	Status           string
	StatusCategory   string // "new" | "indeterminate" | "done" | ""
	IssueType        string
	IssueTypeIconURL string
	// HierarchyLevel is Jira's issuetype.hierarchyLevel: -1 sub-task,
	// 0 story/task/bug, 1 epic, 2 feature, 3 initiative/outcome.
	HierarchyLevel int
}

// IssueLink is one "Linked work items" entry. Label is Jira's own phrase for
// the relationship from THIS issue's side ("blocks", "is cloned by").
type IssueLink struct {
	Label string
	Issue IssueSummary
}

// LinkGraph is everything one issue says about what it links to.
type LinkGraph struct {
	Issue     IssueSummary
	Ancestors []IssueSummary // nearest parent first
	Subtasks  []IssueSummary
	Links     []IssueLink // in Jira's order
	// DescriptionURLs are the URLs in the description, in order, deduped.
	DescriptionURLs []string
	// GitPRURLs are the URLs in the Git Pull Request field; nil when no
	// field ID was given.
	GitPRURLs []string
}

const summaryFields = "summary,status,issuetype,parent"

type rawSummaryFields struct {
	Summary string `json:"summary"`
	Status  struct {
		Name           string `json:"name"`
		StatusCategory struct {
			Key string `json:"key"`
		} `json:"statusCategory"`
	} `json:"status"`
	IssueType struct {
		Name           string `json:"name"`
		IconURL        string `json:"iconUrl"`
		HierarchyLevel int    `json:"hierarchyLevel"`
	} `json:"issuetype"`
	Parent *rawIssueRef `json:"parent"`
}

type rawIssueRef struct {
	Key    string           `json:"key"`
	Fields rawSummaryFields `json:"fields"`
}

func (r rawIssueRef) summary() IssueSummary {
	return IssueSummary{
		Key:              r.Key,
		Summary:          r.Fields.Summary,
		Status:           r.Fields.Status.Name,
		StatusCategory:   r.Fields.Status.StatusCategory.Key,
		IssueType:        r.Fields.IssueType.Name,
		IssueTypeIconURL: r.Fields.IssueType.IconURL,
		HierarchyLevel:   r.Fields.IssueType.HierarchyLevel,
	}
}

func (c *Client) fetchSummaryRef(key string) (*rawIssueRef, error) {
	body, err := c.get(fmt.Sprintf("%s/rest/api/3/issue/%s?fields=%s",
		normalizeBaseURL(c.BaseURL), url.PathEscape(key), url.QueryEscape(summaryFields)))
	if err != nil {
		return nil, err
	}
	var ref rawIssueRef
	if err := json.Unmarshal(body, &ref); err != nil {
		return nil, fmt.Errorf("decode %s: %w", key, err)
	}
	return &ref, nil
}

// FetchLinkGraph fetches an issue's parent chain, sub-tasks, issue links,
// description URLs and (when gitPRFieldID is non-empty) Git Pull Request
// field URLs. Children other than sub-tasks are NOT included — use
// SearchSummaries("parent = KEY") for those.
//
// Only the first request is fatal. An ancestor that can't be read (deleted,
// no permission) ends the walk, keeping the parent ref the child embedded.
func (c *Client) FetchLinkGraph(key, gitPRFieldID string) (*LinkGraph, error) {
	fields := summaryFields + ",subtasks,issuelinks,description"
	if gitPRFieldID != "" {
		fields += "," + gitPRFieldID
	}
	body, err := c.get(fmt.Sprintf("%s/rest/api/3/issue/%s?fields=%s",
		normalizeBaseURL(c.BaseURL), url.PathEscape(key), url.QueryEscape(fields)))
	if err != nil {
		return nil, err
	}
	var raw struct {
		Key    string `json:"key"`
		Fields struct {
			rawSummaryFields
			Subtasks   []rawIssueRef `json:"subtasks"`
			IssueLinks []struct {
				Type struct {
					Inward  string `json:"inward"`
					Outward string `json:"outward"`
				} `json:"type"`
				InwardIssue  *rawIssueRef `json:"inwardIssue"`
				OutwardIssue *rawIssueRef `json:"outwardIssue"`
			} `json:"issuelinks"`
			Description interface{} `json:"description"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode %s: %w", key, err)
	}
	g := &LinkGraph{}
	g.Issue = rawIssueRef{Key: raw.Key, Fields: raw.Fields.rawSummaryFields}.summary()
	if g.Issue.Key == "" {
		g.Issue.Key = key
	}
	for _, s := range raw.Fields.Subtasks {
		g.Subtasks = append(g.Subtasks, s.summary())
	}
	for _, l := range raw.Fields.IssueLinks {
		switch {
		case l.OutwardIssue != nil:
			g.Links = append(g.Links, IssueLink{Label: l.Type.Outward, Issue: l.OutwardIssue.summary()})
		case l.InwardIssue != nil:
			g.Links = append(g.Links, IssueLink{Label: l.Type.Inward, Issue: l.InwardIssue.summary()})
		}
	}
	g.DescriptionURLs = ExtractADFURLs(raw.Fields.Description)
	if gitPRFieldID != "" {
		var all map[string]json.RawMessage
		var fieldsMap map[string]json.RawMessage
		if json.Unmarshal(body, &all) == nil && json.Unmarshal(all["fields"], &fieldsMap) == nil {
			var v interface{}
			if rv, ok := fieldsMap[gitPRFieldID]; ok && json.Unmarshal(rv, &v) == nil {
				g.GitPRURLs = ExtractADFURLs(v)
			}
		}
		if g.GitPRURLs == nil {
			g.GitPRURLs = []string{}
		}
	}

	seen := map[string]bool{g.Issue.Key: true}
	next := raw.Fields.Parent
	for hops := 0; next != nil && next.Key != "" && !seen[next.Key] && hops < MaxAncestorHops; hops++ {
		seen[next.Key] = true
		ref, err := c.fetchSummaryRef(next.Key)
		if err != nil {
			// Unreadable ancestor: keep what the child told us and stop.
			g.Ancestors = append(g.Ancestors, next.summary())
			break
		}
		g.Ancestors = append(g.Ancestors, ref.summary())
		next = ref.Fields.Parent
	}
	return g, nil
}

// SearchSummaries runs one JQL search and returns up to max issues, plus
// whether more matched. The /search/jql endpoint reports no total, so one
// extra result is requested to answer hasMore.
func (c *Client) SearchSummaries(jql string, max int) ([]IssueSummary, bool, error) {
	q := url.Values{}
	q.Set("jql", jql)
	q.Set("fields", summaryFields)
	q.Set("maxResults", strconv.Itoa(max+1))
	body, err := c.get(normalizeBaseURL(c.BaseURL) + "/rest/api/3/search/jql?" + q.Encode())
	if err != nil {
		return nil, false, err
	}
	var raw struct {
		Issues []rawIssueRef `json:"issues"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false, fmt.Errorf("decode search: %w", err)
	}
	more := len(raw.Issues) > max
	if more {
		raw.Issues = raw.Issues[:max]
	}
	out := make([]IssueSummary, 0, len(raw.Issues))
	for _, r := range raw.Issues {
		out = append(out, r.summary())
	}
	return out, more, nil
}
