package vultr

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"github.com/openclaw/crabbox/internal/testutil"
)

type fakeVultrAPI struct {
	accountID    string
	accountErr   error
	accountFn    func(context.Context) (string, error)
	instances    []vultrInstance
	createErr    error
	getErr       error
	getFn        func(context.Context, string) (vultrInstance, error)
	deleteFn     func(context.Context, string) error
	keyDeleteFn  func(context.Context, string) error
	deleteErr    error
	keyDeleteErr error
	updateErr    error
	reuseSSHKey  bool
	sshKeys      []vultrSSHKey
	created      []vultrInstance
	deleted      []string
	deletedKeys  []string
	updated      []struct {
		id   string
		tags []string
	}
	createRequests []struct {
		cfg     core.Config
		leaseID string
		slug    string
		keep    bool
	}
}

type vultrTestClock struct{ now time.Time }

func (c vultrTestClock) Now() time.Time { return c.now }

func (f *fakeVultrAPI) AccountID(ctx context.Context) (string, error) {
	if f.accountFn != nil {
		return f.accountFn(ctx)
	}
	if f.accountErr != nil {
		return "", f.accountErr
	}
	if f.accountID != "" {
		return f.accountID, nil
	}
	return "account:test-account", nil
}

func (f *fakeVultrAPI) ListCrabboxInstances(context.Context) ([]vultrInstance, error) {
	var out []vultrInstance
	for _, item := range append(append([]vultrInstance{}, f.instances...), f.created...) {
		if isOwnedInstance(item) {
			out = append(out, item)
		}
	}
	return out, nil
}

func (f *fakeVultrAPI) GetInstance(ctx context.Context, id string) (vultrInstance, error) {
	if f.getFn != nil {
		return f.getFn(ctx, id)
	}
	if f.getErr != nil {
		return vultrInstance{}, f.getErr
	}
	for _, item := range append(append([]vultrInstance{}, f.instances...), f.created...) {
		if item.ID == id {
			return item, nil
		}
	}
	return vultrInstance{}, &vultrAPIError{Status: 404}
}

func (f *fakeVultrAPI) CreateInstance(_ context.Context, cfg core.Config, publicKey string, leaseID, slug string, keep bool, now time.Time) (vultrInstance, error) {
	f.createRequests = append(f.createRequests, struct {
		cfg     core.Config
		leaseID string
		slug    string
		keep    bool
	}{cfg: cfg, leaseID: leaseID, slug: slug, keep: keep})
	if f.createErr != nil {
		return vultrInstance{}, f.createErr
	}
	id := "11111111-1111-4111-8111-111111111111"
	item := vultrInstance{
		ID:            id,
		MainIP:        "203.0.113.20",
		Status:        "active",
		PowerStatus:   "running",
		ServerStatus:  "ok",
		Label:         core.LeaseProviderName(leaseID, slug),
		Hostname:      core.LeaseProviderName(leaseID, slug),
		Plan:          cfg.ServerType,
		Tags:          leaseTags(cfg, leaseID, slug, "provisioning", keep, now),
		SSHKeyID:      "key-123",
		SSHKeyCreated: !f.reuseSSHKey,
	}
	if !f.reuseSSHKey {
		f.sshKeys = append(f.sshKeys, vultrSSHKey{ID: "key-123", Name: providerKeyForLease(leaseID), SSHKey: publicKey})
	}
	f.created = append(f.created, item)
	return item, nil
}

func (f *fakeVultrAPI) DeleteInstance(ctx context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	if f.deleteFn != nil {
		if err := f.deleteFn(ctx, id); err != nil {
			return err
		}
	}
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.instances = removeVultrInstance(f.instances, id)
	f.created = removeVultrInstance(f.created, id)
	return nil
}

func (f *fakeVultrAPI) FindSSHKey(_ context.Context, name, publicKey string) (vultrSSHKey, bool, error) {
	return selectVultrSSHKey(f.sshKeys, name, publicKey)
}

func (f *fakeVultrAPI) FindSSHKeyByID(_ context.Context, id string) (vultrSSHKey, bool, error) {
	for _, key := range f.sshKeys {
		if key.ID == id {
			return key, true, nil
		}
	}
	return vultrSSHKey{}, false, nil
}

func (f *fakeVultrAPI) DeleteSSHKey(ctx context.Context, id string) error {
	f.deletedKeys = append(f.deletedKeys, id)
	if f.keyDeleteFn != nil {
		if err := f.keyDeleteFn(ctx, id); err != nil {
			return err
		}
	}
	if f.keyDeleteErr == nil {
		for i, key := range f.sshKeys {
			if key.ID == id {
				f.sshKeys = append(f.sshKeys[:i], f.sshKeys[i+1:]...)
				break
			}
		}
	}
	return f.keyDeleteErr
}

func (f *fakeVultrAPI) UpdateInstanceTags(_ context.Context, id string, tags []string) error {
	f.updated = append(f.updated, struct {
		id   string
		tags []string
	}{id: id, tags: append([]string(nil), tags...)})
	if f.updateErr != nil {
		return f.updateErr
	}
	for i := range f.created {
		if f.created[i].ID == id {
			f.created[i].Tags = tags
		}
	}
	for i := range f.instances {
		if f.instances[i].ID == id {
			f.instances[i].Tags = tags
		}
	}
	return nil
}

func removeVultrInstance(items []vultrInstance, id string) []vultrInstance {
	out := items[:0]
	for _, item := range items {
		if item.ID != id {
			out = append(out, item)
		}
	}
	return out
}

func newTestBackend(t *testing.T, api *fakeVultrAPI) *backend {
	t.Helper()
	testutil.IsolateUserDirs(t)
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "vc2-1c-1gb"
	cfg.SSHUser = "root"
	cfg.WorkRoot = "/work/crabbox"
	cfg.Vultr.OS = "2284"
	b := NewBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*backend)
	b.clientFactory = func(core.Runtime) (vultrAPI, error) { return api, nil }
	b.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error { return nil }
	return b
}

