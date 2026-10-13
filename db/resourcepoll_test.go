package db

import (
	"testing"

	"github.com/mturley/watcher"
)

func TestRecordResourcePoll_ErrorThenSuccess(t *testing.T) {
	c := migrated(t)
	r := watcher.Resource{Type: "pr", ID: "o/r#1"}
	if err := RecordResourcePoll(c, r, false, "GitHub API error: Unicorn! · GitHub",
		ResourcePollDetail{HTTPStatus: 504, DurationMS: 12300, Note: "GitHub API error"}); err != nil {
		t.Fatal(err)
	}
	m, err := GetResourcePollStatuses(c, []watcher.Resource{r, {Type: "jira", ID: "NEVER-1"}})
	if err != nil {
		t.Fatal(err)
	}
	st, ok := m[ResourceKey{"pr", "o/r#1"}]
	if !ok || !st.Failing() || st.LastErrorMessage != "GitHub API error: Unicorn! · GitHub" || st.Detail == nil || st.Detail.HTTPStatus != 504 || st.Detail.OK {
		t.Fatalf("after error: %+v (detail %+v)", st, st.Detail)
	}
	if _, present := m[ResourceKey{"jira", "NEVER-1"}]; present {
		t.Fatal("never-polled resource should be absent")
	}
	if err := RecordResourcePoll(c, r, true, "", ResourcePollDetail{OK: true, EventsEmitted: 2}); err != nil {
		t.Fatal(err)
	}
	m, _ = GetResourcePollStatuses(c, []watcher.Resource{r})
	st = m[ResourceKey{"pr", "o/r#1"}]
	if st.Failing() || st.LastSuccess == "" || st.LastErrorMessage == "" || st.Detail.EventsEmitted != 2 || !st.Detail.OK {
		t.Fatalf("after success: %+v (detail %+v) — last error kept for history, detail replaced", st, st.Detail)
	}
	if st.LastAttempt != st.LastSuccess {
		t.Fatalf("last_attempt %q != last_success %q", st.LastAttempt, st.LastSuccess)
	}
}
