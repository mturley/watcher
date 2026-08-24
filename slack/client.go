// internal/slackapi/client.go
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrAuth is returned (wrapped) when Slack reports an authentication
// failure (invalid_auth, token_expired, not_authed).
var ErrAuth = errors.New("slack auth failed")

// Client is the interface the rest of the codebase depends on for talking
// to Slack's Web API.
type Client interface {
	AuthTest(ctx context.Context) error
	WhoAmI(ctx context.Context) (string, error)
	Replies(ctx context.Context, channel, threadTS string) (Thread, error)
	Users(ctx context.Context, ids []string) (map[string]User, error)
	Channel(ctx context.Context, id string) (string, error)
	Emoji(ctx context.Context) (map[string]string, error)
	UserGroups(ctx context.Context) (map[string]UserGroup, error)
	UserGroupsInfo(ctx context.Context, ids []string) (map[string]UserGroup, error)
	MarkRead(ctx context.Context, channel, threadTS, ts string) error
	MarkUnread(ctx context.Context, channel, threadTS, ts string) error
	PostReply(ctx context.Context, channel, threadTS, text string) (Message, error)
	AddReaction(ctx context.Context, channel, ts, name string) error
	RemoveReaction(ctx context.Context, channel, ts, name string) error
}

// HTTPClient implements Client against Slack's real (or test) Web API.
type HTTPClient struct {
	token, cookie, baseURL string
	// edgeBaseURL hosts Slack's per-org cache API, which is a DIFFERENT host
	// from the main Web API and speaks JSON rather than form encoding. Tests
	// point it at the same httptest server as baseURL.
	edgeBaseURL string
	hc          *http.Client

	teamIDMu sync.Mutex
	teamID   string // cached auth.test team/enterprise id, for edge paths
}

// New returns an HTTPClient pointed at the real Slack Web API.
func New(token, cookie string) *HTTPClient {
	c := NewWithBaseURL(token, cookie, "https://slack.com/api")
	c.edgeBaseURL = "https://edgeapi.slack.com"
	return c
}

// NewWithBaseURL returns an HTTPClient pointed at baseURL, for use with
// httptest servers in tests.
func NewWithBaseURL(token, cookie, baseURL string) *HTTPClient {
	return &HTTPClient{
		token:       token,
		cookie:      cookie,
		baseURL:     strings.TrimRight(baseURL, "/"),
		edgeBaseURL: strings.TrimRight(baseURL, "/"),
		hc:          &http.Client{Timeout: 20 * time.Second},
	}
}

// TeamInfo calls Slack's team.info and returns the workspace's canonical web
// host (e.g. "myteam.slack.com", or "myteam.enterprise.slack.com" on an
// Enterprise Grid). It derives the host from the team's url field, which is the
// authoritative base for archive permalinks — this is NOT the same as
// {team.domain}.slack.com on Enterprise Grid, and it is never app.slack.com.
func (c *HTTPClient) TeamInfo(ctx context.Context) (string, error) {
	var r struct {
		Team struct {
			URL    string `json:"url"`
			Domain string `json:"domain"`
		} `json:"team"`
	}
	if err := c.call(ctx, "team.info", url.Values{}, &r); err != nil {
		return "", err
	}
	if r.Team.URL != "" {
		if u, err := url.Parse(r.Team.URL); err == nil && u.Host != "" {
			return u.Host, nil
		}
	}
	if r.Team.Domain != "" {
		// Fallback: only correct for non-Grid workspaces, but better than
		// nothing if url is unexpectedly absent.
		return r.Team.Domain + ".slack.com", nil
	}
	return "", errors.New("team.info returned no usable workspace URL")
}

// AuthTest verifies the configured token/cookie by calling Slack's auth.test.
// It returns nil when the credentials are valid, a wrapped ErrAuth when Slack
// rejects them (invalid_auth / token_expired / not_authed), or another error
// for transport/other failures.
func (c *HTTPClient) AuthTest(ctx context.Context) error {
	return c.call(ctx, "auth.test", url.Values{}, nil)
}

// WhoAmI calls Slack's auth.test and returns the authenticated user's ID.
func (c *HTTPClient) WhoAmI(ctx context.Context) (string, error) {
	var r struct {
		UserID string `json:"user_id"`
	}
	if err := c.call(ctx, "auth.test", url.Values{}, &r); err != nil {
		return "", err
	}
	return r.UserID, nil
}

