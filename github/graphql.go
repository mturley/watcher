package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// PRRef identifies a GitHub pull request.
type PRRef struct {
	Owner  string
	Repo   string
	Number int
}

// PRData contains pull request data from GitHub GraphQL API.
type PRData struct {
	Number         int
	Owner          string
	Repo           string
	State          string
	Title          string
	UpdatedAt      string
	Reviews        []Review
	Comments       []Comment
	ReviewComments []ReviewComment
	Commits        CommitInfo
	CheckRuns           []CheckRun
	CheckRunsTotalCount int
	CheckRunsHasMore    bool
	CheckRunsEndCursor  string
}

// Review represents a PR review.
type Review struct {
	Author      string
	AuthorType  string
	State       string
	SubmittedAt string
	Body        string
}

// Comment represents a PR issue comment.
type Comment struct {
	Author     string
	AuthorType string
	CreatedAt  string
	Body       string
}

// ReviewComment represents a PR review comment (inline comment on code).
type ReviewComment struct {
	Author     string
	AuthorType string
	CreatedAt  string
	Path       string
	Body       string
}

// CommitEntry represents a single commit.
type CommitEntry struct {
	SHA             string
	Date            string
	MessageHeadline string
}

// CommitInfo contains commit count, latest SHA, and recent commits.
type CommitInfo struct {
	TotalCount int
	LatestSHA  string
	LatestDate string
	Recent     []CommitEntry
}

// CheckRun represents a check run status.
type CheckRun struct {
	Name        string
	Status      string
	Conclusion  string
	CompletedAt string
}

// RateLimit contains GitHub API rate limit info.
type RateLimit struct {
	Remaining int
	Limit     int
}

// ParsePRResourceID parses a resource ID like "owner/repo#123" into a PRRef.
func ParsePRResourceID(resourceID string) (PRRef, error) {
	re := regexp.MustCompile(`^([^/]+)/([^#]+)#(\d+)$`)
	matches := re.FindStringSubmatch(resourceID)
	if matches == nil {
		return PRRef{}, fmt.Errorf("invalid PR resource ID format: %q (expected owner/repo#number)", resourceID)
	}

	number, err := strconv.Atoi(matches[3])
	if err != nil {
		return PRRef{}, fmt.Errorf("invalid PR number in resource ID %q: %w", resourceID, err)
	}

	return PRRef{
		Owner:  matches[1],
		Repo:   matches[2],
		Number: number,
	}, nil
}

// FetchPRs fetches PR data for multiple PRs in a single batched GraphQL query.
// Optional apiURL parameter allows overriding the GitHub API endpoint (for testing).
func FetchPRs(token string, prs []PRRef, apiURL ...string) ([]PRData, *RateLimit, error) {
	if len(prs) == 0 {
		return nil, nil, fmt.Errorf("no PRs to fetch")
	}

	endpoint := "https://api.github.com/graphql"
	if len(apiURL) > 0 && apiURL[0] != "" {
		endpoint = apiURL[0]
	}

	query := buildBatchedPRQuery(prs)

	reqBody := map[string]interface{}{
		"query": query,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("GitHub API returned status %d: %s", resp.StatusCode, string(body))
	}

	var result graphQLResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if len(result.Errors) > 0 {
		var errMsgs []string
		for _, e := range result.Errors {
			errMsgs = append(errMsgs, e.Message)
		}
		return nil, nil, fmt.Errorf("GraphQL errors: %s", strings.Join(errMsgs, "; "))
	}

	prDataList, rateLimit, err := parseGraphQLResponse(result.Data, prs)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse GraphQL response: %w", err)
	}

	return prDataList, rateLimit, nil
}

