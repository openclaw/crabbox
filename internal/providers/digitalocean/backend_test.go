package digitalocean

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

type fakeDigitalOceanAPI struct {
	droplets       []droplet
	listFn         func() ([]droplet, error)
	accountID      string
	accountErr     error
	accountFn      func() (string, error)
	nextID         int64
	createErr      error
	getErr         error
	getFn          func(context.Context, int64) (droplet, error)
	getCalls       int
	deleteErr      error
	deleteFn       func(context.Context, int64) error
	deleteSawDone  bool
	keyDeleteErr   error
	replaceErr     error
	reuseSSHKey    bool
	created        []droplet
	deleted        []int64
	keyDeleteDone  bool
	deleteKeyOnErr bool
	sshKeys        []sshKey
	findKeyFn      func(string, string) (sshKey, bool, error)
	findKeyByIDFn  func(int64) (sshKey, bool, error)
	findKeyByPubFn func(string) (sshKey, bool, error)
	deletedKeyIDs  []int64
	deletedKeys    []string
	events         []string
	replaced       []int64
	replacedFrom   [][]string
	replacedTo     [][]string
	createRequests []struct {
		cfg     core.Config
		leaseID string
		slug    string
		keep    bool
	}
}

func (f *fakeDigitalOceanAPI) AccountID(context.Context) (string, error) {
	if f.accountFn != nil {
		return f.accountFn()
	}
	if f.accountErr != nil {
		return "", f.accountErr
	}
	if f.accountID != "" {
		return f.accountID, nil
	}
	return "team:test-account", nil
}

func (f *fakeDigitalOceanAPI) ListCrabboxDroplets(context.Context) ([]droplet, error) {
	if f.listFn != nil {
		return f.listFn()
	}
	var out []droplet
	for _, item := range f.droplets {
		if isOwnedDroplet(item) {
			out = append(out, item)
		}
	}
	return out, nil
}

func (f *fakeDigitalOceanAPI) GetDroplet(ctx context.Context, id int64) (droplet, error) {
	f.getCalls++
	if f.getFn != nil {
		return f.getFn(ctx, id)
	}
	if f.getErr != nil {
		return droplet{}, f.getErr
	}
	for _, item := range append(f.droplets, f.created...) {
		if item.ID == id {
			return item, nil
		}
	}
	return droplet{}, &digitalOceanAPIError{Status: 404}
}

func (f *fakeDigitalOceanAPI) CreateDroplet(_ context.Context, cfg core.Config, publicKey, leaseID, slug string, keep bool, now time.Time) (droplet, error) {
	f.createRequests = append(f.createRequests, struct {
		cfg     core.Config
		leaseID string
		slug    string
		keep    bool
	}{cfg: cfg, leaseID: leaseID, slug: slug, keep: keep})
	if f.createErr != nil {
		return droplet{}, f.createErr
	}
	if f.nextID == 0 {
		f.nextID = 100
	}
	item := droplet{
		ID:            f.nextID,
		Name:          core.LeaseProviderName(leaseID, slug),
		Status:        "active",
		Tags:          leaseTags(cfg, leaseID, slug, "provisioning", keep, now),
		SSHKeyID:      700,
		SSHKeyCreated: !f.reuseSSHKey,
	}
	item.Networks.V4 = append(item.Networks.V4, struct {
		IPAddress string `json:"ip_address"`
		Type      string `json:"type"`
	}{IPAddress: "203.0.113.10", Type: "public"})
	item.Size.Slug = cfg.ServerType
	if !f.reuseSSHKey {
		for i := 0; i < len(f.sshKeys); {
			if f.sshKeys[i].ID == 700 {
				f.sshKeys = append(f.sshKeys[:i], f.sshKeys[i+1:]...)
				continue
			}
			i++
		}
		f.sshKeys = append(f.sshKeys, sshKey{ID: 700, Name: providerKeyForLease(leaseID), PublicKey: publicKey})
	}
	f.created = append(f.created, item)
	f.nextID++
	return item, nil
}

func (f *fakeDigitalOceanAPI) DeleteDroplet(ctx context.Context, id int64) error {
	select {
	case <-ctx.Done():
		f.deleteSawDone = true
	default:
	}
	f.deleted = append(f.deleted, id)
	f.events = append(f.events, "droplet")
	if f.deleteFn != nil {
		return f.deleteFn(ctx, id)
	}
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.droplets = removeDropletByID(f.droplets, id)
	f.created = removeDropletByID(f.created, id)
	return nil
}

func (f *fakeDigitalOceanAPI) FindSSHKey(_ context.Context, name, publicKey string) (sshKey, bool, error) {
	if f.findKeyFn != nil {
		return f.findKeyFn(name, publicKey)
	}
	return selectSSHKey(f.sshKeys, name, publicKey)
}

func (f *fakeDigitalOceanAPI) FindSSHKeyByID(_ context.Context, id int64) (sshKey, bool, error) {
	if f.findKeyByIDFn != nil {
		return f.findKeyByIDFn(id)
	}
	for _, key := range f.sshKeys {
		if key.ID == id {
			return key, true, nil
		}
	}
	return sshKey{}, false, nil
}

func (f *fakeDigitalOceanAPI) FindSSHKeyByPublicKey(_ context.Context, publicKey string) (sshKey, bool, error) {
	if f.findKeyByPubFn != nil {
		return f.findKeyByPubFn(publicKey)
	}
	var match sshKey
	matches := 0
	for _, key := range f.sshKeys {
		if strings.TrimSpace(key.PublicKey) == strings.TrimSpace(publicKey) {
			match, matches = key, matches+1
		}
	}
	if matches > 1 {
		return sshKey{}, false, core.Exit(4, "multiple public-key matches")
	}
	return match, matches == 1, nil
}

func (f *fakeDigitalOceanAPI) DeleteSSHKey(ctx context.Context, id int64) error {
	select {
	case <-ctx.Done():
		f.keyDeleteDone = true
	default:
	}
	f.deletedKeyIDs = append(f.deletedKeyIDs, id)
	f.events = append(f.events, "key")
	if f.keyDeleteErr == nil || f.deleteKeyOnErr {
		for i, key := range f.sshKeys {
			if key.ID == id {
				f.sshKeys = append(f.sshKeys[:i], f.sshKeys[i+1:]...)
				break
			}
		}
	}
	return f.keyDeleteErr
}

func (f *fakeDigitalOceanAPI) ReplaceDropletTags(_ context.Context, id int64, currentTags, desiredTags []string) error {
	f.replaced = append(f.replaced, id)
	f.replacedFrom = append(f.replacedFrom, append([]string(nil), currentTags...))
	f.replacedTo = append(f.replacedTo, append([]string(nil), desiredTags...))
	if f.replaceErr != nil {
		return f.replaceErr
	}
	for i := range f.created {
		if f.created[i].ID == id {
			f.created[i].Tags = desiredTags
		}
	}
	for i := range f.droplets {
		if f.droplets[i].ID == id {
			f.droplets[i].Tags = desiredTags
		}
	}
	return nil
}

func removeDropletByID(items []droplet, id int64) []droplet {
	out := items[:0]
	for _, item := range items {
		if item.ID != id {
			out = append(out, item)
		}
	}
	return out
}

func newTestBackend(t *testing.T, api *fakeDigitalOceanAPI) *digitalOceanLeaseBackend {
	t.Helper()
	testutil.IsolateUserDirs(t)
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-1vcpu-1gb"
	cfg.SSHUser = "root"
	cfg.WorkRoot = "/work/crabbox"
	backend := NewDigitalOceanLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*digitalOceanLeaseBackend)
	backend.clientFactory = func(core.Runtime) (digitalOceanAPI, error) { return api, nil }
	backend.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error { return nil }
	return backend
}

func TestAcquireCreatesDropletClaimsLeaseAndMarksReady(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "my-app", Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID == "" || lease.Server.ID != 100 || lease.SSH.Host != "203.0.113.10" || lease.SSH.User != "root" {
		t.Fatalf("lease=%#v", lease)
	}
	if len(api.createRequests) != 1 || api.createRequests[0].slug != "my-app" || !api.createRequests[0].keep {
		t.Fatalf("createRequests=%#v", api.createRequests)
	}
	if len(api.replaced) != 1 || api.replaced[0] != 100 {
		t.Fatalf("replaced=%v", api.replaced)
	}
	if lease.Server.Labels["state"] != "ready" {
		t.Fatalf("labels=%v", lease.Server.Labels)
	}
	if lease.Server.Labels[digitalOceanRecoveryKeyIDLabel] != "700" || lease.Server.Labels[digitalOceanKeyOwnedLabel] != "true" {
		t.Fatalf("key identity labels=%v", lease.Server.Labels)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider("my-app", providerName)
	if err != nil || !ok || claim.Labels[digitalOceanRecoveryKeyIDLabel] != "700" || claim.Labels[digitalOceanKeyOwnedLabel] != "true" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
	from := labelsFromTags(api.replacedFrom[0])
	to := labelsFromTags(api.replacedTo[0])
	if from["created_at"] != to["created_at"] || from["expires_at"] != to["expires_at"] {
		t.Fatalf("ready transition changed lifetime: from=%v to=%v", from, to)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != 700 || len(api.deletedKeys) != 0 {
		t.Fatalf("deletedKeyIDs=%v deletedKeys=%v", api.deletedKeyIDs, api.deletedKeys)
	}
}

func TestSuccessfulAcquireDoesNotDeleteReusedSSHKey(t *testing.T) {
	api := &fakeDigitalOceanAPI{reuseSSHKey: true}
	backend := newTestBackend(t, api)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "reused-key"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Labels[digitalOceanKeyOwnedLabel] != "false" || lease.Server.Labels[digitalOceanRecoveryKeyIDLabel] != "700" {
		t.Fatalf("key identity labels=%v", lease.Server.Labels)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedKeyIDs) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("deletedKeyIDs=%v deletedKeys=%v", api.deletedKeyIDs, api.deletedKeys)
	}
}

func TestReleaseRefusesLeaseWhenClaimIsMissing(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "missing-claim"})
	if err != nil {
		t.Fatal(err)
	}
	core.RemoveLeaseClaim(lease.LeaseID)

	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 {
		t.Fatalf("deleted=%v deletedKeyIDs=%v", api.deleted, api.deletedKeyIDs)
	}
}

func TestPrepareCleanupCarriesExactClaimSnapshot(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "prepared"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := backend.prepareCleanupServer(context.Background(), lease.Server)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, exists, set := core.ServerLeaseClaimSnapshot(prepared)
	current, currentExists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !set || !exists || !currentExists || snapshot.Revision == "" || !reflect.DeepEqual(snapshot, current) {
		t.Fatalf("snapshot=%#v exists=%v set=%v current=%#v currentExists=%v err=%v", snapshot, exists, set, current, currentExists, err)
	}
	if prepared.ProviderMetadata != nil {
		t.Fatalf("ProviderMetadata=%#v, want exact claim snapshot only", prepared.ProviderMetadata)
	}
}

func TestDeleteRefusesChangedClaimBeforeProviderMutation(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "stale"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := backend.prepareCleanupServer(context.Background(), lease.Server)
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
	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: lease.LeaseID, Server: prepared}})
	if err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 {
		t.Fatalf("provider mutations droplet=%v key=%v", api.deleted, api.deletedKeyIDs)
	}
}

func TestClaimUpdateBlocksDuringDigitalOceanProviderAction(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "locked"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := backend.prepareCleanupServer(context.Background(), lease.Server)
	if err != nil {
		t.Fatal(err)
	}
	expected, _, _ := core.ServerLeaseClaimSnapshot(prepared)
	deleteStarted := make(chan struct{})
	allowDelete := make(chan struct{})
	api.deleteFn = func(context.Context, int64) error {
		close(deleteStarted)
		<-allowDelete
		return nil
	}
	releaseDone := make(chan error, 1)
	go func() {
		releaseDone <- backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: lease.LeaseID, Server: prepared}})
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
		t.Fatal("claim update succeeded after the locked provider action removed the claim")
	}
}

func TestReleaseDeletesDropletBeforeManagedSSHKey(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "ordered"})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.events, []string{"droplet", "key"}) {
		t.Fatalf("provider mutation order=%v", api.events)
	}
}

func TestReleaseUsesManagedSSHKeyImmutableIdentityAfterRename(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "renamed-key"})
	if err != nil {
		t.Fatal(err)
	}
	api.sshKeys[0].Name = "renamed-outside-crabbox"
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != 700 {
		t.Fatalf("deletedKeyIDs=%v", api.deletedKeyIDs)
	}
}

func TestConfirmedAbsentDropletFinalizesExactManagedKey(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "absent"})
	if err != nil {
		t.Fatal(err)
	}
	api.created = nil
	api.deleted = nil
	api.deletedKeyIDs = nil
	api.events = nil
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 1 || !reflect.DeepEqual(api.events, []string{"key"}) {
		t.Fatalf("droplet deletes=%v key deletes=%v events=%v", api.deleted, api.deletedKeyIDs, api.events)
	}
}

func TestDeleteRevalidatesAccountInsideClaimLock(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "scope"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	api.accountFn = func() (string, error) {
		calls++
		if calls == 1 {
			return "team:test-account", nil
		}
		return "team:changed", nil
	}
	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
	if err == nil || !strings.Contains(err.Error(), "account mismatch") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if calls != 2 || len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 {
		t.Fatalf("account calls=%d droplet deletes=%v key deletes=%v", calls, api.deleted, api.deletedKeyIDs)
	}
}

