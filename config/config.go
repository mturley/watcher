// Package config defines the format of the credentials file (service
// tokens for GitHub, Jira and Slack) and the behavior types (bot
// usernames, Jira custom fields) that consumers embed in their own
// config files. The library never chooses a file location: every
// consumer passes the path it owns to Load and Save.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the top-level shape of the credentials file.
type Config struct {
	Services Services `yaml:"services"`
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

// SlackConfig contains Slack API credentials. Slack uses a browser user
// session: a token (xoxc-) plus the d= cookie (xoxd-). WorkspaceDomain is
// resolved at setup time (via team.info) and used to build permalinks.
type SlackConfig struct {
	Token           string `yaml:"token"`
	Cookie          string `yaml:"cookie"`
	WorkspaceDomain string `yaml:"workspace_domain,omitempty"`
}

// GitHubCreds are the typed credentials returned by (*Config).GitHub.
type GitHubCreds struct {
	Token string
}

// JiraCreds are the typed credentials returned by (*Config).Jira.
type JiraCreds struct {
	Host  string
	Email string
	Token string
}

// SlackCreds are the typed credentials returned by (*Config).Slack.
type SlackCreds struct {
	Token           string
	Cookie          string
	WorkspaceDomain string
}

// GitHubBehavior holds non-auth GitHub behavior settings.
type GitHubBehavior struct {
	// BotUsernames are glob patterns (see botmatch) for logins to classify
	// as bots although GitHub reports them as users, e.g. "*ci-bot*".
	BotUsernames []string `yaml:"bot_usernames,omitempty"`
}

// JiraBehavior holds non-auth Jira behavior settings.
type JiraBehavior struct {
	// BotUsernames lists Jira usernames to classify as bots when
	// attributing comment/changelog authors: glob patterns (see botmatch),
	// so a name without wildcards is an exact, case-insensitive match.
	BotUsernames []string `yaml:"bot_usernames,omitempty"`
	// CustomFields maps friendly names (e.g. "epic_key", "story_points") to
	// Jira custom field IDs (e.g. "customfield_10014"), letting the poller
	// fetch and cache extra issue data. These are configuration, not
	// credentials.
	CustomFields map[string]string `yaml:"custom_fields,omitempty"`
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

// Jira returns Jira credentials, or an
// error if Jira is not configured.
func (c *Config) Jira() (JiraCreds, error) {
	if c.Services.Jira == nil || c.Services.Jira.Token == "" {
		return JiraCreds{}, fmt.Errorf("jira not configured")
	}
	return JiraCreds{
		Host:  c.Services.Jira.Host,
		Email: c.Services.Jira.Email,
		Token: c.Services.Jira.Token,
	}, nil
}

// Slack returns Slack credentials, or an error if Slack is not fully
// configured (both token and cookie are required).
func (c *Config) Slack() (SlackCreds, error) {
	if c.Services.Slack == nil || c.Services.Slack.Token == "" || c.Services.Slack.Cookie == "" {
		return SlackCreds{}, fmt.Errorf("slack not configured")
	}
	return SlackCreds{
		Token:           c.Services.Slack.Token,
		Cookie:          c.Services.Slack.Cookie,
		WorkspaceDomain: c.Services.Slack.WorkspaceDomain,
	}, nil
}
