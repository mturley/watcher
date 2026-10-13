package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/mturley/watcher"
)

func migrated(t *testing.T) *sql.DB {
	t.Helper()
	c := mem(t)
	if err := Migrate(c); err != nil {
		t.Fatal(err)
	}
	return c
}

var (
	prA = watcher.Resource{Type: "pr", ID: "o/r#1"}
	prB = watcher.Resource{Type: "pr", ID: "o/r#2"}
)

func ins(t *testing.T, c *sql.DB, e PollLogEntry) int64 {
	t.Helper()
	id, err := InsertPollLog(c, e)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ids(es []PollLogEntry) []int64 {
	out := make([]int64, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}

func TestInsertPollLog_RoundTripWithResources(t *testing.T) {
	c := migrated(t)
	id := ins(t, c, PollLogEntry{Source: "github", Level: "error", Kind: "http",
		Message: "POST api.github.com/graphql → 504", Method: "POST",
		Endpoint: "api.github.com/graphql", HTTPStatus: 504, DurationMS: 12300,
		Bytes: 1234, ErrorBody: "Unicorn! · GitHub", Resources: []watcher.Resource{prA, prB}})
	got, _, err := QueryPollLog(c, PollLogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("got %+v", got)
	}
	e := got[0]
	if e.HTTPStatus != 504 || e.ErrorBody != "Unicorn! · GitHub" || e.Bytes != 1234 || len(e.Resources) != 2 {
		t.Fatalf("bad round trip: %+v", e)
	}
	if _, err := time.Parse(PollLogTSFormat, e.TS); err != nil {
		t.Fatalf("TS %q not in PollLogTSFormat: %v", e.TS, err)
	}
}

func TestQueryPollLog_NewestFirstCursorsAndHasMore(t *testing.T) {
	c := migrated(t)
	var all []int64
	for i := 0; i < 5; i++ {
		all = append(all, ins(t, c, PollLogEntry{Source: "jira", Level: "info", Kind: "http", Message: "x"}))
	}
	got, more, _ := QueryPollLog(c, PollLogQuery{Limit: 2})
	if want := []int64{all[4], all[3]}; !equalInt64(ids(got), want) || !more {
		t.Fatalf("page1 = %v more=%v, want %v true", ids(got), more, want)
	}
	got, more, _ = QueryPollLog(c, PollLogQuery{Limit: 10, BeforeID: all[3]})
	if want := []int64{all[2], all[1], all[0]}; !equalInt64(ids(got), want) || more {
		t.Fatalf("older = %v more=%v", ids(got), more)
	}
	got, _, _ = QueryPollLog(c, PollLogQuery{AfterID: all[2]})
	if want := []int64{all[4], all[3]}; !equalInt64(ids(got), want) {
		t.Fatalf("newer = %v", ids(got))
	}
}

func TestQueryPollLog_AfterIDPagesOldestFirstWithoutGaps(t *testing.T) {
	c := migrated(t)
	var all []int64
	for i := 0; i < 5; i++ {
		all = append(all, ins(t, c, PollLogEntry{Source: "jira", Level: "info", Kind: "http", Message: "x"}))
	}
	got, more, err := QueryPollLog(c, PollLogQuery{AfterID: all[0], Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{all[2], all[1]}; !equalInt64(ids(got), want) || !more {
		t.Fatalf("page1 = %v more=%v, want %v true", ids(got), more, want)
	}
	got, more, err = QueryPollLog(c, PollLogQuery{AfterID: all[2], Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{all[4], all[3]}; !equalInt64(ids(got), want) || more {
		t.Fatalf("page2 = %v more=%v, want %v false", ids(got), more, want)
	}
}

func TestQueryPollLog_SourceAndLevelFilters(t *testing.T) {
	c := migrated(t)
	ins(t, c, PollLogEntry{Source: "github", Level: "info", Kind: "poll", Message: "a"})
	w := ins(t, c, PollLogEntry{Source: "github", Level: "warn", Kind: "poll", Message: "b"})
	e := ins(t, c, PollLogEntry{Source: "jira", Level: "error", Kind: "http", Message: "c"})
	got, _, _ := QueryPollLog(c, PollLogQuery{MinLevel: "warn"})
	if want := []int64{e, w}; !equalInt64(ids(got), want) {
		t.Fatalf("warn+ = %v", ids(got))
	}
	got, _, _ = QueryPollLog(c, PollLogQuery{MinLevel: "warn", Sources: []string{"github"}})
	if want := []int64{w}; !equalInt64(ids(got), want) {
		t.Fatalf("github warn+ = %v", ids(got))
	}
}

func TestQueryPollLog_ResourceScopeIncludesSameSourcePollRows(t *testing.T) {
	c := migrated(t)
	ghStart := ins(t, c, PollLogEntry{Source: "github", Level: "info", Kind: "poll", Message: "github poll started"})
	_ = ins(t, c, PollLogEntry{Source: "jira", Level: "info", Kind: "poll", Message: "jira poll started"})
	mine := ins(t, c, PollLogEntry{Source: "github", Level: "info", Kind: "http", Message: "batch", Resources: []watcher.Resource{prA, prB}})
	_ = ins(t, c, PollLogEntry{Source: "github", Level: "error", Kind: "resource", Message: "B failed", Resources: []watcher.Resource{prB}})
	got, _, err := QueryPollLog(c, PollLogQuery{ResourceScoped: true, Resources: []watcher.Resource{prA}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{mine, ghStart}; !equalInt64(ids(got), want) {
		t.Fatalf("scoped = %v, want %v (no jira poll row, no prB-only row)", ids(got), want)
	}
	got, _, _ = QueryPollLog(c, PollLogQuery{ResourceScoped: true})
	if len(got) != 0 {
		t.Fatalf("empty scope should return nothing, got %v", ids(got))
	}
}

func TestPrunePollLog_ByAgeAndCount(t *testing.T) {
	c := migrated(t)
	old := time.Now().UTC().Add(-8 * 24 * time.Hour).Format(PollLogTSFormat)
	oldID := ins(t, c, PollLogEntry{TS: old, Source: "github", Level: "info", Kind: "http", Message: "old", Resources: []watcher.Resource{prA}})
	var kept []int64
	for i := 0; i < 4; i++ {
		kept = append(kept, ins(t, c, PollLogEntry{Source: "github", Level: "info", Kind: "http", Message: "new", Resources: []watcher.Resource{prA}}))
	}
	if err := PrunePollLog(c, 7*24*time.Hour, 3); err != nil {
		t.Fatal(err)
	}
	got, _, _ := QueryPollLog(c, PollLogQuery{})
	if want := []int64{kept[3], kept[2], kept[1]}; !equalInt64(ids(got), want) {
		t.Fatalf("after prune = %v, want %v", ids(got), want)
	}
	var orphans int
	if err := c.QueryRow(`SELECT COUNT(*) FROM watcher_poll_log_resources WHERE log_id NOT IN (SELECT id FROM watcher_poll_log)`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Fatalf("%d orphan join rows", orphans)
	}
	next := ins(t, c, PollLogEntry{Source: "github", Level: "info", Kind: "http", Message: "after"})
	if next <= kept[3] || next == oldID {
		t.Fatalf("id reused: %d", next)
	}
}

func equalInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
