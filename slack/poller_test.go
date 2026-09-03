package slack

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"testing"

	"github.com/mturley/watcher"
	"github.com/mturley/watcher/db"
	"github.com/mturley/watcher/testutil"
	"strings"
)

// testLogger returns a logger that discards output.
func testLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

// slackResource is the resource under test across cases.
var slackResource = watcher.Resource{
	Type: "slack",
	ID:   "C1:1699000000.000100",
}

// fakeClient implements Client with real Replies/Users/Channel behavior for
// the poller test; everything else returns zero values. The repliesErr /
// usersErr / channelErr fields let a test inject failures on those calls to
// exercise the poller's error-handling and degradation paths.
type fakeClient struct {
	thread       Thread
	users        map[string]User
	channelName  string
	channelCalls int
	repliesErr   error
	usersErr     error
	channelErr   error
}

func (f *fakeClient) AuthTest(ctx context.Context) error         { return nil }
func (f *fakeClient) WhoAmI(ctx context.Context) (string, error) { return "", nil }

func (f *fakeClient) Replies(ctx context.Context, channel, threadTS string) (Thread, error) {
	if f.repliesErr != nil {
		return Thread{}, f.repliesErr
	}
	return f.thread, nil
}

func (f *fakeClient) Users(ctx context.Context, ids []string) (map[string]User, error) {
	if f.usersErr != nil {
		return nil, f.usersErr
	}
	out := make(map[string]User, len(ids))
	for _, id := range ids {
		if u, ok := f.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (f *fakeClient) Channel(ctx context.Context, id string) (string, error) {
	f.channelCalls++
	if f.channelErr != nil {
		return "", f.channelErr
	}
	return f.channelName, nil
}

func (f *fakeClient) Emoji(ctx context.Context) (map[string]string, error)               { return nil, nil }
func (f *fakeClient) UserGroups(ctx context.Context) (map[string]UserGroup, error)       { return nil, nil }
func (f *fakeClient) UserGroupsInfo(ctx context.Context, ids []string) (map[string]UserGroup, error) {
	return nil, nil
}
func (f *fakeClient) MarkRead(ctx context.Context, channel, threadTS, ts string) error   { return nil }
func (f *fakeClient) MarkUnread(ctx context.Context, channel, threadTS, ts string) error { return nil }
func (f *fakeClient) PostReply(ctx context.Context, channel, threadTS, text string) (Message, error) {
	return Message{}, nil
}
func (f *fakeClient) AddReaction(ctx context.Context, channel, ts, name string) error    { return nil }
func (f *fakeClient) RemoveReaction(ctx context.Context, channel, ts, name string) error { return nil }

func TestPoll_FirstPollWatchStartedOnly(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	root := Message{TS: "1699000000.000100", UserID: "U1", Text: "hello from the root message"}
	client := &fakeClient{
		thread: Thread{
			Channel:  "C1",
			ThreadTS: "1699000000.000100",
			Messages: []Message{root},
		},
		users: map[string]User{
			"U1": {ID: "U1", DisplayName: "Alice"},
		},
		channelName: "wg-dashboard-zaffre",
	}

	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatalf("poll #1: %v", err)
	}

	events, err := db.EventsForResource(conn, "slack", slackResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 consumer-visible events on first poll, got %d: %+v", len(events), events)
	}

	var watchStartedCount int
	if err := conn.QueryRow(`
		SELECT COUNT(*) FROM watcher_events e
		JOIN watcher_event_resources er ON er.event_id = e.id
		WHERE er.resource_type = ? AND er.resource_id = ? AND e.type = ?
	`, "slack", slackResource.ID, string(watcher.EventTypeWatchStarted)).Scan(&watchStartedCount); err != nil {
		t.Fatalf("query watch_started count: %v", err)
	}
	if watchStartedCount != 1 {
		t.Errorf("expected exactly 1 watch_started event, got %d", watchStartedCount)
	}

	var slackReplyCount int
	if err := conn.QueryRow(`
		SELECT COUNT(*) FROM watcher_events e
		JOIN watcher_event_resources er ON er.event_id = e.id
		WHERE er.resource_type = ? AND er.resource_id = ? AND e.type = ?
	`, "slack", slackResource.ID, string(watcher.EventTypeSlackReply)).Scan(&slackReplyCount); err != nil {
		t.Fatalf("query slack_reply count: %v", err)
	}
	if slackReplyCount != 0 {
		t.Errorf("expected 0 slack_reply events on first poll, got %d", slackReplyCount)
	}

	st, err := db.GetResourceState(conn, "slack", slackResource.ID)
	if err != nil || st == nil {
		t.Fatalf("GetResourceState: %v, st=%v", err, st)
	}
	var state map[string]interface{}
	if err := json.Unmarshal([]byte(st.StateJSON), &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state["title"] != fallbackTitle(root.Text) {
		t.Errorf("title = %v, want %v", state["title"], fallbackTitle(root.Text))
	}
	if state["channel_name"] != "wg-dashboard-zaffre" {
		t.Errorf("channel_name = %v, want wg-dashboard-zaffre", state["channel_name"])
	}
	if state["author"] != "Alice" {
		t.Errorf("author = %v, want Alice", state["author"])
	}
	if state["created_ts"] != root.TS {
		t.Errorf("created_ts = %v, want %v", state["created_ts"], root.TS)
	}
	if state["updated_ts"] != root.TS {
		t.Errorf("updated_ts = %v, want %v", state["updated_ts"], root.TS)
	}

	if client.channelCalls != 1 {
		t.Fatalf("expected 1 Channel() call after first poll, got %d", client.channelCalls)
	}

	// Second poll with no new replies: Channel() must not be called again
	// (stable-name caching).
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatalf("poll #2 (no new replies): %v", err)
	}
	if client.channelCalls != 1 {
		t.Errorf("expected Channel() still called exactly once across two polls, got %d", client.channelCalls)
	}
}

func TestPoll_NewReplyEmitsSlackReplyWithVerbatimTS(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	root := Message{TS: "1699000000.000100", UserID: "U1", Text: "hello from the root message"}
	reply := Message{TS: "1699000001.000200", UserID: "U2", Text: "a reply to the thread"}

	client := &fakeClient{
		thread: Thread{
			Channel:  "C1",
			ThreadTS: "1699000000.000100",
			Messages: []Message{root},
		},
		users: map[string]User{
			"U1": {ID: "U1", DisplayName: "Alice"},
			"U2": {ID: "U2", DisplayName: "Bob"},
		},
		channelName: "wg-dashboard-zaffre",
	}

	// First poll seeds the cursor via watch_started.
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatalf("poll #1: %v", err)
	}

	// Second poll: a new reply has arrived.
	client.thread.Messages = []Message{root, reply}
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatalf("poll #2: %v", err)
	}

	events, err := db.EventsForResource(conn, "slack", slackResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	var replyEvents []watcher.Event
	for _, e := range events {
		if e.Type == watcher.EventTypeSlackReply {
			replyEvents = append(replyEvents, e)
		}
	}
	if len(replyEvents) != 1 {
		t.Fatalf("expected exactly 1 slack_reply event, got %d: %+v", len(replyEvents), replyEvents)
	}
	ev := replyEvents[0]
	if ev.ExternalTS == nil || *ev.ExternalTS != reply.TS {
		t.Errorf("ExternalTS = %v, want verbatim %q (raw Slack ts, never RFC3339)", ev.ExternalTS, reply.TS)
	}
	if ev.Body == nil {
		t.Fatal("expected non-nil body")
	}
	wantBody := "Bob: " + fallbackTitle(reply.Text)
	if *ev.Body != wantBody {
		t.Errorf("Body = %q, want %q", *ev.Body, wantBody)
	}

	if client.channelCalls != 1 {
		t.Errorf("expected Channel() called exactly once across two polls, got %d", client.channelCalls)
	}

	// Third poll with no new replies: zero new events, cursor holds via dedup.
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatalf("poll #3: %v", err)
	}
	events, err = db.EventsForResource(conn, "slack", slackResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource after poll #3: %v", err)
	}
	replyEvents = nil
	for _, e := range events {
		if e.Type == watcher.EventTypeSlackReply {
			replyEvents = append(replyEvents, e)
		}
	}
	if len(replyEvents) != 1 {
		t.Fatalf("expected still exactly 1 slack_reply event after poll #3 (no new replies), got %d", len(replyEvents))
	}
}

