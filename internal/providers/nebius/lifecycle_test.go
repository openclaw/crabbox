package nebius

import (
	"context"
	"errors"
	"io"
	"os"
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
	deleteFn        func(context.Context, string)
	deleteDeadline  bool
	deleteCtxErr    error
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
	return nebiusInstance{}, errors.New("instance " + id + " not found")
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

func (f *fakeNebiusAPI) DeleteInstance(ctx context.Context, id string) error {
	f.deletedIDs = append(f.deletedIDs, id)
	_, f.deleteDeadline = ctx.Deadline()
	f.deleteCtxErr = ctx.Err()
	if f.deleteFn != nil {
		f.deleteFn(ctx, id)
	}
	return f.deleteErr
}

func newTestBackend(t *testing.T, api *fakeNebiusAPI) *backend {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
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
	cfg := testConfig()
	cfg.SSHUser = "alice"
	got, err := renderNebiusCloudInit(cfg, "ssh-ed25519 AAAA test")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#cloud-config", `name: "alice"`, "NOPASSWD", "ssh-ed25519 AAAA test", "/usr/local/bin/crabbox-ready", "/var/lib/crabbox/bootstrapped", "rsync --version", "disable_root: true"} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloud-init missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `name: "crabbox"`) {
		t.Fatal("cloud-init ignored effective SSH user")
	}
	cfg.SSHUser = "root"
	if _, err := renderNebiusCloudInit(cfg, "ssh-ed25519 AAAA test"); err == nil {
		t.Fatal("reserved root user accepted")
	}
	for _, user := range []string{"alice\n  - name: root", "Alice", "bad user", "1alice", "alice:$evil"} {
		cfg.SSHUser = user
		if _, err := renderNebiusCloudInit(cfg, "ssh-ed25519 AAAA test"); err == nil {
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
		if !strings.Contains(joined, "--network-interfaces") || !strings.Contains(joined, `"name":"eth0"`) || !strings.Contains(joined, `"subnet_id":"subnet-123"`) || !strings.Contains(joined, `"ip_address":{}`) || !strings.Contains(joined, `"public_ip_address":{}`) {
			return LocalCommandResult{}, errors.New("missing dynamic public IP network interface: " + joined)
		}
		if !strings.Contains(joined, "--boot-disk-managed-disk-name cbx-demo") ||
			!strings.Contains(joined, "--boot-disk-managed-disk-source-image-family-image-family ubuntu24.04-driverless") ||
			!strings.Contains(joined, "--boot-disk-managed-disk-type network_ssd") ||
			!strings.Contains(joined, "--boot-disk-managed-disk-size-gibibytes 50") ||
			!strings.Contains(joined, "--boot-disk-attach-mode read_write") {
			return LocalCommandResult{}, errors.New("missing managed boot disk args: " + joined)
		}
		if strings.Contains(joined, "--boot-disk-managed-disk-source-image-family-parent-id") {
			return LocalCommandResult{}, errors.New("public image family incorrectly scoped to the lease project: " + joined)
		}
		for i, arg := range req.Args {
			if arg == "--cloud-init-user-data" && i+1 < len(req.Args) && req.Args[i+1] != "#cloud-config\n" {
				return LocalCommandResult{}, errors.New("cloud-init content was not passed as a raw string")
			}
		}
		if !strings.Contains(joined, "--labels ") || !strings.Contains(joined, "crabbox_provider=nebius") || !strings.Contains(joined, "crabbox_scope=") {
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

func TestCLIErrorRedactsCloudInitAndDeleteUsesSupportedFlags(t *testing.T) {
	runner := &recordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{Stderr: "request failed"}, errors.New("request failed")
	}}
	client := newNebiusClient(testConfig().Nebius, Runtime{Exec: runner})
	secretMarker := "ssh-ed25519 AAAA-marker"
	_, err := client.CreateInstance(context.Background(), nebiusCreateRequest{Name: "cbx-demo", UserData: "#cloud-config\n" + secretMarker})
	if err == nil || strings.Contains(err.Error(), secretMarker) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("CreateInstance err=%v", err)
	}
	runner.fn = nil
	if err := client.DeleteInstance(context.Background(), "vm-1"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(runner.calls[len(runner.calls)-1], " ")
	if strings.Contains(got, "--quiet") || !strings.Contains(got, "compute instance delete vm-1 --format json") {
		t.Fatalf("delete command=%q", got)
	}
}

func TestUpdateLabelsIncludesParentScope(t *testing.T) {
	runner := &recordingRunner{}
	client := newNebiusClient(testConfig().Nebius, Runtime{Exec: runner})
	if err := client.UpdateLabels(context.Background(), "vm-1", map[string]string{"state": "ready"}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(runner.calls[len(runner.calls)-1], " ")
	if !strings.Contains(got, "compute instance update vm-1 --parent-id project-123 --labels-add state=ready --format json") {
		t.Fatalf("update command=%q", got)
	}
}

func TestListInstancesRequestsAllPages(t *testing.T) {
	runner := &recordingRunner{fn: func(LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{Stdout: `{"items":[]}`}, nil
	}}
	client := newNebiusClient(testConfig().Nebius, Runtime{Exec: runner})
	if _, err := client.ListInstances(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(runner.calls[len(runner.calls)-1], " ")
	if !strings.Contains(got, "compute instance list --parent-id project-123 --all --format json") {
		t.Fatalf("list command=%q", got)
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

func TestResolveAssociatesLiveInstanceWithRepoUnlessMutationsDisabled(t *testing.T) {
	cfg := testConfig()
	leaseID := "cbx_a123456789ab"
	labels := nebiusLeaseLabels(cfg, leaseID, "demo", "ready", false, time.Unix(1000, 0))
	api := &fakeNebiusAPI{items: []nebiusInstance{{ID: "vm-1", Name: "demo", Status: "RUNNING", Labels: labels, PublicIP: "203.0.113.10"}}}
	b := newTestBackend(t, api)
	repo := t.TempDir()
	if _, err := b.Resolve(context.Background(), ResolveRequest{ID: leaseID, Repo: core.Repo{Root: repo}}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if claim.RepoRoot != repo || claim.CloudID != "vm-1" {
		t.Fatalf("claim=%#v", claim)
	}

	otherLeaseID := "cbx_b123456789ab"
	otherLabels := nebiusLeaseLabels(cfg, otherLeaseID, "no-mutate", "ready", false, time.Unix(1000, 0))
	api.items = append(api.items, nebiusInstance{ID: "vm-2", Name: "no-mutate", Status: "RUNNING", Labels: otherLabels, PublicIP: "203.0.113.11"})
	if _, err := b.Resolve(context.Background(), ResolveRequest{ID: otherLeaseID, Repo: core.Repo{Root: repo}, NoLocalStateMutations: true}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(otherLeaseID); err != nil || exists {
		t.Fatalf("no-mutation claim exists=%v err=%v", exists, err)
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
	leaseID := "cbx_123456789abc"
	claimedServer := Server{Provider: providerName, CloudID: "vm-1", Name: "demo", Labels: labels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "demo", b.Cfg, claimedServer, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	server, err := b.Touch(context.Background(), TouchRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: claimedServer}, State: "ready", IdleTimeout: 30 * time.Minute})
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
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists {
		t.Fatalf("claim exists=%v err=%v", exists, err)
	}
	if claim.Labels["state"] != "ready" || claim.Labels["idle_timeout_secs"] != "1800" {
		t.Fatalf("claim labels=%v", claim.Labels)
	}
	if claim.IdleTimeoutSeconds != 1800 {
		t.Fatalf("claim idle timeout=%d", claim.IdleTimeoutSeconds)
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

func TestAcquireQuotaFailureWithTimeoutLabelDoesNotRetainRecoveryClaim(t *testing.T) {
	api := &fakeNebiusAPI{createErr: errors.New("create --labels idle_timeout=300 failed: quota limit exceeded")}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "quota-failure"})
	if err == nil {
		t.Fatal("Acquire succeeded unexpectedly")
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("quota-failure", providerName); claimErr != nil || ok {
		t.Fatalf("quota failure claim ok=%v err=%v", ok, claimErr)
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

func TestAcquireKeepRetainsCreatedInstanceClaimAndKeyAfterFailure(t *testing.T) {
	api := &fakeNebiusAPI{created: nebiusInstance{ID: "vm-kept", Name: "pending", Status: "CREATING"}}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "kept-failure",
		Keep:          true,
		OnAcquired: func(LeaseTarget) error {
			return errors.New("controller rejected identity")
		},
	})
	if err == nil {
		t.Fatal("Acquire succeeded unexpectedly")
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("kept instance deleted: %v", api.deletedIDs)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("kept-failure", providerName)
	if claimErr != nil || !ok {
		t.Fatalf("recovery claim ok=%v err=%v", ok, claimErr)
	}
	if claim.CloudID != "vm-kept" || claim.Labels["recovery"] != "kept-after-failure" {
		t.Fatalf("recovery claim=%#v", claim)
	}
	keyPath, keyErr := core.TestboxKeyPath(claim.LeaseID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("kept SSH key missing: %v", statErr)
	}
}

func TestAcquireRollbackUsesBoundedContextIndependentOfCaller(t *testing.T) {
	api := &fakeNebiusAPI{created: nebiusInstance{ID: "vm-new", Name: "pending", Status: "CREATING"}}
	b := newTestBackend(t, api)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := b.Acquire(ctx, AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "rollback-context",
		OnAcquired: func(LeaseTarget) error {
			return errors.New("controller rejected identity")
		},
	})
	if err == nil {
		t.Fatal("Acquire succeeded unexpectedly")
	}
	if !api.deleteDeadline || api.deleteCtxErr != nil {
		t.Fatalf("rollback context deadline=%v err=%v", api.deleteDeadline, api.deleteCtxErr)
	}
}

func TestReleaseCleansLocalClaimWhenInstanceAlreadyAbsent(t *testing.T) {
	cfg := testConfig()
	leaseID := "cbx_absent123456"
	slug := "gone-absent"
	cloudID := "vm-gone-absent"
	labels := nebiusLeaseLabels(cfg, leaseID, slug, "ready", false, time.Unix(1000, 0))
	api := &fakeNebiusAPI{getErr: errors.New("instance " + cloudID + " not found")}
	b := newTestBackend(t, api)
	server := Server{Provider: providerName, CloudID: cloudID, Name: slug, Labels: labels}
	server.PublicNet.IPv4.IP = "203.0.113.10"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, b.Cfg, server, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("delete called despite confirmed absence: %v", api.deletedIDs)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName); err != nil || ok {
		t.Fatalf("claim still present ok=%v err=%v", ok, err)
	}
}

func TestReleaseRetainsClaimChangedDuringDelete(t *testing.T) {
	cfg := testConfig()
	leaseID := "cbx_changed12345"
	slug := "changed-release"
	cloudID := "vm-changed-release"
	labels := nebiusLeaseLabels(cfg, leaseID, slug, "ready", false, time.Unix(1000, 0))
	api := &fakeNebiusAPI{items: []nebiusInstance{{ID: cloudID, Name: slug, Status: "RUNNING", Labels: labels, PublicIP: "203.0.113.10"}}}
	b := newTestBackend(t, api)
	server := Server{Provider: providerName, CloudID: cloudID, Name: slug, Labels: labels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, b.Cfg, server, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	original, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists {
		t.Fatalf("claim exists=%v err=%v", exists, err)
	}
	api.deleteFn = func(context.Context, string) {
		changed := cloneLabels(original.Labels)
		changed["state"] = "renewed"
		if _, updateErr := core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, original, changed); updateErr != nil {
			t.Errorf("update claim during delete: %v", updateErr)
		}
	}
	err = b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}})
	if err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	claim, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID)
	if readErr != nil || !exists || claim.Labels["state"] != "renewed" {
		t.Fatalf("changed claim exists=%v err=%v claim=%#v", exists, readErr, claim)
	}
}

func TestReleaseRetainsIdentitylessRecoveryClaim(t *testing.T) {
	cfg := testConfig()
	leaseID := "cbx_identityless1"
	slug := "identityless"
	labels := nebiusLeaseLabels(cfg, leaseID, slug, "provisioning", false, time.Unix(1000, 0))
	api := &fakeNebiusAPI{}
	b := newTestBackend(t, api)
	if err := b.persistRecoveryClaim(leaseID, slug, "", b.Cfg, t.TempDir(), labels, false); err != nil {
		t.Fatal(err)
	}
	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	err = b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease})
	if err == nil || !strings.Contains(err.Error(), "no instance identity") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("identityless delete called: %v", api.deletedIDs)
	}
	if _, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID); readErr != nil || !exists {
		t.Fatalf("claim exists=%v err=%v", exists, readErr)
	}
}

