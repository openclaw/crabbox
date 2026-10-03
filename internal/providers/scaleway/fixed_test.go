package scaleway

import (
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"

	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (api *fakeInstanceAPI) GetImage(req *instance.GetImageRequest, _ ...scw.RequestOption) (*instance.GetImageResponse, error) {
	return &instance.GetImageResponse{Image: &instance.Image{ID: req.ImageID, RootVolume: &instance.VolumeSummary{ID: "root-snapshot", VolumeType: instance.VolumeVolumeTypeLSSD}}}, nil
}

func (api *fakeInstanceAPI) CreateVolume(req *instance.CreateVolumeRequest, _ ...scw.RequestOption) (*instance.CreateVolumeResponse, error) {
	api.f.createVolumeCalls++
	volume := &instance.Volume{ID: "44444444-4444-4444-4444-444444444444", Name: req.Name, Project: *req.Project, Zone: req.Zone, Tags: req.Tags}
	api.f.volumes = map[string]*instance.Volume{volume.ID: volume}
	if api.f.afterVolume != nil {
		api.f.afterVolume()
	}
	if api.f.createVolumeReplyErr != nil {
		return nil, api.f.createVolumeReplyErr
	}
	if api.f.createVolumeEmptyReply {
		return &instance.CreateVolumeResponse{}, nil
	}
	return &instance.CreateVolumeResponse{Volume: volume}, nil
}

func (api *fakeInstanceAPI) ListVolumes(_ *instance.ListVolumesRequest, _ ...scw.RequestOption) (*instance.ListVolumesResponse, error) {
	var volumes []*instance.Volume
	for _, volume := range api.f.volumes {
		volumes = append(volumes, volume)
	}
	return &instance.ListVolumesResponse{Volumes: volumes}, nil
}

func fixedRequest(t *testing.T) core.AcquireRequest {
	t.Helper()
	return core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123456", RequestedSlug: "fixed", Repo: core.Repo{Root: t.TempDir()}, Keep: true}
}

func TestFixedScalewayAcquireReplay(t *testing.T) {
	for _, interruption := range []string{"none", "key response lost", "root tag publication", "volume response lost", "volume response empty", "cloud-init"} {
		t.Run(interruption, func(t *testing.T) {
			backend, fake := newTestBackend(t)
			req := fixedRequest(t)
			switch interruption {
			case "key response lost":
				fake.createKeyErr = errors.New("response lost")
			case "root tag publication":
				fake.updateErr = errors.New("response lost")
			case "volume response lost":
				fake.createVolumeReplyErr = errors.New("response lost")
			case "volume response empty":
				fake.createVolumeEmptyReply = true
			case "cloud-init":
				fake.userDataErr = errors.New("request interrupted")
			}
			first, err := backend.Acquire(t.Context(), req)
			if interruption == "none" && err != nil || interruption != "none" && err == nil {
				t.Fatalf("initial acquire: %v", err)
			}
			fake.createKeyErr, fake.updateErr = nil, nil
			fake.createVolumeReplyErr, fake.userDataErr, fake.createVolumeEmptyReply = nil, nil, false
			replay, err := backend.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if fake.createCalls != 1 || fake.createVolumeCalls != 1 || len(fake.keys) != 1 || replay.LeaseID != req.RequestedLeaseID ||
				first.LeaseID != "" && first.Server.CloudID != replay.Server.CloudID {
				t.Fatalf("duplicate allocation: servers=%d keys=%d", fake.createCalls, len(fake.keys))
			}
		})
	}
}

func TestFixedScalewayVolumeJournalPrecedesServer(t *testing.T) {
	backend, fake := newTestBackend(t)
	req := fixedRequest(t)
	fake.afterCreate = func() {
		claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
		if err != nil || claim.Labels[rootVolumeLabel] != *fake.lastCreate.Volumes["0"].ID || claim.Labels["scaleway_ssh_key_id"] == "" {
			t.Fatalf("server submission has no durable children: %+v %v", claim, err)
		}
		if !maps.Equal(labelsFromTags(fake.lastCreate.Tags), labelsFromTags(tagsFromLabels(claim.Labels))) {
			t.Fatal("server creation lost the volume/key journal labels")
		}
	}
	if _, err := backend.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
}

func interruptFixedBeforeServer(t *testing.T, backend *Backend, fake *fakeScalewayClient, req core.AcquireRequest) core.LeaseClaim {
	t.Helper()
	interrupted := errors.New("interrupted before server submission")
	fake.beforeCreate = func() { panic(interrupted) }
	func() {
		defer func() {
			if got := recover(); got != interrupted {
				t.Fatalf("interruption: got %v, want %v", got, interrupted)
			}
		}()
		_, _ = backend.Acquire(t.Context(), req)
	}()
	fake.beforeCreate = nil
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.Attempt["server"] != "submitted" || claim.CloudID != "" ||
		claim.Labels[rootVolumeLabel] == "" || claim.Labels["scaleway_ssh_key_id"] == "" || fake.createCalls != 0 || fake.server != nil ||
		fake.createVolumeCalls != 1 || fake.createKeyCalls != 1 {
		t.Fatalf("interrupted journal: %+v err=%v", claim, err)
	}
	return claim
}

func TestFixedScalewayAdmittedWithoutServer(t *testing.T) {
	for _, operation := range []string{"replay", "stop"} {
		t.Run(operation, func(t *testing.T) {
			backend, fake := newTestBackend(t)
			req := fixedRequest(t)
			before := interruptFixedBeforeServer(t, backend, fake, req)
			volumeID, keyID := before.Labels[rootVolumeLabel], before.Labels["scaleway_ssh_key_id"]
			if operation == "replay" {
				for range 2 {
					if _, err := backend.Acquire(t.Context(), req); err != nil {
						t.Fatal(err)
					}
				}
				if fake.createCalls != 1 || fake.server.Volumes["0"].ID != volumeID || *fake.lastCreate.Volumes["0"].ID != volumeID ||
					fake.volumes[volumeID].Server.ID != fake.server.ID || len(fake.keys) != 1 || fake.keys[0].ID != keyID {
					t.Fatal("replay did not attach the original journaled children exactly once")
				}
			} else {
				status, err := backend.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, StatusOnly: true, NoLocalStateMutations: true})
				if err != nil || status.Server.CloudID != "" {
					t.Fatalf("incomplete status: %+v %v", status, err)
				}
				for range 2 {
					lease, err := backend.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
					if err != nil {
						t.Fatal(err)
					}
					if err := backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
						t.Fatal(err)
					}
				}
				terminal, err := core.ReadLeaseClaim(req.RequestedLeaseID)
				if err != nil || terminal.FixedCreateIntent.State != "released" || len(fake.volumes) != 0 || len(fake.keys) != 0 ||
					fake.server != nil || len(fake.servers) != 0 || fake.createCalls != 0 || fake.deletedServer {
					t.Fatalf("child-only stop: %+v err=%v volumes=%v keys=%v", terminal, err, fake.volumes, fake.keys)
				}
				if _, err := backend.Acquire(t.Context(), req); core.ExitCodeForError(err, 1) != 4 {
					t.Fatalf("terminal replay: %v", err)
				}
			}
			if fake.createVolumeCalls != 1 || fake.createKeyCalls != 1 {
				t.Fatal("recovery allocated replacement children")
			}
		})
	}
}