// Channel looks up a channel's name via conversations.info.
func (c *HTTPClient) Channel(ctx context.Context, id string) (string, error) {
	var r struct {
		Channel struct {
			Name string `json:"name"`
		} `json:"channel"`
	}
	if err := c.call(ctx, "conversations.info", url.Values{"channel": {id}}, &r); err != nil {
		return "", err
	}
	return r.Channel.Name, nil
}

// call issues a POST to {baseURL}/{method} with form-encoded params and the
// standard Slack browser-token auth headers, then decodes the JSON body
// into out (if non-nil) after checking the top-level "ok" field.
func (c *HTTPClient) call(ctx context.Context, method string, params url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+method,
		strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Cookie", "d="+c.cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var head struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return err
	}
	if !head.OK {
		switch head.Error {
		case "invalid_auth", "token_expired", "not_authed":
			return fmt.Errorf("%w: %s", ErrAuth, head.Error)
		default:
			return fmt.Errorf("slack error: %s", head.Error)
		}
	}

	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

// Replies fetches a thread's messages via conversations.replies and
// normalizes them into the domain Thread type.
func (c *HTTPClient) Replies(ctx context.Context, channel, threadTS string) (Thread, error) {
	var raw RepliesResponse
	err := c.call(ctx, "conversations.replies", url.Values{
		"channel": {channel},
		"ts":      {threadTS},
		"limit":   {"200"},
	}, &raw)
	if err != nil {
		return Thread{}, err
	}
	return NormalizeThread(channel, threadTS, raw), nil
}

// MarkRead marks a thread as read up through ts via
// subscriptions.thread.mark.
func (c *HTTPClient) MarkRead(ctx context.Context, channel, threadTS, ts string) error {
	return c.call(ctx, "subscriptions.thread.mark", url.Values{
		"channel":   {channel},
		"thread_ts": {threadTS},
		"ts":        {ts},
		"read":      {"1"},
	}, nil)
}

// MarkUnread marks a thread as unread from ts via subscriptions.thread.mark,
// setting the current user's last_read to just before ts.
func (c *HTTPClient) MarkUnread(ctx context.Context, channel, threadTS, ts string) error {
	return c.call(ctx, "subscriptions.thread.mark", url.Values{
		"channel":   {channel},
		"thread_ts": {threadTS},
		"ts":        {ts},
		"read":      {"0"},
	}, nil)
}

// PostReply posts a threaded reply via chat.postMessage and returns the
// created message normalized to the same domain Message shape used by
// Replies (chat.postMessage's response "message" field has the same raw
// shape as a message in conversations.replies).
func (c *HTTPClient) PostReply(ctx context.Context, channel, threadTS, text string) (Message, error) {
	var resp struct {
		Message rawMessage `json:"message"`
	}
	err := c.call(ctx, "chat.postMessage", url.Values{
		"channel":   {channel},
		"thread_ts": {threadTS},
		"text":      {text},
	}, &resp)
	if err != nil {
		return Message{}, err
	}
	return normalizeMessage(resp.Message), nil
}

// Users looks up each id via users.info and returns a map of id -> User.
func (c *HTTPClient) Users(ctx context.Context, ids []string) (map[string]User, error) {
	out := make(map[string]User, len(ids))
	for _, id := range ids {
		var r struct {
			User struct {
				ID       string `json:"id"`
				RealName string `json:"real_name"`
				Profile  struct {
					DisplayName        string `json:"display_name"`
					Image72            string `json:"image_72"`
					RealNameNormalized string `json:"real_name_normalized"`
				} `json:"profile"`
			} `json:"user"`
		}
		if err := c.call(ctx, "users.info", url.Values{"user": {id}}, &r); err != nil {
			out[id] = User{ID: id, RealName: id, DisplayName: id} // graceful fallback
			continue
		}
		out[id] = User{
			ID:          id,
			RealName:    r.User.RealName,
			DisplayName: r.User.Profile.DisplayName,
			Avatar72:    r.User.Profile.Image72,
		}
	}
	return out, nil
}

// Emoji fetches the workspace emoji list via emoji.list and dereferences
// one level of "alias:name" indirection so callers get real URLs.
func (c *HTTPClient) Emoji(ctx context.Context) (map[string]string, error) {
	var r struct {
		Emoji map[string]string `json:"emoji"`
	}
	if err := c.call(ctx, "emoji.list", url.Values{}, &r); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(r.Emoji))
	for k, v := range r.Emoji {
		if strings.HasPrefix(v, "alias:") {
			if real, ok := r.Emoji[strings.TrimPrefix(v, "alias:")]; ok {
				out[k] = real
				continue
			}
		}
		out[k] = v
	}
	return out, nil
}

