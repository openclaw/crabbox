package hyperv

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	testCheckpointLeaseID  = "cbx_checkpoint1234"
	testCheckpointVMName   = "crabbox-checkpoint-1234"
	testCheckpointVMID     = "11111111-2222-3333-4444-555555555555"
	testCheckpointSnapshot = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

func TestNativeCheckpointCapabilityPrefersProductionCheckpointForAuto(t *testing.T) {
	capability, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{
		Config:   core.Config{Provider: providerName, TargetOS: core.TargetWindows},
		Target:   core.SSHTarget{TargetOS: core.TargetWindows},
		Strategy: "auto",
	})
	if !ok || capability.Kind != hypervCheckpointKind || !capability.Direct || !capability.PreferredForAuto {
		t.Fatalf("capability=%#v ok=%v", capability, ok)
	}
}

func TestCreateNativeCheckpointExportsProductionMetadata(t *testing.T) {
	setCheckpointTestState(t)
	artifactDir := t.TempDir()
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Get-VM checkpoint query"):
			return core.LocalCommandResult{}, nil, false
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 2}), nil, true
		case strings.Contains(script, "Export-VMSnapshot"):
			configPath := filepath.Join(artifactDir, "hyperv", "exported", "Virtual Machines", "checkpoint.vmcx")
			if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, []byte("vmcx"), 0o600); err != nil {
				t.Fatal(err)
			}
			return jsonResult(t, checkpointCreateOutput{
				SourceVMID:     testCheckpointVMID,
				SnapshotID:     testCheckpointSnapshot,
				SnapshotName:   checkpointNameFromScript(script),
				ExportedConfig: configPath,
			}), nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	persistCheckpointSource(t, b)
	var journal []core.NativeCheckpointCreateResult
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	result, err := (Provider{}).CreateNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{
		Config:      b.cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		Server:      checkpointSourceServer(b),
		Target:      core.SSHTarget{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
		LeaseID:     testCheckpointLeaseID,
		Name:        "release-ready",
		RepoName:    "my-app",
		ArtifactDir: artifactDir,
		Strategy:    "disk-snapshot",
		Persist: func(result core.NativeCheckpointCreateResult) error {
			if len(journal) == 0 && findScript(runner.calls, "Export-VMSnapshot") != "" {
				t.Fatal("checkpoint mutation preceded cleanup journal")
			}
			journal = append(journal, result)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("CreateNativeCheckpoint: %v", err)
	}
	if result.Image.Kind != hypervCheckpointKind || result.Image.ID != testCheckpointSnapshot || result.Image.ResourceID == "" {
		t.Fatalf("image=%#v", result.Image)
	}
	if len(journal) != 2 || journal[0].Metadata[checkpointMetadataSourceVMID] != testCheckpointVMID ||
		journal[0].Metadata[checkpointMetadataSnapshotName] == "" || journal[0].Image.ID != journal[0].Metadata[checkpointMetadataSnapshotName] || journal[0].ArtifactDir != artifactDir ||
		journal[1].Image.ID != testCheckpointSnapshot || result.ArtifactDir != artifactDir {
		t.Fatalf("cleanup and completion journal=%#v", journal)
	}
	if result.Metadata[checkpointMetadataCheckpointType] != "ProductionOnly" ||
		result.Metadata[checkpointMetadataSourceVMID] != testCheckpointVMID ||
		result.Metadata[checkpointMetadataArtifactDir] != artifactDir {
		t.Fatalf("metadata=%#v", result.Metadata)
	}
	if _, err := os.Stat(result.Metadata[checkpointMetadataExportConfig]); err != nil {
		t.Fatalf("exported config: %v", err)
	}
	createScript := findScript(runner.calls, "Export-VMSnapshot")
	for _, expected := range []string{
		"-CheckpointType ProductionOnly",
		"$_.Directory.Name -eq 'Virtual Machines'",
		"$configs.Count -ne 1",
	} {
		if !strings.Contains(createScript, expected) {
			t.Fatalf("create script missing %q: %s", expected, createScript)
		}
	}
}

func TestCreateNativeCheckpointJournalFailurePreventsMutation(t *testing.T) {
	setCheckpointTestState(t)
	runner := &recordingRunner{respond: func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		if strings.Contains(script, "Select-Object Name,@{Name='ID'") {
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 2}), nil, true
		}
		t.Fatalf("journal failure dispatched a mutating command: %s", script)
		return core.LocalCommandResult{}, nil, true
	}}
	b := testBackend(runner)
	persistCheckpointSource(t, b)
	failure := errors.New("journal unavailable")
	_, err := (Provider{}).CreateNativeCheckpoint(t.Context(), core.NativeCheckpointCreateRequest{
		Config: b.cfg, Runtime: b.rt, ArtifactDir: t.TempDir(),
		Server: checkpointSourceServer(b), LeaseID: testCheckpointLeaseID,
		Persist: func(core.NativeCheckpointCreateResult) error { return failure },
	})
	var notSubmitted core.NativeCheckpointNotSubmittedError
	if !errors.Is(err, failure) || !errors.As(err, &notSubmitted) {
		t.Fatalf("journal failure=%v", err)
	}
}