func TestWaitForInstanceReady(t *testing.T) {
	const instanceID = "11111111-1111-4111-8111-111111111111"
	t.Run("pending to ready", func(t *testing.T) {
		calls := 0
		api := &fakeVultrAPI{getFn: func(context.Context, string) (vultrInstance, error) {
			calls++
			if calls == 1 {
				return vultrInstance{ID: instanceID, Status: "pending", MainIP: "0.0.0.0"}, nil
			}
			return vultrInstance{ID: instanceID, Status: "active", PowerStatus: "running", ServerStatus: "ok", MainIP: "203.0.113.20"}, nil
		}}
		got, err := newTestBackend(t, api).waitForInstanceReady(context.Background(), api, instanceID, time.Minute)
		if err != nil || got.ID != instanceID || calls != 2 {
			t.Fatalf("instance=%#v err=%v calls=%d", got, err, calls)
		}
	})

	t.Run("read error", func(t *testing.T) {
		wantErr := errors.New("read denied")
		calls := 0
		api := &fakeVultrAPI{getFn: func(context.Context, string) (vultrInstance, error) {
			calls++
			return vultrInstance{}, wantErr
		}}
		_, err := newTestBackend(t, api).waitForInstanceReady(context.Background(), api, instanceID, time.Minute)
		if !errors.Is(err, wantErr) || calls != 1 {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	})

	t.Run("client deadline", func(t *testing.T) {
		api := &fakeVultrAPI{getErr: context.DeadlineExceeded}
		_, err := newTestBackend(t, api).waitForInstanceReady(context.Background(), api, instanceID, time.Minute)
		if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timed out waiting") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("read error at deadline", func(t *testing.T) {
		wantErr := errors.New("late read denied")
		api := &fakeVultrAPI{getFn: func(ctx context.Context, _ string) (vultrInstance, error) {
			<-ctx.Done()
			return vultrInstance{}, wantErr
		}}
		_, err := newTestBackend(t, api).waitForInstanceReady(context.Background(), api, instanceID, 10*time.Millisecond)
		if !errors.Is(err, wantErr) || strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("cancellation during delay", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		api := &fakeVultrAPI{getFn: func(context.Context, string) (vultrInstance, error) {
			calls++
			time.AfterFunc(time.Millisecond, cancel)
			return vultrInstance{ID: instanceID, Status: "pending", MainIP: "0.0.0.0"}, nil
		}}
		_, err := newTestBackend(t, api).waitForInstanceReady(ctx, api, instanceID, time.Minute)
		if !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		api := &fakeVultrAPI{instances: []vultrInstance{{ID: instanceID, Status: "pending", MainIP: "0.0.0.0"}}}
		_, err := newTestBackend(t, api).waitForInstanceReady(context.Background(), api, instanceID, 10*time.Millisecond)
		if err == nil || err.Error() != "timed out waiting for Vultr instance IP" {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestLimitedUserSchemeDefaultsSSHUser(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Vultr.UserScheme = "limited"
	b := NewBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*backend)
	if b.Cfg.SSHUser != "limited" {
		t.Fatalf("SSHUser=%q", b.Cfg.SSHUser)
	}

	cfg = core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Vultr.UserScheme = "limited"
	cfg.SSHUser = "alice"
	core.MarkSSHUserExplicit(&cfg)
	b = NewBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*backend)
	if b.Cfg.SSHUser != "alice" {
		t.Fatalf("explicit SSHUser=%q", b.Cfg.SSHUser)
	}
}

func TestAcquireCreatesInstanceClaimsLeaseAndMarksReady(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "my-app", Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID == "" || lease.Server.CloudID == "" || lease.SSH.Host != "203.0.113.20" || lease.SSH.User != "root" {
		t.Fatalf("lease=%#v", lease)
	}
	if len(api.createRequests) != 1 || api.createRequests[0].slug != "my-app" || !api.createRequests[0].keep {
		t.Fatalf("createRequests=%#v", api.createRequests)
	}
	if len(api.updated) != 1 || labelsFromTags(api.updated[0].tags)["state"] != "ready" {
		t.Fatalf("updated=%#v", api.updated)
	}
	claim, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != providerName || claim.CloudID != lease.Server.CloudID || claim.Labels[vultrAccountLabel] != "account:test-account" || claim.Labels[vultrKeyOwnedLabel] != "true" {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestReleaseRefusesAccountMismatchAndForeignTags(t *testing.T) {
	api := &fakeVultrAPI{accountID: "account:current"}
	b := newTestBackend(t, api)
	labels := leaseTags(b.Cfg, "cbx_111111111111", "blue", "ready", false, time.Now())
	server := serverFromInstance(vultrInstance{
		ID:           "11111111-1111-4111-8111-111111111111",
		MainIP:       "203.0.113.20",
		Status:       "active",
		PowerStatus:  "running",
		ServerStatus: "ok",
		Label:        core.LeaseProviderName("cbx_111111111111", "blue"),
		Plan:         b.Cfg.ServerType,
		Tags:         labels,
	}, b.Cfg)
	server.Labels[vultrAccountLabel] = "account:other"
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_111111111111", "blue", b.Cfg, server, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	attachCurrentVultrClaim(t, &server, "cbx_111111111111")
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: "cbx_111111111111", Server: server}}); err == nil || !strings.Contains(err.Error(), "account mismatch") {
		t.Fatalf("err=%v", err)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("deleted=%v", api.deleted)
	}
	server.Labels = map[string]string{"crabbox": "true", "provider": providerName}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: "cbx_111111111111", Server: server}}); err == nil || !strings.Contains(err.Error(), "non-Crabbox Vultr instance") {
		t.Fatalf("foreign err=%v", err)
	}
}

func TestReleaseDeletesOwnedInstanceKeyAndClaim(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != lease.Server.CloudID {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if len(api.deletedKeys) != 1 || api.deletedKeys[0] != "key-123" {
		t.Fatalf("deletedKeys=%v", api.deletedKeys)
	}
	if _, ok, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
}

func TestReleaseDeletesOwnedInstanceWhenSSHKeyAlreadyGone(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	api.sshKeys = nil
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != lease.Server.CloudID {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if len(api.deletedKeys) != 0 {
		t.Fatalf("deletedKeys=%v", api.deletedKeys)
	}
	if _, ok, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
}

func TestReleaseDeletesOwnedRenamedSSHKeyByImmutableID(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	api.sshKeys[0].Name = "renamed-outside-crabbox"
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedKeys) != 1 || api.deletedKeys[0] != "key-123" {
		t.Fatalf("deletedKeys=%v", api.deletedKeys)
	}
}

func TestPrepareCleanupCarriesOnlyExactClaimSnapshot(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "prepared"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := b.prepareCleanupServer(context.Background(), lease.Server)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, exists, set := core.ServerLeaseClaimSnapshot(prepared)
	current, currentExists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if !set || !exists || !currentExists || snapshot.Revision == "" || !reflect.DeepEqual(snapshot, current) {
		t.Fatalf("snapshot=%#v exists=%v set=%v current=%#v currentExists=%v", snapshot, exists, set, current, currentExists)
	}
	if prepared.ProviderMetadata != nil {
		t.Fatalf("ProviderMetadata=%#v, want no redundant cleanup authorization", prepared.ProviderMetadata)
	}
}

func TestDeleteRefusesClaimChangedAfterPreparationBeforeProviderMutation(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "stale"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := b.prepareCleanupServer(context.Background(), lease.Server)
	if err != nil {
		t.Fatal(err)
	}
	expected, _, _ := core.ServerLeaseClaimSnapshot(prepared)
	replacement := expected
	replacement.Labels = maps.Clone(expected.Labels)
	replacement.Labels["state"] = "running"
	if err := core.ReplaceLeaseClaimIfUnchanged(lease.LeaseID, expected, replacement); err != nil {
		t.Fatal(err)
	}
	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: lease.LeaseID, Server: prepared}})
	if err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("provider mutations instance=%v key=%v", api.deleted, api.deletedKeys)
	}
}

func TestClaimUpdateBlocksDuringVultrProviderAction(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "locked"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := b.prepareCleanupServer(context.Background(), lease.Server)
	if err != nil {
		t.Fatal(err)
	}
	expected, _, _ := core.ServerLeaseClaimSnapshot(prepared)
	deleteStarted := make(chan struct{})
	allowDelete := make(chan struct{})
	api.deleteFn = func(context.Context, string) error {
		close(deleteStarted)
		<-allowDelete
		return nil
	}
	releaseDone := make(chan error, 1)
	go func() {
		releaseDone <- b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: lease.LeaseID, Server: prepared}})
	}()
	<-deleteStarted
	updateDone := make(chan error, 1)
	go func() {
		replacement := expected
		replacement.LastUsedAt = "2030-01-02T03:04:05Z"
		updateDone <- core.ReplaceLeaseClaimIfUnchanged(lease.LeaseID, expected, replacement)
	}()
	select {
	case err := <-updateDone:
		t.Fatalf("claim update completed while provider action held claim lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowDelete)
	if err := <-releaseDone; err != nil {
		t.Fatal(err)
	}
	if err := <-updateDone; err == nil {
		t.Fatal("claim update succeeded after locked provider action removed the claim")
	}
}

func TestInstanceDeleteErrorPreservesClaimKeyAndOrdering(t *testing.T) {
	api := &fakeVultrAPI{deleteErr: errors.New("instance delete failed")}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "instance-error"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := b.prepareCleanupServer(context.Background(), lease.Server)
	if err != nil {
		t.Fatal(err)
	}
	expected, _, _ := core.ServerLeaseClaimSnapshot(prepared)
	keyPath, err := core.TestboxKeyPath(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: lease.LeaseID, Server: prepared}})
	if err == nil || !strings.Contains(err.Error(), "instance delete failed") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists || !reflect.DeepEqual(claim, expected) {
		t.Fatalf("claim=%#v exists=%v err=%v expected=%#v", claim, exists, err, expected)
	}
	if len(api.deleted) != 1 || len(api.deletedKeys) != 0 {
		t.Fatalf("provider calls instance=%v key=%v", api.deleted, api.deletedKeys)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key missing after failed instance delete: %v", err)
	}
}

