package nebius

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeNebiusAPI struct {
	items           []nebiusInstance
	createReq       nebiusCreateRequest
	created         nebiusInstance
	waited          nebiusInstance
	updatedLabels   map[string]string
	deletedIDs      []string
	getErr          error
	deleteErr       error
	createErr       error
	listErr         error
	waitErr         error
	updateLabelsErr error
}

func (f *fakeNebiusAPI) ListInstances(context.Context) ([]nebiusInstance, error) {
	return append([]nebiusInstance(nil), f.items...), f.listErr
}

func (f *fakeNebiusAPI) GetInstance(_ context.Context, id string) (nebiusInstance, error) {
	if f.getErr != nil {
		return nebiusInstance{}, f.getErr
	}
	for _, item := range f.items {
		if item.ID == id {
			return item, nil
		}
	}
	if f.waited.ID == id {
		return f.waited, nil
	}
	return nebiusInstance{}, errors.New("not found")
}

func (f *fakeNebiusAPI) CreateInstance(_ context.Context, req nebiusCreateRequest) (nebiusInstance, error) {
	f.createReq = req
	return f.created, f.createErr
}

func (f *fakeNebiusAPI) WaitInstance(context.Context, string) (nebiusInstance, error) {
	return f.waited, f.waitErr
}

func (f *fakeNebiusAPI) UpdateLabels(_ context.Context, _ string, labels map[string]string) error {
	f.updatedLabels = labels
	return f.updateLabelsErr
}

func (f *fakeNebiusAPI) DeleteInstance(_ context.Context, id string) error {
	f.deletedIDs = append(f.deletedIDs, id)
	return f.deleteErr
}

func newTestBackend(t *testing.T, api *fakeNebiusAPI) *backend {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cfg := testConfig()
	cfg.IdleTimeout = time.Hour
	cfg.TTL = 2 * time.Hour
	cfg.Nebius.SecurityGroupIDs = []string{"sg-a", "sg-b"}
	cfg.Nebius.ServiceAccountID = "sa-a"
	b := NewBackend(Provider{}.Spec(), cfg, Runtime{Stderr: io.Discard}).(*backend)
	b.clientFactory = func(Runtime) nebiusAPI { return api }
	b.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error { return nil }
	b.now = func() time.Time { return time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC) }
	return b
}

func TestCloudInitRendersUserAndKey(t *testing.T) {
	got, err := renderNebiusCloudInit("crabbox", "ssh-ed25519 AAAA test")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#cloud-config", "name: crabbox", "NOPASSWD", "ssh-ed25519 AAAA test", "disable_root: true"} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloud-init missing %q:\n%s", want, got)
		}
	}
	if _, err := renderNebiusCloudInit("root", "ssh-ed25519 AAAA test"); err == nil {
		t.Fatal("reserved root user accepted")
	}
	for _, user := range []string{"alice\n  - name: root", "Alice", "bad user", "1alice", "alice:$evil"} {
		if _, err := renderNebiusCloudInit(user, "ssh-ed25519 AAAA test"); err == nil {
			t.Fatalf("unsafe user %q accepted", user)
		}
	}
}

func TestLabelOwnershipRequiresCompleteMatchingScope(t *testing.T) {
	cfg := testConfig()
	cfg.IdleTimeout = time.Hour
	labels := nebiusLeaseLabels(cfg, "cbx_123456789abc", "demo", "ready", false, time.Unix(1000, 0))
	if err := validateNebiusOwnership(labels, cfg); err != nil {
		t.Fatalf("complete labels rejected: %v", err)
	}
	partial := cloneLabels(labels)
	delete(partial, nebiusScopeLabel)
	if err := validateNebiusOwnership(partial, cfg); err == nil {
		t.Fatal("partial ownership accepted")
	}
	foreign := cloneLabels(labels)
	foreign[nebiusParentLabel] = "other-project"
	if err := validateNebiusOwnership(foreign, cfg); err == nil {
		t.Fatal("foreign parent ownership accepted")
	}
}

