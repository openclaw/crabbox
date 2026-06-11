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
	created        []droplet
	deleted        []int64
	deletedKeys    []string
	tagged         []int64
	taggedTags     [][]string
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

func (f *fakeDigitalOceanAPI) DeleteDroplet(_ context.Context, id int64) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

func (f *fakeDigitalOceanAPI) DeleteSSHKeyByName(_ context.Context, name string) error {
	f.deletedKeys = append(f.deletedKeys, name)
	return nil
}

func (f *fakeDigitalOceanAPI) TagDroplet(_ context.Context, id int64, tags []string) error {
	f.tagged = append(f.tagged, id)
	f.taggedTags = append(f.taggedTags, append([]string(nil), tags...))
	for i := range f.created {
		if f.created[i].ID == id {
			f.created[i].Tags = tags
		}
	}
	return nil
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
	if len(api.tagged) != 1 || api.tagged[0] != 100 {
		t.Fatalf("tagged=%v", api.tagged)
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
	if len(api.tagged) != 1 || api.tagged[0] != 99 {
		t.Fatalf("tagged=%v", api.tagged)
	}
	decoded := labelsFromTags(api.taggedTags[0])
	if decoded["state"] != "running" || decoded["idle_timeout_secs"] != "1200" {
		t.Fatalf("persisted labels=%v tags=%v", decoded, api.taggedTags[0])
	}
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