func TestKeyDeleteFailureRetainsRecoveryThenRetrySkipsAbsentInstance(t *testing.T) {
	api := &fakeVultrAPI{keyDeleteErr: errors.New("key delete failed")}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "partial"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := b.prepareCleanupServer(context.Background(), lease.Server)
	if err != nil {
		t.Fatal(err)
	}
	expected, _, _ := core.ServerLeaseClaimSnapshot(prepared)
	keyPath, err := core.TestboxKeyPath(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: lease.LeaseID, Server: prepared}})
	if err == nil || !strings.Contains(err.Error(), "key delete failed") {
		t.Fatalf("first ReleaseLease err=%v", err)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists || !reflect.DeepEqual(claim, expected) || claim.CloudID != lease.Server.CloudID || claim.Labels[vultrKeyIDLabel] != "key-123" {
		t.Fatalf("claim=%#v exists=%v err=%v expected=%#v", claim, exists, err, expected)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key missing after partial delete: %v", err)
	}
	api.keyDeleteErr = nil
	retry, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "partial", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: retry}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 {
		t.Fatalf("retry called instance delete after confirmed absence: %v", api.deleted)
	}
	if len(api.deletedKeys) != 2 || api.deletedKeys[1] != "key-123" {
		t.Fatalf("key deletes=%v", api.deletedKeys)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v", exists, err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stored key still present after retry: %v", err)
	}
}

func TestPreparedDeleteRefusesChangedRetainedPublicKey(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "changed-public-key"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := b.prepareCleanupServer(context.Background(), lease.Server)
	if err != nil {
		t.Fatal(err)
	}
	keyPath, err := core.TestboxKeyPath(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath+".pub", []byte("ssh-ed25519 changed-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: lease.LeaseID, Server: prepared}})
	if err == nil || !strings.Contains(err.Error(), "public key does not match retained ownership") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("provider mutations instance=%v key=%v", api.deleted, api.deletedKeys)
	}
}

func TestPreparedDeleteRefusesChangedProviderPublicKey(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "changed-provider-key"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := b.prepareCleanupServer(context.Background(), lease.Server)
	if err != nil {
		t.Fatal(err)
	}
	api.sshKeys[0].SSHKey = "ssh-ed25519 changed-provider-key"
	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: lease.LeaseID, Server: prepared}})
	if err == nil || !strings.Contains(err.Error(), "public key does not match retained ownership") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("provider mutations instance=%v key=%v", api.deleted, api.deletedKeys)
	}
}

func TestDeleteRevalidatesAccountInsideClaimLock(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "changed-account"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := b.prepareCleanupServer(context.Background(), lease.Server)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	api.accountFn = func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return "account:test-account", nil
		}
		return "account:changed", nil
	}
	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: lease.LeaseID, Server: prepared}})
	if err == nil || !strings.Contains(err.Error(), "account mismatch") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if calls != 2 || len(api.deleted) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("account calls=%d instance deletes=%v key deletes=%v", calls, api.deleted, api.deletedKeys)
	}
}

func TestDeleteRevalidatesLiveInstanceIdentityInsideClaimLock(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*vultrInstance)
	}{
		{"name", func(item *vultrInstance) { item.Label = "replaced" }},
		{"lease-tag", func(item *vultrInstance) {
			labels := labelsFromTags(item.Tags)
			labels["lease"] = "cbx_ffffffffffff"
			item.Tags = tagsFromLabels(labels)
		}},
		{"provider-key", func(item *vultrInstance) {
			labels := labelsFromTags(item.Tags)
			labels["provider_key"] = "changed"
			item.Tags = tagsFromLabels(labels)
		}},
		{"owned-key-id", func(item *vultrInstance) {
			labels := labelsFromTags(item.Tags)
			labels[vultrKeyIDLabel] = "changed"
			item.Tags = tagsFromLabels(labels)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &fakeVultrAPI{}
			b := newTestBackend(t, api)
			lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "changed-instance"})
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := b.prepareCleanupServer(context.Background(), lease.Server)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&api.created[0])
			err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: lease.LeaseID, Server: prepared}})
			if err == nil || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("ReleaseLease err=%v", err)
			}
			if len(api.deleted) != 0 || len(api.deletedKeys) != 0 {
				t.Fatalf("provider mutations instance=%v key=%v", api.deleted, api.deletedKeys)
			}
		})
	}
}

func TestDeleteRequiresRetainedPublicKeyBeforeProviderMutation(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "missing-key"})
	if err != nil {
		t.Fatal(err)
	}
	keyPath, err := core.TestboxKeyPath(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath + ".pub"); err != nil {
		t.Fatal(err)
	}
	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
	if err == nil || !strings.Contains(err.Error(), "requires retained local public key") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("provider mutations instance=%v key=%v", api.deleted, api.deletedKeys)
	}
}

func TestUnownedProviderKeyIsNeverDeleted(t *testing.T) {
	api := &fakeVultrAPI{reuseSSHKey: true}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "reused-key"})
	if err != nil {
		t.Fatal(err)
	}
	api.sshKeys = append(api.sshKeys, vultrSSHKey{ID: "key-123", Name: "shared-key", SSHKey: "ssh-ed25519 shared"})
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || len(api.deletedKeys) != 0 || len(api.sshKeys) != 1 {
		t.Fatalf("instance deletes=%v key deletes=%v keys=%#v", api.deleted, api.deletedKeys, api.sshKeys)
	}
}

func TestConfirmedAbsentInstanceSkipsInstanceCallAndFinalizesExactKey(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "absent-instance"})
	if err != nil {
		t.Fatal(err)
	}
	api.created = nil
	prepared, err := b.prepareCleanupServer(context.Background(), lease.Server)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: lease.LeaseID, Server: prepared}}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeys) != 1 {
		t.Fatalf("instance deletes=%v key deletes=%v", api.deleted, api.deletedKeys)
	}
}

