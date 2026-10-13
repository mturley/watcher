package polllog

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/mturley/watcher"
	"github.com/mturley/watcher/db"
)

type resourcesKey struct{}
type statsKey struct{}

// WithResources tags every request made with ctx with the given resources.
func WithResources(ctx context.Context, rs ...watcher.Resource) context.Context {
	return context.WithValue(ctx, resourcesKey{}, rs)
}

func ResourcesFrom(ctx context.Context) []watcher.Resource {
	rs, _ := ctx.Value(resourcesKey{}).([]watcher.Resource)
	return rs
}

// CallStats receives the status and duration of the most recent instrumented
// call made with its context, so a poller can put them in a resource's detail.
type CallStats struct {
	mu         sync.Mutex
	status     int
	durationMS int
}

// Get returns the status and duration (ms) of the last call; nil-safe.
func (s *CallStats) Get() (status, durationMS int) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.durationMS
}

func (s *CallStats) set(status, ms int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.status, s.durationMS = status, ms
	s.mu.Unlock()
}

func WithStats(ctx context.Context) (context.Context, *CallStats) {
	s := &CallStats{}
	return context.WithValue(ctx, statsKey{}, s), s
}

func statsFrom(ctx context.Context) *CallStats {
	s, _ := ctx.Value(statsKey{}).(*CallStats)
	return s
}

// Transport writes one kind=http watcher log row per request. Logging never
// fails or alters the request: on a log write error it logs to Logger.
type Transport struct {
	Base   http.RoundTripper
	Conn   *sql.DB
	Source string
	Logger *log.Logger
}

func NewClient(conn *sql.DB, source string, timeout time.Duration, logger *log.Logger) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &Transport{Conn: conn, Source: source, Logger: logger}}
}

func (t *Transport) logger() *log.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return log.Default()
}

func (t *Transport) write(e db.PollLogEntry) {
	if _, err := db.InsertPollLog(t.Conn, e); err != nil {
		t.logger().Printf("WARNING: watcher log write failed: %v", err)
	}
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	endpoint := req.URL.Host + req.URL.Path
	e := db.PollLogEntry{
		Source: t.Source, Kind: "http", Method: req.Method, Endpoint: endpoint,
		Resources: ResourcesFrom(req.Context()),
	}
	stats := statsFrom(req.Context())
	start := time.Now()
	resp, err := base.RoundTrip(req)
	elapsed := func() int { return int(time.Since(start).Milliseconds()) }
	if err != nil {
		e.Level, e.DurationMS = "error", elapsed()
		e.Message = fmt.Sprintf("%s %s failed: %s", req.Method, endpoint, SanitizeMessage(err.Error()))
		t.write(e)
		stats.set(0, e.DurationMS)
		return nil, err
	}
	if resp.StatusCode >= 400 {
		body, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		var rd io.Reader = bytes.NewReader(body)
		if rerr != nil {
			rd = io.MultiReader(rd, errReader{rerr})
		}
		resp.Body = io.NopCloser(rd)
		e.Level, e.HTTPStatus, e.Bytes, e.DurationMS = "error", resp.StatusCode, int64(len(body)), elapsed()
		e.ErrorBody = Sanitize(body, resp.Header.Get("Content-Type"))
		e.Message = fmt.Sprintf("%s %s → %d", req.Method, endpoint, resp.StatusCode)
		t.write(e)
		stats.set(resp.StatusCode, e.DurationMS)
		return resp, nil
	}
	status := resp.StatusCode
	resp.Body = &countingBody{rc: resp.Body, done: func(n int64) {
		e.Level, e.HTTPStatus, e.Bytes, e.DurationMS = "info", status, n, elapsed()
		e.Message = fmt.Sprintf("%s %s → %d", req.Method, endpoint, status)
		t.write(e)
		stats.set(status, e.DurationMS)
	}}
	return resp, nil
}

// countingBody counts bytes read and logs the call once, on Close, so the
// duration covers reading the body and the size is known even when chunked.
type countingBody struct {
	rc   io.ReadCloser
	n    int64
	once sync.Once
	done func(int64)
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingBody) Close() error {
	err := c.rc.Close()
	c.once.Do(func() { c.done(c.n) })
	return err
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
