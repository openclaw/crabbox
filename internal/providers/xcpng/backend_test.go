package xcpng

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const xcpNgTestVMUUID = "11111111-1111-1111-1111-111111111111"

type fakeLifecycleClient struct {
	calls           []string
	servers         []Server
	templateRef     string
	srRef           string
	networkRef      string
	hostRef         string
	iso             xcpNgISOMediaRef
	cloneVM         xapiVM
	freshVM         xcpNgFreshVMResult
	freshReq        xcpNgFreshVMRequest
	drive           xcpNgConfigDrive
	importedISO     xcpNgConfigDrive
	attachedDisk    xcpNgConfigDrive
	guestIP         string
	discoveredIP    string
	getServer       map[string]Server
	errOn           map[string]error
	mutated         bool
	deleted         []string
	deletedCD       []xcpNgConfigDrive
	deleteBounded   bool
	deleteCDBounded bool
	closeBounded    bool
	closeCanceled   bool
	setLabels       map[string]map[string]string
	afterGuestIP    func()
	diskReq         xcpNgDiskAttachRequest
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

func (f *fakeLifecycleClient) Close(ctx context.Context) error {
	f.record("close")
	_, f.closeBounded = ctx.Deadline()
	f.closeCanceled = ctx.Err() != nil
	return f.fail("close")
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

func (f *fakeLifecycleClient) ResolveISOMedia(context.Context, xcpNgConfig, string) (xcpNgISOMediaRef, error) {
	f.record("resolve-iso")
	if err := f.fail("resolve-iso"); err != nil {
		return xcpNgISOMediaRef{}, err
	}
	return f.iso, nil
}

func (f *fakeLifecycleClient) CloneVM(_ context.Context, req xcpNgCloneRequest) (xapiVM, error) {
	f.record("clone")
	f.mutated = true
	if err := f.fail("clone"); err != nil {
		return f.cloneVM, err
	}
	vm := f.cloneVM
	if vm.Ref == "" {
		vm.Ref = "OpaqueRef:vm-1"
	}
	if vm.UUID == "" {
		vm.UUID = xcpNgTestVMUUID
	}
	if vm.Name == "" {
		vm.Name = leaseVMName(req.LeaseID, req.Slug)
	}
	vm.Labels = req.Labels
	return vm, nil
}

func (f *fakeLifecycleClient) CreateFreshVM(_ context.Context, req xcpNgFreshVMRequest) (xcpNgFreshVMResult, error) {
	f.record("create-fresh-vm")
	f.mutated = true
	f.freshReq = req
	if err := f.fail("create-fresh-vm"); err != nil {
		return f.freshVM, err
	}
	result := f.freshVM
	if result.VM.Ref == "" {
		result.VM.Ref = "OpaqueRef:fresh-vm"
	}
	if result.VM.UUID == "" {
		result.VM.UUID = xcpNgTestVMUUID
	}
	if result.VM.Name == "" {
		result.VM.Name = req.Name
	}
	result.VM.Labels = req.Labels
	if result.VIFRef == "" && req.Network != nil {
		result.VIFRef = "OpaqueRef:vif"
	}
	if result.VTPMRef == "" && req.VTPM {
		result.VTPMRef = "OpaqueRef:vtpm"
	}
	return result, nil
}

func (f *fakeLifecycleClient) ImportISO(_ context.Context, req xcpNgImportISORequest) (xcpNgConfigDrive, error) {
	f.record("import-iso")
	f.mutated = true
	if err := f.fail("import-iso"); err != nil {
		return f.importedISO, err
	}
	drive := f.importedISO
	if drive.VDIRef == "" {
		drive.VDIRef = "OpaqueRef:imported-iso-vdi"
	}
	if drive.Name == "" {
		drive.Name = req.Name
	}
	if drive.Labels == nil {
		drive.Labels = isoMediaLabels(req.Labels)
	}
	drive.DestroyVDI = req.DestroyVDI
	return drive, nil
}

func (f *fakeLifecycleClient) AttachDisk(_ context.Context, req xcpNgDiskAttachRequest) (xcpNgConfigDrive, error) {
	f.record("attach-disk")
	f.mutated = true
	f.diskReq = req
	if err := f.fail("attach-disk"); err != nil {
		return f.attachedDisk, err
	}
	drive := f.attachedDisk
	if drive.VDIRef == "" {
		drive.VDIRef = "OpaqueRef:disk-vdi"
	}
	if drive.VBDRef == "" {
		drive.VBDRef = "OpaqueRef:disk-vbd"
	}
	if drive.Name == "" {
		drive.Name = req.Name
	}
	if drive.Labels == nil {
		drive.Labels = vmDiskLabels(req.Labels)
	}
	drive.DestroyVDI = req.DestroyVDI
	return drive, nil
}

func (f *fakeLifecycleClient) AttachConfigDrive(_ context.Context, req xcpNgConfigDriveRequest) (xcpNgConfigDrive, error) {
	f.record("attach-config-drive")
	f.mutated = true
	if err := f.fail("attach-config-drive"); err != nil {
		return f.drive, err
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

func (f *fakeLifecycleClient) AttachISO(_ context.Context, req xcpNgISOAttachRequest) (xcpNgConfigDrive, error) {
	f.record("attach-iso")
	f.mutated = true
	if err := f.fail("attach-iso"); err != nil {
		return xcpNgConfigDrive{}, err
	}
	drive := f.drive
	if drive.VDIRef == "" {
		drive.VDIRef = req.ISO.VDIRef
	}
	if drive.Name == "" {
		drive.Name = req.ISO.NameLabel
	}
	if drive.Labels == nil {
		drive.Labels = isoMediaLabels(req.Labels)
	}
	if drive.VBDRef == "" {
		drive.VBDRef = "OpaqueRef:iso-vbd"
	}
	return drive, nil
}

func (req xcpNgConfigDriveRequest) PublicKeyNotAvailableForTests() string { return "" }

func (f *fakeLifecycleClient) StartVM(context.Context, xapiRef) error {
	f.record("start")
	f.mutated = true
	return f.fail("start")
}

func (f *fakeLifecycleClient) SetVMBootOrder(context.Context, xapiRef, string) error {
	f.record("set-boot-order")
	f.mutated = true
	return f.fail("set-boot-order")
}

func (f *fakeLifecycleClient) GuestIPv4(context.Context, xapiRef) (string, error) {
	f.record("guest-ip")
	if err := f.fail("guest-ip"); err != nil {
		return "", err
	}
	if f.afterGuestIP != nil {
		f.afterGuestIP()
	}
	return f.guestIP, nil
}

func (f *fakeLifecycleClient) GuestIPv4ForID(context.Context, string) (string, error) {
	f.record("guest-ip-by-id")
	if err := f.fail("guest-ip"); err != nil {
		return "", err
	}
	return f.guestIP, nil
}

func (f *fakeLifecycleClient) DiscoverGuestIPv4(context.Context, xapiRef) (string, error) {
	f.record("discover-guest-ip")
	if err := f.fail("discover-guest-ip"); err != nil {
		return "", err
	}
	return f.discoveredIP, nil
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

func (f *fakeLifecycleClient) DeleteServer(ctx context.Context, id string) error {
	f.record("delete")
	f.mutated = true
	f.deleted = append(f.deleted, id)
	_, f.deleteBounded = ctx.Deadline()
	return f.fail("delete")
}

func (f *fakeLifecycleClient) DeleteFreshServer(ctx context.Context, id, vtpmRef string) error {
	f.record("delete-fresh-server")
	if vtpmRef != "" {
		f.record("delete-vtpm")
	}
	return f.DeleteServer(ctx, id)
}

func (f *fakeLifecycleClient) DeleteConfigDrive(ctx context.Context, drive xcpNgConfigDrive) error {
	f.record("delete-config-drive")
	f.mutated = true
	f.deletedCD = append(f.deletedCD, drive)
	_, f.deleteCDBounded = ctx.Deadline()
	return f.fail("delete-config-drive")
}

func TestDoctorUsesReadOnlyPlacementAndInventory(t *testing.T) {
	fake := &fakeLifecycleClient{
		servers:     []Server{crabboxServer("OpaqueRef:vm-1", "cbx_lease", "ready", time.Now().Add(time.Hour))},
		templateRef: "OpaqueRef:tpl",
		srRef:       "OpaqueRef:sr",
	}
	backend := newTestBackend(t, fake)
	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != "xcp-ng" || !strings.Contains(result.Message, "placement=ready") || !strings.Contains(result.Message, "mutation=false") || !strings.Contains(result.Message, "leases=1") {
		t.Fatalf("result=%#v", result)
	}
	if fake.mutated {
		t.Fatal("doctor mutated provider state")
	}
	if got, want := fake.calls, []string{"resolve-template", "resolve-sr", "doctor-inventory", "close"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls=%v want %v", got, want)
	}
}

func TestDoctorValidatesOptionalPlacementReadOnly(t *testing.T) {
	fake := &fakeLifecycleClient{
		templateRef: "OpaqueRef:tpl",
		srRef:       "OpaqueRef:sr",
		networkRef:  "OpaqueRef:net",
		hostRef:     "OpaqueRef:host",
	}
	backend := newTestBackend(t, fake)
	backend.Cfg.XCPNg.Network = "pool-network"
	backend.Cfg.XCPNg.Host = "host-a"
	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" || !strings.Contains(result.Message, "placement=ready") {
		t.Fatalf("result=%#v", result)
	}
	if fake.mutated {
		t.Fatal("doctor mutated provider state")
	}
	wantCalls := []string{"resolve-template", "resolve-sr", "resolve-network", "resolve-host", "doctor-inventory", "close"}
	if !reflect.DeepEqual(fake.calls, wantCalls) {
		t.Fatalf("calls=%v want %v", fake.calls, wantCalls)
	}
}

func TestDoctorReportsPlacementFailureWithoutInventory(t *testing.T) {
	fake := &fakeLifecycleClient{
		templateRef: "OpaqueRef:tpl",
		srRef:       "OpaqueRef:sr",
		errOn:       map[string]error{"resolve-network": errors.New("network not found")},
	}
	backend := newTestBackend(t, fake)
	backend.Cfg.XCPNg.Network = "missing-network"
	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || !strings.Contains(result.Message, "placement=failed") || !strings.Contains(result.Message, "inventory=unchecked") {
		t.Fatalf("result=%#v", result)
	}
	if len(result.Checks) != 2 || result.Checks[1].Check != "placement" || result.Checks[1].Status != "failed" || !strings.Contains(result.Checks[1].Message, "network not found") {
		t.Fatalf("checks=%#v", result.Checks)
	}
	if fake.mutated {
		t.Fatal("doctor mutated provider state")
	}
	wantCalls := []string{"resolve-template", "resolve-sr", "resolve-network", "close"}
	if !reflect.DeepEqual(fake.calls, wantCalls) {
		t.Fatalf("calls=%v want %v", fake.calls, wantCalls)
	}
}

func TestDoctorFailsConfigurationWhenTemplateMissing(t *testing.T) {
	fake := &fakeLifecycleClient{}
	backend := newTestBackend(t, fake)
	backend.Cfg.XCPNg.Template = ""
	backend.Cfg.XCPNg.TemplateUUID = ""
	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || !strings.Contains(result.Message, "auth=configuration-incomplete") {
		t.Fatalf("result=%#v", result)
	}
	if len(result.Checks) != 1 || result.Checks[0].Check != "configuration" || result.Checks[0].Status != "failed" || !strings.Contains(result.Checks[0].Message, "xcpNg.template/xcpNg.templateUuid") {
		t.Fatalf("checks=%#v", result.Checks)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("doctor should fail before opening XAPI session, calls=%v", fake.calls)
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
	if lease.Server.CloudID != xcpNgTestVMUUID {
		t.Fatalf("lease server should expose durable UUID cloud id, got %#v", lease.Server)
	}
	if labels := fake.setLabels[xcpNgTestVMUUID]; labels["state"] != "ready" || labels["lease"] != "cbx_testlease" || labels["provider"] != "xcp-ng" {
		t.Fatalf("labels=%#v", labels)
	}
}

func TestAcquireRefreshesReadyLeaseTouchTimeAfterBootstrap(t *testing.T) {
	start := time.Unix(1700000000, 0).UTC()
	ready := start.Add(15 * time.Minute)
	clock := mutableClock{t: start}
	fake := &fakeLifecycleClient{
		templateRef: "OpaqueRef:tpl",
		srRef:       "OpaqueRef:sr",
		networkRef:  "OpaqueRef:net",
		hostRef:     "OpaqueRef:host",
		guestIP:     "192.0.2.44",
		afterGuestIP: func() {
			clock.t = ready
		},
	}
	backend := newTestBackend(t, fake)
	backend.RT.Clock = &clock

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	labels := fake.setLabels[xcpNgTestVMUUID]
	if labels["last_touched_at"] != core.LeaseLabelTime(ready) {
		t.Fatalf("last_touched_at=%q want %q labels=%#v", labels["last_touched_at"], core.LeaseLabelTime(ready), labels)
	}
	wantExpires := core.LeaseLabelTime(ready.Add(time.Hour))
	if labels["expires_at"] != wantExpires {
		t.Fatalf("expires_at=%q want %q labels=%#v", labels["expires_at"], wantExpires, labels)
	}
}

func TestAcquireCleansUpVMAndConfigDriveOnGuestIPFailure(t *testing.T) {
	fake := &fakeLifecycleClient{
		templateRef: "OpaqueRef:tpl",
		drive:       xcpNgConfigDrive{VDIRef: "OpaqueRef:vdi", VBDRef: "OpaqueRef:vbd", Name: "drive"},
		errOn:       map[string]error{"guest-ip": errors.New("guest tools missing")},
	}
	backend := newTestBackend(t, fake)
	var removedKeys []string
	removeStoredTestboxKey = func(leaseID string) { removedKeys = append(removedKeys, leaseID) }
	if _, err := backend.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "blue"}); err == nil {
		t.Fatal("expected guest IP failure")
	}
	if !reflect.DeepEqual(fake.deleted, []string{xcpNgTestVMUUID, xcpNgTestVMUUID}) {
		t.Fatalf("deleted=%v", fake.deleted)
	}
	if len(fake.deletedCD) != 2 || fake.deletedCD[0].VDIRef != "OpaqueRef:vdi" || fake.deletedCD[1].VDIRef != "OpaqueRef:vdi" {
		t.Fatalf("deleted config drives=%#v", fake.deletedCD)
	}
	if !reflect.DeepEqual(removedKeys, []string{"cbx_testlease", "cbx_testlease"}) {
		t.Fatalf("removed keys=%v", removedKeys)
	}
	if !fake.deleteBounded || !fake.deleteCDBounded {
		t.Fatalf("rollback cleanup contexts: delete=%v delete-config-drive=%v", fake.deleteBounded, fake.deleteCDBounded)
	}
}

func TestCloseClientUsesDetachedBoundedContext(t *testing.T) {
	fake := &fakeLifecycleClient{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	closeClient(ctx, fake, io.Discard)
	if !fake.closeBounded || fake.closeCanceled {
		t.Fatalf("close context bounded=%v canceled=%v", fake.closeBounded, fake.closeCanceled)
	}
}

func TestCleanupFailedLeaseIsBoundedAndDetachedFromCancellation(t *testing.T) {
	fake := &fakeLifecycleClient{}
	backend := newTestBackend(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	retained, err := backend.cleanupFailedLease(ctx, fake, xcpNgTestVMUUID, xcpNgConfigDrive{VDIRef: "OpaqueRef:vdi"})
	if err != nil {
		t.Fatal(err)
	}
	if retained {
		t.Fatal("cleanup unexpectedly retained VM")
	}
	if !fake.deleteBounded || !fake.deleteCDBounded {
		t.Fatalf("cleanup contexts: delete=%v delete-config-drive=%v", fake.deleteBounded, fake.deleteCDBounded)
	}
}

func TestAcquireRetainsKeyWhenVMRollbackFails(t *testing.T) {
	fake := &fakeLifecycleClient{
		templateRef: "OpaqueRef:tpl",
		drive:       xcpNgConfigDrive{VDIRef: "OpaqueRef:vdi", VBDRef: "OpaqueRef:vbd", Name: "drive"},
		errOn: map[string]error{
			"guest-ip": errors.New("guest tools missing"),
			"delete":   errors.New("xapi delete failed"),
		},
	}
	backend := newTestBackend(t, fake)
	var removedKeys []string
	removeStoredTestboxKey = func(leaseID string) { removedKeys = append(removedKeys, leaseID) }
	if _, err := backend.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "blue"}); err == nil || !strings.Contains(err.Error(), "xapi delete failed") {
		t.Fatalf("err=%v", err)
	}
	if len(removedKeys) != 0 {
		t.Fatalf("removed keys for retained VM=%v", removedKeys)
	}
	if got := countCalls(fake.calls, "clone"); got != 1 {
		t.Fatalf("clone calls=%d want no retry after retained VM, calls=%v", got, fake.calls)
	}
}

func TestAcquireRemovesKeyWhenPlacementFailsBeforeVMCreation(t *testing.T) {
	fake := &fakeLifecycleClient{
		templateRef: "OpaqueRef:tpl",
		srRef:       "OpaqueRef:sr",
		errOn:       map[string]error{"resolve-network": errors.New("network unavailable")},
	}
	backend := newTestBackend(t, fake)
	var removedKeys []string
	removeStoredTestboxKey = func(leaseID string) { removedKeys = append(removedKeys, leaseID) }
	if _, err := backend.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "blue"}); err == nil {
		t.Fatal("expected placement failure")
	}
	if !reflect.DeepEqual(removedKeys, []string{"cbx_testlease"}) {
		t.Fatalf("removed keys=%v", removedKeys)
	}
}

func TestAcquireSurfacesRetainedVMWhenCloneRollbackFails(t *testing.T) {
	fake := &fakeLifecycleClient{
		templateRef: "OpaqueRef:tpl",
		srRef:       "OpaqueRef:sr",
		cloneVM: xapiVM{
			Ref:    "OpaqueRef:retained-copy",
			Labels: map[string]string{"lease": "cbx_testlease"},
		},
		errOn: map[string]error{"clone": errors.New("clone rollback failed")},
	}
	backend := newTestBackend(t, fake)
	var removedKeys []string
	removeStoredTestboxKey = func(leaseID string) { removedKeys = append(removedKeys, leaseID) }
	if _, err := backend.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "blue"}); err == nil ||
		!strings.Contains(err.Error(), "OpaqueRef:retained-copy") ||
		!strings.Contains(err.Error(), "manual cleanup") {
		t.Fatalf("err=%v", err)
	}
	if len(removedKeys) != 0 {
		t.Fatalf("removed keys for retained VM=%v", removedKeys)
	}
}

