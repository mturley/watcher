package credsetup

import (
	"testing"

	"github.com/mturley/watcher/config"
	wgithub "github.com/mturley/watcher/github"
	wjira "github.com/mturley/watcher/jira"
)

// fakePrompter records calls and returns scripted responses.
type fakePrompter struct {
	confirmResult bool
	tokenResults  []string // successive PromptToken return values
	tokenCalls    int

	slackTokenResult  string // PromptSlack token return value
	slackCookieResult string // PromptSlack cookie return value
	slackCalls        int

	infoCalls    []string
	confirmCalls []string
	promptCalls  []Service
}

func (f *fakePrompter) Info(msg string) {
	f.infoCalls = append(f.infoCalls, msg)
}

func (f *fakePrompter) Confirm(msg string) bool {
	f.confirmCalls = append(f.confirmCalls, msg)
	return f.confirmResult
}

func (f *fakePrompter) PromptToken(service Service, instructions string) string {
	f.promptCalls = append(f.promptCalls, service)
	if f.tokenCalls >= len(f.tokenResults) {
		return ""
	}
	tok := f.tokenResults[f.tokenCalls]
	f.tokenCalls++
	return tok
}

func (f *fakePrompter) PromptSlack(instructions string) (string, string) {
	f.slackCalls++
	return f.slackTokenResult, f.slackCookieResult
}

// withGitHubSeam overrides validateGitHub for the duration of the test.
func withGitHubSeam(t *testing.T, fn func(token string, apiURL ...string) error) {
	t.Helper()
	orig := validateGitHub
	validateGitHub = fn
	t.Cleanup(func() { validateGitHub = orig })
}

// withJiraSeam overrides validateJira for the duration of the test.
func withJiraSeam(t *testing.T, fn func(host, email, token string) error) {
	t.Helper()
	orig := validateJira
	validateJira = fn
	t.Cleanup(func() { validateJira = orig })
}

// withSlackSeam overrides validateSlack and slackDomain for the duration of
// the test.
func withSlackSeam(t *testing.T, validate func(token, cookie string) error, domain func(token, cookie string) string) {
	t.Helper()
	origValidate := validateSlack
	origDomain := slackDomain
	validateSlack = func(token, cookie string, baseURL ...string) error {
		return validate(token, cookie)
	}
	slackDomain = domain
	t.Cleanup(func() {
		validateSlack = origValidate
		slackDomain = origDomain
	})
}

func TestTestAndRepair_GitHub_ValidFirstTry(t *testing.T) {
	withGitHubSeam(t, func(token string, apiURL ...string) error { return nil })

	cfg := &config.Config{Services: config.Services{GitHub: &config.GitHubConfig{Token: "good"}}}
	p := &fakePrompter{}

	changed, err := TestAndRepair(cfg, GitHub, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Fatalf("expected changed=false")
	}
	if len(p.confirmCalls) != 0 || len(p.promptCalls) != 0 {
		t.Fatalf("expected no prompt calls, got confirm=%v prompt=%v", p.confirmCalls, p.promptCalls)
	}
	if cfg.Services.GitHub.Token != "good" {
		t.Fatalf("token should be unchanged")
	}
}

func TestTestAndRepair_GitHub_ErrAuthConfirmYesNewTokenValid(t *testing.T) {
	calls := 0
	withGitHubSeam(t, func(token string, apiURL ...string) error {
		calls++
		if token == "bad" {
			return wgithub.ErrAuth
		}
		return nil
	})

	cfg := &config.Config{Services: config.Services{GitHub: &config.GitHubConfig{Token: "bad"}}}
	p := &fakePrompter{confirmResult: true, tokenResults: []string{"new-good-token"}}

	changed, err := TestAndRepair(cfg, GitHub, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true")
	}
	if cfg.Services.GitHub.Token != "new-good-token" {
		t.Fatalf("expected token updated, got %q", cfg.Services.GitHub.Token)
	}
	if calls != 2 {
		t.Fatalf("expected 2 validate calls (initial + retest), got %d", calls)
	}
}

func TestTestAndRepair_GitHub_ErrAuthConfirmNo(t *testing.T) {
	withGitHubSeam(t, func(token string, apiURL ...string) error { return wgithub.ErrAuth })

	cfg := &config.Config{Services: config.Services{GitHub: &config.GitHubConfig{Token: "bad"}}}
	p := &fakePrompter{confirmResult: false}

	changed, err := TestAndRepair(cfg, GitHub, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Fatalf("expected changed=false")
	}
	if cfg.Services.GitHub.Token != "bad" {
		t.Fatalf("cfg should be unchanged")
	}
	if len(p.promptCalls) != 0 {
		t.Fatalf("expected no PromptToken call after declining confirm")
	}
}