func TestFixedScalewayAdmittedRecoveryRefusesUncertainOwnership(t *testing.T) {
	for _, change := range []string{"inventory error", "empty inventory reply", "volume attempt", "attached volume", "key project", "server attempt", "server tags"} {
		t.Run(change, func(t *testing.T) {
			backend, fake := newTestBackend(t)
			req := fixedRequest(t)
			before := interruptFixedBeforeServer(t, backend, fake, req)
			volume := fake.volumes[before.Labels[rootVolumeLabel]]
			inventoryErr := errors.New("inventory unavailable")
			switch change {
			case "inventory error":
				fake.listErr = inventoryErr
			case "empty inventory reply":
				fake.listEmptyReply = true
			case "volume attempt":
				labels := labelsFromTags(volume.Tags)
				labels["fixed_attempt"] = "other-attempt"
				volume.Tags = tagsFromLabels(labels)
			case "attached volume":
				volume.Server = &instance.ServerSummary{ID: "other-server"}
			case "key project":
				fake.keys[0].ProjectID = "other-project"
			case "server attempt", "server tags":
				labels := maps.Clone(before.Labels)
				labels["fixed_attempt"] = "other-attempt"
				fake.server = testServer("foreign-server", core.LeaseProviderName(before.LeaseID, before.Slug), tagsFromLabels(labels), "203.0.113.10")
				if change == "server tags" {
					fake.server.Tags = nil
				}
			}
			_, acquireErr := backend.Acquire(t.Context(), req)
			releaseErr := backend.releaseFixed(t.Context(), fake, before)
			if acquireErr == nil || releaseErr == nil {
				t.Fatalf("unproven recovery: acquire=%v release=%v", acquireErr, releaseErr)
			}
			if change == "inventory error" {
				_, resolveErr := backend.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
				if !errors.Is(acquireErr, inventoryErr) || !errors.Is(releaseErr, inventoryErr) || !errors.Is(resolveErr, inventoryErr) {
					t.Fatalf("lost inventory error: acquire=%v release=%v resolve=%v", acquireErr, releaseErr, resolveErr)
				}
			}
			claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || claim.FixedCreateIntent.State == "released" || fake.createCalls != 0 || fake.createVolumeCalls != 1 ||
				fake.createKeyCalls != 1 || len(fake.volumes) != 1 || len(fake.keys) != 1 || fake.deletedKey || fake.deletedServer {
				t.Fatalf("refusal changed resources or released claim: %+v err=%v", claim, err)
			}
		})
	}
}