func TestReleaseTargetsCarryCurrentExactClaimSnapshot(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	acquired, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshot := func(name string, target core.LeaseTarget) {
		t.Helper()
		snapshot, exists, set := core.ServerLeaseClaimSnapshot(target.Server)
		current, currentExists, err := core.ReadLeaseClaimWithPresence(target.LeaseID)
		if err != nil || !set || !exists || !currentExists || snapshot.Revision == "" || !reflect.DeepEqual(snapshot, current) {
			t.Fatalf("%s snapshot=%#v exists=%v set=%v current=%#v currentExists=%v err=%v", name, snapshot, exists, set, current, currentExists, err)
		}
	}
	assertSnapshot("acquire", acquired)
	visible, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "snapshot", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshot("visible release", visible)
	api.created = nil
	absent, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "snapshot", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshot("claim fallback", absent)
}

func TestAmbiguousCreateRecoveryCASFencesConcurrentClaimChange(t *testing.T) {
	const (
		leaseID = "cbx_acacacacacac"
		slug    = "recovery-cas"
	)
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	backend.Cfg.ProviderKey = providerKeyForLease(leaseID)
	labels := core.DirectLeaseLabels(backend.Cfg, leaseID, slug, providerName, "", false, time.Now())
	item := droplet{ID: 121, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)}
	claimLabels := maps.Clone(labels)
	claimLabels["recovery"] = "ambiguous-create"
	claimLabels[digitalOceanAccountLabel] = "team:test-account"
	claimLabels[digitalOceanKeyOwnedLabel] = "false"
	claimServer := core.Server{Provider: providerName, Name: item.Name, Labels: claimLabels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, backend.Cfg, claimServer, core.SSHTarget{}, t.TempDir(), backend.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	expected, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	server := serverFromDroplet(item, backend.Cfg)
	core.SetServerLeaseClaimSnapshot(&server, expected, true)
	var mutationErr error
	api.listFn = func() ([]droplet, error) {
		replacement := expected
		replacement.Labels = maps.Clone(expected.Labels)
		replacement.Labels["state"] = "running"
		mutationErr = core.ReplaceLeaseClaimIfUnchanged(leaseID, expected, replacement)
		return []droplet{item}, nil
	}
	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}})
	if mutationErr != nil {
		t.Fatalf("concurrent claim mutation: %v", mutationErr)
	}
	if err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 {
		t.Fatalf("provider mutations droplet=%v key=%v", api.deleted, api.deletedKeyIDs)
	}
}

func TestCleanupDryRunAmbiguousCreateRecoveryIsObservational(t *testing.T) {
	const (
		leaseID = "cbx_dadadadadada"
		slug    = "dry-recovery"
	)
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	backend.Cfg.TTL = time.Hour
	backend.Cfg.ProviderKey = providerKeyForLease(leaseID)
	now := time.Now().UTC()
	labels := core.DirectLeaseLabels(backend.Cfg, leaseID, slug, providerName, "", false, now.Add(-48*time.Hour))
	item := droplet{ID: 122, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)}
	claimLabels := maps.Clone(labels)
	claimLabels["recovery"] = "ambiguous-create"
	claimLabels[digitalOceanAccountLabel] = "team:test-account"
	claimLabels[digitalOceanKeyOwnedLabel] = "false"
	claimServer := core.Server{Provider: providerName, Name: item.Name, Labels: claimLabels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, backend.Cfg, claimServer, core.SSHTarget{}, t.TempDir(), backend.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	api.droplets = []droplet{item}
	before, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || before.Revision == "" {
		t.Fatalf("before=%#v exists=%v err=%v", before, exists, err)
	}
	backend.RT.Clock = fixedClock{t: now}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	after, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || !reflect.DeepEqual(after, before) {
		t.Fatalf("after=%#v exists=%v err=%v before=%#v", after, exists, err, before)
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 {
		t.Fatalf("dry-run provider mutations droplet=%v key=%v", api.deleted, api.deletedKeyIDs)
	}
}

func TestCleanupDryRunAmbiguousKeyRecoveryIsObservational(t *testing.T) {
	const (
		leaseID = "cbx_bebebebebebe"
		slug    = "dry-key-recovery"
	)
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	backend.Cfg.TTL = time.Hour
	now := time.Now().UTC()
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(backend.Cfg, leaseID)
	if err != nil {
		t.Fatal(err)
	}
	labels := core.DirectLeaseLabels(backend.Cfg, leaseID, slug, providerName, "", false, now.Add(-48*time.Hour))
	labels["recovery"] = "ambiguous-key-create"
	labels[digitalOceanAccountLabel] = "team:test-account"
	server := core.Server{Provider: providerName, Name: core.LeaseProviderName(leaseID, slug), Labels: labels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, backend.Cfg, server, core.SSHTarget{}, t.TempDir(), backend.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	api.sshKeys = []sshKey{{ID: 123, Name: providerKeyForLease(leaseID), PublicKey: publicKey}}
	before, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || before.Revision == "" {
		t.Fatalf("before=%#v exists=%v err=%v", before, exists, err)
	}
	backend.RT.Clock = fixedClock{t: now}
	prepared, err := backend.prepareCleanupServer(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Labels[digitalOceanRecoveryKeyIDLabel] != "" || prepared.Labels[digitalOceanKeyOwnedLabel] != "" {
		t.Fatalf("read-only preparation exposed unpersisted key identity: %#v", prepared.Labels)
	}
	if err := backend.CleanupServers(context.Background(), core.CleanupRequest{DryRun: true}, []core.Server{server}); err != nil {
		t.Fatal(err)
	}
	after, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || !reflect.DeepEqual(after, before) {
		t.Fatalf("after=%#v exists=%v err=%v before=%#v", after, exists, err, before)
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 {
		t.Fatalf("dry-run provider mutations droplet=%v key=%v", api.deleted, api.deletedKeyIDs)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key changed during dry-run: %v", err)
	}
}

func TestDoctorRequiresDigitalOceanAccountRead(t *testing.T) {
	api := &fakeDigitalOceanAPI{accountErr: errors.New("account scope denied")}
	backend := newTestBackend(t, api)

	_, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err == nil || !strings.Contains(err.Error(), "account scope denied") {
		t.Fatalf("Doctor err=%v", err)
	}
}

func TestAcquireRollsBackDropletAndKeyOnSSHFailure(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	backend.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error { return errors.New("ssh no") }
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "rollback"})
	if err == nil || !strings.Contains(err.Error(), "ssh no") {
		t.Fatalf("Acquire err=%v", err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 100 {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != 700 || len(api.deletedKeys) != 0 {
		t.Fatalf("deletedKeyIDs=%v deletedKeys=%v", api.deletedKeyIDs, api.deletedKeys)
	}
	keyPath, err := core.TestboxKeyPath(api.createRequests[0].leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local key still exists after successful rollback: %v", err)
	}
}

func TestAcquireRetainsLocalKeyWhenRollbackFails(t *testing.T) {
	api := &fakeDigitalOceanAPI{
		keyDeleteErr: errors.New("key cleanup failed"),
	}
	backend := newTestBackend(t, api)
	backend.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
		return core.Exit(5, "timed out waiting for SSH")
	}

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "rollback-fail"})
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for SSH") || !strings.Contains(err.Error(), "key cleanup failed") {
		t.Fatalf("Acquire err=%v", err)
	}
	if len(api.createRequests) != 1 {
		t.Fatalf("createRequests=%d, want no retry after rollback failure", len(api.createRequests))
	}
	keyPath, pathErr := core.TestboxKeyPath(api.createRequests[0].leaseID)
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("local key removed after failed rollback: %v", statErr)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("rollback-fail", providerName)
	if claimErr != nil || !ok || claim.LeaseID != api.createRequests[0].leaseID || claim.CloudID != "100" {
		t.Fatalf("cleanup claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}

	api.keyDeleteErr = nil
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "rollback-fail", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 100 || len(api.deletedKeyIDs) != 2 || len(api.deletedKeys) != 0 {
		t.Fatalf("deleted=%v deletedKeyIDs=%v deletedKeys=%v", api.deleted, api.deletedKeyIDs, api.deletedKeys)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("rollback-fail", providerName); err != nil || ok {
		t.Fatalf("claim after retry ok=%v err=%v", ok, err)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("local key retained after cleanup retry: %v", statErr)
	}
}

func TestRollbackRetryRefusesReplacedManagedKey(t *testing.T) {
	api := &fakeDigitalOceanAPI{
		deleteErr:      errors.New("droplet cleanup failed"),
		keyDeleteErr:   errors.New("lost key delete response"),
		deleteKeyOnErr: true,
		sshKeys: []sshKey{{
			ID:        700,
			Name:      "original",
			PublicKey: "ssh-ed25519 original",
		}},
	}
	backend := newTestBackend(t, api)
	backend.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
		return core.Exit(5, "timed out waiting for SSH")
	}

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "live-rollback"})
	if err == nil || !strings.Contains(err.Error(), "droplet cleanup failed") || !strings.Contains(err.Error(), "lost key delete response") {
		t.Fatalf("Acquire err=%v", err)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("live-rollback", providerName)
	if claimErr != nil || !ok || claim.CloudID != "100" || claim.Labels[digitalOceanRecoveryKeyIDLabel] != "700" {
		t.Fatalf("cleanup claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
	if len(api.created) != 1 || len(api.sshKeys) != 0 {
		t.Fatalf("created=%v sshKeys=%v", api.created, api.sshKeys)
	}

	api.deleteErr = nil
	api.keyDeleteErr = nil
	api.sshKeys = []sshKey{{
		ID:        701,
		Name:      providerKeyForLease(claim.LeaseID),
		PublicKey: "ssh-ed25519 replacement",
	}}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "live-rollback", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Labels[digitalOceanRecoveryKeyIDLabel] != "700" || lease.Server.Labels[digitalOceanKeyOwnedLabel] != "true" {
		t.Fatalf("live Droplet key identity labels=%v", lease.Server.Labels)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "different public key") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != 700 || len(api.deletedKeys) != 0 {
		t.Fatalf("deletedKeyIDs=%v deletedKeys=%v", api.deletedKeyIDs, api.deletedKeys)
	}
	if len(api.sshKeys) != 1 || api.sshKeys[0].ID != 701 {
		t.Fatalf("replacement sshKeys=%v", api.sshKeys)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("live-rollback", providerName); err != nil || !ok {
		t.Fatalf("retained claim ok=%v err=%v", ok, err)
	}
}

func TestRollbackRetryPreservesReusedKeyNonOwnership(t *testing.T) {
	api := &fakeDigitalOceanAPI{
		deleteErr:   errors.New("droplet cleanup failed"),
		reuseSSHKey: true,
		sshKeys: []sshKey{{
			ID:        700,
			Name:      "pre-existing",
			PublicKey: "ssh-ed25519 pre-existing",
		}},
	}
	backend := newTestBackend(t, api)
	backend.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
		return core.Exit(5, "timed out waiting for SSH")
	}

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "reused-rollback"})
	if err == nil || !strings.Contains(err.Error(), "droplet cleanup failed") {
		t.Fatalf("Acquire err=%v", err)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("reused-rollback", providerName)
	if claimErr != nil || !ok || claim.Labels[digitalOceanRecoveryKeyIDLabel] != "700" ||
		claim.Labels[digitalOceanKeyOwnedLabel] != "false" {
		t.Fatalf("cleanup claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}

	api.deleteErr = nil
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "reused-rollback", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedKeyIDs) != 0 || len(api.sshKeys) != 1 || api.sshKeys[0].ID != 700 {
		t.Fatalf("deletedKeyIDs=%v sshKeys=%v", api.deletedKeyIDs, api.sshKeys)
	}
}

func TestAcquireRollsBackOriginalDropletWhenWaitLookupFails(t *testing.T) {
	api := &fakeDigitalOceanAPI{getErr: errors.New("droplet lookup failed")}
	backend := newTestBackend(t, api)

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "lookup-fail"})
	if err == nil || !strings.Contains(err.Error(), "droplet lookup failed") {
		t.Fatalf("Acquire err=%v", err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 100 {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != 700 || len(api.deletedKeys) != 0 {
		t.Fatalf("deletedKeyIDs=%v deletedKeys=%v", api.deletedKeyIDs, api.deletedKeys)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("lookup-fail", providerName); err != nil || ok {
		t.Fatalf("claim after successful rollback ok=%v err=%v", ok, err)
	}
}

func TestAcquirePersistsKeyOnlyCleanupClaim(t *testing.T) {
	api := &fakeDigitalOceanAPI{
		createErr: &sshKeyCleanupError{
			cause:   errors.New("droplet create failed"),
			cleanup: errors.New("initial key cleanup failed"),
			keyID:   701,
		},
		keyDeleteErr: errors.New("key cleanup failed"),
	}
	backend := newTestBackend(t, api)

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "key-only"})
	if err == nil || !strings.Contains(err.Error(), "droplet create failed") || !strings.Contains(err.Error(), "key cleanup failed") {
		t.Fatalf("Acquire err=%v", err)
	}
	if len(api.createRequests) != 1 || len(api.deleted) != 0 || len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != 701 || len(api.deletedKeys) != 0 {
		t.Fatalf("createRequests=%d deleted=%v deletedKeyIDs=%v deletedKeys=%v", len(api.createRequests), api.deleted, api.deletedKeyIDs, api.deletedKeys)
	}
	leaseID := api.createRequests[0].leaseID
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("key-only", providerName)
	if claimErr != nil || !ok || claim.CloudID != "" || claim.Labels["recovery"] != "rollback-cleanup" ||
		claim.Labels[digitalOceanAccountLabel] != "team:test-account" ||
		claim.Labels[digitalOceanRecoveryKeyIDLabel] != "701" {
		t.Fatalf("cleanup claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
	keyPath, pathErr := core.TestboxKeyPath(leaseID)
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("local key removed after failed key cleanup: %v", statErr)
	}
	publicKey, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	api.sshKeys = []sshKey{{ID: 701, Name: providerKeyForLease(leaseID), PublicKey: strings.TrimSpace(string(publicKey))}}

	api.keyDeleteErr = nil
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "key-only", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.ID != 0 || lease.LeaseID != leaseID {
		t.Fatalf("key-only lease=%#v", lease)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 2 || api.deletedKeyIDs[1] != 701 || len(api.deletedKeys) != 0 {
		t.Fatalf("deleted=%v deletedKeyIDs=%v deletedKeys=%v", api.deleted, api.deletedKeyIDs, api.deletedKeys)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("key-only", providerName); err != nil || ok {
		t.Fatalf("claim after key cleanup ok=%v err=%v", ok, err)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("local key retained after key cleanup: %v", statErr)
	}
}

func TestAcquireDoesNotDeleteUnownedKeyOnCreatePreflightFailure(t *testing.T) {
	api := &fakeDigitalOceanAPI{
		createErr: core.Exit(3, `digitalocean ssh key "existing" exists with different public key`),
	}
	backend := newTestBackend(t, api)

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "preflight"})
	if err == nil || !strings.Contains(err.Error(), "different public key") {
		t.Fatalf("Acquire err=%v", err)
	}
	if len(api.createRequests) != 1 || len(api.deleted) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("createRequests=%d deleted=%v deletedKeys=%v", len(api.createRequests), api.deleted, api.deletedKeys)
	}
	leaseID := api.createRequests[0].leaseID
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("preflight", providerName); claimErr != nil || ok {
		t.Fatalf("cleanup claim ok=%v err=%v", ok, claimErr)
	}
	keyPath, pathErr := core.TestboxKeyPath(leaseID)
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("local key retained after definitive preflight failure: %v", statErr)
	}
}

func TestAcquireRetainsAmbiguousSSHKeyUntilVisibleForCleanup(t *testing.T) {
	api := &fakeDigitalOceanAPI{
		createErr: &ambiguousSSHKeyCreateError{err: errors.New("key reconciliation timed out")},
	}
	backend := newTestBackend(t, api)

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "ambiguous-key"})
	var ambiguous *ambiguousSSHKeyCreateError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("Acquire err=%v, want ambiguousSSHKeyCreateError", err)
	}
	if len(api.createRequests) != 1 || len(api.deletedKeys) != 0 {
		t.Fatalf("createRequests=%d deletedKeys=%v", len(api.createRequests), api.deletedKeys)
	}
	leaseID := api.createRequests[0].leaseID
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("ambiguous-key", providerName)
	if claimErr != nil || !ok || claim.CloudID != "" || claim.Labels["recovery"] != "ambiguous-key-create" {
		t.Fatalf("recovery claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
	keyPath, pathErr := core.TestboxKeyPath(leaseID)
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	publicKey, readErr := os.ReadFile(keyPath + ".pub")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if _, resolveErr := backend.Resolve(context.Background(), core.ResolveRequest{ID: "ambiguous-key", ReleaseOnly: true}); resolveErr == nil || !strings.Contains(resolveErr.Error(), "still pending") {
		t.Fatalf("immediate recovery resolve err=%v", resolveErr)
	}
	createdAt, parseErr := strconv.ParseInt(claim.Labels["created_at"], 10, 64)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	backend.RT.Clock = fixedClock{t: time.Unix(createdAt, 0).Add(ambiguousCreateRecoveryGrace + time.Second)}
	backend.recoveryReconcilePolls = 2
	backend.recoveryReconcileInterval = time.Nanosecond
	if _, resolveErr := backend.Resolve(context.Background(), core.ResolveRequest{ID: "ambiguous-key", ReleaseOnly: true}); resolveErr == nil || !strings.Contains(resolveErr.Error(), "remains indeterminate") {
		t.Fatalf("hidden key recovery resolve err=%v", resolveErr)
	}
	if len(api.deletedKeys) != 0 {
		t.Fatalf("hidden key deletedKeys=%v", api.deletedKeys)
	}

	api.sshKeys = []sshKey{{
		ID:        122,
		Name:      providerKeyForLease(leaseID),
		PublicKey: "ssh-ed25519 different",
	}}
	if _, resolveErr := backend.Resolve(context.Background(), core.ResolveRequest{ID: "ambiguous-key", ReleaseOnly: true}); resolveErr == nil || !strings.Contains(resolveErr.Error(), "different public key") {
		t.Fatalf("mismatched key recovery resolve err=%v", resolveErr)
	}
	if len(api.deletedKeys) != 0 {
		t.Fatalf("mismatched key deletedKeys=%v", api.deletedKeys)
	}

	api.sshKeys = []sshKey{
		{
			ID:        122,
			Name:      providerKeyForLease(leaseID),
			PublicKey: "ssh-ed25519 different",
		},
		{
			ID:        123,
			Name:      providerKeyForLease(leaseID),
			PublicKey: strings.TrimSpace(string(publicKey)),
		},
	}
	lease, resolveErr := backend.Resolve(context.Background(), core.ResolveRequest{ID: "ambiguous-key", ReleaseOnly: true})
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	claim, ok, claimErr = core.ResolveLeaseClaimForProvider("ambiguous-key", providerName)
	if claimErr != nil || !ok || claim.Labels[digitalOceanRecoveryKeyIDLabel] != "123" {
		t.Fatalf("recovery claim after key validation=%#v ok=%v err=%v", claim, ok, claimErr)
	}
	if snapshot, exists, set := core.ServerLeaseClaimSnapshot(lease.Server); !set || !exists || !reflect.DeepEqual(snapshot, claim) {
		t.Fatalf("recovered key snapshot=%#v exists=%v set=%v claim=%#v", snapshot, exists, set, claim)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != 123 || len(api.deletedKeys) != 0 ||
		len(api.sshKeys) != 1 || api.sshKeys[0].ID != 122 {
		t.Fatalf("deletedKeyIDs=%v deletedKeys=%v sshKeys=%v", api.deletedKeyIDs, api.deletedKeys, api.sshKeys)
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("ambiguous-key", providerName); claimErr != nil || ok {
		t.Fatalf("claim after cleanup ok=%v err=%v", ok, claimErr)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("local key retained after cleanup: %v", statErr)
	}
}

func TestAmbiguousSSHKeyRecoveryRefusesReplacement(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	cfg := backend.Cfg
	leaseID := "cbx_abcdef123456"
	slug := "key-race"
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		t.Fatal(err)
	}
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now().Add(-ambiguousCreateRecoveryGrace-time.Minute))
	labels["state"] = "provisioning"
	labels["recovery"] = "ambiguous-key-create"
	labels[digitalOceanAccountLabel] = "team:test-account"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, core.Server{
		Provider: providerName,
		Name:     core.LeaseProviderName(leaseID, slug),
		Labels:   labels,
	}, core.SSHTarget{}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	api.sshKeys = []sshKey{{ID: 123, Name: providerKeyForLease(leaseID), PublicKey: publicKey}}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	api.sshKeys = []sshKey{{ID: 124, Name: providerKeyForLease(leaseID), PublicKey: "ssh-ed25519 replacement"}}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "different public key") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deletedKeyIDs) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("deletedKeyIDs=%v deletedKeys=%v", api.deletedKeyIDs, api.deletedKeys)
	}
	if len(api.sshKeys) != 1 || api.sshKeys[0].ID != 124 {
		t.Fatalf("replacement sshKeys=%v", api.sshKeys)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("local key removed after refused replacement: %v", statErr)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName); err != nil || !ok {
		t.Fatalf("claim after refused replacement ok=%v err=%v", ok, err)
	}
}

