package slack

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"testing"
	"time"

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
	groups       map[string]UserGroup
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
	out := make(map[string]UserGroup, len(ids))
	for _, id := range ids {
		if g, ok := f.groups[id]; ok {
			out[id] = g
		}
	}
	return out, nil
}
func (f *fakeClient) SearchUsers(ctx context.Context, query, currentChannel string, limit int) ([]User, error) {
	return nil, nil
}
func (f *fakeClient) SearchUserGroups(ctx context.Context, query string, limit int) ([]UserGroup, error) {
	return nil, nil
}
func (f *fakeClient) SearchChannels(ctx context.Context, query string, limit int) ([]Channel, error) {
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
	if ev.AuthorID == nil || *ev.AuthorID != "U2" {
		t.Errorf("AuthorID = %v, want the replier's user ID U2", ev.AuthorID)
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
	_, resolvedRoot, err := processThread(conn, client, thread, names, resource, backfill, logger)
	if err != nil {
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
	stateJSON := buildSlackStateJSON(thread, channelName, rootAuthor, resolvedRoot)
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

// TestEventTextResolvesMentions pins that event titles and reply bodies are
// stored with mention tokens already resolved. Consumers read them as plain
// strings with no user directory, so a raw "<@U3>" stored here is shown raw.
// The mentioned people never posted in the thread, so their names must come
// from a directory lookup, not the author map.
func TestEventTextResolvesMentions(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	root := Message{TS: "1699000000.000100", UserID: "U1", Text: "hey <@U3> can <!subteam^S1> look"}
	reply := Message{TS: "1699000001.000200", UserID: "U2", Text: "<@U4> agreed"}
	client := &fakeClient{
		thread: Thread{Channel: "C1", ThreadTS: root.TS, Messages: []Message{root}},
		users: map[string]User{
			"U1": {ID: "U1", DisplayName: "Alice"},
			"U2": {ID: "U2", DisplayName: "Bob"},
			"U3": {ID: "U3", DisplayName: "cy"},
			"U4": {ID: "U4", RealName: "Dee"},
		},
		groups:      map[string]UserGroup{"S1": {ID: "S1", Handle: "platform"}},
		channelName: "eng",
	}
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatalf("poll #1: %v", err)
	}
	client.thread.Messages = []Message{root, reply}
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatalf("poll #2: %v", err)
	}

	events, err := db.EventsForResource(conn, "slack", slackResource.ID)
	if err != nil {
		t.Fatalf("EventsForResource: %v", err)
	}
	const wantTitle = "hey @cy can @platform look"
	got := map[watcher.EventType]watcher.Event{}
	for _, e := range events {
		got[e.Type] = e
	}
	// EventsForResource hides bookkeeping types like watch_started.
	var wsTitle string
	if err := conn.QueryRow(`SELECT title FROM watcher_events WHERE type = ?`,
		watcher.EventTypeWatchStarted).Scan(&wsTitle); err != nil {
		t.Fatalf("watch_started: %v", err)
	}
	if wsTitle != "Started watching thread: "+wantTitle {
		t.Errorf("watch_started title = %q", wsTitle)
	}
	r := got[watcher.EventTypeSlackReply]
	if r.Title != "New reply in "+wantTitle {
		t.Errorf("slack_reply title = %q", r.Title)
	}
	if r.Body == nil || *r.Body != "Bob: @Dee agreed" {
		t.Errorf("slack_reply body = %v, want %q", r.Body, "Bob: @Dee agreed")
	}

	st, err := db.GetResourceState(conn, "slack", slackResource.ID)
	if err != nil || st == nil {
		t.Fatalf("GetResourceState: %v, %v", st, err)
	}
	if !strings.Contains(st.StateJSON, `"title":"`+wantTitle+`"`) {
		t.Errorf("cached title not resolved: %s", st.StateJSON)
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

// backfillRow is one recorded event of the resource under test, as the
// backfill tests need to see it.
type backfillRow struct {
	typ, ts, extTS string
	authorID       sql.NullString
}

func backfillRows(t *testing.T, conn *sql.DB) []backfillRow {
	t.Helper()
	rows, err := conn.Query(`
		SELECT e.type, e.ts, COALESCE(e.external_ts, ''), e.author_id
		  FROM watcher_events e
		  JOIN watcher_event_resources er ON er.event_id = e.id
		 WHERE er.resource_type = ? AND er.resource_id = ?
		 ORDER BY e.external_ts, e.type`, "slack", slackResource.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []backfillRow
	for rows.Next() {
		var r backfillRow
		if err := rows.Scan(&r.typ, &r.ts, &r.extTS, &r.authorID); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func countType(rows []backfillRow, typ watcher.EventType) int {
	n := 0
	for _, r := range rows {
		if r.typ == string(typ) {
			n++
		}
	}
	return n
}

// assertRecent fails unless ts is an RFC3339 time within a minute of now:
// the wall-clock stamp, as opposed to a message's send time.
func assertRecent(t *testing.T, what, ts string) {
	t.Helper()
	got, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("%s ts %q: %v", what, ts, err)
	}
	if d := time.Since(got); d < -time.Minute || d > time.Minute {
		t.Errorf("%s ts = %s, want within a minute of now", what, ts)
	}
}

func TestSlackSendTime(t *testing.T) {
	for in, want := range map[string]string{
		"1699000000.000100": "2023-11-03T08:26:40Z",
		"abc":               "",
		"":                  "",
	} {
		if got := slackSendTime(in); got != want {
			t.Errorf("slackSendTime(%q) = %q, want %q", in, got, want)
		}
	}
}

func backfillThread() Thread {
	return Thread{
		Channel:  "C1",
		ThreadTS: "1699000000.000100",
		Messages: []Message{
			{TS: "1699000000.000100", UserID: "U1", Text: "root"},
			{TS: "1699000100.000200", UserID: "U2", Text: "first reply"},
			{TS: "1699000200.000300", UserID: "U1", Text: "second reply"},
		},
	}
}

func TestPoll_BackfillDatesRepliesAtSendTime(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{Backfill: true}); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{thread: backfillThread(), users: map[string]User{
		"U1": {ID: "U1", DisplayName: "Alice"}, "U2": {ID: "U2", DisplayName: "Bob"},
	}}
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatal(err)
	}

	rows := backfillRows(t, conn)
	if got := countType(rows, watcher.EventTypeWatchStarted); got != 1 {
		t.Fatalf("watch_started = %d, want 1: %+v", got, rows)
	}
	want := []struct{ extTS, ts, author string }{
		{"1699000100.000200", "2023-11-03T08:28:20Z", "U2"},
		{"1699000200.000300", "2023-11-03T08:30:00Z", "U1"},
	}
	var replies []backfillRow
	for _, r := range rows {
		switch r.typ {
		case string(watcher.EventTypeSlackReply):
			replies = append(replies, r)
		case string(watcher.EventTypeWatchStarted):
			assertRecent(t, "watch_started", r.ts)
		}
	}
	if len(replies) != len(want) {
		t.Fatalf("slack_reply = %d, want %d: %+v", len(replies), len(want), rows)
	}
	for i, w := range want {
		r := replies[i]
		if r.extTS != w.extTS || r.ts != w.ts || r.authorID.String != w.author {
			t.Errorf("reply %d = %+v, want external_ts %s ts %s author_id %s", i, r, w.extTS, w.ts, w.author)
		}
	}

	// A reply arriving after the backfill is live: stamped with the wall
	// clock, so the notifier (which reads ts near now) still sees it.
	client.thread.Messages = append(client.thread.Messages,
		Message{TS: "1699000300.000400", UserID: "U2", Text: "live reply"})
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatal(err)
	}
	rows = backfillRows(t, conn)
	if got := countType(rows, watcher.EventTypeSlackReply); got != 3 {
		t.Fatalf("slack_reply after live poll = %d, want 3", got)
	}
	last := rows[len(rows)-1]
	if last.extTS != "1699000300.000400" {
		t.Fatalf("last row = %+v, want the live reply", last)
	}
	assertRecent(t, "live reply", last.ts)
}

func TestPoll_BackfillRootOnly(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{Backfill: true}); err != nil {
		t.Fatal(err)
	}
	thread := backfillThread()
	thread.Messages = thread.Messages[:1]
	client := &fakeClient{thread: thread}
	for i := 0; i < 2; i++ {
		if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
			t.Fatal(err)
		}
	}
	rows := backfillRows(t, conn)
	if countType(rows, watcher.EventTypeWatchStarted) != 1 || countType(rows, watcher.EventTypeSlackReply) != 0 {
		t.Fatalf("rows = %+v, want exactly one watch_started", rows)
	}
}