func TestRunUnderClaimFenceAttestsNonSubmissionWhenClaimChanged(t *testing.T) {
	setCheckpointTestState(t)
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	persistCheckpointSource(t, b)
	stale, exists, err := core.ReadLeaseClaimWithPresence(testCheckpointLeaseID)
	if err != nil || !exists {
		t.Fatalf("read claim: exists=%v err=%v", exists, err)
	}
	if _, err := core.UpdateLeaseClaimTouchIfUnchanged(t.Context(), testCheckpointLeaseID, stale, nil, time.Now().Add(time.Minute), nil); err != nil {
		t.Fatalf("touch claim: %v", err)
	}
	ran := false
	err = runUnderClaimFence(testCheckpointLeaseID, stale, func() error { ran = true; return nil })
	var notSubmitted core.NativeCheckpointNotSubmittedError
	if err == nil || ran || !errors.As(err, &notSubmitted) {
		t.Fatalf("stale claim: err=%v ran=%v", err, ran)
	}
	fresh, _, _ := core.ReadLeaseClaimWithPresence(testCheckpointLeaseID)
	failure := errors.New("callback failed")
	err = runUnderClaimFence(testCheckpointLeaseID, fresh, func() error { return failure })
	if !errors.Is(err, failure) || errors.As(err, &notSubmitted) {
		t.Fatalf("callback failure must not be attested as unsubmitted: %v", err)
	}
}

func TestApplyNativeCheckpointForkFlagsExplicitSwitchWins(t *testing.T) {
	values := core.HyperVConfigFlagValues{}
	sw := "Private"
	values.Switch = &sw
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"explicit", []string{"--hyperv-switch", "Private"}, "Private"},
		{"unset", nil, "Default Switch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("fork", flag.ContinueOnError)
			fs.String("hyperv-switch", "", "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			cfg := core.Config{}
			cfg.HyperV.Switch = "Default Switch"
			if err := (Provider{}).ApplyNativeCheckpointForkFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if cfg.HyperV.Switch != tc.want {
				t.Fatalf("switch=%q want %q", cfg.HyperV.Switch, tc.want)
			}
		})
	}
}

func TestImportCheckpointVMCatchCapturesDisksBeforeRemoveVM(t *testing.T) {
	setCheckpointTestState(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	_, _ = b.importCheckpointVM(context.Background(), `C:\export\vm.vmcx`, "fork-vm", false)
	script := findScript(runner.calls, "Import-VM")
	catch := strings.Index(script, "} catch {")
	if catch < 0 {
		t.Fatalf("missing catch: %s", script)
	}
	tail := script[catch:]
	capture := strings.Index(tail, "Get-VMHardDiskDrive")
	remove := strings.Index(tail, "Remove-VM")
	cleanup := strings.Index(tail, "Remove-Item")
	if capture < 0 || remove < 0 || cleanup < 0 || !(capture < remove && remove < cleanup) {
		t.Fatalf("import rollback must capture disks before Remove-VM and delete them after: %s", tail)
	}
	if !strings.Contains(tail, hypervVHDDir()) {
		t.Fatalf("disk cleanup must be scoped to the fork VHD root: %s", tail)
	}
}

func TestCreateNativeCheckpointSourceQueryFailureAttestsNonSubmission(t *testing.T) {
	setCheckpointTestState(t)
	failure := errors.New("Get-VM unavailable")
	runner := &recordingRunner{respond: func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		return core.LocalCommandResult{Stderr: "boom"}, failure, true
	}}
	b := testBackend(runner)
	persistCheckpointSource(t, b)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })
	persisted := false
	_, err := (Provider{}).CreateNativeCheckpoint(t.Context(), core.NativeCheckpointCreateRequest{
		Config: b.cfg, Runtime: b.rt, ArtifactDir: t.TempDir(),
		Server: checkpointSourceServer(b), LeaseID: testCheckpointLeaseID,
		Persist: func(core.NativeCheckpointCreateResult) error { persisted = true; return nil },
	})
	var notSubmitted core.NativeCheckpointNotSubmittedError
	if err == nil || !errors.As(err, &notSubmitted) || persisted {
		t.Fatalf("err=%v persisted=%v; a pre-mutation failure must attest non-submission", err, persisted)
	}
}

