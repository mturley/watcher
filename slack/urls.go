package slack

import (
	"regexp"
	"strings"

	"github.com/mturley/watcher"
)

// ExtractURLs returns the URLs a message links to, deduplicated, in this
// order: rich_text link elements, mrkdwn links in Text (covers block-less
// messages), then attachment from_url/title_link and links inside attachment
// rich_text. Mentions, emoji and files are not URLs and are skipped.
func ExtractURLs(m Message) []string {
	return watcher.DedupeLinkURLs(extractLinks(m, true))
}

// ExtractLinks returns every link occurrence in a message, in order and NOT
// deduplicated, each with its label and the text around it within the same
// block: rich_text links (per section/quote/preformatted block, each list
// item separately), the mrkdwn Text when the message has no blocks, then
// attachment from_url/title_link (labelled with the attachment title) and
// links inside attachment rich_text.
func ExtractLinks(m Message) []watcher.LinkRef {
	return extractLinks(m, false)
}

func extractLinks(m Message, alwaysScanText bool) []watcher.LinkRef {
	var out []watcher.LinkRef
	out = append(out, blockLinks(m.Blocks)...)
	if len(m.Blocks) == 0 || alwaysScanText {
		out = append(out, mrkdwnLinks(m.Text)...)
	}
	for _, a := range m.Attachments {
		for _, u := range []string{a.FromURL, a.TitleLink} {
			if u == "" {
				continue
			}
			label := a.Title
			if label == "" {
				label = u
			}
			out = append(out, watcher.LinkRef{URL: u, Label: label})
		}
		for _, bk := range a.Blocks {
			out = append(out, blockLinks(bk.RichText)...)
		}
	}
	return out
}

// linkLine accumulates one block's text and the links within it.
type linkLine struct {
	text  strings.Builder
	links []pendingLink
}

type pendingLink struct {
	url, label string
	start, end int
}

func (l *linkLine) write(s string) { l.text.WriteString(s) }

func (l *linkLine) link(url, label string) {
	if label == "" {
		label = url
	}
	start := l.text.Len()
	l.text.WriteString(label)
	if url != "" {
		l.links = append(l.links, pendingLink{url, label, start, l.text.Len()})
	}
}

func (l *linkLine) flush(out *[]watcher.LinkRef) {
	full := l.text.String()
	for _, k := range l.links {
		*out = append(*out, watcher.LinkRef{
			URL: k.url, Label: k.label,
			Before: watcher.CollapseSpace(full[:k.start]),
			After:  watcher.CollapseSpace(full[k.end:]),
		})
	}
}

func elementsLine(els []Element, out *[]watcher.LinkRef) {
	var l linkLine
	for _, e := range els {
		switch e.Type {
		case "text":
			l.write(e.Text)
		case "link":
			l.link(e.URL, e.Text)
		case "user":
			l.write("@user")
		case "emoji":
			l.write(":" + e.Name + ":")
		case "usergroup":
			l.write("@group")
		case "broadcast":
			l.write("@" + e.Range)
		}
	}
	l.flush(out)
}

func blockLinks(bs []Block) []watcher.LinkRef {
	var out []watcher.LinkRef
	for _, b := range bs {
		elementsLine(b.Elements, &out)
		for _, item := range b.Items {
			elementsLine(item, &out)
		}
	}
	return out
}

// mrkdwnToken matches any <...> token in mrkdwn text: links, mentions,
// channels and special commands.
var mrkdwnToken = regexp.MustCompile(`<([^<>]*)>`)

// mrkdwnLinks scans block-less mrkdwn text line by line. Mentions and
// channels are rendered as readable context (@U123, #name, @here).
func mrkdwnLinks(text string) []watcher.LinkRef {
	var out []watcher.LinkRef
	for _, line := range strings.Split(text, "\n") {
		var l linkLine
		pos := 0
		for _, loc := range mrkdwnToken.FindAllStringSubmatchIndex(line, -1) {
			l.write(unescapeMrkdwnEntities(line[pos:loc[0]]))
			pos = loc[1]
			body := line[loc[2]:loc[3]]
			target, label, _ := strings.Cut(body, "|")
			switch {
			case strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://"):
				if strings.ContainsAny(target, " \t") {
					l.write(unescapeMrkdwnEntities(line[loc[0]:loc[1]]))
					continue
				}
				l.link(unescapeMrkdwnEntities(target), unescapeMrkdwnEntities(label))
			case strings.HasPrefix(target, "@"):
				if label != "" {
					l.write("@" + strings.TrimPrefix(unescapeMrkdwnEntities(label), "@"))
				} else {
					l.write(target)
				}
			case strings.HasPrefix(target, "#"):
				if label != "" {
					l.write("#" + unescapeMrkdwnEntities(label))
				} else {
					l.write(target)
				}
			case strings.HasPrefix(target, "!"):
				name := strings.TrimPrefix(target, "!")
				switch {
				case label != "":
					l.write(unescapeMrkdwnEntities(label))
				case strings.HasPrefix(name, "subteam"):
					l.write("@group")
				default:
					l.write("@" + name)
				}
			default:
				l.write(unescapeMrkdwnEntities(line[loc[0]:loc[1]]))
			}
		}
		l.write(unescapeMrkdwnEntities(line[pos:]))
		l.flush(&out)
	}
	return out
}

// unescapeMrkdwnEntities reverses the HTML escaping Slack applies to message
// text (& < >), so a mrkdwn URL equals the rich_text URL for the same link and
// dedupes against it. &amp; is last so "&amp;lt;" becomes "&lt;", not "<".
func unescapeMrkdwnEntities(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}
