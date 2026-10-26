package github

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// PRReviewThreads is a PR's review threads plus the refs they were made
// against.
type PRReviewThreads struct {
	HeadRefName, HeadRefOid, BaseRefOid string
	Threads                             []ReviewThread
}

// ReviewThread is one review conversation on a file. Line fields are 0
// where GitHub returns null (e.g. Line for an outdated thread).
type ReviewThread struct {
	ID                     string
	IsResolved, IsOutdated bool
	Path                   string
	DiffSide               string // "LEFT" | "RIGHT"
	Line, StartLine        int
	OriginalLine           int
	OriginalStartLine      int
	Comments               []ThreadComment // oldest first
}

// ThreadComment is one comment in a review thread.
type ThreadComment struct {
	DatabaseID                              int64
	Author, AuthorType, AuthorID, AvatarURL string
	Body, CreatedAt, UpdatedAt, URL         string
	Commit, OriginalCommit                  string // oids; "" when null
	Pending                                 bool   // state == PENDING
}

type threadCommentNode struct {
	DatabaseID fullDatabaseID `json:"fullDatabaseId"`
	Body       string         `json:"body"`
	CreatedAt  string         `json:"createdAt"`
	UpdatedAt  string         `json:"updatedAt"`
	URL        string         `json:"url"`
	State      string         `json:"state"`
	Commit     *struct {
		Oid string `json:"oid"`
	} `json:"commit"`
	OriginalCommit *struct {
		Oid string `json:"oid"`
	} `json:"originalCommit"`
	Author *struct {
		authorNode
		AvatarURL string `json:"avatarUrl"`
	} `json:"author"`
}

type threadPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type threadCommentsConn struct {
	PageInfo threadPageInfo      `json:"pageInfo"`
	Nodes    []threadCommentNode `json:"nodes"`
}

const threadCommentFields = `fullDatabaseId body createdAt updatedAt url state commit{oid} originalCommit{oid} author{__typename login avatarUrl ... on User{databaseId} ... on Bot{databaseId}}`

func (n threadCommentNode) toComment() ThreadComment {
	c := ThreadComment{
		DatabaseID: int64(n.DatabaseID), Body: n.Body, CreatedAt: n.CreatedAt,
		UpdatedAt: n.UpdatedAt, URL: n.URL, Pending: n.State == "PENDING",
	}
	if n.Commit != nil {
		c.Commit = n.Commit.Oid
	}
	if n.OriginalCommit != nil {
		c.OriginalCommit = n.OriginalCommit.Oid
	}
	if n.Author != nil {
		c.Author = n.Author.Login
		c.AuthorType = authorType(n.Author.Typename)
		c.AuthorID = n.Author.id()
		c.AvatarURL = n.Author.AvatarURL
	}
	return c
}

