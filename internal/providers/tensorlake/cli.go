package tensorlake

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

type tensorlakeCLI struct {
	cfg Config
	rt  Runtime
}

func newTensorlakeCLI(cfg Config, rt Runtime) (*tensorlakeCLI, error) {
	if strings.TrimSpace(cfg.Tensorlake.APIKey) == "" {
		return nil, exit(2, "provider=tensorlake requires TENSORLAKE_API_KEY")
	}
	if rt.Exec == nil {
		return nil, exit(2, "provider=tensorlake requires Runtime.Exec")
	}
	return &tensorlakeCLI{cfg: cfg, rt: rt}, nil
}

func (c *tensorlakeCLI) binary() string {
	return blank(strings.TrimSpace(c.cfg.Tensorlake.CLIPath), defaultCLIPath)
}

func (c *tensorlakeCLI) globalArgs() []string {
	var args []string
	if v := strings.TrimSpace(c.cfg.Tensorlake.APIURL); v != "" {
		args = append(args, "--api-url", v)
	}
	if v := strings.TrimSpace(c.cfg.Tensorlake.OrganizationID); v != "" {
		args = append(args, "--organization", v)
	}
	if v := strings.TrimSpace(c.cfg.Tensorlake.ProjectID); v != "" {
		args = append(args, "--project", v)
	}
	if v := strings.TrimSpace(c.cfg.Tensorlake.Namespace); v != "" {
		args = append(args, "--namespace", v)
	}
	return args
}

func (c *tensorlakeCLI) env() []string {
	env := append([]string{}, os.Environ()...)
	env = append(env, "TENSORLAKE_API_KEY="+c.cfg.Tensorlake.APIKey)
	if v := strings.TrimSpace(c.cfg.Tensorlake.APIURL); v != "" {
		env = append(env, "TENSORLAKE_API_URL="+v)
	}
	return env
}

// runQuiet runs a tensorlake CLI subcommand and captures stdout/stderr.
// Returns trimmed stdout on success, or an error on non-zero exit.
func (c *tensorlakeCLI) runQuiet(ctx context.Context, sub []string, args []string) (string, error) {
	full := append([]string{}, c.globalArgs()...)
	full = append(full, sub...)
	full = append(full, args...)
	var stdout, stderr bytes.Buffer
	res, err := c.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:   c.binary(),
		Args:   full,
		Env:    c.env(),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return strings.TrimSpace(stdout.String()), tensorlakeError(strings.Join(sub, " "), res.ExitCode, &stdout, &stderr, err)
	}
	if res.ExitCode != 0 {
		return strings.TrimSpace(stdout.String()), tensorlakeError(strings.Join(sub, " "), res.ExitCode, &stdout, &stderr, nil)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// runStreamed runs a tensorlake CLI subcommand and streams output to the
// provided writers. Non-zero exit codes are reported via the int return,
// not as an error — callers must propagate them as the wrapped command's
// exit. Errors are reserved for transport-level failures (binary missing,
// I/O errors).
func (c *tensorlakeCLI) runStreamed(ctx context.Context, sub []string, args []string, stdout, stderr io.Writer) (int, error) {
	full := append([]string{}, c.globalArgs()...)
	full = append(full, sub...)
	full = append(full, args...)
	res, err := c.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:   c.binary(),
		Args:   full,
		Env:    c.env(),
		Stdout: stdout,
		Stderr: stderr,
	})
	if err != nil && res.ExitCode == 0 {
		return res.ExitCode, fmt.Errorf("tensorlake %s: %w", strings.Join(sub, " "), err)
	}
	return res.ExitCode, nil
}