func TestCreateNativeCheckpointProductionFailureDoesNotFallback(t *testing.T) {
	setCheckpointTestState(t)
	artifactDir := t.TempDir()
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 2}), nil, true
		case strings.Contains(script, "Export-VMSnapshot"):
			return core.LocalCommandResult{Stderr: "production checkpoint failed"}, errors.New("exit 1"), true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	persistCheckpointSource(t, b)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	_, err := (Provider{}).CreateNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{
		Config:      b.cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		Server:      checkpointSourceServer(b),
		Target:      core.SSHTarget{TargetOS: core.TargetWindows},
		LeaseID:     testCheckpointLeaseID,
		ArtifactDir: artifactDir,
		Strategy:    "disk-snapshot",
	})
	if err == nil {
		t.Fatal("CreateNativeCheckpoint succeeded after production checkpoint failure")
	}
	var unsubmitted core.NativeCheckpointNotSubmittedError
	if !errors.As(err, &unsubmitted) {
		t.Fatalf("verified-clean export failure must resolve as not submitted so release is not blocked: %v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(commandScript(call), "CheckpointType Standard") {
			t.Fatalf("production checkpoint failure fell back to a standard checkpoint: %s", commandScript(call))
		}
	}
	if _, statErr := os.Stat(filepath.Join(artifactDir, "hyperv")); !os.IsNotExist(statErr) {
		t.Fatalf("failed create left export directory behind: %v", statErr)
	}
}

func TestCreateNativeCheckpointFailureWithUnverifiedCleanupStaysUnresolved(t *testing.T) {
	setCheckpointTestState(t)
	artifactDir := t.TempDir()
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 2}), nil, true
		case strings.Contains(script, "Export-VMSnapshot"):
			return core.LocalCommandResult{Stderr: "export failed"}, errors.New("exit 1"), true
		case strings.Contains(script, "Remove-VMSnapshot"):
			return core.LocalCommandResult{Stderr: "cleanup failed"}, errors.New("exit 1"), true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	persistCheckpointSource(t, b)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	_, err := (Provider{}).CreateNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{
		Config:      b.cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		Server:      checkpointSourceServer(b),
		Target:      core.SSHTarget{TargetOS: core.TargetWindows},
		LeaseID:     testCheckpointLeaseID,
		ArtifactDir: artifactDir,
		Strategy:    "disk-snapshot",
	})
	var unsubmitted core.NativeCheckpointNotSubmittedError
	if err == nil || errors.As(err, &unsubmitted) {
		t.Fatalf("an export failure whose snapshot cleanup failed must stay unresolved: %v", err)
	}
}

func TestCreateNativeCheckpointCanceledRequestStillCleansLiveSnapshot(t *testing.T) {
	setCheckpointTestState(t)
	artifactDir := t.TempDir()
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if strings.Contains(commandScript(req), "Select-Object Name,@{Name='ID'") {
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 2}), nil, true
		}
		return core.LocalCommandResult{}, nil, false
	}
	runner.blockUntilCtx = func(req core.LocalCommandRequest) bool {
		return strings.Contains(commandScript(req), "Export-VMSnapshot")
	}
	b := testBackend(runner)
	persistCheckpointSource(t, b)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (Provider{}).CreateNativeCheckpoint(ctx, core.NativeCheckpointCreateRequest{
		Config:      b.cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		Server:      checkpointSourceServer(b),
		Target:      core.SSHTarget{TargetOS: core.TargetWindows},
		LeaseID:     testCheckpointLeaseID,
		ArtifactDir: artifactDir,
		Strategy:    "disk-snapshot",
	})
	if err == nil {
		t.Fatal("CreateNativeCheckpoint succeeded with a canceled request")
	}
	if findCallIndex(runner.calls, "Remove-VMSnapshot") < 0 {
		t.Fatal("canceled create did not attempt live checkpoint cleanup with a fresh context")
	}
}

func TestVerifyNativeCheckpointUsesExportAfterSourceRelease(t *testing.T) {
	paths, metadata := createCheckpointArtifact(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if strings.Contains(commandScript(req), "source_missing") {
			return jsonResult(t, checkpointLiveStatus{State: "source_missing"}), nil, true
		}
		return core.LocalCommandResult{}, nil, false
	}
	result, err := (Provider{}).VerifyNativeCheckpoint(context.Background(), core.NativeCheckpointResourceRequest{
		Config:      testBackend(runner).cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		ArtifactDir: paths.artifactDir,
		Metadata:    metadata,
	})
	if err != nil {
		t.Fatalf("VerifyNativeCheckpoint: %v", err)
	}
	if result.ProviderState != "available_exported" || result.NextAction != "fork_or_delete" {
		t.Fatalf("verify result=%#v", result)
	}
}

