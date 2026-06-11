package digitalocean

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeDigitalOceanAPI struct {
	droplets       []droplet
	listFn         func() ([]droplet, error)
	nextID         int64
	createErr      error
	deleteErr      error
	deleteSawDone  bool
	keyDeleteErr   error
	replaceErr     error
	created        []droplet
	deleted        []int64
	keyDeleteDone  bool
	deletedKeys    []string
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

func (f *fakeDigitalOceanAPI) GetDroplet(_ context.Context, id int64) (droplet, error) {
	for _, item := range append(f.droplets, f.created...) {
		if item.ID == id {
			return item, nil
		}
	}
	return droplet{}, &digitalOceanAPIError{Status: 404}
}

func (f *fakeDigitalOceanAPI) CreateDroplet(_ context.Context, cfg core.Config, _ string, leaseID, slug string, keep bool, now time.Time) (droplet, error) {
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
	item := droplet{ID: f.nextID, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: leaseTags(cfg, leaseID, slug, "provisioning", keep, now)}
	item.Networks.V4 = append(item.Networks.V4, struct {
		IPAddress string `json:"ip_address"`
		Type      string `json:"type"`
	}{IPAddress: "203.0.113.10", Type: "public"})
	item.Size.Slug = cfg.ServerType
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
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.droplets = removeDropletByID(f.droplets, id)
	f.created = removeDropletByID(f.created, id)
	return nil
}

func (f *fakeDigitalOceanAPI) DeleteSSHKeyByName(ctx context.Context, name string) error {
	select {
	case <-ctx.Done():
		f.keyDeleteDone = true
	default:
	}
	f.deletedKeys = append(f.deletedKeys, name)
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
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
	from := labelsFromTags(api.replacedFrom[0])
	to := labelsFromTags(api.replacedTo[0])
	if from["created_at"] != to["created_at"] || from["expires_at"] != to["expires_at"] {
		t.Fatalf("ready transition changed lifetime: from=%v to=%v", from, to)
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
	if len(api.deletedKeys) != 1 || !strings.HasPrefix(api.deletedKeys[0], "crabbox-cbx-") {
		t.Fatalf("deletedKeys=%v", api.deletedKeys)
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
}

func TestAcquireRetainsCredentialsWhenDropletCreationIsAmbiguous(t *testing.T) {
	api := &fakeDigitalOceanAPI{
		createErr: &ambiguousDropletCreateError{err: errors.New("create reconciliation timed out")},
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
	if claimErr != nil || !ok || claim.LeaseID != api.createRequests[0].leaseID || claim.CloudID != "" {
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
	claim, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName)
	if err != nil || !ok || claim.CloudID != strconv.FormatInt(item.ID, 10) {
		t.Fatalf("recovered claim=%#v ok=%v err=%v", claim, ok, err)
	}

	api.keyDeleteErr = errors.New("key cleanup failed")
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "key cleanup failed") {
		t.Fatalf("first ReleaseLease err=%v", err)
	}
	claim, ok, err = core.ResolveLeaseClaimForProvider(slug, providerName)
	if err != nil || !ok || claim.CloudID != strconv.FormatInt(item.ID, 10) {
		t.Fatalf("retained claim=%#v ok=%v err=%v", claim, ok, err)
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
	if _, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName); err != nil || ok {
		t.Fatalf("claim after retry ok=%v err=%v", ok, err)
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

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.ID != item.ID || lease.LeaseID != leaseID {
		t.Fatalf("lease=%#v", lease)
	}
}

func TestResolvePendingRecoveryRejectsDuplicateVisibleDroplets(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123459"
	slug := "duplicate"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
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

func TestReleasePersistsPendingRecoveryBeforeKeyCleanupFailure(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123460"
	slug := "cleanup-retry"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	item := droplet{ID: 111, Name: core.LeaseProviderName(leaseID, slug), Status: "active", Tags: tagsFromLabels(labels)}
	api := &fakeDigitalOceanAPI{
		droplets:     []droplet{item},
		keyDeleteErr: errors.New("key cleanup failed"),
	}
	backend := newTestBackend(t, api)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, core.Server{
		Provider: providerName,
		Name:     item.Name,
		Labels:   labels,
	}, core.SSHTarget{}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}

	lease := core.LeaseTarget{LeaseID: leaseID, Server: serverFromDroplet(item, cfg)}
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
	if len(api.deletedKeys) != 1 || !strings.HasPrefix(api.deletedKeys[0], "crabbox-cbx-") {
		t.Fatalf("deletedKeys=%v", api.deletedKeys)
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
	if len(api.deletedKeys) != 1 || api.keyDeleteDone {
		t.Fatalf("deletedKeys=%v keyDeleteDone=%v", api.deletedKeys, api.keyDeleteDone)
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
	if len(api.deletedKeys) != 1 || !strings.HasPrefix(api.deletedKeys[0], "crabbox-cbx-") {
		t.Fatalf("deletedKeys=%v", api.deletedKeys)
	}
}

func TestAcquireRollsBackKeepDropletAndKeyOnClaimFailure(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	backend := newTestBackend(t, api)
	claimErr := errors.New("claim failed")
	oldClaim := claimLeaseTargetForRepoConfig
	claimLeaseTargetForRepoConfig = func(string, string, core.Config, core.Server, core.SSHTarget, string, time.Duration, bool) error {
		return claimErr
	}
	t.Cleanup(func() { claimLeaseTargetForRepoConfig = oldClaim })

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "rollback-claim", Keep: true})
	if err == nil || !strings.Contains(err.Error(), "claim failed") {
		t.Fatalf("Acquire err=%v", err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 100 {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if len(api.deletedKeys) != 1 || !strings.HasPrefix(api.deletedKeys[0], "crabbox-cbx-") {
		t.Fatalf("deletedKeys=%v", api.deletedKeys)
	}
}

func TestResolveBySlugAndReleaseDeletesOwnedDroplet(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-1vcpu-1gb"
	item := droplet{ID: 77, Name: "crabbox-resolve", Status: "active", Tags: leaseTags(cfg, "cbx_abcdef123456", "resolve-me", "ready", false, time.Now())}
	item.Networks.V4 = append(item.Networks.V4, struct {
		IPAddress string `json:"ip_address"`
		Type      string `json:"type"`
	}{IPAddress: "203.0.113.77", Type: "public"})
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "resolve-me"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_abcdef123456" || lease.SSH.Host != "203.0.113.77" {
		t.Fatalf("lease=%#v", lease)
	}
	if err := core.ClaimLeaseTargetForRepoConfig(lease.LeaseID, "resolve-me", backend.Cfg, lease.Server, lease.SSH, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
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
	item := droplet{ID: 77, Name: "crabbox-retry-cleanup", Status: "active", Tags: leaseTags(cfg, leaseID, slug, "ready", false, time.Now())}
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
	ssh := core.SSHTargetFromConfig(backend.Cfg, server.PublicNet.IPv4.IP)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, backend.Cfg, server, ssh, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	keyPath := writeStoredTestboxKey(t, leaseID)

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
	if len(api.deleted) != 2 || api.deleted[1] != 77 || len(api.deletedKeys) != 2 {
		t.Fatalf("deleted=%v deletedKeys=%v", api.deleted, api.deletedKeys)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName); err != nil || ok {
		t.Fatalf("claim after successful retry ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stored key still exists after retry: %v", err)
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

func TestResolvePrefersNumericSlugOverDropletID(t *testing.T) {
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
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.ID != 456 || lease.LeaseID != "cbx_abcdef123456" {
		t.Fatalf("lease=%#v", lease)
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
	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{Server: server, LeaseID: "cbx_9"}})
	if err == nil {
		t.Fatal("ReleaseLease accepted unowned server")
	}
	if len(api.deleted) != 0 {
		t.Fatalf("deleted=%v", api.deleted)
	}
}

func TestReleaseRemovesStoredKeyWithoutClaim(t *testing.T) {
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
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stored key still exists after unclaimed release: %v", err)
	}
}

func TestReleasePersistsClaimBeforeUnclaimedDeleteFailure(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123461"
	item := droplet{ID: 92, Name: "retry-delete", Status: "active", Tags: leaseTags(cfg, leaseID, "retry-delete", "ready", false, time.Now())}
	api := &fakeDigitalOceanAPI{
		droplets:  []droplet{item},
		deleteErr: errors.New("delete response lost"),
	}
	backend := newTestBackend(t, api)
	keyPath := writeStoredTestboxKey(t, leaseID)
	lease := core.LeaseTarget{Server: serverFromDroplet(item, backend.Cfg), LeaseID: leaseID}

	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "delete response lost") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok || claim.CloudID != strconv.FormatInt(item.ID, 10) {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key removed after failed delete: %v", err)
	}

	api.deleteErr = nil
	retry, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: retry}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName); err != nil || ok {
		t.Fatalf("claim after retry ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stored key after retry: %v", err)
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
	api.droplets[0].Tags = removeTag(api.droplets[0].Tags, "crabbox:target:linux")

	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		Server:  server,
		LeaseID: "cbx_abcdef123456",
	}})
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
	item := droplet{ID: 88, Name: "old", Status: "active", Tags: leaseTags(cfg, "cbx_deadbeef1234", "old", "ready", false, time.Now().Add(-time.Hour))}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	server := serverFromDroplet(item, backend.Cfg)
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
	leaseID := "cbx_shared123456"
	item := droplet{ID: 89, Name: "old", Status: "active", Tags: leaseTags(cfg, leaseID, "old", "ready", false, time.Now().Add(-time.Hour))}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	otherCfg := core.BaseConfig()
	otherCfg.Provider = "aws"
	otherServer := core.Server{CloudID: "i-123", Provider: "aws"}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "other-provider", otherCfg, otherServer, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	keyPath := writeStoredTestboxKey(t, leaseID)

	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 89 {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(leaseID, "aws"); err != nil || !ok {
		t.Fatalf("other provider claim after cleanup ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("other provider key removed by cleanup: %v", err)
	}
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

func TestReleaseDerivesProviderKeyFromLeaseID(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	leaseID := "cbx_abcdef123456"
	cfg.ProviderKey = "crabbox-cbx-deadbeef1234"
	item := droplet{ID: 104, Name: "changed-key", Status: "active", Tags: leaseTags(cfg, leaseID, "changed-key", "ready", false, time.Now())}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	server := serverFromDroplet(item, backend.Cfg)

	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		Server:  server,
		LeaseID: leaseID,
	}}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedKeys) != 1 || api.deletedKeys[0] != providerKeyForLease(leaseID) {
		t.Fatalf("deletedKeys=%v", api.deletedKeys)
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
	os.Exit(m.Run())
}
