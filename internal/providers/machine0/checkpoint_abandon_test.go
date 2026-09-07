package machine0

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestMachine0AbandonSourceAccountCannotAuthorizeImageAccount(t *testing.T) {
	for _, tc := range []struct {
		name, disposition, imageAccount, cleanupAccount string
		wantAbsent                                      bool
	}{
		{"cleanup account owns source", "abandon", "original-account", "fixture-account", true},
		{"image account cannot replace cleanup account", "abandon", "fixture-account", "", false},
		{"image account cannot override switched cleanup account", "abandon", "fixture-account", "other-account", false},
		{"cleanup account cannot replace image account", "retire", "", "fixture-account", false},
		{"cleanup account cannot override switched image account", "retire", "other-account", "fixture-account", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{machines: []machine{}}
			b := testBackendWithAPI(api)
			absent, err := b.CheckpointSourceAbsent(context.Background(), core.CheckpointSourceRequest{
				Capture: core.NativeCheckpointCapture{SourceDisposition: tc.disposition, SourceID: "original-vm", SourceName: "original-source"},
				Resource: core.NativeCheckpointResourceRequest{Metadata: map[string]string{
					metadataAccountID:              tc.imageAccount,
					metadataSourceCleanupAccountID: tc.cleanupAccount,
				}},
			})
			if absent != tc.wantAbsent || (err == nil) != tc.wantAbsent {
				t.Fatalf("source absence=%t err=%v, want authorized=%t", absent, err, tc.wantAbsent)
			}
			if len(api.removed)+len(api.savedImages)+len(api.started)+len(api.stopped)+len(api.removedImage) != 0 {
				t.Fatal("source absence observation mutated cloud resources")
			}
		})
	}
}

func TestMachine0AbandonPreparationRequiresCurrentOwnedSource(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*backend, *fakeAPI, *core.NativeCheckpointCreateRequest)
	}{
		{name: "owned source"},
		{name: "missing source", alter: func(_ *backend, api *fakeAPI, _ *core.NativeCheckpointCreateRequest) { api.machine = machine{} }},
		{name: "replacement source", alter: func(_ *backend, api *fakeAPI, _ *core.NativeCheckpointCreateRequest) {
			api.machine.ID = "replacement-vm"
		}},
		{name: "renamed source", alter: func(_ *backend, api *fakeAPI, _ *core.NativeCheckpointCreateRequest) {
			api.machine.Name = "renamed-source"
		}},
		{name: "stale generation", alter: func(_ *backend, _ *fakeAPI, req *core.NativeCheckpointCreateRequest) {
			req.Capture.SourceRevision = "stale-generation"
		}},
		{name: "different source intent", alter: func(_ *backend, _ *fakeAPI, req *core.NativeCheckpointCreateRequest) {
			req.Capture.SourceIntent = "another-intent"
		}},
		{name: "different metadata source", alter: func(_ *backend, _ *fakeAPI, req *core.NativeCheckpointCreateRequest) {
			req.Metadata[metadataSourceMachine] = "another-source"
		}},
		{name: "different metadata operation", alter: func(_ *backend, _ *fakeAPI, req *core.NativeCheckpointCreateRequest) {
			req.Metadata["crabbox_checkpoint"] = "another-checkpoint"
		}},
		{name: "different image name", alter: func(_ *backend, _ *fakeAPI, req *core.NativeCheckpointCreateRequest) {
			req.Metadata[metadataImageName] = "another-image"
		}},
		{name: "known image ID", alter: func(_ *backend, _ *fakeAPI, req *core.NativeCheckpointCreateRequest) {
			req.Metadata[metadataImageID] = "known-image"
		}},
		{name: "known image version", alter: func(_ *backend, _ *fakeAPI, req *core.NativeCheckpointCreateRequest) {
			req.Metadata[metadataImageVersion] = "1"
		}},
		{name: "suspend policy", alter: func(b *backend, _ *fakeAPI, _ *core.NativeCheckpointCreateRequest) {
			b.cfg.Machine0.ReleasePolicy = "suspend"
		}},
		{name: "account unavailable", alter: func(_ *backend, api *fakeAPI, _ *core.NativeCheckpointCreateRequest) {
			api.accountErr = errors.New("account unavailable")
		}},
		{name: "account changes during observation", alter: func(_ *backend, api *fakeAPI, _ *core.NativeCheckpointCreateRequest) {
			api.getFn = func(context.Context, string) (machine, error) {
				api.accountID = "switched-account"
				return api.machine, nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, api, acquire := fixedMachine0TestFixture(t)
			lease, err := b.Acquire(context.Background(), acquire)
			if err != nil {
				t.Fatal(err)
			}
			claim := readFixedMachine0Claim(t, lease.LeaseID)
			req := core.NativeCheckpointCreateRequest{
				CheckpointID: "chk_abandon_source", LeaseID: lease.LeaseID, Server: lease.Server,
				Metadata: map[string]string{metadataAccountID: "original-image-account"},
				Capture: &core.NativeCheckpointCapture{
					SourceDisposition: "abandon", Phase: "prepared", SourceID: claim.CloudID,
					SourceName: claim.Labels["machine0_name"], SourceScope: claim.ProviderScope,
					SourceRevision: claim.Revision, SourceClaimedAt: claim.ClaimedAt, SourceIntent: claim.FixedCreateIntent.Fingerprint,
				},
			}
			if tc.alter != nil {
				tc.alter(b, api, &req)
			}
			original := maps.Clone(req.Metadata)
			guarded := &abandonObservationAPI{fakeAPI: api}
			b.api = guarded
			metadata, err := b.PrepareNativeCheckpointAbandon(context.Background(), req)
			if tc.alter == nil {
				if err != nil || metadata[metadataSourceCleanupAccountID] != "fixture-account" || metadata[metadataAccountID] != "original-image-account" || metadata["machine0_original_submission"] != "unknown" || metadata[metadataImageName] != "crabbox-abandon_source" {
					t.Fatalf("source cleanup provenance=%v err=%v", metadata, err)
				}
			} else if err == nil || metadata != nil {
				t.Fatalf("unsafe source observation admitted: metadata=%v err=%v", metadata, err)
			}
			if !maps.Equal(req.Metadata, original) || !reflect.DeepEqual(claim, readFixedMachine0Claim(t, lease.LeaseID)) {
				t.Fatal("source observation changed original metadata or claim")
			}
			if guarded.imageReads != 0 || len(api.created) != 1 || len(api.started)+len(api.stopped)+len(api.removed)+len(api.savedImages)+len(api.removedImage) != 0 {
				t.Fatal("source-only preparation inspected images or mutated provider resources")
			}
		})
	}
}