func TestRestoreNativeCheckpointRefreshesClaimEndpoint(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 2}), nil, true
		case strings.Contains(script, "Restore-VMSnapshot"):
			return core.LocalCommandResult{}, nil, true
		case strings.Contains(script, "Select-Object -ExpandProperty IPAddresses"):
			return core.LocalCommandResult{Stdout: `["192.0.2.45"]`}, nil, true
		case strings.Contains(script, "ConvertTo-Json -InputObject $items"):
			return core.LocalCommandResult{Stdout: "[]"}, nil, true
		case strings.Contains(script, "Invoke-Command"):
			return core.LocalCommandResult{}, nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	persistCheckpointSource(t, b)
	lease, err := b.restoreNativeCheckpoint(context.Background(), core.NativeCheckpointRestoreRequest{
		Config:  b.cfg,
		Record:  checkpointForkRecord(paths, metadata),
		LeaseID: testCheckpointLeaseID,
		Repo:    core.Repo{Root: t.TempDir()},
		Reclaim: true,
	})
	if err != nil {
		t.Fatalf("restoreNativeCheckpoint: %v", err)
	}
	if lease.SSH.Host != "192.0.2.45" {
		t.Fatalf("restored SSH host=%q", lease.SSH.Host)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(testCheckpointLeaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("resolve refreshed claim: ok=%v err=%v", ok, err)
	}
	if claim.SSHHost != "192.0.2.45" {
		t.Fatalf("claim SSH host=%q", claim.SSHHost)
	}
}

func TestRestoreNativeCheckpointRejectsRepoConflictBeforeMutation(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	persistCheckpointSource(t, b)

	_, err := b.restoreNativeCheckpoint(context.Background(), core.NativeCheckpointRestoreRequest{
		Config:  b.cfg,
		Record:  checkpointForkRecord(paths, metadata),
		LeaseID: testCheckpointLeaseID,
		Repo:    core.Repo{Root: t.TempDir()},
	})
	if err == nil || !strings.Contains(err.Error(), "claimed by repo") {
		t.Fatalf("restore conflict err=%v", err)
	}
	if findCallIndex(runner.calls, "Restore-VMSnapshot") >= 0 {
		t.Fatal("restore mutated the VM before rejecting the repo ownership conflict")
	}
}

func TestRestoreNativeCheckpointRollsBackReclaimedLeaseOnFailure(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if strings.Contains(commandScript(req), "Select-Object Name,@{Name='ID'") {
			return core.LocalCommandResult{Stderr: "query failed"}, errors.New("exit 1"), true
		}
		return core.LocalCommandResult{}, nil, false
	}
	b := testBackend(runner)
	persistCheckpointSource(t, b)
	original, ok, err := core.ResolveLeaseClaimForProvider(testCheckpointLeaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("resolve original claim: ok=%v err=%v", ok, err)
	}

	_, err = b.restoreNativeCheckpoint(context.Background(), core.NativeCheckpointRestoreRequest{
		Config:  b.cfg,
		Record:  checkpointForkRecord(paths, metadata),
		LeaseID: testCheckpointLeaseID,
		Repo:    core.Repo{Root: t.TempDir()},
		Reclaim: true,
	})
	if err == nil {
		t.Fatal("restore succeeded after VM query failure")
	}
	restored, ok, resolveErr := core.ResolveLeaseClaimForProvider(testCheckpointLeaseID, providerName)
	if resolveErr != nil || !ok {
		t.Fatalf("resolve rolled-back claim: ok=%v err=%v", ok, resolveErr)
	}
	if restored.RepoRoot != original.RepoRoot {
		t.Fatalf("claim repo=%q, want rollback to %q", restored.RepoRoot, original.RepoRoot)
	}
}

func TestRestoreNativeCheckpointRollsBackWhenSnapshotRestoreFails(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 3}), nil, true
		case strings.Contains(script, "Restore-VMSnapshot"):
			return core.LocalCommandResult{Stderr: "restore failed"}, errors.New("exit 1"), true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	persistCheckpointSource(t, b)
	original, ok, err := core.ResolveLeaseClaimForProvider(testCheckpointLeaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("resolve original claim: ok=%v err=%v", ok, err)
	}

	_, err = b.restoreNativeCheckpoint(context.Background(), core.NativeCheckpointRestoreRequest{
		Config:  b.cfg,
		Record:  checkpointForkRecord(paths, metadata),
		LeaseID: testCheckpointLeaseID,
		Repo:    core.Repo{Root: t.TempDir()},
		Reclaim: true,
	})
	if err == nil {
		t.Fatal("restore succeeded after Restore-VMSnapshot failed")
	}
	restored, ok, resolveErr := core.ResolveLeaseClaimForProvider(testCheckpointLeaseID, providerName)
	if resolveErr != nil || !ok {
		t.Fatalf("resolve rolled-back claim: ok=%v err=%v", ok, resolveErr)
	}
	if restored.RepoRoot != original.RepoRoot {
		t.Fatalf("claim repo=%q, want rollback to %q", restored.RepoRoot, original.RepoRoot)
	}
}