func TestAmbiguousSSHKeyRecoveryRetriesExactIDAfterLostDeleteResponse(t *testing.T) {
	api := &fakeDigitalOceanAPI{
		keyDeleteErr:   errors.New("lost delete response"),
		deleteKeyOnErr: true,
	}
	backend := newTestBackend(t, api)
	cfg := backend.Cfg
	leaseID := "cbx_abcdef123455"
	slug := "key-delete-retry"
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		t.Fatal(err)
	}
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now().Add(-ambiguousCreateRecoveryGrace-time.Minute))
	labels["state"] = "provisioning"
	labels["recovery"] = "ambiguous-key-create"
	labels[digitalOceanAccountLabel] = "team:test-account"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, core.Server{
		Provider: providerName,
		Name:     core.LeaseProviderName(leaseID, slug),
		Labels:   labels,
	}, core.SSHTarget{}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	api.sshKeys = []sshKey{{ID: 125, Name: providerKeyForLease(leaseID), PublicKey: publicKey}}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "lost delete response") {
		t.Fatalf("first ReleaseLease err=%v", err)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName)
	if err != nil || !ok || claim.Labels[digitalOceanRecoveryKeyIDLabel] != "125" || len(api.sshKeys) != 0 {
		t.Fatalf("retained claim=%#v ok=%v sshKeys=%v err=%v", claim, ok, api.sshKeys, err)
	}

	api.keyDeleteErr = nil
	retry, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: retry}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != 125 {
		t.Fatalf("deletedKeyIDs=%v", api.deletedKeyIDs)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName); err != nil || ok {
		t.Fatalf("claim after retry ok=%v err=%v", ok, err)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("local key retained after retry: %v", statErr)
	}
}

func TestAcquireRetainsCredentialsWhenDropletCreationIsAmbiguous(t *testing.T) {
	api := &fakeDigitalOceanAPI{
		createErr: &ambiguousDropletCreateError{
			err:               errors.New("create reconciliation timed out"),
			keyID:             702,
			keyCreated:        true,
			keyOwnershipKnown: true,
		},
	}
	backend := newTestBackend(t, api)

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "ambiguous"})
	var ambiguous *ambiguousDropletCreateError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("Acquire err=%v, want ambiguousDropletCreateError", err)
	}
	if len(api.createRequests) != 1 || len(api.deletedKeys) != 0 {
		t.Fatalf("createRequests=%d deletedKeys=%v", len(api.createRequests), api.deletedKeys)
	}
	keyPath, pathErr := core.TestboxKeyPath(api.createRequests[0].leaseID)
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("retained key stat: %v", statErr)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("ambiguous", providerName)
	if claimErr != nil || !ok || claim.LeaseID != api.createRequests[0].leaseID || claim.CloudID != "" ||
		claim.Labels[digitalOceanRecoveryKeyIDLabel] != "702" ||
		claim.Labels[digitalOceanKeyOwnedLabel] != "true" {
		t.Fatalf("recovery claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
	if _, resolveErr := backend.Resolve(context.Background(), core.ResolveRequest{ID: "ambiguous", ReleaseOnly: true}); resolveErr == nil || !strings.Contains(resolveErr.Error(), "still pending") {
		t.Fatalf("immediate recovery resolve err=%v", resolveErr)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("ambiguous", providerName); err != nil || !ok {
		t.Fatalf("pending recovery claim ok=%v err=%v", ok, err)
	}
	createdAt, parseErr := strconv.ParseInt(claim.Labels["created_at"], 10, 64)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	backend.RT.Clock = fixedClock{t: time.Unix(createdAt, 0).Add(ambiguousCreateRecoveryGrace + time.Second)}
	backend.recoveryReconcilePolls = 2
	backend.recoveryReconcileInterval = time.Nanosecond
	if _, resolveErr := backend.Resolve(context.Background(), core.ResolveRequest{ID: "ambiguous", ReleaseOnly: true}); resolveErr == nil || !strings.Contains(resolveErr.Error(), "remains indeterminate") {
		t.Fatalf("aged recovery resolve err=%v", resolveErr)
	}
	if len(api.deletedKeys) != 0 {
		t.Fatalf("deletedKeys=%v", api.deletedKeys)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("ambiguous", providerName); err != nil || !ok {
		t.Fatalf("retained recovery claim ok=%v err=%v", ok, err)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("retained key after recovery retry: %v", statErr)
	}
}

func TestResolvePendingRecoveryFindsLateDroplet(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123456"
	slug := "late"
	createdAt := time.Now().Add(-ambiguousCreateRecoveryGrace - time.Minute)
	cfg.ProviderKey = providerKeyForLease(leaseID)
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, createdAt)
	labels["state"] = "provisioning"
	labels["recovery"] = "ambiguous-create"
	labels[digitalOceanAccountLabel] = "team:test-account"
	labels[digitalOceanRecoveryKeyIDLabel] = "126"
	labels[digitalOceanKeyOwnedLabel] = "true"
	item := droplet{ID: 106, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)}
	listCalls := 0
	api := &fakeDigitalOceanAPI{}
	api.listFn = func() ([]droplet, error) {
		if len(api.deleted) > 0 {
			return nil, nil
		}
		listCalls++
		if listCalls < 3 {
			return nil, nil
		}
		return api.droplets, nil
	}
	api.droplets = []droplet{item}
	backend := newTestBackend(t, api)
	keyPath := writeStoredTestboxKey(t, leaseID)
	api.sshKeys = []sshKey{{ID: 126, Name: providerKeyForLease(leaseID), PublicKey: "ssh-ed25519 test-key"}}
	backend.RT.Clock = fixedClock{t: time.Now()}
	backend.recoveryReconcilePolls = 3
	backend.recoveryReconcileInterval = time.Nanosecond
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, core.Server{
		Provider: providerName,
		Name:     item.Name,
		Labels:   labels,
	}, core.SSHTarget{}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.ID != item.ID || lease.LeaseID != leaseID || listCalls != 3 {
		t.Fatalf("lease=%#v listCalls=%d", lease, listCalls)
	}
	if lease.Server.Labels[digitalOceanRecoveryKeyIDLabel] != "126" || lease.Server.Labels[digitalOceanKeyOwnedLabel] != "true" {
		t.Fatalf("lease key identity labels=%v", lease.Server.Labels)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName)
	if err != nil || !ok || claim.CloudID != strconv.FormatInt(item.ID, 10) {
		t.Fatalf("recovered claim=%#v ok=%v err=%v", claim, ok, err)
	}
	if snapshot, exists, set := core.ServerLeaseClaimSnapshot(lease.Server); !set || !exists || !reflect.DeepEqual(snapshot, claim) {
		t.Fatalf("recovered Droplet snapshot=%#v exists=%v set=%v claim=%#v", snapshot, exists, set, claim)
	}

	api.keyDeleteErr = errors.New("key cleanup failed")
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "key cleanup failed") {
		t.Fatalf("first ReleaseLease err=%v", err)
	}
	claim, ok, err = core.ResolveLeaseClaimForProvider(slug, providerName)
	if err != nil || !ok || claim.CloudID != strconv.FormatInt(item.ID, 10) {
		t.Fatalf("retained claim=%#v ok=%v err=%v", claim, ok, err)
	}
	staleServer := serverFromDroplet(item, backend.Cfg)
	staleServer.Labels[digitalOceanAccountLabel] = "team:test-account"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, backend.Cfg, staleServer, core.SSHTarget{}, t.TempDir(), 0, true); err != nil {
		t.Fatal(err)
	}
	claim, ok, err = core.ResolveLeaseClaimForProvider(slug, providerName)
	if err != nil || !ok || claim.Labels[digitalOceanRecoveryKeyIDLabel] != "126" ||
		claim.Labels[digitalOceanKeyOwnedLabel] != "true" {
		t.Fatalf("guarded workflow rewrite claim=%#v ok=%v err=%v", claim, ok, err)
	}

	api.keyDeleteErr = nil
	retry, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if retry.Server.ID != item.ID || retry.Server.CloudID != strconv.FormatInt(item.ID, 10) {
		t.Fatalf("retry lease=%#v", retry)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: retry}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedKeyIDs) != 2 || api.deletedKeyIDs[0] != 126 || api.deletedKeyIDs[1] != 126 || len(api.deletedKeys) != 0 {
		t.Fatalf("deletedKeyIDs=%v deletedKeys=%v", api.deletedKeyIDs, api.deletedKeys)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName); err != nil || ok {
		t.Fatalf("claim after retry ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stored key still exists after retry: %v", err)
	}
}

