package ovh

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeAPI struct {
	authCalls         int
	regionCalls       int
	flavorCalls       int
	imageCalls        int
	instanceCalls     int
	mutatingCalls     int
	deletedInstances  []string
	deletedKeys       []string
	createKeys        []SSHKey
	createInstances   []InstanceCreateRequest
	createInstanceErr error
	getInstanceErr    error
	deleteInstanceErr error
	deleteKeyErr      error
	regions           []Region
	flavors           []Flavor
	images            []Image
	instances         []Instance
	sshKeys           []SSHKey
}

func (f *fakeAPI) AuthTime(context.Context) (int64, error) {
	f.authCalls++
	return 1234567890, nil
}

func (f *fakeAPI) ListProjects(context.Context) ([]Project, error) {
	return []Project{{ID: "project-test"}}, nil
}

func (f *fakeAPI) ListRegions(context.Context, string) ([]Region, error) {
	f.regionCalls++
	return f.regions, nil
}

func (f *fakeAPI) ListFlavors(context.Context, string, string) ([]Flavor, error) {
	f.flavorCalls++
	return f.flavors, nil
}

func (f *fakeAPI) GetFlavor(context.Context, string, string) (Flavor, error) {
	f.mutatingCalls++
	return Flavor{}, nil
}

func (f *fakeAPI) ListImages(context.Context, string, string) ([]Image, error) {
	f.imageCalls++
	return f.images, nil
}

func (f *fakeAPI) GetImage(context.Context, string, string) (Image, error) {
	f.mutatingCalls++
	return Image{}, nil
}

func (f *fakeAPI) ListSSHKeys(context.Context, string) ([]SSHKey, error) {
	f.mutatingCalls++
	return nil, nil
}

func (f *fakeAPI) GetSSHKey(context.Context, string, string) (SSHKey, error) {
	f.mutatingCalls++
	return SSHKey{}, nil
}

func (f *fakeAPI) CreateSSHKey(_ context.Context, _ string, name, publicKey string) (SSHKey, error) {
	f.mutatingCalls++
	key := SSHKey{ID: "key-" + name, Name: name, PublicKey: publicKey}
	f.createKeys = append(f.createKeys, key)
	f.sshKeys = append(f.sshKeys, key)
	return key, nil
}

func (f *fakeAPI) DeleteSSHKey(_ context.Context, _, keyID string) error {
	f.mutatingCalls++
	f.deletedKeys = append(f.deletedKeys, keyID)
	return f.deleteKeyErr
}

func (f *fakeAPI) ListInstances(context.Context, string) ([]Instance, error) {
	f.instanceCalls++
	return f.instances, nil
}

func (f *fakeAPI) GetInstance(_ context.Context, _, instanceID string) (Instance, error) {
	f.mutatingCalls++
	if f.getInstanceErr != nil {
		return Instance{}, f.getInstanceErr
	}
	for _, instance := range f.instances {
		if instance.ID == instanceID {
			return instance, nil
		}
	}
	return Instance{}, &APIError{Status: 404}
}

func (f *fakeAPI) CreateInstance(_ context.Context, _ string, req InstanceCreateRequest) (Instance, error) {
	f.mutatingCalls++
	f.createInstances = append(f.createInstances, req)
	if f.createInstanceErr != nil {
		return Instance{}, f.createInstanceErr
	}
	instance := Instance{
		ID:       "inst-1",
		Name:     req.Name,
		Status:   "ACTIVE",
		Region:   req.Region,
		SSHKeyID: req.SSHKeyID,
		Labels:   req.Labels,
		Flavor:   Flavor{ID: req.FlavorID, Name: req.FlavorID},
		Image:    Image{ID: req.ImageID, Name: req.ImageID},
		IPAddresses: []IPAddress{{
			IP:      "203.0.113.10",
			Version: 4,
			Type:    "public",
		}},
	}
	f.instances = append(f.instances, instance)
	return instance, nil
}