// buildBatchedPRQuery constructs a GraphQL query for multiple PRs.
func buildBatchedPRQuery(prs []PRRef) string {
	var aliases []string
	for i, pr := range prs {
		alias := fmt.Sprintf(`pr%d: repository(owner: "%s", name: "%s") {
      pullRequest(number: %d) {
        number
        state
        title
        updatedAt
        reviews(last: 20) {
          nodes {
            author {
              __typename
              login
            }
            state
            submittedAt
            body
          }
        }
        comments(last: 20) {
          nodes {
            author {
              __typename
              login
            }
            createdAt
            body
          }
        }
        reviewThreads(last: 20) {
          nodes {
            comments(last: 20) {
              nodes {
                author {
                  __typename
                  login
                }
                createdAt
                path
                body
              }
            }
          }
        }
        commits(last: 10) {
          totalCount
          nodes {
            commit {
              oid
              committedDate
              messageHeadline
              statusCheckRollup {
                contexts(first: 100) {
                  totalCount
                  pageInfo {
                    hasNextPage
                    endCursor
                  }
                  nodes {
                    __typename
                    ... on CheckRun {
                      name
                      status
                      conclusion
                      completedAt
                    }
                  }
                }
              }
            }
          }
        }
      }
    }`, i, pr.Owner, pr.Repo, pr.Number)
		aliases = append(aliases, alias)
	}

	return fmt.Sprintf(`query {
    %s
    rateLimit {
      remaining
      limit
    }
  }`, strings.Join(aliases, "\n    "))
}

// graphQLResponse represents the top-level GraphQL response.
type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphQLError  `json:"errors,omitempty"`
}

type graphQLError struct {
	Message string `json:"message"`
}

type graphQLData struct {
	RateLimit rateLimitData `json:"rateLimit"`
}

type rateLimitData struct {
	Remaining int `json:"remaining"`
	Limit     int `json:"limit"`
}

// parseGraphQLResponse parses the GraphQL response into PRData structs.
func parseGraphQLResponse(data json.RawMessage, prs []PRRef) ([]PRData, *RateLimit, error) {
	var rawData map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawData); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal raw data: %w", err)
	}

	// Parse rate limit
	var rateLimitData rateLimitData
	if rlRaw, ok := rawData["rateLimit"]; ok {
		if err := json.Unmarshal(rlRaw, &rateLimitData); err != nil {
			return nil, nil, fmt.Errorf("failed to unmarshal rate limit: %w", err)
		}
	}

	rateLimit := &RateLimit{
		Remaining: rateLimitData.Remaining,
		Limit:     rateLimitData.Limit,
	}

	// Parse PRs
	var result []PRData
	for i, pr := range prs {
		alias := fmt.Sprintf("pr%d", i)
		prRaw, ok := rawData[alias]
		if !ok {
			return nil, nil, fmt.Errorf("missing PR data for alias %s", alias)
		}

		var repoData struct {
			PullRequest *prNode `json:"pullRequest"`
		}
		if err := json.Unmarshal(prRaw, &repoData); err != nil {
			return nil, nil, fmt.Errorf("failed to unmarshal repo data for %s: %w", alias, err)
		}

		if repoData.PullRequest == nil {
			return nil, nil, fmt.Errorf("PR %s/%s#%d not found", pr.Owner, pr.Repo, pr.Number)
		}

		prData := parsePRNode(repoData.PullRequest, pr.Owner, pr.Repo)
		result = append(result, prData)
	}

	return result, rateLimit, nil
}

// prNode represents a PR node in the GraphQL response.
type prNode struct {
	Number        int                    `json:"number"`
	State         string                 `json:"state"`
	Title         string                 `json:"title"`
	UpdatedAt     string                 `json:"updatedAt"`
	Reviews       reviewsConnection      `json:"reviews"`
	Comments      commentsConnection     `json:"comments"`
	ReviewThreads reviewThreadsConnection `json:"reviewThreads"`
	Commits       commitsConnection      `json:"commits"`
}

type reviewsConnection struct {
	Nodes []reviewNode `json:"nodes"`
}

type reviewNode struct {
	Author      authorNode `json:"author"`
	State       string     `json:"state"`
	SubmittedAt string     `json:"submittedAt"`
	Body        string     `json:"body"`
}

type commentsConnection struct {
	Nodes []commentNode `json:"nodes"`
}

