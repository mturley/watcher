package watcher

import "strings"

// LinkRef is one occurrence of a link in rich text: its URL, the text it
// was displayed as, and the text immediately around it in the same block.
//
// Label equals URL when the link was shown as its URL (bare URL, autolink,
// smart card). Before and After are the block's text before and after the
// link with whitespace collapsed; they are NOT truncated — consumers do that.
type LinkRef struct{ URL, Label, Before, After string }

// CollapseSpace trims s and collapses every whitespace run to one space.
func CollapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// DedupeLinkURLs returns the URLs of refs, in order, without repeats.
func DedupeLinkURLs(refs []LinkRef) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range refs {
		if r.URL == "" || seen[r.URL] {
			continue
		}
		seen[r.URL] = true
		out = append(out, r.URL)
	}
	return out
}
