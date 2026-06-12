package anthropicsandboxruntime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

type srtCLI struct {
	cfg Config
	rt  Runtime
}

func newSRTCLI(cfg Config, rt Runtime) (*srtCLI, error) {
	if rt.Exec == nil {
		return nil, exit(2, "provider=anthropic-sandbox-runtime requires Runtime.Exec")
	}
	return &srtCLI{cfg: cfg, rt: rt}, nil
}

func (c *srtCLI) binary() string {
	return blank(strings.TrimSpace(c.cfg.AnthropicSRT.CLIPath), defaultCLIPath)
}

func (c *srtCLI) baseArgs() []string {
	args := []string{}
	if c.cfg.AnthropicSRT.Debug {
		args = append(args, "--debug")
	}
	if settings := strings.TrimSpace(c.cfg.AnthropicSRT.Settings); settings != "" {
		args = append(args, "--settings", settings)
	}
	return args
}

func (c *srtCLI) env(extra map[string]string) []string {
	env := anthropicSandboxRuntimeBaseEnv()
	byName := make(map[string]int, len(env))
	for i, value := range env {
		key, _, ok := strings.Cut(value, "=")
		if ok {
			byName[key] = i
		}
	}
	for key, value := range extra {
		if idx, ok := byName[key]; ok {
			env[idx] = key + "=" + value
			continue
		}
		env = append(env, key+"="+value)
	}
	return env
}

func anthropicSandboxRuntimeBaseEnv() []string {
	keys := []string{
		"PATH",
		"HOME",
		"TMPDIR",
		"TMP",
		"TEMP",
		"USER",
		"LOGNAME",
		"SHELL",
		"LANG",
		"LC_ALL",
		"LC_CTYPE",
		"XDG_CONFIG_HOME",
	}
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func (c *srtCLI) help(ctx context.Context) (string, error) {
	stdout, _, err := c.runQuiet(ctx, []string{"--help"})
	return stdout, err
}

func (c *srtCLI) version(ctx context.Context) (string, error) {
	stdout, _, err := c.runQuiet(ctx, []string{"--version"})
	return firstNonEmptyLine(stdout), err
}

func (c *srtCLI) runQuiet(ctx context.Context, args []string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	result, err := c.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:   c.binary(),
		Args:   args,
		Env:    c.env(nil),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil || result.ExitCode != 0 {
		return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), srtError(args, result, stdout.String(), stderr.String(), err)
	}
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), nil
}

func (c *srtCLI) runCommand(ctx context.Context, dir, commandText string, env map[string]string, stdout, stderr io.Writer) (int, error) {
	args := append(c.baseArgs(), "-c", commandText)
	result, err := c.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:   c.binary(),
		Args:   args,
		Env:    c.env(env),
		Dir:    dir,
		Stdout: stdout,
		Stderr: stderr,
	})
	if err != nil {
		return result.ExitCode, srtError(args, result, result.Stdout, result.Stderr, err)
	}
	if result.ExitCode != 0 {
		return result.ExitCode, srtError(args, result, result.Stdout, result.Stderr, nil)
	}
	return result.ExitCode, nil
}

func srtError(args []string, result LocalCommandResult, stdout, stderr string, err error) error {
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		detail = strings.TrimSpace(stdout)
	}
	if detail == "" && err != nil {
		detail = err.Error()
	}
	if detail == "" {
		detail = fmt.Sprintf("exit code %d", result.ExitCode)
	}
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	action := strings.Join(args, " ")
	if action == "" {
		action = cNameForError
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("srt %s failed exit=%d: %s", action, result.ExitCode, detail)
	}
	return fmt.Errorf("srt %s failed: %s", action, detail)
}

const cNameForError = "command"

func firstNonEmptyLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
