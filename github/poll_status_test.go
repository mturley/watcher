package github

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

var quietLog = log.New(io.Discard, "", 0)

func TestPoll_BatchFailureRecordsEveryPR(t *testing.T) {
	conn := testutil.NewTestDB(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(504)
		io.WriteString(w, "<!DOCTYPE html><html><head><title>Unicorn! &middot; GitHub</title></head></html>")
	}))
	defer ts.Close()
	prs := []watcher.Resource{{Type: "pr", ID: "o/r#1"}, {Type: "pr", ID: "o/r#2"}}

	if err := pollWith(conn, "tok", prs, quietLog, ts.URL); err == nil {
		t.Fatal("expected error from 504")
	}
	st, _ := db.GetResourcePollStatuses(conn, prs)
	for _, r := range prs {
		s := st[db.ResourceKey{Type: r.Type, ID: r.ID}]
		if !s.Failing() || s.Detail == nil || s.Detail.HTTPStatus != 504 {
			t.Fatalf("%s: %+v", r.ID, s)
		}
		if strings.Contains(s.LastErrorMessage, "<") || !strings.Contains(s.LastErrorMessage, "Unicorn! · GitHub") {
			t.Fatalf("message not sanitized: %q", s.LastErrorMessage)
		}
	}
	ps, _ := db.GetPollerStatus(conn, "github")
	if strings.Contains(ps.LastErrorMessage, "<html") {
		t.Fatalf("poller status message not sanitized: %q", ps.LastErrorMessage)
	}
	rows, _, _ := db.QueryPollLog(conn, db.PollLogQuery{})
	var kinds []string
	for _, e := range rows {
		kinds = append(kinds, e.Kind+":"+e.Level)
	}
	// newest first: finish(poll:error), resource:error (tagged both), http:error, start(poll:info)
	want := "poll:error,resource:error,http:error,poll:info"
	if got := strings.Join(kinds, ","); got != want {
		t.Fatalf("log kinds = %s, want %s", got, want)
	}
	if len(rows[1].Resources) != 2 {
		t.Fatalf("resource row tags = %+v", rows[1].Resources)
	}
}

func TestPoll_MissingPRRecordedAsError(t *testing.T) {
	conn := testutil.NewTestDB(t)
	// GraphQL nulls the alias of an unresolvable repo; the batch succeeds.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"rateLimit":{"limit":5000,"remaining":4999},"pr0":null}}`)
	}))
	defer ts.Close()
	pr := watcher.Resource{Type: "pr", ID: "o/gone#1"}
	if err := pollWith(conn, "tok", []watcher.Resource{pr}, quietLog, ts.URL); err != nil {
		t.Fatalf("partial batch should not fail the poll: %v", err)
	}
	st, _ := db.GetResourcePollStatuses(conn, []watcher.Resource{pr})
	s := st[db.ResourceKey{Type: "pr", ID: "o/gone#1"}]
	if !s.Failing() || !strings.Contains(s.LastErrorMessage, "no data returned") {
		t.Fatalf("missing PR: %+v", s)
	}
}

func TestPoll_PartialBatchMissingPR(t *testing.T) {
	conn := testutil.NewTestDB(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"rateLimit":{"remaining":4999,"limit":5000},
"pr0":null,
"pr1":{"pullRequest":{"number":9097,"title":"Real PR","state":"OPEN","url":"https://github.com/o/r/pull/9097","author":{"login":"octocat"}}}},
"errors":[{"message":"nope"}]}`)
	}))
	defer ts.Close()
	gone := watcher.Resource{Type: "pr", ID: "o/gone#1"}
	good := watcher.Resource{Type: "pr", ID: "o/r#9097"}
	rs := []watcher.Resource{gone, good}
	if err := pollWith(conn, "tok", rs, quietLog, ts.URL); err != nil {
		t.Fatalf("partial batch should not fail the poll: %v", err)
	}
	st, _ := db.GetResourcePollStatuses(conn, rs)
	g := st[db.ResourceKey{Type: "pr", ID: "o/gone#1"}]
	if !g.Failing() || !strings.Contains(g.LastErrorMessage, "no data returned") {
		t.Fatalf("missing PR: %+v", g)
	}
	ok := st[db.ResourceKey{Type: "pr", ID: "o/r#9097"}]
	if ok.Failing() || ok.Detail == nil || !ok.Detail.OK || ok.Detail.HTTPStatus != 200 {
		t.Fatalf("good PR: %+v", ok)
	}
	rows, _, _ := db.QueryPollLog(conn, db.PollLogQuery{})
	if len(rows) == 0 || !strings.Contains(rows[0].Message, "rate limit") {
		t.Fatalf("finish row: %+v", rows)
	}
}

func TestPoll_InvalidResourceIDRecorded(t *testing.T) {
	conn := testutil.NewTestDB(t)
	bad := watcher.Resource{Type: "pr", ID: "not-a-pr"}
	_ = pollWith(conn, "tok", []watcher.Resource{bad}, quietLog, "http://127.0.0.1:1")
	st, _ := db.GetResourcePollStatuses(conn, []watcher.Resource{bad})
	if !st[db.ResourceKey{Type: "pr", ID: "not-a-pr"}].Failing() {
		t.Fatal("invalid resource id not recorded as error")
	}
}
