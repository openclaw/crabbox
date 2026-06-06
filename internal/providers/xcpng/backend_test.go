package xcpng

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeLifecycleClient struct {
	calls       []string
	servers     []Server
	templateRef string
	srRef       string
	networkRef  string
	hostRef     string
	cloneVM     xapiVM
	drive       xcpNgConfigDrive
	guestIP     string
	getServer   map[string]Server
	errOn       map[string]error
	mutated     bool
	deleted     []string
	deletedCD   []xcpNgConfigDrive
	setLabels   map[string]map[string]string
}

func (f *fakeLifecycleClient) record(call string) {
	f.calls = append(f.calls, call)
}

func (f *fakeLifecycleClient) fail(call string) error {
	if f.errOn == nil {
		return nil
	}
	return f.errOn[call]
}

func (f *fakeLifecycleClient) Close(context.Context) error {
	f.record("close")
	return nil
}

func (f *fakeLifecycleClient) DoctorInventory(context.Context, xcpNgConfig) ([]Server, error) {
	f.record("doctor-inventory")
	return f.servers, f.fail("doctor-inventory")
}

func (f *fakeLifecycleClient) ListCrabboxServers(context.Context) ([]Server, error) {
	f.record("list")
	out := make([]Server, 0, len(f.servers))
	for _, server := range f.servers {
		if isCrabboxLease(server) {
			out = append(out, server)
		}
	}
	return out, f.fail("list")
}

func (f *fakeLifecycleClient) ResolveTemplate(context.Context, xcpNgConfig) (xapiRef, error) {
	f.record("resolve-template")
	return xapiRef(f.templateRef), f.fail("resolve-template")
}

func (f *fakeLifecycleClient) ResolveSR(context.Context, xcpNgConfig) (xapiRef, error) {
	f.record("resolve-sr")
	return xapiRef(f.srRef), f.fail("resolve-sr")
}

func (f *fakeLifecycleClient) ResolveNetwork(context.Context, xcpNgConfig) (xapiRef, error) {
	f.record("resolve-network")
	return xapiRef(f.networkRef), f.fail("resolve-network")
}

func (f *fakeLifecycleClient) ResolveHost(context.Context, xcpNgConfig) (xapiRef, error) {
	f.record("resolve-host")
	return xapiRef(f.hostRef), f.fail("resolve-host")
}

func (f *fakeLifecycleClient) CloneVM(_ context.Context, req xcpNgCloneRequest) (xapiVM, error) {
	f.record("clone")
	f.mutated = true
	if err := f.fail("clone"); err != nil {
		return xapiVM{}, err
	}
	vm := f.cloneVM
	if vm.Ref == "" {
		vm.Ref = "OpaqueRef:vm-1"
	}
	if vm.Name == "" {
		vm.Name = leaseVMName(req.LeaseID, req.Slug)
	}
	vm.Labels = req.Labels
	return vm, nil
}

func (f *fakeLifecycleClient) AttachConfigDrive(_ context.Context, req xcpNgConfigDriveRequest) (xcpNgConfigDrive, error) {
	f.record("attach-config-drive")
	f.mutated = true
	if err := f.fail("attach-config-drive"); err != nil {
		return xcpNgConfigDrive{}, err
	}
	if !strings.Contains(req.Payload.UserData, req.PublicKeyNotAvailableForTests()) {
		// Keep the fake focused on cloud-init being non-empty without coupling to key text.
	}
	if f.drive.Name == "" {
		f.drive.Name = configDriveName(req.LeaseID, req.Slug)
		f.drive.Labels = configDriveLabels(req.Labels)
	}
	return f.drive, nil
}

func (req xcpNgConfigDriveRequest) PublicKeyNotAvailableForTests() string { return "" }

func (f *fakeLifecycleClient) StartVM(context.Context, xapiRef) error {
	f.record("start")
	f.mutated = true
	return f.fail("start")
}

func (f *fakeLifecycleClient) GuestIPv4(context.Context, xapiRef) (string, error) {
	f.record("guest-ip")
	if err := f.fail("guest-ip"); err != nil {
		return "", err
	}
	return f.guestIP, nil
}

func (f *fakeLifecycleClient) GetServer(_ context.Context, id string) (Server, error) {
	f.record("get")
	if f.getServer != nil {
		if server, ok := f.getServer[id]; ok {
			return server, nil
		}
	}
	return Server{}, xapiHTTPError{StatusCode: 404, Body: "not found"}
}

func (f *fakeLifecycleClient) SetLabels(_ context.Context, id string, labels map[string]string) error {
	f.record("set-labels")
	f.mutated = true
	if f.setLabels == nil {
		f.setLabels = map[string]map[string]string{}
	}
	f.setLabels[id] = labels
	return f.fail("set-labels")
}

func (f *fakeLifecycleClient) DeleteServer(_ context.Context, id string) error {
	f.record("delete")
	f.mutated = true
	f.deleted = append(f.deleted, id)
	return f.fail("delete")
}