// eventTypeCount returns how many events of type t are linked to the resource.
func eventTypeCount(t *testing.T, conn *sql.DB, resourceID string, evType watcher.EventType) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(`
		SELECT COUNT(*) FROM watcher_events e
		JOIN watcher_event_resources er ON er.event_id = e.id
		WHERE er.resource_type = ? AND er.resource_id = ? AND e.type = ?
	`, "slack", resourceID, string(evType)).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", evType, err)
	}
	return n
}

func TestPoll_RepliesErrorEmitsWatcherError(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	client := &fakeClient{repliesErr: errors.New("boom")}

	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatalf("pollOnce should not return an error on a Replies failure, got %v", err)
	}
	if got := eventTypeCount(t, conn, slackResource.ID, watcher.EventTypeWatcherError); got != 1 {
		t.Errorf("expected 1 watcher_error event on Replies failure, got %d", got)
	}
	// No watch_started / slack_reply should be emitted when the fetch failed.
	if got := eventTypeCount(t, conn, slackResource.ID, watcher.EventTypeWatchStarted); got != 0 {
		t.Errorf("expected 0 watch_started on Replies failure, got %d", got)
	}
}

func TestPoll_ZeroMessageThreadEmitsNothing(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Root deleted out-of-band: Replies returns a thread with no messages.
	client := &fakeClient{thread: Thread{Channel: "C1", ThreadTS: "1699000000.000100", Messages: nil}}

	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if got := eventTypeCount(t, conn, slackResource.ID, watcher.EventTypeWatchStarted); got != 0 {
		t.Errorf("expected 0 watch_started for a zero-message thread, got %d", got)
	}
	// No resource_state row should have been written (empty ts would poison it).
	st, err := db.GetResourceState(conn, "slack", slackResource.ID)
	if err != nil {
		t.Fatalf("GetResourceState: %v", err)
	}
	if st != nil {
		t.Errorf("expected no resource_state for a zero-message thread, got %+v", st)
	}
}