func TestRollbackVultrAcquireRetainsSSHKeyWhenInstanceDeleteFails(t *testing.T) {
	api := &fakeVultrAPI{deleteErr: os.ErrPermission}
	err := rollbackVultrAcquire(api, "instance-123", "key-123", true)
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("err=%v", err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != "instance-123" {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if len(api.deletedKeys) != 0 {
		t.Fatalf("deletedKeys=%v", api.deletedKeys)
	}
}

func TestResolveHyphenatedSlugDoesNotUseInstanceIDLookup(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	tags := leaseTags(b.Cfg, "cbx_222222222222", "my-long-app", "ready", false, time.Now())
	api.instances = []vultrInstance{{
		ID:           "33333333-3333-4333-8333-333333333333",
		Label:        core.LeaseProviderName("cbx_222222222222", "my-long-app"),
		MainIP:       "203.0.113.40",
		Status:       "active",
		PowerStatus:  "running",
		ServerStatus: "ok",
		Plan:         b.Cfg.ServerType,
		Tags:         tags,
	}}
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "my-long-app", Repo: core.Repo{Root: t.TempDir()}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_222222222222" || lease.Server.CloudID != "33333333-3333-4333-8333-333333333333" {
		t.Fatalf("lease=%#v", lease)
	}
}

func TestResolveReadOnlyDoesNotClaimVisibleInstance(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	leaseID := "cbx_222222222223"
	slug := "read-only"
	api.instances = []vultrInstance{{
		ID:           "33333333-3333-4333-8333-333333333334",
		Label:        core.LeaseProviderName(leaseID, slug),
		MainIP:       "203.0.113.41",
		Status:       "active",
		PowerStatus:  "running",
		ServerStatus: "ok",
		Plan:         b.Cfg.ServerType,
		Tags:         leaseTags(b.Cfg, leaseID, slug, "ready", false, time.Now()),
	}}

	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: slug, Repo: core.Repo{Root: t.TempDir()}, NoLocalStateMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.Server.CloudID != api.instances[0].ID {
		t.Fatalf("lease=%#v", lease)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("read-only resolve wrote claim: exists=%v err=%v", exists, err)
	}
}

func TestResolveReadOnlyIgnoresStaleInstanceClaim(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	leaseID := "cbx_222222222225"
	slug := "stale-read-only"
	cloudID := "33333333-3333-4333-8333-333333333336"
	api.instances = []vultrInstance{{
		ID:           cloudID,
		Label:        core.LeaseProviderName(leaseID, slug),
		MainIP:       "203.0.113.42",
		Status:       "active",
		PowerStatus:  "running",
		ServerStatus: "ok",
		Plan:         b.Cfg.ServerType,
		Tags:         leaseTags(b.Cfg, leaseID, slug, "ready", false, time.Now()),
	}}
	labels := labelsFromTags(api.instances[0].Tags)
	labels[vultrAccountLabel] = "account:test-account"
	claimServer := core.Server{Provider: providerName, CloudID: "stale-cloud-id", Name: api.instances[0].Label, Labels: labels}
	if err := core.ClaimLeaseTargetForConfig(leaseID, slug, b.Cfg, claimServer, core.SSHTarget{}, b.Cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}

	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: slug, StatusOnly: true, NoLocalStateMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.Server.CloudID != cloudID {
		t.Fatalf("lease=%#v", lease)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || claim.CloudID != "stale-cloud-id" {
		t.Fatalf("read-only resolve changed stale claim: claim=%#v exists=%v err=%v", claim, exists, err)
	}
}

func TestStatusTouchClaimRequiresMatchingAccount(t *testing.T) {
	backend := &backend{}
	claim := core.LeaseClaim{Labels: map[string]string{vultrAccountLabel: "account:account-a"}}
	lease := core.LeaseTarget{Server: core.Server{Labels: map[string]string{vultrAccountLabel: "account:account-a"}}}
	if !backend.StatusTouchClaimMatches(lease, claim) {
		t.Fatal("matching account identity was rejected")
	}
	lease.Server.Labels[vultrAccountLabel] = "account:account-b"
	if backend.StatusTouchClaimMatches(lease, claim) {
		t.Fatal("mismatched account identity was accepted")
	}
	delete(claim.Labels, vultrAccountLabel)
	if backend.StatusTouchClaimMatches(lease, claim) {
		t.Fatal("missing claim account identity was accepted")
	}
}

func TestResolveRejectsCloudlessClaimBeforeRebinding(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	const (
		leaseID  = "cbx_222222222224"
		slug     = "cloudless"
		cloudID  = "33333333-3333-4333-8333-333333333335"
		repoRoot = "/tmp/repo"
	)
	labels := labelsFromTags(leaseTags(b.Cfg, leaseID, slug, "ready", false, time.Now()))
	labels[vultrAccountLabel] = "account:test-account"
	claimServer := core.Server{
		Provider: providerName,
		Name:     core.LeaseProviderName(leaseID, slug),
		Labels:   labels,
	}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, b.Cfg, claimServer, core.SSHTarget{}, repoRoot, b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	api.instances = []vultrInstance{{
		ID:           cloudID,
		Label:        core.LeaseProviderName(leaseID, slug),
		MainIP:       "203.0.113.42",
		Status:       "active",
		PowerStatus:  "running",
		ServerStatus: "ok",
		Plan:         b.Cfg.ServerType,
		Tags:         tagsFromLabels(labels),
	}}

	_, err := b.Resolve(context.Background(), core.ResolveRequest{ID: slug, Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "no instance identity") {
		t.Fatalf("Resolve err=%v", err)
	}
	claim, claimErr := core.ReadLeaseClaim(leaseID)
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if claim.CloudID != "" {
		t.Fatalf("cloudless claim was rebound: %#v", claim)
	}
}

func TestReleaseRefusesUnknownKeyOwnershipBeforeDeleting(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	tags := leaseTags(b.Cfg, "cbx_333333333333", "blue", "ready", false, time.Now())
	api.instances = []vultrInstance{{
		ID:           "44444444-4444-4444-8444-444444444444",
		Label:        core.LeaseProviderName("cbx_333333333333", "blue"),
		MainIP:       "203.0.113.50",
		Status:       "active",
		PowerStatus:  "running",
		ServerStatus: "ok",
		Plan:         b.Cfg.ServerType,
		Tags:         tags,
	}}
	server := serverFromInstance(api.instances[0], b.Cfg)
	server.Labels[vultrAccountLabel] = "account:test-account"
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_333333333333", "blue", b.Cfg, server, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	attachCurrentVultrClaim(t, &server, "cbx_333333333333")
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: "cbx_333333333333", Server: server}})
	if err == nil || !strings.Contains(err.Error(), "SSH key ownership remains indeterminate") {
		t.Fatalf("err=%v", err)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("deleted before proving key cleanup: %v", api.deleted)
	}
}

func TestReleaseFromCloudTagsWithoutLocalClaimFailsClosed(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	_, publicKey, err := core.EnsureTestboxKeyForConfig(b.Cfg, "cbx_444444444444")
	if err != nil {
		t.Fatal(err)
	}
	api.sshKeys = append(api.sshKeys, vultrSSHKey{ID: "key-cloud", Name: providerKeyForLease("cbx_444444444444"), SSHKey: publicKey})
	labels := labelsFromTags(leaseTags(b.Cfg, "cbx_444444444444", "blue", "ready", false, time.Now()))
	setVultrKeyIdentity(labels, "key-cloud", true)
	tags := tagsFromLabels(labels)
	api.instances = []vultrInstance{{
		ID:           "66666666-6666-4666-8666-666666666666",
		Label:        core.LeaseProviderName("cbx_444444444444", "blue"),
		MainIP:       "203.0.113.60",
		Status:       "active",
		PowerStatus:  "running",
		ServerStatus: "ok",
		Plan:         b.Cfg.ServerType,
		Tags:         tags,
	}}
	server := serverFromInstance(api.instances[0], b.Cfg)
	server.Labels[vultrAccountLabel] = "account:test-account"
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: "cbx_444444444444", Server: server}}); err == nil || !strings.Contains(err.Error(), "claim snapshot is missing") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if len(api.deletedKeys) != 0 {
		t.Fatalf("deletedKeys=%v", api.deletedKeys)
	}
}

