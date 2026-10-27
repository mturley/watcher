package github

import (
	"encoding/json"
	"fmt"
)

// PRWriteContext is what a client needs before writing to a PR's review:
// the PR's node id, its refs, whether the viewer authored it, and the
// viewer's own pending review, if any.
type PRWriteContext struct {
	PullRequestID          string // GraphQL node id
	HeadRefOid, BaseRefOid string
	ViewerDidAuthor        bool
	Pending                *PendingReview // nil when the viewer has none
}

// PendingReview is the viewer's own unsubmitted review on a PR.
type PendingReview struct {
	ID        string // node id
	CommitOid string // the commit the review is against
	Comments  []PendingComment
}

// PendingComment is one comment in a pending review.
type PendingComment struct {
	DatabaseID int64
	Path, Body string
}

// NewReviewComment is a new review thread to add to a pending review.
type NewReviewComment struct {
	Path            string
	Side            string // "LEFT" | "RIGHT"
	Line, StartLine int    // StartLine 0 for a single line
	Body            string
}

type gqlErrors []struct {
	Message string `json:"message"`
}

func (e gqlErrors) err() error {
	if len(e) == 0 {
		return nil
	}
	return fmt.Errorf("github graphql: %s", e[0].Message)
}

func endpointFor(apiURL []string) string {
	if len(apiURL) > 0 && apiURL[0] != "" {
		return apiURL[0]
	}
	return graphqlEndpoint
}

const pendingCommentFields = `pageInfo { hasNextPage endCursor } nodes { fullDatabaseId path body }`

const prWriteContextQuery = `query($owner: String!, $repo: String!, $number: Int!) { repository(owner: $owner, name: $repo) { pullRequest(number: $number) { ` +
	`id headRefOid baseRefOid viewerDidAuthor ` +
	`reviews(states: PENDING, first: 1) { nodes { id commit { oid } comments(first: 100) { ` + pendingCommentFields + ` } } } } } }`

const pendingCommentsPageQuery = `query($id: ID!, $cursor: String!) { node(id: $id) { ... on PullRequestReview { comments(first: 100, after: $cursor) { ` +
	pendingCommentFields + ` } } } }`

type pendingCommentsConn struct {
	PageInfo threadPageInfo `json:"pageInfo"`
	Nodes    []struct {
		DatabaseID fullDatabaseID `json:"fullDatabaseId"`
		Path       string         `json:"path"`
		Body       string         `json:"body"`
	} `json:"nodes"`
}

func (c pendingCommentsConn) comments() []PendingComment {
	var out []PendingComment
	for _, n := range c.Nodes {
		out = append(out, PendingComment{DatabaseID: int64(n.DatabaseID), Path: n.Path, Body: n.Body})
	}
	return out
}

