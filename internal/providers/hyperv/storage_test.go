package hyperv

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func useWindowsHyperVHost(t *testing.T) {
	t.Helper()
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })
}

// isolateHyperVProfile points the legacy profile storage at a temporary home.
func isolateHyperVProfile(t *testing.T) {
	t.Helper()
	testutil.IsolateUserDirs(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

// seedLeaseFiles writes the files Hyper-V leaves for a lease: the root
// differencing disk, an automatic checkpoint disk, and the VM configuration.
func seedLeaseFiles(t *testing.T, storage hypervStorage, name string) []string {
	t.Helper()
	paths := []string{
		filepath.Join(storage.vhdDir, name+".vhdx"),
		filepath.Join(storage.vhdDir, name+"_0123abcd-0123-4567-89ab-0123456789ab.avhdx"),
		filepath.Join(storage.vmDir, name, "Virtual Machines", "lease.vmcx"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("lease"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

func requireRemoved(t *testing.T, paths []string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("lease file %s still present: %v", path, err)
		}
	}
}

func requirePresent(t *testing.T, paths []string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("file %s outside the lease's recorded storage was removed: %v", path, err)
		}
	}
}

func TestAcquireRejectsRelativeStorageBeforeHostEffects(t *testing.T) {
	isolateHyperVProfile(t)
	useWindowsHyperVHost(t)
	for _, storage := range []string{"relative-storage", `E:relative`} {
		t.Run(storage, func(t *testing.T) {
			runner := &recordingRunner{}
			b := testBackend(runner)
			b.cfg.HyperV.Storage = storage
			_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
			if err == nil || !strings.Contains(err.Error(), "hyperv.storage must be an absolute host directory") {
				t.Fatalf("Acquire err=%v, want absolute storage refusal", err)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("relative storage reached host commands: %d calls", len(runner.calls))
			}
			if claims, err := core.ListLeaseClaims(); err != nil || len(claims) != 0 {
				t.Fatalf("relative storage left claims=%#v err=%v", claims, err)
			}
		})
	}
}

// A lease keeps the storage it was created in: changing hyperv.storage
// afterwards must neither strand its disks nor touch the new location.
func TestLeaseCreatedUnderStorageIsReleasedFromItAfterSettingChange(t *testing.T) {
	isolateHyperVProfile(t)
	useWindowsHyperVHost(t)
	rootA := filepath.Join(t.TempDir(), "hyperv-a")
	rootB := filepath.Join(t.TempDir(), "hyperv-b")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}, onRun: func(req core.LocalCommandRequest) {
		if strings.Contains(commandScript(req), "Get-VMNetworkAdapter") {
			cancel()
		}
	}}
	b := testBackend(runner)
	b.cfg.HyperV.Storage = rootA + string(filepath.Separator)
	if _, err := b.Acquire(ctx, core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "storage-move", Keep: true}); err == nil {
		t.Fatal("Acquire unexpectedly succeeded")
	}
	claims, err := core.ListLeaseClaims()
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims=%#v err=%v, want the retained lease", claims, err)
	}
	claim := claims[0]
	name := instanceNameFromClaim(claim)
	want := hypervStorageUnderRoot(rootA)
	if claim.Labels[storageVHDDirLabel] != want.vhdDir || claim.Labels[storageVMDirLabel] != want.vmDir {
		t.Fatalf("claim storage labels=%q/%q, want %q/%q", claim.Labels[storageVHDDirLabel], claim.Labels[storageVMDirLabel], want.vhdDir, want.vmDir)
	}
	var newVHD, newVM string
	for _, call := range runner.calls {
		switch script := commandScript(call); {
		case strings.Contains(script, "New-VHD "):
			newVHD = script
		case strings.Contains(script, "New-VM "):
			newVM = script
		}
	}
	if !strings.Contains(newVHD, core.PowerShellQuote(filepath.Join(want.vhdDir, name+".vhdx"))) {
		t.Fatalf("lease disk was not created under the configured storage: %q", newVHD)
	}
	if !strings.Contains(newVM, "-Path "+core.PowerShellQuote(want.vmDir)) {
		t.Fatalf("VM configuration was not placed under the configured storage: %q", newVM)
	}
	for _, dir := range []string{want.vhdDir, want.vmDir} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("configured storage directory %s was not created: %v", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Dir(hypervVHDDir())); !os.IsNotExist(err) {
		t.Fatalf("configured storage still created profile storage: %v", err)
	}

	leaseFiles := seedLeaseFiles(t, want, name)
	others := append(seedLeaseFiles(t, hypervStorageUnderRoot(rootB), name), seedLeaseFiles(t, legacyHyperVStorage(), name)...)
	moved := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	moved.cfg.HyperV.Storage = rootB
	resolved, err := moved.Resolve(context.Background(), core.ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatalf("Resolve release-only: %v", err)
	}
	if err := moved.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: resolved}); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	requireRemoved(t, leaseFiles)
	requirePresent(t, others)
	if claims, err := core.ListLeaseClaims(); err != nil || len(claims) != 0 {
		t.Fatalf("claims after release=%#v err=%v", claims, err)
	}
}