func TestReleaseRefusesTamperedProviderKeyIDBeforeDeleting(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	_, publicKey, err := core.EnsureTestboxKeyForConfig(b.Cfg, "cbx_555555555555")
	if err != nil {
		t.Fatal(err)
	}
	api.sshKeys = append(api.sshKeys, vultrSSHKey{ID: "real-key", Name: providerKeyForLease("cbx_555555555555"), SSHKey: publicKey})
	labels := labelsFromTags(leaseTags(b.Cfg, "cbx_555555555555", "blue", "ready", false, time.Now()))
	setVultrKeyIdentity(labels, "other-key", true)
	api.instances = []vultrInstance{{
		ID:           "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		Label:        core.LeaseProviderName("cbx_555555555555", "blue"),
		MainIP:       "203.0.113.61",
		Status:       "active",
		PowerStatus:  "running",
		ServerStatus: "ok",
		Plan:         b.Cfg.ServerType,
		Tags:         tagsFromLabels(labels),
	}}
	server := serverFromInstance(api.instances[0], b.Cfg)
	server.Labels[vultrAccountLabel] = "account:test-account"
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_555555555555", "blue", b.Cfg, server, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	attachCurrentVultrClaim(t, &server, "cbx_555555555555")
	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: "cbx_555555555555", Server: server}})
	if err == nil || !strings.Contains(err.Error(), "refusing to delete Vultr SSH key") {
		t.Fatalf("err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("deleted instance/key: %v %v", api.deleted, api.deletedKeys)
	}
}

func TestReleaseOnlyResolveFallsBackToLocalClaimBySlug(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	labels := core.DirectLeaseLabels(b.Cfg, "cbx_666666666666", "stale-slug", providerName, "", false, time.Now())
	labels["state"] = "ready"
	labels[vultrAccountLabel] = "account:test-account"
	labels[vultrKeyOwnedLabel] = "false"
	server := core.Server{
		Provider: providerName,
		CloudID:  "55555555-5555-4555-8555-555555555555",
		Name:     core.LeaseProviderName("cbx_666666666666", "stale-slug"),
		Labels:   labels,
	}
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_666666666666", "stale-slug", b.Cfg, server, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"stale-slug", server.CloudID} {
		lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: id, ReleaseOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		claim, exists, set := core.ServerLeaseClaimSnapshot(lease.Server)
		if lease.LeaseID != "cbx_666666666666" || lease.Server.CloudID != server.CloudID || !set || !exists || claim.Revision == "" {
			t.Fatalf("id=%s lease=%#v claim=%#v exists=%v set=%v", id, lease, claim, exists, set)
		}
	}
}

func TestReleaseOnlyClaimFallbackUsesProviderNameForLiveValidation(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "claim-only"})
	if err != nil {
		t.Fatal(err)
	}
	api.instances = nil
	api.created = nil
	api.deleted = nil
	api.deletedKeys = nil
	live := vultrInstance{
		ID:           lease.Server.CloudID,
		Label:        core.LeaseProviderName(lease.LeaseID, "claim-only"),
		MainIP:       "203.0.113.80",
		Status:       "active",
		PowerStatus:  "running",
		ServerStatus: "ok",
		Plan:         b.Cfg.ServerType,
		Tags:         tagsFromLabels(lease.Server.Labels),
	}
	api.instances = []vultrInstance{live}
	target, err := b.releaseTargetFromClaim("claim-only", "account:test-account")
	if err != nil {
		t.Fatal(err)
	}
	if target.Server.Name != live.Label {
		t.Fatalf("target name=%q want %q", target.Server.Name, live.Label)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: target}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != live.ID {
		t.Fatalf("deleted=%v", api.deleted)
	}
}

func TestReleaseOnlyResolveDoesNotHideLiveAliasAmbiguity(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	tagsA := leaseTags(b.Cfg, "cbx_777777777777", "same-slug", "ready", false, time.Now())
	tagsB := leaseTags(b.Cfg, "cbx_888888888888", "same-slug", "ready", false, time.Now())
	api.instances = []vultrInstance{
		{
			ID:           "77777777-7777-4777-8777-777777777777",
			Label:        core.LeaseProviderName("cbx_777777777777", "same-slug"),
			MainIP:       "203.0.113.70",
			Status:       "active",
			PowerStatus:  "running",
			ServerStatus: "ok",
			Plan:         b.Cfg.ServerType,
			Tags:         tagsA,
		},
		{
			ID:           "88888888-8888-4888-8888-888888888888",
			Label:        core.LeaseProviderName("cbx_888888888888", "same-slug"),
			MainIP:       "203.0.113.71",
			Status:       "active",
			PowerStatus:  "running",
			ServerStatus: "ok",
			Plan:         b.Cfg.ServerType,
			Tags:         tagsB,
		},
	}
	err := core.ClaimLeaseTargetForRepoConfig("cbx_999999999999", "same-slug", b.Cfg, serverFromInstance(api.instances[0], b.Cfg), core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Resolve(context.Background(), core.ResolveRequest{ID: "same-slug", ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "matches multiple active leases") {
		t.Fatalf("err=%v", err)
	}
}

func TestReleaseOnlyResolveRejectsExactNonVultrLeaseID(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	otherCfg := b.Cfg
	otherCfg.Provider = "digitalocean"
	otherServer := core.Server{
		Provider: "digitalocean",
		CloudID:  "123",
		Name:     "other",
		Labels: map[string]string{
			"crabbox":    "true",
			"created_by": "crabbox",
			"provider":   "digitalocean",
			"lease":      "cbx_aaaaaaaaaaaa",
			"slug":       "other",
			"target":     core.TargetLinux,
		},
	}
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_aaaaaaaaaaaa", "other", otherCfg, otherServer, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	labels := core.DirectLeaseLabels(b.Cfg, "cbx_bbbbbbbbbbbb", "cbx_aaaaaaaaaaaa", providerName, "", false, time.Now())
	labels["state"] = "ready"
	labels[vultrAccountLabel] = "account:test-account"
	labels[vultrKeyOwnedLabel] = "false"
	server := core.Server{
		Provider: providerName,
		CloudID:  "99999999-9999-4999-8999-999999999999",
		Name:     core.LeaseProviderName("cbx_bbbbbbbbbbbb", "cbx_aaaaaaaaaaaa"),
		Labels:   labels,
	}
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_bbbbbbbbbbbb", "cbx_aaaaaaaaaaaa", b.Cfg, server, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	_, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "cbx_aaaaaaaaaaaa", ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "exact lease identifier") {
		t.Fatalf("err=%v", err)
	}
}

func TestReleaseCanonicalIdentifierDoesNotFallBackToClaimSlug(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	const (
		requestedID = "cbx_aaaaaaaaaaaa"
		lookalikeID = "cbx_bbbbbbbbbbbb"
	)
	labels := core.DirectLeaseLabels(b.Cfg, lookalikeID, requestedID, providerName, "", false, time.Now())
	labels[vultrAccountLabel] = "account:test-account"
	labels[vultrKeyOwnedLabel] = "false"
	server := core.Server{
		Provider: providerName,
		CloudID:  "99999999-9999-4999-8999-999999999999",
		Name:     core.LeaseProviderName(lookalikeID, requestedID),
		Labels:   labels,
	}
	if err := core.ClaimLeaseTargetForRepoConfig(lookalikeID, requestedID, b.Cfg, server, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}

	_, err := b.Resolve(context.Background(), core.ResolveRequest{ID: requestedID, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "exact lease identifier") {
		t.Fatalf("Resolve release-only err=%v", err)
	}
}

func TestReleaseCanonicalIdentifierRejectsMismatchedProviderScope(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	backend := newTestBackend(t, &fakeVultrAPI{})
	const leaseID = "cbx_abcdef123476"
	labels := core.DirectLeaseLabels(backend.Cfg, leaseID, "wrong-scope", providerName, "", false, time.Now())
	labels[vultrAccountLabel] = "account:test-account"
	labels[vultrKeyOwnedLabel] = "false"
	server := core.Server{Provider: providerName, CloudID: "99999999-9999-4999-8999-999999999476", Name: core.LeaseProviderName(leaseID, "wrong-scope"), Labels: labels}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "wrong-scope", providerName, "account:other", "", t.TempDir(), time.Minute, false, server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	_, err := backend.releaseTargetFromClaim(leaseID, "account:test-account")
	if err == nil || !strings.Contains(err.Error(), "exact lease identifier") {
		t.Fatalf("err=%v", err)
	}
}

func TestTouchAppliesIdleTimeoutOverride(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "touch"})
	if err != nil {
		t.Fatal(err)
	}
	labels := shared.CloneLabels(lease.Server.Labels)
	labels["idle_timeout_secs"] = "3600"
	labels["idle_timeout"] = "1h0m0s"
	tags := tagsFromLabels(labels)
	if err := api.UpdateInstanceTags(context.Background(), lease.Server.CloudID, tags); err != nil {
		t.Fatal(err)
	}
	api.updated = nil
	lease.Server.Labels = labels
	touched, err := b.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "running", IdleTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if touched.Labels["idle_timeout_secs"] != "60" {
		t.Fatalf("labels=%v", touched.Labels)
	}
	if len(api.updated) != 1 || labelsFromTags(api.updated[0].tags)["idle_timeout_secs"] != "60" {
		t.Fatalf("updated=%#v", api.updated)
	}
}