func TestRestoreNativeCheckpointKeepsReclaimedLeaseAfterVMMutation(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 3}), nil, true
		case strings.Contains(script, "Restore-VMSnapshot"):
			return core.LocalCommandResult{}, nil, true
		case strings.Contains(script, "Select-Object -ExpandProperty IPAddresses"):
			return core.LocalCommandResult{Stdout: `["192.0.2.45"]`}, nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		return errors.New("ssh unavailable")
	}
	persistCheckpointSource(t, b)
	newRepo := t.TempDir()

	_, err := b.restoreNativeCheckpoint(context.Background(), core.NativeCheckpointRestoreRequest{
		Config:  b.cfg,
		Record:  checkpointForkRecord(paths, metadata),
		LeaseID: testCheckpointLeaseID,
		Repo:    core.Repo{Root: newRepo},
		Reclaim: true,
	})
	if err == nil {
		t.Fatal("restore succeeded when SSH readiness failed")
	}
	claim, ok, resolveErr := core.ResolveLeaseClaimForProvider(testCheckpointLeaseID, providerName)
	if resolveErr != nil || !ok {
		t.Fatalf("resolve retained claim: ok=%v err=%v", ok, resolveErr)
	}
	if claim.RepoRoot != newRepo {
		t.Fatalf("claim repo=%q, want mutated VM retained by %q", claim.RepoRoot, newRepo)
	}
}

func TestRestoreNativeCheckpointHoldsClaimFenceAcrossVMEffects(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	var b *backend
	reassigned := make(chan struct{})
	var once sync.Once
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			// A concurrent actor tries to reassign the lease while restore effects run.
			once.Do(func() {
				go func() {
					defer close(reassigned)
					other := t.TempDir()
					if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(testCheckpointLeaseID, "other", providerName, instanceScope(testCheckpointVMName), b.cfg.Pond, other, b.cfg.IdleTimeout, true, core.Server{}, core.SSHTarget{}); err == nil {
						record("claim-reassigned")
					}
				}()
			})
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 3}), nil, true
		case strings.Contains(script, "Restore-VMSnapshot"):
			time.Sleep(200 * time.Millisecond)
			record("snapshot-restored")
			return core.LocalCommandResult{}, nil, true
		case strings.Contains(script, "Start-VM"):
			record("vm-started")
			return core.LocalCommandResult{}, nil, true
		case strings.Contains(script, "Select-Object -ExpandProperty IPAddresses"):
			return core.LocalCommandResult{Stdout: `["192.0.2.45"]`}, nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b = testBackend(runner)
	b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		return errors.New("ssh unavailable")
	}
	persistCheckpointSource(t, b)

	_, _ = b.restoreNativeCheckpoint(context.Background(), core.NativeCheckpointRestoreRequest{
		Config:  b.cfg,
		Record:  checkpointForkRecord(paths, metadata),
		LeaseID: testCheckpointLeaseID,
		Repo:    core.Repo{Root: t.TempDir()},
		Reclaim: true,
	})
	<-reassigned
	mu.Lock()
	defer mu.Unlock()
	pos := map[string]int{}
	for i, event := range events {
		pos[event] = i
	}
	for _, effect := range []string{"snapshot-restored", "vm-started"} {
		if at, ok := pos[effect]; ok {
			if re, ok := pos["claim-reassigned"]; ok && re < at {
				t.Fatalf("claim was reassigned before %s completed: %v", effect, events)
			}
		}
	}
	if _, ok := pos["snapshot-restored"]; !ok {
		t.Fatalf("restore did not run under its own authority: %v", events)
	}
}

func TestAcquireCheckpointCallbackFailureRollsBackNewFork(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	var imported bool
	var callbackLease core.LeaseTarget
	runner := &recordingRunner{respond: func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		if strings.Contains(script, "Import-VM") {
			imported = true
			return jsonResult(t, checkpointImportOutput{ID: "99999999-8888-7777-6666-555555555555"}), nil, true
		}
		if strings.Contains(script, "Invoke-Command") || strings.Contains(script, "Get-VMNetworkAdapter -VMName") ||
			strings.Contains(script, "ConvertTo-Json -InputObject $items") {
			t.Fatalf("callback failure reached fork readiness: %s", script)
		}
		return core.LocalCommandResult{}, nil, true
	}}
	b := testBackend(runner)
	record := checkpointForkRecord(paths, metadata)
	failure := errors.New("acquisition owner rejected fork")
	calls := 0
	_, err := b.Acquire(t.Context(), core.AcquireRequest{
		CheckpointSource: &record,
		Repo:             core.Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedSlug:    "callback-fork",
		OnAcquired: func(lease core.LeaseTarget) error {
			calls++
			if !imported || lease.LeaseID == "" || lease.LeaseID == testCheckpointLeaseID ||
				lease.Server.CloudID == "" || lease.Server.CloudID == testCheckpointVMName ||
				lease.Server.CloudID != lease.Server.Labels["instance"] {
				t.Fatalf("callback received unvalidated fork identity: %#v", lease)
			}
			callbackLease = lease
			return failure
		},
	})
	if !errors.Is(err, failure) || calls != 1 {
		t.Fatalf("callback calls=%d error=%v", calls, err)
	}
	cleanup := findScript(runner.calls, "Remove-VM -VM $candidate")
	if !strings.Contains(cleanup, "$name='"+callbackLease.Server.CloudID+"'") ||
		strings.Contains(cleanup, testCheckpointVMName) {
		t.Fatal("callback failure did not target only the new imported registration")
	}
	claims, err := core.ListLeaseClaims()
	if err != nil || len(claims) != 0 {
		t.Fatalf("callback failure left fork claims: %#v error=%v", claims, err)
	}
	for _, path := range []string{
		filepath.Join(hypervVMDir(), callbackLease.Server.CloudID),
		filepath.Join(hypervVHDDir(), callbackLease.Server.CloudID),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("callback failure left fork storage %s: %v", path, err)
		}
	}
	if _, err := os.Stat(paths.config); err != nil {
		t.Fatalf("callback failure modified source checkpoint: %v", err)
	}
}

