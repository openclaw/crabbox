package scaleway

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (api *fakeInstanceAPI) GetImage(req *instance.GetImageRequest, _ ...scw.RequestOption) (*instance.GetImageResponse, error) {
	if api.f.getImageErr != nil {
		return nil, api.f.getImageErr
	}
	return &instance.GetImageResponse{Image: &instance.Image{ID: req.ImageID, RootVolume: &instance.VolumeSummary{ID: "root-snapshot", Size: 10_000_000_000, VolumeType: instance.VolumeVolumeTypeLSSD}}}, nil
}

// Keep a sentinel for the regression against the snapshot-based implementation.
func (api *fakeInstanceAPI) CreateVolume(req *instance.CreateVolumeRequest, _ ...scw.RequestOption) (*instance.CreateVolumeResponse, error) {
	api.f.createVolumeCalls++
	return nil, api.f.createVolumeReplyErr
}

func (api *fakeInstanceAPI) UpdateVolume(req *instance.UpdateVolumeRequest, _ ...scw.RequestOption) (*instance.UpdateVolumeResponse, error) {
	if api.f.updateVolumeErr != nil {
		return nil, api.f.updateVolumeErr
	}
	volume := api.f.volumes[req.VolumeID]
	if req.Tags != nil {
		volume.Tags = *req.Tags
	}
	return &instance.UpdateVolumeResponse{Volume: volume}, nil
}

func fixedRequest(t *testing.T) core.AcquireRequest {
	t.Helper()
	return core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123456", RequestedSlug: "fixed", Repo: core.Repo{Root: t.TempDir()}, Keep: true}
}

