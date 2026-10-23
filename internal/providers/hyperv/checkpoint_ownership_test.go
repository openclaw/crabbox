package hyperv

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func reassignClaim(t *testing.T, b *backend, leaseID, slug, vmName, repo string) {
	t.Helper()
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, instanceScope(vmName), b.cfg.Pond, repo, b.cfg.IdleTimeout, true, core.Server{}, core.SSHTarget{}); err != nil {
		t.Fatalf("reassign claim: %v", err)
	}
}

func TestForkRollbackRetainsResourcesWhenClaimWasReassigned(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	runner := &recordingRunner{respond: func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if strings.Contains(commandScript(req), "Import-VM") {
			return jsonResult(t, checkpointImportOutput{ID: "99999999-8888-7777-6666-555555555555"}), nil, true
		}
		return core.LocalCommandResult{}, nil, true
	}}
	b := testBackend(runner)
	record := checkpointForkRecord(paths, metadata)
	other := t.TempDir()
	var forkLease core.LeaseTarget
	_, err := b.Acquire(t.Context(), core.AcquireRequest{
		CheckpointSource: &record,
		Repo:             core.Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedSlug:    "reassigned-fork",
		OnAcquired: func(lease core.LeaseTarget) error {
			forkLease = lease
			reassignClaim(t, b, lease.LeaseID, "other", lease.Server.CloudID, other)
			return errors.New("fork failed after reassignment")
		},
	})
	if err == nil {
		t.Fatal("expected fork failure")
	}
	if script := findScript(runner.calls, "Get-CrabboxImportedVM"); script != "" {
		t.Fatalf("rollback destroyed a fork whose claim was reassigned: %s", script)
	}
	claims, listErr := core.ListLeaseClaims()
	if listErr != nil || len(claims) != 1 || claims[0].LeaseID != forkLease.LeaseID || claims[0].RepoRoot != other {
		t.Fatalf("reassigned claim was removed or rewritten: %#v err=%v", claims, listErr)
	}
}

func TestOrdinaryCheckpointRefusesSourceReassignedAfterResolution(t *testing.T) {
	setCheckpointTestState(t)
	runner := &recordingRunner{respond: func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		return core.LocalCommandResult{}, nil, true
	}}
	b := testBackend(runner)
	persistCheckpointSource(t, b)
	claim, exists, err := core.ReadLeaseClaimWithPresence(testCheckpointLeaseID)
	if err != nil || !exists {
		t.Fatalf("read source claim: %v exists=%v", err, exists)
	}
	server := checkpointSourceServer(b)
	core.SetServerLeaseClaimSnapshot(&server, claim, true)
	reassignClaim(t, b, testCheckpointLeaseID, "other", testCheckpointVMName, t.TempDir())

	_, err = (Provider{}).CreateNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{
		Config:      b.cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		Server:      server,
		Target:      core.SSHTarget{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
		LeaseID:     testCheckpointLeaseID,
		Name:        "stale",
		ArtifactDir: t.TempDir(),
		Strategy:    "disk-snapshot",
	})
	if err == nil {
		t.Fatal("checkpoint of a reassigned source must be refused")
	}
	if script := findScript(runner.calls, "Checkpoint-VM"); script != "" {
		t.Fatalf("snapshot ran against reassigned source: %s", script)
	}
}

func TestCreateNativeCheckpointFinalPersistFailureDoesNotReturnRemovedImage(t *testing.T) {
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
			snapshotName := regexp.MustCompile(`-SnapshotName '([^']+)'`).FindStringSubmatch(script)
			if len(snapshotName) != 2 {
				t.Fatalf("export script has no snapshot name: %s", script)
			}
			return jsonResult(t, checkpointCreateOutput{
				SourceVMID: testCheckpointVMID, SnapshotID: testCheckpointSnapshot,
				SnapshotName: snapshotName[1], ExportedConfig: configPath,
			}), nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	persistCheckpointSource(t, b)
	calls := 0
	result, err := (Provider{}).CreateNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{
		Config:      b.cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		Server:      checkpointSourceServer(b),
		Target:      core.SSHTarget{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
		LeaseID:     testCheckpointLeaseID,
		Name:        "persist-fails",
		ArtifactDir: artifactDir,
		Strategy:    "disk-snapshot",
		Persist: func(core.NativeCheckpointCreateResult) error {
			calls++
			if calls == 2 {
				return errors.New("store unavailable")
			}
			return nil
		},
	})
	if err == nil {
		t.Fatal("expected final persist failure")
	}
	if calls != 2 {
		t.Fatalf("Persist calls=%d, want the final persist to be reached (2)", calls)
	}
	if result.Image.State == "available" || result.Image.ResourceID != "" {
		t.Fatalf("rolled-back export still advertised as available: %#v", result.Image)
	}
}

func TestRemoveImportedCheckpointRegistrationCapturesDisksBeforeRemoveVM(t *testing.T) {
	setCheckpointTestState(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	if err := b.removeImportedCheckpointRegistration(context.Background(), "fork-vm"); err != nil {
		t.Fatal(err)
	}
	script := findScript(runner.calls, "Remove-VM")
	capture := strings.Index(script, "$disks += $disk.Path")
	remove := strings.Index(script, "Remove-VM -VM")
	cleanup := strings.Index(script, "foreach ($path in $disks)")
	if capture < 0 || remove < 0 || cleanup < 0 || !(capture < remove && remove < cleanup) {
		t.Fatalf("disk paths must be captured before Remove-VM and deleted after: %s", script)
	}
}