func TestCleanupDryRunDoesNotDelete(t *testing.T) {
	var stderr bytes.Buffer
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	b.RT.Stderr = &stderr
	expired := time.Now().Add(-time.Hour)
	api.instances = []vultrInstance{{
		ID:           "22222222-2222-4222-8222-222222222222",
		Label:        core.LeaseProviderName("cbx_cccccccccccc", "old"),
		MainIP:       "203.0.113.30",
		Status:       "active",
		PowerStatus:  "running",
		ServerStatus: "ok",
		Plan:         b.Cfg.ServerType,
		Tags:         leaseTagsWithExpiry(b.Cfg, "cbx_cccccccccccc", "old", expired),
	}}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if !strings.Contains(stderr.String(), "skip server id=22222222-2222-4222-8222-222222222222") || !strings.Contains(stderr.String(), "reason=no-exact-local-claim") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestCleanupDryRunWithExactClaimDoesNotMutate(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	before, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists {
		t.Fatalf("before=%#v exists=%v err=%v", before, exists, err)
	}
	b.RT.Clock = vultrTestClock{now: time.Now().Add(24 * time.Hour)}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	after, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists || !reflect.DeepEqual(after, before) {
		t.Fatalf("after=%#v exists=%v err=%v before=%#v", after, exists, err, before)
	}
	if len(api.deleted) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("dry-run provider mutations instance=%v key=%v", api.deleted, api.deletedKeys)
	}
}

func TestCleanupDryRunAmbiguousCreateRecoveryDoesNotMutate(t *testing.T) {
	const (
		leaseID = "cbx_dadadadadada"
		slug    = "late-instance"
	)
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	now := time.Now().UTC()
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(b.Cfg, leaseID)
	if err != nil {
		t.Fatal(err)
	}
	api.sshKeys = append(api.sshKeys, vultrSSHKey{ID: "late-instance-key", Name: providerKeyForLease(leaseID), SSHKey: publicKey})
	labels := core.DirectLeaseLabels(b.Cfg, leaseID, slug, providerName, "", false, now.Add(-48*time.Hour))
	labels["state"] = "provisioning"
	labels["recovery"] = "ambiguous-create"
	labels[vultrAccountLabel] = "account:test-account"
	setVultrKeyIdentity(labels, "late-instance-key", true)
	claimServer := core.Server{Provider: providerName, Name: core.LeaseProviderName(leaseID, slug), Labels: labels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, b.Cfg, claimServer, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	api.instances = []vultrInstance{{
		ID:           "dadadada-dada-4ada-8ada-dadadadadada",
		Label:        claimServer.Name,
		MainIP:       "203.0.113.92",
		Status:       "active",
		PowerStatus:  "running",
		ServerStatus: "ok",
		Plan:         b.Cfg.ServerType,
		Tags:         tagsFromLabels(labels),
	}}
	before, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || before.Revision == "" {
		t.Fatalf("before=%#v exists=%v err=%v", before, exists, err)
	}
	b.RT.Clock = vultrTestClock{now: now}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	after, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || after.Revision != before.Revision || !reflect.DeepEqual(after, before) {
		t.Fatalf("after=%#v exists=%v err=%v before=%#v", after, exists, err, before)
	}
	if len(api.created) != 0 || len(api.updated) != 0 || len(api.deleted) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("dry-run provider mutations created=%v updated=%v instance=%v key=%v", api.created, api.updated, api.deleted, api.deletedKeys)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key changed during dry-run: %v", err)
	}
}

func TestCleanupDryRunAmbiguousSSHKeyCreateRecoveryDoesNotMutate(t *testing.T) {
	const (
		leaseID = "cbx_bebebebebebe"
		slug    = "late-key"
	)
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	now := time.Now().UTC()
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(b.Cfg, leaseID)
	if err != nil {
		t.Fatal(err)
	}
	labels := core.DirectLeaseLabels(b.Cfg, leaseID, slug, providerName, "", false, now.Add(-48*time.Hour))
	labels["state"] = "provisioning"
	labels["recovery"] = "ambiguous-key-create"
	labels[vultrAccountLabel] = "account:test-account"
	server := core.Server{Provider: providerName, Name: core.LeaseProviderName(leaseID, slug), Labels: labels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, b.Cfg, server, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	api.sshKeys = append(api.sshKeys, vultrSSHKey{ID: "late-key", Name: providerKeyForLease(leaseID), SSHKey: publicKey})
	before := attachCurrentVultrClaim(t, &server, leaseID)
	if before.Revision == "" {
		t.Fatalf("before=%#v", before)
	}
	b.RT.Clock = vultrTestClock{now: now}
	if err := b.CleanupServers(context.Background(), core.CleanupRequest{DryRun: true}, []core.Server{server}); err != nil {
		t.Fatal(err)
	}
	after, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || after.Revision != before.Revision || !reflect.DeepEqual(after, before) {
		t.Fatalf("after=%#v exists=%v err=%v before=%#v", after, exists, err, before)
	}
	if len(api.created) != 0 || len(api.updated) != 0 || len(api.deleted) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("dry-run provider mutations created=%v updated=%v instance=%v key=%v", api.created, api.updated, api.deleted, api.deletedKeys)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key changed during dry-run: %v", err)
	}
}

func TestCleanupSkipsClaimlessBeforeClaimedInstance(t *testing.T) {
	var stderr bytes.Buffer
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	b.RT.Stderr = &stderr
	expired := time.Now().Add(-time.Hour)
	staleLabels := labelsFromTags(leaseTagsWithExpiry(b.Cfg, "cbx_cccccccccccc", "stale-account", expired))
	claimlessLabels := labelsFromTags(leaseTagsWithExpiry(b.Cfg, "cbx_aaaaaaaaaaaa", "claimless", expired))
	claimlessLabels[vultrAccountLabel] = "account:test-account"
	claimedLabels := labelsFromTags(leaseTagsWithExpiry(b.Cfg, "cbx_bbbbbbbbbbbb", "claimed", expired))
	claimedLabels[vultrAccountLabel] = "account:test-account"
	setVultrKeyIdentity(claimedLabels, "", false)
	api.instances = []vultrInstance{
		{ID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Label: core.LeaseProviderName("cbx_cccccccccccc", "stale-account"), Status: "active", PowerStatus: "running", ServerStatus: "ok", Plan: b.Cfg.ServerType, Tags: tagsFromLabels(staleLabels)},
		{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Label: core.LeaseProviderName("cbx_aaaaaaaaaaaa", "claimless"), Status: "active", PowerStatus: "running", ServerStatus: "ok", Plan: b.Cfg.ServerType, Tags: tagsFromLabels(claimlessLabels)},
		{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Label: core.LeaseProviderName("cbx_bbbbbbbbbbbb", "claimed"), Status: "active", PowerStatus: "running", ServerStatus: "ok", Plan: b.Cfg.ServerType, Tags: tagsFromLabels(claimedLabels)},
	}
	staleServer := serverFromInstance(api.instances[0], b.Cfg)
	staleServer.Labels[vultrAccountLabel] = "account:stale-account"
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_cccccccccccc", "stale-account", b.Cfg, staleServer, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	claimedID := api.instances[2].ID
	claimedServer := serverFromInstance(api.instances[2], b.Cfg)
	claimedServer.Labels[vultrAccountLabel] = "account:test-account"
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_bbbbbbbbbbbb", "claimed", b.Cfg, claimedServer, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != claimedID {
		t.Fatalf("deleted=%v stderr=%q, want only claimed instance", api.deleted, stderr.String())
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("cbx_cccccccccccc", providerName); err != nil || !ok {
		t.Fatalf("stale-account claim after cleanup ok=%v err=%v", ok, err)
	}
}