type commentNode struct {
	Author    authorNode `json:"author"`
	CreatedAt string     `json:"createdAt"`
	Body      string     `json:"body"`
}

type reviewThreadsConnection struct {
	Nodes []reviewThreadNode `json:"nodes"`
}

type reviewThreadNode struct {
	Comments reviewCommentsConnection `json:"comments"`
}

type reviewCommentsConnection struct {
	Nodes []reviewCommentNode `json:"nodes"`
}

type reviewCommentNode struct {
	Author    authorNode `json:"author"`
	CreatedAt string     `json:"createdAt"`
	Path      string     `json:"path"`
	Body      string     `json:"body"`
}

type commitsConnection struct {
	TotalCount int          `json:"totalCount"`
	Nodes      []commitNode `json:"nodes"`
}

type commitNode struct {
	Commit struct {
		OID                string              `json:"oid"`
		CommittedDate      string              `json:"committedDate"`
		MessageHeadline    string              `json:"messageHeadline"`
		StatusCheckRollup  *statusCheckRollup  `json:"statusCheckRollup"`
	} `json:"commit"`
}

type statusCheckRollup struct {
	Contexts statusCheckContexts `json:"contexts"`
}

type statusCheckContexts struct {
	TotalCount int                   `json:"totalCount"`
	PageInfo   struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes      []statusCheckContext  `json:"nodes"`
}

type statusCheckContext struct {
	TypeName    string  `json:"__typename"`
	Name        string  `json:"name"`
	Status      *string `json:"status"`
	Conclusion  *string `json:"conclusion"`
	CompletedAt *string `json:"completedAt"`
}

type authorNode struct {
	Typename string `json:"__typename"`
	Login    string `json:"login"`
}

// parsePRNode converts a prNode into a PRData struct.
func parsePRNode(node *prNode, owner, repo string) PRData {
	data := PRData{
		Number:    node.Number,
		Owner:     owner,
		Repo:      repo,
		State:     node.State,
		Title:     node.Title,
		UpdatedAt: node.UpdatedAt,
	}

	// Parse reviews
	for _, r := range node.Reviews.Nodes {
		data.Reviews = append(data.Reviews, Review{
			Author:      r.Author.Login,
			AuthorType:  authorType(r.Author.Typename),
			State:       r.State,
			SubmittedAt: r.SubmittedAt,
			Body:        r.Body,
		})
	}

	// Parse comments
	for _, c := range node.Comments.Nodes {
		data.Comments = append(data.Comments, Comment{
			Author:     c.Author.Login,
			AuthorType: authorType(c.Author.Typename),
			CreatedAt:  c.CreatedAt,
			Body:       c.Body,
		})
	}

	// Parse review comments (inline comments from review threads)
	for _, thread := range node.ReviewThreads.Nodes {
		for _, rc := range thread.Comments.Nodes {
			data.ReviewComments = append(data.ReviewComments, ReviewComment{
				Author:     rc.Author.Login,
				AuthorType: authorType(rc.Author.Typename),
				CreatedAt:  rc.CreatedAt,
				Path:       rc.Path,
				Body:       rc.Body,
			})
		}
	}

	// Parse commits (nodes are in chronological order; last element is most recent)
	data.Commits.TotalCount = node.Commits.TotalCount
	for _, cn := range node.Commits.Nodes {
		data.Commits.Recent = append(data.Commits.Recent, CommitEntry{
			SHA:             cn.Commit.OID,
			Date:            cn.Commit.CommittedDate,
			MessageHeadline: cn.Commit.MessageHeadline,
		})
	}
	if len(node.Commits.Nodes) > 0 {
		latest := node.Commits.Nodes[len(node.Commits.Nodes)-1]
		data.Commits.LatestSHA = latest.Commit.OID
		data.Commits.LatestDate = latest.Commit.CommittedDate
	}

	// Parse check runs from statusCheckRollup (includes workflow_run-triggered checks)
	if len(node.Commits.Nodes) > 0 {
		latestNode := node.Commits.Nodes[len(node.Commits.Nodes)-1]
		if latestNode.Commit.StatusCheckRollup != nil {
			rollup := latestNode.Commit.StatusCheckRollup.Contexts
			data.CheckRunsTotalCount = rollup.TotalCount
			data.CheckRunsHasMore = rollup.PageInfo.HasNextPage
			data.CheckRunsEndCursor = rollup.PageInfo.EndCursor
			for _, ctx := range rollup.Nodes {
				if ctx.TypeName != "CheckRun" {
					continue
				}
				status := ""
				if ctx.Status != nil {
					status = *ctx.Status
				}
				conclusion := ""
				if ctx.Conclusion != nil {
					conclusion = *ctx.Conclusion
				}
				completedAt := ""
				if ctx.CompletedAt != nil {
					completedAt = *ctx.CompletedAt
				}
				data.CheckRuns = append(data.CheckRuns, CheckRun{
					Name:        ctx.Name,
					Status:      status,
					Conclusion:  conclusion,
					CompletedAt: completedAt,
				})
			}
		}
	}

	return data
}