func TestFixedScalewayUsesEffectiveMachineType(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "inherited default", true: "explicit override"}[explicit], func(t *testing.T) {
			backend, fake := newTestBackend(t)
			backend.cfg.ServerType, backend.cfg.ServerTypeExplicit = "DEV1-M", explicit
			if _, err := backend.Acquire(t.Context(), fixedRequest(t)); err != nil {
				t.Fatal(err)
			}
			want := "DEV1-S"
			if explicit {
				want = "DEV1-M"
			}
			if fake.lastCreate.CommercialType != want {
				t.Fatalf("created type=%s, want %s", fake.lastCreate.CommercialType, want)
			}
		})
	}
}

func TestFixedScalewayIntentConflicts(t *testing.T) {
	for _, field := range []string{"repository", "keep", "slug", "type", "image", "security group", "SSH user", "work root", "idle timeout", "hostname template"} {
		t.Run(field, func(t *testing.T) {
			backend, fake := newTestBackend(t)
			req := fixedRequest(t)
			if field == "hostname template" {
				backend.cfg.Tailscale.Enabled, backend.cfg.Tailscale.AuthKey = true, "fixture-key"
				backend.cfg.Tailscale.HostnameTemplate = "box-{slug}"
			}
			if _, err := backend.Acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "repository":
				req.Repo.Root = t.TempDir()
			case "keep":
				req.Keep = false
			case "slug":
				req.RequestedSlug = "other"
			case "type":
				backend.cfg.Scaleway.Type = "DEV1-M"
			case "image":
				backend.cfg.Scaleway.Image = "other-image"
			case "security group":
				backend.cfg.Scaleway.SecurityGroup = "sg-other"
			case "SSH user":
				backend.cfg.SSHUser = "alice"
			case "work root":
				backend.cfg.WorkRoot = "/other"
			case "idle timeout":
				backend.cfg.IdleTimeout++
			case "hostname template":
				backend.cfg.Tailscale.HostnameTemplate = "other-{slug}"
			}
			_, err := backend.Acquire(t.Context(), req)
			if core.ExitCodeForError(err, 1) != 4 || !strings.Contains(err.Error(), "lease_id_conflict") || fake.createCalls != 1 || len(fake.keys) != 1 {
				t.Fatalf("changed intent: %v servers=%d keys=%d", err, fake.createCalls, len(fake.keys))
			}
		})
	}
}

