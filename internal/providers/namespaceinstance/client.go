package namespaceinstance

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type nscClient struct {
	path   string
	cfg    NamespaceInstanceConfig
	runner CommandRunner
}

func newNSCClient(cfg Config, rt Runtime) (*nscClient, error) {
	if rt.Exec == nil {
		return nil, exit(2, "provider=%s requires a local command runner for nsc", providerName)
	}
	return &nscClient{path: "nsc", cfg: cfg.NamespaceInstance, runner: rt.Exec}, nil
}

func (c *nscClient) CheckReadiness(ctx context.Context) (string, error) {
	if _, err := c.run(ctx, "--help"); err != nil {
		return "", fmt.Errorf("nsc CLI unavailable: %w", err)
	}
	if _, err := c.run(ctx, "auth", "check-login"); err != nil {
		return "", fmt.Errorf("nsc auth check-login failed: %w", err)
	}
	result, err := c.run(ctx, "list", "-o", "json")
	if err != nil {
		return "", fmt.Errorf("nsc list readiness check failed: %w", err)
	}
	count, err := parseNSCListCount(result.Stdout)
	if err != nil {
		return "", fmt.Errorf("decode nsc list readiness output: %w", err)
	}
	return strconv.Itoa(count), nil
}

func (c *nscClient) run(ctx context.Context, args ...string) (LocalCommandResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = contextWithTimeout(ctx, commandTimeout())
		defer cancel()
	}
	argv := c.globalArgs()
	argv = append(argv, args...)
	result, err := c.runner.Run(ctx, LocalCommandRequest{Name: c.path, Args: argv})
	if err != nil {
		return result, exit(commandExitCode(result), "nsc %s failed", safeCommand(args))
	}
	return result, nil
}

func (c *nscClient) globalArgs() []string {
	var args []string
	if value := strings.TrimSpace(c.cfg.Endpoint); value != "" {
		args = append(args, "--endpoint", value)
	}
	if value := strings.TrimSpace(c.cfg.Keychain); value != "" {
		args = append(args, "--keychain", value)
	}
	if value := strings.TrimSpace(c.cfg.Region); value != "" {
		args = append(args, "--region", value)
	}
	return args
}

func parseNSCListCount(out string) (int, error) {
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return 0, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(out), &items); err == nil {
		return len(items), nil
	}
	var wrapped struct {
		Instances []json.RawMessage `json:"instances"`
		Items     []json.RawMessage `json:"items"`
		Results   []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &wrapped); err != nil {
		return 0, err
	}
	switch {
	case wrapped.Instances != nil:
		return len(wrapped.Instances), nil
	case wrapped.Items != nil:
		return len(wrapped.Items), nil
	case wrapped.Results != nil:
		return len(wrapped.Results), nil
	default:
		return 0, nil
	}
}

func commandExitCode(result LocalCommandResult) int {
	if result.ExitCode != 0 {
		return result.ExitCode
	}
	return 1
}

func safeCommand(args []string) string {
	if len(args) == 0 {
		return "command"
	}
	switch args[0] {
	case "--help":
		return "--help"
	case "auth":
		return "auth check-login"
	case "list":
		return "list"
	default:
		return "command"
	}
}
