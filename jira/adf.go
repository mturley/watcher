package jira

import (
	"regexp"
	"strings"

	"github.com/mturley/watcher"
)

// bareURLPattern finds URLs typed as plain text. Brackets and quotes end a
// URL so that "(https://x)" and "\"https://x\"" yield just the URL.
var bareURLPattern = regexp.MustCompile(`https?://[^\s<>"'()\[\]]+`)

const urlTrimChars = ".,;:!?"

// ExtractADFURLs returns every URL in an Atlassian Document Format value, in
// document order, deduplicated: inlineCard/blockCard/embedCard attrs.url,
// text nodes' link-mark hrefs, and bare URLs typed into text. A plain string
// (Jira Server-style field values) is scanned for bare URLs. Anything else
// yields nil.
func ExtractADFURLs(doc interface{}) []string {
	return watcher.DedupeLinkURLs(ExtractADFLinks(doc))
}

// pendingLink is a link found while building a block's text; start/end are
// byte offsets into the block's text.
type pendingLink struct {
	url, label string
	start, end int
}

// blockBuilder accumulates the inline text of one block and the links in it.
type blockBuilder struct {
	text  strings.Builder
	links []pendingLink
	// openHref/openIdx: the link occurrence the previous text node belongs
	// to, so consecutive nodes sharing an href merge into one occurrence.
	openHref string
	openIdx  int
}

func (b *blockBuilder) closeLink() { b.openHref = ""; b.openIdx = -1 }

func (b *blockBuilder) plain(s string) {
	b.closeLink()
	b.text.WriteString(s)
}

func (b *blockBuilder) bareURLs(s string) {
	b.closeLink()
	base := b.text.Len()
	b.text.WriteString(s)
	for _, loc := range bareURLPattern.FindAllStringIndex(s, -1) {
		raw := s[loc[0]:loc[1]]
		u := strings.TrimRight(raw, urlTrimChars)
		if u == "" {
			continue
		}
		b.links = append(b.links, pendingLink{u, u, base + loc[0], base + loc[0] + len(u)})
	}
}

func (b *blockBuilder) linked(href, s string) {
	href = strings.TrimRight(href, urlTrimChars)
	start := b.text.Len()
	b.text.WriteString(s)
	if href == "" {
		b.closeLink()
		return
	}
	if b.openHref == href && b.openIdx >= 0 {
		l := &b.links[b.openIdx]
		l.label += s
		l.end = b.text.Len()
		return
	}
	b.links = append(b.links, pendingLink{href, s, start, b.text.Len()})
	b.openHref, b.openIdx = href, len(b.links)-1
}

func (b *blockBuilder) card(u string) {
	b.closeLink()
	u = strings.TrimRight(u, urlTrimChars)
	start := b.text.Len()
	b.text.WriteString(u)
	if u != "" {
		b.links = append(b.links, pendingLink{u, u, start, b.text.Len()})
	}
}

func (b *blockBuilder) flush(out *[]watcher.LinkRef) {
	full := b.text.String()
	for _, l := range b.links {
		*out = append(*out, watcher.LinkRef{
			URL:    l.url,
			Label:  l.label,
			Before: watcher.CollapseSpace(full[:l.start]),
			After:  watcher.CollapseSpace(full[l.end:]),
		})
	}
}

func isInlineADF(t string) bool {
	switch t {
	case "text", "hardBreak", "mention", "emoji", "inlineCard", "status", "date", "mediaInline":
		return true
	}
	return false
}

func attrString(v map[string]interface{}, keys ...string) string {
	a, _ := v["attrs"].(map[string]interface{})
	for _, k := range keys {
		if s, ok := a[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func linkHref(v map[string]interface{}) string {
	marks, _ := v["marks"].([]interface{})
	for _, m := range marks {
		mm, _ := m.(map[string]interface{})
		if mm["type"] != "link" {
			continue
		}
		if a, ok := mm["attrs"].(map[string]interface{}); ok {
			if h, ok := a["href"].(string); ok {
				return h
			}
		}
	}
	return ""
}

// ExtractADFLinks returns every link occurrence in an Atlassian Document
// Format value, in document order and NOT deduplicated, each with its label
// and the text around it within the same block (the nearest ancestor whose
// children are inline nodes). A plain string (Jira Server-style field
// values) is scanned for bare URLs. Anything else yields nil.
func ExtractADFLinks(doc interface{}) []watcher.LinkRef {
	var out []watcher.LinkRef
	var walk func(n interface{})
	walk = func(n interface{}) {
		switch v := n.(type) {
		case string:
			b := &blockBuilder{openIdx: -1}
			b.bareURLs(v)
			b.flush(&out)
		case []interface{}:
			for _, c := range v {
				walk(c)
			}
		case map[string]interface{}:
			t, _ := v["type"].(string)
			if t == "blockCard" || t == "embedCard" {
				if u := strings.TrimRight(attrString(v, "url"), urlTrimChars); u != "" {
					out = append(out, watcher.LinkRef{URL: u, Label: u})
				}
				return
			}
			content, _ := v["content"].([]interface{})
			inline := false
			for _, c := range content {
				if cm, ok := c.(map[string]interface{}); ok {
					if ct, _ := cm["type"].(string); isInlineADF(ct) {
						inline = true
						break
					}
				}
			}
			if !inline {
				walk(content)
				return
			}
			b := &blockBuilder{openIdx: -1}
			for _, c := range content {
				cm, ok := c.(map[string]interface{})
				if !ok {
					continue
				}
				ct, _ := cm["type"].(string)
				switch ct {
				case "text":
					s, _ := cm["text"].(string)
					if h := linkHref(cm); h != "" {
						b.linked(h, s)
					} else {
						b.bareURLs(s)
					}
				case "hardBreak":
					b.plain(" ")
				case "mention":
					b.plain(attrString(cm, "text"))
				case "emoji":
					b.plain(attrString(cm, "text", "shortName"))
				case "inlineCard":
					b.card(attrString(cm, "url"))
				case "status":
					b.plain(attrString(cm, "text"))
				default:
					// Block-level child mixed in with inline ones (not
					// expected): walk it for its own links.
					b.closeLink()
					if !isInlineADF(ct) {
						walk(cm)
					}
				}
			}
			b.flush(&out)
		}
	}
	walk(doc)
	return out
}
