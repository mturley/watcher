package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveLoadRoundTripGitHubToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.yaml")

	cfg := &Config{
		Services: Services{
			GitHub: &GitHubConfig{Token: "ghp_test123"},
		},
	}

	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected mode 0600, got %o", perm)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	creds, err := loaded.GitHub()
	if err != nil {
		t.Fatalf("GitHub(): %v", err)
	}
	if creds.Token != "ghp_test123" {
		t.Fatalf("expected token ghp_test123, got %q", creds.Token)
	}
}

func TestJiraNotConfigured(t *testing.T) {
	cfg := &Config{}
	if _, err := cfg.Jira(); err == nil {
		t.Fatal("expected error for unconfigured jira, got nil")
	}
}

func TestLoadRefusesGroupWorldReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.yaml")

	if err := os.WriteFile(path, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for group/world-readable config, got nil")
	}
	if !strings.Contains(err.Error(), "chmod") {
		t.Fatalf("expected error to mention chmod, got: %v", err)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg == nil || cfg.Services.GitHub != nil || cfg.Services.Jira != nil || cfg.Services.Slack != nil {
		t.Fatalf("expected an empty Config, got %+v", cfg)
	}
}

func TestSlackReturnsTokenCookieAndDomain(t *testing.T) {
	c := &Config{Services: Services{Slack: &SlackConfig{
		Token: "xoxc-1", Cookie: "xoxd-2", WorkspaceDomain: "acme.slack.com",
	}}}
	creds, err := c.Slack()
	if err != nil {
		t.Fatal(err)
	}
	if creds.Token != "xoxc-1" || creds.Cookie != "xoxd-2" || creds.WorkspaceDomain != "acme.slack.com" {
		t.Fatalf("got %+v", creds)
	}
}

func TestSlackNotConfiguredWithoutCookie(t *testing.T) {
	c := &Config{Services: Services{Slack: &SlackConfig{Token: "xoxc-1"}}} // no cookie
	if _, err := c.Slack(); err == nil {
		t.Fatal("expected error when cookie missing")
	}
}
