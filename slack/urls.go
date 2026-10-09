package slack

import "regexp"

// mrkdwnLinkPattern matches <url> and <url|label> in mrkdwn text. Mentions
// (<@U…>, <#C…>, <!here>) never start with http so they don't match.
var mrkdwnLinkPattern = regexp.MustCompile(`<(https?://[^|>\s]+)(?:\|[^>]*)?>`)

// ExtractURLs returns the URLs a message links to, deduplicated, in this
// order: rich_text link elements, mrkdwn links in Text (covers block-less
// messages), then attachment from_url/title_link and links inside attachment
// rich_text. Mentions, emoji and files are not URLs and are skipped.
func ExtractURLs(m Message) []string {
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	walkElems := func(els []Element) {
		for _, e := range els {
			if e.Type == "link" {
				add(e.URL)
			}
		}
	}
	walkBlocks := func(bs []Block) {
		for _, b := range bs {
			walkElems(b.Elements)
			for _, item := range b.Items {
				walkElems(item)
			}
		}
	}
	walkBlocks(m.Blocks)
	for _, mm := range mrkdwnLinkPattern.FindAllStringSubmatch(m.Text, -1) {
		add(mm[1])
	}
	for _, a := range m.Attachments {
		add(a.FromURL)
		add(a.TitleLink)
		for _, bk := range a.Blocks {
			walkBlocks(bk.RichText)
		}
	}
	return out
}
