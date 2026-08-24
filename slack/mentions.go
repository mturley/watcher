package slack

import "regexp"

// mentionTokenRe matches any Slack angle-bracket token so we can rewrite the
// mention kinds and leave everything else (links, unknown tokens) untouched.
var mentionTokenRe = regexp.MustCompile(`<([^<>]+)>`)

var (
	userMentionRe  = regexp.MustCompile(`^@([A-Z0-9]+)$`)
	groupMentionRe = regexp.MustCompile(`^!subteam\^([A-Z0-9]+)(?:\|(.*))?$`)
)

// MentionIDs returns the distinct user and group ids referenced by text.
// Callers use it to fetch only the directories a message actually needs.
func MentionIDs(text string) (userIDs, groupIDs []string) {
	seenU, seenG := map[string]bool{}, map[string]bool{}
	for _, m := range mentionTokenRe.FindAllStringSubmatch(text, -1) {
		inner := m[1]
		if mm := userMentionRe.FindStringSubmatch(inner); mm != nil {
			if !seenU[mm[1]] {
				seenU[mm[1]] = true
				userIDs = append(userIDs, mm[1])
			}
			continue
		}
		if mm := groupMentionRe.FindStringSubmatch(inner); mm != nil {
			if !seenG[mm[1]] {
				seenG[mm[1]] = true
				groupIDs = append(groupIDs, mm[1])
			}
		}
	}
	return userIDs, groupIDs
}

// ResolveMentions rewrites Slack's mention tokens into readable text:
// "<@U123>" becomes "@ana", "<!subteam^S1>" becomes "@platform",
// "<!here>" becomes "@here".
//
// This is the Go counterpart of ui/src/lib/resolveMentions.ts. It exists for
// the same reason fallbackTitle does — the cached thread title must match
// what the live view renders, and without it a cached title shows raw
// "<@U…>" ids for the very text that resolves to a name in the thread.
//
// Unknown ids fall back to the bare id, never a generic word: an unresolved
// mention should still say WHICH mention it was.
func ResolveMentions(text string, users map[string]string, groups map[string]UserGroup) string {
	if text == "" {
		return ""
	}
	return mentionTokenRe.ReplaceAllStringFunc(text, func(tok string) string {
		inner := tok[1 : len(tok)-1]
		if mm := userMentionRe.FindStringSubmatch(inner); mm != nil {
			if name := users[mm[1]]; name != "" {
				return "@" + name
			}
			return "@" + mm[1]
		}
		if mm := groupMentionRe.FindStringSubmatch(inner); mm != nil {
			// An inline label, when Slack supplies one, wins: it is what the
			// sender's client displayed.
			if len(mm) > 2 && mm[2] != "" {
				label := mm[2]
				if label[0] != '@' {
					label = "@" + label
				}
				return label
			}
			if g, ok := groups[mm[1]]; ok {
				if g.Handle != "" {
					return "@" + g.Handle
				}
				if g.Name != "" {
					return "@" + g.Name
				}
			}
			return "@" + mm[1]
		}
		switch inner {
		case "!here":
			return "@here"
		case "!channel":
			return "@channel"
		case "!everyone":
			return "@everyone"
		}
		return tok // links and anything else are left exactly as they were
	})
}