func TestFixedScalewayLostCreateResponseRequiredRecovery(t *testing.T) {
	backend, fake := newTestBackend(t)
	req := fixedRequest(t)
	fake.createResponseWithoutServer = true
	if _, err := backend.Acquire(t.Context(), req); err == nil {
		t.Fatal("expected missing create response")
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	item, err := backend.loadFixedServer(t.Context(), fake, claim)
	if err != nil || item.ID != fake.server.ID {
		t.Fatalf("lease-tag inventory reconciliation: %v", err)
	}
	t.Logf("inventory recovered server=%s; claim root=%q; live root label=%q; attached root=%s", item.ID,
		claim.Labels[rootVolumeLabel], labelsFromTags(item.Tags)[rootVolumeLabel], item.Volumes["0"].ID)
	fake.createResponseWithoutServer = false
	if _, err := backend.Acquire(t.Context(), req); err != nil {
		t.Fatalf("required lost-response recovery failed: %v (server creates=%d, keys=%d)", err, fake.createCalls, len(fake.keys))
	}
}

func TestFixedScalewayLifecycle(t *testing.T) {
	for _, interruption := range []string{"none", "key reply", "volume reply", "server reply", "SSH key cleanup"} {
		t.Run(interruption, func(t *testing.T) {
			backend, fake := newTestBackend(t)
			req := fixedRequest(t)
			if !backend.SupportsRequestedLeaseID() {
				t.Fatal("fixed lease capability missing")
			}
			switch interruption {
			case "key reply":
				fake.createKeyErr = errors.New("reply lost")
			case "volume reply":
				fake.createVolumeReplyErr = errors.New("reply lost")
			case "server reply":
				fake.createResponseWithoutServer = true
			}
			_, err := backend.Acquire(t.Context(), req)
			if (interruption == "none" || interruption == "SSH key cleanup") && err != nil {
				t.Fatal(err)
			}
			before, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil {
				t.Fatal(err)
			}
			status, err := backend.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, StatusOnly: true, NoLocalStateMutations: true})
			if err != nil || status.LeaseID != req.RequestedLeaseID || status.SSH.Key == "" {
				t.Fatalf("status: %+v %v", status, err)
			}
			after, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("read-only status changed the claim")
			}
			if interruption == "none" {
				var authorizer core.StatusTouchClaimAuthorizer = backend
				if err := authorizer.AuthorizeStatusTouchClaim(t.Context(), status, before); err != nil {
					t.Fatalf("CLI heartbeat project scope admission: %v", err)
				}
				wrongScope := core.CloneLeaseClaim(before)
				wrongScope.ProviderScope = "other-project"
				if err := authorizer.AuthorizeStatusTouchClaim(t.Context(), status, wrongScope); err == nil {
					t.Fatal("CLI heartbeat accepted another project")
				}
				lease, err := backend.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, Repo: req.Repo})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := backend.Touch(t.Context(), core.TouchRequest{Lease: lease, State: "ready"}); err != nil {
					t.Fatal(err)
				}
			}
			lease, err := backend.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if interruption == "SSH key cleanup" {
				fake.deleteKeyErr = errors.New("cleanup unavailable")
				if err := backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
					t.Fatal("expected key cleanup failure")
				}
				fake.deleteKeyErr, fake.server, fake.getErr = nil, nil, &scw.ResourceNotFoundError{}
				lease, err = backend.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
			terminal, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || terminal.FixedCreateIntent.State != "released" || !fake.deletedKey || len(fake.volumes) != 0 {
				t.Fatalf("release: %+v err=%v keyDeleted=%v volumes=%v", terminal, err, fake.deletedKey, fake.volumes)
			}
			retained, err := backend.RetainLeaseClaimAfterReleaseWithClaim(lease, before)
			if !retained || err != nil {
				t.Fatalf("tombstone not retained: %v %v", retained, err)
			}
			if _, err := backend.Acquire(t.Context(), req); core.ExitCodeForError(err, 1) != 4 {
				t.Fatalf("terminal replay: %v", err)
			}
			lease, err = backend.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFixedScalewayVolumeRecoveryRefusesChangedOwnership(t *testing.T) {
	for _, change := range []string{"tags", "project", "zone", "attached", "duplicate", "missing"} {
		t.Run(change, func(t *testing.T) {
			backend, fake := newTestBackend(t)
			req := fixedRequest(t)
			fake.createVolumeReplyErr = errors.New("reply lost")
			if _, err := backend.Acquire(t.Context(), req); err == nil {
				t.Fatal("expected lost volume reply")
			}
			volume := fake.volumes["44444444-4444-4444-4444-444444444444"]
			switch change {
			case "tags":
				volume.Tags = []string{"crabbox"}
			case "project":
				volume.Project = "other-project"
			case "zone":
				volume.Zone = "fr-par-2"
			case "attached":
				volume.Server = &instance.ServerSummary{ID: "another-server"}
			case "duplicate":
				other := *volume
				other.ID = "55555555-5555-5555-5555-555555555555"
				fake.volumes[other.ID] = &other
			case "missing":
				fake.volumes = nil
			}
			fake.createVolumeReplyErr = nil
			if _, err := backend.Acquire(t.Context(), req); core.ExitCodeForError(err, 1) != 4 {
				t.Fatalf("changed volume accepted: %v", err)
			}
			if fake.createCalls != 0 || fake.createVolumeCalls != 1 || len(fake.keys) != 1 || fake.deletedKey {
				t.Fatal("recovery changed allocation or cleanup authority")
			}
		})
	}
}

