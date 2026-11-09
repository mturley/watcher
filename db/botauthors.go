package db

import (
	"database/sql"
	"strings"

	"github.com/mturley/watcher/botmatch"
)

// MarkBotAuthors marks every stored event from source whose author matches
// one of patterns (botmatch globs) as a bot, returning how many it changed.
//
// The pollers classify new events as they arrive; this brings events already
// stored into line when the patterns change, so an account added to the list
// reads as a bot in past activity too. It only ever marks: the type GitHub or
// Jira originally reported is not kept, so removing a pattern leaves the
// events it marked as they are. Idempotent; blank patterns are ignored, and
// none at all changes nothing.
func MarkBotAuthors(conn *sql.DB, source string, patterns []string) (int64, error) {
	var likes []string
	args := []any{source}
	for _, p := range patterns {
		if l := botmatch.LikePattern(p); l != "" {
			likes = append(likes, `author LIKE ? ESCAPE '\'`)
			args = append(args, l)
		}
	}
	if len(likes) == 0 {
		return 0, nil
	}
	res, err := conn.Exec(`UPDATE watcher_events SET author_type = 'bot'
WHERE source = ? AND author IS NOT NULL AND author <> ''
  AND COALESCE(author_type, '') <> 'bot'
  AND (`+strings.Join(likes, " OR ")+`)`, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