func TestBackfillThread_FillsOnlyMissing(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	full := backfillThread()
	full.Messages = append(full.Messages, Message{TS: "1699000300.000400", UserID: "U2", Text: "third reply"})

	// Followed without backfill after the first two replies existed, then
	// the third arrived and was recorded live.
	client := &fakeClient{thread: Thread{Channel: "C1", ThreadTS: full.ThreadTS, Messages: full.Messages[:3]}}
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatal(err)
	}
	client.thread = full
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatal(err)
	}
	if got := countType(backfillRows(t, conn), watcher.EventTypeSlackReply); got != 1 {
		t.Fatalf("slack_reply before backfill = %d, want 1", got)
	}

	n, err := BackfillThread(conn, client, slackResource, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 { // the two older replies and the root
		t.Errorf("BackfillThread emitted %d, want 3", n)
	}
	if n, err := BackfillThread(conn, client, slackResource, testLogger()); err != nil || n != 0 {
		t.Errorf("second BackfillThread = %d, %v; want 0, nil", n, err)
	}
	rows := backfillRows(t, conn)
	if countType(rows, watcher.EventTypeWatchStarted) != 1 || countType(rows, watcher.EventTypeSlackReply) != 3 {
		t.Fatalf("rows = %+v, want 1 watch_started and 3 slack_reply", rows)
	}
	for _, r := range rows {
		if r.extTS == "1699000100.000200" && r.ts != "2023-11-03T08:28:20Z" {
			t.Errorf("backfilled reply ts = %s, want send time", r.ts)
		}
	}
}

