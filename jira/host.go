package jira

import (
	"fmt"
	"net/url"
	"strings"
)

// NormalizeHost turns a Jira site URL as a person types it into the base URL
// the client and Validate expect. A bare host ("acme.atlassian.net") gets
// "https://"; surrounding space and trailing slashes are trimmed. Anything
// that is not an http(s) URL with a host is an error.
func NormalizeHost(raw string) (string, error) {
	h := strings.TrimSpace(raw)
	if h == "" {
		return "", fmt.Errorf("jira site URL is empty")
	}
	// Decide on the scheme before trimming slashes, so "https://" alone
	// stays a URL with no host rather than becoming the host "https:".
	if !strings.Contains(h, "://") {
		h = "https://" + h
	}
	h = strings.TrimRight(h, "/")
	u, err := url.Parse(h)
	if err != nil {
		return "", fmt.Errorf("jira site URL %q: %w", raw, err)
	}
	if s := strings.ToLower(u.Scheme); s != "https" && s != "http" {
		return "", fmt.Errorf("jira site URL %q must be http(s)", raw)
	}
	if u.Host == "" || strings.ContainsAny(u.Host, " \t") {
		return "", fmt.Errorf("jira site URL %q has no host", raw)
	}
	return h, nil
}
