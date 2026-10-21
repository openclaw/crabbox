package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
)

type checkpointAttestationTestProvider struct {
	testAWSProvider
	result NativeCheckpointCreateResult
}

func (p checkpointAttestationTestProvider) NativeCheckpointWorkdir(req NativeCheckpointWorkdirRequest) string {
	return remoteJoin(req.Config, req.LeaseID, req.RepoName)
}
func (p checkpointAttestationTestProvider) CreateNativeCheckpoint(context.Context, NativeCheckpointCreateRequest) (NativeCheckpointCreateResult, error) {
	return p.result, nil
}
func (checkpointAttestationTestProvider) VerifyNativeCheckpoint(context.Context, NativeCheckpointResourceRequest) (NativeCheckpointVerifyResult, error) {
	return NativeCheckpointVerifyResult{}, nil
}
func (checkpointAttestationTestProvider) DeleteNativeCheckpoint(context.Context, NativeCheckpointResourceRequest) error {
	return nil
}

type checkpointHyperVAdmissionProvider struct {
	checkpointAttestationTestProvider
	backend *checkpointHyperVAdmissionBackend
}

func (checkpointHyperVAdmissionProvider) Spec() ProviderSpec {
	spec := testAWSProvider{}.Spec()
	spec.Name = "hyperv"
	return spec
}

func (p checkpointHyperVAdmissionProvider) Configure(Config, Runtime) (Backend, error) {
	return p.backend, nil
}

func (checkpointHyperVAdmissionProvider) NativeCheckpointCapability(NativeCheckpointRequest) (NativeCheckpointCapability, bool) {
	return NativeCheckpointCapability{Kind: checkpointKindHyperV, Direct: true, PreferredForAuto: true}, true
}

type checkpointHyperVAdmissionBackend struct {
	checkpointForkReleaseBackend
}

func (checkpointHyperVAdmissionBackend) Spec() ProviderSpec {
	return checkpointHyperVAdmissionProvider{}.Spec()
}

func (b *checkpointHyperVAdmissionBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	lease, err := b.checkpointForkReleaseBackend.Resolve(ctx, req)
	lease.Server.Provider = "hyperv"
	return lease, err
}

func TestCheckpointCreateAdmitsHyperVNativeKind(t *testing.T) {
	original, existed := providerRegistry["hyperv"]
	t.Cleanup(func() {
		if existed {
			providerRegistry["hyperv"] = original
		} else {
			delete(providerRegistry, "hyperv")
		}
	})
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	for _, mode := range []string{"native", "auto"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			backend := &checkpointHyperVAdmissionBackend{checkpointForkReleaseBackend{leaseID: "cbx_abcdef123456"}}
			providerRegistry["hyperv"] = checkpointHyperVAdmissionProvider{
				backend: backend,
				checkpointAttestationTestProvider: checkpointAttestationTestProvider{result: NativeCheckpointCreateResult{
					CaptureEstablished: true,
					Image: NativeCheckpointImage{
						ID: "checkpoint-exact", Provider: "hyperv", Kind: checkpointKindHyperV, State: "available", Direct: true,
					},
				}},
			}
			var stdout bytes.Buffer
			app := App{Stdout: &stdout, Stderr: io.Discard}
			if err := app.checkpointCreate(t.Context(), []string{
				"--provider", "hyperv", "--target", "linux", "--id", backend.leaseID,
				"--mode", mode, "--wait", "--json",
			}); err != nil {
				t.Fatal(err)
			}
			var record checkpointRecord
			if err := json.Unmarshal(stdout.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			store, err := defaultCheckpointStore()
			if err != nil {
				t.Fatal(err)
			}
			saved, _, err := store.Read(record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Kind != checkpointKindHyperV || saved.Provider != "hyperv" || saved.Native.ImageID != "checkpoint-exact" || !saved.CaptureEstablished {
				t.Fatalf("native Hyper-V checkpoint not preserved: %+v", saved)
			}
		})
	}
}

func TestCheckpointCreatePreservesProviderSuccessAndAttestation(t *testing.T) {
	original := providerRegistry["aws"]
	t.Cleanup(func() { providerRegistry["aws"] = original; testAWSBackendOverride = nil })
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	for _, test := range []struct {
		state       string
		established bool
	}{
		{"active", true}, {"active", false}, {"creating", false}, {"available", false}, {"available", true},
	} {
		name := test.state + "/omitted"
		if test.established {
			name = test.state + "/attested"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			backend := &checkpointForkReleaseBackend{leaseID: "cbx_abcdef123456"}
			testAWSBackendOverride = backend
			providerRegistry["aws"] = checkpointAttestationTestProvider{result: NativeCheckpointCreateResult{
				CaptureEstablished: test.established,
				Image:              NativeCheckpointImage{ID: "ami-exact", Provider: "aws", Kind: checkpointKindAWSAMI, State: test.state, Direct: true},
			}}
			var stdout bytes.Buffer
			app := App{Stdout: &stdout, Stderr: io.Discard}
			if err := app.checkpointCreate(t.Context(), []string{"--provider", "aws", "--id", backend.leaseID, "--mode", "native", "--wait=false", "--json"}); err != nil {
				t.Fatalf("provider success changed: %v", err)
			}
			var record map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			value, present := record["captureEstablished"]
			if present != test.established || present && value != true {
				t.Fatalf("attestation=%v present=%v, want %v", value, present, test.established)
			}
			native := record["native"].(map[string]any)
			if native["imageId"] != "ami-exact" || native["state"] != test.state {
				t.Fatalf("provider result changed: %v", native)
			}
			store, err := defaultCheckpointStore()
			if err != nil {
				t.Fatal(err)
			}
			saved, _, err := store.Read(record["id"].(string))
			if err != nil || saved.CaptureEstablished != test.established {
				t.Fatalf("persisted attestation=%v err=%v", saved.CaptureEstablished, err)
			}
		})
	}
}
