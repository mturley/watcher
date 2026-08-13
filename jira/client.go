package jira

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a REST client for Jira API v3.
type Client struct {
	BaseURL string
	Email   string
	Token   string
}

// IssueData represents Jira issue data with changelog and comments.
type IssueData struct {
	Key          string
	Summary      string
	Status       string
	Priority     string
	IssueType    string
	Assignee     *string
	Reporter     *string
	Labels       []string
	CreatedAt    string
	UpdatedAt    string
	Comments     []IssueComment
	Changelog    []ChangelogEntry
	CustomFields map[string]interface{}
}

// IssueComment represents a Jira issue comment.
type IssueComment struct {
	Author    string
	CreatedAt string
	Body      string // Summary text, not full ADF
}

// ChangelogEntry represents a single Jira changelog item.
type ChangelogEntry struct {
	Author    string
	CreatedAt string
	Field     string
	From      string
	To        string
}

// changelogPageSize is the number of changelog entries to request per page
// from the dedicated /changelog endpoint. 100 is Jira's default maximum.
const changelogPageSize = 100

// commentPageSize is the number of comments to request per page from the
// dedicated /comment endpoint.
const commentPageSize = 100

// normalizeBaseURL ensures a Jira base URL has an explicit scheme. Config
// allows a bare host (e.g. "redhat.atlassian.net", per the README example),
// but request URLs are built via fmt.Sprintf("%s/rest/api/3/...", BaseURL),
// which produces a malformed request without a scheme. If the value already
// contains "://" it is left unchanged.
func normalizeBaseURL(host string) string {
	if strings.Contains(host, "://") {
		return host
	}
	return "https://" + host
}

// FetchIssue fetches issue data from Jira API v3.
//
// The issue's fields (summary/status/priority/assignee/labels/customfields)
// are fetched via the normal issue GET. The changelog and comments are each
// fetched via their dedicated PAGINATED endpoints, because the issue GET's
// expand=changelog silently caps at the 100 most recent entries, dropping the
// oldest history on active issues.
func (c *Client) FetchIssue(issueKey string, customFieldIDs map[string]string) (*IssueData, error) {
	fields := "summary,status,assignee,reporter,labels,priority,issuetype,created,updated"
	for _, fieldID := range customFieldIDs {
		fields += "," + fieldID
	}
	// Fetch fields only. Changelog and comments are paginated separately
	// below so we never lose history to the 100-entry expand cap.
	fieldsURL := fmt.Sprintf("%s/rest/api/3/issue/%s?fields=%s", c.BaseURL, issueKey, url.QueryEscape(fields))

	bodyBytes, err := c.get(fieldsURL)
	if err != nil {
		return nil, err
	}

	var raw struct {
		Key    string `json:"key"`
		Fields struct {
			Summary string `json:"summary"`
			Status  struct {
				Name string `json:"name"`
			} `json:"status"`
			Priority struct {
				Name string `json:"name"`
			} `json:"priority"`
			IssueType struct {
				Name string `json:"name"`
			} `json:"issuetype"`
			Assignee *struct {
				DisplayName string `json:"displayName"`
			} `json:"assignee"`
			Reporter *struct {
				DisplayName string `json:"displayName"`
			} `json:"reporter"`
			Labels  []string `json:"labels"`
			Created string   `json:"created"`
			Updated string   `json:"updated"`
		} `json:"fields"`
	}

	// Decode typed fields
	if err := json.Unmarshal(bodyBytes, &raw); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// Decode raw for custom fields
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &rawMap); err != nil {
		return nil, fmt.Errorf("failed to decode raw response: %w", err)
	}
	var fieldsMap map[string]json.RawMessage
	if rawFields, ok := rawMap["fields"]; ok {
		json.Unmarshal(rawFields, &fieldsMap)
	}

	// Build IssueData
	issue := &IssueData{
		Key:          raw.Key,
		Summary:      raw.Fields.Summary,
		Status:       raw.Fields.Status.Name,
		Priority:     raw.Fields.Priority.Name,
		IssueType:    raw.Fields.IssueType.Name,
		Labels:       raw.Fields.Labels,
		CreatedAt:    raw.Fields.Created,
		UpdatedAt:    raw.Fields.Updated,
		CustomFields: make(map[string]interface{}),
	}
	if raw.Key == "" {
		issue.Key = issueKey
	}

	if raw.Fields.Assignee != nil {
		issue.Assignee = &raw.Fields.Assignee.DisplayName
	}
	if raw.Fields.Reporter != nil {
		issue.Reporter = &raw.Fields.Reporter.DisplayName
	}

	// Extract custom fields
	for displayName, fieldID := range customFieldIDs {
		if rawVal, ok := fieldsMap[fieldID]; ok {
			issue.CustomFields[displayName] = extractFieldValue(rawVal)
		}
	}

	// Fetch all comments (paginated).
	comments, err := c.fetchComments(issueKey)
	if err != nil {
		return nil, err
	}
	issue.Comments = comments

	// Fetch the full, paginated changelog.
	changelog, err := c.fetchChangelog(issueKey)
	if err != nil {
		return nil, err
	}
	issue.Changelog = changelog

	return issue, nil
}

// get performs an authenticated GET and returns the response body, or an
// error if the request fails or returns a non-200 status.
func (c *Client) get(reqURL string) ([]byte, error) {
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.SetBasicAuth(c.Email, c.Token)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	return body, nil
}

