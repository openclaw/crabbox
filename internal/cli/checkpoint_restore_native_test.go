package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type nativeRestoreContractProvider struct {
	testAWSProvider
	restore func(context.Context, NativeCheckpointRestoreRequest) (LeaseTarget, error)
}

func (p nativeRestoreContractProvider) RestoreNativeCheckpoint(ctx context.Context, req NativeCheckpointRestoreRequest) (LeaseTarget, error) {
	return p.restore(ctx, req)
}

type nativeRestoreContractBackend struct {
	*checkpointForkReleaseBackend
	requests []ResolveRequest
}

func (b *nativeRestoreContractBackend) Resolve(_ context.Context, req ResolveRequest) (LeaseTarget, error) {
	b.requests = append(b.requests, req)
	return LeaseTarget{LeaseID: b.leaseID, Server: Server{Provider: "aws", CloudID: "i-123"}, SSH: SSHTarget{Host: "restored.example.test", User: "guest", TargetOS: targetLinux}}, nil
}
func nativeRestoreContractSetup(t *testing.T) (checkpointStore, checkpointRecord, *nativeRestoreContractBackend) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	repo, err := findRepo()
	if err != nil {
		t.Fatal(err)
	}
	backend := &nativeRestoreContractBackend{checkpointForkReleaseBackend: &checkpointForkReleaseBackend{leaseID: "cbx_0123456789ab"}}
	if err = ClaimLeaseForRepoProviderScopePondEndpoint(backend.leaseID, "fixture", "aws", "fixture-scope", "", repo.Root, time.Hour, false, Server{Provider: "aws", CloudID: "i-123"}, SSHTarget{Host: "restored.example.test", TargetOS: targetLinux}); err != nil {
		t.Fatal(err)
	}
	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	record := checkpointRecord{ID: "chk_0123456789abcdef", Provider: "aws", Kind: checkpointKindAWSAMI, LeaseID: backend.leaseID, CaptureEstablished: true}
	record.Native.Provider = "aws"
	record.Native.ImageID = "native-exact"
	record.Native.Resource = "i-123"
	record.Native.State = "available"
	record.Native.Direct = true
	record, _, err = store.Reserve(record)
	if err != nil {
		t.Fatal(err)
	}
	original := providerRegistry["aws"]
	t.Cleanup(func() { providerRegistry["aws"] = original; testAWSBackendOverride = nil })
	testAWSBackendOverride = backend
	return store, record, backend
}
func TestNativeCheckpointRestoreOptionalHookUsesStatusOnlyAndSerializesDeletion(t *testing.T) {
	store, record, backend := nativeRestoreContractSetup(t)
	calls := 0
	providerRegistry["aws"] = nativeRestoreContractProvider{restore: func(ctx context.Context, req NativeCheckpointRestoreRequest) (LeaseTarget, error) {
		calls++
		claim, exists, set := ServerLeaseClaimSnapshot(req.Lease.Server)
		if !set || !exists || claim.CloudID != "i-123" || claim.RepoRoot != req.Repo.Root || req.Resource.Image.ID != "native-exact" || !req.Resource.CaptureEstablished {
			t.Fatalf("lost source/capture authority: %+v", req)
		}
		blocked := make(chan error, 1)
		go func() {
			blocked <- store.withRecord(record.ID, false, func(checkpointRecord) error { return errors.New("deletion was admitted") })
		}()
		var busy checkpointBusyError
		if err := <-blocked; !errors.As(err, &busy) {
			t.Fatalf("restore did not exclude deletion: %v", err)
		}
		return req.Lease, nil
	}}
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.checkpointRestore(t.Context(), []string{record.ID, "--provider", "aws", "--id", backend.leaseID}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || backend.acquireCalls != 0 || backend.releaseCount != 0 || len(backend.requests) != 1 || !backend.requests[0].StatusOnly || !backend.requests[0].NoLocalStateMutations {
		t.Fatalf("restore unexpectedly acquired, booted or mutated a claim: %+v calls=%d", backend, calls)
	}
	if !strings.Contains(stdout.String(), "checkpoint restored") {
		t.Fatalf("output=%s", stdout.String())
	}
}
func TestNativeCheckpointRestoreFailureAndDryRunPreserveUsage(t *testing.T) {
	for _, mode := range []string{"failed", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			store, record, backend := nativeRestoreContractSetup(t)
			old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
			record.LastUsedAt = old
			if err := store.Write(record); err != nil {
				t.Fatal(err)
			}
			calls := 0
			providerRegistry["aws"] = nativeRestoreContractProvider{restore: func(_ context.Context, req NativeCheckpointRestoreRequest) (LeaseTarget, error) {
				calls++
				if req.DryRun != (mode == "dry-run") {
					t.Fatal("dry-run missing")
				}
				if mode == "failed" {
					return req.Lease, errors.New("verified workload transport unavailable")
				}
				return req.Lease, nil
			}}
			args := []string{record.ID, "--provider", "aws", "--id", backend.leaseID}
			if mode == "dry-run" {
				args = append(args, "--dry-run")
			}
			var out bytes.Buffer
			err := (App{Stdout: &out, Stderr: io.Discard}).checkpointRestore(t.Context(), args)
			if (err != nil) != (mode == "failed") || calls != 1 {
				t.Fatalf("mode=%s calls=%d err=%v", mode, calls, err)
			}
			current, _, err := store.Read(record.ID)
			if err != nil || current.LastUsedAt != old {
				t.Fatalf("unsuccessful restore recorded use: %+v %v", current, err)
			}
			if mode == "failed" && strings.Contains(out.String(), "checkpoint restored") {
				t.Fatal("failure reported success")
			}
		})
	}
}
func TestNativeCheckpointRestoreRejectsForeignClaimAndReclaimBeforeHook(t *testing.T) {
	for _, scenario := range []string{"foreign", "reclaim", "workdir"} {
		t.Run(scenario, func(t *testing.T) {
			_, record, backend := nativeRestoreContractSetup(t)
			calls := 0
			providerRegistry["aws"] = nativeRestoreContractProvider{restore: func(_ context.Context, r NativeCheckpointRestoreRequest) (LeaseTarget, error) {
				calls++
				return r.Lease, nil
			}}
			args := []string{record.ID, "--provider", "aws", "--id", backend.leaseID}
			switch scenario {
			case "foreign":
				if err := WithDurableLeaseClaimLock(backend.leaseID, func(claim *LeaseClaim, _ bool, persist func() error) error {
					claim.RepoRoot = filepath.Join(t.TempDir(), "different-repo")
					return persist()
				}); err != nil {
					t.Fatal(err)
				}
			case "reclaim":
				args = append(args, "--reclaim")
			case "workdir":
				args = append(args, "--workdir", "/other")
			}
			err := (App{Stdout: io.Discard, Stderr: io.Discard}).checkpointRestore(t.Context(), args)
			if err == nil || calls != 0 || backend.acquireCalls != 0 {
				t.Fatalf("unsafe restore scenario=%s calls=%d err=%v", scenario, calls, err)
			}
		})
	}
}
func TestNativeCheckpointCaptureAttestationSurvivesResourceConversion(t *testing.T) {
	_, record, _ := nativeRestoreContractSetup(t)
	if !nativeCheckpointResourceRequest(record).CaptureEstablished {
		t.Fatal("capture attestation lost")
	}
	record.CaptureEstablished = false
	if nativeCheckpointResourceRequest(record).CaptureEstablished {
		t.Fatal("resource identity manufactured capture attestation")
	}
}
