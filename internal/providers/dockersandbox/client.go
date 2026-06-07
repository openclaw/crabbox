package dockersandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

type sbxCLI struct {
	cfg Config
	rt  Runtime
}

func newSBXCLI(cfg Config, rt Runtime) (*sbxCLI, error) {
	if rt.Exec == nil {
		return nil, exit(2, "provider=docker-sandbox requires Runtime.Exec")
	}
	return &sbxCLI{cfg: cfg, rt: rt}, nil
}

func (c *sbxCLI) binary() string {
	return blank(strings.TrimSpace(c.cfg.DockerSandbox.CLIPath), defaultCLIPath)
}

func (c *sbxCLI) env() []string {
	return os.Environ()
}

func (c *sbxCLI) runQuiet(ctx context.Context, args []string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	res, err := c.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:   c.binary(),
		Args:   args,
		Env:    c.env(),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), sbxError(args, res.ExitCode, &stdout, &stderr, err)
	}
	if res.ExitCode != 0 {
		return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), sbxError(args, res.ExitCode, &stdout, &stderr, nil)
	}
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), nil
}

func (c *sbxCLI) runStreamed(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	res, err := c.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:   c.binary(),
		Args:   args,
		Env:    c.env(),
		Stdout: stdout,
		Stderr: stderr,
	})
	if isMissingExecutableError(err) {
		var stdoutBuf, stderrBuf bytes.Buffer
		return res.ExitCode, sbxError(args, res.ExitCode, &stdoutBuf, &stderrBuf, err)
	}
	if err != nil && res.ExitCode == 0 {
		return res.ExitCode, fmt.Errorf("sbx %s: %w", strings.Join(args, " "), err)
	}
	return res.ExitCode, nil
}

func (c *sbxCLI) version(ctx context.Context) (string, error) {
	out, _, err := c.runQuiet(ctx, []string{"version"})
	if err != nil {
		return "", err
	}
	return firstNonEmptyLine(out), nil
}

func (c *sbxCLI) diagnose(ctx context.Context) (string, error) {
	out, _, err := c.runQuiet(ctx, []string{"diagnose", "--output", "json"})
	return out, err
}

func (c *sbxCLI) create(ctx context.Context, name string, repo Repo) error {
	args := []string{"create", "--name", name}
	if v := strings.TrimSpace(c.cfg.DockerSandbox.Template); v != "" {
		args = append(args, "--template", v)
	}
	if c.cfg.DockerSandbox.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(c.cfg.DockerSandbox.CPUs, 'f', -1, 64))
	}
	if v := strings.TrimSpace(c.cfg.DockerSandbox.Memory); v != "" {
		args = append(args, "--memory", v)
	}
	for _, value := range c.cfg.DockerSandbox.MCP {
		args = append(args, "--mcp", strings.TrimSpace(value))
	}
	for _, value := range c.cfg.DockerSandbox.Kit {
		args = append(args, "--kit", strings.TrimSpace(value))
	}
	if c.cfg.DockerSandbox.Clone {
		args = append(args, "--clone")
	}
	args = append(args, dockerSandboxAgent(c.cfg))
	args = append(args, repo.Root)
	for _, value := range c.cfg.DockerSandbox.ExtraWorkspaces {
		args = append(args, strings.TrimSpace(value))
	}
	_, _, err := c.runQuiet(ctx, args)
	return err
}

func (c *sbxCLI) execStream(ctx context.Context, name, workdir, envFile string, command []string, stdout, stderr io.Writer) (int, error) {
	args := []string{"exec"}
	if strings.TrimSpace(workdir) != "" {
		args = append(args, "--workdir", workdir)
	}
	if strings.TrimSpace(envFile) != "" {
		args = append(args, "--env-file", envFile)
	}
	args = append(args, name)
	args = append(args, command...)
	return c.runStreamed(ctx, args, stdout, stderr)
}

func (c *sbxCLI) remove(ctx context.Context, name string) error {
	_, _, err := c.runQuiet(ctx, []string{"rm", "--force", name})
	return err
}

func (c *sbxCLI) ports(ctx context.Context, name string, publish, unpublish []string, jsonOut bool) (string, error) {
	args := []string{"ports", name}
	if jsonOut {
		args = append(args, "--json")
	}
	for _, spec := range publish {
		args = append(args, "--publish", strings.TrimSpace(spec))
	}
	for _, spec := range unpublish {
		args = append(args, "--unpublish", strings.TrimSpace(spec))
	}
	out, _, err := c.runQuiet(ctx, args)
	return out, err
}

