package vast

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeVastAPI struct {
	user       vastUser
	offers     []vastOffer
	instances  []vastInstance
	authErr    error
	listErr    error
	createErr  error
	getErr     error
	manageErr  error
	destroyErr error
	attachErr  error
	detachErr  error

	searches []vastOfferSearchInput
	creates  []struct {
		offerID int
		input   vastCreateInstanceInput
	}
	managed []struct {
		id    int
		input vastManageInstanceInput
	}
	destroyed []int
	attached  []struct {
		id        int
		publicKey string
	}
	detached []struct {
		id    int
		keyID string
	}
	nextID int
}

func (f *fakeVastAPI) CheckAuth(context.Context) (vastUser, error) {
	if f.authErr != nil {
		return vastUser{}, f.authErr
	}
	if f.user.ID == 0 {
		return vastUser{ID: 7, Username: "alice"}, nil
	}
	return f.user, nil
}

func (f *fakeVastAPI) SearchOffers(_ context.Context, input vastOfferSearchInput) ([]vastOffer, error) {
	f.searches = append(f.searches, input)
	return append([]vastOffer(nil), f.offers...), nil
}

func (f *fakeVastAPI) CreateInstance(_ context.Context, offerID int, input vastCreateInstanceInput) (vastCreateInstanceResponse, error) {
	f.creates = append(f.creates, struct {
		offerID int
		input   vastCreateInstanceInput
	}{offerID: offerID, input: input})
	if f.createErr != nil {
		return vastCreateInstanceResponse{}, f.createErr
	}
	if f.nextID == 0 {
		f.nextID = 100
	}
	item := vastInstance{
		ID:       f.nextID,
		Label:    input.Label,
		Status:   "running",
		SSHHost:  "203.0.113.24",
		SSHPort:  2201,
		GPUName:  "RTX 4090",
		GPUCount: 1,
		DphTotal: 0.75,
	}
	f.instances = append(f.instances, item)
	f.nextID++
	return vastCreateInstanceResponse{Success: true, NewContract: item.ID, Instance: item}, nil
}

func (f *fakeVastAPI) GetInstance(_ context.Context, id int) (vastInstance, error) {
	if f.getErr != nil {
		return vastInstance{}, f.getErr
	}
	for _, item := range f.instances {
		if item.ID == id {
			return item, nil
		}
	}
	return vastInstance{}, &vastAPIError{StatusCode: 404, Status: "404 Not Found"}
}

func (f *fakeVastAPI) ListInstances(context.Context) ([]vastInstance, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]vastInstance(nil), f.instances...), nil
}

func (f *fakeVastAPI) ManageInstance(_ context.Context, id int, input vastManageInstanceInput) (vastInstance, error) {
	f.managed = append(f.managed, struct {
		id    int
		input vastManageInstanceInput
	}{id: id, input: input})
	if f.manageErr != nil {
		return vastInstance{}, f.manageErr
	}
	for i := range f.instances {
		if f.instances[i].ID == id {
			if input.Label != "" {
				f.instances[i].Label = input.Label
			}
			if input.State != "" {
				f.instances[i].Status = input.State
			} else if f.instances[i].Status == "starting" {
				f.instances[i].Status = "running"
			}
			return f.instances[i], nil
		}
	}
	return vastInstance{}, &vastAPIError{StatusCode: 404, Status: "404 Not Found"}
}

func (f *fakeVastAPI) DestroyInstance(_ context.Context, id int) error {
	f.destroyed = append(f.destroyed, id)
	if f.destroyErr != nil {
		return f.destroyErr
	}
	out := f.instances[:0]
	for _, item := range f.instances {
		if item.ID != id {
			out = append(out, item)
		}
	}
	f.instances = out
	return nil
}

func (f *fakeVastAPI) ListInstanceSSHKeys(context.Context, int) ([]vastInstanceSSHKey, error) {
	return nil, nil
}

func (f *fakeVastAPI) AttachInstanceSSHKey(_ context.Context, id int, publicKey string) (vastAttachSSHKeyResponse, error) {
	f.attached = append(f.attached, struct {
		id        int
		publicKey string
	}{id: id, publicKey: publicKey})
	if f.attachErr != nil {
		return vastAttachSSHKeyResponse{}, f.attachErr
	}
	return vastAttachSSHKeyResponse{Success: true, Key: vastInstanceSSHKey{ID: "key-" + strconv.Itoa(id), PublicKey: publicKey}}, nil
}

