package slack

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/mturley/watcher"
	"github.com/mturley/watcher/db"
	"github.com/mturley/watcher/polllog"
)

// SlackAuth holds the credentials needed to poll Slack.
type SlackAuth struct {
	Token           string
	Cookie          string
	WorkspaceDomain string
}

// Poll polls Slack for new thread replies and emits events. Each resource's
// backfill flag is resolved from its live subscriptions.
func Poll(conn *sql.DB, cfg SlackAuth, resources []watcher.Resource, logger *log.Logger) error {
	if cfg.Token == "" || cfg.Cookie == "" {
		return fmt.Errorf("slack not configured")
	}

	client := New(cfg.Token, cfg.Cookie).WithTransport(&polllog.Transport{Conn: conn, Source: "slack", Logger: logger})
	return pollWith(conn, client, resources, logger)
}

// pollWith polls the given resources using client, recording per-thread
// outcomes and watcher log rows.
func pollWith(conn *sql.DB, client Client, resources []watcher.Resource, logger *log.Logger) error {
	defer polllog.Prune(conn, logger)
	logRow := func(level, msg string, rs ...watcher.Resource) {
		if err := polllog.Write(conn, "slack", level, msg, rs...); err != nil {
			logger.Printf("WARNING: watcher log write failed: %v", err)
		}
	}
	record := func(r watcher.Resource, ok bool, msg string, d db.ResourcePollDetail) {
		if err := db.RecordResourcePoll(conn, r, ok, polllog.SanitizeMessage(msg), d); err != nil {
			logger.Printf("WARNING: record resource poll %s: %v", r.ID, err)
		}
	}
	logRow("info", fmt.Sprintf("slack poll started: %d resources", len(resources)))
	failed := 0

	eventCount := 0
	for _, resource := range resources {
		channel, threadTS, ok := parseResourceID(resource.ID)
		if !ok {
			logger.Printf("ERROR: bad slack resource id %q", resource.ID)
			record(resource, false, "invalid slack resource id", db.ResourcePollDetail{})
			logRow("error", "invalid slack resource id "+resource.ID, resource)
			failed++
			continue
		}

		ctx, stats := polllog.WithStats(polllog.WithResources(context.Background(), resource))
		thread, err := client.Replies(ctx, channel, threadTS)
		if err != nil {
			// Slack reports API errors as HTTP 200 with "ok": false, so the
			// http log row is info; this per-thread row carries the failure.
			logger.Printf("ERROR: fetch thread %s: %v", resource.ID, err)
			errBody := polllog.SanitizeMessage(fmt.Sprintf("Failed to fetch thread: %v", err))
			if e := emitError(conn, fmt.Sprintf("Failed to fetch %s", resource.ID), &errBody, resource); e != nil {
				logger.Printf("ERROR: emit watcher error: %v", e)
			}
			if e := db.RecordPollerError(conn, "slack", errBody); e != nil {
				logger.Printf("ERROR: record poller error: %v", e)
			}
			status, ms := stats.Get()
			record(resource, false, errBody, db.ResourcePollDetail{HTTPStatus: status, DurationMS: ms})
			logRow("error", errBody, resource)
			failed++
			continue
		}

		// Guard the degenerate case of a thread with no messages (e.g. the
		// root message was deleted out-of-band). Emitting watch_started with
		// an empty ExternalTS would leave the cursor at "" forever, so the
		// first-poll gate would re-fire a watch_started on every poll. Skip
		// this resource this cycle rather than caching/emitting empty state.
		if len(thread.Messages) == 0 {
			logger.Printf("WARNING: slack thread %s has no messages; skipping this cycle", resource.ID)
			status, ms := stats.Get()
			msg := "thread has no messages (root deleted?)"
			record(resource, false, msg, db.ResourcePollDetail{HTTPStatus: status, DurationMS: ms})
			logRow("warn", msg, resource)
			failed++
			continue
		}

		backfill, err := db.BackfillFor(conn, resource.Type, resource.ID)
		if err != nil {
			logger.Printf("WARNING: backfill resolve %s: %v", resource.ID, err)
		}

		// Resolve display names for ALL message authors (root + replies) in
		// one batched client.Users call; used for both slack_reply event
		// authors and the cached root author.
		names := resolveAuthors(client, thread.Messages)

		count, resolvedRoot, err := processThread(conn, client, thread, names, resource, backfill, logger)
		if err != nil {
			logger.Printf("ERROR: process thread %s: %v", resource.ID, err)
			errBody := polllog.SanitizeMessage(fmt.Sprintf("Failed to process thread: %v", err))
			if e := emitError(conn, fmt.Sprintf("Error processing %s", resource.ID), &errBody, resource); e != nil {
				logger.Printf("ERROR: emit watcher error: %v", e)
			}
			status, ms := stats.Get()
			record(resource, false, errBody, db.ResourcePollDetail{HTTPStatus: status, DurationMS: ms})
			logRow("error", errBody, resource)
			failed++
			continue
		}
		eventCount += count

		// Cache thread title/state (always, incl. first poll). The channel
		// name is stable, so resolveChannelName reuses the cached value and
		// only calls the Slack API until it first succeeds.
		channelName := resolveChannelName(conn, client, resource, channel)
		rootAuthor := ""
		if len(thread.Messages) > 0 {
			rootAuthor = names[thread.Messages[0].UserID]
		}
		// The root text arrives with its mention tokens already resolved, so
		// the cached title agrees with the event titles and the thread view.
		stateJSON := buildSlackStateJSON(thread, channelName, rootAuthor, resolvedRoot)
		latestTS := latestThreadTS(thread)
		now := time.Now().UTC().Format(time.RFC3339)
		if err := db.UpsertResourceState(conn, "slack", resource.ID, stateJSON, latestTS, now); err != nil {
			logger.Printf("WARNING: upsert resource state %s: %v", resource.ID, err)
		}
		status, ms := stats.Get()
		record(resource, true, "", db.ResourcePollDetail{HTTPStatus: status, DurationMS: ms, EventsEmitted: count})
	}

	logger.Printf("Emitted %d events", eventCount)
	level := "info"
	if failed > 0 {
		level = "warn"
	}
	logRow(level, fmt.Sprintf("slack poll finished: %d events, %d of %d resources failed", eventCount, failed, len(resources)))
	if err := db.RecordPollerSuccess(conn, "slack"); err != nil {
		logger.Printf("ERROR: record poller success: %v", err)
	}
	return nil
}

