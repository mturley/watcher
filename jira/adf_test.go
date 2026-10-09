package jira

import (
	"encoding/json"
	"reflect"
	"testing"
)

func decodeADF(t *testing.T, s string) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestExtractADFURLs(t *testing.T) {
	doc := decodeADF(t, `{"type":"doc","version":1,"content":[
		{"type":"paragraph","content":[
			{"type":"inlineCard","attrs":{"url":"https://github.com/example/repo/pull/1"}},
			{"type":"text","text":" see "},
			{"type":"text","text":"the guide","marks":[{"type":"link","attrs":{"href":"https://docs.example.com/guide"}}]},
			{"type":"text","text":" and https://example.atlassian.net/browse/PROJ-2."}
		]},
		{"type":"blockCard","attrs":{"url":"https://example.atlassian.net/browse/PROJ-3"}},
		{"type":"paragraph","content":[
			{"type":"inlineCard","attrs":{"url":"https://github.com/example/repo/pull/1"}}
		]}
	]}`)
	got := ExtractADFURLs(doc)
	want := []string{
		"https://github.com/example/repo/pull/1",
		"https://docs.example.com/guide",
		"https://example.atlassian.net/browse/PROJ-2",
		"https://example.atlassian.net/browse/PROJ-3",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestExtractADFURLsPlainStringAndNil(t *testing.T) {
	if got := ExtractADFURLs(nil); len(got) != 0 {
		t.Fatalf("nil: got %v", got)
	}
	// Jira Server style: a plain text field value.
	got := ExtractADFURLs("https://github.com/example/repo/pull/7, https://github.com/example/repo/pull/8")
	want := []string{"https://github.com/example/repo/pull/7", "https://github.com/example/repo/pull/8"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