func TestPoll_UsersErrorEmitsReplyWithoutAuthor(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	root := Message{TS: "1699000000.000100", UserID: "U1", Text: "root message"}
	reply := Message{TS: "1699000001.000200", UserID: "U2", Text: "a reply"}
	client := &fakeClient{
		thread:      Thread{Channel: "C1", ThreadTS: "1699000000.000100", Messages: []Message{root}},
		channelName: "wg-dashboard-zaffre",
		usersErr:    errors.New("users boom"),
	}
	// Poll #1 seeds cursor.
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatalf("poll #1: %v", err)
	}
	// Poll #2: a reply arrives, but Users() fails so author resolution degrades.
	client.thread.Messages = []Message{root, reply}
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatalf("poll #2: %v", err)
	}

	events, err := db.EventsForResource(conn, "slack", slackResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	var reply1 *watcher.Event
	for i := range events {
		if events[i].Type == watcher.EventTypeSlackReply {
			reply1 = &events[i]
		}
	}
	if reply1 == nil {
		t.Fatal("expected a slack_reply event even when Users() failed")
	}
	if reply1.Author != nil {
		t.Errorf("expected nil author when Users() failed, got %v", *reply1.Author)
	}
	if reply1.Body == nil || *reply1.Body != fallbackTitle(reply.Text) {
		t.Errorf("Body = %v, want %q (no 'author: ' prefix)", reply1.Body, fallbackTitle(reply.Text))
	}
	// Cached author should be empty.
	st, err := db.GetResourceState(conn, "slack", slackResource.ID)
	if err != nil || st == nil {
		t.Fatalf("GetResourceState: %v st=%v", err, st)
	}
	var state map[string]interface{}
	if err := json.Unmarshal([]byte(st.StateJSON), &state); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if state["author"] != "" {
		t.Errorf("cached author = %v, want empty on Users() failure", state["author"])
	}
}

