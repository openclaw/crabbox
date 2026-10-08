package hyperv

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestRestoreCheckpointCachesAfterFailureUsesSSHReadinessForLinux(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if strings.Contains(commandScript(req), "Select-Object -ExpandProperty IPAddresses") {
			return core.LocalCommandResult{Stdout: `["192.0.2.45"]`}, nil, true
		}
		return core.LocalCommandResult{}, nil, false
	}
	b := testBackend(runner)
	var probedHost string
	b.sshReady = func(_ context.Context, target *core.SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
		probedHost = target.Host
		return nil
	}
	cfg := b.cfg
	cfg.TargetOS = core.TargetLinux
	_ = b.restoreCheckpointCachesAfterFailure(testCheckpointVMName, cfg, core.SSHTarget{TargetOS: core.TargetLinux}, []hypervDetachedCacheVolume{{}})

	if probedHost != "192.0.2.45" {
		t.Fatalf("Linux cache recovery did not wait for SSH readiness on the guest address: host=%q", probedHost)
	}
	for _, call := range runner.calls {
		if strings.Contains(commandScript(call), "Invoke-Command") {
			t.Fatalf("Linux cache recovery used Windows PowerShell Direct readiness: %s", commandScript(call))
		}
	}
}