func TestTestAndRepair_GitHub_NewTokenAlsoInvalid(t *testing.T) {
	withGitHubSeam(t, func(token string, apiURL ...string) error { return wgithub.ErrAuth })

	cfg := &config.Config{Services: config.Services{GitHub: &config.GitHubConfig{Token: "bad"}}}
	p := &fakePrompter{confirmResult: true, tokenResults: []string{"still-bad"}}

	changed, err := TestAndRepair(cfg, GitHub, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Fatalf("expected changed=false after one failed retry")
	}
	if cfg.Services.GitHub.Token != "bad" {
		t.Fatalf("cfg should be unchanged, got %q", cfg.Services.GitHub.Token)
	}
	if len(p.promptCalls) != 1 {
		t.Fatalf("expected exactly one PromptToken call (single retry), got %d", len(p.promptCalls))
	}
}

func TestTestAndRepair_GitHub_TransportErrorSurfaced(t *testing.T) {
	transportErr := &transportError{msg: "network unreachable"}
	withGitHubSeam(t, func(token string, apiURL ...string) error { return transportErr })

	cfg := &config.Config{Services: config.Services{GitHub: &config.GitHubConfig{Token: "whatever"}}}
	p := &fakePrompter{}

	changed, err := TestAndRepair(cfg, GitHub, p)
	if err != transportErr {
		t.Fatalf("expected transport error to be returned, got %v", err)
	}
	if changed {
		t.Fatalf("expected changed=false")
	}
	if len(p.confirmCalls) != 0 || len(p.promptCalls) != 0 {
		t.Fatalf("expected no prompt calls on transport error")
	}
}

func TestTestAndRepair_Jira_HostEmailReusedTokenRepaired(t *testing.T) {
	withJiraSeam(t, func(host, email, token string) error {
		if token == "old" {
			return wjira.ErrAuth
		}
		return nil
	})

	cfg := &config.Config{Services: config.Services{Jira: &config.JiraConfig{
		Host:  "https://x.atlassian.net",
		Email: "a@b.c",
		Token: "old",
	}}}
	p := &fakePrompter{confirmResult: true, tokenResults: []string{"new-good-token"}}

	changed, err := TestAndRepair(cfg, Jira, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true")
	}
	if cfg.Services.Jira.Token != "new-good-token" {
		t.Fatalf("expected token updated, got %q", cfg.Services.Jira.Token)
	}
	if cfg.Services.Jira.Host != "https://x.atlassian.net" {
		t.Fatalf("expected host unchanged, got %q", cfg.Services.Jira.Host)
	}
	if cfg.Services.Jira.Email != "a@b.c" {
		t.Fatalf("expected email unchanged, got %q", cfg.Services.Jira.Email)
	}
	if len(p.promptCalls) != 1 {
		t.Fatalf("expected exactly one PromptToken call, got %d", len(p.promptCalls))
	}
}

func TestTestAndRepair_Slack_UnconfiguredSetsWorkspaceDomain(t *testing.T) {
	withSlackSeam(t,
		func(token, cookie string) error { return nil },
		func(token, cookie string) string { return "acme.slack.com" },
	)

	cfg := &config.Config{}
	p := &fakePrompter{slackTokenResult: "tok", slackCookieResult: "cookie"}

	changed, err := TestAndRepair(cfg, Slack, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true")
	}
	if cfg.Services.Slack == nil {
		t.Fatalf("expected Slack config to be set")
	}
	if cfg.Services.Slack.Token != "tok" {
		t.Fatalf("expected token=tok, got %q", cfg.Services.Slack.Token)
	}
	if cfg.Services.Slack.Cookie != "cookie" {
		t.Fatalf("expected cookie=cookie, got %q", cfg.Services.Slack.Cookie)
	}
	if cfg.Services.Slack.WorkspaceDomain != "acme.slack.com" {
		t.Fatalf("expected WorkspaceDomain=acme.slack.com, got %q", cfg.Services.Slack.WorkspaceDomain)
	}
}

type transportError struct{ msg string }

func (e *transportError) Error() string { return e.msg }