func TestDoctorUsesNonMutatingInventory(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	result, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != providerName || !strings.Contains(result.Message, "mutation=false") || !strings.Contains(result.Message, "region=ewr") {
		t.Fatalf("result=%#v", result)
	}
	if len(api.created) != 0 || len(api.deleted) != 0 {
		t.Fatalf("doctor mutated: created=%v deleted=%v", api.created, api.deleted)
	}
}

func TestAmbiguousCreatePreservesRecoveryClaimAndKey(t *testing.T) {
	api := &fakeVultrAPI{createErr: &ambiguousInstanceCreateError{err: os.ErrDeadlineExceeded, keyID: "key-ambiguous", keyCreated: true}}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "recover"})
	if err == nil || !strings.Contains(err.Error(), "indeterminate") {
		t.Fatalf("err=%v", err)
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Labels["recovery"] != "ambiguous-create" || claims[0].Labels[vultrKeyIDLabel] != "key-ambiguous" {
		t.Fatalf("claims=%#v", claims)
	}
}

func TestAmbiguousCreateRecoveryWithoutCloudIDDoesNotDropClaimOrKey(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	labels := core.DirectLeaseLabels(b.Cfg, "cbx_dddddddddddd", "recover", providerName, "", false, time.Now())
	labels["state"] = "provisioning"
	labels["recovery"] = "ambiguous-create"
	labels[vultrAccountLabel] = "account:test-account"
	setVultrKeyIdentity(labels, "key-recover", true)
	server := core.Server{Provider: providerName, Name: core.LeaseProviderName("cbx_dddddddddddd", "recover"), Labels: labels}
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_dddddddddddd", "recover", b.Cfg, server, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	attachCurrentVultrClaim(t, &server, "cbx_dddddddddddd")
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: "cbx_dddddddddddd", Server: server}})
	if err == nil || !strings.Contains(err.Error(), "ambiguous create recovery is still indeterminate") {
		t.Fatalf("err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("deleted instance/key: %v %v", api.deleted, api.deletedKeys)
	}
	if _, ok, err := core.ReadLeaseClaimWithPresence("cbx_dddddddddddd"); err != nil || !ok {
		t.Fatalf("claim retained ok=%v err=%v", ok, err)
	}
}

func TestAmbiguousCreateRecoveryDeletesAfterLiveInstanceIsFound(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	_, publicKey, err := core.EnsureTestboxKeyForConfig(b.Cfg, "cbx_eeeeeeeeeeee")
	if err != nil {
		t.Fatal(err)
	}
	api.sshKeys = append(api.sshKeys, vultrSSHKey{ID: "key-found", Name: providerKeyForLease("cbx_eeeeeeeeeeee"), SSHKey: publicKey})
	labels := core.DirectLeaseLabels(b.Cfg, "cbx_eeeeeeeeeeee", "found", providerName, "", false, time.Now())
	labels["state"] = "provisioning"
	labels["recovery"] = "ambiguous-create"
	labels[vultrAccountLabel] = "account:test-account"
	setVultrKeyIdentity(labels, "key-found", true)
	claimServer := core.Server{Provider: providerName, Name: core.LeaseProviderName("cbx_eeeeeeeeeeee", "found"), Labels: labels}
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_eeeeeeeeeeee", "found", b.Cfg, claimServer, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	live := vultrInstance{
		ID:           "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		Label:        core.LeaseProviderName("cbx_eeeeeeeeeeee", "found"),
		MainIP:       "203.0.113.90",
		Status:       "active",
		PowerStatus:  "running",
		ServerStatus: "ok",
		Plan:         b.Cfg.ServerType,
		Tags:         tagsFromLabels(labels),
	}
	api.instances = []vultrInstance{live}
	server := serverFromInstance(live, b.Cfg)
	preserveVultrIdentity(server.Labels, labels)
	before, beforeExists, err := core.ReadLeaseClaimWithPresence("cbx_eeeeeeeeeeee")
	if err != nil || !beforeExists || before.Revision == "" {
		t.Fatalf("before=%#v exists=%v err=%v", before, beforeExists, err)
	}
	prepared, err := b.prepareCleanupServer(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, exists, set := core.ServerLeaseClaimSnapshot(prepared)
	current, currentExists, err := core.ReadLeaseClaimWithPresence("cbx_eeeeeeeeeeee")
	if err != nil || !set || !exists || !currentExists || prepared.CloudID != live.ID || snapshot.CloudID != "" || snapshot.Revision != before.Revision || !reflect.DeepEqual(snapshot, before) || !reflect.DeepEqual(current, before) {
		t.Fatalf("prepared=%#v snapshot=%#v exists=%v set=%v current=%#v currentExists=%v before=%#v err=%v", prepared, snapshot, exists, set, current, currentExists, before, err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: "cbx_eeeeeeeeeeee", Server: prepared}}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != live.ID {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if len(api.deletedKeys) != 1 || api.deletedKeys[0] != "key-found" {
		t.Fatalf("deletedKeys=%v", api.deletedKeys)
	}
	if _, ok, err := core.ReadLeaseClaimWithPresence("cbx_eeeeeeeeeeee"); err != nil || ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
}

func TestAmbiguousSSHKeyCreateRecoveryKeepsOwnershipIndeterminate(t *testing.T) {
	api := &fakeVultrAPI{createErr: &ambiguousSSHKeyCreateError{err: os.ErrDeadlineExceeded}}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "key-recover"})
	if err == nil || !strings.Contains(err.Error(), "SSH-key creation remains indeterminate") {
		t.Fatalf("err=%v", err)
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Labels["recovery"] != "ambiguous-key-create" || claims[0].Labels[vultrKeyOwnedLabel] != "" {
		t.Fatalf("claims=%#v", claims)
	}
	server := core.Server{Provider: providerName, Name: claims[0].Slug, Labels: claims[0].Labels}
	core.SetServerLeaseClaimSnapshot(&server, claims[0], true)
	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: claims[0].LeaseID, Server: server}})
	if err == nil || !strings.Contains(err.Error(), "SSH key creation remains indeterminate") {
		t.Fatalf("release err=%v", err)
	}
	if _, ok, err := core.ReadLeaseClaimWithPresence(claims[0].LeaseID); err != nil || !ok {
		t.Fatalf("claim retained ok=%v err=%v", ok, err)
	}
}

