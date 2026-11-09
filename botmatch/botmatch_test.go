package botmatch

import "testing"

func TestMatch(t *testing.T) {
	for _, tc := range []struct {
		patterns []string
		name     string
		want     bool
	}{
		{[]string{"*ci-bot*"}, "rhods-ci-bot", true},
		{[]string{"*ci-bot*"}, "ci-bot", true},
		{[]string{"*ci-bot*"}, "my-ci-bot-2", true},
		{[]string{"*ci-bot*"}, "RHODS-CI-BOT", true},
		{[]string{"*ci-bot*"}, "cibot", false},
		{[]string{"jenkins"}, "jenkins", true},
		{[]string{"jenkins"}, "Jenkins", true},
		{[]string{"jenkins"}, "jenkins2", false},
		{[]string{"bot?"}, "bot1", true},
		{[]string{"bot?"}, "bot12", false},
		{[]string{"a*b*c"}, "axxbyyc", true},
		{[]string{"a*b*c"}, "axxbyy", false},
		{[]string{"", "  "}, "anyone", false},
		{[]string{"*"}, "", false},
		{nil, "anyone", false},
		{[]string{"x", " *ci-bot* "}, "rhods-ci-bot", true},
	} {
		if got := Match(tc.patterns, tc.name); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.patterns, tc.name, got, tc.want)
		}
	}
}

func TestLikePattern(t *testing.T) {
	for p, want := range map[string]string{
		"*ci-bot*":  "%ci-bot%",
		"bot?":      "bot_",
		"a_b%c\\d":  `a\_b\%c\\d`,
		" jenkins ": "jenkins",
		"   ":       "",
	} {
		if got := LikePattern(p); got != want {
			t.Errorf("LikePattern(%q) = %q, want %q", p, got, want)
		}
	}
}