func TestResolvePendingRecoveryPersistsDropletFoundInInitialInventory(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123457"
	slug := "already-visible"
	createdAt := time.Now().Add(-ambiguousCreateRecoveryGrace - time.Minute)
	cfg.ProviderKey = providerKeyForLease(leaseID)
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, createdAt)
	labels["state"] = "provisioning"
	labels["recovery"] = "ambiguous-create"
	labels[digitalOceanAccountLabel] = "team:test-account"
	item := droplet{ID: 107, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, core.Server{
		Provider: providerName,
		Name:     item.Name,
		Labels:   labels,
	}, core.SSHTarget{}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.ID != item.ID || lease.LeaseID != leaseID {
		t.Fatalf("lease=%#v", lease)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName)
	if err != nil || !ok || claim.CloudID != strconv.FormatInt(item.ID, 10) {
		t.Fatalf("recovered claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestResolveVisibleDropletIgnoresUnrelatedCorruptClaim(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123458"
	slug := "visible"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	item := droplet{ID: 108, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	claimsDir := filepath.Join(stateDir, "claims")
	if err := os.MkdirAll(claimsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claimsDir, "cbx_unrelated.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, Repo: core.Repo{Root: t.TempDir()}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.ID != item.ID || lease.LeaseID != leaseID {
		t.Fatalf("lease=%#v", lease)
	}
}

func TestResolveReadOnlyDoesNotClaimVisibleDroplet(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123459"
	slug := "read-only"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	item := droplet{ID: 109, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)}
	backend := newTestBackend(t, &fakeDigitalOceanAPI{droplets: []droplet{item}})

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, Repo: core.Repo{Root: t.TempDir()}, NoLocalStateMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.Server.ID != item.ID {
		t.Fatalf("lease=%#v", lease)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("read-only resolve wrote claim: exists=%v err=%v", exists, err)
	}
}

func TestResolveReadOnlyIgnoresStaleDropletClaim(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef12345a"
	slug := "stale-read-only"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	item := droplet{ID: 110, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)}
	backend := newTestBackend(t, &fakeDigitalOceanAPI{droplets: []droplet{item}})
	labels[digitalOceanAccountLabel] = "team:test-account"
	claimServer := core.Server{Provider: providerName, CloudID: "999", ID: 999, Name: item.Name, Labels: labels}
	if err := core.ClaimLeaseTargetForConfig(leaseID, slug, backend.Cfg, claimServer, core.SSHTarget{}, backend.Cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, NoLocalStateMutations: true}); err == nil || !strings.Contains(err.Error(), "stale local claim") {
		t.Fatalf("identity-bound resolve err=%v", err)
	}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, StatusOnly: true, NoLocalStateMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.Server.ID != item.ID {
		t.Fatalf("lease=%#v", lease)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || claim.CloudID != "999" {
		t.Fatalf("read-only resolve changed stale claim: claim=%#v exists=%v err=%v", claim, exists, err)
	}
}

func TestStatusTouchClaimRequiresMatchingAccount(t *testing.T) {
	backend := &digitalOceanLeaseBackend{}
	claim := core.LeaseClaim{Labels: map[string]string{digitalOceanAccountLabel: "team:account-a"}}
	lease := core.LeaseTarget{Server: core.Server{Labels: map[string]string{digitalOceanAccountLabel: "team:account-a"}}}
	if !backend.StatusTouchClaimMatches(lease, claim) {
		t.Fatal("matching account identity was rejected")
	}
	lease.Server.Labels[digitalOceanAccountLabel] = "team:account-b"
	if backend.StatusTouchClaimMatches(lease, claim) {
		t.Fatal("mismatched account identity was accepted")
	}
	delete(claim.Labels, digitalOceanAccountLabel)
	if backend.StatusTouchClaimMatches(lease, claim) {
		t.Fatal("missing claim account identity was accepted")
	}
}

func TestResolveNumericIdentifierPrefersDropletIDOverSlug(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	idLabels := core.DirectLeaseLabels(cfg, "cbx_abcdef123420", "id-match", providerName, "", false, time.Now())
	slugLabels := core.DirectLeaseLabels(cfg, "cbx_abcdef123421", "105", providerName, "", false, time.Now())
	api := &fakeDigitalOceanAPI{droplets: []droplet{
		{ID: 105, Name: core.LeaseProviderName("cbx_abcdef123420", "id-match"), Status: "active", Tags: tagsFromLabels(idLabels)},
		{ID: 106, Name: core.LeaseProviderName("cbx_abcdef123421", "105"), Status: "active", Tags: tagsFromLabels(slugLabels)},
	}}
	backend := newTestBackend(t, api)
	repoRoot := t.TempDir()

	for _, identifier := range []string{"105", " 105 ", "+105"} {
		lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: identifier, Repo: core.Repo{Root: repoRoot}, Reclaim: true})
		if err != nil {
			t.Fatalf("Resolve(%q) err=%v", identifier, err)
		}
		if lease.Server.ID != 105 || lease.LeaseID != "cbx_abcdef123420" {
			t.Fatalf("Resolve(%q)=%#v", identifier, lease)
		}
	}
}

func TestReleaseNumericIdentifierPrefersClaimCloudIDOverSlug(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	for _, tc := range []struct {
		leaseID string
		slug    string
		cloudID string
	}{
		{leaseID: "cbx_abcdef123422", slug: "cloud-id-match", cloudID: "105"},
		{leaseID: "cbx_abcdef123423", slug: "105", cloudID: "106"},
	} {
		labels := core.DirectLeaseLabels(cfg, tc.leaseID, tc.slug, providerName, "", false, time.Now())
		labels[digitalOceanAccountLabel] = "team:test-account"
		if err := core.ClaimLeaseTargetForRepoConfig(tc.leaseID, tc.slug, cfg, core.Server{
			Provider: providerName,
			CloudID:  tc.cloudID,
			Name:     core.LeaseProviderName(tc.leaseID, tc.slug),
			Labels:   labels,
		}, core.SSHTarget{}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
			t.Fatal(err)
		}
	}

	for _, identifier := range []string{"105", "00105", "+105", " 105 "} {
		lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: identifier, ReleaseOnly: true})
		if err != nil {
			t.Fatalf("Resolve(%q) err=%v", identifier, err)
		}
		if lease.LeaseID != "cbx_abcdef123422" || lease.Server.CloudID != "105" {
			t.Fatalf("Resolve(%q)=%#v", identifier, lease)
		}
	}
}

func TestReleaseCanonicalIdentifierDoesNotFallBackToClaimSlug(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	const (
		requestedID = "cbx_aaaaaaaaaaaa"
		lookalikeID = "cbx_bbbbbbbbbbbb"
	)
	labels := core.DirectLeaseLabels(backend.Cfg, lookalikeID, requestedID, providerName, "", false, time.Now())
	labels[digitalOceanAccountLabel] = "team:test-account"
	server := core.Server{
		Provider: providerName,
		CloudID:  "105",
		ID:       105,
		Name:     core.LeaseProviderName(lookalikeID, requestedID),
		Labels:   labels,
	}
	if err := core.ClaimLeaseTargetForRepoConfig(lookalikeID, requestedID, backend.Cfg, server, core.SSHTarget{}, t.TempDir(), backend.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: requestedID, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "exact lease identifier") {
		t.Fatalf("Resolve release-only err=%v", err)
	}
}

func TestReleaseCanonicalIdentifierRejectsMismatchedProviderScope(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	backend := newTestBackend(t, &fakeDigitalOceanAPI{})
	const leaseID = "cbx_abcdef123476"
	labels := core.DirectLeaseLabels(backend.Cfg, leaseID, "wrong-scope", providerName, "", false, time.Now())
	labels[digitalOceanAccountLabel] = "team:test-account"
	server := core.Server{Provider: providerName, CloudID: "476", ID: 476, Name: core.LeaseProviderName(leaseID, "wrong-scope"), Labels: labels}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "wrong-scope", providerName, "account:other", "", t.TempDir(), time.Minute, false, server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	_, err := backend.releaseTargetFromClaim(context.Background(), &fakeDigitalOceanAPI{}, leaseID, "team:test-account")
	if err == nil || !strings.Contains(err.Error(), "exact lease identifier") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveVisibleDropletRejectsAccountMismatchBeforePreservingKeyIdentity(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123451"
	slug := "account-mismatch"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	item := droplet{ID: 111, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)}
	api := &fakeDigitalOceanAPI{
		accountID: "team:account-b",
		droplets:  []droplet{item},
	}
	backend := newTestBackend(t, api)
	claimLabels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	claimLabels[digitalOceanAccountLabel] = "team:account-a"
	claimLabels[digitalOceanRecoveryKeyIDLabel] = "711"
	claimLabels[digitalOceanKeyOwnedLabel] = "true"
	claimServer := core.Server{
		Provider: providerName,
		CloudID:  strconv.FormatInt(item.ID, 10),
		ID:       item.ID,
		Name:     item.Name,
		Labels:   claimLabels,
	}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, claimServer, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "account mismatch") {
		t.Fatalf("Resolve err=%v", err)
	}
	claim, claimErr := core.ReadLeaseClaim(leaseID)
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if claim.Labels[digitalOceanAccountLabel] != "team:account-a" ||
		claim.Labels[digitalOceanRecoveryKeyIDLabel] != "711" ||
		claim.Labels[digitalOceanKeyOwnedLabel] != "true" {
		t.Fatalf("mismatched claim was rewritten: %#v", claim)
	}
}

func TestResolveVisibleDropletRejectsStaleClaimCloudID(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123447"
	slug := "cloud-id-mismatch"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	item := droplet{ID: 107, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	claimLabels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	claimLabels[digitalOceanAccountLabel] = "team:test-account"
	claimLabels[digitalOceanRecoveryKeyIDLabel] = "707"
	claimLabels[digitalOceanKeyOwnedLabel] = "true"
	claimServer := core.Server{
		Provider: providerName,
		CloudID:  "999",
		ID:       999,
		Name:     item.Name,
		Labels:   claimLabels,
	}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, claimServer, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "stale local claim") {
		t.Fatalf("Resolve err=%v", err)
	}
	claim, claimErr := core.ReadLeaseClaim(leaseID)
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if claim.CloudID != "999" || claim.Labels[digitalOceanRecoveryKeyIDLabel] != "707" {
		t.Fatalf("stale claim was rewritten: %#v", claim)
	}
}

func TestResolveVisibleDropletRejectsClaimWithoutAccountIdentity(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123450"
	slug := "account-missing"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	item := droplet{ID: 110, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	claimLabels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	claimLabels[digitalOceanRecoveryKeyIDLabel] = "710"
	claimLabels[digitalOceanKeyOwnedLabel] = "true"
	claimServer := core.Server{
		Provider: providerName,
		CloudID:  strconv.FormatInt(item.ID, 10),
		ID:       item.ID,
		Name:     item.Name,
		Labels:   claimLabels,
	}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, claimServer, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "no account identity") {
		t.Fatalf("Resolve err=%v", err)
	}
	claim, claimErr := core.ReadLeaseClaim(leaseID)
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if claim.Labels[digitalOceanAccountLabel] != "" ||
		claim.Labels[digitalOceanRecoveryKeyIDLabel] != "710" ||
		claim.Labels[digitalOceanKeyOwnedLabel] != "true" {
		t.Fatalf("account-less claim was rewritten: %#v", claim)
	}
}