func TestCommandConstructionUsesManagedDiskPublicIPLabelsAndNoSecrets(t *testing.T) {
	runner := &recordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		joined := strings.Join(req.Args, " ")
		if !strings.Contains(joined, "compute instance create") {
			return LocalCommandResult{}, errors.New("unexpected command: " + joined)
		}
		if strings.Contains(joined, "osb_") || strings.Contains(joined, "private_key") {
			return LocalCommandResult{}, errors.New("secret-like value passed on argv")
		}
		if !strings.Contains(joined, "--network-interface subnet-id=subnet-123,public-ip-address={}") {
			return LocalCommandResult{}, errors.New("missing dynamic public IP network interface: " + joined)
		}
		if !strings.Contains(joined, "--boot-disk-image-family ubuntu24.04-driverless") || !strings.Contains(joined, "--boot-disk-type network_ssd") {
			return LocalCommandResult{}, errors.New("missing managed boot disk args: " + joined)
		}
		if !strings.Contains(joined, "--label crabbox_provider=nebius") || !strings.Contains(joined, "--label crabbox_scope=") {
			return LocalCommandResult{}, errors.New("missing ownership labels: " + joined)
		}
		return LocalCommandResult{Stdout: `{"metadata":{"id":"vm-1","name":"cbx-demo","labels":{"crabbox":"true"}},"status":{"state":"RUNNING","network_interfaces":[{"public_ip_address":{"address":"203.0.113.10/32"}}]}}`}, nil
	}}
	client := newNebiusClient(testConfig().Nebius, Runtime{Exec: runner})
	labels := nebiusLeaseLabels(testConfig(), "cbx_123456789abc", "demo", "ready", false, time.Unix(1000, 0))
	item, err := client.CreateInstance(context.Background(), nebiusCreateRequest{Name: "cbx-demo", Labels: labels, UserData: "#cloud-config\n"})
	if err != nil {
		t.Fatal(err)
	}
	if item.PublicIP != "203.0.113.10" {
		t.Fatalf("PublicIP=%q", item.PublicIP)
	}
}