func TestFixedScalewayPublicImage(t *testing.T) {
	backend, fake := newTestBackend(t)
	req := fixedRequest(t)
	backend.cfg.Scaleway.Image = "ubuntu_noble"
	fake.createVolumeReplyErr = errors.New("HTTP 403: read compute_snapshots denied")
	fake.afterCreate = func() { fake.server.Volumes["0"].Boot = false }
	lease, err := backend.Acquire(t.Context(), req)
	if err != nil {
		t.Fatalf("public image must not require root snapshot access: %v", err)
	}
	if fake.createVolumeCalls != 0 || len(fake.lastCreate.Volumes) != 0 {
		t.Fatal("fixed acquisition must let the image create its root volume")
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.Labels[rootVolumeLabel] != fake.server.Volumes["0"].ID || claim.FixedCreateIntent.Attempt["root_created_at"] == "" {
		t.Fatalf("root volume was not journaled: %v", err)
	}
	if err := backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(fake.volumes) != 0 || len(fake.keys) != 0 || !fake.deletedServer {
		t.Fatal("stop left allocation resources behind")
	}
}

func TestFixedScalewayAcquireReplay(t *testing.T) {
	for _, interruption := range []string{"none", "key response lost", "root tag publication", "volume tag publication", "cloud-init"} {
		t.Run(interruption, func(t *testing.T) {
			backend, fake := newTestBackend(t)
			req := fixedRequest(t)
			switch interruption {
			case "key response lost":
				fake.createKeyErr = errors.New("response lost")
			case "root tag publication":
				fake.updateErr = errors.New("response lost")
			case "volume tag publication":
				fake.updateVolumeErr = errors.New("response lost")
			case "cloud-init":
				fake.userDataErr = errors.New("request interrupted")
			}
			first, err := backend.Acquire(t.Context(), req)
			if interruption == "none" && err != nil || interruption != "none" && err == nil {
				t.Fatalf("initial acquire: %v", err)
			}
			fake.createKeyErr, fake.updateErr = nil, nil
			fake.updateVolumeErr, fake.userDataErr = nil, nil
			replay, err := backend.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if fake.createCalls != 1 || fake.createVolumeCalls != 0 || len(fake.keys) != 1 || replay.LeaseID != req.RequestedLeaseID ||
				first.LeaseID != "" && first.Server.CloudID != replay.Server.CloudID {
				t.Fatalf("duplicate allocation: servers=%d keys=%d", fake.createCalls, len(fake.keys))
			}
		})
	}
}

func TestFixedScalewayRootIsBoundBeforePublication(t *testing.T) {
	backend, fake := newTestBackend(t)
	req := fixedRequest(t)
	fake.updateVolumeErr = errors.New("publication interrupted")
	if _, err := backend.Acquire(t.Context(), req); err == nil {
		t.Fatal("expected interruption")
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.CloudID != fake.server.ID || claim.Labels[rootVolumeLabel] != fake.server.Volumes["0"].ID || claim.Labels[volumePendingLabel] != "true" {
		t.Fatalf("missing durable root binding: %+v %v", claim, err)
	}
	fake.updateVolumeErr = nil
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
		claim.Labels[rootVolumeLabel] != "" || claim.Labels["scaleway_ssh_key_id"] == "" || fake.createCalls != 0 || fake.server != nil ||
		fake.createVolumeCalls != 0 || fake.createKeyCalls != 1 {
		t.Fatalf("interrupted journal: %+v err=%v", claim, err)
	}
	return claim
}

func TestFixedScalewayAdmittedWithoutServerRetainsCustody(t *testing.T) {
	backend, fake := newTestBackend(t)
	req := fixedRequest(t)
	before := interruptFixedBeforeServer(t, backend, fake, req)
	if _, err := backend.Acquire(t.Context(), req); err == nil {
		t.Fatal("submitted attempt was retried")
	}
	if err := backend.releaseFixed(t.Context(), fake, before); err == nil {
		t.Fatal("unresolved attempt was released")
	}
	if fake.createCalls != 0 || fake.deletedKey || fake.deletedServer {
		t.Fatal("uncertain custody changed resources")
	}
}

func TestFixedScalewayAdmittedRecoveryRefusesUncertainOwnership(t *testing.T) {
	for _, change := range []string{"inventory error", "empty inventory reply", "server attempt", "server tags", "duplicate server"} {
		t.Run(change, func(t *testing.T) {
			backend, fake := newTestBackend(t)
			req := fixedRequest(t)
			fake.createResponseWithoutServer = true
			if _, err := backend.Acquire(t.Context(), req); err == nil {
				t.Fatal("expected lost response")
			}
			before, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
			inventoryErr := errors.New("inventory unavailable")
			switch change {
			case "inventory error":
				fake.listErr = inventoryErr
			case "empty inventory reply":
				fake.listEmptyReply = true
			case "server attempt":
				labels := labelsFromTags(fake.server.Tags)
				labels["fixed_attempt"] = "other-attempt"
				fake.server.Tags = tagsFromLabels(labels)
			case "server tags":
				fake.server.Tags = nil
			case "duplicate server":
				other := *fake.server
				other.ID = "another-server"
				fake.servers = []*instance.Server{fake.server, &other}
			}
			_, acquireErr := backend.Acquire(t.Context(), req)
			releaseErr := backend.releaseFixed(t.Context(), fake, before)
			if acquireErr == nil || releaseErr == nil {
				t.Fatalf("unproven recovery: acquire=%v release=%v", acquireErr, releaseErr)
			}
			if change == "inventory error" && (!errors.Is(acquireErr, inventoryErr) || !errors.Is(releaseErr, inventoryErr)) {
				t.Fatal("lost inventory error")
			}
			if fake.createCalls != 1 || fake.createVolumeCalls != 0 || fake.deletedServer || fake.deletedKey || len(fake.volumes) != 1 {
				t.Fatal("refusal changed resources")
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

func TestFixedScalewayLostResponseRefusesMismatchedIdentity(t *testing.T) {
	for _, field := range []string{"fixed_attempt", "scaleway_ssh_key_id"} {
		t.Run(field, func(t *testing.T) {
			backend, fake := newTestBackend(t)
			req := fixedRequest(t)
			fake.createResponseWithoutServer = true
			if _, err := backend.Acquire(t.Context(), req); err == nil {
				t.Fatal("expected lost create response")
			}
			before, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || before.CloudID != "" || before.Labels[rootVolumeLabel] != "" {
				t.Fatalf("expected unbound attempt: %v", err)
			}

			// Replace the vanished server with a candidate copying its name and tags,
			// but with one mismatched identity field and its own co-created root.
			candidate := *fake.server
			candidate.ID = "replacement-server"
			createdAt := candidate.CreationDate.Add(time.Minute)
			candidate.CreationDate = &createdAt
			root := *fake.volumes[candidate.Volumes["0"].ID]
			root.ID, root.CreationDate = "55555555-5555-4555-8555-555555555555", &createdAt
			root.Server = &instance.ServerSummary{ID: candidate.ID}
			candidate.Volumes = map[string]*instance.VolumeServer{"0": {ID: root.ID}}
			labels := labelsFromTags(candidate.Tags)
			labels[field] = "another-attempt"
			candidate.Tags = tagsFromLabels(labels)
			fake.server, fake.volumes = &candidate, map[string]*instance.Volume{root.ID: &root}

			if _, err := backend.Acquire(t.Context(), req); core.ExitCodeForError(err, 1) != 4 {
				t.Fatalf("replay accepted mismatched %s: %v", field, err)
			}
			if err := backend.releaseFixed(t.Context(), fake, before); core.ExitCodeForError(err, 1) != 4 {
				t.Fatalf("stop accepted mismatched %s: %v", field, err)
			}
			after, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || !reflect.DeepEqual(before, after) || fake.createCalls != 1 || fake.updateCalls != 0 || fake.deletedServer || fake.deletedKey || len(fake.volumes) != 1 || len(root.Tags) != 0 {
				t.Fatalf("refusal changed the claim or resources: %v", err)
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
	for _, interruption := range []string{"none", "key reply", "volume publication", "server reply", "SSH key cleanup"} {
		t.Run(interruption, func(t *testing.T) {
			backend, fake := newTestBackend(t)
			req := fixedRequest(t)
			if !backend.SupportsRequestedLeaseID() {
				t.Fatal("fixed lease capability missing")
			}
			switch interruption {
			case "key reply":
				fake.createKeyErr = errors.New("reply lost")
			case "volume publication":
				fake.updateVolumeErr = errors.New("reply lost")
			case "server reply":
				fake.createResponseWithoutServer = true
			}
			_, err := backend.Acquire(t.Context(), req)
			if (interruption == "none" || interruption == "SSH key cleanup") && err != nil {
				t.Fatal(err)
			}
			fake.updateVolumeErr = nil
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
	for _, phase := range []string{"unbound", "bound", "server absent"} {
		for _, change := range []string{"creation time", "tags", "nonce", "project", "zone", "attached"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				backend, fake := newTestBackend(t)
				req := fixedRequest(t)
				fake.createResponseWithoutServer = phase == "unbound"
				_, err := backend.Acquire(t.Context(), req)
				if (err != nil) != (phase == "unbound") {
					t.Fatalf("acquire: %v", err)
				}
				volume := fake.volumes["44444444-4444-4444-4444-444444444444"]
				if phase == "server absent" {
					fake.server = nil
					fake.getErr = &scw.ResourceNotFoundError{}
					volume.Server = nil
				}
				switch change {
				case "creation time":
					volume.CreationDate = nil
				case "tags":
					volume.Tags = []string{"crabbox"}
				case "nonce":
					labels := labelsFromTags(volume.Tags)
					if labels == nil {
						labels = map[string]string{}
					}
					labels["fixed_attempt"] = "other-attempt"
					volume.Tags = tagsFromLabels(labels)
				case "project":
					volume.Project = "other-project"
				case "zone":
					volume.Zone = "fr-par-2"
				case "attached":
					volume.Server = &instance.ServerSummary{ID: "another-server"}
				}
				before, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
				if _, err := backend.Acquire(t.Context(), req); err == nil {
					t.Fatal("acquired foreign root")
				}
				if err := backend.releaseFixed(t.Context(), fake, before); err == nil {
					t.Fatal("deleted foreign root")
				}
				if fake.createCalls != 1 || fake.deletedServer || fake.deletedKey || len(fake.volumes) != 1 {
					t.Fatal("refusal changed resources")
				}
			})
		}
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
			if fake.createCalls != 1 || fake.createVolumeCalls != 0 || len(fake.keys) != 1 {
				t.Fatal("changed key/ownership allocated a replacement")
			}
		})
	}
}

func TestFixedScalewayRefusesSwappedUntaggedRootBeforeBinding(t *testing.T) {
	backend, fake := newTestBackend(t)
	req := fixedRequest(t)
	fake.createResponseWithoutServer = true
	if _, err := backend.Acquire(t.Context(), req); err == nil {
		t.Fatal("expected lost response")
	}
	original := fake.volumes[fake.server.Volumes["0"].ID]
	original.Server = nil
	older := original.CreationDate.Add(-time.Second)
	replacement := &instance.Volume{ID: "55555555-5555-4555-8555-555555555555", Name: "unrelated", CreationDate: &older, Project: fake.ProjectID(), Zone: scw.Zone(fake.Zone()), Server: &instance.ServerSummary{ID: fake.server.ID}}
	fake.volumes[replacement.ID] = replacement
	fake.server.Volumes["0"].ID = replacement.ID
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.Labels[rootVolumeLabel] != "" {
		t.Fatalf("unexpected claim: %v", err)
	}
	if _, err := backend.Acquire(t.Context(), req); err == nil {
		t.Fatal("adopted an untagged replacement")
	}
	if err := backend.releaseFixed(t.Context(), fake, claim); err == nil {
		t.Fatal("released an untagged replacement")
	}
	if fake.deletedServer || fake.deletedKey || len(fake.volumes) != 2 || len(replacement.Tags) != 0 {
		t.Fatal("refusal changed ownership or resources")
	}
}

func TestFixedScalewayPendingRootCleanupAfterServerDisappears(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "foreign"}[foreign], func(t *testing.T) {
			backend, fake := newTestBackend(t)
			req := fixedRequest(t)
			fake.updateVolumeErr = errors.New("publication interrupted")
			if _, err := backend.Acquire(t.Context(), req); err == nil {
				t.Fatal("expected interruption")
			}
			claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || claim.Labels[volumePendingLabel] != "true" {
				t.Fatalf("missing pending journal: %v", err)
			}
			volume := fake.volumes[claim.Labels[rootVolumeLabel]]
			volume.Server = nil
			if foreign {
				volume.CreationDate = nil
			}
			fake.server = nil
			fake.getErr = &scw.ResourceNotFoundError{}
			err = backend.releaseFixed(t.Context(), fake, claim)
			if foreign {
				if err == nil || fake.deletedKey || len(fake.volumes) != 1 {
					t.Fatal("foreign pending root was released")
				}
			} else if err != nil || !fake.deletedKey || len(fake.volumes) != 0 {
				t.Fatalf("pending cleanup: %v", err)
			}
		})
	}
}
