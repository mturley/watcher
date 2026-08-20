package slack

import "context"

// Validate reports whether the given browser-session credentials are accepted
// by Slack. Returns a wrapped ErrAuth when Slack rejects them. An optional
// baseURL overrides the Slack API base URL, for use in tests.
func Validate(token, cookie string, baseURL ...string) error {
	c := New(token, cookie)
	if len(baseURL) > 0 && baseURL[0] != "" {
		c = NewWithBaseURL(token, cookie, baseURL[0])
	}
	return c.AuthTest(context.Background())
}
