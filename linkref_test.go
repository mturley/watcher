package watcher

import (
	"reflect"
	"testing"
)

func TestCollapseSpace(t *testing.T) {
	if got := CollapseSpace("  a \n\t b  "); got != "a b" {
		t.Fatalf("got %q", got)
	}
}

func TestDedupeLinkURLs(t *testing.T) {
	got := DedupeLinkURLs([]LinkRef{{URL: "a"}, {URL: "b"}, {URL: "a"}, {URL: ""}})
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("got %v", got)
	}
}