func TestAcquireRetriesFreshLeaseWhenGuestIPNeverAppears(t *testing.T) {
	fake := &fakeLifecycleClient{
		templateRef: "OpaqueRef:tpl",
		srRef:       "OpaqueRef:sr",
		networkRef:  "OpaqueRef:net",
		hostRef:     "OpaqueRef:host",
		drive:       xcpNgConfigDrive{VDIRef: "OpaqueRef:vdi", VBDRef: "OpaqueRef:vbd", Name: "drive"},
	}
	backend := newTestBackend(t, fake)
	if _, err := backend.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "blue"}); err == nil || !strings.Contains(err.Error(), "timed out waiting for XCP-ng guest IPv4") {
		t.Fatalf("err=%v, want guest IPv4 timeout", err)
	}
	if got := countCalls(fake.calls, "clone"); got != 2 {
		t.Fatalf("clone calls=%d want 2 after retry, calls=%v", got, fake.calls)
	}
	if got := countCalls(fake.calls, "delete"); got != 2 {
		t.Fatalf("delete calls=%d want 2 after retry, calls=%v", got, fake.calls)
	}
}

func TestWaitForGuestIPv4ClassifiesGuestMetricsNoIPAsBootstrapTimeout(t *testing.T) {
	fake := &fakeLifecycleClient{errOn: map[string]error{"guest-ip": errors.New("no guest ipv4 address reported by XCP-ng guest metrics")}}
	backend := newTestBackend(t, fake)
	_, err := backend.waitForGuestIPv4(context.Background(), fake, "OpaqueRef:vm", 0)
	if err == nil || !core.IsBootstrapWaitError(err) {
		t.Fatalf("err=%v, want retry-classified bootstrap timeout", err)
	}
	if !strings.Contains(err.Error(), "no guest ipv4 address reported by XCP-ng guest metrics") {
		t.Fatalf("err=%v, want guest metrics context", err)
	}
}