func TestReleaseDoesNotCleanClaimOnUnrelatedNotFound(t *testing.T) {
	cfg := testConfig()
	leaseID := "cbx_unrelated123"
	slug := "gone-unrelated"
	cloudID := "vm-gone-unrelated"
	labels := nebiusLeaseLabels(cfg, leaseID, slug, "ready", false, time.Unix(1000, 0))
	api := &fakeNebiusAPI{getErr: errors.New("profile production not found")}
	b := newTestBackend(t, api)
	server := Server{Provider: providerName, CloudID: cloudID, Name: slug, Labels: labels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, b.Cfg, server, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}); err == nil {
		t.Fatal("ReleaseLease cleaned state after unrelated not-found error")
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName); err != nil || !ok {
		t.Fatalf("claim removed after unrelated not-found ok=%v err=%v", ok, err)
	}
}

func TestResolveReleaseOnlyFindsClaimByCloudID(t *testing.T) {
	cfg := testConfig()
	leaseID := "cbx_resolve12345"
	slug := "gone-resolve"
	cloudID := "vm-gone-resolve"
	labels := nebiusLeaseLabels(cfg, leaseID, slug, "ready", false, time.Unix(1000, 0))
	api := &fakeNebiusAPI{}
	b := newTestBackend(t, api)
	server := Server{Provider: providerName, CloudID: cloudID, Name: slug, Labels: labels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, b.Cfg, server, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: cloudID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.Server.CloudID != cloudID {
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
	for _, text := range []string{"timeout waiting", "i/o timeout", "TLS handshake timeout", "request timeout", "connection reset by peer", "lost delete response"} {
		if !isIndeterminateNebiusError(errors.New(text)) {
			t.Fatalf("%q not classified indeterminate", text)
		}
	}
	for _, text := range []string{"validation failed", "create --labels idle_timeout=300 failed: quota limit exceeded"} {
		if isIndeterminateNebiusError(errors.New(text)) {
			t.Fatalf("%q classified indeterminate", text)
		}
	}
}

func TestClassifyNebiusInstanceNotFoundRequiresInstanceEvidence(t *testing.T) {
	if !isNebiusInstanceNotFound(errors.New("instance vm-gone not found"), "vm-gone") {
		t.Fatal("instance-specific not found was not classified")
	}
	if isNebiusInstanceNotFound(errors.New("profile production not found"), "vm-gone") {
		t.Fatal("unrelated not found classified as instance absence")
	}
}

func cloneLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