func (f *fakeAPI) DeleteInstance(_ context.Context, _, instanceID string) error {
	f.mutatingCalls++
	f.deletedInstances = append(f.deletedInstances, instanceID)
	return f.deleteInstanceErr
}

func TestDoctorUsesReadOnlyDiscovery(t *testing.T) {
	fake := &fakeAPI{
		regions:   []Region{{Name: "GRA11"}},
		flavors:   []Flavor{{ID: "flavor-id", Name: "b3-8"}},
		images:    []Image{{ID: "image-id", Name: "Ubuntu 24.04"}},
		instances: []Instance{{ID: "one", Name: "crabbox-ready"}, {ID: "two", Name: "unrelated"}},
	}
	backend := NewBackend(Provider{}.Spec(), core.Config{OVH: core.OVHConfig{
		Endpoint:  "https://user:pass@api.us.ovhcloud.com/1.0",
		ProjectID: "project-test",
		Region:    "GRA11",
		Image:     "Ubuntu 24.04",
		Flavor:    "b3-8",
	}}, core.Runtime{})
	backend.clientFactory = func(core.Config, core.Runtime) (API, error) {
		return fake, nil
	}

	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != providerName || !strings.Contains(result.Message, "inventory=ready api=list mutation=false leases=1") {
		t.Fatalf("result=%#v", result)
	}
	if strings.Contains(result.Message, "user:pass") {
		t.Fatalf("doctor leaked endpoint userinfo: %s", result.Message)
	}
	if fake.authCalls != 1 || fake.regionCalls != 1 || fake.flavorCalls != 1 || fake.imageCalls != 1 || fake.instanceCalls != 1 {
		t.Fatalf("unexpected read call counts: %#v", fake)
	}
	if fake.mutatingCalls != 0 {
		t.Fatalf("doctor used non-discovery calls: %#v", fake)
	}
}

func TestDoctorReportsMissingProjectWithoutClient(t *testing.T) {
	backend := NewBackend(Provider{}.Spec(), core.Config{}, core.Runtime{})
	backend.clientFactory = func(core.Config, core.Runtime) (API, error) {
		t.Fatal("client should not be created when project ID is missing")
		return nil, nil
	}

	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || !strings.Contains(result.Message, "mutation=false") || len(result.Checks) != 1 || result.Checks[0].Check != "configuration" {
		t.Fatalf("result=%#v", result)
	}
}

func TestDoctorReportsUnavailableFlavor(t *testing.T) {
	fake := &fakeAPI{
		regions: []Region{{Name: "GRA11"}},
		flavors: []Flavor{{ID: "other", Name: "b3-16"}},
	}
	backend := NewBackend(Provider{}.Spec(), core.Config{OVH: core.OVHConfig{
		ProjectID: "project-test",
		Region:    "GRA11",
		Flavor:    "b3-8",
	}}, core.Runtime{})
	backend.clientFactory = func(core.Config, core.Runtime) (API, error) {
		return fake, nil
	}

	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || len(result.Checks) != 1 || result.Checks[0].Check != "flavor" || !strings.Contains(result.Checks[0].Message, "b3-8") {
		t.Fatalf("result=%#v", result)
	}
	if fake.imageCalls != 0 || fake.instanceCalls != 0 || fake.mutatingCalls != 0 {
		t.Fatalf("doctor continued after failed flavor check: %#v", fake)
	}
}

func TestBackendImplementsLeaseInterfacesWithNonMutatingStubs(t *testing.T) {
	var backend any = NewBackend(Provider{}.Spec(), core.Config{}, core.Runtime{})
	if _, ok := backend.(core.SSHLeaseBackend); !ok {
		t.Fatal("ovh backend should satisfy SSHLeaseBackend with explicit lifecycle stubs")
	}
	if _, ok := backend.(core.CleanupBackend); !ok {
		t.Fatal("ovh backend should satisfy CleanupBackend with explicit lifecycle stub")
	}
}