func (f *fakeLifecycleClient) DeleteConfigDrive(_ context.Context, drive xcpNgConfigDrive) error {
	f.record("delete-config-drive")
	f.mutated = true
	f.deletedCD = append(f.deletedCD, drive)
	return f.fail("delete-config-drive")
}

func TestDoctorUsesReadOnlyInventory(t *testing.T) {
	fake := &fakeLifecycleClient{servers: []Server{crabboxServer("OpaqueRef:vm-1", "cbx_lease", "ready", time.Now().Add(time.Hour))}}
	backend := newTestBackend(t, fake)
	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != "xcp-ng" || !strings.Contains(result.Message, "mutation=false") || !strings.Contains(result.Message, "leases=1") {
		t.Fatalf("result=%#v", result)
	}
	if fake.mutated {
		t.Fatal("doctor mutated provider state")
	}
	if got, want := fake.calls, []string{"doctor-inventory", "close"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls=%v want %v", got, want)
	}
}

func TestAcquireLifecycleCallOrderAndTarget(t *testing.T) {
	fake := &fakeLifecycleClient{
		templateRef: "OpaqueRef:tpl",
		srRef:       "OpaqueRef:sr",
		networkRef:  "OpaqueRef:net",
		hostRef:     "OpaqueRef:host",
		guestIP:     "192.0.2.44",
	}
	backend := newTestBackend(t, fake)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_testlease" || lease.SSH.Host != "192.0.2.44" || lease.SSH.Key != "/tmp/crabbox-test-key" {
		t.Fatalf("lease=%#v", lease)
	}
	wantCalls := []string{"list", "resolve-template", "resolve-sr", "resolve-network", "resolve-host", "clone", "attach-config-drive", "start", "guest-ip", "set-labels", "close"}
	if !reflect.DeepEqual(fake.calls, wantCalls) {
		t.Fatalf("calls=%v want %v", fake.calls, wantCalls)
	}
	if labels := fake.setLabels["OpaqueRef:vm-1"]; labels["state"] != "ready" || labels["lease"] != "cbx_testlease" || labels["provider"] != "xcp-ng" {
		t.Fatalf("labels=%#v", labels)
	}
}

func TestAcquireCleansUpVMAndConfigDriveOnGuestIPFailure(t *testing.T) {
	fake := &fakeLifecycleClient{
		templateRef: "OpaqueRef:tpl",
		drive:       xcpNgConfigDrive{VDIRef: "OpaqueRef:vdi", VBDRef: "OpaqueRef:vbd", Name: "drive"},
		errOn:       map[string]error{"guest-ip": errors.New("guest tools missing")},
	}
	backend := newTestBackend(t, fake)
	if _, err := backend.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "blue"}); err == nil {
		t.Fatal("expected guest IP failure")
	}
	if !reflect.DeepEqual(fake.deleted, []string{"OpaqueRef:vm-1"}) {
		t.Fatalf("deleted=%v", fake.deleted)
	}
	if len(fake.deletedCD) != 1 || fake.deletedCD[0].VDIRef != "OpaqueRef:vdi" {
		t.Fatalf("deleted config drives=%#v", fake.deletedCD)
	}
}

func TestResolveRejectsExistingNonCrabboxVM(t *testing.T) {
	fake := &fakeLifecycleClient{getServer: map[string]Server{"OpaqueRef:user": {CloudID: "OpaqueRef:user", Name: "user-vm", Labels: map[string]string{"crabbox": "false"}}}}
	backend := newTestBackend(t, fake)
	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "OpaqueRef:user"}); err == nil || !strings.Contains(err.Error(), "not Crabbox-managed") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveByAliasReturnsGuestIPLookupErrorWhenHostMissing(t *testing.T) {
	managed := crabboxServer("OpaqueRef:vm-1", "cbx_lease", "ready", time.Now().Add(time.Hour))
	managed.PublicNet.IPv4.IP = ""
	managed.PrivateNet.IPv4.IP = ""
	fake := &fakeLifecycleClient{
		servers: []Server{managed},
		errOn:   map[string]error{"guest-ip": errors.New("guest metrics unavailable")},
	}
	backend := newTestBackend(t, fake)
	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "lease"}); err == nil || !strings.Contains(err.Error(), "guest metrics unavailable") {
		t.Fatalf("err=%v", err)
	}
	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "lease", ReleaseOnly: true}); err != nil {
		t.Fatalf("release-only resolve err=%v", err)
	}
}

