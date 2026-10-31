package hyperv

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func registryFlushCreateRequest(t *testing.T, b *backend, runner *recordingRunner) core.NativeCheckpointCreateRequest {
	t.Helper()
	return core.NativeCheckpointCreateRequest{
		Config:      b.cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		Server:      checkpointSourceServer(b),
		Target:      core.SSHTarget{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
		LeaseID:     testCheckpointLeaseID,
		Name:        "flushed",
		ArtifactDir: t.TempDir(),
		Strategy:    "disk-snapshot",
	}
}

// A production checkpoint must include registry writes that finished before
// create and restore to a booting guest, so the source guest is prepared before
// Checkpoint-VM.
func TestCreateNativeCheckpointPreparesGuestBeforeCheckpoint(t *testing.T) {
	setCheckpointTestState(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if strings.Contains(commandScript(req), "Select-Object Name,@{Name='ID'") {
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 2}), nil, true
		}
		return core.LocalCommandResult{}, nil, false
	}
	b := testBackend(runner)
	persistCheckpointSource(t, b)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	_, _ = (Provider{}).CreateNativeCheckpoint(context.Background(), registryFlushCreateRequest(t, b, runner))
	flush := findCallIndex(runner.calls, "RegFlushKey")
	checkpoint := findCallIndex(runner.calls, "Checkpoint-VM")
	if flush < 0 || checkpoint < 0 || flush > checkpoint {
		t.Fatalf("guest preparation=%d must run before Checkpoint-VM=%d", flush, checkpoint)
	}
	if script := commandScript(runner.calls[flush]); !strings.Contains(script, "-VMName '"+testCheckpointVMName+"'") {
		t.Fatalf("guest preparation did not target the source VM: %s", script)
	}
}

// When the guest cannot be prepared, create stops before any VM change
// and attests that nothing was submitted.
func TestCreateNativeCheckpointGuestPreparationFailureAttestsNonSubmission(t *testing.T) {
	setCheckpointTestState(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 2}), nil, true
		case strings.Contains(script, "RegFlushKey"):
			return core.LocalCommandResult{Stderr: "PowerShell Direct unavailable"}, errors.New("exit 1"), true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	b.guestRetryBackoff = 0
	persistCheckpointSource(t, b)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })
	req := registryFlushCreateRequest(t, b, runner)

	_, err := b.createNativeCheckpoint(context.Background(), req)
	var unsubmitted core.NativeCheckpointNotSubmittedError
	if !errors.As(err, &unsubmitted) {
		t.Fatalf("preparation failure err=%v, want non-submission", err)
	}
	for _, verb := range []string{"Set-VM", "Checkpoint-VM", "Export-VMSnapshot"} {
		if findCallIndex(runner.calls, verb) >= 0 {
			t.Fatalf("create ran %s after guest preparation failed", verb)
		}
	}
	if _, statErr := os.Stat(filepath.Join(req.ArtifactDir, "hyperv")); !os.IsNotExist(statErr) {
		t.Fatalf("create left an export directory after guest preparation failed: %v", statErr)
	}
}
