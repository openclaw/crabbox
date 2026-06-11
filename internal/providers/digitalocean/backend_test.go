package digitalocean

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

type fakeDigitalOceanAPI struct {
	droplets       []droplet
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
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 77 {
		t.Fatalf("deleted=%v", api.deleted)
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

func TestCleanupDryRunDoesNotDelete(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.TTL = time.Nanosecond
	item := droplet{ID: 88, Name: "old", Status: "active", Tags: leaseTags(cfg, "cbx_deadbeef1234", "old", "ready", false, time.Now().Add(-time.Hour))}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("dry-run deleted=%v", api.deleted)
	}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 88 {
		t.Fatalf("deleted=%v", api.deleted)
	}
}

func TestTouchPersistsDigitalOceanTags(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-1vcpu-1gb"
	cfg.TTL = time.Hour
	cfg.IdleTimeout = time.Minute
	item := droplet{ID: 99, Name: "touch", Status: "active", Tags: leaseTags(cfg, "cbx_abcdef123456", "touch-me", "ready", false, time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC))}
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := newTestBackend(t, api)
	backend.RT.Clock = fixedClock{t: time.Date(2026, 6, 10, 12, 10, 0, 0, time.UTC)}

	server := serverFromDroplet(item, backend.Cfg)
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
	if decoded["state"] != "running" || decoded["idle_timeout_secs"] != "1200" {
		t.Fatalf("persisted labels=%v tags=%v", decoded, api.replacedTo[0])
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

	applyDigitalOceanDefaults(&cfg)

	if cfg.DigitalOcean.Region != "nyc3" {
		t.Fatalf("DigitalOcean.Region=%q want %q", cfg.DigitalOcean.Region, "nyc3")
	}
	if cfg.DigitalOcean.Image != "ubuntu-24-04-x64" {
		t.Fatalf("DigitalOcean.Image=%q want %q", cfg.DigitalOcean.Image, "ubuntu-24-04-x64")
	}
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