func TestForkNativeCheckpointCreatesFreshIdentityAndConnectsNetworkLast(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Import-VM"):
			return jsonResult(t, checkpointImportOutput{ID: "99999999-8888-7777-6666-555555555555"}), nil, true
		case strings.Contains(script, "Select-Object -ExpandProperty IPAddresses"):
			return core.LocalCommandResult{Stdout: `["192.0.2.55"]`}, nil, true
		case strings.Contains(script, "Invoke-Command"):
			return core.LocalCommandResult{}, nil, true
		case strings.Contains(script, "ConvertTo-Json -InputObject $items"):
			return core.LocalCommandResult{Stdout: "[]"}, nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	lease, err := b.ForkNativeCheckpoint(context.Background(), core.NativeCheckpointForkLifecycleRequest{
		Record:        checkpointForkRecord(paths, metadata),
		Repo:          core.Repo{Root: t.TempDir(), Name: "my-app"},
		Keep:          true,
		RequestedSlug: "checkpoint-fork",
	})
	if err != nil {
		t.Fatalf("ForkNativeCheckpoint: %v", err)
	}
	if lease.LeaseID == "" || lease.LeaseID == testCheckpointLeaseID || lease.SSH.Host != "192.0.2.55" {
		t.Fatalf("forked lease=%#v", lease)
	}
	importScript := findScript(runner.calls, "Import-VM")
	for _, expected := range []string{"-Copy", "-GenerateNewId", "-DynamicMacAddress", "Disconnect-VMNetworkAdapter", "Remove-VM -VM $vm"} {
		if !strings.Contains(importScript, expected) {
			t.Fatalf("import script missing %s: %s", expected, importScript)
		}
	}
	rotationIndex := findCallIndex(runner.calls, "Rename-Computer")
	restartIndex := findCallIndex(runner.calls, "Restart-VM")
	postRestartKeyIndex := findLastCallIndex(runner.calls, "ssh-keygen.exe")
	connectIndex := findCallIndex(runner.calls, "Connect-VMNetworkAdapter")
	if rotationIndex < 0 || restartIndex <= rotationIndex || postRestartKeyIndex <= restartIndex || connectIndex <= postRestartKeyIndex {
		t.Fatalf("network was not connected after post-reboot SSH identity rotation: rotation=%d restart=%d key=%d connect=%d", rotationIndex, restartIndex, postRestartKeyIndex, connectIndex)
	}
	rotationScript := commandScript(runner.calls[rotationIndex])
	keyPath, err := core.OptionalStoredTestboxKeyPath(lease.LeaseID)
	if err != nil {
		t.Fatalf("fork key path: %v", err)
	}
	publicKey, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatalf("fork public key: %v", err)
	}
	keyFields := strings.Fields(string(publicKey))
	if len(keyFields) < 2 {
		t.Fatalf("fork public key=%q", publicKey)
	}
	for _, expected := range []string{keyFields[0] + " " + keyFields[1], "ssh_host_*", "vnc.password", "MachineGuid", "[guid]::NewGuid()"} {
		if !strings.Contains(rotationScript, expected) {
			t.Fatalf("identity rotation missing %s: %s", expected, rotationScript)
		}
	}
	claim, ok, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !ok {
		t.Fatalf("read fork claim ok=%v err=%v", ok, err)
	}
	if claim.SSHHost != "192.0.2.55" {
		t.Fatalf("fork claim SSH host=%q", claim.SSHHost)
	}
}

func TestRemoveImportedCheckpointVMFindsPartialRegistrationByStorage(t *testing.T) {
	setCheckpointTestState(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)

	if err := b.removeImportedCheckpointVM(context.Background(), "crabbox-partial-import"); err != nil {
		t.Fatalf("removeImportedCheckpointVM: %v", err)
	}
	cleanupScript := findScript(runner.calls, "Get-CrabboxImportedVM")
	for _, expected := range []string{
		"ConfigurationLocation",
		"SnapshotFileLocation",
		"Get-VM -ErrorAction Stop",
		"Get-VMHardDiskDrive -VM $candidate -ErrorAction Stop",
		"partial checkpoint import cleanup left",
	} {
		if !strings.Contains(cleanupScript, expected) {
			t.Fatalf("partial import cleanup script missing %q: %s", expected, cleanupScript)
		}
	}
}