// eventBody returns the body of the resource's only event of type typ.
func eventBody(t *testing.T, conn *sql.DB, typ watcher.EventType, extTS string) string {
	t.Helper()
	var body sql.NullString
	if err := conn.QueryRow(`
		SELECT e.body FROM watcher_events e
		  JOIN watcher_event_resources er ON er.event_id = e.id
		 WHERE er.resource_type = 'slack' AND er.resource_id = ? AND e.type = ? AND e.external_ts = ?`,
		slackResource.ID, string(typ), extTS).Scan(&body); err != nil {
		t.Fatalf("event body %s %s: %v", typ, extTS, err)
	}
	return body.String
}

// longText is a multi-line message well past the 60-rune title cut.
var longText = "first line of a long message about the rollout plan for next week\n\n" +
	strings.Repeat("details ", 20) + "\nlast line with the word zanzibar"

func TestPoll_ReplyBodyIsFullText(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	thread := backfillThread()
	thread.Messages = thread.Messages[:1]
	client := &fakeClient{thread: thread, users: map[string]User{"U2": {ID: "U2", DisplayName: "Bob"}}}
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatal(err)
	}
	client.thread.Messages = append(client.thread.Messages, Message{TS: "1699000400.000500", UserID: "U2", Text: longText})
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatal(err)
	}
	if got := eventBody(t, conn, watcher.EventTypeSlackReply, "1699000400.000500"); got != "Bob: "+longText {
		t.Errorf("body = %q, want the whole message", got)
	}
}

func TestPoll_WatchStartedBodyIsFullRoot(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{thread: Thread{Channel: "C1", ThreadTS: "1699000000.000100",
		Messages: []Message{{TS: "1699000000.000100", UserID: "U1", Text: longText}}}}
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatal(err)
	}
	if got := eventBody(t, conn, watcher.EventTypeWatchStarted, "1699000000.000100"); got != longText {
		t.Errorf("watch_started body = %q, want the whole root", got)
	}
}

