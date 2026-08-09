// Package config reads and writes the watcher library's shared
// configuration file: service credentials, Jira custom field IDs, and a
// registry of consumer databases. See the design spec's "Shared
// Configuration" section for the YAML shape.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the top-level shape of the watcher config file.
type Config struct {
	Services         Services            `yaml:"services"`
	JiraCustomFields map[string]string   `yaml:"jira_custom_fields,omitempty"`
	ConsumerRegistry map[string]Consumer `yaml:"consumers,omitempty"`
}

// Services holds per-service credential blocks. Each is a pointer so an
// absent block can be distinguished from an empty one.
type Services struct {
	GitHub *GitHubConfig `yaml:"github,omitempty"`
	Jira   *JiraConfig   `yaml:"jira,omitempty"`
	Slack  *SlackConfig  `yaml:"slack,omitempty"`
}

// GitHubConfig contains GitHub API configuration.
type GitHubConfig struct {
	Token string `yaml:"token"`
}

// JiraConfig contains Jira API configuration. Note the YAML key is
// "host" (not "url" as in agent-handler's config) per the watcher
// library's design spec.
type JiraConfig struct {
	Host  string `yaml:"host"`
	Email string `yaml:"email"`
	Token string `yaml:"token"`
}

// SlackConfig contains Slack API configuration (future).
type SlackConfig struct {
	Token string `yaml:"token"`
}

// Consumer is an entry in the consumer DB registry.
type Consumer struct {
	DB string `yaml:"db"`
}

// GitHubCreds are the typed credentials returned by (*Config).GitHub.
type GitHubCreds struct {
	Token string
}

// JiraCreds are the typed credentials returned by (*Config).Jira.
type JiraCreds struct {
	Host         string
	Email        string
	Token        string
	CustomFields map[string]string
}

// SlackCreds are the typed credentials returned by (*Config).Slack.
type SlackCreds struct {
	Token string
}

// DefaultPath returns the default configuration file path,
// "~/.config/watcher/config.yaml". Honors WATCHER_HOME (used by tests)
// to override the base directory.
func DefaultPath() string {
	if dir := os.Getenv("WATCHER_HOME"); dir != "" {
		return filepath.Join(dir, "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "watcher", "config.yaml")
}

// Load reads and parses the config file at path.
//
// It refuses to load a config file whose permissions are group- or
// world-readable (mode & 0o077 != 0), since the file holds credentials.
// If the file does not exist, Load returns an empty, zero-value *Config
// (not an error) so callers can treat "no config yet" as the starting
// point for interactive setup.
func Load(path string) (*Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("failed to stat config %s: %w", path, err)
	}

	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("config %s is group/world-readable (%o); run: chmod 600 %s", path, perm, path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config %s: %w", path, err)
	}

	return &cfg, nil
}

// Save writes c to path as YAML, creating the parent directory (0700)
// if needed and writing the file itself with 0600 permissions, since it
// holds credentials.
func (c *Config) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create config dir %s: %w", dir, err)
	}

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("failed to write config %s: %w", path, err)
	}

	return nil
}

// GitHub returns GitHub credentials, or an error if GitHub is not
// configured.
func (c *Config) GitHub() (GitHubCreds, error) {
	if c.Services.GitHub == nil || c.Services.GitHub.Token == "" {
		return GitHubCreds{}, fmt.Errorf("github not configured")
	}
	return GitHubCreds{Token: c.Services.GitHub.Token}, nil
}

// Jira returns Jira credentials (including custom field IDs), or an
// error if Jira is not configured.
func (c *Config) Jira() (JiraCreds, error) {
	if c.Services.Jira == nil || c.Services.Jira.Token == "" {
		return JiraCreds{}, fmt.Errorf("jira not configured")
	}
	return JiraCreds{
		Host:         c.Services.Jira.Host,
		Email:        c.Services.Jira.Email,
		Token:        c.Services.Jira.Token,
		CustomFields: c.JiraCustomFields,
	}, nil
}

// Slack returns Slack credentials, or an error if Slack is not
// configured. Slack support is a stub for now.
func (c *Config) Slack() (SlackCreds, error) {
	if c.Services.Slack == nil || c.Services.Slack.Token == "" {
		return SlackCreds{}, fmt.Errorf("slack not configured")
	}
	return SlackCreds{Token: c.Services.Slack.Token}, nil
}

// RegisterConsumer adds or updates a consumer's DB path in the
// registry.
func (c *Config) RegisterConsumer(name, dbPath string) {
	if c.ConsumerRegistry == nil {
		c.ConsumerRegistry = make(map[string]Consumer)
	}
	c.ConsumerRegistry[name] = Consumer{DB: dbPath}
}

// Consumers returns a map of consumer name to DB path.
func (c *Config) Consumers() map[string]string {
	out := make(map[string]string, len(c.ConsumerRegistry))
	for name, consumer := range c.ConsumerRegistry {
		out[name] = consumer.DB
	}
	return out
}
