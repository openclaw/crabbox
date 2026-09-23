package agentsandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func (b *backend) execTimeout() (time.Duration, error) {
	if b.cfg.AgentSandbox.ExecTimeoutSecs == 0 {
		return 0, nil
	}
	if timeout, ok := shared.SecondsWithGrace(int64(b.cfg.AgentSandbox.ExecTimeoutSecs), 0); ok {
		return timeout, nil
	}
	return 0, core.Exit(2, "agent-sandbox execution timeout exceeds the supported duration range")
}

func (b *backend) execContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	timeout, err := b.execTimeout()
	if err != nil {
		return nil, nil, err
	}
	if timeout == 0 {
		child, cancel := context.WithCancel(ctx)
		return child, cancel, nil
	}
	child, cancel := context.WithTimeout(ctx, timeout)
	return child, cancel, nil
}

func (b *backend) cleanupContext(context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), agentSandboxCleanupTimeout)
}

func (b *backend) execShell(ctx context.Context, client kubernetesClient, ready sandboxReadiness, command string) error {
	execCtx, cancel, err := b.execContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	if err := b.execPod(execCtx, client, ready, podExecRequest{
		Command: []string{"sh", "-lc", command},
		Stdout:  b.rt.Stdout,
		Stderr:  b.rt.Stderr,
	}); err != nil {
		if code, ok := remoteExitStatus(err); ok {
			return core.Exit(code, "agent-sandbox exec %q exited %d", command, code)
		}
		return err
	}
	return nil
}

func (b *backend) runCommand(ctx context.Context, client kubernetesClient, ready sandboxReadiness, req core.RunRequest, workdir string) (int, error) {
	req.Observation.Phase(core.RunPhaseCommand)
	stdout, stderr := req.Observation.CommandWriters(b.rt.Stdout, b.rt.Stderr, core.RunOutputProvider)
	intent, err := core.ParseCommandIntent(req.Command, req.ShellMode, req.CommandLiteralArgs)
	if err != nil {
		return 0, err
	}
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		core.PrintEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	script := shared.ShellWorkspaceCommand(workdir, req.Env, intent, "bash", "-lc")
	execCtx, cancel, err := b.execContext(ctx)
	if err != nil {
		return 2, err
	}
	defer cancel()
	err = b.execPod(execCtx, client, ready, podExecRequest{
		Command: []string{"sh", "-s"},
		Stdin:   strings.NewReader(script),
		Stdout:  stdout,
		Stderr:  stderr,
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