func TestResolveVisibleDropletRejectsMismatchedClaimLabels(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123446"
	slug := "claim-label-mismatch"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	item := droplet{ID: 106, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	claimLabels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	claimLabels["slug"] = "foreign"
	claimLabels[digitalOceanAccountLabel] = "team:test-account"
	claimLabels[digitalOceanRecoveryKeyIDLabel] = "706"
	claimLabels[digitalOceanKeyOwnedLabel] = "true"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, core.Server{
		Provider: providerName,
		CloudID:  strconv.FormatInt(item.ID, 10),
		ID:       item.ID,
		Name:     item.Name,
		Labels:   claimLabels,
	}, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "claim identity") {
		t.Fatalf("Resolve err=%v", err)
	}
	claim, claimErr := core.ReadLeaseClaim(leaseID)
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if claim.Labels["slug"] != "foreign" || claim.Labels[digitalOceanRecoveryKeyIDLabel] != "706" {
		t.Fatalf("mismatched claim was rewritten: %#v", claim)
	}
}

func TestResolveVisibleDropletRejectsCloudIDLessClaimWithoutRecoveryMarker(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123445"
	slug := "missing-recovery-marker"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	item := droplet{ID: 105, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	claimLabels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	claimLabels[digitalOceanAccountLabel] = "team:test-account"
	claimLabels[digitalOceanRecoveryKeyIDLabel] = "705"
	claimLabels[digitalOceanKeyOwnedLabel] = "true"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, core.Server{
		Provider: providerName,
		Name:     item.Name,
		Labels:   claimLabels,
	}, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "valid pending recovery state") {
		t.Fatalf("Resolve err=%v", err)
	}
	claim, claimErr := core.ReadLeaseClaim(leaseID)
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if claim.CloudID != "" || claim.Labels[digitalOceanRecoveryKeyIDLabel] != "705" {
		t.Fatalf("unmarked claim was rewritten: %#v", claim)
	}
}

func TestResolveVisibleDropletPreservesKeyIdentity(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123439"
	slug := "preserve-key-delete"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	item := droplet{ID: 102, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	claimLabels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	claimLabels[digitalOceanAccountLabel] = "team:test-account"
	claimLabels[digitalOceanRecoveryKeyIDLabel] = "702"
	claimLabels[digitalOceanKeyOwnedLabel] = "true"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, core.Server{
		Provider: providerName,
		CloudID:  strconv.FormatInt(item.ID, 10),
		ID:       item.ID,
		Name:     item.Name,
		Labels:   claimLabels,
	}, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}

	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, Repo: core.Repo{Root: t.TempDir()}, Reclaim: true}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Labels[digitalOceanRecoveryKeyIDLabel] != "702" ||
		claim.Labels[digitalOceanKeyOwnedLabel] != "true" {
		t.Fatalf("authorization marker lost: %#v", claim)
	}
}

func TestEndpointClaimRewriteCannotChangeDigitalOceanProvider(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123438"
	slug := "provider-rewrite"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	labels[digitalOceanAccountLabel] = "team:test-account"
	labels[digitalOceanRecoveryKeyIDLabel] = "701"
	labels[digitalOceanKeyOwnedLabel] = "true"
	server := core.Server{
		Provider: providerName,
		CloudID:  "101",
		ID:       101,
		Name:     core.LeaseProviderName(leaseID, slug),
		Labels:   labels,
	}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}

	err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		leaseID,
		slug,
		"tart",
		"instance:other",
		"",
		t.TempDir(),
		time.Minute,
		true,
		core.Server{Provider: "tart", CloudID: "other", Labels: map[string]string{"lease": leaseID, "slug": slug, "provider": "tart"}},
		core.SSHTarget{},
	)
	if err == nil || !strings.Contains(err.Error(), "as provider=tart") {
		t.Fatalf("rewrite err=%v", err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != providerName || claim.CloudID != "101" ||
		claim.Labels[digitalOceanRecoveryKeyIDLabel] != "701" {
		t.Fatalf("claim rewritten: %#v", claim)
	}
}

func TestUnreadableExactClaimBlocksResolveAndRelease(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123452"
	slug := "repair-claim"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	labels[digitalOceanRecoveryKeyIDLabel] = "707"
	labels[digitalOceanKeyOwnedLabel] = "true"
	item := droplet{ID: 112, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	claimsDir := filepath.Join(stateDir, "claims")
	if err := os.MkdirAll(claimsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claimsDir, leaseID+".json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "parse claim") {
		t.Fatalf("Resolve lease=%#v err=%v", lease, err)
	}
	server := serverFromDroplet(item, backend.Cfg)
	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}})
	if err == nil || !strings.Contains(err.Error(), "snapshot is missing") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 {
		t.Fatalf("deleted=%v deletedKeyIDs=%v", api.deleted, api.deletedKeyIDs)
	}
	data, readErr := os.ReadFile(filepath.Join(claimsDir, leaseID+".json"))
	if readErr != nil || string(data) != "{not-json" {
		t.Fatalf("unreadable claim changed: data=%q err=%v", data, readErr)
	}
}

func TestResolvePendingRecoveryRejectsDuplicateVisibleDroplets(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123459"
	slug := "duplicate"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	labels["recovery"] = "ambiguous-create"
	labels[digitalOceanAccountLabel] = "team:test-account"
	items := []droplet{
		{ID: 109, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)},
		{ID: 110, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)},
	}
	api := &fakeDigitalOceanAPI{droplets: items}
	backend := newTestBackend(t, api)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, core.Server{
		Provider: providerName,
		Name:     items[0].Name,
		Labels:   labels,
	}, core.SSHTarget{}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}

	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true}); err == nil || !strings.Contains(err.Error(), "found multiple droplets") {
		t.Fatalf("Resolve err=%v", err)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok || claim.CloudID != "" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("deleted droplets=%v keys=%v", api.deleted, api.deletedKeys)
	}
}

func TestReleaseFromClaimRejectsCloudIDLessClaimWithoutRecoveryMarker(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123442"
	slug := "unmarked-claim-only"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now().Add(-ambiguousCreateRecoveryGrace-time.Minute))
	labels[digitalOceanAccountLabel] = "team:test-account"
	labels[digitalOceanRecoveryKeyIDLabel] = "704"
	labels[digitalOceanKeyOwnedLabel] = "true"
	item := droplet{ID: 104, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)}
	listCalls := 0
	api := &fakeDigitalOceanAPI{}
	api.listFn = func() ([]droplet, error) {
		listCalls++
		if listCalls == 1 {
			return nil, nil
		}
		return []droplet{item}, nil
	}
	backend := newTestBackend(t, api)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, core.Server{
		Provider: providerName,
		Name:     item.Name,
		Labels:   labels,
	}, core.SSHTarget{}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "invalid recovery state") {
		t.Fatalf("Resolve err=%v", err)
	}
	if listCalls != 1 {
		t.Fatalf("unexpected reconciliation list calls=%d", listCalls)
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 {
		t.Fatalf("deleted=%v deletedKeyIDs=%v", api.deleted, api.deletedKeyIDs)
	}
	claim, claimErr := core.ReadLeaseClaim(leaseID)
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if claim.CloudID != "" || claim.Labels[digitalOceanRecoveryKeyIDLabel] != "704" {
		t.Fatalf("unmarked claim was rewritten: %#v", claim)
	}
}

func TestReleaseFromClaimRejectsMisfiledClaim(t *testing.T) {
	tests := []struct {
		name        string
		claimID     string
		fileID      string
		slug        string
		cloudID     string
		resolveWith string
	}{
		{
			name:        "exact lease id",
			claimID:     "cbx_abcdef123440",
			fileID:      "cbx_abcdef123441",
			slug:        "misfiled-exact",
			cloudID:     "103",
			resolveWith: "cbx_abcdef123441",
		},
		{
			name:        "slug",
			claimID:     "cbx_abcdef123442",
			fileID:      "cbx_abcdef123443",
			slug:        "misfiled-slug",
			cloudID:     "104",
			resolveWith: "misfiled-slug",
		},
		{
			name:        "droplet id",
			claimID:     "cbx_abcdef123444",
			fileID:      "cbx_abcdef123445",
			slug:        "misfiled-cloud-id",
			cloudID:     "105",
			resolveWith: "105",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.Provider = providerName
			cfg.TargetOS = core.TargetLinux
			labels := core.DirectLeaseLabels(cfg, tt.claimID, tt.slug, providerName, "", false, time.Now())
			labels[digitalOceanAccountLabel] = "team:test-account"
			labels[digitalOceanRecoveryKeyIDLabel] = "703"
			labels[digitalOceanKeyOwnedLabel] = "true"
			cloudID, err := strconv.ParseInt(tt.cloudID, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			server := core.Server{
				Provider: providerName,
				CloudID:  tt.cloudID,
				ID:       cloudID,
				Name:     core.LeaseProviderName(tt.claimID, tt.slug),
				Labels:   labels,
			}
			api := &fakeDigitalOceanAPI{}
			backend := newTestBackend(t, api)
			if err := core.ClaimLeaseTargetForRepoConfig(tt.claimID, tt.slug, cfg, server, core.SSHTarget{}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
				t.Fatal(err)
			}
			stateDir, err := core.CrabboxStateDir()
			if err != nil {
				t.Fatal(err)
			}
			claimsDir := filepath.Join(stateDir, "claims")
			data, err := os.ReadFile(filepath.Join(claimsDir, tt.claimID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(claimsDir, tt.fileID+".json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(claimsDir, tt.claimID+".json")); err != nil {
				t.Fatal(err)
			}
			keyPath := writeStoredTestboxKey(t, tt.claimID)

			_, err = backend.Resolve(context.Background(), core.ResolveRequest{ID: tt.resolveWith, ReleaseOnly: true})
			if err == nil || !strings.Contains(err.Error(), "refusing misfiled claim") {
				t.Fatalf("Resolve err=%v", err)
			}
			if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 {
				t.Fatalf("deleted=%v deletedKeyIDs=%v", api.deleted, api.deletedKeyIDs)
			}
			if _, err := os.Stat(filepath.Join(claimsDir, tt.fileID+".json")); err != nil {
				t.Fatalf("misfiled claim removed: %v", err)
			}
			if _, err := os.Stat(keyPath); err != nil {
				t.Fatalf("claim key removed: %v", err)
			}
		})
	}
}

func TestReleasePersistsPendingRecoveryBeforeKeyCleanupFailure(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123460"
	slug := "cleanup-retry"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	labels[digitalOceanAccountLabel] = "team:test-account"
	labels[digitalOceanRecoveryKeyIDLabel] = "705"
	labels[digitalOceanKeyOwnedLabel] = "true"
	item := droplet{ID: 111, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)}
	labels["recovery"] = "ambiguous-create"
	api := &fakeDigitalOceanAPI{
		droplets:     []droplet{item},
		keyDeleteErr: errors.New("key cleanup failed"),
		sshKeys:      []sshKey{{ID: 705, Name: providerKeyForLease(leaseID), PublicKey: "ssh-ed25519 test-key"}},
	}
	backend := newTestBackend(t, api)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, core.Server{
		Provider: providerName,
		Name:     item.Name,
		Labels:   labels,
	}, core.SSHTarget{}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}

	writeStoredTestboxKey(t, leaseID)
	lease := claimedDigitalOceanTarget(t, serverFromDroplet(item, cfg))
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "key cleanup failed") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok || claim.CloudID != strconv.FormatInt(item.ID, 10) {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != item.ID {
		t.Fatalf("deleted=%v", api.deleted)
	}
}

func TestDigitalOceanAcquireRejectsUnsupportedExplicitPortableOS(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.OSImage = "ubuntu:26.04"
	if err := validateDigitalOceanAcquireConfig(cfg, false); err != nil {
		t.Fatalf("implicit OS err=%v", err)
	}
	if err := validateDigitalOceanAcquireConfig(cfg, true); err == nil || !strings.Contains(err.Error(), "does not support --os ubuntu:26.04") {
		t.Fatalf("explicit OS err=%v", err)
	}
	cfg.DigitalOcean.Image = "custom-image"
	if err := validateDigitalOceanAcquireConfig(cfg, true); err != nil {
		t.Fatalf("explicit provider image err=%v", err)
	}
}

