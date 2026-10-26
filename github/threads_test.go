package github

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var threadsRef = PRRef{Owner: "example", Repo: "repo", Number: 7}

func threadsServer(t *testing.T, handle func(n int, query string) (int, string)) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct{ Query string }
		json.Unmarshal(b, &req)
		queries = append(queries, req.Query)
		code, body := handle(len(queries)-1, req.Query)
		w.WriteHeader(code)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &queries
}

func TestFetchReviewThreadsPaginates(t *testing.T) {
	srv, _ := threadsServer(t, func(n int, q string) (int, string) {
		if n == 0 {
			if strings.Contains(q, "after:") {
				t.Errorf("first query has a cursor: %s", q)
			}
			return 200, `{"data":{"repository":{"pullRequest":{"headRefName":"feat","headRefOid":"h1","baseRefOid":"b1",
			"reviewThreads":{"pageInfo":{"hasNextPage":true,"endCursor":"c1"},"nodes":[{"id":"T1","path":"a.go","comments":{"pageInfo":{"hasNextPage":false},"nodes":[]}}]}}}}}`
		}
		if !strings.Contains(q, `after: "c1"`) {
			t.Errorf("second query lacks cursor: %s", q)
		}
		return 200, `{"data":{"repository":{"pullRequest":{"headRefName":"other","headRefOid":"h2","baseRefOid":"b2",
			"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"T2","path":"b.go","comments":{"pageInfo":{"hasNextPage":false},"nodes":[]}}]}}}}}`
	})
	got, err := FetchReviewThreads("tok", threadsRef, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadRefName != "feat" || got.HeadRefOid != "h1" || got.BaseRefOid != "b1" {
		t.Errorf("refs = %+v", got)
	}
	if len(got.Threads) != 2 || got.Threads[0].ID != "T1" || got.Threads[1].ID != "T2" {
		t.Errorf("threads = %+v", got.Threads)
	}
}

func TestFetchReviewThreadsFields(t *testing.T) {
	srv, _ := threadsServer(t, func(n int, q string) (int, string) {
		return 200, `{"data":{"repository":{"pullRequest":{"headRefName":"f","headRefOid":"h","baseRefOid":"b",
		"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"T1","isResolved":true,"isOutdated":true,"path":"a.go","diffSide":"LEFT",
		"line":null,"startLine":null,"originalLine":12,"originalStartLine":10,
		"comments":{"pageInfo":{"hasNextPage":false},"nodes":[{"databaseId":99,"body":"hi","createdAt":"c","updatedAt":"u","url":"https://x/y","state":"PENDING",
		"commit":null,"originalCommit":{"oid":"abc"},"author":{"__typename":"Bot","login":"bot","avatarUrl":"https://av","databaseId":5}}]}}]}}}}}`
	})
	got, err := FetchReviewThreads("tok", threadsRef, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	th := got.Threads[0]
	if !th.IsResolved || !th.IsOutdated || th.DiffSide != "LEFT" || th.Line != 0 || th.StartLine != 0 || th.OriginalLine != 12 || th.OriginalStartLine != 10 {
		t.Errorf("thread = %+v", th)
	}
	c := th.Comments[0]
	if !c.Pending || c.DatabaseID != 99 || c.Commit != "" || c.OriginalCommit != "abc" || c.URL != "https://x/y" ||
		c.Author != "bot" || c.AuthorType != "bot" || c.AuthorID != "5" || c.AvatarURL != "https://av" || c.Body != "hi" {
		t.Errorf("comment = %+v", c)
	}
}

func TestFetchReviewThreadsMoreComments(t *testing.T) {
	srv, _ := threadsServer(t, func(n int, q string) (int, string) {
		if n == 0 {
			return 200, `{"data":{"repository":{"pullRequest":{"headRefName":"f","headRefOid":"h","baseRefOid":"b",
			"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"T1","path":"a.go",
			"comments":{"pageInfo":{"hasNextPage":true,"endCursor":"cc1"},"nodes":[{"databaseId":1,"body":"one","author":{"__typename":"User","login":"u"}}]}}]}}}}}`
		}
		if !strings.Contains(q, `node(id: "T1")`) || !strings.Contains(q, `after: "cc1"`) {
			t.Errorf("follow-up query: %s", q)
		}
		return 200, `{"data":{"node":{"comments":{"pageInfo":{"hasNextPage":false},"nodes":[{"databaseId":2,"body":"two","author":{"__typename":"User","login":"u"}}]}}}}`
	})
	got, err := FetchReviewThreads("tok", threadsRef, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	cs := got.Threads[0].Comments
	if len(cs) != 2 || cs[0].DatabaseID != 1 || cs[1].DatabaseID != 2 {
		t.Errorf("comments = %+v", cs)
	}
}

func TestFetchReviewThreadsUnauthorizedIsErrAuth(t *testing.T) {
	srv, _ := threadsServer(t, func(n int, q string) (int, string) { return 401, `{}` })
	_, err := FetchReviewThreads("tok", threadsRef, srv.URL)
	if !errors.Is(err, ErrAuth) {
		t.Errorf("err = %v", err)
	}
}

func TestFetchReviewThreadsMissingPR(t *testing.T) {
	srv, _ := threadsServer(t, func(n int, q string) (int, string) {
		return 200, `{"data":{"repository":{"pullRequest":null}}}`
	})
	if _, err := FetchReviewThreads("tok", threadsRef, srv.URL); err == nil {
		t.Error("want error")
	}
}
