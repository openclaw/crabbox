package agentsandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

func (b *backend) execContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if b.cfg.AgentSandbox.ExecTimeoutSecs == 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(b.cfg.AgentSandbox.ExecTimeoutSecs)*time.Second)
}

func (b *backend) cleanupContext(context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), agentSandboxCleanupTimeout)
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

func (b *backend) execShell(ctx context.Context, client kubernetesClient, ready sandboxReadiness, command string) error {
	execCtx, cancel := b.execContext(ctx)
	defer cancel()
	if err := b.execPod(execCtx, client, ready, podExecRequest{
		Command: []string{"sh", "-lc", command},
		Stdout:  b.rt.Stdout,
		Stderr:  b.rt.Stderr,
	}); err != nil {
		if code, ok := remoteExitStatus(err); ok {
			return exit(code, "agent-sandbox exec %q exited %d", command, code)
		}
		return err
	}
	return nil
}

func (b *backend) runCommand(ctx context.Context, client kubernetesClient, ready sandboxReadiness, req RunRequest, workdir string) (int, error) {
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
	err = b.execPod(execCtx, client, ready, podExecRequest{
		Command: []string{"sh", "-s"},
		Stdin:   strings.NewReader(script),
		Stdout:  b.rt.Stdout,
		Stderr:  b.rt.Stderr,
	})
	if err == nil {
		return 0, nil
	}
	if code, ok := remoteExitStatus(err); ok {
		return code, nil
	}
	return 1, fmt.Errorf("agent-sandbox run transport failed: %w", err)
}

func (b *backend) execPod(ctx context.Context, client kubernetesClient, ready sandboxReadiness, req podExecRequest) error {
	if err := revalidateSandboxReadiness(ctx, client, b.cfg.AgentSandbox.Namespace, ready); err != nil {
		return err
	}
	req.Namespace = b.cfg.AgentSandbox.Namespace
	req.Pod = ready.PodName
	req.Container = ready.Container
	return client.Exec(ctx, req)
}

func remoteExitStatus(err error) (int, bool) {
	var exitErr interface{ ExitStatus() int }
	if errors.As(err, &exitErr) {
		code := exitErr.ExitStatus()
		if code < 0 {
			code = 1
		}
		return code, true
	}
	return 0, false
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
	return command, nil
}

func leadingEnvAssignment(command []string) bool {
	return len(command) > 1 && strings.Contains(command[0], "=") && !strings.HasPrefix(command[0], "-")
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
