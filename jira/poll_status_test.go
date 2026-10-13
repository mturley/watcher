package jira

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mturley/watcher"
	"github.com/mturley/watcher/db"
	"github.com/mturley/watcher/testutil"
)

const goodIssueJSON = `{"key":"GOOD-1","fields":{"summary":"Issue with reporter","status":{"name":"Open"},"reporter":{"displayName":"Bob Reporter"}}}`

func TestPoll_PerIssueOutcomesAndTaggedHTTPRows(t *testing.T) {
	conn := testutil.NewTestDB(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "BAD-1"):
			w.WriteHeader(503)
			io.WriteString(w, `{"message":"","status":503}`)
		case strings.HasSuffix(r.URL.Path, "/changelog"):
			io.WriteString(w, `{"values":[],"isLast":true,"startAt":0,"maxResults":100,"total":0}`)
		case strings.HasSuffix(r.URL.Path, "/comment"):
			io.WriteString(w, `{"startAt":0,"maxResults":100,"total":0,"comments":[]}`)
		default:
			io.WriteString(w, goodIssueJSON)
		}
	}))
	defer ts.Close()
	good := watcher.Resource{Type: "jira", ID: "GOOD-1"}
	bad := watcher.Resource{Type: "jira", ID: "BAD-1"}
	auth := JiraAuth{URL: ts.URL, Email: "e", Token: "t"}

	if err := Poll(conn, auth, []watcher.Resource{good, bad}, log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}
	st, _ := db.GetResourcePollStatuses(conn, []watcher.Resource{good, bad})
	if g := st[db.ResourceKey{Type: "jira", ID: "GOOD-1"}]; g.Failing() || g.Detail == nil || !g.Detail.OK {
		t.Fatalf("good: %+v", g)
	}
	if b := st[db.ResourceKey{Type: "jira", ID: "BAD-1"}]; !b.Failing() || b.Detail.HTTPStatus != 503 {
		t.Fatalf("bad: %+v detail %+v", b, b.Detail)
	}
	rows, _, _ := db.QueryPollLog(conn, db.PollLogQuery{ResourceScoped: true, Resources: []watcher.Resource{bad}})
	var sawHTTP bool
	for _, e := range rows {
		if e.Kind == "http" && e.HTTPStatus == 503 {
			sawHTTP = true
		}
		if e.Kind == "http" && len(e.Resources) == 1 && e.Resources[0].ID != "BAD-1" {
			t.Fatalf("http row mis-tagged: %+v", e)
		}
	}
	if !sawHTTP {
		t.Fatalf("no tagged 503 http row in %+v", rows)
	}
}
