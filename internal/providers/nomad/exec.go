package nomad

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type nomadExecRequest struct {
	JobID        string
	AllocationID string
	NodeID       string
	NodeName     string
	Task         string
	Command      []string
	Stdin        io.Reader
	Stdout       io.Writer
	Stderr       io.Writer
}

func (b *backend) execContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if b.cfg.Nomad.ExecTimeoutSecs <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(b.cfg.Nomad.ExecTimeoutSecs)*time.Second)
}

func (b *backend) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

func (b *backend) execShell(ctx context.Context, client Client, ready allocationReadiness, command string) error {
	execCtx, cancel := b.execContext(ctx)
	defer cancel()
	exitCode, err := b.allocationExec(execCtx, client, ready, []string{"sh", "-lc", command}, nil, b.rt.Stdout, b.rt.Stderr)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return exit(exitCode, "nomad exec %q exited %d", command, exitCode)
	}
	return nil
}

func (b *backend) runCommand(ctx context.Context, client Client, ready allocationReadiness, req RunRequest, workdir string) (int, error) {
	command, err := buildCommand(req.Command, req.ShellMode)
	if err != nil {
		return 0, err
	}
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	script := remoteCommandScript(workdir, req.Env, command)
	execCtx, cancel := b.execContext(ctx)
	defer cancel()
	exitCode, err := b.allocationExec(execCtx, client, ready, []string{"sh", "-s"}, strings.NewReader(script), b.rt.Stdout, b.rt.Stderr)
	if err != nil {
		exitCode = normalizeExitCode(exitCode)
		if exitCode == 0 {
			exitCode = 1
		}
		return exitCode, fmt.Errorf("nomad run transport failed: %w", err)
	}
	return normalizeExitCode(exitCode), nil
}

func (b *backend) allocationExec(ctx context.Context, client Client, ready allocationReadiness, command []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if strings.TrimSpace(ready.AllocationID) == "" {
		return 0, exit(5, "nomad allocation is not ready for exec")
	}
	if strings.TrimSpace(ready.Task) == "" {
		return 0, exit(5, "nomad allocation has no task for exec")
	}
	if len(command) == 0 {
		return 0, errors.New("missing command")
	}
	return client.AllocationExec(ctx, nomadExecRequest{
		JobID:        ready.JobID,
		AllocationID: ready.AllocationID,
		NodeID:       ready.NodeID,
		NodeName:     ready.NodeName,
		Task:         ready.Task,
		Command:      append([]string(nil), command...),
		Stdin:        stdin,
		Stdout:       stdout,
		Stderr:       stderr,
	})
}

func normalizeExitCode(code int) int {
	if code < 0 {
		return 1
	}
	return code
}

func buildCommand(command []string, shellMode bool) ([]string, error) {
	if len(command) == 0 {
		return nil, errors.New("missing command")
	}
	if shellMode {
		return []string{"bash", "-lc", strings.Join(command, " ")}, nil
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		if len(command) == 1 {
			return []string{"bash", "-lc", command[0]}, nil
		}
		return []string{"bash", "-lc", shellScriptFromArgv(command)}, nil
	}
	return append([]string(nil), command...), nil
}

func remoteCommandScript(workdir string, env map[string]string, command []string) string {
	var b strings.Builder
	b.WriteString("mkdir -p ")
	b.WriteString(shellQuote(workdir))
	b.WriteString(" && cd ")
	b.WriteString(shellQuote(workdir))
	for key, value := range env {
		if !validShellEnvName(key) {
			continue
		}
		b.WriteString(" && export ")
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(shellQuote(value))
	}
	b.WriteString(" && exec ")
	b.WriteString(shellScriptFromArgv(command))
	return b.String()
}

func validShellEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if i == 0 {
			if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_') {
				return false
			}
			continue
		}
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_') {
			return false
		}
	}
	return true
}
