package slack

import "testing"

func TestMentionIDs(t *testing.T) {
	u, g := MentionIDs("hi <@U1> and <@U1> and <!subteam^S1> plus <!subteam^S2|@lbl> <https://x|y>")
	if len(u) != 1 || u[0] != "U1" {
		t.Errorf("user ids wrong (should dedupe): %v", u)
	}
	if len(g) != 2 || g[0] != "S1" || g[1] != "S2" {
		t.Errorf("group ids wrong: %v", g)
	}
}

func TestResolveMentions(t *testing.T) {
	users := map[string]string{"U1": "ana"}
	groups := map[string]UserGroup{"S1": {ID: "S1", Handle: "platform", Name: "Platform Team"}}

	cases := []struct{ in, want string }{
		{"ping <@U1>", "ping @ana"},
		{"ping <@U9>", "ping @U9"},                       // unknown user -> id, not a generic word
		{"ping <!subteam^S1>", "ping @platform"},         // handle preferred
		{"ping <!subteam^S9>", "ping @S9"},               // unknown group -> id
		{"ping <!subteam^S1|@override>", "ping @override"}, // inline label wins
		{"<!here> <!channel> <!everyone>", "@here @channel @everyone"},
		{"see <https://example.com|docs>", "see <https://example.com|docs>"}, // links untouched
		{"", ""},
	}
	for _, c := range cases {
		if got := ResolveMentions(c.in, users, groups); got != c.want {
			t.Errorf("ResolveMentions(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveMentionsFallsBackToGroupName(t *testing.T) {
	groups := map[string]UserGroup{"S2": {ID: "S2", Handle: "", Name: "Design"}}
	if got := ResolveMentions("<!subteam^S2>", nil, groups); got != "@Design" {
		t.Errorf("want @Design, got %q", got)
	}
}
