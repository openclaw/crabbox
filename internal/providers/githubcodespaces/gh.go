package githubcodespaces

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

var githubTokenPattern = regexp.MustCompile(`(?i)(?:github_pat_[a-z0-9_]{12,}|gh[opur]_[a-z0-9_]{12,}|ghs_[a-z0-9._-]{36,})`)

type ghRunner struct {
	cfg GitHubCodespacesConfig
	rt  Runtime
}

func newGHRunner(cfg GitHubCodespacesConfig, rt Runtime) ghRunner {
	return ghRunner{cfg: cfg, rt: rt}
}

func (r ghRunner) authStatus(ctx context.Context) error {
	host, err := r.apiHostname()
	if err != nil {
		return err
	}
	_, err = r.run(ctx, "auth", "status", "--active", "--hostname", host)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unknown flag") && strings.Contains(err.Error(), "--active") {
		_, err = r.run(ctx, "auth", "status", "--hostname", host)
	}
	return err
}

func (r ghRunner) authToken(ctx context.Context) (string, error) {
	host, err := r.apiHostname()
	if err != nil {
		return "", err
	}
	result, err := r.run(ctx, "auth", "token", "--hostname", host)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(result.Stdout)
	if token == "" {
		return "", fmt.Errorf("github-codespaces gh auth token returned empty token")
	}
	return token, nil
}

func (r ghRunner) codespaceSSHConfig(ctx context.Context, codespace string) (string, error) {
	result, err := r.run(ctx, "codespace", "ssh", "--config", "-c", strings.TrimSpace(codespace))
	if err != nil {
		return "", err
	}
	return result.Stdout, nil
}

func (r ghRunner) run(ctx context.Context, args ...string) (LocalCommandResult, error) {
	if r.rt.Exec == nil {
		return LocalCommandResult{}, exit(2, "provider=github-codespaces requires local command runner")
	}
	name := strings.TrimSpace(r.cfg.GHPath)
	if name == "" {
		name = defaultGHPath
	}
	host, err := r.apiHostname()
	if err != nil {
		return LocalCommandResult{}, err
	}
	dotcomTokens := githubCodespacesUsesDotcomTokenEnv(r.cfg)
	selectedToken := githubCodespacesTokenFromEnv(r.cfg)
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "GH_HOST=") {
			continue
		}
		if strings.HasPrefix(entry, "GH_TOKEN=") || strings.HasPrefix(entry, "GITHUB_TOKEN=") || strings.HasPrefix(entry, "GH_ENTERPRISE_TOKEN=") || strings.HasPrefix(entry, "GITHUB_ENTERPRISE_TOKEN=") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "GH_HOST="+host)
	if selectedToken != "" {
		name := "GH_ENTERPRISE_TOKEN"
		if dotcomTokens {
			name = "GH_TOKEN"
		}
		env = append(env, name+"="+selectedToken)
	}
	result, err := r.rt.Exec.Run(ctx, LocalCommandRequest{Name: name, Args: args, Env: env})
	if err != nil {
		return result, fmt.Errorf("github-codespaces gh %s failed: %s", strings.Join(redactGHArgs(args), " "), redactSecretText(result.Stderr+" "+err.Error()))
	}
	if result.ExitCode != 0 {
		return result, fmt.Errorf("github-codespaces gh %s failed with exit=%d: %s", strings.Join(redactGHArgs(args), " "), result.ExitCode, redactSecretText(result.Stderr))
	}
	return result, nil
}

func (r ghRunner) apiHostname() (string, error) {
	raw := strings.TrimSpace(r.cfg.APIURL)
	if raw == "" {
		raw = defaultAPIURL
	}
	if err := validateGitHubCodespacesAPIBase(raw); err != nil {
		return "", err
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	host := strings.TrimSpace(parsed.Host)
	if strings.EqualFold(parsed.Hostname(), "api.github.com") && (parsed.Port() == "" || parsed.Port() == "443") {
		host = "github.com"
	} else if strings.HasPrefix(strings.ToLower(parsed.Hostname()), "api.") && strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".ghe.com") {
		host = strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "api.")
		if parsed.Port() != "" && parsed.Port() != "443" {
			host += ":" + parsed.Port()
		}
	}
	if host == "" {
		return "", exit(2, "github-codespaces API URL has no hostname")
	}
	return host, nil
}

func githubCodespacesUsesDotcomTokenEnv(cfg GitHubCodespacesConfig) bool {
	raw := strings.TrimSpace(cfg.APIURL)
	if raw == "" {
		raw = defaultAPIURL
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "github.com" || host == "api.github.com" || host == "ghe.com" || strings.HasSuffix(host, ".ghe.com")
}

func redactGHArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		lower := strings.ToLower(arg)
		if lower == "--token" || lower == "--api-key" || lower == "-t" {
			out = append(out, arg, "<redacted>")
			i++
			continue
		}
		if strings.HasPrefix(lower, "--token=") || strings.HasPrefix(lower, "--api-key=") {
			before, _, _ := strings.Cut(arg, "=")
			out = append(out, before+"=<redacted>")
			continue
		}
		out = append(out, redactSecretText(arg))
	}
	return out
}

func redactSecretText(text string) string {
	return githubTokenPattern.ReplaceAllString(text, "<redacted>")
}

func looksLikeGitHubToken(value string) bool {
	return githubTokenPattern.MatchString(value)
}