func TestAcquireCreatesInstanceSSHKeyTargetAndClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fake := &fakeAPI{
		flavors: []Flavor{{ID: "flavor-id", Name: "b3-8"}},
		images:  []Image{{ID: "image-id", Name: "Ubuntu 24.04"}},
	}
	backend := testBackend(fake)

	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{
		Repo:          core.Repo{Root: filepath.Join(t.TempDir(), "repo")},
		RequestedSlug: "blue-lobster",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID == "" || lease.Server.CloudID != "inst-1" || lease.SSH.Host != "203.0.113.10" || lease.SSH.Key == "" {
		t.Fatalf("lease=%#v", lease)
	}
	if len(fake.createKeys) != 1 || len(fake.createInstances) != 1 {
		t.Fatalf("create keys=%d instances=%d", len(fake.createKeys), len(fake.createInstances))
	}
	req := fake.createInstances[0]
	if req.FlavorID != "flavor-id" || req.ImageID != "image-id" || req.SSHKeyID == "" || !strings.Contains(req.UserData, "ssh-ed25519") {
		t.Fatalf("create request=%#v", req)
	}
	if req.Labels["crabbox"] != "true" || req.Labels["provider"] != providerName || req.Labels["lease"] != lease.LeaseID || req.Labels[ovhProjectLabel] != "project-test" {
		t.Fatalf("labels=%#v", req.Labels)
	}
	claim, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.CloudID != "inst-1" || claim.Provider != providerName || claim.SSHHost != "203.0.113.10" {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestAcquireRejectsUnsupportedExplicitOSWithoutOVHImageOverride(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fake := &fakeAPI{
		flavors: []Flavor{{ID: "flavor-id", Name: "b3-8"}},
		images:  []Image{{ID: "image-id", Name: "Ubuntu 24.04"}},
	}
	backend := testBackend(fake)
	backend.Cfg.OSImage = "ubuntu:26.04"
	core.SetOSImageExplicit(&backend.Cfg)

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "does not support --os ubuntu:26.04") {
		t.Fatalf("err=%v", err)
	}
	if len(fake.createKeys) != 0 || len(fake.createInstances) != 0 {
		t.Fatalf("unsupported OS mutated keys=%d instances=%d", len(fake.createKeys), len(fake.createInstances))
	}
}

func TestAcquireAllowsUnsupportedExplicitOSWithOVHImageOverride(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fake := &fakeAPI{
		flavors: []Flavor{{ID: "flavor-id", Name: "b3-8"}},
		images:  []Image{{ID: "custom-image-id", Name: "Custom Debian"}},
	}
	backend := testBackend(fake)
	backend.Cfg.OSImage = "ubuntu:26.04"
	backend.Cfg.OVH.Image = "Custom Debian"
	core.SetOSImageExplicit(&backend.Cfg)
	core.SetOVHImageExplicit(&backend.Cfg)

	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID == "" || len(fake.createInstances) != 1 || fake.createInstances[0].ImageID != "custom-image-id" {
		t.Fatalf("lease=%#v create=%#v", lease, fake.createInstances)
	}
}

func TestResolveBySlugUsesClaimedInstance(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fake := &fakeAPI{flavors: []Flavor{{ID: "flavor-id", Name: "b3-8"}}, images: []Image{{ID: "image-id", Name: "Ubuntu 24.04"}}}
	backend := testBackend(fake)
	repoRoot := t.TempDir()
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: repoRoot}, RequestedSlug: "blue-lobster"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := backend.Resolve(context.Background(), core.ResolveRequest{Repo: core.Repo{Root: repoRoot}, ID: "blue-lobster"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.LeaseID != lease.LeaseID || resolved.Server.CloudID != lease.Server.CloudID || resolved.SSH.Host != lease.SSH.Host {
		t.Fatalf("resolved=%#v lease=%#v", resolved, lease)
	}
}

func TestResolveDirectInstanceIDPropagatesProviderError(t *testing.T) {
	fake := &fakeAPI{getInstanceErr: errors.New("ovh api unavailable")}
	backend := testBackend(fake)
	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "missing-from-list"})
	if err == nil || !strings.Contains(err.Error(), "ovh api unavailable") {
		t.Fatalf("err=%v", err)
	}
}

