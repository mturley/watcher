package polllog

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mturley/watcher"
	"github.com/mturley/watcher/db"
	"github.com/mturley/watcher/testutil"
)

var quiet = log.New(io.Discard, "", 0)

func server(t *testing.T) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(504)
			io.WriteString(w, githubUnicorn)
			return
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestTransport_LogsSuccessAndFailureWithTags(t *testing.T) {
	conn := testutil.NewTestDB(t)
	ts := server(t)
	hc := NewClient(conn, "github", 5*time.Second, quiet)
	pr := watcher.Resource{Type: "pr", ID: "o/r#1"}
	ctx, stats := WithStats(WithResources(context.Background(), pr))

	req, _ := http.NewRequestWithContext(ctx, "POST", ts.URL+"/ok?token=SECRET", nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	req, _ = http.NewRequestWithContext(ctx, "POST", ts.URL+"/fail", nil)
	resp, err = hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	got, _, err := db.QueryPollLog(conn, db.PollLogQuery{})
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d entries err=%v", len(got), err)
	}
	fail, ok := got[0], got[1]
	if ok.Level != "info" || ok.HTTPStatus != 200 || ok.Bytes != int64(len(`{"ok":true}`)) || ok.Kind != "http" || ok.Method != "POST" {
		t.Fatalf("ok row: %+v", ok)
	}
	if strings.Contains(ok.Endpoint, "SECRET") || strings.Contains(ok.Message, "SECRET") || !strings.HasSuffix(ok.Endpoint, "/ok") {
		t.Fatalf("query string leaked or bad endpoint: %q / %q", ok.Endpoint, ok.Message)
	}
	if fail.Level != "error" || fail.HTTPStatus != 504 || fail.ErrorBody != "Unicorn! · GitHub" {
		t.Fatalf("fail row: %+v", fail)
	}
	if len(fail.Resources) != 1 || fail.Resources[0] != (watcher.Resource{Type: "pr", ID: "o/r#1"}) {
		t.Fatalf("tags: %+v", fail.Resources)
	}
	if st, _ := stats.Get(); st != 504 {
		t.Fatalf("stats status = %d, want 504 (last call)", st)
	}
}

func TestTransport_BodyStillReadable(t *testing.T) {
	conn := testutil.NewTestDB(t)
	ts := server(t)
	hc := NewClient(conn, "github", 5*time.Second, quiet)
	for _, path := range []string{"/ok", "/fail"} {
		resp, err := hc.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if path == "/ok" && string(b) != `{"ok":true}` {
			t.Fatalf("ok body = %q", b)
		}
		if path == "/fail" && !strings.Contains(string(b), "Unicorn!") {
			t.Fatalf("fail body not passed through intact")
		}
	}
}

func TestTransport_TransportErrorLogged(t *testing.T) {
	conn := testutil.NewTestDB(t)
	hc := NewClient(conn, "jira", time.Second, quiet)
	if _, err := hc.Get("http://127.0.0.1:1/unreachable"); err == nil {
		t.Fatal("expected dial error")
	}
	got, _, _ := db.QueryPollLog(conn, db.PollLogQuery{})
	if len(got) != 1 || got[0].Level != "error" || got[0].HTTPStatus != 0 || got[0].Source != "jira" {
		t.Fatalf("got %+v", got)
	}
}

func TestTransport_LogWriteFailureDoesNotFailRequest(t *testing.T) {
	conn := testutil.NewTestDB(t)
	ts := server(t)
	hc := NewClient(conn, "github", time.Second, quiet)
	conn.Close() // every log write now fails
	resp, err := hc.Get(ts.URL + "/ok")
	if err != nil {
		t.Fatalf("request failed because logging failed: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != `{"ok":true}` {
		t.Fatalf("body = %q", b)
	}
}

func TestWrite_KindFromResources(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := Write(conn, "github", "info", "github poll started: 2 resources"); err != nil {
		t.Fatal(err)
	}
	if err := Write(conn, "github", "error", "no data returned", watcher.Resource{Type: "pr", ID: "o/r#2"}); err != nil {
		t.Fatal(err)
	}
	got, _, err := db.QueryPollLog(conn, db.PollLogQuery{})
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d entries err=%v", len(got), err)
	}
	if got[0].Kind != "resource" || got[1].Kind != "poll" {
		t.Fatalf("kinds: %q %q", got[0].Kind, got[1].Kind)
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failingBody struct {
	r   io.Reader
	err error
}

func (f *failingBody) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err == io.EOF {
		return n, f.err
	}
	return n, err
}
func (f *failingBody) Close() error { return nil }

func TestTransport_ErrorBodyReadFailureSurfaces(t *testing.T) {
	conn := testutil.NewTestDB(t)
	boom := errors.New("boom")
	tr := &Transport{Conn: conn, Source: "github", Logger: quiet, Base: rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Header: http.Header{}, Request: r,
			Body: &failingBody{r: strings.NewReader("partial"), err: boom}}, nil
	})}
	req, _ := http.NewRequest("GET", "http://example.invalid/x", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	if !errors.Is(err, boom) || string(b) != "partial" {
		t.Fatalf("got %q err=%v", b, err)
	}
	got, _, _ := db.QueryPollLog(conn, db.PollLogQuery{})
	if len(got) != 1 || got[0].Level != "error" || got[0].Bytes != 7 {
		t.Fatalf("rows: %+v", got)
	}
}

func TestTransport_StatsAfter2xx(t *testing.T) {
	conn := testutil.NewTestDB(t)
	ts := server(t)
	hc := NewClient(conn, "github", 5*time.Second, quiet)
	ctx, stats := WithStats(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/ok", nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if st, _ := stats.Get(); st != 200 {
		t.Fatalf("status = %d", st)
	}
	if st, d := (*CallStats)(nil).Get(); st != 0 || d != 0 {
		t.Fatal("nil Get not zero")
	}
}

func TestTransport_RedirectIsNotError(t *testing.T) {
	conn := testutil.NewTestDB(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusFound)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer ts.Close()
	hc := NewClient(conn, "github", 5*time.Second, quiet)
	resp, err := hc.Get(ts.URL + "/old")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	got, _, _ := db.QueryPollLog(conn, db.PollLogQuery{})
	if len(got) != 2 {
		t.Fatalf("rows: %d", len(got))
	}
	for _, e := range got {
		if e.Level != "info" {
			t.Fatalf("unexpected level: %+v", e)
		}
	}
}