// FetchReviewThreads fetches every review thread of one PR (all pages, and
// every page of each thread's comments). A rejected token wraps ErrAuth; a
// PR that can't be resolved is an error.
func FetchReviewThreads(token string, ref PRRef, apiURL ...string) (*PRReviewThreads, error) {
	endpoint := "https://api.github.com/graphql"
	if len(apiURL) > 0 && apiURL[0] != "" {
		endpoint = apiURL[0]
	}
	out := &PRReviewThreads{}
	cursor := ""
	for first := true; ; first = false {
		after := ""
		if cursor != "" {
			after = ", after: " + strconv.Quote(cursor)
		}
		q := fmt.Sprintf(`query { repository(owner: %s, name: %s) { pullRequest(number: %d) { headRefName headRefOid baseRefOid `+
			`reviewThreads(first: 100%s) { pageInfo{hasNextPage endCursor} nodes{ id isResolved isOutdated path diffSide line startLine originalLine originalStartLine `+
			`comments(first: 100) { pageInfo{hasNextPage endCursor} nodes{ %s } } } } } } }`,
			strconv.Quote(ref.Owner), strconv.Quote(ref.Repo), ref.Number, after, threadCommentFields)
		body, err := postGraphQL(token, endpoint, q)
		if err != nil {
			return nil, err
		}
		var res struct {
			Data struct {
				Repository *struct {
					PullRequest *struct {
						HeadRefName   string `json:"headRefName"`
						HeadRefOid    string `json:"headRefOid"`
						BaseRefOid    string `json:"baseRefOid"`
						ReviewThreads struct {
							PageInfo threadPageInfo `json:"pageInfo"`
							Nodes    []struct {
								ID                string             `json:"id"`
								IsResolved        bool               `json:"isResolved"`
								IsOutdated        bool               `json:"isOutdated"`
								Path              string             `json:"path"`
								DiffSide          string             `json:"diffSide"`
								Line              *int               `json:"line"`
								StartLine         *int               `json:"startLine"`
								OriginalLine      *int               `json:"originalLine"`
								OriginalStartLine *int               `json:"originalStartLine"`
								Comments          threadCommentsConn `json:"comments"`
							} `json:"nodes"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("decode graphql: %w", err)
		}
		repo := res.Data.Repository
		if repo == nil || repo.PullRequest == nil {
			if len(res.Errors) > 0 {
				return nil, fmt.Errorf("github graphql: %s", res.Errors[0].Message)
			}
			return nil, fmt.Errorf("github graphql: pull request %s/%s#%d not found", ref.Owner, ref.Repo, ref.Number)
		}
		pr := repo.PullRequest
		if first {
			out.HeadRefName, out.HeadRefOid, out.BaseRefOid = pr.HeadRefName, pr.HeadRefOid, pr.BaseRefOid
		}
		for _, n := range pr.ReviewThreads.Nodes {
			t := ReviewThread{
				ID: n.ID, IsResolved: n.IsResolved, IsOutdated: n.IsOutdated, Path: n.Path, DiffSide: n.DiffSide,
				Line: intOrZero(n.Line), StartLine: intOrZero(n.StartLine),
				OriginalLine: intOrZero(n.OriginalLine), OriginalStartLine: intOrZero(n.OriginalStartLine),
			}
			for _, c := range n.Comments.Nodes {
				t.Comments = append(t.Comments, c.toComment())
			}
			if n.Comments.PageInfo.HasNextPage {
				more, err := fetchMoreThreadComments(token, endpoint, n.ID, n.Comments.PageInfo.EndCursor)
				if err != nil {
					return nil, err
				}
				t.Comments = append(t.Comments, more...)
			}
			out.Threads = append(out.Threads, t)
		}
		if !pr.ReviewThreads.PageInfo.HasNextPage {
			return out, nil
		}
		cursor = pr.ReviewThreads.PageInfo.EndCursor
	}
}

func intOrZero(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// fetchMoreThreadComments pages through the rest of one thread's comments.
func fetchMoreThreadComments(token, endpoint, threadID, cursor string) ([]ThreadComment, error) {
	var out []ThreadComment
	for cursor != "" {
		q := fmt.Sprintf(`query { node(id: %s) { ... on PullRequestReviewThread { comments(first: 100, after: %s) { pageInfo{hasNextPage endCursor} nodes{ %s } } } } }`,
			strconv.Quote(threadID), strconv.Quote(cursor), threadCommentFields)
		body, err := postGraphQL(token, endpoint, q)
		if err != nil {
			return nil, err
		}
		var res struct {
			Data struct {
				Node *struct {
					Comments threadCommentsConn `json:"comments"`
				} `json:"node"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("decode graphql: %w", err)
		}
		if res.Data.Node == nil {
			return nil, fmt.Errorf("github graphql: review thread %s not found", threadID)
		}
		for _, c := range res.Data.Node.Comments.Nodes {
			out = append(out, c.toComment())
		}
		cursor = ""
		if res.Data.Node.Comments.PageInfo.HasNextPage {
			cursor = res.Data.Node.Comments.PageInfo.EndCursor
		}
	}
	return out, nil
}
