package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveLoadRoundTripGitHubToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

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
	path := filepath.Join(dir, "config.yaml")

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
	path := filepath.Join(dir, "config.yaml")
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
