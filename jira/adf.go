package jira

import (
	"regexp"
	"strings"
)

// bareURLPattern finds URLs typed as plain text. Brackets and quotes end a
// URL so that "(https://x)" and "\"https://x\"" yield just the URL.
var bareURLPattern = regexp.MustCompile(`https?://[^\s<>"'()\[\]]+`)

// ExtractADFURLs returns every URL in an Atlassian Document Format value, in
// document order, deduplicated: inlineCard/blockCard/embedCard attrs.url,
// text nodes' link-mark hrefs, and bare URLs typed into text. A plain string
// (Jira Server-style field values) is scanned for bare URLs. Anything else
// yields nil.
func ExtractADFURLs(doc interface{}) []string {
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		u = strings.TrimRight(u, ".,;:!?")
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	var walk func(n interface{})
	walk = func(n interface{}) {
		switch v := n.(type) {
		case string:
			for _, u := range bareURLPattern.FindAllString(v, -1) {
				add(u)
			}
		case []interface{}:
			for _, c := range v {
				walk(c)
			}
		case map[string]interface{}:
			t, _ := v["type"].(string)
			switch t {
			case "inlineCard", "blockCard", "embedCard":
				if a, ok := v["attrs"].(map[string]interface{}); ok {
					if u, ok := a["url"].(string); ok {
						add(u)
					}
				}
			case "text":
				linked := false
				if marks, ok := v["marks"].([]interface{}); ok {
					for _, m := range marks {
						mm, _ := m.(map[string]interface{})
						if mm["type"] != "link" {
							continue
						}
						if a, ok := mm["attrs"].(map[string]interface{}); ok {
							if h, ok := a["href"].(string); ok {
								add(h)
								linked = true
							}
						}
					}
				}
				if !linked {
					if s, ok := v["text"].(string); ok {
						walk(s)
					}
				}
			}
			// Only "content" is walked: map iteration order is random, and
			// attrs/marks were handled above.
			if c, ok := v["content"].([]interface{}); ok {
				walk(c)
			}
		}
	}
	walk(doc)
	return out
}
