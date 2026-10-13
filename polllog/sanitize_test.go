package polllog

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const githubUnicorn = `<!DOCTYPE html>
<!--
Hello future GitHubber!
-->
<html><head><title>Unicorn! &middot; GitHub</title>
<style>body { color: red }</style></head>
<body><p><strong>We couldn't respond to your request in time.</strong></p>
<img src="data:image/png;base64,iVBORw0KGgoAAAA"></body></html>`

func TestSanitize(t *testing.T) {
	cases := []struct {
		name, body, ct, want string
	}{
		{"html title wins", githubUnicorn, "text/html", "Unicorn! · GitHub"},
		{"html sniffed without content type", githubUnicorn, "", "Unicorn! · GitHub"},
		{"html without title strips tags/style", `<html><style>x{}</style><p>Bad  <b>gateway</b></p></html>`, "text/html", "Bad gateway"},
		{"json kept, whitespace collapsed", "{\"message\": \"\",\n  \"status\": 503}", "application/json", `{"message": "", "status": 503}`},
		{"empty", "", "", ""},
	}
	for _, c := range cases {
		if got := Sanitize([]byte(c.body), c.ct); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestSanitize_CapsAt500Runes(t *testing.T) {
	got := Sanitize([]byte(strings.Repeat("é", 800)), "text/plain")
	if n := utf8.RuneCountInString(got); n != 500 || !strings.HasSuffix(got, "…") {
		t.Fatalf("len %d suffix %q", n, got[len(got)-3:])
	}
}

func TestSanitize_InvalidUTF8(t *testing.T) {
	if got := Sanitize([]byte{'o', 'k', 0xff, 'x'}, ""); !utf8.ValidString(got) {
		t.Fatalf("invalid utf8 out: %q", got)
	}
}

func TestSanitizeMessage_EmbeddedHTML(t *testing.T) {
	msg := "Failed to fetch PR data: GitHub API returned status 504: " + githubUnicorn
	want := "Failed to fetch PR data: GitHub API returned status 504: Unicorn! · GitHub"
	if got := SanitizeMessage(msg); got != want {
		t.Fatalf("got %q", got)
	}
	if got := SanitizeMessage("slack error: thread_not_found"); got != "slack error: thread_not_found" {
		t.Fatalf("plain message changed: %q", got)
	}
}