func TestWaitForGuestIPv4FallsBackToDiscoveredGuestIP(t *testing.T) {
	fake := &fakeLifecycleClient{
		errOn:        map[string]error{"guest-ip": errors.New("no guest ipv4 address reported by XCP-ng guest metrics")},
		discoveredIP: "192.0.2.77",
	}
	backend := newTestBackend(t, fake)
	ip, err := backend.waitForGuestIPv4(context.Background(), fake, "OpaqueRef:vm", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "192.0.2.77" {
		t.Fatalf("ip=%s", ip)
	}
	if got := strings.Join(fake.calls, ","); !strings.Contains(got, "discover-guest-ip") {
		t.Fatalf("calls=%v", fake.calls)
	}
}

func TestWaitForGuestIPv4ReturnsDiscoveryConfigurationErrorImmediately(t *testing.T) {
	fake := &fakeLifecycleClient{
		errOn: map[string]error{
			"guest-ip":          errors.New("guest metrics unavailable"),
			"discover-guest-ip": guestProbeConfigError{message: "invalid guest CIDR"},
		},
	}
	backend := newTestBackend(t, fake)

	_, err := backend.waitForGuestIPv4(context.Background(), fake, "OpaqueRef:vm", time.Hour)

	if err == nil || err.Error() != "invalid guest CIDR" {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveRejectsExistingNonCrabboxVM(t *testing.T) {
	fake := &fakeLifecycleClient{getServer: map[string]Server{"OpaqueRef:user": {CloudID: "OpaqueRef:user", Name: "user-vm", Labels: map[string]string{"crabbox": "false"}}}}
	backend := newTestBackend(t, fake)
	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "OpaqueRef:user"}); err == nil || !strings.Contains(err.Error(), "not Crabbox-managed") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveStatusOnlySkipsGuestIPAndUsesLivePowerState(t *testing.T) {
	managed := crabboxServer(xcpNgTestVMUUID, "cbx_status", "ready", time.Now().Add(time.Hour))
	managed.Status = "Halted"
	managed.PublicNet.IPv4.IP = ""
	managed.PrivateNet.IPv4.IP = ""
	fake := &fakeLifecycleClient{
		getServer: map[string]Server{"cbx_status": managed},
		errOn: map[string]error{
			"guest-ip-by-id":    errors.New("guest tools unavailable"),
			"discover-guest-ip": errors.New("guest network unavailable"),
		},
	}
	backend := newTestBackend(t, fake)

	resolved, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "cbx_status", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Server.Status != "halted" || resolved.Server.Labels["state"] != "halted" || resolved.SSH.Host != "" {
		t.Fatalf("resolved=%#v", resolved)
	}
	if countCalls(fake.calls, "guest-ip-by-id") != 0 || countCalls(fake.calls, "discover-guest-ip") != 0 {
		t.Fatalf("status-only probed guest network: %v", fake.calls)
	}
}

func TestResolveStatusOnlyPreservesHealthyRunningEndpoint(t *testing.T) {
	managed := crabboxServer(xcpNgTestVMUUID, "cbx_status", "ready", time.Now().Add(time.Hour))
	managed.Status = "Running"
	managed.Labels["state"] = "active"
	managed.PublicNet.IPv4.IP = ""
	managed.PrivateNet.IPv4.IP = ""
	fake := &fakeLifecycleClient{
		getServer: map[string]Server{"cbx_status": managed},
		guestIP:   "192.0.2.55",
	}
	backend := newTestBackend(t, fake)

	resolved, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "cbx_status", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.SSH.Host != "192.0.2.55" || resolved.Server.PublicNet.IPv4.IP != "192.0.2.55" || resolved.Server.Labels["state"] != "active" {
		t.Fatalf("resolved=%#v", resolved)
	}
	if countCalls(fake.calls, "guest-ip-by-id") != 1 {
		t.Fatalf("calls=%v", fake.calls)
	}
}

func TestResolveStatusOnlyToleratesUnavailableRunningEndpoint(t *testing.T) {
	managed := crabboxServer(xcpNgTestVMUUID, "cbx_status", "active", time.Now().Add(time.Hour))
	managed.Status = "Running"
	managed.PublicNet.IPv4.IP = ""
	managed.PrivateNet.IPv4.IP = ""
	fake := &fakeLifecycleClient{
		getServer: map[string]Server{"cbx_status": managed},
		errOn: map[string]error{
			"guest-ip-by-id":    errors.New("guest tools unavailable"),
			"discover-guest-ip": errors.New("guest network unavailable"),
		},
	}
	backend := newTestBackend(t, fake)

	resolved, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "cbx_status", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.SSH.Host != "" || resolved.Server.Labels["state"] != "active" {
		t.Fatalf("resolved=%#v", resolved)
	}
}

func countCalls(calls []string, want string) int {
	var count int
	for _, call := range calls {
		if call == want {
			count++
		}
	}
	return count
}

func TestResolveByAliasFallsBackToMACDiscoveryWhenGuestMetricsFail(t *testing.T) {
	managed := crabboxServer(xcpNgTestVMUUID, "cbx_lease", "ready", time.Now().Add(time.Hour))
	managed.PublicNet.IPv4.IP = ""
	managed.PrivateNet.IPv4.IP = ""
	fake := &fakeLifecycleClient{
		servers:      []Server{managed},
		getServer:    map[string]Server{xcpNgTestVMUUID: managed},
		discoveredIP: "192.0.2.77",
		errOn:        map[string]error{"guest-ip": errors.New("guest metrics unavailable")},
	}
	backend := newTestBackend(t, fake)
	target, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "lease"})
	if err != nil {
		t.Fatal(err)
	}
	if target.SSH.Host != "192.0.2.77" || target.Server.PublicNet.IPv4.IP != "192.0.2.77" {
		t.Fatalf("target=%#v", target)
	}
	if countCalls(fake.calls, "guest-ip-by-id") != 1 || countCalls(fake.calls, "discover-guest-ip") != 1 {
		t.Fatalf("calls=%v", fake.calls)
	}
	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "lease", ReleaseOnly: true}); err != nil {
		t.Fatalf("release-only resolve err=%v", err)
	}
}

func TestTargetForServerRestoresStoredTargetLabels(t *testing.T) {
	backend := newTestBackend(t, &fakeLifecycleClient{})
	backend.Cfg.TargetOS = "macos"
	backend.Cfg.WindowsMode = "normal"
	server := crabboxServer(xcpNgTestVMUUID, "cbx_lease", "ready", time.Now().Add(time.Hour))
	server.Labels["target"] = "linux"
	server.Labels["work_root"] = "/srv/crabbox"

	target := backend.targetForServer(server)
	if target.SSH.TargetOS != "linux" || target.SSH.WindowsMode != "" {
		t.Fatalf("ssh target=%#v", target.SSH)
	}

	server.Labels["target"] = "windows"
	server.Labels["windows_mode"] = "wsl2"
	target = backend.targetForServer(server)
	if target.SSH.TargetOS != "windows" || target.SSH.WindowsMode != "wsl2" {
		t.Fatalf("windows ssh target=%#v", target.SSH)
	}
}

func TestListResolveTouchReleaseUseOnlyCrabboxMetadata(t *testing.T) {
	managed := crabboxServer(xcpNgTestVMUUID, "cbx_lease", "ready", time.Now().Add(time.Hour))
	unmanaged := Server{CloudID: "OpaqueRef:vm-2", Name: "crabbox-prefix-only", Labels: map[string]string{"provider": "xcp-ng"}}
	fake := &fakeLifecycleClient{
		servers: []Server{managed, unmanaged},
		getServer: map[string]Server{
			"cbx_lease": managed,
		},
	}
	backend := newTestBackend(t, fake)
	backend.Cfg.XCPNg.Template = ""
	backend.Cfg.XCPNg.TemplateUUID = ""
	backend.Cfg.XCPNg.SR = ""
	backend.Cfg.XCPNg.SRUUID = ""
	servers, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].CloudID != xcpNgTestVMUUID {
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
	if !reflect.DeepEqual(fake.deleted, []string{xcpNgTestVMUUID}) {
		t.Fatalf("deleted=%v", fake.deleted)
	}
}

func TestReleaseRemovesStoredKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	managed := crabboxServer(xcpNgTestVMUUID, "cbx_release", "ready", time.Now().Add(time.Hour))
	fake := &fakeLifecycleClient{getServer: map[string]Server{"cbx_release": managed}}
	backend := newTestBackend(t, fake)
	removeStoredTestboxKey = func(leaseID string) { core.RemoveStoredTestboxKey(leaseID) }
	keyPath, err := core.TestboxKeyPath("cbx_release")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{Server: managed, LeaseID: "cbx_release"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("stored key still exists or stat failed with unexpected error: %v", err)
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
	backend.Cfg.XCPNg.Template = ""
	backend.Cfg.XCPNg.TemplateUUID = ""
	backend.Cfg.XCPNg.SR = ""
	backend.Cfg.XCPNg.SRUUID = ""
	backend.RT.Clock = fixedClock{t: now}
	var removedClaims []string
	var removedKeys []string
	removeLeaseClaim = func(leaseID string) { removedClaims = append(removedClaims, leaseID) }
	removeStoredTestboxKey = func(leaseID string) { removedKeys = append(removedKeys, leaseID) }
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.deleted, []string{"OpaqueRef:expired"}) {
		t.Fatalf("deleted=%v", fake.deleted)
	}
	if !reflect.DeepEqual(removedClaims, []string{"cbx_expired"}) || !reflect.DeepEqual(removedKeys, []string{"cbx_expired"}) {
		t.Fatalf("removed claims=%v keys=%v", removedClaims, removedKeys)
	}
}