func (f *fakeVastAPI) DetachInstanceSSHKey(_ context.Context, id int, keyID string) error {
	f.detached = append(f.detached, struct {
		id    int
		keyID string
	}{id: id, keyID: keyID})
	return f.detachErr
}

func newTestBackend(t *testing.T, api *fakeVastAPI) *backend {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.SSHUser = "root"
	cfg.SSHPort = "22"
	cfg.WorkRoot = "/work/crabbox"
	cfg.Vast.APIURL = "https://console.vast.ai/api/v0"
	cfg.Vast.APIKey = "test-key"
	cfg.Vast.User = "root"
	cfg.Vast.WorkRoot = "/work/crabbox"
	cfg.Vast.InstanceType = "ondemand"
	cfg.Vast.Runtype = "ssh_direct"
	cfg.Vast.Image = "nvidia/cuda:12"
	cfg.Vast.Order = "dlperf_per_dphtotal desc"
	cfg.Vast.ReleaseAction = "destroy"
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard})
	b.apiFactory = func(core.Runtime) (vastAPI, error) { return api, nil }
	b.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error { return nil }
	b.sleep = func(context.Context, time.Duration) error { return nil }
	return b
}

func TestNewBackendPreservesExplicitGenericSSHUser(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.SSHUser = "ubuntu"
	core.MarkSSHUserExplicit(&cfg)
	cfg.Vast.User = "root"

	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard})
	if b.cfg.SSHUser != "ubuntu" {
		t.Fatalf("backend SSHUser=%q want explicit generic user", b.cfg.SSHUser)
	}
	if b.DirectSSHBackend.Cfg.SSHUser != "ubuntu" {
		t.Fatalf("direct SSH backend SSHUser=%q want explicit generic user", b.DirectSSHBackend.Cfg.SSHUser)
	}
}

func TestDoctorIsReadOnlyAndCountsOwnedInventory(t *testing.T) {
	api := &fakeVastAPI{instances: []vastInstance{
		{ID: 1, Label: encodeVastOwnershipLabel("cbx_owned", "owned", "ready"), Status: "running"},
		{ID: 2, Label: "manual", Status: "running"},
	}}
	result, err := newTestBackend(t, api).Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Message, "leases=1") || !strings.Contains(result.Message, "mutation=false") {
		t.Fatalf("doctor result=%#v", result)
	}
	if len(api.creates) != 0 || len(api.destroyed) != 0 || len(api.managed) != 0 {
		t.Fatalf("doctor mutated api: creates=%v destroyed=%v managed=%v", api.creates, api.destroyed, api.managed)
	}
}

func TestListFiltersOwnedByDefaultAndAllShowsManual(t *testing.T) {
	api := &fakeVastAPI{instances: []vastInstance{
		{ID: 1, Label: encodeVastOwnershipLabel("cbx_owned", "owned", "ready"), Status: "running"},
		{ID: 2, Label: "manual", Status: "running"},
	}}
	b := newTestBackend(t, api)
	owned, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 1 || owned[0].CloudID != "1" {
		t.Fatalf("owned=%#v", owned)
	}
	all, err := b.List(context.Background(), core.ListRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all=%#v", all)
	}
}