func TestAcquireRollsBackKeepDropletAndKeyOnSSHFailure(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	backend.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error { return errors.New("ssh no") }
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "rollback-keep", Keep: true})
	if err == nil || !strings.Contains(err.Error(), "ssh no") {
		t.Fatalf("Acquire err=%v", err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 100 {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != 700 || len(api.deletedKeys) != 0 {
		t.Fatalf("deletedKeyIDs=%v deletedKeys=%v", api.deletedKeyIDs, api.deletedKeys)
	}
}

func TestAcquireRollbackUsesFreshCleanupContextAfterCancellation(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	backend.waitSSH = func(ctx context.Context, _ *core.SSHTarget, _ string, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := backend.Acquire(ctx, core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "cancelled", Keep: true})
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("Acquire err=%v", err)
	}
	if len(api.deleted) != 1 || api.deleteSawDone {
		t.Fatalf("deleted=%v deleteSawDone=%v", api.deleted, api.deleteSawDone)
	}
	if len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != 700 || len(api.deletedKeys) != 0 || api.keyDeleteDone {
		t.Fatalf("deletedKeyIDs=%v deletedKeys=%v keyDeleteDone=%v", api.deletedKeyIDs, api.deletedKeys, api.keyDeleteDone)
	}
}

func TestAcquireRollsBackDropletAndKeyOnReadyTagFailure(t *testing.T) {
	api := &fakeDigitalOceanAPI{replaceErr: errors.New("tag update failed")}
	backend := newTestBackend(t, api)

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "tag-fail", Keep: true})
	if err == nil || !strings.Contains(err.Error(), "tag update failed") {
		t.Fatalf("Acquire err=%v", err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 100 {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != 700 || len(api.deletedKeys) != 0 {
		t.Fatalf("deletedKeyIDs=%v deletedKeys=%v", api.deletedKeyIDs, api.deletedKeys)
	}
}

func TestAcquireRollsBackKeepDropletAndKeyOnClaimFailure(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	claimErr := errors.New("claim failed")
	oldClaim := claimLeaseTargetForRepoConfig
	claimLeaseTargetForRepoConfig = func(string, string, core.Config, core.Server, core.SSHTarget, string, time.Duration, bool, core.LeaseClaim, bool) (core.LeaseClaim, error) {
		return core.LeaseClaim{}, claimErr
	}
	t.Cleanup(func() { claimLeaseTargetForRepoConfig = oldClaim })

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "rollback-claim", Keep: true})
	if err == nil || !strings.Contains(err.Error(), "claim failed") {
		t.Fatalf("Acquire err=%v", err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 100 {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != 700 || len(api.deletedKeys) != 0 {
		t.Fatalf("deletedKeyIDs=%v deletedKeys=%v", api.deletedKeyIDs, api.deletedKeys)
	}
}

func TestResolveBySlugAndReleaseDeletesOwnedDroplet(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-1vcpu-1gb"
	item := droplet{ID: 77, Name: core.LeaseProviderName("cbx_abcdef123456", "resolve-me"), Status: "active", Tags: leaseTags(cfg, "cbx_abcdef123456", "resolve-me", "ready", false, time.Now())}
	item.Networks.V4 = append(item.Networks.V4, struct {
		IPAddress string `json:"ip_address"`
		Type      string `json:"type"`
	}{IPAddress: "203.0.113.77", Type: "public"})
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "resolve-me", Repo: core.Repo{Root: t.TempDir()}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_abcdef123456" || lease.SSH.Host != "203.0.113.77" {
		t.Fatalf("lease=%#v", lease)
	}
	keyPath := writeStoredTestboxKey(t, lease.LeaseID)
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 77 {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("resolve-me", providerName); err != nil || ok {
		t.Fatalf("claim after release ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stored key still exists: %v", err)
	}
}

func TestReleaseRetriesKeyCleanupFromRetainedClaimAfterDropletDeletion(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-1vcpu-1gb"
	leaseID := "cbx_abcdef123456"
	slug := "retry-cleanup"
	cfg.ProviderKey = providerKeyForLease(leaseID)
	item := droplet{ID: 77, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: leaseTags(cfg, leaseID, slug, "ready", false, time.Now())}
	item.Networks.V4 = append(item.Networks.V4, struct {
		IPAddress string `json:"ip_address"`
		Type      string `json:"type"`
	}{IPAddress: "203.0.113.77", Type: "public"})
	api := &fakeDigitalOceanAPI{
		droplets:     []droplet{item},
		keyDeleteErr: errors.New("key cleanup failed"),
	}
	backend := newTestBackend(t, api)
	server := serverFromDroplet(item, backend.Cfg)
	server.Labels[digitalOceanAccountLabel] = "team:test-account"
	server.Labels[digitalOceanRecoveryKeyIDLabel] = "706"
	server.Labels[digitalOceanKeyOwnedLabel] = "true"
	ssh := core.SSHTargetFromConfig(backend.Cfg, server.PublicNet.IPv4.IP)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, backend.Cfg, server, ssh, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	keyPath := writeStoredTestboxKey(t, leaseID)
	api.sshKeys = []sshKey{{ID: 706, Name: providerKeyForLease(leaseID), PublicKey: "ssh-ed25519 test-key"}}

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if keyName := core.ServerProviderKey(lease.Server); !core.ValidCrabboxProviderKey(keyName) {
		t.Fatalf("provider key=%q labels=%v", keyName, lease.Server.Labels)
	}
	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
	if err == nil || !strings.Contains(err.Error(), "key cleanup failed") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.droplets) != 0 || len(api.deleted) != 1 {
		t.Fatalf("droplets=%v deleted=%v", api.droplets, api.deleted)
	}
	if !reflect.DeepEqual(api.events, []string{"droplet", "key"}) {
		t.Fatalf("first cleanup order=%v", api.events)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName); err != nil || !ok {
		t.Fatalf("claim after failed cleanup ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key removed after failed cleanup: %v", err)
	}

	api.keyDeleteErr = nil
	lease, err = backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.ID != 77 || lease.Server.CloudID != "77" || lease.LeaseID != leaseID {
		t.Fatalf("fallback lease=%#v", lease)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 77 || len(api.deletedKeyIDs) != 2 ||
		api.deletedKeyIDs[0] != 706 || api.deletedKeyIDs[1] != 706 || len(api.deletedKeys) != 0 {
		t.Fatalf("deleted=%v deletedKeyIDs=%v deletedKeys=%v", api.deleted, api.deletedKeyIDs, api.deletedKeys)
	}
	if !reflect.DeepEqual(api.events, []string{"droplet", "key", "key"}) {
		t.Fatalf("retry cleanup order=%v", api.events)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName); err != nil || ok {
		t.Fatalf("claim after successful retry ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stored key still exists after retry: %v", err)
	}
}

func TestReleaseFromClaimRefusesDigitalOceanAccountMismatch(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123456"
	slug := "account-guard"
	cfg.ProviderKey = providerKeyForLease(leaseID)
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	labels[digitalOceanAccountLabel] = "team:account-a"
	labels[digitalOceanKeyOwnedLabel] = "false"
	server := core.Server{
		Provider: providerName,
		CloudID:  "77",
		ID:       77,
		Name:     core.LeaseProviderName(leaseID, slug),
		Labels:   labels,
	}
	api := &fakeDigitalOceanAPI{accountID: "team:account-b"}
	backend := newTestBackend(t, api)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, backend.Cfg, server, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	keyPath := writeStoredTestboxKey(t, leaseID)

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "account mismatch") {
		t.Fatalf("Resolve err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("deleted=%v deletedKeys=%v", api.deleted, api.deletedKeys)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName); err != nil || !ok {
		t.Fatalf("claim after mismatch ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key after mismatch: %v", err)
	}

	api.accountID = "team:account-a"
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("deleted=%v deletedKeyIDs=%v deletedKeys=%v", api.deleted, api.deletedKeyIDs, api.deletedKeys)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName); err != nil || ok {
		t.Fatalf("claim after matching cleanup ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("key after matching cleanup: %v", err)
	}
}

func TestReleaseFromClaimRefusesMissingAccountIdentity(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123449"
	slug := "claim-account-missing"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	labels[digitalOceanRecoveryKeyIDLabel] = "709"
	labels[digitalOceanKeyOwnedLabel] = "true"
	server := core.Server{
		Provider: providerName,
		CloudID:  "79",
		ID:       79,
		Name:     core.LeaseProviderName(leaseID, slug),
		Labels:   labels,
	}
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, backend.Cfg, server, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	keyPath := writeStoredTestboxKey(t, leaseID)

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "no account identity") {
		t.Fatalf("Resolve err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 {
		t.Fatalf("deleted=%v deletedKeyIDs=%v", api.deleted, api.deletedKeyIDs)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider(slug, providerName)
	if claimErr != nil || !ok || claim.Labels[digitalOceanAccountLabel] != "" ||
		claim.Labels[digitalOceanRecoveryKeyIDLabel] != "709" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key after refusal: %v", err)
	}
}

func TestReleaseLeaseRefusesMissingClaimAccountIdentity(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123448"
	slug := "cleanup-account-missing"
	item := droplet{ID: 78, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: leaseTags(cfg, leaseID, slug, "ready", false, time.Now())}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	liveServer := serverFromDroplet(item, backend.Cfg)
	claimServer := liveServer
	claimServer.Labels = core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	claimServer.Labels[digitalOceanRecoveryKeyIDLabel] = "708"
	claimServer.Labels[digitalOceanKeyOwnedLabel] = "true"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, backend.Cfg, claimServer, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	keyPath := writeStoredTestboxKey(t, leaseID)

	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: claimedDigitalOceanTarget(t, liveServer)})
	if err == nil || !strings.Contains(err.Error(), "no account identity") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 {
		t.Fatalf("deleted=%v deletedKeyIDs=%v", api.deleted, api.deletedKeyIDs)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider(slug, providerName)
	if claimErr != nil || !ok || claim.Labels[digitalOceanAccountLabel] != "" ||
		claim.Labels[digitalOceanRecoveryKeyIDLabel] != "708" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key after refusal: %v", err)
	}
}

func TestReleaseLeaseRefusesMismatchedClaimLabels(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123444"
	slug := "cleanup-label-mismatch"
	item := droplet{ID: 77, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: leaseTags(cfg, leaseID, slug, "ready", false, time.Now())}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	liveServer := serverFromDroplet(item, backend.Cfg)
	claimServer := liveServer
	claimServer.Labels = core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	claimServer.Labels["lease"] = "cbx_abcdef123443"
	claimServer.Labels[digitalOceanAccountLabel] = "team:test-account"
	claimServer.Labels[digitalOceanRecoveryKeyIDLabel] = "707"
	claimServer.Labels[digitalOceanKeyOwnedLabel] = "true"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, backend.Cfg, claimServer, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	keyPath := writeStoredTestboxKey(t, leaseID)

	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: claimedDigitalOceanTarget(t, liveServer)})
	if err == nil || !strings.Contains(err.Error(), "claim identity") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 {
		t.Fatalf("deleted=%v deletedKeyIDs=%v", api.deleted, api.deletedKeyIDs)
	}
	claim, claimErr := core.ReadLeaseClaim(leaseID)
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if claim.Labels["lease"] != "cbx_abcdef123443" || claim.Labels[digitalOceanRecoveryKeyIDLabel] != "707" {
		t.Fatalf("mismatched claim was rewritten: %#v", claim)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key after refusal: %v", err)
	}
}

func TestReleaseLeaseRefusesUnboundClaimWithoutRecoveryState(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123437"
	slug := "cleanup-unbound"
	item := droplet{ID: 76, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: leaseTags(cfg, leaseID, slug, "ready", false, time.Now())}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	liveServer := serverFromDroplet(item, backend.Cfg)
	claimLabels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	claimLabels[digitalOceanAccountLabel] = "team:test-account"
	claimLabels[digitalOceanRecoveryKeyIDLabel] = "706"
	claimLabels[digitalOceanKeyOwnedLabel] = "true"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, backend.Cfg, core.Server{
		Provider: providerName,
		Name:     item.Name,
		Labels:   claimLabels,
	}, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	keyPath := writeStoredTestboxKey(t, leaseID)

	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: claimedDigitalOceanTarget(t, liveServer)})
	if err == nil || !strings.Contains(err.Error(), "valid cleanup recovery state") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 {
		t.Fatalf("deleted=%v deletedKeyIDs=%v", api.deleted, api.deletedKeyIDs)
	}
	claim, claimErr := core.ReadLeaseClaim(leaseID)
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if claim.CloudID != "" || claim.Labels[digitalOceanRecoveryKeyIDLabel] != "706" {
		t.Fatalf("unbound claim changed: %#v", claim)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key after refusal: %v", err)
	}
}

func TestReleaseLeaseRefusesAmbiguousCreateWithoutVerifiedDroplet(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123436"
	slug := "cleanup-ambiguous-hidden"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	labels["recovery"] = "ambiguous-create"
	labels[digitalOceanAccountLabel] = "team:test-account"
	labels[digitalOceanRecoveryKeyIDLabel] = "705"
	labels[digitalOceanKeyOwnedLabel] = "true"
	server := core.Server{
		Provider: providerName,
		ID:       75,
		Name:     core.LeaseProviderName(leaseID, slug),
		Labels:   labels,
	}
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, backend.Cfg, server, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	keyPath := writeStoredTestboxKey(t, leaseID)

	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: claimedDigitalOceanTarget(t, server)})
	if err == nil || !strings.Contains(err.Error(), "still indeterminate") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 {
		t.Fatalf("deleted=%v deletedKeyIDs=%v", api.deleted, api.deletedKeyIDs)
	}
	claim, claimErr := core.ReadLeaseClaim(leaseID)
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if claim.CloudID != "" || claim.Labels["recovery"] != "ambiguous-create" {
		t.Fatalf("ambiguous claim changed: %#v", claim)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key after refusal: %v", err)
	}
}

func TestReleaseDoesNotBackfillStaleLegacyClaim(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123456"
	slug := "stale-legacy"
	item := droplet{ID: 77, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: leaseTags(cfg, leaseID, slug, "ready", false, time.Now())}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	liveServer := serverFromDroplet(item, backend.Cfg)
	staleServer := liveServer
	staleServer.ID = 999
	staleServer.CloudID = "999"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, backend.Cfg, staleServer, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}

	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: claimedDigitalOceanTarget(t, liveServer)})
	if err == nil || !strings.Contains(err.Error(), "stale local claim") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("deleted=%v deletedKeys=%v", api.deleted, api.deletedKeys)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName)
	if err != nil || !ok || claim.CloudID != "999" || claim.Labels[digitalOceanAccountLabel] != "" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestReleaseFromClaimRefusesDropletMissingOwnershipTags(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-1vcpu-1gb"
	leaseID := "cbx_abcdef123456"
	slug := "lost-tags"
	cfg.ProviderKey = providerKeyForLease(leaseID)
	item := droplet{ID: 78, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: leaseTags(cfg, leaseID, slug, "ready", false, time.Now())}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	server := serverFromDroplet(item, backend.Cfg)
	server.Labels[digitalOceanAccountLabel] = "team:test-account"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, backend.Cfg, server, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	for i, tag := range api.droplets[0].Tags {
		if tag == "crabbox:target:linux" {
			api.droplets[0].Tags = append(api.droplets[0].Tags[:i], api.droplets[0].Tags[i+1:]...)
			break
		}
	}

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "refusing to operate") {
		t.Fatalf("Resolve lease=%#v err=%v", lease, err)
	}
	if len(api.deleted) != 0 || len(api.droplets) != 1 {
		t.Fatalf("deleted=%v droplets=%v", api.deleted, api.droplets)
	}
}