func TestRunISOE2ELinuxReadOnlyAcceptsLocalInstallerPath(t *testing.T) {
	dir := t.TempDir()
	isoPath := filepath.Join(dir, "ubuntu.iso")
	if err := os.WriteFile(isoPath, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLifecycleClient{srRef: "OpaqueRef:sr", networkRef: "OpaqueRef:net", hostRef: "OpaqueRef:host", iso: xcpNgISOMediaRef{Source: "local-file", NameLabel: isoPath}}
	oldClient := newLifecycleClient
	newLifecycleClient = func(context.Context, Config) (lifecycleClient, error) { return fake, nil }
	t.Cleanup(func() { newLifecycleClient = oldClient })
	summary, err := RunISOE2E(context.Background(), ISOE2EOptions{Config: testConfig(), Mode: "read-only", OS: "linux", ISO: isoPath, EvidenceDir: filepath.Join(dir, "evidence")})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Classification != "read_only_passed" || summary.Phase != "read_only_validation" {
		t.Fatalf("summary=%#v", summary)
	}
	if summary.Details["installer_source"] != "local-file" {
		t.Fatalf("summary details=%#v", summary.Details)
	}
}

func TestRunISOE2EMutateRequiresNetworkBeforeCreatingVM(t *testing.T) {
	dir := t.TempDir()
	isoPath := filepath.Join(dir, "ubuntu.iso")
	if err := os.WriteFile(isoPath, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLifecycleClient{
		srRef:   "OpaqueRef:sr",
		hostRef: "OpaqueRef:host",
	}
	oldClient := newLifecycleClient
	newLifecycleClient = func(context.Context, Config) (lifecycleClient, error) { return fake, nil }
	t.Cleanup(func() { newLifecycleClient = oldClient })

	cfg := testConfig()
	cfg.TargetOS = "windows"
	cfg.WindowsMode = "normal"
	cfg.WorkRoot = `C:\crabbox`
	cfg.XCPNg.WorkRoot = `C:\crabbox`
	summary, err := RunISOE2E(context.Background(), ISOE2EOptions{Config: cfg, Mode: "mutate", OS: "linux", ISO: isoPath, EvidenceDir: filepath.Join(dir, "evidence"), MutateGate: true})
	if err == nil || !strings.Contains(err.Error(), "requires xcpNg.network") {
		t.Fatalf("err=%v", err)
	}
	if summary.Classification != "environment_blocked" || summary.Phase != "placement" {
		t.Fatalf("summary=%#v", summary)
	}
	if fake.mutated {
		t.Fatalf("network validation mutated provider state: calls=%v", fake.calls)
	}
}

func TestRunISOE2EReportsLogoutFailureInSummary(t *testing.T) {
	dir := t.TempDir()
	isoPath := filepath.Join(dir, "ubuntu.iso")
	if err := os.WriteFile(isoPath, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLifecycleClient{
		srRef:      "OpaqueRef:sr",
		networkRef: "OpaqueRef:net",
		hostRef:    "OpaqueRef:host",
		iso:        xcpNgISOMediaRef{Source: "local-file", NameLabel: isoPath},
		errOn:      map[string]error{"close": errors.New("logout failed")},
	}
	oldClient := newLifecycleClient
	newLifecycleClient = func(context.Context, Config) (lifecycleClient, error) { return fake, nil }
	t.Cleanup(func() { newLifecycleClient = oldClient })

	summary, err := RunISOE2E(context.Background(), ISOE2EOptions{Config: testConfig(), Mode: "read-only", OS: "linux", ISO: isoPath, EvidenceDir: filepath.Join(dir, "evidence")})
	if err == nil || !strings.Contains(err.Error(), "logout failed") {
		t.Fatalf("err=%v", err)
	}
	if summary.Classification != "resource_cleanup_failed" || summary.Cleanup != "failed" || summary.Phase != "close" || !strings.Contains(summary.Reason, "logout failed") {
		t.Fatalf("summary=%#v", summary)
	}
}

func TestRunISOE2EJoinsLogoutFailureWithPrimaryFailure(t *testing.T) {
	dir := t.TempDir()
	isoPath := filepath.Join(dir, "ubuntu.iso")
	if err := os.WriteFile(isoPath, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLifecycleClient{
		srRef:      "OpaqueRef:sr",
		networkRef: "OpaqueRef:net",
		hostRef:    "OpaqueRef:host",
		errOn: map[string]error{
			"resolve-iso": errors.New("ISO unavailable"),
			"close":       errors.New("logout failed"),
		},
	}
	oldClient := newLifecycleClient
	newLifecycleClient = func(context.Context, Config) (lifecycleClient, error) { return fake, nil }
	t.Cleanup(func() { newLifecycleClient = oldClient })

	summary, err := RunISOE2E(context.Background(), ISOE2EOptions{Config: testConfig(), Mode: "read-only", OS: "linux", ISO: isoPath, EvidenceDir: filepath.Join(dir, "evidence")})
	if err == nil || !strings.Contains(err.Error(), "ISO unavailable") || !strings.Contains(err.Error(), "logout failed") {
		t.Fatalf("err=%v", err)
	}
	if summary.Classification != "environment_blocked" || summary.Cleanup != "failed" || summary.Phase != "installer_iso" ||
		!strings.Contains(summary.Reason, "ISO unavailable") || !strings.Contains(summary.Reason, "logout failed") {
		t.Fatalf("summary=%#v", summary)
	}
}

func TestRunISOE2EWindowsReadOnlyBlocksARMInstaller(t *testing.T) {
	dir := t.TempDir()
	isoDir := filepath.Join(dir, "ISOs-ARM")
	if err := os.MkdirAll(isoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	isoPath := filepath.Join(isoDir, "Win11_Arm64.iso")
	if err := os.WriteFile(isoPath, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLifecycleClient{srRef: "OpaqueRef:sr", networkRef: "OpaqueRef:net", hostRef: "OpaqueRef:host", iso: xcpNgISOMediaRef{Source: "local-file", NameLabel: isoPath}}
	oldClient := newLifecycleClient
	newLifecycleClient = func(context.Context, Config) (lifecycleClient, error) { return fake, nil }
	t.Cleanup(func() { newLifecycleClient = oldClient })
	summary, err := RunISOE2E(context.Background(), ISOE2EOptions{Config: testConfig(), Mode: "read-only", OS: "windows", ISO: isoPath, EvidenceDir: filepath.Join(dir, "evidence")})
	if err == nil {
		t.Fatal("expected arm installer requirements blocker")
	}
	if summary.Classification != "windows_requirements_blocked" || summary.Phase != "installer_iso" {
		t.Fatalf("summary=%#v err=%v", summary, err)
	}
}

func TestWindowsInstallerVTPMDetection(t *testing.T) {
	for _, value := range []string{"Win11_25H2_English_x64.iso", "Windows-11-Pro.iso", "w11.iso"} {
		if !windowsInstallerRequiresVTPM(value) {
			t.Fatalf("Windows 11 installer not detected: %s", value)
		}
	}
	for _, value := range []string{"Win10_22H2.iso", "Windows_Server_2025.iso", "custom-x64.iso"} {
		if windowsInstallerRequiresVTPM(value) {
			t.Fatalf("non-Windows 11 installer requires vTPM: %s", value)
		}
	}
}

func TestRunISOE2EWindowsUsesResolvedInstallerNameForVTPM(t *testing.T) {
	dir := t.TempDir()
	answerPath := filepath.Join(dir, "provided-answer.iso")
	if err := os.WriteFile(answerPath, []byte("answer"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLifecycleClient{
		srRef:      "OpaqueRef:sr",
		networkRef: "OpaqueRef:net",
		hostRef:    "OpaqueRef:host",
		iso: xcpNgISOMediaRef{
			VDIRef:    "OpaqueRef:installer",
			NameLabel: "Win11_25H2_English_x64.iso",
			Source:    "sr-vdi",
		},
		errOn: map[string]error{"create-fresh-vm": errors.New("stop after request capture")},
	}
	oldClient := newLifecycleClient
	newLifecycleClient = func(context.Context, Config) (lifecycleClient, error) { return fake, nil }
	t.Cleanup(func() { newLifecycleClient = oldClient })

	_, err := RunISOE2E(context.Background(), ISOE2EOptions{
		Config:      testConfig(),
		Mode:        "mutate",
		OS:          "windows",
		ISO:         xcpNgTestVMUUID,
		AnswerISO:   answerPath,
		EvidenceDir: filepath.Join(dir, "evidence"),
		MutateGate:  true,
	})
	if err == nil || !strings.Contains(err.Error(), "stop after request capture") {
		t.Fatalf("err=%v", err)
	}
	if !fake.freshReq.VTPM {
		t.Fatalf("fresh VM request did not require vTPM: %#v", fake.freshReq)
	}
}

func TestRunISOE2EWindowsMutateGeneratesAndAttachesBootstrapBeforeStart(t *testing.T) {
	dir := t.TempDir()
	isoPath := filepath.Join(dir, "Win11_25H2_English_x64_v2.iso")
	if err := os.WriteFile(isoPath, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLifecycleClient{
		srRef:        "OpaqueRef:sr",
		networkRef:   "OpaqueRef:net",
		hostRef:      "OpaqueRef:host",
		iso:          xcpNgISOMediaRef{Source: "local-file", NameLabel: isoPath},
		guestIP:      "192.0.2.60",
		drive:        xcpNgConfigDrive{VDIRef: "OpaqueRef:answer-vdi", VBDRef: "OpaqueRef:answer-vbd", Name: "answer.iso"},
		importedISO:  xcpNgConfigDrive{VDIRef: "OpaqueRef:imported-vdi", Name: "installer.iso", DestroyVDI: true},
		attachedDisk: xcpNgConfigDrive{VDIRef: "OpaqueRef:disk-vdi", VBDRef: "OpaqueRef:disk-vbd", Name: "install-disk", DestroyVDI: true},
	}
	oldClient := newLifecycleClient
	oldWait := isoE2EWaitForSSHReady
	oldRunSSH := isoE2ERunSSHQuiet
	oldEnsure := isoE2EEnsureTestboxKey
	oldWrite := isoE2EWriteWindowsAnswerISO
	oldPassword := isoE2EGenerateWindowsPassword
	var sshTimeout time.Duration
	var sshContextDeadline time.Time
	newLifecycleClient = func(context.Context, Config) (lifecycleClient, error) { return fake, nil }
	isoE2EWaitForSSHReady = func(ctx context.Context, _ *core.SSHTarget, _ string, timeout time.Duration) error {
		sshTimeout = timeout
		sshContextDeadline, _ = ctx.Deadline()
		return nil
	}
	isoE2ERunSSHQuiet = func(context.Context, core.SSHTarget, string) error { return nil }
	isoE2EEnsureTestboxKey = func(Config, string) (string, string, error) {
		return filepath.Join(dir, "id_ed25519"), "ssh-ed25519 AAAATEST crabbox", nil
	}
	isoE2EWriteWindowsAnswerISO = func(_ context.Context, _ string, payload xcpNgWindowsAutounattendPayload) (string, error) {
		if payload.Username != "crabbox" || payload.AnswerXML == "" || payload.BootstrapPowerShell == "" {
			t.Fatalf("payload=%#v", payload)
		}
		answerPath := filepath.Join(dir, "answer.iso")
		if err := os.WriteFile(answerPath, []byte("answer"), 0o600); err != nil {
			return "", err
		}
		return answerPath, nil
	}
	isoE2EGenerateWindowsPassword = func() (string, error) { return "TempPass1!", nil }
	t.Cleanup(func() {
		newLifecycleClient = oldClient
		isoE2EWaitForSSHReady = oldWait
		isoE2ERunSSHQuiet = oldRunSSH
		isoE2EEnsureTestboxKey = oldEnsure
		isoE2EWriteWindowsAnswerISO = oldWrite
		isoE2EGenerateWindowsPassword = oldPassword
	})
	summary, err := RunISOE2E(context.Background(), ISOE2EOptions{Config: testConfig(), Mode: "mutate", OS: "windows", ISO: isoPath, EvidenceDir: filepath.Join(dir, "evidence"), MutateGate: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Classification != "windows_install_passed" || summary.Phase != "windows_command_ok" {
		t.Fatalf("summary=%#v", summary)
	}
	if summary.Details["answer_iso_source"] != "generated-local-file" || summary.Details["windows_bootstrap"] != "openssh-key" {
		t.Fatalf("summary=%#v", summary)
	}
	if fake.diskReq.SizeBytes != isoE2EWindowsDiskBytes {
		t.Fatalf("Windows install disk size=%d", fake.diskReq.SizeBytes)
	}
	if !strings.Contains(strings.Join(fake.calls, ","), "delete-vtpm") {
		t.Fatalf("Windows vTPM was not cleaned up: calls=%v", fake.calls)
	}
	if sshTimeout != isoE2EDefaultTimeout || time.Until(sshContextDeadline) <= isoE2EWindowsInstallTimeout {
		t.Fatalf("ssh proof timeout=%s deadline=%s", sshTimeout, sshContextDeadline)
	}
	if _, err := os.Stat(summary.AnswerISO); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generated answer ISO was not removed: err=%v", err)
	}
	calls := strings.Join(fake.calls, ",")
	if strings.Index(calls, "attach-iso") < 0 || strings.Index(calls, "start") < 0 || strings.LastIndex(calls, "attach-iso") > strings.Index(calls, "start") {
		t.Fatalf("answer media must attach before start, calls=%v", fake.calls)
	}
}

func TestPrepareWindowsAnswerMediaRetainsGeneratedISOOnlyWhenRequested(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRABBOX_XCP_NG_ISO_E2E_KEEP_WINDOWS_ANSWER", "1")
	oldEnsure := isoE2EEnsureTestboxKey
	oldWrite := isoE2EWriteWindowsAnswerISO
	oldPassword := isoE2EGenerateWindowsPassword
	isoE2EEnsureTestboxKey = func(Config, string) (string, string, error) {
		return filepath.Join(dir, "id_ed25519"), "ssh-ed25519 AAAATEST crabbox", nil
	}
	isoE2EWriteWindowsAnswerISO = func(context.Context, string, xcpNgWindowsAutounattendPayload) (string, error) {
		answerPath := filepath.Join(dir, "answer.iso")
		if err := os.WriteFile(answerPath, []byte("answer"), 0o600); err != nil {
			return "", err
		}
		return answerPath, nil
	}
	isoE2EGenerateWindowsPassword = func() (string, error) { return "TempPass1!", nil }
	t.Cleanup(func() {
		isoE2EEnsureTestboxKey = oldEnsure
		isoE2EWriteWindowsAnswerISO = oldWrite
		isoE2EGenerateWindowsPassword = oldPassword
	})
	runtime := isoE2ERuntime{
		leaseID:   "cbx_test",
		keepLocal: map[string]struct{}{},
	}
	summary := ISOE2ESummary{Evidence: map[string]string{}, Details: map[string]string{}}
	if err := runtime.prepareWindowsAnswerMedia(context.Background(), ISOE2EOptions{Config: testConfig(), EvidenceDir: dir}, &summary); err != nil {
		t.Fatal(err)
	}
	if err := runtime.cleanupLocalArtifacts(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(summary.AnswerISO); err != nil {
		t.Fatalf("retained answer ISO missing: %v", err)
	}
	if summary.Evidence["windows_answer_iso"] != summary.AnswerISO {
		t.Fatalf("summary=%#v", summary)
	}
}

func TestFinalizeLocalArtifactsSurfacesRemovalFailure(t *testing.T) {
	oldRemove := isoE2ERemoveLocalArtifact
	isoE2ERemoveLocalArtifact = func(string) error { return errors.New("permission denied") }
	t.Cleanup(func() { isoE2ERemoveLocalArtifact = oldRemove })
	runtime := &isoE2ERuntime{
		cleanupLocal: []string{"/tmp/windows-answer.iso"},
		keepLocal:    map[string]struct{}{},
	}
	summary := ISOE2ESummary{Classification: "windows_install_passed", Cleanup: "cleaned"}
	var runErr error
	finalizeLocalArtifacts(runtime, &summary, &runErr)
	if runErr == nil || !strings.Contains(runErr.Error(), "permission denied") {
		t.Fatalf("err=%v", runErr)
	}
	if summary.Classification != "resource_cleanup_failed" || summary.Cleanup != "failed" {
		t.Fatalf("summary=%#v", summary)
	}
}

func TestFinalizeLocalArtifactsPreservesPrimaryFailure(t *testing.T) {
	oldRemove := isoE2ERemoveLocalArtifact
	isoE2ERemoveLocalArtifact = func(string) error { return errors.New("permission denied") }
	t.Cleanup(func() { isoE2ERemoveLocalArtifact = oldRemove })
	runtime := &isoE2ERuntime{
		cleanupLocal: []string{"/tmp/windows-answer.iso"},
		keepLocal:    map[string]struct{}{},
	}
	primaryErr := errors.New("guest readiness timed out")
	runErr := primaryErr
	summary := ISOE2ESummary{Classification: "environment_blocked", Cleanup: "failed", Reason: primaryErr.Error()}
	finalizeLocalArtifacts(runtime, &summary, &runErr)
	if !errors.Is(runErr, primaryErr) {
		t.Fatalf("err=%v", runErr)
	}
	if summary.Classification != "environment_blocked" || !strings.Contains(summary.Reason, "permission denied") {
		t.Fatalf("summary=%#v", summary)
	}
}

func TestWriteWindowsAnswerISOEnforcesPrivatePermissions(t *testing.T) {
	dir := t.TempDir()
	evidenceDir := filepath.Join(dir, "evidence")
	if err := os.Mkdir(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	xorriso := filepath.Join(binDir, "xorriso")
	script := `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then
    shift
    : > "$1"
    chmod 0644 "$1"
    exit 0
  fi
  shift
done
exit 1
`
	if err := os.WriteFile(xorriso, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	path, err := writeWindowsAnswerISO(context.Background(), evidenceDir, xcpNgWindowsAutounattendPayload{
		AnswerXML:           "<unattend/>",
		BootstrapPowerShell: "Write-Output ready",
	})
	if err != nil {
		t.Fatal(err)
	}
	for target, want := range map[string]os.FileMode{evidenceDir: 0o755, filepath.Dir(path): 0o700, path: 0o600} {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode=%#o want=%#o", target, got, want)
		}
	}
}

func TestIsCrabboxLeaseRequiresXCPNgProviderLabel(t *testing.T) {
	server := crabboxServer(xcpNgTestVMUUID, "cbx_lease", "ready", time.Now().Add(time.Hour))
	delete(server.Labels, "provider")
	if isCrabboxLease(server) {
		t.Fatalf("server without provider label considered managed: %#v", server.Labels)
	}
	server.Labels["provider"] = "other"
	if isCrabboxLease(server) {
		t.Fatalf("server with wrong provider label considered managed: %#v", server.Labels)
	}
	server.Labels["provider"] = "xcp-ng"
	if !isCrabboxLease(server) {
		t.Fatalf("server with xcp-ng provider label not considered managed: %#v", server.Labels)
	}
}

func TestRunISOE2EWindowsMutateFallsBackToSourceUncoveredWithProvidedAnswerISO(t *testing.T) {
	dir := t.TempDir()
	isoPath := filepath.Join(dir, "Win11_25H2_English_x64_v2.iso")
	answerPath := filepath.Join(dir, "provided-answer.iso")
	for _, file := range []string{isoPath, answerPath} {
		if err := os.WriteFile(file, []byte("iso"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeLifecycleClient{
		srRef:        "OpaqueRef:sr",
		networkRef:   "OpaqueRef:net",
		hostRef:      "OpaqueRef:host",
		iso:          xcpNgISOMediaRef{Source: "local-file", NameLabel: isoPath},
		guestIP:      "192.0.2.61",
		importedISO:  xcpNgConfigDrive{VDIRef: "OpaqueRef:imported-vdi", Name: "installer.iso", DestroyVDI: true},
		attachedDisk: xcpNgConfigDrive{VDIRef: "OpaqueRef:disk-vdi", VBDRef: "OpaqueRef:disk-vbd", Name: "install-disk", DestroyVDI: true},
	}
	oldClient := newLifecycleClient
	newLifecycleClient = func(context.Context, Config) (lifecycleClient, error) { return fake, nil }
	t.Cleanup(func() { newLifecycleClient = oldClient })
	summary, err := RunISOE2E(context.Background(), ISOE2EOptions{Config: testConfig(), Mode: "mutate", OS: "windows", ISO: isoPath, AnswerISO: answerPath, EvidenceDir: filepath.Join(dir, "evidence"), MutateGate: true})
	if err == nil {
		t.Fatal("expected source uncovered classification")
	}
	if summary.Classification != "source_uncovered" || summary.Phase != "windows_first_boot" || summary.Cleanup != "cleaned" {
		t.Fatalf("summary=%#v err=%v", summary, err)
	}
	if !strings.Contains(summary.Reason, "remote command proof remains uncovered") {
		t.Fatalf("summary=%#v", summary)
	}
	if fake.freshReq.Labels["target"] != "windows" || fake.freshReq.Labels["windows_mode"] != "normal" || fake.freshReq.Labels["work_root"] != `C:\crabbox` {
		t.Fatalf("fresh VM labels=%#v", fake.freshReq.Labels)
	}
}

func TestRunISOE2EWindowsMutateClassifiesGuestMetricsBlocker(t *testing.T) {
	dir := t.TempDir()
	isoPath := filepath.Join(dir, "Win11_25H2_English_x64_v2.iso")
	answerPath := filepath.Join(dir, "provided-answer.iso")
	for _, file := range []string{isoPath, answerPath} {
		if err := os.WriteFile(file, []byte("iso"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeLifecycleClient{
		srRef:        "OpaqueRef:sr",
		networkRef:   "OpaqueRef:net",
		hostRef:      "OpaqueRef:host",
		iso:          xcpNgISOMediaRef{Source: "local-file", NameLabel: isoPath},
		importedISO:  xcpNgConfigDrive{VDIRef: "OpaqueRef:imported-vdi", Name: "installer.iso", DestroyVDI: true},
		attachedDisk: xcpNgConfigDrive{VDIRef: "OpaqueRef:disk-vdi", VBDRef: "OpaqueRef:disk-vbd", Name: "install-disk", DestroyVDI: true},
		errOn:        map[string]error{"guest-ip": errors.New("no guest ipv4 address reported by XCP-ng guest metrics")},
	}
	oldClient := newLifecycleClient
	newLifecycleClient = func(context.Context, Config) (lifecycleClient, error) { return fake, nil }
	t.Cleanup(func() { newLifecycleClient = oldClient })
	summary, err := RunISOE2E(context.Background(), ISOE2EOptions{Config: testConfig(), Mode: "mutate", OS: "windows", ISO: isoPath, AnswerISO: answerPath, EvidenceDir: filepath.Join(dir, "evidence"), Timeout: 25 * time.Second, MutateGate: true})
	if err == nil {
		t.Fatal("expected guest metrics blocker")
	}
	if summary.Classification != "environment_blocked" || summary.Phase != "windows_install_complete" {
		t.Fatalf("summary=%#v err=%v", summary, err)
	}
}

func TestRunISOE2ELinuxMutatePassesWithImportedMediaAndSSHProof(t *testing.T) {
	dir := t.TempDir()
	isoPath := filepath.Join(dir, "ubuntu.iso")
	if err := os.WriteFile(isoPath, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLifecycleClient{
		srRef:        "OpaqueRef:sr",
		networkRef:   "OpaqueRef:net",
		hostRef:      "OpaqueRef:host",
		iso:          xcpNgISOMediaRef{Source: "local-file", NameLabel: isoPath},
		guestIP:      "192.0.2.50",
		drive:        xcpNgConfigDrive{VDIRef: "OpaqueRef:seed-vdi", VBDRef: "OpaqueRef:seed-vbd", Name: "linux-seed", DestroyVDI: true},
		importedISO:  xcpNgConfigDrive{VDIRef: "OpaqueRef:imported-vdi", Name: "installer.iso", DestroyVDI: true},
		attachedDisk: xcpNgConfigDrive{VDIRef: "OpaqueRef:disk-vdi", VBDRef: "OpaqueRef:disk-vbd", Name: "install-disk", DestroyVDI: true},
	}
	oldClient := newLifecycleClient
	oldWait := isoE2EWaitForSSHReady
	oldRunSSH := isoE2ERunSSHQuiet
	oldEnsure := isoE2EEnsureTestboxKey
	oldNow := isoE2ECurrentTime
	oldRemaster := isoE2ERemasterUbuntuISO
	oldSeed := isoE2EWriteLinuxSeedISO
	newLifecycleClient = func(context.Context, Config) (lifecycleClient, error) { return fake, nil }
	isoE2EWaitForSSHReady = func(context.Context, *core.SSHTarget, string, time.Duration) error { return nil }
	isoE2ERunSSHQuiet = func(context.Context, core.SSHTarget, string) error { return nil }
	isoE2EEnsureTestboxKey = func(Config, string) (string, string, error) {
		return filepath.Join(dir, "id_ed25519"), "ssh-ed25519 AAAATEST crabbox", nil
	}
	isoE2ECurrentTime = func() time.Time { return time.Unix(1700000000, 0).UTC() }
	isoE2ERemasterUbuntuISO = func(context.Context, string, string) (string, error) { return isoPath, nil }
	isoE2EWriteLinuxSeedISO = func(context.Context, string, xcpNgLinuxAutoinstallPayload) (string, error) {
		seedPath := filepath.Join(dir, "seed.iso")
		if err := os.WriteFile(seedPath, []byte("seed"), 0o600); err != nil {
			return "", err
		}
		return seedPath, nil
	}
	t.Cleanup(func() {
		newLifecycleClient = oldClient
		isoE2EWaitForSSHReady = oldWait
		isoE2ERunSSHQuiet = oldRunSSH
		isoE2EEnsureTestboxKey = oldEnsure
		isoE2ECurrentTime = oldNow
		isoE2ERemasterUbuntuISO = oldRemaster
		isoE2EWriteLinuxSeedISO = oldSeed
	})
	summary, err := RunISOE2E(context.Background(), ISOE2EOptions{Config: testConfig(), Mode: "mutate", OS: "linux", ISO: isoPath, EvidenceDir: filepath.Join(dir, "evidence"), MutateGate: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Classification != "linux_install_passed" || summary.Phase != "linux_ssh_ok" || summary.Cleanup != "cleaned" {
		t.Fatalf("summary=%#v", summary)
	}
	for _, want := range []string{"create-fresh-vm", "attach-disk", "import-iso", "attach-iso", "attach-config-drive", "start", "set-boot-order", "guest-ip", "delete", "delete-config-drive"} {
		if !strings.Contains(strings.Join(fake.calls, ","), want) {
			t.Fatalf("calls=%v missing %s", fake.calls, want)
		}
	}
	if summary.Details["first_boot_ip"] != "192.0.2.50" {
		t.Fatalf("summary=%#v", summary)
	}
	if summary.Details["answer_iso_source"] != "generated-config-drive" {
		t.Fatalf("summary=%#v", summary)
	}
	if fake.freshReq.Labels["target"] != "linux" || fake.freshReq.Labels["windows_mode"] != "" || fake.freshReq.Labels["work_root"] != "/work/crabbox" {
		t.Fatalf("fresh VM labels=%#v", fake.freshReq.Labels)
	}
}

func TestRunISOE2ELinuxMutateClassifiesGuestMetricsBlocker(t *testing.T) {
	dir := t.TempDir()
	isoPath := filepath.Join(dir, "ubuntu.iso")
	if err := os.WriteFile(isoPath, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLifecycleClient{
		srRef:        "OpaqueRef:sr",
		networkRef:   "OpaqueRef:net",
		hostRef:      "OpaqueRef:host",
		iso:          xcpNgISOMediaRef{Source: "local-file", NameLabel: isoPath},
		drive:        xcpNgConfigDrive{VDIRef: "OpaqueRef:seed-vdi", VBDRef: "OpaqueRef:seed-vbd", Name: "linux-seed", DestroyVDI: true},
		importedISO:  xcpNgConfigDrive{VDIRef: "OpaqueRef:imported-vdi", Name: "installer.iso", DestroyVDI: true},
		attachedDisk: xcpNgConfigDrive{VDIRef: "OpaqueRef:disk-vdi", VBDRef: "OpaqueRef:disk-vbd", Name: "install-disk", DestroyVDI: true},
		errOn:        map[string]error{"guest-ip": errors.New("no guest ipv4 address reported by XCP-ng guest metrics")},
	}
	oldClient := newLifecycleClient
	oldEnsure := isoE2EEnsureTestboxKey
	oldRemaster := isoE2ERemasterUbuntuISO
	oldSeed := isoE2EWriteLinuxSeedISO
	newLifecycleClient = func(context.Context, Config) (lifecycleClient, error) { return fake, nil }
	isoE2EEnsureTestboxKey = func(Config, string) (string, string, error) {
		return filepath.Join(dir, "id_ed25519"), "ssh-ed25519 AAAATEST crabbox", nil
	}
	isoE2ERemasterUbuntuISO = func(context.Context, string, string) (string, error) { return isoPath, nil }
	isoE2EWriteLinuxSeedISO = func(context.Context, string, xcpNgLinuxAutoinstallPayload) (string, error) {
		seedPath := filepath.Join(dir, "seed.iso")
		if err := os.WriteFile(seedPath, []byte("seed"), 0o600); err != nil {
			return "", err
		}
		return seedPath, nil
	}
	t.Cleanup(func() {
		newLifecycleClient = oldClient
		isoE2EEnsureTestboxKey = oldEnsure
		isoE2ERemasterUbuntuISO = oldRemaster
		isoE2EWriteLinuxSeedISO = oldSeed
	})
	summary, err := RunISOE2E(context.Background(), ISOE2EOptions{Config: testConfig(), Mode: "mutate", OS: "linux", ISO: isoPath, EvidenceDir: filepath.Join(dir, "evidence"), Timeout: 25 * time.Second, MutateGate: true})
	if err == nil {
		t.Fatal("expected guest metrics blocker")
	}
	if summary.Classification != "environment_blocked" || summary.Phase != "linux_install_complete" {
		t.Fatalf("summary=%#v err=%v", summary, err)
	}
	if !strings.Contains(summary.Reason, "guest IPv4") && !strings.Contains(summary.Reason, "guest metrics") && !strings.Contains(summary.Reason, "timed out waiting") {
		t.Fatalf("summary=%#v", summary)
	}
}

func TestRunISOE2ELinuxMutateUsesGuestMetricsForInstallCompletion(t *testing.T) {
	dir := t.TempDir()
	isoPath := filepath.Join(dir, "ubuntu.iso")
	if err := os.WriteFile(isoPath, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLifecycleClient{
		srRef:        "OpaqueRef:sr",
		networkRef:   "OpaqueRef:net",
		hostRef:      "OpaqueRef:host",
		iso:          xcpNgISOMediaRef{Source: "local-file", NameLabel: isoPath},
		drive:        xcpNgConfigDrive{VDIRef: "OpaqueRef:seed-vdi", VBDRef: "OpaqueRef:seed-vbd", Name: "linux-seed", DestroyVDI: true},
		importedISO:  xcpNgConfigDrive{VDIRef: "OpaqueRef:imported-vdi", Name: "installer.iso", DestroyVDI: true},
		attachedDisk: xcpNgConfigDrive{VDIRef: "OpaqueRef:disk-vdi", VBDRef: "OpaqueRef:disk-vbd", Name: "install-disk", DestroyVDI: true},
		guestIP:      "192.0.2.88",
		discoveredIP: "198.51.100.88",
	}
	oldClient := newLifecycleClient
	oldWait := isoE2EWaitForSSHReady
	oldRunSSH := isoE2ERunSSHQuiet
	oldEnsure := isoE2EEnsureTestboxKey
	oldRemaster := isoE2ERemasterUbuntuISO
	oldSeed := isoE2EWriteLinuxSeedISO
	newLifecycleClient = func(context.Context, Config) (lifecycleClient, error) { return fake, nil }
	isoE2EWaitForSSHReady = func(context.Context, *core.SSHTarget, string, time.Duration) error { return nil }
	isoE2ERunSSHQuiet = func(context.Context, core.SSHTarget, string) error { return nil }
	isoE2EEnsureTestboxKey = func(Config, string) (string, string, error) {
		return filepath.Join(dir, "id_ed25519"), "ssh-ed25519 AAAATEST crabbox", nil
	}
	isoE2ERemasterUbuntuISO = func(context.Context, string, string) (string, error) { return isoPath, nil }
	isoE2EWriteLinuxSeedISO = func(context.Context, string, xcpNgLinuxAutoinstallPayload) (string, error) {
		seedPath := filepath.Join(dir, "seed.iso")
		if err := os.WriteFile(seedPath, []byte("seed"), 0o600); err != nil {
			return "", err
		}
		return seedPath, nil
	}
	t.Cleanup(func() {
		newLifecycleClient = oldClient
		isoE2EWaitForSSHReady = oldWait
		isoE2ERunSSHQuiet = oldRunSSH
		isoE2EEnsureTestboxKey = oldEnsure
		isoE2ERemasterUbuntuISO = oldRemaster
		isoE2EWriteLinuxSeedISO = oldSeed
	})
	summary, err := RunISOE2E(context.Background(), ISOE2EOptions{Config: testConfig(), Mode: "mutate", OS: "linux", ISO: isoPath, EvidenceDir: filepath.Join(dir, "evidence"), MutateGate: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Details["first_boot_ip"] != "192.0.2.88" {
		t.Fatalf("summary=%#v", summary)
	}
	if got := strings.Join(fake.calls, ","); strings.Contains(got, "discover-guest-ip") {
		t.Fatalf("installer completion used ARP discovery: calls=%v", fake.calls)
	}
}

func TestISOE2EGuestDiscoveryRequiresExplicitInstallPathOptIn(t *testing.T) {
	fake := &fakeLifecycleClient{
		errOn:        map[string]error{"guest-ip": errors.New("guest metrics unavailable")},
		discoveredIP: "192.0.2.88",
	}
	runtime := isoE2ERuntime{client: fake, vm: xcpNgFreshVMResult{VM: xapiVM{Ref: "OpaqueRef:vm"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runtime.waitForGuestIPv4(ctx, "Linux", false); err == nil {
		t.Fatal("expected metrics-only wait to fail")
	}
	if got := strings.Join(fake.calls, ","); strings.Contains(got, "discover-guest-ip") {
		t.Fatalf("metrics-only wait used discovery: calls=%v", fake.calls)
	}

	fake.calls = nil
	ip, err := runtime.waitForGuestIPv4(context.Background(), "Windows", true)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "192.0.2.88" || !strings.Contains(strings.Join(fake.calls, ","), "discover-guest-ip") {
		t.Fatalf("ip=%q calls=%v", ip, fake.calls)
	}

	fake.calls = nil
	fake.errOn["discover-guest-ip"] = guestProbeConfigError{message: "invalid guest CIDR"}
	if _, err := runtime.waitForGuestIPv4(context.Background(), "Windows", true); err == nil || err.Error() != "invalid guest CIDR" {
		t.Fatalf("err=%v", err)
	}
}

func TestRunISOE2ELinuxMutateCleansImportedInstallerWhenAttachFails(t *testing.T) {
	dir := t.TempDir()
	isoPath := filepath.Join(dir, "ubuntu.iso")
	if err := os.WriteFile(isoPath, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLifecycleClient{
		srRef:       "OpaqueRef:sr",
		networkRef:  "OpaqueRef:net",
		hostRef:     "OpaqueRef:host",
		iso:         xcpNgISOMediaRef{Source: "local-file", NameLabel: isoPath},
		importedISO: xcpNgConfigDrive{VDIRef: "OpaqueRef:imported-vdi", Name: "installer.iso", DestroyVDI: true},
		errOn:       map[string]error{"attach-iso": errors.New("attach failed")},
	}
	oldClient := newLifecycleClient
	oldEnsure := isoE2EEnsureTestboxKey
	oldRemaster := isoE2ERemasterUbuntuISO
	oldSeed := isoE2EWriteLinuxSeedISO
	newLifecycleClient = func(context.Context, Config) (lifecycleClient, error) { return fake, nil }
	isoE2EEnsureTestboxKey = func(Config, string) (string, string, error) {
		return filepath.Join(dir, "id_ed25519"), "ssh-ed25519 AAAATEST crabbox", nil
	}
	isoE2ERemasterUbuntuISO = func(context.Context, string, string) (string, error) { return isoPath, nil }
	isoE2EWriteLinuxSeedISO = func(context.Context, string, xcpNgLinuxAutoinstallPayload) (string, error) {
		seedPath := filepath.Join(dir, "seed.iso")
		if err := os.WriteFile(seedPath, []byte("seed"), 0o600); err != nil {
			return "", err
		}
		return seedPath, nil
	}
	t.Cleanup(func() {
		newLifecycleClient = oldClient
		isoE2EEnsureTestboxKey = oldEnsure
		isoE2ERemasterUbuntuISO = oldRemaster
		isoE2EWriteLinuxSeedISO = oldSeed
	})
	_, err := RunISOE2E(context.Background(), ISOE2EOptions{Config: testConfig(), Mode: "mutate", OS: "linux", ISO: isoPath, EvidenceDir: filepath.Join(dir, "evidence"), MutateGate: true})
	if err == nil || !strings.Contains(err.Error(), "attach failed") {
		t.Fatalf("err=%v", err)
	}
	if len(fake.deletedCD) == 0 || fake.deletedCD[0].VDIRef != "OpaqueRef:imported-vdi" {
		t.Fatalf("deleted config drives=%#v", fake.deletedCD)
	}
}

func TestISOE2ECleanupUsesBoundedContext(t *testing.T) {
	fake := &fakeLifecycleClient{}
	runtime := isoE2ERuntime{
		client: fake,
		vm:     xcpNgFreshVMResult{VM: xapiVM{Ref: "OpaqueRef:vm"}},
		installerDrive: xcpNgConfigDrive{
			VDIRef:     "OpaqueRef:installer",
			DestroyVDI: true,
		},
	}
	summary := ISOE2ESummary{
		Classification: "linux_install_passed",
		Details:        map[string]string{},
	}
	var runErr error

	runtime.cleanup(context.Background(), &summary, &runErr)

	if runErr != nil {
		t.Fatal(runErr)
	}
	if summary.Cleanup != "cleaned" {
		t.Fatalf("cleanup=%q", summary.Cleanup)
	}
	if !fake.deleteBounded || !fake.deleteCDBounded {
		t.Fatalf("cleanup contexts: delete=%v delete-config-drive=%v", fake.deleteBounded, fake.deleteCDBounded)
	}
	if isoE2ECleanupTimeout <= 2*xcpNgShutdownTimeout {
		t.Fatalf("cleanup timeout %s must reserve deletion time after graceful and forced shutdown", isoE2ECleanupTimeout)
	}
}

func TestReserveISOArtifactPathDoesNotReuseNames(t *testing.T) {
	dir := t.TempDir()
	first, err := reserveISOArtifactPath(dir, "linux-seed-*.iso")
	if err != nil {
		t.Fatal(err)
	}
	second, err := reserveISOArtifactPath(dir, "linux-seed-*.iso")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("artifact path reused: %s", first)
	}
}

func TestISOE2ELeaseIDIsCollisionResistantUnlessOverridden(t *testing.T) {
	oldNewLeaseID := isoE2ENewLeaseID
	ids := []string{"cbx_010203040506", "cbx_111213141516"}
	isoE2ENewLeaseID = func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	}
	t.Cleanup(func() { isoE2ENewLeaseID = oldNewLeaseID })

	first := isoE2ELeaseID()
	second := isoE2ELeaseID()
	if first == second || first != "cbx_isoe2e_010203040506" || second != "cbx_isoe2e_111213141516" {
		t.Fatalf("lease IDs first=%q second=%q", first, second)
	}
	t.Setenv("CRABBOX_XCP_NG_ISO_E2E_LEASE_ID", "cbx_isoe2e_recovery")
	if got := isoE2ELeaseID(); got != "cbx_isoe2e_recovery" {
		t.Fatalf("override lease ID=%q", got)
	}
}

func TestISOE2EKeyCleanupFollowsRemoteCleanupStatus(t *testing.T) {
	oldRemoveStoredKey := isoE2ERemoveStoredTestboxKey
	var removed []string
	isoE2ERemoveStoredTestboxKey = func(leaseID string) error {
		removed = append(removed, leaseID)
		return nil
	}
	t.Cleanup(func() { isoE2ERemoveStoredTestboxKey = oldRemoveStoredKey })

	for _, status := range []string{"skipped", "resource_cleanup_failed"} {
		runtime := isoE2ERuntime{leaseID: "cbx_keep", keyPath: "/tmp/key", ownsKey: true, vm: xcpNgFreshVMResult{VM: xapiVM{Ref: "OpaqueRef:vm"}}}
		if err := runtime.cleanupStoredTestboxKey(status); err != nil {
			t.Fatal(err)
		}
	}
	reused := isoE2ERuntime{leaseID: "cbx_reused", keyPath: "/tmp/key", vm: xcpNgFreshVMResult{VM: xapiVM{Ref: "OpaqueRef:vm"}}}
	if err := reused.cleanupStoredTestboxKey("cleaned"); err != nil {
		t.Fatal(err)
	}
	cleaned := isoE2ERuntime{leaseID: "cbx_cleaned", keyPath: "/tmp/key", ownsKey: true, vm: xcpNgFreshVMResult{VM: xapiVM{Ref: "OpaqueRef:vm"}}}
	if err := cleaned.cleanupStoredTestboxKey("cleaned"); err != nil {
		t.Fatal(err)
	}
	preparationFailure := isoE2ERuntime{leaseID: "cbx_preparation", keyPath: "/tmp/key", ownsKey: true}
	if err := preparationFailure.cleanupStoredTestboxKey("not_needed"); err != nil {
		t.Fatal(err)
	}
	if got, want := removed, []string{"cbx_cleaned", "cbx_preparation"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("removed=%v want %v", got, want)
	}
}

func TestWindowsAnswerPreparationRegistersOwnedKeyBeforeLaterFailure(t *testing.T) {
	oldEnsure := isoE2EEnsureTestboxKey
	oldExists := isoE2EStoredTestboxKeyExists
	oldPassword := isoE2EGenerateWindowsPassword
	isoE2EStoredTestboxKeyExists = func(string) bool { return false }
	isoE2EEnsureTestboxKey = func(Config, string) (string, string, error) {
		return "/tmp/owned-key", "ssh-ed25519 AAAATEST", nil
	}
	isoE2EGenerateWindowsPassword = func() (string, error) {
		return "", errors.New("password generation failed")
	}
	t.Cleanup(func() {
		isoE2EEnsureTestboxKey = oldEnsure
		isoE2EStoredTestboxKeyExists = oldExists
		isoE2EGenerateWindowsPassword = oldPassword
	})
	runtime := isoE2ERuntime{leaseID: "cbx_windows"}
	summary := ISOE2ESummary{Details: map[string]string{}}

	err := runtime.prepareWindowsAnswerMedia(context.Background(), ISOE2EOptions{}, &summary)

	if err == nil || !strings.Contains(err.Error(), "password generation failed") {
		t.Fatalf("err=%v", err)
	}
	if runtime.keyPath != "/tmp/owned-key" || !runtime.ownsKey {
		t.Fatalf("runtime key state=%#v", runtime)
	}
}

func TestISOE2ERuntimeKeepsRecoveryHandlesFromFailedAllocations(t *testing.T) {
	fake := &fakeLifecycleClient{
		freshVM:      xcpNgFreshVMResult{VM: xapiVM{Ref: "OpaqueRef:vm"}},
		attachedDisk: xcpNgConfigDrive{VDIRef: "OpaqueRef:disk", DestroyVDI: true},
		errOn: map[string]error{
			"create-fresh-vm": errors.New("create failed"),
			"attach-disk":     errors.New("attach failed"),
		},
	}
	runtime := isoE2ERuntime{
		client:    fake,
		placement: xcpNgPlacement{srRef: "OpaqueRef:sr"},
		labels:    map[string]string{"lease": "cbx_recovery"},
	}
	summary := ISOE2ESummary{Details: map[string]string{}}

	if err := runtime.createBaseVM(context.Background(), ISOE2EOptions{}, &summary); err == nil {
		t.Fatal("expected create failure")
	}
	if runtime.vm.VM.Ref != "OpaqueRef:vm" || summary.VMRef != "OpaqueRef:vm" {
		t.Fatalf("vm recovery handle runtime=%#v summary=%#v", runtime.vm, summary)
	}
	if err := runtime.attachInstallDisk(context.Background()); err == nil {
		t.Fatal("expected attach failure")
	}
	if runtime.installDisk.VDIRef != "OpaqueRef:disk" || !runtime.installDisk.DestroyVDI {
		t.Fatalf("disk recovery handle=%#v", runtime.installDisk)
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

func TestValidationSplitsConnectionFromProvisioningPlacement(t *testing.T) {
	cfg := xcpNgProviderConfig(testConfig())
	cfg.Template = ""
	cfg.TemplateUUID = ""
	cfg.SR = ""
	cfg.SRUUID = ""
	if err := validateXCPNgConfig(cfg); err != nil {
		t.Fatalf("connection validation should not require placement config: %v", err)
	}
	if err := validateXCPNgProvisioningConfig(cfg); err == nil || !strings.Contains(err.Error(), "xcpNg.template/xcpNg.templateUuid") || !strings.Contains(err.Error(), "xcpNg.sr/xcpNg.srUuid") {
		t.Fatalf("provisioning validation err=%v", err)
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
	oldRemoveStoredKey := removeStoredTestboxKey
	newLifecycleClient = func(context.Context, Config) (lifecycleClient, error) { return fake, nil }
	newLeaseID = func() string { return "cbx_testlease" }
	ensureTestboxKeyForConfig = func(Config, string) (string, string, error) {
		return "/tmp/crabbox-test-key", "ssh-ed25519 AAAATEST crabbox", nil
	}
	waitForSSHReady = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error { return nil }
	bootstrapWaitTimeout = func(Config) time.Duration { return 10 * time.Millisecond }
	guestIPPollInterval = time.Millisecond
	removeLeaseClaim = func(string) {}
	removeStoredTestboxKey = func(string) {}
	t.Cleanup(func() {
		newLifecycleClient = oldClient
		newLeaseID = oldLeaseID
		ensureTestboxKeyForConfig = oldKey
		waitForSSHReady = oldWait
		bootstrapWaitTimeout = oldBootstrapTimeout
		guestIPPollInterval = oldPollInterval
		removeLeaseClaim = oldRemove
		removeStoredTestboxKey = oldRemoveStoredKey
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

type mutableClock struct{ t time.Time }

func (c *mutableClock) Now() time.Time { return c.t }
