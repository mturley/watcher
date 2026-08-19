package slack

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"testing"

	"github.com/mturley/watcher"
	"github.com/mturley/watcher/db"
	"github.com/mturley/watcher/testutil"
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
// the poller test; everything else returns zero values.
type fakeClient struct {
	thread       Thread
	users        map[string]User
	channelName  string
	channelCalls int
}

func (f *fakeClient) AuthTest(ctx context.Context) error         { return nil }
func (f *fakeClient) WhoAmI(ctx context.Context) (string, error) { return "", nil }

func (f *fakeClient) Replies(ctx context.Context, channel, threadTS string) (Thread, error) {
	return f.thread, nil
}

func (f *fakeClient) Users(ctx context.Context, ids []string) (map[string]User, error) {
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
	return f.channelName, nil
}

func (f *fakeClient) Emoji(ctx context.Context) (map[string]string, error)               { return nil, nil }
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

// pollOnce drives the same sequence Poll performs for a single resource, but
// against an injected fake Client (Poll itself always constructs a real
// *HTTPClient via New(), so it can't be used directly with a fake here).
func pollOnce(conn *sql.DB, client Client, resource watcher.Resource, logger *log.Logger) error {
	channel, threadTS, ok := parseResourceID(resource.ID)
	if !ok {
		logger.Printf("ERROR: bad slack resource id %q", resource.ID)
		return nil
	}
	thread, err := client.Replies(context.Background(), channel, threadTS)
	if err != nil {
		return err
	}
	backfill, err := db.BackfillFor(conn, resource.Type, resource.ID)
	if err != nil {
		return err
	}
	names := resolveAuthors(client, thread.Messages)
	if _, err := processThread(conn, thread, names, resource, backfill, logger); err != nil {
		return err
	}
	channelName := resolveChannelName(conn, client, resource, channel)
	rootAuthor := ""
	if len(thread.Messages) > 0 {
		rootAuthor = names[thread.Messages[0].UserID]
	}
	stateJSON := buildSlackStateJSON(thread, channelName, rootAuthor)
	latestTS := latestThreadTS(thread)
	return db.UpsertResourceState(conn, "slack", resource.ID, stateJSON, latestTS, "2026-08-19T00:00:00Z")
}