func TestBackfillThread_RewritesTruncatedBody(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	thread := backfillThread()
	thread.Messages = append(thread.Messages[:1], Message{TS: "1699000100.000200", UserID: "U2", Text: longText})
	client := &fakeClient{thread: thread, users: map[string]User{"U2": {ID: "U2", DisplayName: "Bob"}}}

	// A thread first polled (watch_started) with a reply recorded before
	// bodies held the whole text.
	rootTS := "1699000100.000200"
	rootBody := "root"
	if err := db.InsertEvent(conn, watcher.Event{ID: "ws", TS: "2023-11-03T08:28:20Z", ExternalTS: &rootTS,
		Source: "slack", Type: watcher.EventTypeWatchStarted, Title: "Started watching thread: root", Body: &rootBody}, slackResource); err != nil {
		t.Fatal(err)
	}
	ts := "1699000100.000200"
	short := "Bob: " + fallbackTitle(longText)
	if err := db.InsertEvent(conn, watcher.Event{ID: "old", TS: "2023-11-03T08:28:20Z", ExternalTS: &ts,
		Source: "slack", Type: watcher.EventTypeSlackReply, Title: "New reply in root", Body: &short}, slackResource); err != nil {
		t.Fatal(err)
	}

	n, err := BackfillThread(conn, client, slackResource, testLogger())
	if err != nil || n != 2 { // the rewrite, and the missing root event
		t.Fatalf("BackfillThread = %d, %v; want 2, nil", n, err)
	}
	if got := eventBody(t, conn, watcher.EventTypeSlackReply, ts); got != "Bob: "+longText {
		t.Errorf("body = %q, want the whole message", got)
	}
	if n, err := BackfillThread(conn, client, slackResource, testLogger()); err != nil || n != 0 {
		t.Errorf("second BackfillThread = %d, %v; want 0, nil", n, err)
	}
}

// seedReply records a reply event as an earlier poll would have.
func seedReply(t *testing.T, conn *sql.DB, id, ts, body string) {
	t.Helper()
	if err := db.InsertEvent(conn, watcher.Event{ID: id, TS: "2023-11-03T08:28:20Z", ExternalTS: &ts,
		Source: "slack", Type: watcher.EventTypeSlackReply, Title: "New reply in root", Body: &body}, slackResource); err != nil {
		t.Fatal(err)
	}
}

// A rate-limited or failed user lookup must never turn a good body into one
// naming raw user ids: nothing would ever repair it.
func TestBackfillThread_KeepsBodyWhenNamesUnresolved(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	thread := backfillThread()
	thread.Messages = append(thread.Messages[:1],
		Message{TS: "1699000100.000200", UserID: "U2", Text: "edited text"},
		Message{TS: "1699000200.000300", UserID: "U1", Text: "hey <@U3> edited"})
	// U2 (author) and U3 (mention) fail to resolve, as HTTPClient.Users
	// reports a failed users.info: the id as the name.
	client := &fakeClient{thread: thread, users: map[string]User{
		"U1": {ID: "U1", DisplayName: "Alice"},
		"U2": {ID: "U2", DisplayName: "U2"},
		"U3": {ID: "U3", DisplayName: "U3"},
	}}
	seedReply(t, conn, "r1", "1699000100.000200", "Bob: original text")
	seedReply(t, conn, "r2", "1699000200.000300", "Alice: hey @Carol original")

	n, err := BackfillThread(conn, client, slackResource, testLogger())
	if err != nil || n != 0 {
		t.Fatalf("BackfillThread = %d, %v; want 0, nil", n, err)
	}
	if got := eventBody(t, conn, watcher.EventTypeSlackReply, "1699000100.000200"); got != "Bob: original text" {
		t.Errorf("unresolved author rewrote body to %q", got)
	}
	if got := eventBody(t, conn, watcher.EventTypeSlackReply, "1699000200.000300"); got != "Alice: hey @Carol original" {
		t.Errorf("unresolved mention rewrote body to %q", got)
	}
}

func TestBackfillThread_RewritesWatchStartedBody(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	root := "1699000000.000100"
	short := fallbackTitle(longText)
	if err := db.InsertEvent(conn, watcher.Event{ID: "ws", TS: "2023-11-03T08:28:20Z", ExternalTS: &root,
		Source: "slack", Type: watcher.EventTypeWatchStarted, Title: "Started watching thread: x", Body: &short}, slackResource); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{thread: Thread{Channel: "C1", ThreadTS: root,
		Messages: []Message{{TS: root, UserID: "U1", Text: longText}}}}
	n, err := BackfillThread(conn, client, slackResource, testLogger())
	if err != nil || n != 2 { // the rewrite, and the missing root event
		t.Fatalf("BackfillThread = %d, %v; want 2, nil", n, err)
	}
	if got := eventBody(t, conn, watcher.EventTypeWatchStarted, root); got != longText {
		t.Errorf("watch_started body = %q, want the whole root", got)
	}
}

