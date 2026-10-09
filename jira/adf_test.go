package jira

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/mturley/watcher"
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

func adfPara(content ...interface{}) map[string]interface{} {
	return map[string]interface{}{"type": "paragraph", "content": content}
}

func adfText(s string, href string) map[string]interface{} {
	n := map[string]interface{}{"type": "text", "text": s}
	if href != "" {
		n["marks"] = []interface{}{map[string]interface{}{"type": "link", "attrs": map[string]interface{}{"href": href}}}
	}
	return n
}

func TestExtractADFLinks_LabelAndContext(t *testing.T) {
	doc := map[string]interface{}{"type": "doc", "content": []interface{}{
		adfPara(adfText("See  the ", ""), adfText("design doc", "https://example.com/d"), adfText(" for\ndetails.", "")),
		adfPara(adfText("Bare https://example.com/b. then more", "")),
	}}
	got := ExtractADFLinks(doc)
	want := []watcher.LinkRef{
		{URL: "https://example.com/d", Label: "design doc", Before: "See the", After: "for details."},
		{URL: "https://example.com/b", Label: "https://example.com/b", Before: "Bare", After: ". then more"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestExtractADFLinks_MergesSameHrefAndKeepsDuplicates(t *testing.T) {
	bold := adfText("bold", "https://example.com/m")
	bold["marks"] = append(bold["marks"].([]interface{}), map[string]interface{}{"type": "strong"})
	doc := adfPara(adfText("a ", ""), bold, adfText(" plain", "https://example.com/m"), adfText(" b ", ""),
		adfText("again", "https://example.com/m"))
	got := ExtractADFLinks(doc)
	if len(got) != 2 {
		t.Fatalf("want 2 occurrences, got %#v", got)
	}
	if got[0].Label != "bold plain" || got[0].Before != "a" || got[0].After != "b again" {
		t.Errorf("merged occurrence wrong: %#v", got[0])
	}
	if got[1].Label != "again" || got[1].Before != "a bold plain b" {
		t.Errorf("second occurrence wrong: %#v", got[1])
	}
}

func TestExtractADFLinks_CardsMentionsAndBlocks(t *testing.T) {
	doc := map[string]interface{}{"type": "doc", "content": []interface{}{
		adfPara(adfText("PR: ", ""),
			map[string]interface{}{"type": "inlineCard", "attrs": map[string]interface{}{"url": "https://example.com/pr/1"}},
			map[string]interface{}{"type": "hardBreak"},
			map[string]interface{}{"type": "mention", "attrs": map[string]interface{}{"text": "@alex"}},
			map[string]interface{}{"type": "emoji", "attrs": map[string]interface{}{"shortName": ":tada:"}}),
		map[string]interface{}{"type": "blockCard", "attrs": map[string]interface{}{"url": "https://example.com/bc"}},
		map[string]interface{}{"type": "bulletList", "content": []interface{}{
			map[string]interface{}{"type": "listItem", "content": []interface{}{
				adfPara(adfText("item ", ""), adfText("x", "https://example.com/li")),
			}},
		}},
	}}
	got := ExtractADFLinks(doc)
	want := []watcher.LinkRef{
		{URL: "https://example.com/pr/1", Label: "https://example.com/pr/1", Before: "PR:", After: "@alex:tada:"},
		{URL: "https://example.com/bc", Label: "https://example.com/bc"},
		{URL: "https://example.com/li", Label: "x", Before: "item"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestExtractADFLinks_PlainString(t *testing.T) {
	got := ExtractADFLinks("fix in https://example.com/p, ok")
	want := []watcher.LinkRef{{URL: "https://example.com/p", Label: "https://example.com/p", Before: "fix in", After: ", ok"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	if ExtractADFLinks(42) != nil {
		t.Error("non-doc input must yield nil")
	}
}