func TestListResolveTouchReleaseUseOnlyCrabboxMetadata(t *testing.T) {
	managed := crabboxServer("OpaqueRef:vm-1", "cbx_lease", "ready", time.Now().Add(time.Hour))
	unmanaged := Server{CloudID: "OpaqueRef:vm-2", Name: "crabbox-prefix-only", Labels: map[string]string{"provider": "xcp-ng"}}
	fake := &fakeLifecycleClient{
		servers: []Server{managed, unmanaged},
		getServer: map[string]Server{
			"cbx_lease": managed,
		},
	}
	backend := newTestBackend(t, fake)
	servers, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].CloudID != "OpaqueRef:vm-1" {
		t.Fatalf("servers=%#v", servers)
	}
	resolved, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "cbx_lease"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.LeaseID != "cbx_lease" {
		t.Fatalf("resolved=%#v", resolved)
	}
	touched, err := backend.Touch(context.Background(), core.TouchRequest{Lease: resolved, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if touched.Labels["state"] != "active" {
		t.Fatalf("touched=%#v", touched.Labels)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: resolved}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.deleted, []string{"OpaqueRef:vm-1"}) {
		t.Fatalf("deleted=%v", fake.deleted)
	}
}

func TestCleanupIsMetadataAndExpiryGated(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	fake := &fakeLifecycleClient{servers: []Server{
		crabboxServer("OpaqueRef:expired", "cbx_expired", "ready", now.Add(-time.Minute)),
		crabboxServer("OpaqueRef:fresh", "cbx_fresh", "ready", now.Add(time.Hour)),
		{CloudID: "OpaqueRef:prefix", Name: "crabbox-prefix-only", Labels: map[string]string{"provider": "xcp-ng"}},
	}}
	backend := newTestBackend(t, fake)
	backend.RT.Clock = fixedClock{t: now}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.deleted, []string{"OpaqueRef:expired"}) {
		t.Fatalf("deleted=%v", fake.deleted)
	}
}

func TestCleanupDryRunDoesNotDelete(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	fake := &fakeLifecycleClient{servers: []Server{crabboxServer("OpaqueRef:expired", "cbx_expired", "ready", now.Add(-time.Minute))}}
	backend := newTestBackend(t, fake)
	backend.RT.Clock = fixedClock{t: now}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("dry-run deleted=%v", fake.deleted)
	}
}

func newTestBackend(t *testing.T, fake *fakeLifecycleClient) *leaseBackend {
	t.Helper()
	oldClient := newLifecycleClient
	oldLeaseID := newLeaseID
	oldKey := ensureTestboxKeyForConfig
	oldWait := waitForSSHReady
	oldBootstrapTimeout := bootstrapWaitTimeout
	oldPollInterval := guestIPPollInterval
	oldRemove := removeLeaseClaim
	newLifecycleClient = func(context.Context, Config) (lifecycleClient, error) { return fake, nil }
	newLeaseID = func() string { return "cbx_testlease" }
	ensureTestboxKeyForConfig = func(Config, string) (string, string, error) {
		return "/tmp/crabbox-test-key", "ssh-ed25519 AAAATEST crabbox", nil
	}
	waitForSSHReady = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error { return nil }
	bootstrapWaitTimeout = func(Config) time.Duration { return 10 * time.Millisecond }
	guestIPPollInterval = time.Millisecond
	removeLeaseClaim = func(string) {}
	t.Cleanup(func() {
		newLifecycleClient = oldClient
		newLeaseID = oldLeaseID
		ensureTestboxKeyForConfig = oldKey
		waitForSSHReady = oldWait
		bootstrapWaitTimeout = oldBootstrapTimeout
		guestIPPollInterval = oldPollInterval
		removeLeaseClaim = oldRemove
	})
	cfg := testConfig()
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stderr: &bytes.Buffer{}}).(*leaseBackend)
	return backend
}

func testConfig() Config {
	cfg := Config{}
	cfg.Provider = "xcp-ng"
	cfg.TargetOS = "linux"
	cfg.SSHUser = "crabbox"
	cfg.SSHPort = "22"
	cfg.WorkRoot = "/work/crabbox"
	cfg.IdleTimeout = time.Hour
	cfg.TTL = 2 * time.Hour
	cfg.XCPNg.APIURL = "https://xcp-ng.example.test"
	cfg.XCPNg.Username = "root"
	cfg.XCPNg.Password = "secret"
	cfg.XCPNg.Template = "ubuntu-template"
	cfg.XCPNg.SRUUID = "sr-uuid"
	cfg.XCPNg.User = "crabbox"
	cfg.XCPNg.WorkRoot = "/work/crabbox"
	return cfg
}

func crabboxServer(id, lease, state string, expires time.Time) Server {
	labels := map[string]string{
		"crabbox":     "true",
		"created_by":  "crabbox",
		"provider":    "xcp-ng",
		"lease":       lease,
		"slug":        strings.TrimPrefix(lease, "cbx_"),
		"state":       state,
		"keep":        "false",
		"expires_at":  core.LeaseLabelTime(expires),
		"server_type": "template-ubuntu",
	}
	server := Server{CloudID: id, Name: "crabbox-" + strings.TrimPrefix(lease, "cbx_"), Status: state, Labels: labels, Provider: "xcp-ng"}
	server.PublicNet.IPv4.IP = "192.0.2.44"
	return server
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }
