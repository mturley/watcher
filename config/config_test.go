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

func TestDefaultPathUsesAuthYAML(t *testing.T) {
	t.Setenv("WATCHER_HOME", "/tmp/wh")
	if got := DefaultPath(); got != "/tmp/wh/auth.yaml" {
		t.Fatalf("DefaultPath = %q, want /tmp/wh/auth.yaml", got)
	}
}

func TestLoadConfigMissingFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil ConfigFile")
	}
	if cfg.Jira != nil {
		t.Fatalf("expected nil Jira behavior block, got %+v", cfg.Jira)
	}
	if got := cfg.JiraBotUsernames(); got != nil {
		t.Fatalf("expected nil bot usernames, got %v", got)
	}
}

func TestLoadConfigJiraBotUsernames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	yamlContent := "jira:\n  bot_usernames:\n    - bot1\n    - bot2\n"
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Jira == nil {
		t.Fatal("expected non-nil Jira behavior block")
	}
	want := []string{"bot1", "bot2"}
	if strings.Join(cfg.Jira.BotUsernames, ",") != strings.Join(want, ",") {
		t.Fatalf("Jira.BotUsernames = %v, want %v", cfg.Jira.BotUsernames, want)
	}
	if got := cfg.JiraBotUsernames(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("JiraBotUsernames() = %v, want %v", got, want)
	}
}

func TestLoadConfigIgnoresWorldReadablePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	yamlContent := "jira:\n  bot_usernames:\n    - bot1\n    - bot2\n"
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: expected no error for world-readable config.yaml, got %v", err)
	}
	want := []string{"bot1", "bot2"}
	if got := cfg.JiraBotUsernames(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("JiraBotUsernames() = %v, want %v", got, want)
	}
}

func TestLoadConfigSurfacesParseError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(path, []byte("jira: [not-a-map]\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error for malformed config.yaml, got nil")
	}
}

func TestConfigDefaultPathHonorsWatcherHome(t *testing.T) {
	t.Setenv("WATCHER_HOME", "/tmp/wh")
	if got := ConfigDefaultPath(); got != "/tmp/wh/config.yaml" {
		t.Fatalf("ConfigDefaultPath = %q, want /tmp/wh/config.yaml", got)
	}
}

func TestRegisterConsumerRoundTrip(t *testing.T) {
	cfg := &Config{}
	cfg.RegisterConsumer("handler", "~/.agent-handler/handler.db")
	cfg.RegisterConsumer("worktree", "~/.config/worktree/worktree.db")

	consumers := cfg.Consumers()
	if len(consumers) != 2 {
		t.Fatalf("expected 2 consumers, got %d", len(consumers))
	}
	if consumers["handler"] != "~/.agent-handler/handler.db" {
		t.Fatalf("unexpected handler db path: %q", consumers["handler"])
	}
	if consumers["worktree"] != "~/.config/worktree/worktree.db" {
		t.Fatalf("unexpected worktree db path: %q", consumers["worktree"])
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "auth.yaml")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	loadedConsumers := loaded.Consumers()
	if loadedConsumers["handler"] != "~/.agent-handler/handler.db" {
		t.Fatalf("round-tripped handler db path mismatch: %q", loadedConsumers["handler"])
	}
	if loadedConsumers["worktree"] != "~/.config/worktree/worktree.db" {
		t.Fatalf("round-tripped worktree db path mismatch: %q", loadedConsumers["worktree"])
	}
}