func TestReleaseAndCleanupRemoveOnlyRecordedLeaseStorage(t *testing.T) {
	for _, op := range []string{"release", "cleanup"} {
		for _, tc := range []struct {
			name    string
			labels  func(recorded hypervStorage) map[string]string
			legacy  bool
			invalid bool
		}{
			{name: "recorded", labels: func(recorded hypervStorage) map[string]string {
				labels := map[string]string{}
				recorded.recordLabels(labels)
				return labels
			}},
			{name: "unrecorded legacy lease", labels: func(hypervStorage) map[string]string { return map[string]string{} }, legacy: true},
			{name: "relative recorded directory", invalid: true, labels: func(recorded hypervStorage) map[string]string {
				return map[string]string{storageVHDDirLabel: "Virtual Hard Disks", storageVMDirLabel: recorded.vmDir}
			}},
			{name: "half recorded", invalid: true, labels: func(recorded hypervStorage) map[string]string {
				return map[string]string{storageVHDDirLabel: recorded.vhdDir}
			}},
		} {
			t.Run(op+"/"+tc.name, func(t *testing.T) {
				isolateHyperVProfile(t)
				useWindowsHyperVHost(t)
				const leaseID = "cbx_storageowned"
				const name = "crabbox-storage-owned"
				recorded := hypervStorageUnderRoot(filepath.Join(t.TempDir(), "recorded"))
				configuredRoot := filepath.Join(t.TempDir(), "configured")

				var stderr bytes.Buffer
				b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
				b.cfg.HyperV.Storage = configuredRoot
				b.rt.Stderr = &stderr
				labels := tc.labels(recorded)
				labels["instance"], labels["lease"], labels["state"] = name, leaseID, "ready"
				claim := core.LeaseClaim{LeaseID: leaseID, Slug: "storage-owned", Provider: providerName, ProviderScope: instanceScope(name), Labels: labels}
				cfg := b.configForRun()
				lease := core.LeaseTarget{Server: b.serverFromInstance(hypervVM{Name: name, State: 2}, claim, cfg), LeaseID: leaseID}
				if err := persistLease(leaseID, claim.Slug, name, cfg, core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}}, &lease); err != nil {
					t.Fatalf("persistLease: %v", err)
				}
				recordedFiles := seedLeaseFiles(t, recorded, name)
				legacyFiles := seedLeaseFiles(t, legacyHyperVStorage(), name)
				configuredFiles := seedLeaseFiles(t, hypervStorageUnderRoot(configuredRoot), name)

				var err error
				if op == "release" {
					resolved, resolveErr := b.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
					if resolveErr != nil {
						t.Fatalf("Resolve release-only: %v", resolveErr)
					}
					err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: resolved})
				} else {
					err = b.Cleanup(context.Background(), core.CleanupRequest{})
				}

				requirePresent(t, configuredFiles)
				claims, listErr := core.ListLeaseClaims()
				if listErr != nil {
					t.Fatal(listErr)
				}
				if tc.invalid {
					refused := err != nil && strings.Contains(err.Error(), "invalid storage directories")
					if op == "cleanup" {
						refused = err == nil && strings.Contains(stderr.String(), "invalid storage directories")
					}
					if !refused {
						t.Fatalf("%s err=%v stderr=%q, want invalid storage refusal", op, err, stderr.String())
					}
					requirePresent(t, append(recordedFiles, legacyFiles...))
					if len(claims) != 1 {
						t.Fatalf("refused %s dropped the claim: %#v", op, claims)
					}
					return
				}
				if err != nil {
					t.Fatalf("%s: %v", op, err)
				}
				if tc.legacy {
					requireRemoved(t, legacyFiles)
					requirePresent(t, recordedFiles)
				} else {
					requireRemoved(t, recordedFiles)
					requirePresent(t, legacyFiles)
				}
				if len(claims) != 0 {
					t.Fatalf("claims after %s=%#v", op, claims)
				}
			})
		}
	}
}