func TestRemoveImportedCheckpointVMReportsUnverifiedCleanup(t *testing.T) {
	setCheckpointTestState(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if strings.Contains(commandScript(req), "Get-CrabboxImportedVM") {
			return core.LocalCommandResult{Stderr: "registered VM remains"}, errors.New("cleanup failed"), true
		}
		return core.LocalCommandResult{}, nil, false
	}
	b := testBackend(runner)

	err := b.removeImportedCheckpointVM(context.Background(), "crabbox-partial-import")
	if err == nil || !strings.Contains(err.Error(), "remove partial Hyper-V checkpoint import") {
		t.Fatalf("cleanup error=%v", err)
	}
}

func TestDeleteNativeCheckpointIsIndependentAndIdempotent(t *testing.T) {
	paths, metadata := createCheckpointArtifact(t)
	sourceMarker := filepath.Join(t.TempDir(), "source.vm")
	if err := os.WriteFile(sourceMarker, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	req := core.NativeCheckpointResourceRequest{
		Config:      testBackend(runner).cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		ArtifactDir: paths.artifactDir,
		Metadata:    metadata,
	}
	if err := (Provider{}).DeleteNativeCheckpoint(context.Background(), req); err != nil {
		t.Fatalf("DeleteNativeCheckpoint: %v", err)
	}
	if _, err := os.Stat(paths.exportRoot); !os.IsNotExist(err) {
		t.Fatalf("export remains after delete: %v", err)
	}
	if _, err := os.Stat(sourceMarker); err != nil {
		t.Fatalf("checkpoint delete removed independent source marker: %v", err)
	}
	if err := (Provider{}).DeleteNativeCheckpoint(context.Background(), req); err != nil {
		t.Fatalf("idempotent DeleteNativeCheckpoint: %v", err)
	}
}

func TestDeleteNativeCheckpointCleansPendingJournalWithoutSnapshotID(t *testing.T) {
	paths, metadata := createCheckpointArtifact(t)
	delete(metadata, checkpointMetadataSnapshotID)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	req := core.NativeCheckpointResourceRequest{
		Config:      testBackend(runner).cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		ArtifactDir: paths.artifactDir,
		Metadata:    metadata,
	}
	if err := (Provider{}).DeleteNativeCheckpoint(context.Background(), req); err != nil {
		t.Fatalf("DeleteNativeCheckpoint pending journal: %v", err)
	}
	if _, err := os.Stat(paths.exportRoot); !os.IsNotExist(err) {
		t.Fatalf("export remains after pending cleanup: %v", err)
	}
	if findScript(runner.calls, "Remove-VMSnapshot") == "" {
		t.Fatal("pending cleanup did not remove the named snapshot")
	}
}

func TestDeleteNativeCheckpointRejectsPathTraversal(t *testing.T) {
	artifactDir := t.TempDir()
	victim := filepath.Join(filepath.Dir(artifactDir), "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(victim, "victim.vmcx")
	if err := os.WriteFile(config, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	metadata := checkpointMetadata(artifactDir, filepath.Join(artifactDir, "..", "victim"), config)
	err := (Provider{}).DeleteNativeCheckpoint(context.Background(), core.NativeCheckpointResourceRequest{
		Config:      testBackend(runner).cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		ArtifactDir: artifactDir,
		Metadata:    metadata,
	})
	if err == nil || !strings.Contains(err.Error(), "unowned") {
		t.Fatalf("path traversal delete err=%v", err)
	}
	if _, err := os.Stat(config); err != nil {
		t.Fatalf("path traversal validation removed victim: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("path traversal reached PowerShell: %#v", runner.calls)
	}
}

func TestReleaseStoragePreservesCheckpointExport(t *testing.T) {
	setCheckpointTestState(t)
	name := "crabbox-release-1234"
	vhdDir := hypervVHDDir()
	if err := os.MkdirAll(vhdDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vhdDir, name+".vhdx"), []byte("lease"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, _ := createCheckpointArtifact(t)
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	if err := b.removeVMStorage(name, nil); err != nil {
		t.Fatalf("removeVMStorage: %v", err)
	}
	if _, err := os.Stat(paths.config); err != nil {
		t.Fatalf("source release removed checkpoint export: %v", err)
	}
}

func TestCleanupSkipsCheckpointRestoreReservation(t *testing.T) {
	now := time.Now().UTC()
	server := core.Server{Status: "stopped", Labels: map[string]string{
		hypervCheckpointRestoreReservationLabel: core.LeaseLabelTime(now.Add(time.Minute)),
	}}
	claim := core.LeaseClaim{LeaseID: testCheckpointLeaseID, Labels: server.Labels}
	if cleanup, reason := shouldCleanup(server, claim, true, now); cleanup || reason != "checkpoint restore reserved" {
		t.Fatalf("cleanup=%v reason=%q", cleanup, reason)
	}
}

func setCheckpointTestState(t *testing.T) {
	t.Helper()
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
}

func persistCheckpointSource(t *testing.T, b *backend) {
	t.Helper()
	cfg := b.configForRun()
	server := checkpointSourceServer(b)
	lease := core.LeaseTarget{Server: server, LeaseID: testCheckpointLeaseID}
	req := core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}}
	if err := persistLease(testCheckpointLeaseID, "checkpoint-source", testCheckpointVMName, cfg, req, &lease); err != nil {
		t.Fatalf("persist source lease: %v", err)
	}
}

func checkpointSourceServer(b *backend) core.Server {
	cfg := b.configForRun()
	labels := core.DirectLeaseLabels(cfg, testCheckpointLeaseID, "checkpoint-source", providerName, "", true, time.Now().UTC())
	labels["instance"] = testCheckpointVMName
	labels["ssh_user"] = cfg.HyperV.User
	labels["work_root"] = cfg.HyperV.WorkRoot
	claim := core.LeaseClaim{
		LeaseID:       testCheckpointLeaseID,
		Slug:          "checkpoint-source",
		Provider:      providerName,
		ProviderScope: instanceScope(testCheckpointVMName),
		Labels:        labels,
	}
	return b.serverFromInstance(hypervVM{Name: testCheckpointVMName, State: 2}, claim, cfg)
}

func createCheckpointArtifact(t *testing.T) (checkpointArtifactPaths, map[string]string) {
	t.Helper()
	artifactDir := t.TempDir()
	exportRoot := filepath.Join(artifactDir, "hyperv")
	config := filepath.Join(exportRoot, "exported", "Virtual Machines", "checkpoint.vmcx")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("vmcx"), 0o600); err != nil {
		t.Fatal(err)
	}
	return checkpointArtifactPaths{artifactDir: artifactDir, exportRoot: exportRoot, config: config}, checkpointMetadata(artifactDir, exportRoot, config)
}

func checkpointMetadata(artifactDir, exportRoot, config string) map[string]string {
	return map[string]string{
		checkpointMetadataArtifactDir:    artifactDir,
		checkpointMetadataExportRoot:     exportRoot,
		checkpointMetadataExportConfig:   config,
		checkpointMetadataSourceLease:    testCheckpointLeaseID,
		checkpointMetadataSourceVM:       testCheckpointVMName,
		checkpointMetadataSourceVMID:     testCheckpointVMID,
		checkpointMetadataSnapshotID:     testCheckpointSnapshot,
		checkpointMetadataSnapshotName:   "checkpoint",
		checkpointMetadataCheckpointType: "ProductionOnly",
		checkpointMetadataTarget:         core.TargetWindows,
		checkpointMetadataProviderScope:  instanceScope(testCheckpointVMName),
		checkpointMetadataSSHUser:        "crabbox",
		checkpointMetadataWorkRoot:       `C:\crabbox`,
		checkpointMetadataSwitch:         "Default Switch",
	}
}

func checkpointForkRecord(paths checkpointArtifactPaths, metadata map[string]string) core.NativeCheckpointForkRecord {
	target, err := checkpointTarget(metadata)
	if err != nil {
		panic(err)
	}
	windowsMode := ""
	if target == core.TargetWindows {
		windowsMode = core.WindowsModeNormal
	}
	return core.NativeCheckpointForkRecord{
		Kind:        hypervCheckpointKind,
		ImageID:     testCheckpointSnapshot,
		Name:        metadata[checkpointMetadataSnapshotName],
		Resource:    paths.config,
		ArtifactDir: paths.artifactDir,
		TargetOS:    target,
		WindowsMode: windowsMode,
		Metadata:    metadata,
	}
}

func findScript(calls []core.LocalCommandRequest, needle string) string {
	index := findCallIndex(calls, needle)
	if index < 0 {
		return ""
	}
	return commandScript(calls[index])
}

func findCallIndex(calls []core.LocalCommandRequest, needle string) int {
	for i, call := range calls {
		if strings.Contains(commandScript(call), needle) {
			return i
		}
	}
	return -1
}

func findLastCallIndex(calls []core.LocalCommandRequest, needle string) int {
	for i := len(calls) - 1; i >= 0; i-- {
		if strings.Contains(commandScript(calls[i]), needle) {
			return i
		}
	}
	return -1
}

func jsonResult(t *testing.T, value any) core.LocalCommandResult {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return core.LocalCommandResult{Stdout: string(data)}
}

func checkpointNameFromScript(script string) string {
	const marker = "-SnapshotName '"
	start := strings.Index(script, marker)
	if start < 0 {
		return ""
	}
	start += len(marker)
	end := strings.Index(script[start:], "'")
	if end < 0 {
		return ""
	}
	return strings.ReplaceAll(script[start:start+end], "''", "'")
}