func TestAmbiguousSSHKeyCreateRecoveryDeletesExactDiscoveredKeyUnderFence(t *testing.T) {
	api := &fakeVultrAPI{createErr: &ambiguousSSHKeyCreateError{err: os.ErrDeadlineExceeded}}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "found-key"})
	if err == nil {
		t.Fatal("Acquire unexpectedly succeeded")
	}
	claims, err := core.ListLeaseClaims()
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims=%#v err=%v", claims, err)
	}
	_, publicKey, err := core.EnsureTestboxKeyForConfig(b.Cfg, claims[0].LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	api.sshKeys = append(api.sshKeys, vultrSSHKey{ID: "late-key", Name: providerKeyForLease(claims[0].LeaseID), SSHKey: publicKey})
	target, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "found-key", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists, set := core.ServerLeaseClaimSnapshot(target.Server); !set || !exists {
		t.Fatalf("release target missing exact claim snapshot: %#v", target)
	}
	before, beforeExists, err := core.ReadLeaseClaimWithPresence(claims[0].LeaseID)
	if err != nil || !beforeExists || before.Revision == "" {
		t.Fatalf("before=%#v exists=%v err=%v", before, beforeExists, err)
	}
	prepared, err := b.prepareCleanupServer(context.Background(), target.Server)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, exists, set := core.ServerLeaseClaimSnapshot(prepared)
	current, currentExists, err := core.ReadLeaseClaimWithPresence(claims[0].LeaseID)
	if err != nil || !set || !exists || !currentExists || snapshot.Labels[vultrKeyOwnedLabel] != "" || snapshot.Labels[vultrKeyIDLabel] != "" || snapshot.Revision != before.Revision || !reflect.DeepEqual(snapshot, before) || !reflect.DeepEqual(current, before) {
		t.Fatalf("snapshot=%#v exists=%v set=%v current=%#v currentExists=%v before=%#v err=%v", snapshot, exists, set, current, currentExists, before, err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: target.LeaseID, Server: prepared}}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeys) != 1 || api.deletedKeys[0] != "late-key" {
		t.Fatalf("instance deletes=%v key deletes=%v", api.deleted, api.deletedKeys)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(claims[0].LeaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v", exists, err)
	}
}

func TestAmbiguousCreateRecoveryRefusesMultipleExactMatches(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	const leaseID = "cbx_edededededed"
	const slug = "multiple"
	labels := core.DirectLeaseLabels(b.Cfg, leaseID, slug, providerName, "", false, time.Now())
	labels["state"] = "provisioning"
	labels["recovery"] = "ambiguous-create"
	labels[vultrAccountLabel] = "account:test-account"
	setVultrKeyIdentity(labels, "key-multiple", true)
	claimServer := core.Server{Provider: providerName, Name: core.LeaseProviderName(leaseID, slug), Labels: labels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, b.Cfg, claimServer, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"edededed-eded-4ded-8ded-edededededed", "dededede-dede-4ded-8ded-dededededede"} {
		api.instances = append(api.instances, vultrInstance{ID: id, Label: claimServer.Name, Tags: tagsFromLabels(labels)})
	}
	attachCurrentVultrClaim(t, &claimServer, leaseID)
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: claimServer}})
	if err == nil || !strings.Contains(err.Error(), "multiple instances") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("provider mutations instance=%v key=%v", api.deleted, api.deletedKeys)
	}
}

func TestKeyOnlyRollbackRecoveryDeletesExactKeyWithoutInstanceCall(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	const leaseID = "cbx_abababababab"
	const slug = "key-only-cleanup"
	_, publicKey, err := core.EnsureTestboxKeyForConfig(b.Cfg, leaseID)
	if err != nil {
		t.Fatal(err)
	}
	labels := core.DirectLeaseLabels(b.Cfg, leaseID, slug, providerName, "", false, time.Now())
	labels["state"] = "provisioning"
	labels["recovery"] = "rollback-cleanup"
	labels[vultrAccountLabel] = "account:test-account"
	setVultrKeyIdentity(labels, "rollback-key", true)
	server := core.Server{Provider: providerName, Name: core.LeaseProviderName(leaseID, slug), Labels: labels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, b.Cfg, server, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	api.sshKeys = append(api.sshKeys, vultrSSHKey{ID: "rollback-key", Name: providerKeyForLease(leaseID), SSHKey: publicKey})
	target, err := b.Resolve(context.Background(), core.ResolveRequest{ID: slug, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: target}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeys) != 1 || api.deletedKeys[0] != "rollback-key" {
		t.Fatalf("instance deletes=%v key deletes=%v", api.deleted, api.deletedKeys)
	}
}

func TestSSHKeyRollbackCleanupFailurePreservesRecoveryClaim(t *testing.T) {
	api := &fakeVultrAPI{createErr: &vultrSSHKeyCleanupError{cause: os.ErrInvalid, cleanup: os.ErrPermission, keyID: "key-rollback"}}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "rollback-key"})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err=%v", err)
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Labels["recovery"] != "rollback-cleanup" || claims[0].Labels[vultrKeyIDLabel] != "key-rollback" || claims[0].Labels[vultrKeyOwnedLabel] != "true" {
		t.Fatalf("claims=%#v", claims)
	}
	if keyPath, err := core.TestboxKeyPath(claims[0].LeaseID); err != nil {
		t.Fatal(err)
	} else if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key not retained: %v", err)
	}
}

func TestKeyOnlyRollbackRecoveryCannotDeleteLiveInstance(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	const leaseID = "cbx_fefefefefefe"
	const slug = "key-only"
	labels := core.DirectLeaseLabels(b.Cfg, leaseID, slug, providerName, "", false, time.Now())
	labels["state"] = "provisioning"
	labels["recovery"] = "rollback-cleanup"
	labels[vultrAccountLabel] = "account:test-account"
	setVultrKeyIdentity(labels, "key-only-rollback", true)
	claimServer := core.Server{Provider: providerName, Name: core.LeaseProviderName(leaseID, slug), Labels: labels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, b.Cfg, claimServer, core.SSHTarget{}, t.TempDir(), b.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	live := vultrInstance{
		ID:           "fefefefe-fefe-4efe-8efe-fefefefefefe",
		Label:        core.LeaseProviderName(leaseID, slug),
		MainIP:       "203.0.113.91",
		Status:       "active",
		PowerStatus:  "running",
		ServerStatus: "ok",
		Plan:         b.Cfg.ServerType,
		Tags:         tagsFromLabels(labels),
	}
	liveLabels := labelsFromTags(live.Tags)
	liveLabels["provider_key"] = "changed-provider-key"
	live.Tags = tagsFromLabels(liveLabels)
	api.instances = []vultrInstance{live}
	target, err := b.releaseTargetFromClaim(slug, "account:test-account")
	if err != nil {
		t.Fatal(err)
	}
	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: target})
	if err == nil || !strings.Contains(err.Error(), "key-only recovery claim cannot authorize instance cleanup") {
		t.Fatalf("err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("deleted instance/key: %v %v", api.deleted, api.deletedKeys)
	}
	if _, ok, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || !ok {
		t.Fatalf("claim retained ok=%v err=%v", ok, err)
	}
}

func TestAcquireRejectsSSHCIDRsWithoutFirewallGroup(t *testing.T) {
	api := &fakeVultrAPI{}
	b := newTestBackend(t, api)
	b.Cfg.Vultr.SSHCIDRs = []string{"203.0.113.0/24"}
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "cidrs"})
	if err == nil || !strings.Contains(err.Error(), "requires vultr.firewallGroup") {
		t.Fatalf("err=%v", err)
	}
	if len(api.created) != 0 {
		t.Fatalf("created=%v", api.created)
	}
}

func leaseTagsWithExpiry(cfg core.Config, leaseID, slug string, expiresAt time.Time) []string {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, expiresAt.Add(-time.Hour))
	labels["state"] = "ready"
	labels["expires_at"] = core.LeaseLabelTime(expiresAt)
	return tagsFromLabels(labels)
}

func attachCurrentVultrClaim(t *testing.T, server *core.Server, leaseID string) core.LeaseClaim {
	t.Helper()
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists {
		t.Fatalf("read current claim lease=%s exists=%v err=%v", leaseID, exists, err)
	}
	core.SetServerLeaseClaimSnapshot(server, claim, true)
	return claim
}
