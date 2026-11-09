package github

import "testing"

func TestMarkBotLogins(t *testing.T) {
	d := PRData{
		Author: "rhods-ci-bot", AuthorType: "user",
		MergedBy: "alice", MergedByType: "user",
		Reviews:        []Review{{Author: "rhods-ci-bot", AuthorType: "user"}, {Author: "bob", AuthorType: "user"}},
		Comments:       []Comment{{Author: "Other-CI-Bot", AuthorType: "user"}, {Author: "dependabot", AuthorType: "bot"}},
		ReviewComments: []ReviewComment{{Author: "carol", AuthorType: "user"}, {Author: "x-ci-bot", AuthorType: "user"}},
	}
	markBotLogins(&d, []string{"*ci-bot*"})
	got := []string{d.AuthorType, d.MergedByType, d.Reviews[0].AuthorType, d.Reviews[1].AuthorType,
		d.Comments[0].AuthorType, d.Comments[1].AuthorType, d.ReviewComments[0].AuthorType, d.ReviewComments[1].AuthorType}
	want := []string{"bot", "user", "bot", "user", "bot", "bot", "user", "bot"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("author types %v, want %v", got, want)
		}
	}
	// No patterns: nothing changes.
	e := PRData{Author: "rhods-ci-bot", AuthorType: "user"}
	markBotLogins(&e, nil)
	if e.AuthorType != "user" {
		t.Fatalf("no patterns marked %q", e.AuthorType)
	}
}