// emitEvent inserts a watcher event for the given resource, setting the ID
// and recording timestamp that db.InsertEvent does not set.
func emitEvent(conn *sql.DB, t watcher.EventType, title string, body *string, externalTS string, author, authorType, authorID *string, r watcher.Resource) error {
	extTS := externalTS
	return db.InsertEvent(conn, watcher.Event{
		ID:         uuid.New().String(),
		TS:         time.Now().UTC().Format(time.RFC3339),
		ExternalTS: &extTS,
		Source:     "slack",
		Type:       t,
		Title:      title,
		Body:       body,
		Author:     author,
		AuthorType: authorType,
		AuthorID:   authorID,
	}, r)
}

// emitError inserts a watcher_error event, guarded by db.HasPollerError to
// avoid duplicate error spam (mirrors the framework's EmitWatcherError guard).
func emitError(conn *sql.DB, title string, body *string, r watcher.Resource) error {
	if db.HasPollerError(conn, "slack") {
		return nil
	}
	return db.InsertEvent(conn, watcher.Event{
		ID:     uuid.New().String(),
		TS:     time.Now().UTC().Format(time.RFC3339),
		Source: "slack",
		Type:   watcher.EventTypeWatcherError,
		Title:  title,
		Body:   body,
	}, r)
}

// parseResourceID splits a Slack resource ID ("<channel>:<thread_ts>") on
// the first colon into (channel, threadTS, ok). Slack channel IDs never
// contain a colon; thread_ts is "digits.digits".
func parseResourceID(id string) (channel, threadTS string, ok bool) {
	for i := 0; i < len(id); i++ {
		if id[i] == ':' {
			return id[:i], id[i+1:], true
		}
	}
	return "", "", false
}