type fixedScopeClient struct {
	Client
	project string
}

func (c fixedScopeClient) ProjectID() string { return c.project }

func TestFixedScalewayKeyAndScopePolicy(t *testing.T) {
	for _, change := range []string{"missing key", "project", "provider", "changed server nonce", "missing intent"} {
		t.Run(change, func(t *testing.T) {
			backend, fake := newTestBackend(t)
			req := fixedRequest(t)
			lease, err := backend.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "missing key":
				core.RemoveStoredTestboxKey(req.RequestedLeaseID)
			case "project":
				backend.newClient = func(core.Config, core.Runtime) (Client, error) {
					return fixedScopeClient{Client: fake, project: "other-project"}, nil
				}
			case "provider":
				claim, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
				_, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(req.RequestedLeaseID, claim, func() core.LeaseClaim {
					changed := core.CloneLeaseClaim(claim)
					changed.Provider = "another-provider"
					return changed
				}())
				if err != nil {
					t.Fatal(err)
				}
			case "changed server nonce":
				labels := labelsFromTags(fake.server.Tags)
				labels["fixed_attempt"] = "other-attempt"
				fake.server.Tags = tagsFromLabels(labels)
				if _, err := backend.Touch(t.Context(), core.TouchRequest{Lease: lease}); err == nil {
					t.Fatal("heartbeat accepted changed ownership")
				}
			case "missing intent":
				core.RemoveLeaseClaim(req.RequestedLeaseID)
			}
			if _, err := backend.Acquire(t.Context(), req); err == nil {
				t.Fatal("changed key/ownership accepted")
			}
			if fake.createCalls != 1 || fake.createVolumeCalls != 1 || len(fake.keys) != 1 {
				t.Fatal("changed key/ownership allocated a replacement")
			}
		})
	}
}