// A thread followed but never polled has no events: backfilling it here
// would set the cursor and the first poll would never record watch_started.
// The poller backfills it itself.
func TestBackfillThread_SkipsNeverPolledThread(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{Backfill: true}); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{thread: backfillThread()}
	n, err := BackfillThread(conn, client, slackResource, testLogger())
	if err != nil || n != 0 {
		t.Fatalf("BackfillThread = %d, %v; want 0, nil", n, err)
	}
	if rows := backfillRows(t, conn); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none", rows)
	}
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatal(err)
	}
	if got := countType(backfillRows(t, conn), watcher.EventTypeWatchStarted); got != 1 {
		t.Errorf("watch_started after first poll = %d, want 1", got)
	}
}

// A first poll that died after recording some replies (and before
// watch_started) must not strand the rest: the next poll finishes it.
func TestPoll_BackfillRetriesAfterPartialFirstPoll(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{Backfill: true}); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{thread: backfillThread()}
	seedReply(t, conn, "partial", "1699000100.000200", "first reply")

	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatal(err)
	}
	rows := backfillRows(t, conn)
	if got := countType(rows, watcher.EventTypeWatchStarted); got != 1 {
		t.Fatalf("watch_started = %d, want 1: %+v", got, rows)
	}
	if got := countType(rows, watcher.EventTypeSlackReply); got != 2 {
		t.Fatalf("slack_reply = %d, want 2: %+v", got, rows)
	}
	for _, r := range rows {
		if r.extTS == "1699000200.000300" && r.typ == string(watcher.EventTypeSlackReply) && r.ts != "2023-11-03T08:30:00Z" {
			t.Errorf("remaining reply ts = %s, want its send time", r.ts)
		}
	}
}

func TestPoll_MalformedReplyTSStampedNow(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	thread := backfillThread()
	thread.Messages = thread.Messages[:1]
	client := &fakeClient{thread: thread}
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatal(err)
	}
	// "abc" sorts after any digit ts, so the live path sees it as new.
	client.thread.Messages = append(client.thread.Messages, Message{TS: "abc", UserID: "U2", Text: "odd"})
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatal(err)
	}
	for _, r := range backfillRows(t, conn) {
		if r.extTS == "abc" {
			assertRecent(t, "malformed-ts reply", r.ts)
			return
		}
	}
	t.Fatal("no event recorded for the malformed-ts reply")
}

func rootRow(t *testing.T, conn *sql.DB) []backfillRow {
	t.Helper()
	var out []backfillRow
	for _, r := range backfillRows(t, conn) {
		if r.typ == string(watcher.EventTypeSlackThreadRoot) {
			out = append(out, r)
		}
	}
	return out
}

func TestPoll_BackfillRecordsThreadRoot(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{Backfill: true}); err != nil {
		t.Fatal(err)
	}
	thread := backfillThread()
	thread.Messages[0].Text = longText
	client := &fakeClient{thread: thread, users: map[string]User{
		"U1": {ID: "U1", DisplayName: "Alice"}, "U2": {ID: "U2", DisplayName: "Bob"},
	}}
	for i := 0; i < 2; i++ {
		if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
			t.Fatal(err)
		}
	}
	roots := rootRow(t, conn)
	if len(roots) != 1 {
		t.Fatalf("slack_thread_root rows = %+v, want exactly 1", roots)
	}
	r := roots[0]
	if r.extTS != "1699000000.000100" || r.ts != "2023-11-03T08:26:40Z" || r.authorID.String != "U1" {
		t.Errorf("root = %+v, want external_ts/ts of the root's send time and author_id U1", r)
	}
	if got := eventBody(t, conn, watcher.EventTypeSlackThreadRoot, "1699000000.000100"); got != "Alice: "+longText {
		t.Errorf("root body = %q, want the author and the whole root", got)
	}
	var title string
	if err := conn.QueryRow(`SELECT title FROM watcher_events WHERE type = ?`, string(watcher.EventTypeSlackThreadRoot)).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "Thread started: "+fallbackTitle(longText) {
		t.Errorf("root title = %q", title)
	}
}