func TestPublicIPParsesNestedCIDR(t *testing.T) {
	item, err := parseNebiusInstance(`{"metadata":{"id":"vm-1","name":"n"},"status":{"state":"RUNNING","network_interfaces":[{"public_ip_address":{"address":"198.51.100.7/32"}}]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if item.PublicIP != "198.51.100.7" {
		t.Fatalf("PublicIP=%q", item.PublicIP)
	}
}

func TestAcquirePersistsClaimAndSSHReadyTarget(t *testing.T) {
	cfg := testConfig()
	labels := nebiusLeaseLabels(cfg, "cbx_existing1", "old", "ready", false, time.Unix(1000, 0))
	api := &fakeNebiusAPI{
		items:   []nebiusInstance{{ID: "vm-old", Name: "old", Status: "RUNNING", Labels: labels, PublicIP: "203.0.113.9"}},
		created: nebiusInstance{ID: "vm-new", Name: "pending", Status: "CREATING", Labels: map[string]string{}},
		waited:  nebiusInstance{ID: "vm-new", Name: "ready", Status: "RUNNING", Labels: labels, PublicIP: "203.0.113.20"},
	}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Host != "203.0.113.20" || lease.SSH.User != "crabbox" || lease.SSH.Key == "" {
		t.Fatalf("ssh target=%#v", lease.SSH)
	}
	if api.createReq.Labels[nebiusLeaseLabel] == "" || api.updatedLabels["state"] != "ready" {
		t.Fatalf("labels not persisted/ready: create=%v update=%v", api.createReq.Labels, api.updatedLabels)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(lease.LeaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if claim.CloudID != "vm-new" || claim.SSHHost != "203.0.113.20" {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestResolveListStatusByAliases(t *testing.T) {
	cfg := testConfig()
	labels := nebiusLeaseLabels(cfg, "cbx_123456789abc", "demo", "ready", false, time.Unix(1000, 0))
	api := &fakeNebiusAPI{items: []nebiusInstance{
		{ID: "vm-1", Name: core.LeaseProviderName("cbx_123456789abc", "demo"), Status: "RUNNING", Labels: labels, PublicIP: "203.0.113.10"},
		{ID: "foreign", Name: "foreign", Status: "RUNNING", Labels: map[string]string{"crabbox": "true"}, PublicIP: "203.0.113.11"},
	}}
	b := newTestBackend(t, api)
	list, err := b.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].CloudID != "vm-1" || list[0].Status != "ready" {
		t.Fatalf("list=%#v", list)
	}
	for _, id := range []string{"cbx_123456789abc", "demo", "vm-1", core.LeaseProviderName("cbx_123456789abc", "demo")} {
		got, err := b.Resolve(context.Background(), ResolveRequest{ID: id})
		if err != nil {
			t.Fatalf("Resolve(%q): %v", id, err)
		}
		if got.LeaseID != "cbx_123456789abc" || got.Server.CloudID != "vm-1" {
			t.Fatalf("Resolve(%q)=%#v", id, got)
		}
	}
}

func TestReleaseAndCleanupRefuseForeignOrAmbiguousResources(t *testing.T) {
	cfg := testConfig()
	owned := nebiusLeaseLabels(cfg, "cbx_deadbeef1234", "old", "leased", false, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	foreign := cloneLabels(owned)
	foreign[nebiusScopeLabel] = "foreign"
	api := &fakeNebiusAPI{items: []nebiusInstance{
		{ID: "vm-owned", Name: "old", Status: "RUNNING", Labels: owned, PublicIP: "203.0.113.10"},
		{ID: "vm-foreign", Name: "foreign", Status: "RUNNING", Labels: foreign, PublicIP: "203.0.113.11"},
		{ID: "vm-partial", Name: "partial", Status: "RUNNING", Labels: map[string]string{"crabbox": "true", "provider": providerName}, PublicIP: "203.0.113.12"},
	}}
	b := newTestBackend(t, api)
	if err := b.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("dry-run deleted: %v", api.deletedIDs)
	}
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(api.deletedIDs, ",") != "vm-owned" {
		t.Fatalf("deletedIDs=%v", api.deletedIDs)
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: "cbx_deadbeef1234", Server: Server{CloudID: "vm-foreign", Labels: foreign}}}); err == nil {
		t.Fatal("ReleaseLease accepted foreign ownership")
	}
}

func TestTouchUpdatesLabelsWithoutLosingOwnership(t *testing.T) {
	cfg := testConfig()
	labels := nebiusLeaseLabels(cfg, "cbx_123456789abc", "demo", "leased", false, time.Unix(1000, 0))
	api := &fakeNebiusAPI{items: []nebiusInstance{{ID: "vm-1", Name: "demo", Status: "RUNNING", Labels: labels, PublicIP: "203.0.113.10"}}}
	b := newTestBackend(t, api)
	server, err := b.Touch(context.Background(), TouchRequest{Lease: LeaseTarget{LeaseID: "cbx_123456789abc", Server: Server{CloudID: "vm-1", Labels: labels}}, State: "ready", IdleTimeout: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if server.Labels["state"] != "ready" || api.updatedLabels[nebiusScopeLabel] == "" {
		t.Fatalf("touch labels=%v", server.Labels)
	}
	if api.updatedLabels["idle_timeout_secs"] != "1800" {
		t.Fatalf("idle timeout override ignored: labels=%v", api.updatedLabels)
	}
	if err := validateNebiusOwnership(server.Labels, b.Cfg); err != nil {
		t.Fatalf("touched labels no longer owned: %v", err)
	}
}

func TestTouchRefusesLiveOwnershipMismatch(t *testing.T) {
	cfg := testConfig()
	labels := nebiusLeaseLabels(cfg, "cbx_123456789abc", "demo", "leased", false, time.Unix(1000, 0))
	liveLabels := cloneLabels(labels)
	liveLabels["lease"] = "cbx_other123456"
	liveLabels[nebiusLeaseLabel] = "cbx_other123456"
	api := &fakeNebiusAPI{items: []nebiusInstance{{ID: "vm-1", Name: "demo", Status: "RUNNING", Labels: liveLabels, PublicIP: "203.0.113.10"}}}
	b := newTestBackend(t, api)
	_, err := b.Touch(context.Background(), TouchRequest{Lease: LeaseTarget{LeaseID: "cbx_123456789abc", Server: Server{CloudID: "vm-1", Labels: labels}}, State: "ready"})
	if err == nil {
		t.Fatal("Touch accepted changed live ownership")
	}
	if len(api.updatedLabels) != 0 {
		t.Fatalf("labels updated despite mismatch: %v", api.updatedLabels)
	}
}

func TestManagedDiskAcquireRejectsPublicIPNone(t *testing.T) {
	cfg := testConfig()
	cfg.Nebius.PublicIP = "none"
	if err := validateNebiusAcquireConfig(cfg); err == nil {
		t.Fatal("public_ip=none accepted for direct SSH acquire")
	}
}

func TestAcquireRecoveryRetainsClaimOnIndeterminateCreate(t *testing.T) {
	api := &fakeNebiusAPI{createErr: errors.New("connection reset after create")}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "recover"})
	if err == nil {
		t.Fatal("Acquire succeeded unexpectedly")
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("recover", providerName)
	if claimErr != nil || !ok {
		t.Fatalf("recovery claim ok=%v err=%v", ok, claimErr)
	}
	if claim.Slug != "recover" || claim.Labels["state"] != "provisioning" {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestAcquireRollsBackCreatedInstanceWhenOnAcquiredFails(t *testing.T) {
	api := &fakeNebiusAPI{
		created: nebiusInstance{ID: "vm-new", Name: "pending", Status: "CREATING", Labels: map[string]string{}},
		waited:  nebiusInstance{ID: "vm-new", Name: "ready", Status: "RUNNING", Labels: map[string]string{}, PublicIP: "203.0.113.20"},
	}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "rollback",
		OnAcquired: func(LeaseTarget) error {
			return errors.New("controller rejected identity")
		},
	})
	if err == nil {
		t.Fatal("Acquire succeeded unexpectedly")
	}
	if strings.Join(api.deletedIDs, ",") != "vm-new" {
		t.Fatalf("rollback deletedIDs=%v", api.deletedIDs)
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("rollback", providerName); claimErr != nil || ok {
		t.Fatalf("rollback claim ok=%v err=%v", ok, claimErr)
	}
}

func TestReleaseCleansLocalClaimWhenInstanceAlreadyAbsent(t *testing.T) {
	cfg := testConfig()
	labels := nebiusLeaseLabels(cfg, "cbx_deadbeef1234", "gone", "ready", false, time.Unix(1000, 0))
	api := &fakeNebiusAPI{getErr: errors.New("not found")}
	b := newTestBackend(t, api)
	server := Server{Provider: providerName, CloudID: "vm-gone", Name: "gone", Labels: labels}
	server.PublicNet.IPv4.IP = "203.0.113.10"
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_deadbeef1234", "gone", b.Cfg, server, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: "cbx_deadbeef1234", Server: server}}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("delete called despite confirmed absence: %v", api.deletedIDs)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("gone", providerName); err != nil || ok {
		t.Fatalf("claim still present ok=%v err=%v", ok, err)
	}
}

func TestResolveReleaseOnlyFindsClaimByCloudID(t *testing.T) {
	cfg := testConfig()
	labels := nebiusLeaseLabels(cfg, "cbx_deadbeef1234", "gone", "ready", false, time.Unix(1000, 0))
	api := &fakeNebiusAPI{}
	b := newTestBackend(t, api)
	server := Server{Provider: providerName, CloudID: "vm-gone", Name: "gone", Labels: labels}
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_deadbeef1234", "gone", b.Cfg, server, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "vm-gone", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_deadbeef1234" || lease.Server.CloudID != "vm-gone" {
		t.Fatalf("lease=%#v", lease)
	}
}

func TestReleaseRefusesLiveOwnershipMismatch(t *testing.T) {
	cfg := testConfig()
	labels := nebiusLeaseLabels(cfg, "cbx_deadbeef1234", "gone", "ready", false, time.Unix(1000, 0))
	liveLabels := cloneLabels(labels)
	liveLabels["lease"] = "cbx_other123456"
	liveLabels[nebiusLeaseLabel] = "cbx_other123456"
	api := &fakeNebiusAPI{items: []nebiusInstance{{ID: "vm-1", Name: "other", Status: "RUNNING", Labels: liveLabels, PublicIP: "203.0.113.10"}}}
	b := newTestBackend(t, api)
	err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: "cbx_deadbeef1234", Server: Server{CloudID: "vm-1", Labels: labels}}})
	if err == nil {
		t.Fatal("ReleaseLease accepted changed live ownership")
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("deleted despite mismatch: %v", api.deletedIDs)
	}
}

func TestAcquireRollbackFailureRetainsCreatedInstanceIDClaim(t *testing.T) {
	api := &fakeNebiusAPI{
		created:   nebiusInstance{ID: "vm-new", Name: "pending", Status: "CREATING", Labels: map[string]string{}},
		deleteErr: errors.New("lost delete response"),
	}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "rollback-failed",
		OnAcquired: func(LeaseTarget) error {
			return errors.New("controller rejected identity")
		},
	})
	if err == nil {
		t.Fatal("Acquire succeeded unexpectedly")
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("rollback-failed", providerName)
	if claimErr != nil || !ok {
		t.Fatalf("recovery claim ok=%v err=%v", ok, claimErr)
	}
	if claim.CloudID != "vm-new" {
		t.Fatalf("recovery claim lost cloud id: %#v", claim)
	}
}

func TestClassifyNebiusIndeterminateErrors(t *testing.T) {
	for _, text := range []string{"timeout waiting", "connection reset by peer", "lost delete response"} {
		if !isIndeterminateNebiusError(errors.New(text)) {
			t.Fatalf("%q not classified indeterminate", text)
		}
	}
	if isIndeterminateNebiusError(errors.New("validation failed")) {
		t.Fatal("validation error classified indeterminate")
	}
}

func cloneLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
