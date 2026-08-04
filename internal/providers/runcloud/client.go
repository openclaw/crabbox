package runcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type api interface {
	Check(context.Context) error
	CreateSandbox(context.Context, createRequest) (sandboxData, error)
	GetSandbox(context.Context, string) (sandboxData, error)
	ListSandboxes(context.Context) ([]sandboxData, error)
	ExposeSandbox(context.Context, string, string) (boxData, error)
	InstallSSHKey(context.Context, string, string) error
	ResumeSandbox(context.Context, string) error
	DeleteSandbox(context.Context, string) error
}

type client struct {
	cliPath      string
	runner       coreCommandRunner
	pollInterval time.Duration
}

const (
	exposedSandboxCPUCoreCount = "2"
	exposedSandboxMemoryMiB    = "24576"
)

type coreCommandRunner interface {
	Run(context.Context, LocalCommandRequest) (LocalCommandResult, error)
}

type createRequest struct {
	Name   string
	Image  string
	Region string
	TTL    time.Duration
}

type sandboxData struct {
	ID             string   `json:"id"`
	Name           string   `json:"name,omitempty"`
	State          string   `json:"state,omitempty"`
	Image          string   `json:"image,omitempty"`
	CreatedAt      string   `json:"created_at,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	Hostname       string   `json:"hostname,omitempty"`
	ExposedPort    int      `json:"exposedPort,omitempty"`
	Box            *boxData `json:"box,omitempty"`
}

type boxData struct {
	ID        string `json:"id"`
	SandboxID string `json:"sandboxId,omitempty"`
	Name      string `json:"name,omitempty"`
	Hostname  string `json:"hostname,omitempty"`
	Port      int    `json:"port,omitempty"`
	Status    string `json:"status,omitempty"`
}

type execData struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
}

func newClient(cfg Config, rt Runtime) (api, error) {
	cliPath := strings.TrimSpace(cfg.RunCloud.CLIPath)
	if cliPath == "" {
		return nil, exit(2, "provider=%s requires runCloud.cliPath", providerName)
	}
	return &client{cliPath: cliPath, runner: rt.Exec, pollInterval: time.Second}, nil
}

func (c *client) Check(ctx context.Context) error {
	result, err := c.run(ctx, "account", "--json")
	if err != nil {
		return fmt.Errorf("Run Cloud CLI account check failed: %s", commandError(result, err))
	}
	return nil
}

func (c *client) CreateSandbox(ctx context.Context, req createRequest) (sandboxData, error) {
	args := []string{
		"sandbox", "create",
		"--name", req.Name,
		"--image", req.Image,
		"--cpu", exposedSandboxCPUCoreCount,
		"--memory", exposedSandboxMemoryMiB,
		"--persistent",
	}
	if req.Region != "" {
		args = append(args, "--region", req.Region)
	}
	seconds := int64(0)
	if req.TTL > 0 {
		seconds = int64(req.TTL.Round(time.Second) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
	}
	args = append(args, "--timeout", fmt.Sprint(seconds), "--json")
	result, err := c.run(ctx, args...)
	created, decodeErr := decodeSandbox(result.Stdout)
	if err != nil {
		if created.ID != "" {
			return created, fmt.Errorf("Run Cloud CLI create failed after creating %s: %s", created.ID, commandError(result, err))
		}
		return sandboxData{}, fmt.Errorf("Run Cloud CLI create failed: %s", commandError(result, err))
	}
	if decodeErr != nil {
		return sandboxData{}, fmt.Errorf("decode Run Cloud create response: %w", decodeErr)
	}
	if created.ID == "" {
		return sandboxData{}, fmt.Errorf("Run Cloud create response missing sandbox id")
	}
	return c.waitForRunning(ctx, created)
}

func (c *client) GetSandbox(ctx context.Context, id string) (sandboxData, error) {
	result, err := c.run(ctx, "sandbox", "get", id, "--json")
	if err != nil {
		return sandboxData{}, fmt.Errorf("Run Cloud CLI get failed: %s", commandError(result, err))
	}
	sandbox, err := decodeSandbox(result.Stdout)
	if err != nil {
		return sandboxData{}, fmt.Errorf("decode Run Cloud sandbox: %w", err)
	}
	return sandbox, nil
}

func (c *client) ListSandboxes(ctx context.Context) ([]sandboxData, error) {
	result, err := c.run(ctx, "sandbox", "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("Run Cloud CLI list failed: %s", commandError(result, err))
	}
	var sandboxes []sandboxData
	if err := json.Unmarshal([]byte(result.Stdout), &sandboxes); err != nil {
		return nil, fmt.Errorf("decode Run Cloud sandbox list: %w", err)
	}
	return sandboxes, nil
}

func (c *client) ExposeSandbox(ctx context.Context, id, name string) (boxData, error) {
	result, err := c.run(ctx, "sandbox", "expose", id, "--name", name, "--port", "3000", "--json")
	if err != nil {
		return boxData{}, fmt.Errorf("Run Cloud CLI expose failed: %s", commandError(result, err))
	}
	var box boxData
	if err := json.Unmarshal([]byte(result.Stdout), &box); err != nil {
		return boxData{}, fmt.Errorf("decode Run Cloud exposure: %w", err)
	}
	if box.ID == "" || box.Hostname == "" {
		return boxData{}, fmt.Errorf("Run Cloud expose response missing box id or hostname")
	}
	return box, nil
}

func (c *client) InstallSSHKey(ctx context.Context, id, publicKey string) error {
	quotedKey := shellQuote(strings.TrimSpace(publicKey))
	script := strings.Join([]string{
		"set -eu",
		"user=runcloud",
		"if ! id -u \"$user\" >/dev/null 2>&1; then",
		"  command -v useradd >/dev/null 2>&1 || { printf 'runcloud user is missing and useradd is unavailable\\n' >&2; exit 1; }",
		"  if [ \"$(id -u)\" = 0 ]; then",
		"    useradd --create-home --home-dir /home/runcloud --shell /bin/bash \"$user\"",
		"  else",
		"    command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1 || { printf 'runcloud user is missing; root or passwordless sudo is required\\n' >&2; exit 1; }",
		"    sudo -n useradd --create-home --home-dir /home/runcloud --shell /bin/bash \"$user\"",
		"  fi",
		"fi",
		"home=$(getent passwd \"$user\" | cut -d: -f6)",
		"test -n \"$home\"",
		"mkdir -p \"$home/.ssh\"",
		"chmod 700 \"$home/.ssh\"",
		"touch \"$home/.ssh/authorized_keys\"",
		"grep -qxF -- " + quotedKey + " \"$home/.ssh/authorized_keys\" || printf '%s\\n' " + quotedKey + " >> \"$home/.ssh/authorized_keys\"",
		"chmod 600 \"$home/.ssh/authorized_keys\"",
		"if [ \"$(id -u)\" = 0 ]; then chown -R \"$user:$user\" \"$home/.ssh\"; elif [ \"$(id -un)\" != \"$user\" ]; then sudo -n chown -R \"$user:$user\" \"$home/.ssh\"; fi",
		"missing=''",
		"for tool in git rsync; do command -v \"$tool\" >/dev/null 2>&1 || missing=\"$missing $tool\"; done",
		"if [ -n \"$missing\" ]; then",
		"  command -v apt-get >/dev/null 2>&1 || { printf 'missing required tools:%s; apt-get is unavailable\\n' \"$missing\" >&2; exit 1; }",
		"  export DEBIAN_FRONTEND=noninteractive",
		"  if [ \"$(id -u)\" = 0 ]; then",
		"    apt-get update -qq && apt-get install -y -qq --no-install-recommends git rsync",
		"  else",
		"    command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1 || { printf 'missing required tools:%s; root or passwordless sudo is required\\n' \"$missing\" >&2; exit 1; }",
		"    sudo -n apt-get update -qq && sudo -n apt-get install -y -qq --no-install-recommends git rsync",
		"  fi",
		"fi",
		"command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null && command -v python3 >/dev/null",
	}, "\n")
	result, err := c.run(ctx, "sandbox", "exec", id, script, "--json")
	if err != nil {
		return fmt.Errorf("Run Cloud SSH bootstrap failed: %s", commandError(result, err))
	}
	var executed execData
	if err := json.Unmarshal([]byte(result.Stdout), &executed); err != nil {
		return fmt.Errorf("decode Run Cloud SSH bootstrap response: %w", err)
	}
	if executed.ExitCode != 0 {
		detail := strings.TrimSpace(executed.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(executed.Stdout)
		}
		return fmt.Errorf("Run Cloud SSH bootstrap exited %d: %s", executed.ExitCode, detail)
	}
	return nil
}

func (c *client) ResumeSandbox(ctx context.Context, id string) error {
	result, err := c.run(ctx, "sandbox", "resume", id, "--json")
	if err != nil {
		return fmt.Errorf("Run Cloud CLI resume failed: %s", commandError(result, err))
	}
	_, err = c.waitForRunning(ctx, sandboxData{ID: id, State: "starting"})
	return err
}

func (c *client) DeleteSandbox(ctx context.Context, id string) error {
	result, err := c.run(ctx, "sandbox", "rm", id, "--json")
	if err == nil || isNotFoundResult(result) {
		return nil
	}
	return fmt.Errorf("Run Cloud CLI delete failed: %s", commandError(result, err))
}

func (c *client) waitForRunning(ctx context.Context, sandbox sandboxData) (sandboxData, error) {
	deadline := time.Now().Add(20 * time.Minute)
	for {
		state := normalizedState(sandbox.State)
		if state == "running" {
			return sandbox, nil
		}
		if sandboxStateFailed(state) {
			return sandbox, fmt.Errorf("Run Cloud sandbox %s entered %s state", sandbox.ID, state)
		}
		if time.Now().After(deadline) {
			return sandbox, fmt.Errorf("timed out waiting for Run Cloud sandbox %s to start", sandbox.ID)
		}
		select {
		case <-ctx.Done():
			return sandbox, ctx.Err()
		case <-time.After(c.pollInterval):
		}
		latest, err := c.GetSandbox(ctx, sandbox.ID)
		if err != nil {
			return sandbox, err
		}
		sandbox = latest
	}
}

func (c *client) run(ctx context.Context, args ...string) (LocalCommandResult, error) {
	if c.runner == nil {
		return LocalCommandResult{}, fmt.Errorf("provider=%s command runner is unavailable", providerName)
	}
	return c.runner.Run(ctx, LocalCommandRequest{Name: c.cliPath, Args: args})
}

func decodeSandbox(raw string) (sandboxData, error) {
	var sandbox sandboxData
	err := json.Unmarshal([]byte(raw), &sandbox)
	return sandbox, err
}

func commandError(result LocalCommandResult, err error) string {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail != "" {
		return detail
	}
	return err.Error()
}

func isNotFoundResult(result LocalCommandResult) bool {
	text := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	return strings.Contains(text, "not found") || strings.Contains(text, "status 404") || strings.Contains(text, "(404)")
}
