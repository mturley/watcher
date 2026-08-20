package credsetup

import (
	"testing"

	"github.com/mturley/watcher/config"
	wgithub "github.com/mturley/watcher/github"
)

// fakePrompter records calls and returns scripted responses.
type fakePrompter struct {
	confirmResult bool
	tokenResults  []string // successive PromptToken return values
	tokenCalls    int

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
	return "", ""
}

// withGitHubSeam overrides validateGitHub for the duration of the test.
func withGitHubSeam(t *testing.T, fn func(token string, apiURL ...string) error) {
	t.Helper()
	orig := validateGitHub
	validateGitHub = fn
	t.Cleanup(func() { validateGitHub = orig })
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

type transportError struct{ msg string }

func (e *transportError) Error() string { return e.msg }
