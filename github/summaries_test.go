package github

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchPRSummaries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		var req struct{ Query string }
		json.Unmarshal(b, &req)
		if !strings.Contains(req.Query, `pr0: repository(owner: "example", name: "repo")`) ||
			!strings.Contains(req.Query, "pullRequest(number: 1)") || !strings.Contains(req.Query, "pr1:") {
			t.Errorf("query: %s", req.Query)
		}
		w.Write([]byte(`{"data":{
			"pr0":{"pullRequest":{"title":"One","state":"OPEN","isDraft":true,"url":"https://github.com/example/repo/pull/1","body":"see https://example.com"}},
			"pr1":null},
			"errors":[{"message":"Could not resolve to a Repository"}]}`))
	}))
	defer srv.Close()

	a := PRRef{Owner: "example", Repo: "repo", Number: 1}
	b := PRRef{Owner: "gone", Repo: "repo", Number: 2}
	got, err := FetchPRSummaries("tok", []PRRef{a, b}, srv.URL)
	if err != nil {
		t.Fatalf("partial failure must not fail: %v", err)
	}
	if s, ok := got[a]; !ok || s.Title != "One" || !s.IsDraft || s.State != "OPEN" || s.Body != "see https://example.com" {
		t.Errorf("a = %+v ok=%v", got[a], ok)
	}
	if _, ok := got[b]; ok {
		t.Error("unresolvable PR must be absent")
	}
}

func TestFetchPRSummariesEmptyAndTotalFailure(t *testing.T) {
	got, err := FetchPRSummaries("tok", nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":null,"errors":[{"message":"Bad credentials"}]}`))
	}))
	defer srv.Close()
	if _, err := FetchPRSummaries("tok", []PRRef{{Owner: "o", Repo: "r", Number: 1}}, srv.URL); err == nil {
		t.Fatal("total failure must error")
	}
}
