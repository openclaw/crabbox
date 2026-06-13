package vercelsandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type sandboxSummary struct {
	ID string
}

type vercelSandboxClient interface {
	CheckSDK(ctx context.Context) error
	CheckCLI(ctx context.Context) (string, error)
	CheckAuth(ctx context.Context) error
	CheckProject(ctx context.Context) error
	ListSandboxes(ctx context.Context) ([]sandboxSummary, error)
}

type commandSpec struct {
	Name string
	Args []string
	Env  []string
}

type bridgeClient struct {
	cfg    Config
	rt     Runtime
	lookup func(string) (string, error)
	run    func(context.Context, commandSpec) error
}

func newBridgeClient(cfg Config, rt Runtime) (vercelSandboxClient, error) {
	return &bridgeClient{
		cfg:    cfg,
		rt:     rt,
		lookup: exec.LookPath,
		run:    runBridgeCommand,
	}, nil
}

func (c *bridgeClient) CheckSDK(context.Context) error {
	// PLAN-01 establishes the SDK boundary. PLAN-02 owns the concrete Node SDK
	// bridge process, so foundation doctor reports this as a contract check.
	return nil
}

func (c *bridgeClient) CheckCLI(context.Context) (string, error) {
	path, err := c.lookup("sandbox")
	if err != nil {
		return "", fmt.Errorf("sandbox CLI unavailable: %w", err)
	}
	return path, nil
}

func (c *bridgeClient) CheckAuth(ctx context.Context) error {
	spec := c.sandboxListCommand()
	if err := c.run(ctx, spec); err != nil {
		return fmt.Errorf("sandbox auth/readiness check failed: %s", redactSecrets(err.Error()))
	}
	return nil
}

func (c *bridgeClient) CheckProject(context.Context) error {
	if strings.TrimSpace(c.cfg.VercelSandbox.ProjectID) == "" && strings.TrimSpace(c.cfg.VercelSandbox.Scope) == "" && strings.TrimSpace(c.cfg.VercelSandbox.TeamID) == "" {
		return errors.New("set projectId, teamId, or scope for project-scoped readiness")
	}
	return nil
}

func (c *bridgeClient) ListSandboxes(context.Context) ([]sandboxSummary, error) {
	claims, err := listVercelSandboxLeaseClaims()
	if err != nil {
		return nil, err
	}
	out := make([]sandboxSummary, 0, len(claims))
	for _, claim := range claims {
		if claim.Provider == providerName && strings.HasPrefix(claim.LeaseID, leasePrefix) {
			out = append(out, sandboxSummary{ID: claim.LeaseID})
		}
	}
	return out, nil
}

func (c *bridgeClient) sandboxListCommand() commandSpec {
	return commandSpec{
		Name: "sandbox",
		Args: []string{"list", "--all", "--limit", "1"},
		Env:  vercelSandboxBridgeEnv(os.Environ()),
	}
}

func runBridgeCommand(ctx context.Context, spec commandSpec) error {
	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...)
	cmd.Env = spec.Env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, redactSecrets(string(out)))
	}
	return nil
}

func vercelSandboxBridgeEnv(base []string) []string {
	env := append([]string{}, base...)
	if token := firstEnv("CRABBOX_VERCEL_SANDBOX_AUTH_TOKEN", "CRABBOX_VERCEL_AUTH_TOKEN", "VERCEL_AUTH_TOKEN"); token != "" {
		env = setEnv(env, "VERCEL_AUTH_TOKEN", token)
	}
	if token := firstEnv("CRABBOX_VERCEL_SANDBOX_TOKEN", "CRABBOX_VERCEL_TOKEN", "VERCEL_TOKEN"); token != "" {
		env = setEnv(env, "VERCEL_TOKEN", token)
	}
	if token := firstEnv("CRABBOX_VERCEL_SANDBOX_OIDC_TOKEN", "VERCEL_OIDC_TOKEN"); token != "" {
		env = setEnv(env, "VERCEL_OIDC_TOKEN", token)
	}
	return env
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func redactSecrets(value string) string {
	redacted := value
	for _, env := range os.Environ() {
		key, secret, ok := strings.Cut(env, "=")
		if !ok || !strings.Contains(strings.ToLower(key), "token") || strings.TrimSpace(secret) == "" {
			continue
		}
		redacted = strings.ReplaceAll(redacted, secret, "[REDACTED]")
	}
	return redacted
}
