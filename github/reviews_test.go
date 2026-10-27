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

type gqlReq struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// writeServer records each GraphQL request (query and variables) and answers
// with the given status and body.
func writeServer(t *testing.T, code int, body string) (*httptest.Server, *[]gqlReq) {
	t.Helper()
	var reqs []gqlReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req gqlReq
		if err := json.Unmarshal(b, &req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		reqs = append(reqs, req)
		w.WriteHeader(code)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

// input returns the "input" variable of a mutation request.
func input(t *testing.T, r gqlReq) map[string]any {
	t.Helper()
	in, ok := r.Variables["input"].(map[string]any)
	if !ok {
		t.Fatalf("variables.input missing: %+v", r.Variables)
	}
	return in
}

func TestFetchPRWriteContext(t *testing.T) {
	srv, reqs := writeServer(t, 200, `{"data":{"repository":{"pullRequest":{"id":"PR_1","headRefOid":"h1","baseRefOid":"b1","viewerDidAuthor":true,
	"reviews":{"nodes":[{"id":"R_1","commit":{"oid":"c1"},"comments":{"nodes":[
	{"fullDatabaseId":"3000000001","path":"a.go","body":"one"},{"fullDatabaseId":"42","path":"b.go","body":"two"}]}}]}}}}}`)
	got, err := FetchPRWriteContext("tok", threadsRef, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	q := (*reqs)[0].Query
	for _, want := range []string{"viewerDidAuthor", "reviews(states: PENDING", "fullDatabaseId", "commit", "oid"} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %q: %s", want, q)
		}
	}
	v := (*reqs)[0].Variables
	if v["owner"] != "example" || v["repo"] != "repo" || v["number"] != float64(7) {
		t.Errorf("variables = %+v", v)
	}
	if strings.Contains(q, `"example"`) {
		t.Errorf("owner interpolated into query: %s", q)
	}
	if got.PullRequestID != "PR_1" || got.HeadRefOid != "h1" || got.BaseRefOid != "b1" || !got.ViewerDidAuthor {
		t.Errorf("ctx = %+v", got)
	}
	p := got.Pending
	if p == nil || p.ID != "R_1" || p.CommitOid != "c1" || len(p.Comments) != 2 {
		t.Fatalf("pending = %+v", p)
	}
	if p.Comments[0] != (PendingComment{DatabaseID: 3000000001, Path: "a.go", Body: "one"}) ||
		p.Comments[1] != (PendingComment{DatabaseID: 42, Path: "b.go", Body: "two"}) {
		t.Errorf("comments = %+v", p.Comments)
	}
}

func TestFetchPRWriteContextNoPending(t *testing.T) {
	srv, _ := writeServer(t, 200, `{"data":{"repository":{"pullRequest":{"id":"PR_1","headRefOid":"h","baseRefOid":"b","viewerDidAuthor":false,"reviews":{"nodes":[]}}}}}`)
	got, err := FetchPRWriteContext("tok", threadsRef, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pending != nil || got.ViewerDidAuthor {
		t.Errorf("ctx = %+v", got)
	}
}

func TestFetchPRWriteContextNotFound(t *testing.T) {
	srv, _ := writeServer(t, 200, `{"data":{"repository":null},"errors":[{"message":"Could not resolve to a Repository"}]}`)
	_, err := FetchPRWriteContext("tok", threadsRef, srv.URL)
	if err == nil || !strings.Contains(err.Error(), "Could not resolve") {
		t.Errorf("err = %v", err)
	}
}

func TestStartReview(t *testing.T) {
	srv, reqs := writeServer(t, 200, `{"data":{"addPullRequestReview":{"pullRequestReview":{"id":"R_9"}}}}`)
	id, err := StartReview("tok", "PR_1", "c1", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if id != "R_9" {
		t.Errorf("id = %q", id)
	}
	r := (*reqs)[0]
	if !strings.Contains(r.Query, "addPullRequestReview(") {
		t.Errorf("query = %s", r.Query)
	}
	in := input(t, r)
	if in["pullRequestId"] != "PR_1" || in["commitOID"] != "c1" {
		t.Errorf("input = %+v", in)
	}
	if _, ok := in["event"]; ok || strings.Contains(r.Query, "event") {
		t.Errorf("StartReview must not send an event: %s %+v", r.Query, in)
	}
}

func TestAddReviewThread(t *testing.T) {
	const resp = `{"data":{"addPullRequestReviewThread":{"thread":{"comments":{"nodes":[{"fullDatabaseId":"3000000005"}]}}}}}`
	srv, reqs := writeServer(t, 200, resp)
	id, err := AddReviewThread("tok", "R_1", NewReviewComment{Path: "a.go", Side: "RIGHT", Line: 12, Body: `say "hi"`}, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if id != 3000000005 {
		t.Errorf("id = %d", id)
	}
	r := (*reqs)[0]
	if !strings.Contains(r.Query, "addPullRequestReviewThread(") || !strings.Contains(r.Query, "fullDatabaseId") {
		t.Errorf("query = %s", r.Query)
	}
	in := input(t, r)
	if in["pullRequestReviewId"] != "R_1" || in["path"] != "a.go" || in["line"] != float64(12) ||
		in["side"] != "RIGHT" || in["body"] != `say "hi"` {
		t.Errorf("input = %+v", in)
	}
	if _, ok := in["startLine"]; ok {
		t.Errorf("single-line comment sent startLine: %+v", in)
	}
	if _, ok := in["startSide"]; ok {
		t.Errorf("single-line comment sent startSide: %+v", in)
	}

	srv2, reqs2 := writeServer(t, 200, resp)
	if _, err := AddReviewThread("tok", "R_1", NewReviewComment{Path: "a.go", Side: "LEFT", Line: 12, StartLine: 10, Body: "b"}, srv2.URL); err != nil {
		t.Fatal(err)
	}
	in2 := input(t, (*reqs2)[0])
	if in2["startLine"] != float64(10) || in2["startSide"] != "LEFT" || in2["side"] != "LEFT" {
		t.Errorf("multi-line input = %+v", in2)
	}
}

func TestAddReviewReply(t *testing.T) {
	srv, reqs := writeServer(t, 200, `{"data":{"addPullRequestReviewThreadReply":{"comment":{"fullDatabaseId":"77"}}}}`)
	id, err := AddReviewReply("tok", "R_1", "T_1", "thanks", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if id != 77 {
		t.Errorf("id = %d", id)
	}
	r := (*reqs)[0]
	if !strings.Contains(r.Query, "addPullRequestReviewThreadReply(") {
		t.Errorf("query = %s", r.Query)
	}
	in := input(t, r)
	if in["pullRequestReviewId"] != "R_1" || in["pullRequestReviewThreadId"] != "T_1" || in["body"] != "thanks" {
		t.Errorf("input = %+v", in)
	}
}

func TestSubmitReview(t *testing.T) {
	srv, reqs := writeServer(t, 200, `{"data":{"submitPullRequestReview":{"pullRequestReview":{"id":"R_1"}}}}`)
	if err := SubmitReview("tok", "R_1", "APPROVE", "lgtm", srv.URL); err != nil {
		t.Fatal(err)
	}
	r := (*reqs)[0]
	if !strings.Contains(r.Query, "submitPullRequestReview(") {
		t.Errorf("query = %s", r.Query)
	}
	in := input(t, r)
	if in["pullRequestReviewId"] != "R_1" || in["event"] != "APPROVE" || in["body"] != "lgtm" {
		t.Errorf("input = %+v", in)
	}
}

func TestWriteErrorsSurface(t *testing.T) {
	const gqlErr = `{"data":null,"errors":[{"message":"first problem"},{"message":"second"}]}`
	calls := map[string]func(url string) error{
		"FetchPRWriteContext": func(u string) error { _, err := FetchPRWriteContext("tok", threadsRef, u); return err },
		"StartReview":         func(u string) error { _, err := StartReview("tok", "PR", "c", u); return err },
		"AddReviewThread": func(u string) error {
			_, err := AddReviewThread("tok", "R", NewReviewComment{Path: "a", Side: "RIGHT", Line: 1, Body: "b"}, u)
			return err
		},
		"AddReviewReply": func(u string) error { _, err := AddReviewReply("tok", "R", "T", "b", u); return err },
		"SubmitReview":   func(u string) error { return SubmitReview("tok", "R", "COMMENT", "b", u) },
	}
	for name, call := range calls {
		srv, _ := writeServer(t, 200, gqlErr)
		if err := call(srv.URL); err == nil || !strings.Contains(err.Error(), "first problem") {
			t.Errorf("%s: graphql error = %v, want first message", name, err)
		}
		srv401, _ := writeServer(t, 401, `{"message":"Bad credentials"}`)
		if err := call(srv401.URL); !errors.Is(err, ErrAuth) {
			t.Errorf("%s: 401 err = %v, want ErrAuth", name, err)
		}
	}
}

// Existing callers send no variables: the payload stays {"query": ...}.
func TestPostGraphQLWithoutVariables(t *testing.T) {
	var raw map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &raw)
		w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()
	if _, err := postGraphQL("tok", srv.URL, "query { viewer { login } }"); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["variables"]; ok || len(raw) != 1 {
		t.Errorf("payload = %v", raw)
	}
}