// FetchRemainingCheckContexts paginates through remaining statusCheckRollup contexts.
// Called when the initial query returned hasNextPage=true. Max 10 pages as a safety limit.
func FetchRemainingCheckContexts(token, owner, repo string, prNumber int, cursor string) ([]CheckRun, error) {
	var allRuns []CheckRun
	currentCursor := cursor

	for page := 0; page < 10; page++ {
		query := fmt.Sprintf(`{"query":"{repository(owner:\"%s\",name:\"%s\"){pullRequest(number:%d){commits(last:1){nodes{commit{statusCheckRollup{contexts(first:100,after:\"%s\"){pageInfo{hasNextPage endCursor}nodes{__typename ... on CheckRun{name status conclusion completedAt}}}}}}}}}}"}`,
			owner, repo, prNumber, currentCursor)

		req, err := http.NewRequest("POST", "https://api.github.com/graphql", bytes.NewReader([]byte(query)))
		if err != nil {
			return allRuns, fmt.Errorf("failed to create request for page %d: %w", page+1, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		httpClient := &http.Client{}
		resp, err := httpClient.Do(req)
		if err != nil {
			return allRuns, fmt.Errorf("failed to fetch check contexts page %d: %w", page+1, err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return allRuns, fmt.Errorf("failed to read response for page %d: %w", page+1, err)
		}

		var result struct {
			Data struct {
				Repository struct {
					PullRequest struct {
						Commits struct {
							Nodes []struct {
								Commit struct {
									StatusCheckRollup *statusCheckRollup `json:"statusCheckRollup"`
								} `json:"commit"`
							} `json:"nodes"`
						} `json:"commits"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return allRuns, fmt.Errorf("failed to parse check contexts page %d: %w", page+1, err)
		}

		commits := result.Data.Repository.PullRequest.Commits.Nodes
		if len(commits) == 0 || commits[0].Commit.StatusCheckRollup == nil {
			break
		}

		rollup := commits[0].Commit.StatusCheckRollup.Contexts
		for _, ctx := range rollup.Nodes {
			if ctx.TypeName != "CheckRun" {
				continue
			}
			status := ""
			if ctx.Status != nil {
				status = *ctx.Status
			}
			conclusion := ""
			if ctx.Conclusion != nil {
				conclusion = *ctx.Conclusion
			}
			completedAt := ""
			if ctx.CompletedAt != nil {
				completedAt = *ctx.CompletedAt
			}
			allRuns = append(allRuns, CheckRun{
				Name:        ctx.Name,
				Status:      status,
				Conclusion:  conclusion,
				CompletedAt: completedAt,
			})
		}

		if !rollup.PageInfo.HasNextPage {
			break
		}
		currentCursor = rollup.PageInfo.EndCursor
	}

	return allRuns, nil
}

// authorType converts GitHub's __typename into "user" or "bot".
func authorType(typename string) string {
	if typename == "Bot" {
		return "bot"
	}
	return "user"
}