// AddReaction adds the current user's reaction `name` (no colons) to the
// message at `ts` in `channel` via reactions.add. Slack's `already_reacted`
// error means the reaction is already present, which is the desired end state,
// so it is treated as success.
func (c *HTTPClient) AddReaction(ctx context.Context, channel, ts, name string) error {
	err := c.call(ctx, "reactions.add", url.Values{
		"channel":   {channel},
		"timestamp": {ts},
		"name":      {name},
	}, nil)
	if err != nil && strings.Contains(err.Error(), "already_reacted") {
		return nil
	}
	return err
}

// RemoveReaction removes the current user's reaction `name` from the message at
// `ts` via reactions.remove. Slack's `no_reaction` error means it was already
// absent (the desired end state), so it is treated as success.
func (c *HTTPClient) RemoveReaction(ctx context.Context, channel, ts, name string) error {
	err := c.call(ctx, "reactions.remove", url.Values{
		"channel":   {channel},
		"timestamp": {ts},
		"name":      {name},
	}, nil)
	if err != nil && strings.Contains(err.Error(), "no_reaction") {
		return nil
	}
	return err
}

// UserGroups returns the workspace's user groups keyed by subteam id.
//
// Callers should fetch this once and cache it (see the emoji map for the same
// pattern): the directory changes rarely, and a mention only needs the id to
// resolve. An error is returned rather than an empty map so callers can tell
// "lookup failed" from "workspace has no groups" — silently rendering ids in
// the first case would look identical to having no groups at all.
func (c *HTTPClient) UserGroups(ctx context.Context) (map[string]UserGroup, error) {
	var r struct {
		UserGroups []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Handle string `json:"handle"`
		} `json:"usergroups"`
	}
	if err := c.call(ctx, "usergroups.list", url.Values{}, &r); err != nil {
		return nil, err
	}
	out := make(map[string]UserGroup, len(r.UserGroups))
	for _, g := range r.UserGroups {
		out[g.ID] = UserGroup{ID: g.ID, Name: g.Name, Handle: g.Handle}
	}
	return out, nil
}

// teamIDOnce resolves and caches the team/enterprise id from auth.test. The
// edge cache API is per-org and carries that id in its URL path.
func (c *HTTPClient) teamIDOnce(ctx context.Context) (string, error) {
	c.teamIDMu.Lock()
	defer c.teamIDMu.Unlock()
	if c.teamID != "" {
		return c.teamID, nil
	}
	var r struct {
		TeamID string `json:"team_id"`
	}
	if err := c.call(ctx, "auth.test", url.Values{}, &r); err != nil {
		return "", err
	}
	if r.TeamID == "" {
		return "", fmt.Errorf("auth.test returned no team_id")
	}
	c.teamID = r.TeamID
	return r.TeamID, nil
}

// UserGroupsInfo resolves specific user groups by subteam id.
//
// This is what Slack's own web client uses, and it is the only thing that
// works on an Enterprise Grid org: usergroups.list enumerates and comes back
// empty there, and there is no public usergroups.info. See
// docs/reverse-engineering/slack-web-api.md in the worktree repo for how this
// was found.
//
// It differs from every other call in this client in three ways, which is why
// it does not go through call(): a different host (the per-org edge cache),
// a JSON body rather than form encoding, and the token carried in that body
// rather than as a bearer header.
func (c *HTTPClient) UserGroupsInfo(ctx context.Context, ids []string) (map[string]UserGroup, error) {
	out := make(map[string]UserGroup, len(ids))
	if len(ids) == 0 {
		return out, nil // nothing to resolve; do not make a pointless request
	}
	teamID, err := c.teamIDOnce(ctx)
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(map[string]any{
		"token": c.token, "ids": ids, "enterprise_token": c.token,
	})
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/cache/%s/usergroups/info?_x_app_name=client", c.edgeBaseURL, teamID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "d="+c.cookie)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var r struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Results []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Handle string `json:"handle"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if !r.OK {
		switch r.Error {
		case "invalid_auth", "token_expired", "not_authed":
			return nil, fmt.Errorf("%w: %s", ErrAuth, r.Error)
		default:
			return nil, fmt.Errorf("slack edge error: %s", r.Error)
		}
	}
	for _, g := range r.Results {
		out[g.ID] = UserGroup{ID: g.ID, Name: g.Name, Handle: g.Handle}
	}
	return out, nil
}