func TestPoll_ChannelErrorLeavesChannelEmptyAndRetries(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	root := Message{TS: "1699000000.000100", UserID: "U1", Text: "root message"}
	client := &fakeClient{
		thread:      Thread{Channel: "C1", ThreadTS: "1699000000.000100", Messages: []Message{root}},
		channelName: "wg-dashboard-zaffre",
		channelErr:  errors.New("channel boom"),
	}
	// Poll #1: Channel() fails → channel_name cached as "".
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatalf("poll #1: %v", err)
	}
	st, err := db.GetResourceState(conn, "slack", slackResource.ID)
	if err != nil || st == nil {
		t.Fatalf("GetResourceState: %v st=%v", err, st)
	}
	var state map[string]interface{}
	if err := json.Unmarshal([]byte(st.StateJSON), &state); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if state["channel_name"] != "" {
		t.Errorf("channel_name = %v, want empty on Channel() failure", state["channel_name"])
	}
	if client.channelCalls != 1 {
		t.Fatalf("expected 1 Channel() call, got %d", client.channelCalls)
	}
	// Poll #2: since nothing was cached, Channel() is retried (and now succeeds).
	client.channelErr = nil
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatalf("poll #2: %v", err)
	}
	if client.channelCalls != 2 {
		t.Errorf("expected Channel() retried (2 calls) after a failed lookup, got %d", client.channelCalls)
	}
	st, _ = db.GetResourceState(conn, "slack", slackResource.ID)
	_ = json.Unmarshal([]byte(st.StateJSON), &state)
	if state["channel_name"] != "wg-dashboard-zaffre" {
		t.Errorf("channel_name after retry = %v, want wg-dashboard-zaffre", state["channel_name"])
	}
}

func TestPoll_MalformedResourceIDSkipped(t *testing.T) {
	conn := testutil.NewTestDB(t)
	bad := watcher.Resource{Type: "slack", ID: "no-colon-here"}
	if err := db.Subscribe(conn, "test-sub", bad, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	client := &fakeClient{}
	// Must not panic; returns nil and emits nothing for the bad id.
	if err := pollOnce(conn, client, bad, testLogger()); err != nil {
		t.Fatalf("pollOnce on malformed id should return nil, got %v", err)
	}
	events, err := db.EventsForResource(conn, "slack", bad.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected no events for a malformed resource id, got %d", len(events))
	}
}

// pollOnce drives the SAME sequence Poll performs for a single resource,
// against an injected fake Client (Poll itself always constructs a real
// *HTTPClient via New(), so it can't be used directly with a fake). It mirrors
// Poll's error branches — emitError + RecordPollerError on a Replies failure,
// emitError on a processThread failure, the zero-message guard, and the state
// cache write — so the tests exercise the real error-handling wiring rather
// than a divergent shortcut. Per-resource "continue" in Poll maps to "return
// nil" here (a single resource).
func pollOnce(conn *sql.DB, client Client, resource watcher.Resource, logger *log.Logger) error {
	channel, threadTS, ok := parseResourceID(resource.ID)
	if !ok {
		logger.Printf("ERROR: bad slack resource id %q", resource.ID)
		return nil
	}
	thread, err := client.Replies(context.Background(), channel, threadTS)
	if err != nil {
		errBody := "Failed to fetch thread: " + err.Error()
		if e := emitError(conn, "Failed to fetch "+resource.ID, &errBody, resource); e != nil {
			logger.Printf("ERROR: emit watcher error: %v", e)
		}
		if e := db.RecordPollerError(conn, "slack", errBody); e != nil {
			logger.Printf("ERROR: record poller error: %v", e)
		}
		return nil
	}
	if len(thread.Messages) == 0 {
		logger.Printf("WARNING: slack thread %s has no messages; skipping this cycle", resource.ID)
		return nil
	}
	backfill, err := db.BackfillFor(conn, resource.Type, resource.ID)
	if err != nil {
		logger.Printf("WARNING: backfill resolve %s: %v", resource.ID, err)
	}
	names := resolveAuthors(client, thread.Messages)
	if _, err := processThread(conn, thread, names, resource, backfill, logger); err != nil {
		errBody := "Failed to process thread: " + err.Error()
		if e := emitError(conn, "Error processing "+resource.ID, &errBody, resource); e != nil {
			logger.Printf("ERROR: emit watcher error: %v", e)
		}
		return nil
	}
	channelName := resolveChannelName(conn, client, resource, channel)
	rootAuthor := ""
	if len(thread.Messages) > 0 {
		rootAuthor = names[thread.Messages[0].UserID]
	}
	stateJSON := buildSlackStateJSON(thread, channelName, rootAuthor, rootText(thread))
	latestTS := latestThreadTS(thread)
	return db.UpsertResourceState(conn, "slack", resource.ID, stateJSON, latestTS, "2026-08-19T00:00:00Z")
}

// TestCachedTitleResolvesMentions pins the whole point of the Go-side
// resolver: the cached card title must match what the live thread view
// renders. Before this, a title cached from raw text showed "<@U1>" for the
// same message that displayed "@ana" one pane over.
func TestCachedTitleResolvesMentions(t *testing.T) {
	thread := Thread{Messages: []Message{
		{UserID: "U1", TS: "1.0", Text: "hey <@U2> can <!subteam^S1> review this?"},
	}}
	resolved := ResolveMentions(
		thread.Messages[0].Text,
		map[string]string{"U2": "bo"},
		map[string]UserGroup{"S1": {ID: "S1", Handle: "platform"}},
	)
	stateJSON := buildSlackStateJSON(thread, "eng", "ana", resolved)

	if strings.Contains(stateJSON, "<@U2>") || strings.Contains(stateJSON, "subteam") {
		t.Fatalf("cached title still holds raw mention tokens: %s", stateJSON)
	}
	if !strings.Contains(stateJSON, "@bo") || !strings.Contains(stateJSON, "@platform") {
		t.Fatalf("cached title missing resolved names: %s", stateJSON)
	}
}

// TestCachedStateCarriesUnread pins the data the worktree UI's unread dot
// needs. It is computed at poll time from the read cursor Slack already
// returns with the thread, so a consumer can show unread state per thread
// without a per-thread fetch of its own.
func TestCachedStateCarriesUnread(t *testing.T) {
	decode := func(js string) map[string]interface{} {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(js), &m); err != nil {
			t.Fatalf("state json: %v", err)
		}
		return m
	}

	// Read cursor behind the newest message => unread.
	behind := Thread{LastRead: "1.0", Messages: []Message{
		{UserID: "U1", TS: "1.0", Text: "root"},
		{UserID: "U2", TS: "2.0", Text: "reply"},
	}}
	if got := decode(buildSlackStateJSON(behind, "eng", "ana", "root"))["has_unread"]; got != true {
		t.Fatalf("has_unread = %v, want true when last_read is behind the newest message", got)
	}

	// Caught up => read.
	caught := Thread{LastRead: "2.0", Messages: behind.Messages}
	if got := decode(buildSlackStateJSON(caught, "eng", "ana", "root"))["has_unread"]; got != false {
		t.Fatalf("has_unread = %v, want false when last_read is at the newest message", got)
	}

	// No read cursor at all must NOT read as "everything unread" — that
	// would light up every thread the moment the field shipped.
	none := Thread{LastRead: "", Messages: behind.Messages}
	if got := decode(buildSlackStateJSON(none, "eng", "ana", "root"))["has_unread"]; got != false {
		t.Fatalf("has_unread = %v, want false when there is no read cursor", got)
	}
}

