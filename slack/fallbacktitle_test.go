package slack

import "testing"

func TestFallbackTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"hello world", "hello world"},
		{"  multiple   spaces\n\tand tabs ", "multiple spaces and tabs"},
		{"this is a fairly long first message that exceeds sixty characters for sure", "this is a fairly long first message that exceeds sixty chara…"},
	}
	for _, c := range cases {
		if got := fallbackTitle(c.in); got != c.want {
			t.Errorf("fallbackTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
