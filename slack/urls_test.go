package slack

import (
	"reflect"
	"testing"
)

func TestExtractURLs(t *testing.T) {
	m := Message{
		Text: "see <https://github.com/example/repo/pull/1|the PR> and <@U0TEST> in <#C0TEST|general> <!here> " +
			"plus <https://example.atlassian.net/browse/PROJ-1>",
		Blocks: []Block{
			{Type: "section", Elements: []Element{
				{Type: "text", Text: "see "},
				{Type: "link", URL: "https://github.com/example/repo/pull/1", Text: "the PR"},
			}},
			{Type: "list", Items: [][]Element{{{Type: "link", URL: "https://docs.example.com/a"}}}},
		},
		Attachments: []Attachment{
			{FromURL: "https://example.slack.com/archives/C0TEST/p1700000000000100", TitleLink: "https://example.slack.com/archives/C0TEST/p1700000000000100"},
			{Blocks: []BlockKit{{Type: "rich_text", RichText: []Block{{Type: "section", Elements: []Element{{Type: "link", URL: "https://docs.example.com/b"}}}}}}},
		},
	}
	got := ExtractURLs(m)
	want := []string{
		"https://github.com/example/repo/pull/1",
		"https://docs.example.com/a",
		"https://example.atlassian.net/browse/PROJ-1",
		"https://example.slack.com/archives/C0TEST/p1700000000000100",
		"https://docs.example.com/b",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestExtractURLsUnescapesMrkdwnEntities(t *testing.T) {
	m := Message{
		Text: "see <https://example.com/a?x=1&amp;y=2|label>",
		Blocks: []Block{{Type: "section", Elements: []Element{
			{Type: "link", URL: "https://example.com/a?x=1&y=2", Text: "label"},
		}}},
	}
	got := ExtractURLs(m)
	want := []string{"https://example.com/a?x=1&y=2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	// Block-less message: the mrkdwn URL alone must come out unescaped.
	got = ExtractURLs(Message{Text: "<https://example.com/a?x=1&amp;y=2>"})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("blockless: got %v\nwant %v", got, want)
	}
}