func TestResolveDoesNotTreatNumericIdentifierAsSlug(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-1vcpu-1gb"
	item := droplet{ID: 456, Name: "crabbox-numeric", Status: "active", Tags: leaseTags(cfg, "cbx_abcdef123456", "123", "ready", false, time.Now())}
	item.Networks.V4 = append(item.Networks.V4, struct {
		IPAddress string `json:"ip_address"`
		Type      string `json:"type"`
	}{IPAddress: "203.0.113.123", Type: "public"})
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "123"})
	if err == nil || !isDigitalOceanNotFound(err) {
		t.Fatalf("lease=%#v err=%v", lease, err)
	}
}

func TestResolveByDropletIDRejectsMissingOwnershipTarget(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	item := droplet{ID: 77, Name: "partial", Status: "active", Tags: leaseTags(cfg, "cbx_abcdef123456", "partial", "ready", false, time.Now())}
	for i, tag := range item.Tags {
		if tag == "crabbox:target:linux" {
			item.Tags = append(item.Tags[:i], item.Tags[i+1:]...)
			break
		}
	}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "77", ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "refusing to operate") {
		t.Fatalf("Resolve err=%v", err)
	}
}

func TestReleaseRefusesUnownedDroplet(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	server := core.Server{ID: 9, Name: "foreign", Labels: map[string]string{"crabbox": "true"}}
	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{Server: server, LeaseID: "cbx_999999999999"}})
	if err == nil {
		t.Fatal("ReleaseLease accepted unowned server")
	}
	if len(api.deleted) != 0 {
		t.Fatalf("deleted=%v", api.deleted)
	}
}

func TestReleaseWithoutClaimPreservesStoredKey(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123456"
	item := droplet{ID: 91, Name: "recovered", Status: "active", Tags: leaseTags(cfg, leaseID, "recovered", "ready", false, time.Now())}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	keyPath := writeStoredTestboxKey(t, leaseID)

	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		Server:  serverFromDroplet(item, backend.Cfg),
		LeaseID: leaseID,
	}}); err == nil || !strings.Contains(err.Error(), "snapshot is missing") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key removed after refused unclaimed release: %v", err)
	}
	if len(api.deletedKeyIDs) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("claimless cleanup deletedKeyIDs=%v deletedKeys=%v", api.deletedKeyIDs, api.deletedKeys)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("claimless cleanup deleted droplets=%v", api.deleted)
	}
}

func TestReleaseDoesNotPersistClaimForUnclaimedServer(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123461"
	item := droplet{ID: 92, Name: "retry-delete", Status: "active", Tags: leaseTags(cfg, leaseID, "retry-delete", "ready", false, time.Now())}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	keyPath := writeStoredTestboxKey(t, leaseID)
	lease := core.LeaseTarget{Server: serverFromDroplet(item, backend.Cfg), LeaseID: leaseID}

	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "snapshot is missing") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName); err != nil || ok {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key removed after refused delete: %v", err)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("unclaimed release deleted=%v", api.deleted)
	}
}

func TestReleaseRefusesDropletWhoseLiveOwnershipChanged(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	item := droplet{ID: 90, Name: "changed", Status: "active", Tags: leaseTags(cfg, "cbx_abcdef123456", "changed", "ready", false, time.Now())}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	server := serverFromDroplet(item, backend.Cfg)
	server.Labels[digitalOceanAccountLabel] = "team:test-account"
	server.Labels[digitalOceanKeyOwnedLabel] = "false"
	claimDigitalOceanServer(t, backend, server)
	lease := claimedDigitalOceanTarget(t, server)
	api.droplets[0].Tags = removeTag(api.droplets[0].Tags, "crabbox:target:linux")

	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
	if err == nil || !strings.Contains(err.Error(), "refusing to operate") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("deleted=%v deletedKeys=%v", api.deleted, api.deletedKeys)
	}
}

func TestCleanupDryRunDoesNotDelete(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.TTL = time.Nanosecond
	stale := droplet{ID: 86, Name: "stale-account", Status: "active", Tags: leaseTags(cfg, "cbx_222222222222", "stale-account", "ready", false, time.Now().Add(-time.Hour))}
	claimless := droplet{ID: 87, Name: "claimless", Status: "active", Tags: leaseTags(cfg, "cbx_111111111111", "claimless", "ready", false, time.Now().Add(-time.Hour))}
	item := droplet{ID: 88, Name: core.LeaseProviderName("cbx_deadbeef1234", "old"), Status: "active", Tags: leaseTags(cfg, "cbx_deadbeef1234", "old", "ready", false, time.Now().Add(-time.Hour))}
	api := &fakeDigitalOceanAPI{droplets: []droplet{stale, claimless, item}}
	backend := newTestBackend(t, api)
	staleServer := serverFromDroplet(stale, backend.Cfg)
	staleServer.Labels[digitalOceanAccountLabel] = "team:stale-account"
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_222222222222", "stale-account", backend.Cfg, staleServer, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	server := serverFromDroplet(item, backend.Cfg)
	server.Labels[digitalOceanAccountLabel] = "team:test-account"
	server.Labels[digitalOceanKeyOwnedLabel] = "false"
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_deadbeef1234", "old", backend.Cfg, server, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	keyPath := writeStoredTestboxKey(t, "cbx_deadbeef1234")
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("dry-run deleted=%v", api.deleted)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("old", providerName); err != nil || !ok {
		t.Fatalf("claim after dry-run ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key removed by dry-run: %v", err)
	}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 88 {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("cbx_222222222222", providerName); err != nil || !ok {
		t.Fatalf("stale-account claim after cleanup ok=%v err=%v", ok, err)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("old", providerName); err != nil || ok {
		t.Fatalf("claim after cleanup ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stored key still exists after cleanup: %v", err)
	}
}

func TestCleanupPreservesLocalStateForMismatchedProviderClaim(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.TTL = time.Nanosecond
	leaseID := "cbx_123456abcdef"
	item := droplet{ID: 89, Name: "old", Status: "active", Tags: leaseTags(cfg, leaseID, "old", "ready", false, time.Now().Add(-time.Hour))}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	otherCfg := core.BaseConfig()
	otherCfg.Provider = "aws"
	otherServer := core.Server{
		CloudID:  "i-123",
		Provider: "aws",
		Labels: map[string]string{
			digitalOceanRecoveryKeyIDLabel: "999",
			digitalOceanKeyOwnedLabel:      "true",
		},
	}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "other-provider", otherCfg, otherServer, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	originalClaim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := writeStoredTestboxKey(t, leaseID)

	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup should skip mismatched claim: %v", err)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if len(api.deletedKeyIDs) != 0 {
		t.Fatalf("foreign claim deletedKeyIDs=%v", api.deletedKeyIDs)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, "aws")
	if err != nil || !ok {
		t.Fatalf("other provider claim after cleanup ok=%v err=%v", ok, err)
	}
	if claim.Provider != originalClaim.Provider || claim.CloudID != originalClaim.CloudID ||
		claim.Labels["provider"] != originalClaim.Labels["provider"] {
		t.Fatalf("other provider claim changed: before=%#v after=%#v", originalClaim, claim)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("other provider key removed by cleanup: %v", err)
	}
}

func claimDigitalOceanServer(t *testing.T, backend *digitalOceanLeaseBackend, server core.Server) {
	t.Helper()
	server.Labels[digitalOceanAccountLabel] = "team:test-account"
	leaseID := server.Labels["lease"]
	if err := core.ClaimLeaseTargetForRepoConfig(
		leaseID,
		server.Labels["slug"],
		backend.Cfg,
		server,
		core.SSHTarget{},
		t.TempDir(),
		backend.Cfg.IdleTimeout,
		false,
	); err != nil {
		t.Fatal(err)
	}
}

func claimedDigitalOceanTarget(t *testing.T, server core.Server) core.LeaseTarget {
	t.Helper()
	claim, err := core.ReadLeaseClaim(server.Labels["lease"])
	if err != nil {
		t.Fatal(err)
	}
	core.SetServerLeaseClaimSnapshot(&server, claim, true)
	return core.LeaseTarget{LeaseID: claim.LeaseID, Server: server}
}

func writeStoredTestboxKey(t *testing.T, leaseID string) string {
	t.Helper()
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("test-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath+".pub", []byte("ssh-ed25519 test-key"), 0o644); err != nil {
		t.Fatal(err)
	}
	return keyPath
}

func TestTouchPreservesLiveTailscaleTags(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-1vcpu-1gb"
	cfg.TTL = time.Hour
	cfg.IdleTimeout = time.Minute
	item := droplet{ID: 99, Name: "touch", Status: "active", Tags: leaseTags(cfg, "cbx_abcdef123456", "touch-me", "ready", false, time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC))}
	server := serverFromDroplet(item, cfg)
	server.Labels["tailscale_ipv4"] = "100.64.1.1"
	server.Labels["tailscale_fqdn"] = "stale.example.ts.net"
	server.Labels["tailscale_state"] = "requested"
	server.Labels["tailscale_tags"] = "tag:stale"
	server.Labels["tailscale_exit_node"] = "stale.example.ts.net"
	server.Labels[digitalOceanAccountLabel] = "team:test-account"
	server.Labels[digitalOceanRecoveryKeyIDLabel] = "703"
	server.Labels[digitalOceanKeyOwnedLabel] = "true"
	liveLabels := normalizedDropletLabels(item.Tags)
	liveLabels["tailscale_ipv4"] = "100.64.1.2"
	liveLabels["tailscale_fqdn"] = "touch-me.example.ts.net"
	liveLabels["tailscale_state"] = "ready"
	liveLabels["tailscale_error"] = "last probe failed: retrying"
	liveLabels["tailscale_tags"] = "tag:ci,tag:crabbox"
	liveLabels["tailscale_exit_node"] = "exit.example.ts.net"
	item.Tags = tagsFromLabels(liveLabels)
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	backend.RT.Clock = fixedClock{t: time.Date(2026, 6, 10, 12, 10, 0, 0, time.UTC)}

	touched, err := backend.Touch(context.Background(), core.TouchRequest{
		Lease:       core.LeaseTarget{Server: server, LeaseID: "cbx_abcdef123456"},
		State:       "running",
		IdleTimeout: 20 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if touched.Labels["state"] != "running" || touched.Labels["idle_timeout_secs"] != "1200" {
		t.Fatalf("touched labels=%v", touched.Labels)
	}
	if touched.Labels[digitalOceanAccountLabel] != "team:test-account" {
		t.Fatalf("account label=%q", touched.Labels[digitalOceanAccountLabel])
	}
	if touched.Labels[digitalOceanRecoveryKeyIDLabel] != "703" || touched.Labels[digitalOceanKeyOwnedLabel] != "true" {
		t.Fatalf("key identity labels=%v", touched.Labels)
	}
	if len(api.replaced) != 1 || api.replaced[0] != 99 {
		t.Fatalf("replaced=%v", api.replaced)
	}
	decoded := labelsFromTags(api.replacedTo[0])
	if decoded["state"] != "running" ||
		decoded["idle_timeout_secs"] != "1200" ||
		decoded["tailscale_ipv4"] != "100.64.1.2" ||
		decoded["tailscale_fqdn"] != "touch-me.example.ts.net" ||
		decoded["tailscale_state"] != "ready" ||
		decoded["tailscale_error"] != "last probe failed: retrying" ||
		decoded["tailscale_tags"] != "tag:ci,tag:crabbox" ||
		decoded["tailscale_exit_node"] != "exit.example.ts.net" {
		t.Fatalf("persisted labels=%v tags=%v", decoded, api.replacedTo[0])
	}
}

func TestUpdateTailscaleMetadataPersistsDigitalOceanTags(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Tailscale.Enabled = true
	item := droplet{ID: 105, Name: "metadata", Status: "active", Tags: leaseTags(cfg, "cbx_abcdef123456", "metadata", "ready", false, time.Now())}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	server := serverFromDroplet(item, backend.Cfg)
	server.Labels[digitalOceanAccountLabel] = "team:test-account"
	server.Labels[digitalOceanRecoveryKeyIDLabel] = "704"
	server.Labels[digitalOceanKeyOwnedLabel] = "true"
	meta := core.TailscaleMetadata{
		Enabled:  true,
		Hostname: "metadata",
		FQDN:     "metadata.example.ts.net",
		IPv4:     "100.64.1.3",
		Tags:     []string{"tag:ci", "tag:crabbox"},
		State:    "ready",
		Error:    "last probe failed: retrying",
		ExitNode: "exit.example.ts.net",
	}

	updated, err := backend.UpdateTailscaleMetadata(context.Background(), core.LeaseTarget{
		Server:  server,
		LeaseID: "cbx_abcdef123456",
	}, meta)
	if err != nil {
		t.Fatal(err)
	}
	if len(api.replacedTo) != 1 {
		t.Fatalf("replacedTo=%v", api.replacedTo)
	}
	if updated.Labels[digitalOceanAccountLabel] != "team:test-account" {
		t.Fatalf("account label=%q", updated.Labels[digitalOceanAccountLabel])
	}
	if updated.Labels[digitalOceanRecoveryKeyIDLabel] != "704" || updated.Labels[digitalOceanKeyOwnedLabel] != "true" {
		t.Fatalf("key identity labels=%v", updated.Labels)
	}
	for _, labels := range []map[string]string{labelsFromTags(api.replacedTo[0]), updated.Labels} {
		if labels["tailscale_ipv4"] != meta.IPv4 ||
			labels["tailscale_fqdn"] != meta.FQDN ||
			labels["tailscale_state"] != meta.State ||
			labels["tailscale_error"] != meta.Error ||
			labels["tailscale_tags"] != strings.Join(meta.Tags, ",") ||
			labels["tailscale_exit_node"] != meta.ExitNode {
			t.Fatalf("persisted labels=%v tags=%v", labels, api.replacedTo[0])
		}
	}
}

func TestTouchRefusesDropletWhoseLiveOwnershipChanged(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	item := droplet{ID: 100, Name: "changed", Status: "active", Tags: leaseTags(cfg, "cbx_abcdef123456", "changed", "ready", false, time.Now())}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	server := serverFromDroplet(item, backend.Cfg)
	api.droplets[0].Tags = removeTag(api.droplets[0].Tags, "crabbox:target:linux")

	_, err := backend.Touch(context.Background(), core.TouchRequest{
		Lease: core.LeaseTarget{Server: server, LeaseID: "cbx_abcdef123456"},
		State: "running",
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to operate") {
		t.Fatalf("Touch err=%v", err)
	}
	if len(api.replaced) != 0 {
		t.Fatalf("replaced=%v", api.replaced)
	}
}

func TestTouchRefusesDropletWhoseProviderKeyChanged(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ProviderKey = providerKeyForLease("cbx_abcdef123456")
	item := droplet{ID: 102, Name: "changed-key", Status: "active", Tags: leaseTags(cfg, "cbx_abcdef123456", "changed-key", "ready", false, time.Now())}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	server := serverFromDroplet(item, backend.Cfg)
	for i, tag := range api.droplets[0].Tags {
		if strings.HasPrefix(tag, "crabbox:provider_key:") {
			api.droplets[0].Tags[i] = "crabbox:provider_key:crabbox-cbx-other123456"
		}
	}

	_, err := backend.Touch(context.Background(), core.TouchRequest{
		Lease: core.LeaseTarget{Server: server, LeaseID: "cbx_abcdef123456"},
		State: "running",
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to operate") {
		t.Fatalf("Touch err=%v", err)
	}
	if len(api.replaced) != 0 {
		t.Fatalf("replaced=%v", api.replaced)
	}
}

func TestReleaseWithoutKeyOwnershipFailsClosed(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123456"
	cfg.ProviderKey = "crabbox-cbx-deadbeef1234"
	item := droplet{ID: 104, Name: core.LeaseProviderName(leaseID, "changed-key"), Status: "active", Tags: leaseTags(cfg, leaseID, "changed-key", "ready", false, time.Now())}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	server := serverFromDroplet(item, backend.Cfg)
	claimDigitalOceanServer(t, backend, server)

	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: claimedDigitalOceanTarget(t, server)}); err == nil || !strings.Contains(err.Error(), "ownership remains indeterminate") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 || len(api.deletedKeys) != 0 {
		t.Fatalf("deleted=%v deletedKeyIDs=%v deletedKeys=%v", api.deleted, api.deletedKeyIDs, api.deletedKeys)
	}
}

func TestReleaseValidatesKeyBeforeDropletMutation(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123452"
	slug := "key-lookup-failure"
	item := droplet{ID: 115, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: leaseTags(cfg, leaseID, slug, "ready", false, time.Now())}
	api := &fakeDigitalOceanAPI{
		droplets: []droplet{item},
		findKeyByIDFn: func(int64) (sshKey, bool, error) {
			return sshKey{}, false, errors.New("key lookup failed")
		},
	}
	backend := newTestBackend(t, api)
	server := serverFromDroplet(item, backend.Cfg)
	server.Labels[digitalOceanRecoveryKeyIDLabel] = "710"
	server.Labels[digitalOceanKeyOwnedLabel] = "true"
	claimDigitalOceanServer(t, backend, server)
	writeStoredTestboxKey(t, leaseID)

	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: claimedDigitalOceanTarget(t, server)})
	if err == nil || !strings.Contains(err.Error(), "key lookup failed") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if len(api.deletedKeyIDs) != 0 {
		t.Fatalf("deletedKeyIDs=%v", api.deletedKeyIDs)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName); err != nil || !ok {
		t.Fatalf("retained claim ok=%v err=%v", ok, err)
	}
}