func TestPoll_NoThreadRootWithoutBackfill(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := pollOnce(conn, &fakeClient{thread: backfillThread()}, slackResource, testLogger()); err != nil {
		t.Fatal(err)
	}
	if roots := rootRow(t, conn); len(roots) != 0 {
		t.Fatalf("slack_thread_root rows = %+v, want none", roots)
	}
}

func TestBackfillThread_AddsMissingRootOnce(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	thread := backfillThread()
	thread.Messages = thread.Messages[:1]
	client := &fakeClient{thread: thread, users: map[string]User{"U1": {ID: "U1", DisplayName: "Alice"}}}
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatal(err)
	}
	n, err := BackfillThread(conn, client, slackResource, testLogger())
	if err != nil || n != 1 {
		t.Fatalf("BackfillThread = %d, %v; want 1 (the root), nil", n, err)
	}
	if roots := rootRow(t, conn); len(roots) != 1 || roots[0].ts != "2023-11-03T08:26:40Z" {
		t.Fatalf("roots = %+v, want one at the root's send time", roots)
	}
	if n, err := BackfillThread(conn, client, slackResource, testLogger()); err != nil || n != 0 {
		t.Errorf("second BackfillThread = %d, %v; want 0, nil", n, err)
	}
}

func TestBackfillThread_KeepsRootBodyWhenAuthorUnresolved(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{Backfill: true}); err != nil {
		t.Fatal(err)
	}
	thread := backfillThread()
	thread.Messages = thread.Messages[:1]
	client := &fakeClient{thread: thread, users: map[string]User{"U1": {ID: "U1", DisplayName: "Alice"}}}
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatal(err)
	}
	client.thread.Messages[0].Text = "edited root"
	client.users = map[string]User{"U1": {ID: "U1", DisplayName: "U1"}} // failed lookup
	// watch_started (no author prefix) may take the edit; the root event,
	// whose body would name the author by a raw id, must not.
	if _, err := BackfillThread(conn, client, slackResource, testLogger()); err != nil {
		t.Fatal(err)
	}
	if got := eventBody(t, conn, watcher.EventTypeSlackThreadRoot, "1699000000.000100"); got != "Alice: root" {
		t.Errorf("root body = %q, want it unchanged", got)
	}
}

// The first poll writes watch_started last, after the root and the replies,
// so a poll that fails part-way is still a first poll next time.
func TestPoll_BackfillWritesRootFirstWatchStartedLast(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{Backfill: true}); err != nil {
		t.Fatal(err)
	}
	if err := pollOnce(conn, &fakeClient{thread: backfillThread()}, slackResource, testLogger()); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(`SELECT type FROM watcher_events ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var order []string
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatal(err)
		}
		order = append(order, typ)
	}
	want := []string{"slack_thread_root", "slack_reply", "slack_reply", "watch_started"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("insert order = %v, want %v", order, want)
	}
}

func TestBackfillThread_RewritesShortRootBody(t *testing.T) {
	conn := testutil.NewTestDB(t)
	if err := db.Subscribe(conn, "test-sub", slackResource, db.SubscribeOpts{Backfill: true}); err != nil {
		t.Fatal(err)
	}
	thread := backfillThread()
	thread.Messages = thread.Messages[:1]
	client := &fakeClient{thread: thread, users: map[string]User{"U1": {ID: "U1", DisplayName: "Alice"}}}
	if err := pollOnce(conn, client, slackResource, testLogger()); err != nil {
		t.Fatal(err)
	}
	client.thread.Messages[0].Text = longText
	if _, err := BackfillThread(conn, client, slackResource, testLogger()); err != nil {
		t.Fatal(err)
	}
	if got := eventBody(t, conn, watcher.EventTypeSlackThreadRoot, "1699000000.000100"); got != "Alice: "+longText {
		t.Errorf("root body = %q, want the whole root", got)
	}
}