// fetchChangelog fetches the complete changelog for an issue via the
// dedicated paginated endpoint GET /rest/api/3/issue/{key}/changelog,
// looping on startAt until all entries have been retrieved. This replaces
// expand=changelog, which silently caps at the 100 most recent entries.
func (c *Client) fetchChangelog(issueKey string) ([]ChangelogEntry, error) {
	var entries []ChangelogEntry
	startAt := 0
	for {
		pageURL := fmt.Sprintf("%s/rest/api/3/issue/%s/changelog?startAt=%d&maxResults=%d",
			c.BaseURL, issueKey, startAt, changelogPageSize)
		body, err := c.get(pageURL)
		if err != nil {
			return nil, err
		}

		var page struct {
			StartAt    int  `json:"startAt"`
			MaxResults int  `json:"maxResults"`
			Total      int  `json:"total"`
			IsLast     bool `json:"isLast"`
			Values     []struct {
				Author struct {
					DisplayName string `json:"displayName"`
				} `json:"author"`
				Created string `json:"created"`
				Items   []struct {
					Field      string `json:"field"`
					FromString string `json:"fromString"`
					ToString   string `json:"toString"`
				} `json:"items"`
			} `json:"values"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("failed to decode changelog page: %w", err)
		}

		for _, history := range page.Values {
			for _, item := range history.Items {
				entries = append(entries, ChangelogEntry{
					Author:    history.Author.DisplayName,
					CreatedAt: history.Created,
					Field:     item.Field,
					From:      item.FromString,
					To:        item.ToString,
				})
			}
		}

		// Stop when the server says this is the last page, when we've
		// consumed >= total, or when a page comes back empty (defensive
		// guard against servers that omit isLast/total).
		next := page.StartAt + len(page.Values)
		if page.IsLast || len(page.Values) == 0 || (page.Total > 0 && next >= page.Total) {
			break
		}
		startAt = next
	}
	return entries, nil
}

// fetchComments fetches all comments for an issue via the dedicated
// paginated endpoint GET /rest/api/3/issue/{key}/comment, looping on
// startAt until all comments have been retrieved.
func (c *Client) fetchComments(issueKey string) ([]IssueComment, error) {
	var comments []IssueComment
	startAt := 0
	for {
		pageURL := fmt.Sprintf("%s/rest/api/3/issue/%s/comment?startAt=%d&maxResults=%d",
			c.BaseURL, issueKey, startAt, commentPageSize)
		body, err := c.get(pageURL)
		if err != nil {
			return nil, err
		}

		var page struct {
			StartAt    int `json:"startAt"`
			MaxResults int `json:"maxResults"`
			Total      int `json:"total"`
			Comments   []struct {
				Author struct {
					DisplayName string `json:"displayName"`
				} `json:"author"`
				Created string      `json:"created"`
				Body    interface{} `json:"body"` // ADF JSON
			} `json:"comments"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("failed to decode comments page: %w", err)
		}

		for _, cm := range page.Comments {
			body := extractADFText(cm.Body)
			if body == "" {
				body = fmt.Sprintf("Comment by %s", cm.Author.DisplayName)
			}
			comments = append(comments, IssueComment{
				Author:    cm.Author.DisplayName,
				CreatedAt: cm.Created,
				Body:      body,
			})
		}

		next := page.StartAt + len(page.Comments)
		if len(page.Comments) == 0 || (page.Total > 0 && next >= page.Total) {
			break
		}
		startAt = next
	}
	return comments, nil
}

// extractFieldValue extracts a display value from a Jira field's raw JSON.
// Objects with .value or .name use that string. Strings, numbers, nulls are direct.
func extractFieldValue(raw json.RawMessage) interface{} {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}

	var num float64
	if json.Unmarshal(raw, &num) == nil {
		return num
	}

	var obj map[string]interface{}
	if json.Unmarshal(raw, &obj) == nil {
		if v, ok := obj["value"]; ok {
			return v
		}
		if v, ok := obj["name"]; ok {
			return v
		}
		return obj
	}

	var arr []interface{}
	if json.Unmarshal(raw, &arr) == nil {
		return arr
	}

	return nil
}

// extractADFText extracts plain text from an ADF (Atlassian Document Format) body.
// ADF is a nested JSON structure; we recursively extract text nodes.
func extractADFText(body interface{}) string {
	if body == nil {
		return ""
	}
	doc, ok := body.(map[string]interface{})
	if !ok {
		return ""
	}
	var texts []string
	extractADFTextNodes(doc, &texts)
	result := strings.Join(texts, "")
	// Truncate long comments
	if len(result) > 500 {
		result = result[:497] + "..."
	}
	return strings.TrimSpace(result)
}

func extractADFTextNodes(node map[string]interface{}, texts *[]string) {
	if nodeType, ok := node["type"].(string); ok && nodeType == "text" {
		if text, ok := node["text"].(string); ok {
			*texts = append(*texts, text)
		}
		return
	}
	// Add newlines between block-level elements
	if nodeType, ok := node["type"].(string); ok {
		switch nodeType {
		case "paragraph", "heading", "bulletList", "orderedList", "listItem", "blockquote":
			if len(*texts) > 0 {
				*texts = append(*texts, "\n")
			}
		}
	}
	if content, ok := node["content"].([]interface{}); ok {
		for _, child := range content {
			if childMap, ok := child.(map[string]interface{}); ok {
				extractADFTextNodes(childMap, texts)
			}
		}
	}
}