func TestReleaseRefusesIndeterminateMatchingKey(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123451"
	item := droplet{ID: 113, Name: core.LeaseProviderName(leaseID, "legacy-key"), Status: "active", Tags: leaseTags(cfg, leaseID, "legacy-key", "ready", false, time.Now())}
	api := &fakeDigitalOceanAPI{
		droplets: []droplet{item},
		sshKeys: []sshKey{{
			ID:        708,
			Name:      providerKeyForLease(leaseID),
			PublicKey: "ssh-ed25519 test-key",
		}},
	}
	backend := newTestBackend(t, api)
	writeStoredTestboxKey(t, leaseID)
	server := serverFromDroplet(item, backend.Cfg)
	claimDigitalOceanServer(t, backend, server)

	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: claimedDigitalOceanTarget(t, server)}); err == nil || !strings.Contains(err.Error(), "ownership remains indeterminate") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 || len(api.sshKeys) != 1 {
		t.Fatalf("deleted=%v deletedKeyIDs=%v sshKeys=%v", api.deleted, api.deletedKeyIDs, api.sshKeys)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName); err != nil || !ok {
		t.Fatalf("retained claim ok=%v err=%v", ok, err)
	}
}

func TestReleaseRefusesManagedKeyWithoutLocalProof(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123450"
	slug := "missing-proof"
	item := droplet{ID: 114, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: leaseTags(cfg, leaseID, slug, "ready", false, time.Now())}
	api := &fakeDigitalOceanAPI{
		droplets: []droplet{item},
		sshKeys: []sshKey{{
			ID:        709,
			Name:      providerKeyForLease(leaseID),
			PublicKey: "ssh-ed25519 unknown",
		}},
	}
	backend := newTestBackend(t, api)
	server := serverFromDroplet(item, backend.Cfg)
	server.Labels[digitalOceanRecoveryKeyIDLabel] = "709"
	server.Labels[digitalOceanKeyOwnedLabel] = "true"
	claimDigitalOceanServer(t, backend, server)

	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: claimedDigitalOceanTarget(t, server)}); err == nil || !strings.Contains(err.Error(), "requires retained local public key") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 || len(api.sshKeys) != 1 {
		t.Fatalf("deleted=%v deletedKeyIDs=%v sshKeys=%v", api.deleted, api.deletedKeyIDs, api.sshKeys)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName); err != nil || !ok {
		t.Fatalf("retained claim ok=%v err=%v", ok, err)
	}
}

func TestValidateLiveDropletDerivesMissingProviderKey(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123456"
	item := droplet{ID: 103, Name: "missing-key", Status: "active", Tags: leaseTags(cfg, leaseID, "missing-key", "ready", false, time.Now())}
	for i := 0; i < len(item.Tags); {
		if strings.HasPrefix(item.Tags[i], "crabbox:provider_key:") {
			item.Tags = append(item.Tags[:i], item.Tags[i+1:]...)
			continue
		}
		i++
	}
	expected := serverFromDroplet(item, cfg)
	delete(expected.Labels, "provider_key")

	if err := validateLiveDroplet(item, expected); err != nil {
		t.Fatalf("validateLiveDroplet err=%v tags=%v live=%v expected=%v", err, item.Tags, normalizedDropletLabels(item.Tags), expected.Labels)
	}
}

func TestTouchReplacesObsoleteMutableTags(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-1vcpu-1gb"
	cfg.TTL = time.Hour
	cfg.IdleTimeout = 5 * time.Minute
	item := droplet{ID: 101, Name: "touch", Status: "active", Tags: leaseTags(cfg, "cbx_abcdef123456", "touch-me", "running", false, time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC))}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	backend.RT.Clock = fixedClock{t: time.Date(2026, 6, 10, 12, 10, 0, 0, time.UTC)}

	server := serverFromDroplet(item, backend.Cfg)
	_, err := backend.Touch(context.Background(), core.TouchRequest{
		Lease: core.LeaseTarget{Server: server, LeaseID: "cbx_abcdef123456"},
		State: "ready",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(api.replacedTo) != 1 {
		t.Fatalf("replacedTo=%v", api.replacedTo)
	}
	for _, tag := range api.replacedTo[0] {
		if tag == "crabbox:state:running" {
			t.Fatalf("obsolete running tag persisted: %v", api.replacedTo[0])
		}
	}
	if got := labelsFromTags(api.droplets[0].Tags)["state"]; got != "ready" {
		t.Fatalf("state=%q tags=%v", got, api.droplets[0].Tags)
	}
}

func TestAcquireReadyTagsPreserveRenderedTailscaleHostname(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	backend.Cfg.Tailscale.Enabled = true
	backend.Cfg.Tailscale.AuthKey = "tskey-auth-test"
	backend.Cfg.Tailscale.HostnameTemplate = "{{slug}}-{{lease}}"
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "tailnet", Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(api.createRequests) != 1 || api.createRequests[0].cfg.Tailscale.Hostname == "" {
		t.Fatalf("createRequests=%#v", api.createRequests)
	}
	if got := lease.Server.Labels["tailscale_hostname"]; got == "" || got == "unknown" {
		t.Fatalf("tailscale hostname=%q labels=%v", got, lease.Server.Labels)
	}
	if lease.Server.Labels["tailscale_hostname"] != api.createRequests[0].cfg.Tailscale.Hostname {
		t.Fatalf("ready hostname=%q create hostname=%q", lease.Server.Labels["tailscale_hostname"], api.createRequests[0].cfg.Tailscale.Hostname)
	}
}

func TestApplyDigitalOceanDefaultsDoesNotMutateGenericLocation(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.DigitalOcean.Region = "sfo3"
	want := cfg.Location

	applyDigitalOceanDefaults(&cfg)

	if cfg.Location != want {
		t.Fatalf("Location=%q want %q", cfg.Location, want)
	}
}

func TestApplyDigitalOceanDefaultsDoesNotMutateGenericImage(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.DigitalOcean.Image = "ubuntu-24-04-x64"
	want := cfg.Image

	applyDigitalOceanDefaults(&cfg)

	if cfg.Image != want {
		t.Fatalf("Image=%q want %q", cfg.Image, want)
	}
}

func TestApplyDigitalOceanDefaultsUseProviderDefaults(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.ServerType = "cpx51"

	applyDigitalOceanDefaults(&cfg)

	if cfg.DigitalOcean.Region != "nyc3" {
		t.Fatalf("DigitalOcean.Region=%q want %q", cfg.DigitalOcean.Region, "nyc3")
	}
	if cfg.DigitalOcean.Image != "ubuntu-24-04-x64" {
		t.Fatalf("DigitalOcean.Image=%q want %q", cfg.DigitalOcean.Image, "ubuntu-24-04-x64")
	}
	if cfg.SSHUser != "root" || cfg.SSHPort != "22" || len(cfg.SSHFallbackPorts) != 0 {
		t.Fatalf("effective ssh defaults=%s@:%s fallback=%v", cfg.SSHUser, cfg.SSHPort, cfg.SSHFallbackPorts)
	}
	if cfg.ServerType != "s-1vcpu-1gb" {
		t.Fatalf("ServerType=%q want digitalocean default", cfg.ServerType)
	}
}

func TestApplyDigitalOceanDefaultsPreservesExplicitBaseSSHValues(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	core.MarkSSHUserExplicit(&cfg)
	core.MarkSSHPortExplicit(&cfg)

	applyDigitalOceanDefaults(&cfg)

	if cfg.SSHUser != "crabbox" || cfg.SSHPort != core.BaseConfig().SSHPort {
		t.Fatalf("effective ssh=%s@:%s want explicit base values", cfg.SSHUser, cfg.SSHPort)
	}
}

func TestApplyDigitalOceanDefaultsPreservesExplicitServerType(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.ServerType = "s-2vcpu-2gb"
	cfg.ServerTypeExplicit = true

	applyDigitalOceanDefaults(&cfg)

	if cfg.ServerType != "s-2vcpu-2gb" {
		t.Fatalf("ServerType=%q want explicit type", cfg.ServerType)
	}
}

func TestPublicIPv4RequiresPublicNetwork(t *testing.T) {
	item := droplet{}
	item.Networks.V4 = append(item.Networks.V4, struct {
		IPAddress string `json:"ip_address"`
		Type      string `json:"type"`
	}{IPAddress: "10.0.0.2", Type: "private"})
	if got := publicIPv4(item); got != "" {
		t.Fatalf("publicIPv4(private-only)=%q", got)
	}
	item.Networks.V4 = append(item.Networks.V4, struct {
		IPAddress string `json:"ip_address"`
		Type      string `json:"type"`
	}{IPAddress: "203.0.113.10", Type: "public"})
	if got := publicIPv4(item); got != "203.0.113.10" {
		t.Fatalf("publicIPv4=%q", got)
	}
}

func removeTag(tags []string, target string) []string {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		if tag != target {
			out = append(out, tag)
		}
	}
	return out
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func TestMain(m *testing.M) {
	os.Exit(testutil.RunWithIsolatedUserDirs(m))
}