func TestAcquireCreatesAttachesPollsReadinessAndClaims(t *testing.T) {
	api := &fakeVastAPI{offers: []vastOffer{{ID: 42, GPUName: "RTX 4090", GPUCount: 1, Rentable: true}}}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "gpu-box", Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID == "" || lease.Server.CloudID != "100" || lease.SSH.Host != "203.0.113.24" || lease.SSH.Port != "2201" || lease.SSH.User != "root" || lease.SSH.Key == "" {
		t.Fatalf("lease=%#v", lease)
	}
	if len(api.searches) != 1 || api.searches[0].Config.Order != "dlperf_per_dphtotal desc" {
		t.Fatalf("searches=%#v", api.searches)
	}
	if len(api.creates) != 1 || api.creates[0].offerID != 42 || api.creates[0].input.Config.Runtype != "ssh_direct" || api.creates[0].input.Environment["CRABBOX"] != "1" {
		t.Fatalf("creates=%#v", api.creates)
	}
	if !isVastCrabboxOwnedLabel(api.creates[0].input.Label) || api.creates[0].input.SSHKey == "" {
		t.Fatalf("create input=%#v", api.creates[0].input)
	}
	if len(api.attached) != 1 || api.attached[0].id != 100 || api.attached[0].publicKey == "" {
		t.Fatalf("attached=%#v", api.attached)
	}
	if len(api.managed) != 1 || !strings.Contains(api.managed[0].input.Label, "|ready") {
		t.Fatalf("managed=%#v", api.managed)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider("gpu-box", providerName)
	if err != nil || !ok || claim.CloudID != "100" || claim.Labels[vastKeyIDLabel] != "key-100" || claim.Labels[vastReleaseActionLabel] != "destroy" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestResolvePreservesPersistedVastClaimMetadata(t *testing.T) {
	api := &fakeVastAPI{offers: []vastOffer{{ID: 42, Rentable: true}}}
	b := newTestBackend(t, api)
	repoRoot := t.TempDir()
	b.cfg.Vast.ReleaseAction = "stop"
	b.DirectSSHBackend.Cfg = b.cfg
	if _, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: repoRoot}, RequestedSlug: "preserve-meta"}); err != nil {
		t.Fatal(err)
	}

	b.cfg.Vast.ReleaseAction = "destroy"
	b.DirectSSHBackend.Cfg = b.cfg
	resolved, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "preserve-meta", Repo: core.Repo{Root: repoRoot}})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Server.Labels[vastReleaseActionLabel] != "stop" || resolved.Server.Labels[vastKeyIDLabel] != "key-100" || resolved.Server.Labels[vastKeyOwnedLabel] != "true" {
		t.Fatalf("resolved labels=%#v", resolved.Server.Labels)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("preserve-meta", providerName)
	if claimErr != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, claimErr)
	}
	if claim.Labels[vastReleaseActionLabel] != "stop" || claim.Labels[vastKeyIDLabel] != "key-100" || claim.Labels[vastKeyOwnedLabel] != "true" {
		t.Fatalf("claim labels=%#v", claim.Labels)
	}
}

func TestResolveRejectsTerminalStatusForRunButAllowsRelease(t *testing.T) {
	api := &fakeVastAPI{instances: []vastInstance{{ID: 9, Label: encodeVastOwnershipLabel("cbx_failed", "failed", "ready"), Status: "failed", SSHHost: "203.0.113.9", SSHPort: 22}}}
	b := newTestBackend(t, api)
	_, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "failed"})
	if err == nil || !strings.Contains(err.Error(), "terminal status failed") {
		t.Fatalf("err=%v", err)
	}
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "failed", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_failed" || lease.Server.CloudID != "9" {
		t.Fatalf("lease=%#v", lease)
	}
}

func TestResolveStatusOnlyAllowsInstanceWithoutSSHEndpoint(t *testing.T) {
	api := &fakeVastAPI{instances: []vastInstance{{ID: 10, Label: encodeVastOwnershipLabel("cbx_status", "status-me", "stopped"), Status: "stopped"}}}
	b := newTestBackend(t, api)

	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "status-me", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_status" || lease.SSH.Host != "" {
		t.Fatalf("lease=%#v", lease)
	}
}

