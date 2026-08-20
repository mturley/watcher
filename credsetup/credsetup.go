// Package credsetup holds the shared "test and repair" flow for service
// credentials: validate what's configured, and if it's missing or rejected,
// prompt the operator for new credentials and re-validate. It is the only
// place in the watcher library with prompting-shaped code — the core
// packages (config, github, jira, slack) stay pure and know nothing about
// interactive setup. Consumers (worktree, agent-handler) implement Prompter
// however suits their UI (terminal prompts, web forms, etc.) and call
// TestAndRepair.
package credsetup

import (
	"context"
	"errors"
	"fmt"

	"github.com/mturley/watcher/config"
	wgithub "github.com/mturley/watcher/github"
	wjira "github.com/mturley/watcher/jira"
	wslack "github.com/mturley/watcher/slack"
)

// Service identifies which service's credentials to test and repair.
type Service string

const (
	GitHub Service = "github"
	Jira   Service = "jira"
	Slack  Service = "slack"
)

// Prompter is implemented by consumers to drive interactive credential
// setup. Info reports status messages, Confirm asks a yes/no question,
// PromptToken asks for a single token/secret for a service, and PromptSlack
// asks for the Slack-specific token+cookie pair.
type Prompter interface {
	Info(msg string)
	Confirm(msg string) bool
	PromptToken(service Service, instructions string) string
	PromptSlack(instructions string) (token, cookie string)
}

// Test seam: overridable in tests so TestAndRepair needs no network.
var (
	validateGitHub = wgithub.Validate
	validateJira   = wjira.Validate
	validateSlack  = wslack.Validate
	// slackDomain resolves the workspace host for a valid slack cred; returns
	// "" on error (domain is best-effort, not required for validity).
	slackDomain = func(token, cookie string) string {
		d, err := wslack.New(token, cookie).TeamInfo(context.Background())
		if err != nil {
			return ""
		}
		return d
	}
)

// TestAndRepair tests the configured credentials for svc and, if they are
// missing or rejected, prompts the operator (via p) for replacements and
// re-validates them. It mutates cfg in place on success but does not save
// it — the caller is responsible for persisting cfg after a successful
// repair. changed reports whether cfg was modified.
func TestAndRepair(cfg *config.Config, svc Service, p Prompter) (bool, error) {
	switch svc {
	case GitHub:
		return repairGitHub(cfg, p)
	case Jira:
		return repairJira(cfg, p)
	case Slack:
		return repairSlack(cfg, p)
	default:
		return false, fmt.Errorf("unknown service %q", svc)
	}
}

func repairGitHub(cfg *config.Config, p Prompter) (bool, error) {
	creds, cfgErr := cfg.GitHub()
	configured := cfgErr == nil
	if configured {
		p.Info("Testing GitHub credentials...")
		err := validateGitHub(creds.Token)
		if err == nil {
			p.Info("GitHub: ok")
			return false, nil
		}
		if !errors.Is(err, wgithub.ErrAuth) {
			return false, err // transport/other: surface, do not prompt
		}
		p.Info("GitHub: failed (" + err.Error() + ")")
		if !p.Confirm("Replace the GitHub token?") {
			return false, nil
		}
	} else {
		if !p.Confirm("Configure GitHub?") {
			p.Info("GitHub: skipped")
			return false, nil
		}
	}

	tok := p.PromptToken(GitHub, "Create a token at https://github.com/settings/tokens (needs repo/read scopes)")
	if tok == "" {
		return false, nil
	}
	if err := validateGitHub(tok); err != nil {
		p.Info("GitHub: new token invalid (" + err.Error() + ")")
		return false, nil // one attempt, then give up
	}
	if cfg.Services.GitHub == nil {
		cfg.Services.GitHub = &config.GitHubConfig{}
	}
	cfg.Services.GitHub.Token = tok
	p.Info("GitHub: ok")
	return true, nil
}

func repairJira(cfg *config.Config, p Prompter) (bool, error) {
	creds, cfgErr := cfg.Jira()
	configured := cfgErr == nil

	// Jira repair only ever replaces the token; host/email are reused from
	// the existing config. If Jira isn't configured at all, there's no
	// host/email to reuse, so point the operator at the consumer's full
	// Jira setup instead of building a multi-field prompt here.
	host, email := creds.Host, creds.Email
	if !configured {
		if !p.Confirm("Configure Jira?") {
			p.Info("Jira: skipped")
			return false, nil
		}
		if cfg.Services.Jira == nil || cfg.Services.Jira.Host == "" || cfg.Services.Jira.Email == "" {
			p.Info("Jira is not configured. Run the full Jira setup to provide a host and email before repairing the token.")
			return false, nil
		}
		host, email = cfg.Services.Jira.Host, cfg.Services.Jira.Email
	} else {
		p.Info("Testing Jira credentials...")
		err := validateJira(host, email, creds.Token)
		if err == nil {
			p.Info("Jira: ok")
			return false, nil
		}
		if !errors.Is(err, wjira.ErrAuth) {
			return false, err // transport/other: surface, do not prompt
		}
		p.Info("Jira: failed (" + err.Error() + ")")
		if !p.Confirm("Replace the Jira token?") {
			return false, nil
		}
	}

	tok := p.PromptToken(Jira, "Create an API token at https://id.atlassian.com/manage-profile/security/api-tokens")
	if tok == "" {
		return false, nil
	}
	if err := validateJira(host, email, tok); err != nil {
		p.Info("Jira: new token invalid (" + err.Error() + ")")
		return false, nil // one attempt, then give up
	}
	if cfg.Services.Jira == nil {
		cfg.Services.Jira = &config.JiraConfig{}
	}
	cfg.Services.Jira.Host = host
	cfg.Services.Jira.Email = email
	cfg.Services.Jira.Token = tok
	p.Info("Jira: ok")
	return true, nil
}

func repairSlack(cfg *config.Config, p Prompter) (bool, error) {
	creds, cfgErr := cfg.Slack()
	configured := cfgErr == nil
	if configured {
		p.Info("Testing Slack credentials...")
		err := validateSlack(creds.Token, creds.Cookie)
		if err == nil {
			p.Info("Slack: ok")
			return false, nil
		}
		if !errors.Is(err, wslack.ErrAuth) {
			return false, err // transport/other: surface, do not prompt
		}
		p.Info("Slack: failed (" + err.Error() + ")")
		if !p.Confirm("Replace the Slack token+cookie?") {
			return false, nil
		}
	} else {
		if !p.Confirm("Configure Slack?") {
			p.Info("Slack: skipped")
			return false, nil
		}
	}

	tok, cookie := p.PromptSlack("Extract your Slack session token (xoxc-...) and d= cookie (xoxd-...) from a logged-in browser session")
	if tok == "" || cookie == "" {
		return false, nil
	}
	if err := validateSlack(tok, cookie); err != nil {
		p.Info("Slack: new credentials invalid (" + err.Error() + ")")
		return false, nil // one attempt, then give up
	}
	cfg.Services.Slack = &config.SlackConfig{
		Token:           tok,
		Cookie:          cookie,
		WorkspaceDomain: slackDomain(tok, cookie),
	}
	p.Info("Slack: ok")
	return true, nil
}