// processThread processes a single Slack thread's replies and emits events.
// Returns the count of events emitted, and the root text with its mentions
// resolved for the caller to cache. When cursor is empty and backfill is
// false, only a watch_started event is emitted on the first poll.
//
// Mention tokens ("<@U…>", "<!subteam^S…>") are resolved to names before any
// text is stored: an event's title and body are read by consumers that have no
// user directory, and would otherwise show raw ids for the very text that
// renders as names in the thread view.
func processThread(conn *sql.DB, client Client, thread Thread, names map[string]string, resource watcher.Resource, backfill bool, logger *log.Logger) (int, string, error) {
	eventCount := 0

	cursor, err := db.EventCursor(conn, "slack", resource.Type, resource.ID)
	if err != nil {
		return 0, "", fmt.Errorf("event cursor: %w", err)
	}

	firstPoll := cursor == "" && !backfill

	// Only replies past the cursor get events, so only their mentions (and
	// the root's, which titles every event) need a directory lookup.
	var fresh []Message
	if !firstPoll && len(thread.Messages) > 1 {
		for _, m := range thread.Messages[1:] { // skip the root
			if m.TS > cursor { // raw Slack ts string compare — the invariant
				fresh = append(fresh, m)
			}
		}
	}
	texts := []string{rootText(thread)}
	for _, m := range fresh {
		texts = append(texts, m.Text)
	}
	mentions := newMentionResolver(client, names, texts...)
	resolvedRoot := mentions.resolve(rootText(thread))
	title := fallbackTitle(resolvedRoot)

	if firstPoll {
		wsTitle := fmt.Sprintf("Started watching thread: %s", title)
		body := title
		if err := emitEvent(conn, watcher.EventTypeWatchStarted, wsTitle, &body, latestThreadTS(thread), nil, nil, nil, resource); err != nil {
			return 0, resolvedRoot, fmt.Errorf("emit watch_started: %w", err)
		}
		return 1, resolvedRoot, nil
	}

	for _, m := range fresh {

		dup, err := db.IsDuplicate(conn, db.DedupCheck{
			Source: "slack", ResourceType: resource.Type, ResourceID: resource.ID,
			Type: watcher.EventTypeSlackReply, ExternalTS: &m.TS,
		})
		if err != nil {
			return eventCount, resolvedRoot, fmt.Errorf("dedup: %w", err)
		}
		if dup {
			continue
		}

		author := names[m.UserID]
		evTitle := fmt.Sprintf("New reply in %s", title)
		snippet := fallbackTitle(mentions.resolve(m.Text))
		body := snippet
		if author != "" {
			body = author + ": " + snippet
		}
		var authorPtr *string
		if author != "" {
			authorPtr = &author
		}
		if err := emitEvent(conn, watcher.EventTypeSlackReply, evTitle, &body, m.TS, authorPtr, nil, optional(m.UserID), resource); err != nil {
			return eventCount, resolvedRoot, fmt.Errorf("emit slack_reply: %w", err)
		}
		eventCount++
		logger.Printf("Emitted slack_reply for %s", resource.ID)
	}

	return eventCount, resolvedRoot, nil
}

// rootText returns the root message's text, or "" if the thread has no
// messages.
func rootText(thread Thread) string {
	if len(thread.Messages) == 0 {
		return ""
	}
	return thread.Messages[0].Text
}

// latestThreadTS returns the max ts among the thread's messages. Messages
// are returned in ascending ts order, so this is the last element's ts (the
// root's ts if there are no replies).
func latestThreadTS(thread Thread) string {
	if len(thread.Messages) == 0 {
		return ""
	}
	return thread.Messages[len(thread.Messages)-1].TS
}

// resolveAuthors collects the distinct UserIDs across the given messages
// (root + replies) and resolves them via a single batched client.Users call,
// returning a map of userID -> display name (DisplayName, falling back to
// RealName, then the raw id). On error, returns an empty map so events still
// emit with no author.
func resolveAuthors(client Client, messages []Message) map[string]string {
	names := make(map[string]string)
	seen := make(map[string]bool)
	var ids []string
	for _, m := range messages {
		if m.UserID == "" || seen[m.UserID] {
			continue
		}
		seen[m.UserID] = true
		ids = append(ids, m.UserID)
	}
	if len(ids) == 0 {
		return names
	}
	users, err := client.Users(context.Background(), ids)
	if err != nil {
		return names
	}
	for id, u := range users {
		name := u.DisplayName
		if name == "" {
			name = u.RealName
		}
		if name == "" {
			name = id
		}
		names[id] = name
	}
	return names
}