// TestCachedStateCarriesLastRead pins the read CURSOR itself, not just the
// derived boolean beside it. has_unread answers "does this thread have
// anything new?"; last_read answers "which messages are the new ones?", which
// is what a consumer needs to mark individual replies in a timeline that
// interleaves several resources and so cannot draw a divider.
func TestCachedStateCarriesLastRead(t *testing.T) {
	decode := func(js string) map[string]interface{} {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(js), &m); err != nil {
			t.Fatalf("state json: %v", err)
		}
		return m
	}

	th := Thread{LastRead: "1.0", Messages: []Message{
		{UserID: "U1", TS: "1.0", Text: "root"},
		{UserID: "U2", TS: "2.0", Text: "reply"},
	}}
	if got := decode(buildSlackStateJSON(th, "eng", "ana", "root"))["last_read"]; got != "1.0" {
		t.Fatalf("last_read = %v, want the thread's read cursor verbatim", got)
	}

	// Absent cursor stays absent rather than becoming "0" or the root ts:
	// a consumer must be able to tell "read up to here" from "no cursor at
	// all", which is exactly the case has_unread resolves to false.
	none := Thread{LastRead: "", Messages: th.Messages}
	if got := decode(buildSlackStateJSON(none, "eng", "ana", "root"))["last_read"]; got != "" {
		t.Fatalf("last_read = %v, want empty when the thread has no read cursor", got)
	}
}