func TestAcquireRollsBackOnCallbackFailure(t *testing.T) {
	api := &fakeVastAPI{offers: []vastOffer{{ID: 42, Rentable: true}}}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "rollback",
		OnAcquired: func(core.LeaseTarget) error {
			return errors.New("controller unavailable")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "controller unavailable") {
		t.Fatalf("err=%v", err)
	}
	if len(api.destroyed) != 1 || api.destroyed[0] != 100 {
		t.Fatalf("destroyed=%v", api.destroyed)
	}
	if len(api.detached) != 1 || api.detached[0].keyID != "key-100" {
		t.Fatalf("detached=%v", api.detached)
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("rollback", providerName); claimErr != nil || ok {
		t.Fatalf("claim ok=%v err=%v", ok, claimErr)
	}
}

func TestAcquirePreservesRecoveryClaimWhenRollbackCleanupFails(t *testing.T) {
	api := &fakeVastAPI{offers: []vastOffer{{ID: 42, Rentable: true}}, destroyErr: errors.New("destroy uncertain")}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "recover-me",
		OnAcquired: func(core.LeaseTarget) error {
			return errors.New("controller unavailable")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "vast cleanup failed") {
		t.Fatalf("err=%v", err)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("recover-me", providerName)
	if claimErr != nil || !ok || claim.Labels["recovery"] != "rollback-cleanup" || claim.CloudID != "100" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
}

func TestReleaseDestroysByDefaultAndRemovesClaim(t *testing.T) {
	api := &fakeVastAPI{offers: []vastOffer{{ID: 42, Rentable: true}}}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "destroy-me"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.destroyed) != 1 || api.destroyed[0] != 100 {
		t.Fatalf("destroyed=%v", api.destroyed)
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("destroy-me", providerName); claimErr != nil || ok {
		t.Fatalf("claim ok=%v err=%v", ok, claimErr)
	}
}

func TestReleaseStopIsExplicitAndTested(t *testing.T) {
	api := &fakeVastAPI{offers: []vastOffer{{ID: 42, Rentable: true}}}
	b := newTestBackend(t, api)
	b.cfg.Vast.ReleaseAction = "stop"
	b.DirectSSHBackend.Cfg = b.cfg
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "stop-me"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.destroyed) != 0 {
		t.Fatalf("destroyed=%v", api.destroyed)
	}
	if len(api.managed) < 2 || api.managed[len(api.managed)-1].input.State != "stopped" {
		t.Fatalf("managed=%#v", api.managed)
	}
}

func TestReleaseLeaseMessageUsesPersistedReleaseAction(t *testing.T) {
	b := newTestBackend(t, &fakeVastAPI{})
	b.cfg.Vast.ReleaseAction = "destroy"
	lease := core.LeaseTarget{
		LeaseID: "cbx_message",
		Server: core.Server{
			CloudID:  "100",
			Name:     "message-me",
			Provider: providerName,
			Labels: map[string]string{
				vastReleaseActionLabel: "stop",
			},
		},
	}

	msg := b.ReleaseLeaseMessage(lease)
	if !strings.Contains(msg, "stop lease=cbx_message") || strings.Contains(msg, "destroyed") {
		t.Fatalf("message=%q", msg)
	}
}

func TestCleanupDryRunDoesNotDestroyExpiredOwnedInstance(t *testing.T) {
	api := &fakeVastAPI{instances: []vastInstance{{ID: 8, Label: encodeVastOwnershipLabel("cbx_old", "old", "ready"), Status: "running"}}}
	b := newTestBackend(t, api)
	server := serverFromInstance(api.instances[0], b.cfg)
	server.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_old", "old", b.cfg, server, core.SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	b.rt.Stderr = &stderr
	b.DirectSSHBackend.RT = b.rt
	if err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(api.destroyed) != 0 {
		t.Fatalf("destroyed=%v", api.destroyed)
	}
	if !strings.Contains(stderr.String(), "delete server id=8") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestCleanupReportsMissingClaimForOwnedInstance(t *testing.T) {
	api := &fakeVastAPI{instances: []vastInstance{{ID: 9, Label: encodeVastOwnershipLabel("cbx_orphan", "orphan", "ready"), Status: "running"}}}
	b := newTestBackend(t, api)
	var stderr bytes.Buffer
	b.rt.Stderr = &stderr
	b.DirectSSHBackend.RT = b.rt

	err := b.Cleanup(context.Background(), core.CleanupRequest{})
	if err == nil || !strings.Contains(err.Error(), "lease=cbx_orphan has no local Vast claim") {
		t.Fatalf("err=%v", err)
	}
	if len(api.destroyed) != 0 {
		t.Fatalf("destroyed=%v", api.destroyed)
	}
	if !strings.Contains(stderr.String(), "delete server id=9") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestManualUnownedCleanupIsRejected(t *testing.T) {
	api := &fakeVastAPI{instances: []vastInstance{{ID: 5, Label: "manual-instance", Status: "running", SSHHost: "203.0.113.5", SSHPort: 22}}}
	b := newTestBackend(t, api)
	manual := serverFromInstance(api.instances[0], b.cfg)
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: "manual", Server: manual}})
	if err == nil || !strings.Contains(err.Error(), "non-Crabbox Vast instance") {
		t.Fatalf("err=%v", err)
	}
	if len(api.destroyed) != 0 {
		t.Fatalf("destroyed=%v", api.destroyed)
	}
}

func TestTouchUpdatesLocalClaimLabels(t *testing.T) {
	api := &fakeVastAPI{offers: []vastOffer{{ID: 42, Rentable: true}}}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "touch-me"})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := b.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "busy", IdleTimeout: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Labels["state"] != "busy" {
		t.Fatalf("updated=%#v", updated.Labels)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider("touch-me", providerName)
	if err != nil || !ok || claim.Labels["state"] != "busy" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}