func (c *sbxCLI) copy(ctx context.Context, src, dst string, followLink bool) error {
	args := []string{"cp"}
	if followLink {
		args = append(args, "-L")
	}
	args = append(args, src, dst)
	_, _, err := c.runQuiet(ctx, args)
	return err
}

func (c *sbxCLI) list(ctx context.Context) ([]sandboxRecord, error) {
	out, _, err := c.runQuiet(ctx, []string{"ls", "--json"})
	if err != nil {
		return nil, err
	}
	return parseSandboxList(out)
}

type sandboxRecord struct {
	ID        string
	Name      string
	State     string
	Agent     string
	Workspace string
}

func parseSandboxList(data string) ([]sandboxRecord, error) {
	data = strings.TrimSpace(data)
	if data == "" {
		return nil, nil
	}
	var root any
	if err := json.Unmarshal([]byte(data), &root); err != nil {
		return nil, exit(2, "parse sbx ls --json output: %v", err)
	}
	var items []any
	switch value := root.(type) {
	case []any:
		items = value
	case map[string]any:
		for _, key := range []string{"sandboxes", "items", "data", "results"} {
			if nested, ok := value[key].([]any); ok {
				items = nested
				break
			}
		}
		if items == nil {
			items = []any{value}
		}
	default:
		return nil, exit(2, "parse sbx ls --json output: expected array or object, got %T", root)
	}
	records := make([]sandboxRecord, 0, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		record := sandboxRecord{
			ID:        firstStringField(obj, "id", "ID", "sandboxId", "sandbox_id"),
			Name:      firstStringField(obj, "name", "Name", "sandboxName", "sandbox_name"),
			State:     strings.ToLower(firstStringField(obj, "state", "status", "Status")),
			Agent:     firstStringField(obj, "agent", "Agent"),
			Workspace: firstStringField(obj, "workspace", "workdir", "workingDir", "working_dir"),
		}
		if record.Name == "" {
			record.Name = record.ID
		}
		if record.ID == "" {
			record.ID = record.Name
		}
		if record.State == "" {
			record.State = "unknown"
		}
		if record.Name != "" || record.ID != "" {
			records = append(records, record)
		}
	}
	return records, nil
}

func firstStringField(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := obj[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				return strings.TrimSpace(typed)
			}
		case float64:
			return strconv.FormatFloat(typed, 'f', -1, 64)
		case bool:
			return strconv.FormatBool(typed)
		}
	}
	return ""
}

func sbxError(args []string, exitCode int, stdout, stderr *bytes.Buffer, runErr error) error {
	tail := strings.TrimSpace(stderr.String())
	if tail == "" {
		tail = strings.TrimSpace(stdout.String())
	}
	if len(tail) > 4096 {
		tail = tail[:4096]
	}
	action := strings.Join(args, " ")
	guidance := classifySBXError(action, tail, runErr)
	if runErr != nil {
		if guidance != "" {
			return fmt.Errorf("sbx %s (exit=%d): %v: %s: %s", action, exitCode, runErr, tail, guidance)
		}
		return fmt.Errorf("sbx %s (exit=%d): %v: %s", action, exitCode, runErr, tail)
	}
	if guidance != "" {
		return fmt.Errorf("sbx %s exited %d: %s: %s", action, exitCode, tail, guidance)
	}
	return fmt.Errorf("sbx %s exited %d: %s", action, exitCode, tail)
}

func classifySBXError(action, text string, runErr error) string {
	lower := strings.ToLower(text + " " + action)
	if isMissingExecutableError(runErr) {
		return "install the Docker Sandbox sbx CLI or set dockerSandbox.cliPath / --docker-sandbox-cli"
	}
	if strings.Contains(lower, "not logged in") || strings.Contains(lower, "login required") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "authentication") {
		return "run sbx login and retry"
	}
	if strings.Contains(lower, "virtualization") || strings.Contains(lower, "hypervisor") || strings.Contains(lower, "kvm") {
		return "enable host virtualization/KVM/hypervisor support for Docker Sandboxes"
	}
	if strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out") {
		return "Docker Sandbox control plane did not respond before the command timeout"
	}
	return ""
}

func isMissingExecutableError(err error) bool {
	return err != nil && (errors.Is(err, os.ErrNotExist) || strings.Contains(strings.ToLower(err.Error()), "no such file"))
}

func firstNonEmptyLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}