func TestListAndStatusOverlayReadyClaimLabels(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fake := &fakeAPI{flavors: []Flavor{{ID: "flavor-id", Name: "b3-8"}}, images: []Image{{ID: "image-id", Name: "Ubuntu 24.04"}}}
	backend := testBackend(fake)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "blue-lobster"})
	if err != nil {
		t.Fatal(err)
	}
	if fake.instances[0].Labels["state"] != "provisioning" {
		t.Fatalf("fake live labels unexpectedly ready: %#v", fake.instances[0].Labels)
	}
	views, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Labels["state"] != "ready" {
		t.Fatalf("views=%#v", views)
	}
	status, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID, StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if status.Server.Labels["state"] != "ready" {
		t.Fatalf("status=%#v", status.Server.Labels)
	}
}

func TestReleaseDeletesOnlyOwnedClaimedInstanceAndKey(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fake := &fakeAPI{flavors: []Flavor{{ID: "flavor-id", Name: "b3-8"}}, images: []Image{{ID: "image-id", Name: "Ubuntu 24.04"}}}
	backend := testBackend(fake)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "blue-lobster"})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedInstances) != 1 || fake.deletedInstances[0] != "inst-1" {
		t.Fatalf("deleted instances=%v", fake.deletedInstances)
	}
	if len(fake.deletedKeys) != 1 || fake.deletedKeys[0] == "" {
		t.Fatalf("deleted keys=%v", fake.deletedKeys)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
}

func TestReleaseRefusesForeignOrPartialOwnership(t *testing.T) {
	fake := &fakeAPI{}
	backend := testBackend(fake)
	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		Server: core.Server{
			Provider: providerName,
			CloudID:  "foreign",
			Name:     "crabbox-blue",
			Labels:   map[string]string{"crabbox": "true", "lease": "cbx_abc", "slug": "blue"},
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "refusing to operate") {
		t.Fatalf("err=%v", err)
	}
	if len(fake.deletedInstances) != 0 || len(fake.deletedKeys) != 0 {
		t.Fatalf("unexpected deletes instances=%v keys=%v", fake.deletedInstances, fake.deletedKeys)
	}
}

func TestCleanupDryRunAndExpiredOwnedOnly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fake := &fakeAPI{flavors: []Flavor{{ID: "flavor-id", Name: "b3-8"}}, images: []Image{{ID: "image-id", Name: "Ubuntu 24.04"}}}
	backend := testBackend(fake)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "old-lease"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.instances) != 1 {
		t.Fatalf("instances=%#v", fake.instances)
	}
	labels := fake.instances[0].Labels
	labels["state"] = "released"
	fake.instances = append(fake.instances, Instance{ID: "foreign", Name: "crabbox-foreign", Labels: map[string]string{"crabbox": "true"}})
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedInstances) != 0 {
		t.Fatalf("dry run deleted: %v", fake.deletedInstances)
	}
	if err := markClaimReleased(lease.LeaseID); err != nil {
		t.Fatal(err)
	}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedInstances) != 1 || fake.deletedInstances[0] != lease.Server.CloudID {
		t.Fatalf("deleted instances=%v", fake.deletedInstances)
	}
}

func TestCleanupUsesTouchedLocalClaimBeforeLiveExpiryLabels(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fake := &fakeAPI{flavors: []Flavor{{ID: "flavor-id", Name: "b3-8"}}, images: []Image{{ID: "image-id", Name: "Ubuntu 24.04"}}}
	backend := testBackend(fake)
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "active-lease"})
	if err != nil {
		t.Fatal(err)
	}
	fake.instances[0].Labels["state"] = "released"
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedInstances) != 0 {
		t.Fatalf("cleanup ignored active local claim and deleted %v", fake.deletedInstances)
	}
}