// FetchPRWriteContext reads one PR's node id, refs, viewerDidAuthor and the
// viewer's pending review (GitHub returns only the viewer's own pending
// review), with every page of its comments. A rejected token wraps ErrAuth;
// any GraphQL error, even alongside data, is an error, so a failed field
// never reads as "no pending review".
func FetchPRWriteContext(token string, ref PRRef, apiURL ...string) (*PRWriteContext, error) {
	endpoint := endpointFor(apiURL)
	body, err := postGraphQL(token, endpoint, prWriteContextQuery, map[string]any{
		"owner": ref.Owner, "repo": ref.Repo, "number": ref.Number,
	})
	if err != nil {
		return nil, err
	}
	var res struct {
		Data struct {
			Repository *struct {
				PullRequest *struct {
					ID              string `json:"id"`
					HeadRefOid      string `json:"headRefOid"`
					BaseRefOid      string `json:"baseRefOid"`
					ViewerDidAuthor bool   `json:"viewerDidAuthor"`
					Reviews         struct {
						Nodes []struct {
							ID     string `json:"id"`
							Commit *struct {
								Oid string `json:"oid"`
							} `json:"commit"`
							Comments pendingCommentsConn `json:"comments"`
						} `json:"nodes"`
					} `json:"reviews"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors gqlErrors `json:"errors"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("decode graphql: %w", err)
	}
	if err := res.Errors.err(); err != nil {
		return nil, err
	}
	repo := res.Data.Repository
	if repo == nil || repo.PullRequest == nil {
		return nil, fmt.Errorf("github graphql: pull request %s/%s#%d not found", ref.Owner, ref.Repo, ref.Number)
	}
	pr := repo.PullRequest
	out := &PRWriteContext{
		PullRequestID: pr.ID, HeadRefOid: pr.HeadRefOid, BaseRefOid: pr.BaseRefOid,
		ViewerDidAuthor: pr.ViewerDidAuthor,
	}
	if len(pr.Reviews.Nodes) > 0 {
		n := pr.Reviews.Nodes[0]
		p := &PendingReview{ID: n.ID, Comments: n.Comments.comments()}
		if n.Commit != nil {
			p.CommitOid = n.Commit.Oid
		}
		if n.Comments.PageInfo.HasNextPage {
			more, err := fetchMorePendingComments(token, endpoint, n.ID, n.Comments.PageInfo.EndCursor)
			if err != nil {
				return nil, err
			}
			p.Comments = append(p.Comments, more...)
		}
		out.Pending = p
	}
	return out, nil
}

// fetchMorePendingComments pages through the rest of a pending review's
// comments, starting after cursor.
func fetchMorePendingComments(token, endpoint, reviewID, cursor string) ([]PendingComment, error) {
	var out []PendingComment
	for cursor != "" {
		body, err := postGraphQL(token, endpoint, pendingCommentsPageQuery, map[string]any{"id": reviewID, "cursor": cursor})
		if err != nil {
			return nil, err
		}
		var res struct {
			Data struct {
				Node *struct {
					Comments pendingCommentsConn `json:"comments"`
				} `json:"node"`
			} `json:"data"`
			Errors gqlErrors `json:"errors"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("decode graphql: %w", err)
		}
		if err := res.Errors.err(); err != nil {
			return nil, err
		}
		if res.Data.Node == nil {
			return nil, fmt.Errorf("github graphql: pending review %s not found", reviewID)
		}
		out = append(out, res.Data.Node.Comments.comments()...)
		cursor = ""
		if res.Data.Node.Comments.PageInfo.HasNextPage {
			cursor = res.Data.Node.Comments.PageInfo.EndCursor
		}
	}
	return out, nil
}

// mutate runs one mutation with the given input variable and decodes its
// data into out. GraphQL errors are always an error (first message), even
// alongside data.
func mutate(token string, apiURL []string, query string, input map[string]any, out any) error {
	body, err := postGraphQL(token, endpointFor(apiURL), query, map[string]any{"input": input})
	if err != nil {
		return err
	}
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors gqlErrors       `json:"errors"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return fmt.Errorf("decode graphql: %w", err)
	}
	if err := res.Errors.err(); err != nil {
		return err
	}
	if len(res.Data) == 0 || string(res.Data) == "null" {
		return fmt.Errorf("github graphql: empty response")
	}
	if err := json.Unmarshal(res.Data, out); err != nil {
		return fmt.Errorf("decode graphql: %w", err)
	}
	return nil
}

// StartReview creates a PENDING review (no event) on the PR against
// commitOid and returns its node id.
func StartReview(token, pullRequestID, commitOid string, apiURL ...string) (string, error) {
	var data struct {
		Add *struct {
			Review *struct {
				ID string `json:"id"`
			} `json:"pullRequestReview"`
		} `json:"addPullRequestReview"`
	}
	err := mutate(token, apiURL,
		`mutation($input: AddPullRequestReviewInput!) { addPullRequestReview(input: $input) { pullRequestReview { id } } }`,
		map[string]any{"pullRequestId": pullRequestID, "commitOID": commitOid}, &data)
	if err != nil {
		return "", err
	}
	if data.Add == nil || data.Add.Review == nil || data.Add.Review.ID == "" {
		return "", fmt.Errorf("github graphql: addPullRequestReview returned no review")
	}
	return data.Add.Review.ID, nil
}

// AddReviewThread adds a new thread to the pending review and returns its
// first comment's database id. startLine/startSide are sent only for a
// multi-line comment (StartLine > 0).
func AddReviewThread(token, reviewID string, c NewReviewComment, apiURL ...string) (int64, error) {
	in := map[string]any{
		"pullRequestReviewId": reviewID, "path": c.Path, "body": c.Body,
		"line": c.Line, "side": c.Side,
	}
	if c.StartLine > 0 {
		in["startLine"] = c.StartLine
		in["startSide"] = c.Side
	}
	var data struct {
		Add *struct {
			Thread *struct {
				Comments struct {
					Nodes []struct {
						DatabaseID fullDatabaseID `json:"fullDatabaseId"`
					} `json:"nodes"`
				} `json:"comments"`
			} `json:"thread"`
		} `json:"addPullRequestReviewThread"`
	}
	err := mutate(token, apiURL,
		`mutation($input: AddPullRequestReviewThreadInput!) { addPullRequestReviewThread(input: $input) { thread { comments(first: 1) { nodes { fullDatabaseId } } } } }`,
		in, &data)
	if err != nil {
		return 0, err
	}
	if data.Add == nil || data.Add.Thread == nil || len(data.Add.Thread.Comments.Nodes) == 0 {
		return 0, fmt.Errorf("github graphql: addPullRequestReviewThread returned no comment")
	}
	return int64(data.Add.Thread.Comments.Nodes[0].DatabaseID), nil
}

// AddReviewReply adds a reply to an existing thread as part of the pending
// review and returns the new comment's database id.
func AddReviewReply(token, reviewID, threadID, body string, apiURL ...string) (int64, error) {
	var data struct {
		Add *struct {
			Comment *struct {
				DatabaseID fullDatabaseID `json:"fullDatabaseId"`
			} `json:"comment"`
		} `json:"addPullRequestReviewThreadReply"`
	}
	err := mutate(token, apiURL,
		`mutation($input: AddPullRequestReviewThreadReplyInput!) { addPullRequestReviewThreadReply(input: $input) { comment { fullDatabaseId } } }`,
		map[string]any{"pullRequestReviewId": reviewID, "pullRequestReviewThreadId": threadID, "body": body}, &data)
	if err != nil {
		return 0, err
	}
	if data.Add == nil || data.Add.Comment == nil {
		return 0, fmt.Errorf("github graphql: addPullRequestReviewThreadReply returned no comment")
	}
	return int64(data.Add.Comment.DatabaseID), nil
}

// SubmitReview submits the pending review with event COMMENT, APPROVE or
// REQUEST_CHANGES and a body.
func SubmitReview(token, reviewID, event, body string, apiURL ...string) error {
	var data struct {
		Submit *struct {
			Review *struct {
				ID string `json:"id"`
			} `json:"pullRequestReview"`
		} `json:"submitPullRequestReview"`
	}
	err := mutate(token, apiURL,
		`mutation($input: SubmitPullRequestReviewInput!) { submitPullRequestReview(input: $input) { pullRequestReview { id } } }`,
		map[string]any{"pullRequestReviewId": reviewID, "event": event, "body": body}, &data)
	if err != nil {
		return err
	}
	if data.Submit == nil || data.Submit.Review == nil {
		return fmt.Errorf("github graphql: submitPullRequestReview returned no review")
	}
	return nil
}
