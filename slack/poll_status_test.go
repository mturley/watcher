package slack

import (
	"errors"
	"testing"

	"github.com/mturley/watcher"
	"github.com/mturley/watcher/db"
	"github.com/mturley/watcher/testutil"
)

func TestPollWith_RecordsPerThreadOutcomes(t *testing.T) {
	conn := testutil.NewTestDB(t)
	ok := slackResource
	if err := db.Subscribe(conn, "test-sub", ok, db.SubscribeOpts{}); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{
		thread: Thread{Channel: "C1", ThreadTS: "1699000000.000100",
			Messages: []Message{{TS: "1699000000.000100", UserID: "U1", Text: "hi"}}},
		users: map[string]User{"U1": {ID: "U1", DisplayName: "Alice"}},
	}
	if err := pollWith(conn, client, []watcher.Resource{ok}, testLogger()); err != nil {
		t.Fatal(err)
	}
	st, _ := db.GetResourcePollStatuses(conn, []watcher.Resource{ok})
	if s := st[db.ResourceKey{Type: ok.Type, ID: ok.ID}]; s.Failing() || s.Detail == nil || !s.Detail.OK {
		t.Fatalf("ok thread: %+v", s)
	}

	gone := watcher.Resource{Type: "slack", ID: "C2:1699000000.000200"}
	failing := &fakeClient{repliesErr: errors.New("slack error: thread_not_found")}
	_ = pollWith(conn, failing, []watcher.Resource{gone}, testLogger())
	st, _ = db.GetResourcePollStatuses(conn, []watcher.Resource{gone})
	s := st[db.ResourceKey{Type: gone.Type, ID: gone.ID}]
	if !s.Failing() || s.LastErrorMessage != "Failed to fetch thread: slack error: thread_not_found" {
		t.Fatalf("gone thread: %+v", s)
	}
	rows, _, _ := db.QueryPollLog(conn, db.PollLogQuery{ResourceScoped: true, Resources: []watcher.Resource{gone}})
	if len(rows) == 0 || rows[0].Kind != "poll" {
		t.Fatalf("expected finish poll row first, got %+v", rows)
	}
}

func TestPollWith_EmptyThreadRecordedAsError(t *testing.T) {
	conn := testutil.NewTestDB(t)
	empty := &fakeClient{thread: Thread{Channel: "C1", ThreadTS: "1699000000.000100"}}
	_ = pollWith(conn, empty, []watcher.Resource{slackResource}, testLogger())
	st, _ := db.GetResourcePollStatuses(conn, []watcher.Resource{slackResource})
	if s := st[db.ResourceKey{Type: "slack", ID: slackResource.ID}]; !s.Failing() {
		t.Fatalf("empty thread not recorded as error: %+v", s)
	}
}