// resolveChannelName resolves a channel's display name. The name is stable,
// so once cached in resource_state it is reused rather than re-fetched;
// otherwise it calls client.Channel once. On error, returns "" (the card
// falls back to the title, and the next poll retries the lookup).
func resolveChannelName(conn *sql.DB, client Client, resource watcher.Resource, channelID string) string {
	if st, err := db.GetResourceState(conn, "slack", resource.ID); err == nil && st != nil {
		var m map[string]interface{}
		if json.Unmarshal([]byte(st.StateJSON), &m) == nil {
			if v, ok := m["channel_name"].(string); ok && v != "" {
				return v
			}
		}
	}
	name, err := client.Channel(context.Background(), channelID)
	if err != nil {
		return ""
	}
	return name
}

// buildSlackStateJSON builds a JSON representation of a Slack thread's
// current state, cached to resource_state for the Overview card.
// mentionResolver rewrites mention tokens in message text to names, from
// directories fetched once for a whole batch of texts.
type mentionResolver struct {
	users  map[string]string
	groups map[string]UserGroup
}

// newMentionResolver fetches the directories the given texts reference, in one
// batched call per kind. names already covers message AUTHORS; a mention may be
// someone who never posted, so only the ids still missing are looked up. Any
// lookup failure degrades to bare ids rather than failing the poll — a title is
// a nicety, the events are the point.
func newMentionResolver(client Client, names map[string]string, texts ...string) mentionResolver {
	r := mentionResolver{users: map[string]string{}, groups: map[string]UserGroup{}}
	seenU, seenG := map[string]bool{}, map[string]bool{}
	var missing, groupIDs []string
	for _, text := range texts {
		userIDs, gIDs := MentionIDs(text)
		for _, id := range userIDs {
			if seenU[id] {
				continue
			}
			seenU[id] = true
			if n, ok := names[id]; ok && n != "" {
				r.users[id] = n
			} else {
				missing = append(missing, id)
			}
		}
		for _, id := range gIDs {
			if !seenG[id] {
				seenG[id] = true
				groupIDs = append(groupIDs, id)
			}
		}
	}
	if len(missing) > 0 {
		if fetched, err := client.Users(context.Background(), missing); err == nil {
			for id, u := range fetched {
				name := u.DisplayName
				if name == "" {
					name = u.RealName
				}
				if name != "" {
					r.users[id] = name
				}
			}
		}
	}
	if len(groupIDs) > 0 {
		if fetched, err := client.UserGroupsInfo(context.Background(), groupIDs); err == nil {
			r.groups = fetched
		}
	}
	return r
}

func (r mentionResolver) resolve(text string) string {
	return ResolveMentions(text, r.users, r.groups)
}

func buildSlackStateJSON(thread Thread, channelName, rootAuthor, resolvedRoot string) string {
	createdTS := ""
	if len(thread.Messages) > 0 {
		createdTS = thread.Messages[0].TS // root message ts = thread creation time
	}
	m := map[string]interface{}{
		"title":        fallbackTitle(resolvedRoot),
		"channel_name": channelName,            // "" if unresolved; card shows "#name"
		"author":       rootAuthor,             // display name of the thread's first-message author
		"created_ts":   createdTS,              // raw Slack ts of the root message
		"updated_ts":   latestThreadTS(thread), // raw Slack ts of the latest message
		"reply_count":  max(0, len(thread.Messages)-1),
		// Computed here, not by consumers: the read cursor arrives with the
		// thread Slack already returns, so caching the verdict lets a UI show
		// an unread marker per thread with no extra per-thread fetch. Uses
		// UnreadDividerIndex, so an absent read cursor means "nothing unread"
		// rather than "everything unread".
		"has_unread": UnreadDividerIndex(thread) >= 0,
		// The cursor the boolean above was derived from, kept because the
		// two answer different questions: has_unread says the thread has
		// something new, last_read says WHICH messages are new. A timeline
		// interleaving several resources cannot draw a divider, so it marks
		// individual replies by comparing each message ts against this.
		// Empty when Slack returned no cursor — distinguishable from "read
		// up to ts 0", and the case has_unread resolves to false.
		"last_read": thread.LastRead,
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// optional returns &s, or nil for "", so an unknown ID stores NULL.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