// createSandbox runs `tensorlake sbx create` and returns the
// Tensorlake-assigned sandbox ID parsed from stdout.
func (c *tensorlakeCLI) createSandbox(ctx context.Context, name string) (string, error) {
	args := []string{}
	if c.cfg.Tensorlake.CPUs > 0 {
		args = append(args, "-c", trimFloat(c.cfg.Tensorlake.CPUs))
	}
	if c.cfg.Tensorlake.MemoryMB > 0 {
		args = append(args, "-m", strconv.Itoa(c.cfg.Tensorlake.MemoryMB))
	}
	if c.cfg.Tensorlake.DiskMB > 0 {
		args = append(args, "--disk_mb", strconv.Itoa(c.cfg.Tensorlake.DiskMB))
	}
	if c.cfg.Tensorlake.TimeoutSecs > 0 {
		args = append(args, "-t", strconv.Itoa(c.cfg.Tensorlake.TimeoutSecs))
	}
	if v := strings.TrimSpace(c.cfg.Tensorlake.Snapshot); v != "" {
		args = append(args, "-s", v)
	}
	if v := strings.TrimSpace(c.cfg.Tensorlake.Image); v != "" {
		args = append(args, "-i", v)
	}
	if c.cfg.Tensorlake.NoInternet {
		args = append(args, "-N")
	}
	if strings.TrimSpace(name) != "" {
		args = append(args, name)
	}
	out, err := c.runQuiet(ctx, []string{"sbx", "create"}, args)
	if err != nil {
		return "", err
	}
	id := parseSandboxID(out)
	if id == "" {
		return "", fmt.Errorf("tensorlake sbx create: empty sandbox id in output %q", out)
	}
	return id, nil
}

// parseSandboxID extracts a sandbox ID from `tensorlake sbx create` stdout.
// The CLI prints just the ID on a single line on success.
func parseSandboxID(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if isLikelySandboxID(line) {
			return line
		}
	}
	return ""
}

// isLikelySandboxID returns true for the lowercase-alphanumeric token format
// Tensorlake uses for sandbox IDs (e.g. "3pryjysezwsnlex226i5h").
func isLikelySandboxID(s string) bool {
	if len(s) < 12 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}

func (c *tensorlakeCLI) execStream(ctx context.Context, name, workdir string, command []string, stdout, stderr io.Writer) (int, error) {
	args := []string{}
	if v := strings.TrimSpace(workdir); v != "" {
		args = append(args, "-w", v)
	}
	args = append(args, name)
	args = append(args, command...)
	return c.runStreamed(ctx, []string{"sbx", "exec"}, args, stdout, stderr)
}

// execShell runs `bash -lc <command>` inside the sandbox and returns an error
// for non-zero exits. Used for sync helpers (mkdir, tar, cleanup) where we
// don't care about streaming output back to the user.
func (c *tensorlakeCLI) execShell(ctx context.Context, name, command string) error {
	code, err := c.runStreamed(ctx, []string{"sbx", "exec"}, []string{name, "bash", "-lc", command}, io.Discard, io.Discard)
	if err != nil {
		return fmt.Errorf("tensorlake exec %q: %w", command, err)
	}
	if code != 0 {
		return exit(code, "tensorlake exec %q exited %d", command, code)
	}
	return nil
}

// uploadFile pushes a local file into the sandbox via `tensorlake sbx cp`.
func (c *tensorlakeCLI) uploadFile(ctx context.Context, name, localPath, remotePath string) error {
	src := localPath
	dst := name + ":" + remotePath
	_, err := c.runQuiet(ctx, []string{"sbx", "cp"}, []string{src, dst})
	return err
}

func (c *tensorlakeCLI) terminate(ctx context.Context, name string) error {
	_, err := c.runQuiet(ctx, []string{"sbx", "terminate"}, []string{name})
	return err
}

func (c *tensorlakeCLI) describe(ctx context.Context, name string) (string, error) {
	return c.runQuiet(ctx, []string{"sbx", "describe"}, []string{name})
}

func (c *tensorlakeCLI) listIDs(ctx context.Context) ([]string, error) {
	out, err := c.runQuiet(ctx, []string{"sbx", "ls"}, []string{"-q"})
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	lines := strings.Split(out, "\n")
	ids := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			ids = append(ids, line)
		}
	}
	return ids, nil
}

func tensorlakeError(action string, exitCode int, stdout, stderr *bytes.Buffer, runErr error) error {
	tail := strings.TrimSpace(stderr.String())
	if tail == "" {
		tail = strings.TrimSpace(stdout.String())
	}
	if len(tail) > 4096 {
		tail = tail[:4096]
	}
	if runErr != nil {
		return fmt.Errorf("tensorlake %s (exit=%d): %v: %s", action, exitCode, runErr, tail)
	}
	return fmt.Errorf("tensorlake %s exited %d: %s", action, exitCode, tail)
}

func trimFloat(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	return s
}
