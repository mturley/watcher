package slack

import (
	"reflect"
	"testing"

	"github.com/mturley/watcher"
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

func TestExtractLinks_RichText(t *testing.T) {
	m := Message{
		Text: "ignored <https://ignored.example.com>",
		Blocks: []Block{
			{Type: "section", Elements: []Element{
				{Type: "text", Text: "ping "}, {Type: "user", UserID: "U0TEST"},
				{Type: "text", Text: " about "},
				{Type: "link", URL: "https://example.com/a", Text: "the doc"},
				{Type: "text", Text: " "}, {Type: "emoji", Name: "tada"},
				{Type: "text", Text: " "}, {Type: "broadcast", Range: "here"},
			}},
			{Type: "list", Items: [][]Element{
				{{Type: "text", Text: "first "}, {Type: "link", URL: "https://example.com/b"}},
				{{Type: "link", URL: "https://example.com/c", Text: "C"}, {Type: "text", Text: " second"}},
			}},
		},
		Attachments: []Attachment{
			{Title: "Page title", TitleLink: "https://example.com/t", FromURL: "https://example.com/t"},
			{FromURL: "https://example.com/u"},
		},
	}
	got := ExtractLinks(m)
	want := []watcher.LinkRef{
		{URL: "https://example.com/a", Label: "the doc", Before: "ping @user about", After: ":tada: @here"},
		{URL: "https://example.com/b", Label: "https://example.com/b", Before: "first"},
		{URL: "https://example.com/c", Label: "C", After: "second"},
		{URL: "https://example.com/t", Label: "Page title"},
		{URL: "https://example.com/t", Label: "Page title"},
		{URL: "https://example.com/u", Label: "https://example.com/u"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestExtractLinks_MrkdwnWhenNoBlocks(t *testing.T) {
	m := Message{Text: "hey <@U0TEST> see <https://example.com/a?x=1&amp;y=2|the &lt;doc&gt;> in <#C0TEST|general>, <!here>\n" +
		"and <https://example.com/b> too"}
	got := ExtractLinks(m)
	want := []watcher.LinkRef{
		{URL: "https://example.com/a?x=1&y=2", Label: "the <doc>", Before: "hey @U0TEST see", After: "in #general, @here"},
		{URL: "https://example.com/b", Label: "https://example.com/b", Before: "and", After: "too"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}