func TestTouchAppliesIdleTimeoutOverride(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fake := &fakeAPI{flavors: []Flavor{{ID: "flavor-id", Name: "b3-8"}}, images: []Image{{ID: "image-id", Name: "Ubuntu 24.04"}}}
	backend := testBackend(fake)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "touch-me"})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := backend.Touch(context.Background(), core.TouchRequest{
		Lease:       lease,
		State:       "running",
		IdleTimeout: 2 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Labels["idle_timeout_secs"] != "7200" {
		t.Fatalf("updated labels=%#v", updated.Labels)
	}
	claim, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Labels["idle_timeout_secs"] != "7200" || claim.Labels["state"] != "running" {
		t.Fatalf("claim labels=%#v", claim.Labels)
	}
}

func TestPublicIPv4PrefersPublicNonPrivateAddress(t *testing.T) {
	instance := Instance{IPAddresses: []IPAddress{
		{IP: "10.0.0.5", Version: 4, Type: "private"},
		{IP: "192.168.1.7", Version: 4, Type: "public"},
		{IP: "2001:db8::1", Version: 6, Type: "public"},
		{IP: "203.0.113.42", Version: 4, Type: ""},
		{IP: "198.51.100.42", Version: 4, Type: "public"},
	}}
	if got := publicIPv4(instance); got != "198.51.100.42" {
		t.Fatalf("publicIPv4=%q", got)
	}
}

func TestAcquirePreservesRecoveryClaimOnCreateError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fake := &fakeAPI{
		flavors:           []Flavor{{ID: "flavor-id", Name: "b3-8"}},
		images:            []Image{{ID: "image-id", Name: "Ubuntu 24.04"}},
		createInstanceErr: errors.New("indeterminate create"),
	}
	backend := testBackend(fake)
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "recover-me"})
	if err == nil || !strings.Contains(err.Error(), "indeterminate create") {
		t.Fatalf("err=%v", err)
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Labels["recovery"] != "ambiguous-create" || claims[0].Labels[ovhSSHKeyIDLabel] == "" {
		t.Fatalf("claims=%#v", claims)
	}
	if len(fake.deletedKeys) != 1 {
		t.Fatalf("expected rollback key delete, got %v", fake.deletedKeys)
	}
}

func TestAcquireRollbackRemovesClaimAfterConcreteCreateCleanup(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fake := &fakeAPI{
		flavors: []Flavor{{ID: "flavor-id", Name: "b3-8"}},
		images:  []Image{{ID: "image-id", Name: "Ubuntu 24.04"}},
	}
	backend := testBackend(fake)
	backend.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
		return errors.New("ssh never became ready")
	}
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "rollback-me"})
	if err == nil || !strings.Contains(err.Error(), "ssh never became ready") {
		t.Fatalf("err=%v", err)
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("stale recovery claims=%#v", claims)
	}
	if len(fake.deletedInstances) != 1 || len(fake.deletedKeys) != 1 {
		t.Fatalf("rollback deleted instances=%v keys=%v", fake.deletedInstances, fake.deletedKeys)
	}
}

func markClaimReleased(leaseID string) error {
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		return err
	}
	labels := make(map[string]string, len(claim.Labels))
	for key, value := range claim.Labels {
		labels[key] = value
	}
	labels["state"] = "released"
	_, err = core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, claim, labels)
	return err
}

func testBackend(fake *fakeAPI) *Backend {
	cfg := core.Config{
		Provider:    providerName,
		TargetOS:    core.TargetLinux,
		SSHUser:     "ubuntu",
		SSHPort:     "22",
		WorkRoot:    "/work/crabbox",
		Class:       "standard",
		TTL:         time.Hour,
		IdleTimeout: time.Hour,
		OVH: core.OVHConfig{
			ProjectID: "project-test",
			Region:    "GRA11",
			Image:     "Ubuntu 24.04",
			Flavor:    "b3-8",
		},
	}
	backend := NewBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard})
	backend.clientFactory = func(core.Config, core.Runtime) (API, error) {
		return fake, nil
	}
	backend.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
		return nil
	}
	backend.ipWaitInterval = time.Millisecond
	return backend
}