func TestMachine0AbandonCannotReadOrDeleteOriginalImage(t *testing.T) {
	api := &abandonObservationAPI{fakeAPI: &fakeAPI{images: []machineImage{{ID: "original-image", Name: "original-image-name"}}}}
	metadata := map[string]string{
		metadataAccountID: "fixture-account", metadataSourceCleanupAccountID: "fixture-account",
		metadataImageID: "original-image", metadataImageName: "original-image-name", metadataImageVersion: "1",
		metadataSourceMachine: "original-source", "crabbox_checkpoint": "chk_abandon_image", "crabbox_lease": "cbx_abcdef123456",
	}
	api.imageDetail = machineImageDetail{Image: api.images[0], Versions: []machineImageVersion{{
		Version: 1, Metadata: map[string]any{"crabbox_checkpoint": "chk_abandon_image", "crabbox_lease": "cbx_abcdef123456", "crabbox_source": "original-source"},
	}}}
	b := testBackendWithAPI(api.fakeAPI)
	b.api = api
	req := core.NativeCheckpointResourceRequest{
		Metadata: metadata,
		Image:    core.NativeCheckpointImage{Provider: providerName, Kind: core.CheckpointKindMachine0, Direct: true, ResourceID: "original-image"},
	}
	if _, _, err := b.loadCheckpointImage(context.Background(), req); err != nil {
		t.Fatalf("fixture must have a valid original image identity: %v", err)
	}
	api.imageReads = 0
	req.Capture = &core.NativeCheckpointCapture{SourceDisposition: "abandon"}
	if _, _, err := b.loadCheckpointImage(context.Background(), req); err == nil || !strings.Contains(err.Error(), "abandonment") {
		t.Fatalf("abandoned image read authorized: %v", err)
	}
	if err := b.deleteNativeCheckpoint(context.Background(), req); err == nil || !strings.Contains(err.Error(), "abandonment") {
		t.Fatalf("abandoned image delete authorized: %v", err)
	}
	if api.imageReads != 0 || len(api.removedImage) != 0 {
		t.Fatal("abandonment reached the image API")
	}
	// Even without the operation journal, copied source-cleanup provenance
	// cannot establish absence in the original image's unrecorded account.
	req.Capture = nil
	delete(metadata, metadataAccountID)
	api.images = nil
	if _, _, err := b.loadCheckpointImage(context.Background(), req); err == nil || errors.Is(err, core.ErrNativeCheckpointAbsent) {
		t.Fatalf("cleanup account established original image absence: %v", err)
	}
}

type abandonObservationAPI struct {
	*fakeAPI
	imageReads int
}

func (api *abandonObservationAPI) ListImages(ctx context.Context) ([]machineImage, error) {
	api.imageReads++
	return api.fakeAPI.ListImages(ctx)
}

func (api *abandonObservationAPI) GetImage(ctx context.Context, name string) (machineImageDetail, error) {
	api.imageReads++
	return api.fakeAPI.GetImage(ctx, name)
}
