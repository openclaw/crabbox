package githubcodespaces

import (
	"context"
	"fmt"
	"strings"
)

type ghRunner struct {
	cfg GitHubCodespacesConfig
	rt  Runtime
}

func newGHRunner(cfg GitHubCodespacesConfig, rt Runtime) ghRunner {
	return ghRunner{cfg: cfg, rt: rt}
}

func (r ghRunner) authStatus(ctx context.Context) error {
	_, err := r.run(ctx, "auth", "status")
	return err
}

func (r ghRunner) authToken(ctx context.Context) (string, error) {
	result, err := r.run(ctx, "auth", "token")
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(result.Stdout)
	if token == "" {
		return "", fmt.Errorf("github-codespaces gh auth token returned empty token")
	}
	return token, nil
}

func (r ghRunner) userLogin(ctx context.Context) (string, error) {
	result, err := r.run(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	login := strings.TrimSpace(result.Stdout)
	if login == "" {
		return "", fmt.Errorf("github-codespaces gh api user returned empty login")
	}
	return login, nil
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
	result, err := r.rt.Exec.Run(ctx, LocalCommandRequest{Name: name, Args: args})
	if err != nil {
		return result, fmt.Errorf("github-codespaces gh %s failed: %s", strings.Join(redactGHArgs(args), " "), redactSecretText(result.Stderr+" "+err.Error()))
	}
	if result.ExitCode != 0 {
		return result, fmt.Errorf("github-codespaces gh %s failed with exit=%d: %s", strings.Join(redactGHArgs(args), " "), result.ExitCode, redactSecretText(result.Stderr))
	}
	return result, nil
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
	fields := strings.Fields(text)
	for i, field := range fields {
		if looksLikeGitHubToken(field) {
			fields[i] = "<redacted>"
		}
	}
	return strings.Join(fields, " ")
}

func looksLikeGitHubToken(value string) bool {
	value = strings.Trim(value, `"'.,;:()[]{}<>`)
	for _, prefix := range []string{"ghp_", "github_pat_", "gho_", "ghu_", "ghs_", "ghr_"} {
		if strings.HasPrefix(value, prefix) && len(value) >= len(prefix)+12 {
			return true
		}
	}
	return false
}
