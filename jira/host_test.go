package jira

import "testing"

func TestNormalizeHost(t *testing.T) {
	ok := []struct{ in, want string }{
		{"https://acme.atlassian.net", "https://acme.atlassian.net"},
		{"acme.atlassian.net", "https://acme.atlassian.net"},
		{"  acme.atlassian.net/  ", "https://acme.atlassian.net"},
		{"https://acme.atlassian.net///", "https://acme.atlassian.net"},
		{"http://jira.internal:8080", "http://jira.internal:8080"},
		{"HTTPS://Acme.atlassian.net", "HTTPS://Acme.atlassian.net"},
		{"https://jira.example.com/jira/", "https://jira.example.com/jira"},
	}
	for _, c := range ok {
		got, err := NormalizeHost(c.in)
		if err != nil {
			t.Errorf("NormalizeHost(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	bad := []string{"", "   ", "ftp://acme.atlassian.net", "https://", "https:///browse", "acme atlassian net"}
	for _, in := range bad {
		if got, err := NormalizeHost(in); err == nil {
			t.Errorf("NormalizeHost(%q) = %q, want an error", in, got)
		}
	}
}
