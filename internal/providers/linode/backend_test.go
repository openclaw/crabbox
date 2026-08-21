package linode

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeClock struct {
	t time.Time
}

func (c fakeClock) Now() time.Time { return c.t }

type fakeLinodeAPI struct {
	linodes            []linodeInstance
	accountID          string
	accountErr         error
	accountSettings    accountSettings
	accountSettingsErr error
	nextID             int64
	createErr          error
	getErr             error
	getFn              func(context.Context, int64) (linodeInstance, error)
	deleteErr          error
	deleteFn           func(context.Context, int64) error
	updateErr          error
	updateCalls        int
	created            []linodeInstance
	deleted            []int64
	updated            []int64
	updatedTags        [][]string
	createRequests     []createLinodeRequest
}

func (f *fakeLinodeAPI) AccountID(context.Context) (string, error) {
	if f.accountErr != nil {
		return "", f.accountErr
	}
	if f.accountID != "" {
		return f.accountID, nil
	}
	return "euuid:A1BC2DEF-34GH-567I-J890KLMN12O34P56", nil
}

func (f *fakeLinodeAPI) AccountSettings(context.Context) (accountSettings, error) {
	if f.accountSettingsErr != nil {
		return accountSettings{}, f.accountSettingsErr
	}
	if f.accountSettings.InterfacesForNewLinodes == "" {
		return accountSettings{InterfacesForNewLinodes: "legacy_config_default_but_linode_allowed"}, nil
	}
	return f.accountSettings, nil
}

func (f *fakeLinodeAPI) ListLinodes(context.Context) ([]linodeInstance, error) {
	return append(append([]linodeInstance(nil), f.linodes...), f.created...), nil
}

func (f *fakeLinodeAPI) GetLinode(ctx context.Context, id int64) (linodeInstance, error) {
	if f.getFn != nil {
		return f.getFn(ctx, id)
	}
	if f.getErr != nil {
		return linodeInstance{}, f.getErr
	}
	for _, item := range append(append([]linodeInstance(nil), f.linodes...), f.created...) {
		if item.ID == id {
			return item, nil
		}
	}
	return linodeInstance{}, &linodeAPIError{Status: 404}
}

func (f *fakeLinodeAPI) CreateLinode(_ context.Context, req createLinodeRequest) (linodeInstance, error) {
	f.createRequests = append(f.createRequests, req)
	if f.createErr != nil {
		return linodeInstance{}, f.createErr
	}
	if f.nextID == 0 {
		f.nextID = 100
	}
	item := linodeInstance{
		ID:     f.nextID,
		Label:  req.Label,
		Status: "running",
		Region: req.Region,
		Type:   req.Type,
		Image:  req.Image,
		Tags:   append([]string(nil), req.Tags...),
		IPv4:   []string{"203.0.113.10"},
	}
	f.created = append(f.created, item)
	f.nextID++
	return item, nil
}

func (f *fakeLinodeAPI) DeleteLinode(ctx context.Context, id int64) error {
	f.deleted = append(f.deleted, id)
	if f.deleteFn != nil {
		if err := f.deleteFn(ctx, id); err != nil {
			return err
		}
	}
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.linodes = removeLinodeByID(f.linodes, id)
	f.created = removeLinodeByID(f.created, id)
	return nil
}

func (f *fakeLinodeAPI) UpdateLinodeTags(_ context.Context, id int64, tags []string) error {
	f.updateCalls++
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updated = append(f.updated, id)
	f.updatedTags = append(f.updatedTags, append([]string(nil), tags...))
	for i := range f.linodes {
		if f.linodes[i].ID == id {
			f.linodes[i].Tags = append([]string(nil), tags...)
		}
	}
	for i := range f.created {
		if f.created[i].ID == id {
			f.created[i].Tags = append([]string(nil), tags...)
		}
	}
	return nil
}

func removeLinodeByID(items []linodeInstance, id int64) []linodeInstance {
	out := items[:0]
	for _, item := range items {
		if item.ID != id {
			out = append(out, item)
		}
	}
	return out
}

func newTestBackend(t *testing.T, api *fakeLinodeAPI) *linodeLeaseBackend {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "g6-standard-1"
	cfg.SSHUser = "root"
	cfg.WorkRoot = "/work/crabbox"
	backend := newLinodeLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{
		Stderr: io.Discard,
		Clock:  fakeClock{t: time.Unix(1700000000, 0).UTC()},
	})
	backend.clientFactory = func(core.Runtime) (linodeAPI, error) { return api, nil }
	backend.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error { return nil }
	return backend
}

func TestWaitForLinodeIP(t *testing.T) {
	t.Run("pending to ready", func(t *testing.T) {
		calls := 0
		api := &fakeLinodeAPI{getFn: func(context.Context, int64) (linodeInstance, error) {
			calls++
			if calls == 1 {
				return linodeInstance{ID: 42, Status: "provisioning"}, nil
			}
			return linodeInstance{ID: 42, Status: "offline", IPv4: []string{"203.0.113.42"}}, nil
		}}
		got, err := newTestBackend(t, api).waitForLinodeIP(context.Background(), api, 42, time.Minute)
		if err != nil || got.ID != 42 || calls != 2 {
			t.Fatalf("instance=%#v err=%v calls=%d", got, err, calls)
		}
	})

	t.Run("read error", func(t *testing.T) {
		wantErr := errors.New("read denied")
		calls := 0
		api := &fakeLinodeAPI{getFn: func(context.Context, int64) (linodeInstance, error) {
			calls++
			return linodeInstance{}, wantErr
		}}
		_, err := newTestBackend(t, api).waitForLinodeIP(context.Background(), api, 42, time.Minute)
		if !errors.Is(err, wantErr) || calls != 1 {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	})

	t.Run("client deadline", func(t *testing.T) {
		api := &fakeLinodeAPI{getErr: context.DeadlineExceeded}
		_, err := newTestBackend(t, api).waitForLinodeIP(context.Background(), api, 42, time.Minute)
		if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timed out waiting") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("read error at deadline", func(t *testing.T) {
		wantErr := errors.New("late read denied")
		api := &fakeLinodeAPI{getFn: func(ctx context.Context, _ int64) (linodeInstance, error) {
			<-ctx.Done()
			return linodeInstance{}, wantErr
		}}
		_, err := newTestBackend(t, api).waitForLinodeIP(context.Background(), api, 42, 10*time.Millisecond)
		if !errors.Is(err, wantErr) || strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("cancellation during delay", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		api := &fakeLinodeAPI{getFn: func(context.Context, int64) (linodeInstance, error) {
			calls++
			time.AfterFunc(time.Millisecond, cancel)
			return linodeInstance{ID: 42, Status: "provisioning"}, nil
		}}
		_, err := newTestBackend(t, api).waitForLinodeIP(ctx, api, 42, time.Minute)
		if !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		api := &fakeLinodeAPI{linodes: []linodeInstance{{ID: 42, Status: "provisioning"}}}
		_, err := newTestBackend(t, api).waitForLinodeIP(context.Background(), api, 42, 10*time.Millisecond)
		if err == nil || err.Error() != "timed out waiting for Linode instance IP" {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestAcquireCreatesLinodeClaimsLeaseAndMarksReady(t *testing.T) {
	api := &fakeLinodeAPI{}
	backend := newTestBackend(t, api)

	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "my-app", Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID == "" || lease.Server.ID != 100 || lease.SSH.Host != "203.0.113.10" || lease.SSH.User != "root" {
		t.Fatalf("lease=%#v", lease)
	}
	if len(api.createRequests) != 1 {
		t.Fatalf("createRequests=%#v", api.createRequests)
	}
	req := api.createRequests[0]
	if req.Label != core.LeaseProviderName(lease.LeaseID, "my-app") || req.Region != defaultRegion || req.Type != defaultType || req.Image != defaultImage {
		t.Fatalf("request=%#v", req)
	}
	if len(req.AuthorizedKeys) != 1 || !strings.HasPrefix(req.AuthorizedKeys[0], "ssh-ed25519 ") {
		t.Fatalf("authorized_keys=%v", req.AuthorizedKeys)
	}
	if req.RootPass == "" || len(req.RootPass) < 16 {
		t.Fatalf("root_pass not generated safely: %q", req.RootPass)
	}
	if req.Metadata == nil || req.Metadata.UserData == "" {
		t.Fatalf("metadata=%#v", req.Metadata)
	}
	if labels := labelsFromTags(req.Tags); labels["lease"] != lease.LeaseID || labels["slug"] != "my-app" || labels["state"] != "provisioning" {
		t.Fatalf("create labels=%v", labels)
	}
	if lease.Server.Labels["state"] != "ready" || lease.Server.Labels[linodeAccountLabel] != "euuid:A1BC2DEF-34GH-567I-J890KLMN12O34P56" {
		t.Fatalf("lease labels=%v", lease.Server.Labels)
	}
	if len(api.updated) != 1 || api.updated[0] != 100 {
		t.Fatalf("updated=%v", api.updated)
	}
	if labels := labelsFromTags(api.updatedTags[0]); labels["state"] != "ready" || labels["lease"] != lease.LeaseID {
		t.Fatalf("ready tags labels=%v", labels)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider("my-app", providerName)
	if err != nil || !ok || claim.CloudID != "100" || claim.Labels[linodeAccountLabel] != "euuid:A1BC2DEF-34GH-567I-J890KLMN12O34P56" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestAcquireRecordsConfiguredLinodeTypeInMetadata(t *testing.T) {
	api := &fakeLinodeAPI{}
	backend := newTestBackend(t, api)
	backend.Cfg.Linode.Type = "g6-standard-2"

	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "custom-type"})
	if err != nil {
		t.Fatal(err)
	}
	if len(api.createRequests) != 1 || api.createRequests[0].Type != "g6-standard-2" {
		t.Fatalf("createRequests=%#v", api.createRequests)
	}
	if lease.Server.ServerType.Name != "g6-standard-2" || lease.Server.Labels["server_type"] != "g6-standard-2" {
		t.Fatalf("lease server type=%#v labels=%v", lease.Server.ServerType, lease.Server.Labels)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider("custom-type", providerName)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if claim.Labels["server_type"] != "g6-standard-2" {
		t.Fatalf("claim labels=%v", claim.Labels)
	}
}

func TestAcquireRejectsInvalidFirewallBeforeCreate(t *testing.T) {
	api := &fakeLinodeAPI{}
	backend := newTestBackend(t, api)
	backend.Cfg.Linode.FirewallID = "prod-ssh"

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "bad-firewall"})
	if err == nil || !strings.Contains(err.Error(), "linode firewall must be a positive numeric firewall ID") {
		t.Fatalf("Acquire err=%v", err)
	}
	if len(api.createRequests) != 0 {
		t.Fatalf("createRequests=%#v", api.createRequests)
	}
}

func TestAcquireConfiguresFirewallForLegacyInterfaces(t *testing.T) {
	api := &fakeLinodeAPI{
		accountSettings: accountSettings{InterfacesForNewLinodes: "legacy_config_default_but_linode_allowed"},
	}
	backend := newTestBackend(t, api)
	backend.Cfg.Linode.FirewallID = "123"

	if _, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "legacy-firewall"}); err != nil {
		t.Fatal(err)
	}
	req := api.createRequests[0]
	if req.InterfaceGeneration != "legacy_config" || req.FirewallID != 123 || len(req.Interfaces) != 0 {
		t.Fatalf("request=%#v", req)
	}
}

func TestAcquireConfiguresFirewallForLinodeInterfaces(t *testing.T) {
	api := &fakeLinodeAPI{
		accountSettings: accountSettings{InterfacesForNewLinodes: "linode_only"},
	}
	backend := newTestBackend(t, api)
	backend.Cfg.Linode.FirewallID = "456"

	if _, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "linode-firewall"}); err != nil {
		t.Fatal(err)
	}
	req := api.createRequests[0]
	if req.InterfaceGeneration != "linode" || req.FirewallID != 0 || len(req.Interfaces) != 1 ||
		req.Interfaces[0].FirewallID == nil || *req.Interfaces[0].FirewallID != 456 || req.Interfaces[0].Public == nil {
		t.Fatalf("request=%#v", req)
	}
}

func TestAcquireRejectsUnsupportedExplicitPortableOSBeforeCreate(t *testing.T) {
	home := t.TempDir()
	configPath := home + "/config.yaml"
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("CRABBOX_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("provider: linode\nos: ubuntu:26.04\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := core.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Linode.Image != "" {
		t.Fatalf("Linode.Image=%q, want unresolved provider image", cfg.Linode.Image)
	}
	api := &fakeLinodeAPI{}
	backend := newLinodeLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard})
	backend.clientFactory = func(core.Runtime) (linodeAPI, error) { return api, nil }

	_, err = backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "bad-os"})
	if err == nil || !strings.Contains(err.Error(), `provider=linode does not support os "ubuntu:26.04"`) {
		t.Fatalf("Acquire err=%v", err)
	}
	if len(api.createRequests) != 0 {
		t.Fatalf("createRequests=%#v", api.createRequests)
	}
}

func TestListFiltersForeignAndPartialLinodes(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	owned := linodeInstance{ID: 1, Label: "crabbox-cbx-111111111111-owned", Status: "running", Type: "g6-standard-1", IPv4: []string{"203.0.113.1"}, Tags: leaseTags(cfg, "cbx_111111111111", "owned", "ready", false, time.Unix(1, 0))}
	partial := linodeInstance{ID: 2, Label: "partial", Tags: []string{tagCrabbox, "crabbox:provider:linode"}}
	foreign := linodeInstance{ID: 3, Label: "foreign", Tags: []string{tagCrabbox, "crabbox:provider:aws", "crabbox:target:linux", "crabbox:lease:cbx_two", "crabbox:slug:foreign"}}
	backend := newTestBackend(t, &fakeLinodeAPI{linodes: []linodeInstance{owned, partial, foreign}})

	views, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].ID != 1 || views[0].Status != "ready" {
		t.Fatalf("views=%#v", views)
	}
}

func TestResolveBySlugAndReleaseDeleteOwnedLinode(t *testing.T) {
	api := &fakeLinodeAPI{}
	backend := newTestBackend(t, api)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "delete-me"})
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "delete-me", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.LeaseID != lease.LeaseID || resolved.Server.ID != lease.Server.ID {
		t.Fatalf("resolved=%#v lease=%#v", resolved, lease)
	}
	resolvedClaim, exists, set := core.ServerLeaseClaimSnapshot(resolved.Server)
	if !set || !exists || resolvedClaim.Revision == "" || resolvedClaim.LeaseID != lease.LeaseID {
		t.Fatalf("resolved claim=%#v exists=%v set=%v", resolvedClaim, exists, set)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: resolved}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 100 {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("delete-me", providerName); err != nil || ok {
		t.Fatalf("claim after release ok=%v err=%v", ok, err)
	}
	keyPath, pathErr := core.TestboxKeyPath(lease.LeaseID)
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("local key retained after release: %v", statErr)
	}
}

func TestResolveNumericSlugBeforeRawLinodeID(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = defaultType
	item := linodeInstance{ID: 456, Label: core.LeaseProviderName("cbx_abcdef123456", "123"), Status: "running", Type: defaultType, IPv4: []string{"203.0.113.123"}, Tags: leaseTags(cfg, "cbx_abcdef123456", "123", "ready", false, time.Now())}
	api := &fakeLinodeAPI{linodes: []linodeInstance{item}}
	backend := newTestBackend(t, api)

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "123", Repo: core.Repo{Root: t.TempDir()}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_abcdef123456" || lease.Server.ID != 456 || lease.Server.Labels["slug"] != "123" {
		t.Fatalf("lease=%#v", lease)
	}
}

func TestReleaseCanonicalIdentifierDoesNotFallBackToClaimSlug(t *testing.T) {
	api := &fakeLinodeAPI{}
	backend := newTestBackend(t, api)
	const (
		requestedID = "cbx_aaaaaaaaaaaa"
		lookalikeID = "cbx_bbbbbbbbbbbb"
	)
	labels := core.DirectLeaseLabels(backend.Cfg, lookalikeID, requestedID, providerName, "", false, time.Now())
	labels[linodeAccountLabel] = "euuid:A1BC2DEF-34GH-567I-J890KLMN12O34P56"
	server := core.Server{
		Provider: providerName,
		CloudID:  "456",
		ID:       456,
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
	backend := newTestBackend(t, &fakeLinodeAPI{})
	const leaseID = "cbx_abcdef123476"
	labels := core.DirectLeaseLabels(backend.Cfg, leaseID, "wrong-scope", providerName, "", false, time.Now())
	labels[linodeAccountLabel] = "euuid:account-a"
	server := core.Server{Provider: providerName, CloudID: "476", ID: 476, Name: core.LeaseProviderName(leaseID, "wrong-scope"), Labels: labels}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "wrong-scope", providerName, "account:other", "", t.TempDir(), time.Minute, false, server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	_, err := backend.releaseTargetFromClaim(context.Background(), &fakeLinodeAPI{}, leaseID, "euuid:account-a")
	if err == nil || !strings.Contains(err.Error(), "exact lease identifier") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveReadOnlyDoesNotClaimVisibleLinode(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = defaultType
	leaseID := "cbx_abcdef123457"
	slug := "read-only"
	item := linodeInstance{ID: 457, Label: core.LeaseProviderName(leaseID, slug), Status: "running", Type: defaultType, IPv4: []string{"203.0.113.124"}, Tags: leaseTags(cfg, leaseID, slug, "ready", false, time.Now())}
	backend := newTestBackend(t, &fakeLinodeAPI{linodes: []linodeInstance{item}})

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

func TestResolveReadOnlyIgnoresStaleLinodeClaim(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = defaultType
	leaseID := "cbx_abcdef123458"
	slug := "stale-read-only"
	item := linodeInstance{ID: 458, Label: core.LeaseProviderName(leaseID, slug), Status: "running", Type: defaultType, IPv4: []string{"203.0.113.125"}, Tags: leaseTags(cfg, leaseID, slug, "ready", false, time.Now())}
	backend := newTestBackend(t, &fakeLinodeAPI{linodes: []linodeInstance{item}})
	labels := labelsFromTags(item.Tags)
	labels[linodeAccountLabel] = "euuid:A1BC2DEF-34GH-567I-J890KLMN12O34P56"
	claimServer := core.Server{Provider: providerName, CloudID: "999", ID: 999, Name: item.Label, Labels: labels}
	if err := core.ClaimLeaseTargetForConfig(leaseID, slug, backend.Cfg, claimServer, core.SSHTarget{}, backend.Cfg.IdleTimeout); err != nil {
		t.Fatal(err)
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
	backend := &linodeLeaseBackend{}
	claim := core.LeaseClaim{Labels: map[string]string{linodeAccountLabel: "euuid:account-a"}}
	lease := core.LeaseTarget{Server: core.Server{Labels: map[string]string{linodeAccountLabel: "euuid:account-a"}}}
	if !backend.StatusTouchClaimMatches(lease, claim) {
		t.Fatal("matching account identity was rejected")
	}
	lease.Server.Labels[linodeAccountLabel] = "euuid:account-b"
	if backend.StatusTouchClaimMatches(lease, claim) {
		t.Fatal("mismatched account identity was accepted")
	}
	delete(claim.Labels, linodeAccountLabel)
	if backend.StatusTouchClaimMatches(lease, claim) {
		t.Fatal("missing claim account identity was accepted")
	}
}

func TestResolveReleaseOnlyNumericSlugClaimBeforeRawLinodeID(t *testing.T) {
	leaseID := "cbx_222222222222"
	slug := "123"
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	labels[linodeAccountLabel] = "euuid:A1BC2DEF-34GH-567I-J890KLMN12O34P56"
	server := core.Server{
		Provider: providerName,
		CloudID:  "456",
		ID:       456,
		Name:     core.LeaseProviderName(leaseID, slug),
		Labels:   labels,
	}
	api := &fakeLinodeAPI{}
	backend := newTestBackend(t, api)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.Server.ID != 456 || lease.Server.Labels["slug"] != slug {
		t.Fatalf("lease=%#v", lease)
	}
}

func TestReleaseRefusesAccountMismatch(t *testing.T) {
	api := &fakeLinodeAPI{accountID: "euuid:first"}
	backend := newTestBackend(t, api)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "mismatch"})
	if err != nil {
		t.Fatal(err)
	}

	api.accountID = "euuid:second"
	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
	if err == nil || !strings.Contains(err.Error(), "account mismatch") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("deleted=%v", api.deleted)
	}
}

func TestReleaseMissingLiveLinodeFinalizesLocalClaim(t *testing.T) {
	leaseID := "cbx_333333333333"
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	labels := core.DirectLeaseLabels(cfg, leaseID, "gone", providerName, "", false, time.Now())
	labels[linodeAccountLabel] = "euuid:A1BC2DEF-34GH-567I-J890KLMN12O34P56"
	server := core.Server{
		Provider: providerName,
		CloudID:  "123",
		ID:       123,
		Name:     core.LeaseProviderName(leaseID, "gone"),
		Labels:   labels,
	}
	api := &fakeLinodeAPI{}
	backend := newTestBackend(t, api)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "gone", cfg, server, core.SSHTarget{}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	keyPath, _, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if claim, exists, set := core.ServerLeaseClaimSnapshot(resolved.Server); !set || !exists || claim.Revision == "" {
		t.Fatalf("resolved claim=%#v exists=%v set=%v", claim, exists, set)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: resolved}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("deleted missing linode=%v", api.deleted)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName); err != nil || ok {
		t.Fatalf("claim after release ok=%v err=%v", ok, err)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("local key retained after release: %v", statErr)
	}
}

func TestCleanupDryRunSkipsKeepAndDeletesExpiredWhenLive(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	expiredLabels := core.DirectLeaseLabels(cfg, "cbx_444444444444", "expired", providerName, "", false, now.Add(-2*time.Hour))
	expiredLabels["state"] = "ready"
	expiredLabels[linodeAccountLabel] = "euuid:A1BC2DEF-34GH-567I-J890KLMN12O34P56"
	expiredLabels["expires_at"] = "1"
	claimlessLabels := core.DirectLeaseLabels(cfg, "cbx_333333333333", "claimless", providerName, "", false, now.Add(-2*time.Hour))
	claimlessLabels["state"] = "ready"
	claimlessLabels[linodeAccountLabel] = "euuid:A1BC2DEF-34GH-567I-J890KLMN12O34P56"
	claimlessLabels["expires_at"] = "1"
	staleLabels := core.DirectLeaseLabels(cfg, "cbx_222222222222", "stale-account", providerName, "", false, now.Add(-2*time.Hour))
	staleLabels["state"] = "ready"
	staleLabels["expires_at"] = "1"
	keepLabels := core.DirectLeaseLabels(cfg, "cbx_555555555555", "keep", providerName, "", true, now.Add(-2*time.Hour))
	keepLabels["state"] = "ready"
	keepLabels[linodeAccountLabel] = "euuid:A1BC2DEF-34GH-567I-J890KLMN12O34P56"
	api := &fakeLinodeAPI{linodes: []linodeInstance{
		{ID: 8, Label: core.LeaseProviderName("cbx_222222222222", "stale-account"), Status: "running", Type: "g6-standard-1", IPv4: []string{"203.0.113.8"}, Tags: tagsFromLabels(staleLabels)},
		{ID: 9, Label: core.LeaseProviderName("cbx_333333333333", "claimless"), Status: "running", Type: "g6-standard-1", IPv4: []string{"203.0.113.9"}, Tags: tagsFromLabels(claimlessLabels)},
		{ID: 10, Label: core.LeaseProviderName("cbx_444444444444", "expired"), Status: "running", Type: "g6-standard-1", IPv4: []string{"203.0.113.10"}, Tags: tagsFromLabels(expiredLabels)},
		{ID: 11, Label: core.LeaseProviderName("cbx_555555555555", "keep"), Status: "running", Type: "g6-standard-1", IPv4: []string{"203.0.113.11"}, Tags: tagsFromLabels(keepLabels)},
	}}
	backend := newTestBackend(t, api)
	backend.RT.Clock = fakeClock{t: now}
	staleServer := serverFromLinode(api.linodes[0], cfg)
	staleServer.Labels[linodeAccountLabel] = "euuid:stale-account"
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_222222222222", "stale-account", cfg, staleServer, core.SSHTarget{}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}

	if err := backend.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("dry-run deleted=%v", api.deleted)
	}
	server := serverFromLinode(api.linodes[2], cfg)
	server.Labels[linodeAccountLabel] = expiredLabels[linodeAccountLabel]
	if err := core.ClaimLeaseTargetForRepoConfig(
		"cbx_444444444444",
		"expired",
		cfg,
		server,
		core.SSHTarget{},
		t.TempDir(),
		cfg.IdleTimeout,
		false,
	); err != nil {
		t.Fatal(err)
	}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 10 {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("cbx_222222222222", providerName); err != nil || !ok {
		t.Fatalf("stale-account claim after cleanup ok=%v err=%v", ok, err)
	}
}

func TestPrepareCleanupCarriesExactClaimSnapshot(t *testing.T) {
	api := &fakeLinodeAPI{}
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
	if err != nil {
		t.Fatal(err)
	}
	if !set || !exists || !currentExists || snapshot.Revision == "" || !reflect.DeepEqual(snapshot, current) {
		t.Fatalf("snapshot=%#v exists=%v set=%v current=%#v currentExists=%v", snapshot, exists, set, current, currentExists)
	}
}

func TestDeleteRefusesClaimsChangedAfterPreparation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*core.LeaseClaim)
	}{
		{"renewed", func(claim *core.LeaseClaim) { claim.LastUsedAt = "2030-01-02T03:04:05Z" }},
		{"repo", func(claim *core.LeaseClaim) { claim.RepoRoot = "/different/repo" }},
		{"labels", func(claim *core.LeaseClaim) {
			claim.Labels = maps.Clone(claim.Labels)
			claim.Labels["state"] = "running"
		}},
		{"revision", func(*core.LeaseClaim) {}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &fakeLinodeAPI{}
			backend := newTestBackend(t, api)
			lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "changed-" + test.name})
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := backend.prepareCleanupServer(context.Background(), lease.Server)
			if err != nil {
				t.Fatal(err)
			}
			expected, exists, set := core.ServerLeaseClaimSnapshot(prepared)
			if !set || !exists {
				t.Fatalf("expected=%#v exists=%v set=%v", expected, exists, set)
			}
			replacement := expected
			test.mutate(&replacement)
			if err := core.ReplaceLeaseClaimIfUnchanged(lease.LeaseID, expected, replacement); err != nil {
				t.Fatal(err)
			}
			changed, changedExists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if err != nil || !changedExists || changed.Revision == expected.Revision {
				t.Fatalf("changed=%#v exists=%v err=%v", changed, changedExists, err)
			}
			err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: lease.LeaseID, Server: prepared}})
			if err == nil || !strings.Contains(err.Error(), "claim changed") {
				t.Fatalf("ReleaseLease err=%v", err)
			}
			if len(api.deleted) != 0 {
				t.Fatalf("deleted=%v", api.deleted)
			}
			preserved, preservedExists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if err != nil || !preservedExists || !reflect.DeepEqual(preserved, changed) {
				t.Fatalf("preserved=%#v exists=%v err=%v changed=%#v", preserved, preservedExists, err, changed)
			}
		})
	}
}

func TestClaimUpdateBlocksDuringDeleteThenFailsSafely(t *testing.T) {
	api := &fakeLinodeAPI{}
	backend := newTestBackend(t, api)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "locked-delete"})
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
		t.Fatalf("claim update completed while provider delete held claim lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowDelete)
	if err := <-releaseDone; err != nil {
		t.Fatal(err)
	}
	if err := <-updateDone; err == nil {
		t.Fatal("claim update succeeded after locked deletion removed the claim")
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v", exists, err)
	}
}

func TestDeleteErrorPreservesClaimAndStoredKey(t *testing.T) {
	api := &fakeLinodeAPI{deleteErr: errors.New("linode delete failed")}
	backend := newTestBackend(t, api)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "delete-error"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := backend.prepareCleanupServer(context.Background(), lease.Server)
	if err != nil {
		t.Fatal(err)
	}
	expected, _, _ := core.ServerLeaseClaimSnapshot(prepared)
	keyPath, err := core.TestboxKeyPath(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: lease.LeaseID, Server: prepared}}); err == nil || !strings.Contains(err.Error(), "linode delete failed") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists || !reflect.DeepEqual(claim, expected) {
		t.Fatalf("claim=%#v exists=%v err=%v expected=%#v", claim, exists, err, expected)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key missing after failed delete: %v", err)
	}
}

func TestReleaseWithoutClaimSnapshotFailsClosed(t *testing.T) {
	api := &fakeLinodeAPI{}
	backend := newTestBackend(t, api)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "missing-snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	server := serverFromLinode(api.created[0], backend.Cfg)
	server.Labels[linodeAccountLabel] = lease.Server.Labels[linodeAccountLabel]
	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: lease.LeaseID, Server: server}})
	if err == nil || !strings.Contains(err.Error(), "claim snapshot is missing") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || !exists {
		t.Fatalf("claim exists=%v err=%v", exists, err)
	}
}

func TestPendingRecoveryPublishesUpdatedClaimBeforeLockedDelete(t *testing.T) {
	backend, api, leaseID, server, original, keyPath := setupPendingLinodeRecovery(t, "pending-delete")
	prepared, err := backend.prepareCleanupServer(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, exists, set := core.ServerLeaseClaimSnapshot(prepared)
	if !set || !exists || snapshot.Revision != original.Revision || snapshot.CloudID != "" {
		t.Fatalf("snapshot=%#v exists=%v set=%v original=%#v", snapshot, exists, set, original)
	}
	api.deleteErr = errors.New("delete after recovery bind failed")
	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: prepared}})
	if err == nil || !strings.Contains(err.Error(), "delete after recovery bind failed") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	updated, updatedExists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !updatedExists || updated.CloudID != server.CloudID || updated.Revision == original.Revision {
		t.Fatalf("updated=%#v exists=%v err=%v original=%#v", updated, updatedExists, err, original)
	}
	if len(api.deleted) != 1 || api.deleted[0] != server.ID {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key missing after failed recovered delete: %v", err)
	}
}

func TestPendingRecoveryCleanupDryRunDoesNotMutate(t *testing.T) {
	backend, api, leaseID, _, original, keyPath := setupPendingLinodeRecovery(t, "pending-dry-run")
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	after, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || !reflect.DeepEqual(after, original) {
		t.Fatalf("after=%#v exists=%v err=%v original=%#v", after, exists, err, original)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("dry-run deleted=%v", api.deleted)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key missing after dry-run: %v", err)
	}
}

func setupPendingLinodeRecovery(t *testing.T, slug string) (*linodeLeaseBackend, *fakeLinodeAPI, string, core.Server, core.LeaseClaim, string) {
	t.Helper()
	api := &fakeLinodeAPI{}
	backend := newTestBackend(t, api)
	leaseID := "cbx_" + strings.Repeat("a", 12)
	labels := core.DirectLeaseLabels(backend.Cfg, leaseID, slug, providerName, "", false, time.Unix(1, 0))
	labels["state"] = "provisioning"
	labels["expires_at"] = "1"
	labels["recovery"] = "ambiguous-create"
	labels[linodeAccountLabel] = "euuid:A1BC2DEF-34GH-567I-J890KLMN12O34P56"
	claimServer := core.Server{Provider: providerName, Name: core.LeaseProviderName(leaseID, slug), Labels: maps.Clone(labels)}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, backend.Cfg, claimServer, core.SSHTarget{}, t.TempDir(), backend.Cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	original, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || original.Revision == "" {
		t.Fatalf("original=%#v exists=%v err=%v", original, exists, err)
	}
	item := linodeInstance{
		ID:     700,
		Label:  core.LeaseProviderName(leaseID, slug),
		Status: "running",
		Type:   defaultType,
		IPv4:   []string{"203.0.113.70"},
		Tags:   tagsFromLabels(labels),
	}
	api.linodes = []linodeInstance{item}
	server := serverFromLinode(item, backend.Cfg)
	server.Labels[linodeAccountLabel] = labels[linodeAccountLabel]
	keyPath, _, err := core.EnsureTestboxKeyForConfig(backend.Cfg, leaseID)
	if err != nil {
		t.Fatal(err)
	}
	return backend, api, leaseID, server, original, keyPath
}

func TestTouchPreservesLiveTailscaleTagsAndIdleTimeoutOverride(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = defaultType
	cfg.TTL = time.Hour
	cfg.IdleTimeout = time.Minute
	item := linodeInstance{ID: 99, Label: core.LeaseProviderName("cbx_abcdef123456", "touch-me"), Status: "running", Type: defaultType, IPv4: []string{"203.0.113.10"}, Tags: append(leaseTags(cfg, "cbx_abcdef123456", "touch-me", "ready", false, time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)), "customer:production")}
	server := serverFromLinode(item, cfg)
	server.Labels["tailscale_ipv4"] = "100.64.1.1"
	server.Labels["tailscale_fqdn"] = "stale.example.ts.net"
	server.Labels["tailscale_state"] = "requested"
	server.Labels["tailscale_tags"] = "tag:stale"
	server.Labels["tailscale_exit_node"] = "stale.example.ts.net"
	server.Labels[linodeAccountLabel] = "team:test-account"
	liveLabels := normalizedLinodeLabels(item.Tags)
	liveLabels["tailscale_ipv4"] = "100.64.1.2"
	liveLabels["tailscale_fqdn"] = "touch-me.example.ts.net"
	liveLabels["tailscale_state"] = "ready"
	liveLabels["tailscale_error"] = "last probe failed: retrying"
	liveLabels["tailscale_tags"] = "tag:ci,tag:crabbox"
	liveLabels["tailscale_exit_node"] = "exit.example.ts.net"
	item.Tags = append(tagsFromLabels(liveLabels), "customer:production")
	api := &fakeLinodeAPI{linodes: []linodeInstance{item}, accountID: "team:test-account"}
	backend := newTestBackend(t, api)
	backend.RT.Clock = fakeClock{t: time.Date(2026, 6, 10, 12, 10, 0, 0, time.UTC)}
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_abcdef123456", "touch-me", backend.Cfg, server, core.SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim("cbx_abcdef123456")
	if err != nil {
		t.Fatal(err)
	}
	core.SetServerLeaseClaimSnapshot(&server, claim, true)
	override := 20 * time.Minute

	touched, err := backend.Touch(context.Background(), core.TouchRequest{
		Lease:               core.LeaseTarget{Server: server, LeaseID: "cbx_abcdef123456"},
		State:               "running",
		IdleTimeout:         45 * time.Minute,
		IdleTimeoutOverride: &override,
	})
	if err != nil {
		t.Fatal(err)
	}
	if touched.Labels["state"] != "running" || touched.Labels["idle_timeout_secs"] != "1200" {
		t.Fatalf("touched labels=%v", touched.Labels)
	}
	if touched.Labels[linodeAccountLabel] != "team:test-account" {
		t.Fatalf("account label=%q", touched.Labels[linodeAccountLabel])
	}
	if len(api.updated) != 1 || api.updated[0] != 99 {
		t.Fatalf("updated=%v", api.updated)
	}
	if !containsString(api.updatedTags[0], "customer:production") {
		t.Fatalf("external tags not preserved: %v", api.updatedTags[0])
	}
	decoded := labelsFromTags(api.updatedTags[0])
	if decoded["state"] != "running" ||
		decoded["idle_timeout_secs"] != "1200" ||
		decoded["tailscale_ipv4"] != "100.64.1.2" ||
		decoded["tailscale_fqdn"] != "touch-me.example.ts.net" ||
		decoded["tailscale_state"] != "ready" ||
		decoded["tailscale_error"] != "last probe failed: retrying" ||
		decoded["tailscale_tags"] != "tag:ci,tag:crabbox" ||
		decoded["tailscale_exit_node"] != "exit.example.ts.net" {
		t.Fatalf("persisted labels=%v tags=%v", decoded, api.updatedTags[0])
	}
	snapshot, exists, set := core.ServerLeaseClaimSnapshot(touched)
	persisted, err := core.ReadLeaseClaim("cbx_abcdef123456")
	if err != nil || !set || !exists || !reflect.DeepEqual(snapshot, persisted) || persisted.Revision == claim.Revision || persisted.IdleTimeoutSeconds != 1200 {
		t.Fatalf("snapshot=%#v persisted=%#v exists=%t set=%t err=%v", snapshot, persisted, exists, set, err)
	}
}

func TestFencedLinodeMetadataRejectsInvalidOwnershipBeforeMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *fakeLinodeAPI, *core.LeaseTarget, core.LeaseClaim)
	}{
		{name: "missing snapshot", mutate: func(_ *testing.T, _ *fakeLinodeAPI, lease *core.LeaseTarget, _ core.LeaseClaim) {
			lease.Server = core.Server{Provider: lease.Server.Provider, CloudID: lease.Server.CloudID, ID: lease.Server.ID, Name: lease.Server.Name, Labels: lease.Server.Labels}
		}},
		{name: "false snapshot", mutate: func(_ *testing.T, _ *fakeLinodeAPI, lease *core.LeaseTarget, claim core.LeaseClaim) {
			core.SetServerLeaseClaimSnapshot(&lease.Server, claim, false)
		}},
		{name: "stale snapshot", mutate: func(_ *testing.T, _ *fakeLinodeAPI, lease *core.LeaseTarget, claim core.LeaseClaim) {
			claim.Revision = "stale"
			core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
		}},
		{name: "removed claim", mutate: func(_ *testing.T, _ *fakeLinodeAPI, lease *core.LeaseTarget, _ core.LeaseClaim) {
			core.RemoveLeaseClaim(lease.LeaseID)
		}},
		{name: "wrong lease", mutate: func(_ *testing.T, _ *fakeLinodeAPI, lease *core.LeaseTarget, _ core.LeaseClaim) {
			lease.LeaseID = "cbx_other_lease"
		}},
		{name: "wrong provider", mutate: func(_ *testing.T, _ *fakeLinodeAPI, lease *core.LeaseTarget, _ core.LeaseClaim) {
			lease.Server.Provider = "vultr"
		}},
		{name: "wrong claim provider", mutate: func(_ *testing.T, _ *fakeLinodeAPI, lease *core.LeaseTarget, claim core.LeaseClaim) {
			claim.Provider = "vultr"
			core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
		}},
		{name: "wrong claim scope", mutate: func(t *testing.T, _ *fakeLinodeAPI, lease *core.LeaseTarget, claim core.LeaseClaim) {
			claim.ProviderScope = "account:another-scope"
			replaceFencedLinodeClaim(t, lease, claim)
		}},
		{name: "wrong claim numeric identity", mutate: func(t *testing.T, _ *fakeLinodeAPI, lease *core.LeaseTarget, claim core.LeaseClaim) {
			claim.CloudNumericID = 999
			replaceFencedLinodeClaim(t, lease, claim)
		}},
		{name: "claim has no revision", mutate: func(_ *testing.T, _ *fakeLinodeAPI, lease *core.LeaseTarget, claim core.LeaseClaim) {
			claim.Revision = ""
			core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
		}},
		{name: "wrong cloud identity", mutate: func(_ *testing.T, _ *fakeLinodeAPI, lease *core.LeaseTarget, _ core.LeaseClaim) {
			lease.Server.CloudID = "999"
		}},
		{name: "wrong numeric identity", mutate: func(_ *testing.T, _ *fakeLinodeAPI, lease *core.LeaseTarget, _ core.LeaseClaim) {
			lease.Server.ID = 999
		}},
		{name: "missing cloud identity", mutate: func(_ *testing.T, _ *fakeLinodeAPI, lease *core.LeaseTarget, _ core.LeaseClaim) {
			lease.Server.CloudID = ""
		}},
		{name: "wrong server account", mutate: func(_ *testing.T, _ *fakeLinodeAPI, lease *core.LeaseTarget, _ core.LeaseClaim) {
			lease.Server.Labels = maps.Clone(lease.Server.Labels)
			lease.Server.Labels[linodeAccountLabel] = "team:another-account"
		}},
		{name: "missing server account", mutate: func(_ *testing.T, _ *fakeLinodeAPI, lease *core.LeaseTarget, _ core.LeaseClaim) {
			lease.Server.Labels = maps.Clone(lease.Server.Labels)
			delete(lease.Server.Labels, linodeAccountLabel)
		}},
		{name: "current account drift", mutate: func(_ *testing.T, api *fakeLinodeAPI, _ *core.LeaseTarget, _ core.LeaseClaim) {
			api.accountID = "team:another-account"
		}},
		{name: "live instance drift", mutate: func(_ *testing.T, api *fakeLinodeAPI, _ *core.LeaseTarget, _ core.LeaseClaim) {
			api.created[0].Label = "different-linode"
		}},
		{name: "live provider key drift", mutate: func(_ *testing.T, api *fakeLinodeAPI, _ *core.LeaseTarget, _ core.LeaseClaim) {
			labels := normalizedLinodeLabels(api.created[0].Tags)
			labels["provider_key"] = "different-provider-key"
			api.created[0].Tags = tagsFromLabels(labels)
		}},
		{name: "cleanup recovery", mutate: func(t *testing.T, _ *fakeLinodeAPI, lease *core.LeaseTarget, claim core.LeaseClaim) {
			labels := maps.Clone(claim.Labels)
			labels["recovery"] = "rollback-cleanup"
			updated, err := core.UpdateLeaseClaimLabelsIfUnchanged(lease.LeaseID, claim, labels)
			if err != nil {
				t.Fatal(err)
			}
			core.SetServerLeaseClaimSnapshot(&lease.Server, updated, true)
		}},
	}
	for _, mode := range []string{"touch", "metadata"} {
		for _, test := range tests {
			t.Run(mode+"/"+test.name, func(t *testing.T) {
				backend, api, lease := setupFencedLinodeLease(t)
				original, _, _ := core.ServerLeaseClaimSnapshot(lease.Server)
				originalLeaseID := lease.LeaseID
				test.mutate(t, api, &lease, original)
				before, existed, err := core.ReadLeaseClaimWithPresence(originalLeaseID)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "touch" {
					_, err = backend.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "running"})
				} else {
					_, err = backend.UpdateTailscaleMetadata(context.Background(), lease, core.TailscaleMetadata{Enabled: true, IPv4: "100.64.1.9"})
				}
				if err == nil || api.updateCalls != 0 || len(api.updated) != 0 {
					t.Fatalf("mutation error=%v attempted provider writes=%d successful provider writes=%v", err, api.updateCalls, api.updated)
				}
				after, exists, readErr := core.ReadLeaseClaimWithPresence(originalLeaseID)
				if readErr != nil || exists != existed || exists && !reflect.DeepEqual(after, before) {
					t.Fatalf("before=%#v after=%#v existed=%t exists=%t err=%v", before, after, existed, exists, readErr)
				}
			})
		}
	}
}

func TestFencedLinodeMetadataRejectsClaimReplacementAfterClientCreation(t *testing.T) {
	for _, mode := range []string{"touch", "metadata"} {
		t.Run(mode, func(t *testing.T) {
			backend, api, lease := setupFencedLinodeLease(t)
			initial, _, _ := core.ServerLeaseClaimSnapshot(lease.Server)
			var replacement core.LeaseClaim
			backend.clientFactory = func(core.Runtime) (linodeAPI, error) {
				labels := maps.Clone(initial.Labels)
				labels["owner"] = "replacement-owner"
				var err error
				replacement, err = core.UpdateLeaseClaimLabelsIfUnchanged(lease.LeaseID, initial, labels)
				return api, err
			}
			var err error
			if mode == "touch" {
				_, err = backend.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "running"})
			} else {
				_, err = backend.UpdateTailscaleMetadata(context.Background(), lease, core.TailscaleMetadata{Enabled: true, IPv4: "100.64.1.9"})
			}
			current, readErr := core.ReadLeaseClaim(lease.LeaseID)
			if err == nil || !strings.Contains(err.Error(), "claim changed") || readErr != nil ||
				!reflect.DeepEqual(current, replacement) || api.updateCalls != 0 {
				t.Fatalf("error=%v current=%#v replacement=%#v attempted provider writes=%d readErr=%v", err, current, replacement, api.updateCalls, readErr)
			}
		})
	}
}

func TestFencedLinodeMetadataHoldsClaimLockThroughProviderMutation(t *testing.T) {
	for _, mode := range []string{"touch", "metadata"} {
		t.Run(mode, func(t *testing.T) {
			backend, api, lease := setupFencedLinodeLease(t)
			initial, _, _ := core.ServerLeaseClaimSnapshot(lease.Server)
			item := api.created[0]
			entered := make(chan struct{})
			proceed := make(chan struct{})
			api.getFn = func(ctx context.Context, _ int64) (linodeInstance, error) {
				close(entered)
				select {
				case <-ctx.Done():
					return linodeInstance{}, ctx.Err()
				case <-proceed:
					return item, nil
				}
			}
			operationDone := make(chan error, 1)
			go func() {
				var err error
				if mode == "touch" {
					_, err = backend.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "running"})
				} else {
					_, err = backend.UpdateTailscaleMetadata(context.Background(), lease, core.TailscaleMetadata{Enabled: true, IPv4: "100.64.1.9"})
				}
				operationDone <- err
			}()
			<-entered
			mutationStarted := make(chan struct{})
			mutationDone := make(chan error, 1)
			go func() {
				close(mutationStarted)
				labels := maps.Clone(initial.Labels)
				labels["owner"] = "concurrent-owner"
				_, err := core.UpdateLeaseClaimLabelsIfUnchanged(lease.LeaseID, initial, labels)
				mutationDone <- err
			}()
			<-mutationStarted
			select {
			case err := <-mutationDone:
				close(proceed)
				<-operationDone
				t.Fatalf("claim changed while Linode provider mutation was in progress: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			close(proceed)
			if err := <-operationDone; err != nil {
				t.Fatal(err)
			}
			if err := <-mutationDone; err == nil || !strings.Contains(err.Error(), "claim changed") {
				t.Fatalf("concurrent claim replacement error=%v", err)
			}
			if api.updateCalls != 1 || len(api.updated) != 1 {
				t.Fatalf("attempted provider writes=%d successful provider writes=%v", api.updateCalls, api.updated)
			}
		})
	}
}

func TestFencedLinodeMetadataAcceptsLegacyExactCloudIdentity(t *testing.T) {
	for _, mode := range []string{"touch", "metadata"} {
		t.Run(mode, func(t *testing.T) {
			backend, api, lease := setupFencedLinodeLease(t)
			claim, _, _ := core.ServerLeaseClaimSnapshot(lease.Server)
			claim.CloudNumericID = 0
			replaceFencedLinodeClaim(t, &lease, claim)
			var updated core.Server
			var err error
			if mode == "touch" {
				updated, err = backend.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "running"})
			} else {
				updated, err = backend.UpdateTailscaleMetadata(context.Background(), lease, core.TailscaleMetadata{Enabled: true, IPv4: "100.64.1.9"})
			}
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := core.ReadLeaseClaim(lease.LeaseID)
			snapshot, exists, set := core.ServerLeaseClaimSnapshot(updated)
			if err != nil || !set || !exists || !reflect.DeepEqual(snapshot, persisted) ||
				persisted.CloudID != lease.Server.CloudID || persisted.CloudNumericID != lease.Server.ID || api.updateCalls != 1 {
				t.Fatalf("persisted=%#v snapshot=%#v exists=%t set=%t attempted provider writes=%d err=%v", persisted, snapshot, exists, set, api.updateCalls, err)
			}
		})
	}
}

func TestFencedLinodeTouchPreservesStoredTimeoutAndRejectsInvalidOverrides(t *testing.T) {
	backend, api, lease := setupFencedLinodeLease(t)
	initial, _, _ := core.ServerLeaseClaimSnapshot(lease.Server)
	backend.Cfg.IdleTimeout = 2 * time.Hour
	server, err := backend.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "running", IdleTimeout: 90 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil || claim.IdleTimeoutSeconds != initial.IdleTimeoutSeconds || server.Labels["idle_timeout_secs"] != "1800" ||
		!containsString(api.updatedTags[0], "customer:production") || server.Labels[linodeAccountLabel] != initial.Labels[linodeAccountLabel] {
		t.Fatalf("claim=%#v server=%#v provider tags=%v err=%v", claim, server, api.updatedTags, err)
	}
	lease.Server = server
	for _, invalid := range []time.Duration{0, -time.Second, time.Millisecond} {
		before, err := core.ReadLeaseClaim(lease.LeaseID)
		if err != nil {
			t.Fatal(err)
		}
		writes := len(api.updated)
		if _, err := backend.Touch(context.Background(), core.TouchRequest{Lease: lease, IdleTimeoutOverride: &invalid}); err == nil {
			t.Fatalf("accepted invalid override %s", invalid)
		}
		after, err := core.ReadLeaseClaim(lease.LeaseID)
		if err != nil || !reflect.DeepEqual(after, before) || len(api.updated) != writes {
			t.Fatalf("override=%s before=%#v after=%#v writes=%d want=%d err=%v", invalid, before, after, len(api.updated), writes, err)
		}
	}
}

func TestFencedLinodeTouchReconcilesLiveTimeoutWithClaim(t *testing.T) {
	backend, api, lease := setupFencedLinodeLease(t)
	claim, _, _ := core.ServerLeaseClaimSnapshot(lease.Server)
	labels := normalizedLinodeLabels(api.created[0].Tags)
	labels["idle_timeout"] = "7200"
	labels["idle_timeout_secs"] = "7200"
	api.created[0].Tags = replaceCrabboxTags(api.created[0].Tags, tagsFromLabels(labels))
	backend.Cfg.IdleTimeout = 3 * time.Hour

	updated, err := backend.Touch(context.Background(), core.TouchRequest{
		Lease:       lease,
		State:       "running",
		IdleTimeout: 90 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := core.ReadLeaseClaim(lease.LeaseID)
	providerLabels := normalizedLinodeLabels(api.created[0].Tags)
	if err != nil || persisted.IdleTimeoutSeconds != claim.IdleTimeoutSeconds ||
		updated.Labels["idle_timeout_secs"] != "1800" || providerLabels["idle_timeout_secs"] != "1800" ||
		persisted.Labels["idle_timeout_secs"] != "1800" || api.updateCalls != 1 {
		t.Fatalf("claim=%#v initial=%#v updated=%v provider=%v attempted provider writes=%d err=%v", persisted, claim, updated.Labels, providerLabels, api.updateCalls, err)
	}
}

func TestFencedLinodeProviderFailureAndCancellationRetainClaim(t *testing.T) {
	for _, mode := range []string{"touch", "metadata", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			backend, api, lease := setupFencedLinodeLease(t)
			before, _, _ := core.ServerLeaseClaimSnapshot(lease.Server)
			keyPath, err := core.TestboxKeyPath(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if mode == "cancellation" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
				api.getFn = func(ctx context.Context, _ int64) (linodeInstance, error) {
					<-ctx.Done()
					return linodeInstance{}, ctx.Err()
				}
			} else {
				api.updateErr = errors.New("linode provider tag update failed")
			}
			if mode == "metadata" {
				_, err = backend.UpdateTailscaleMetadata(ctx, lease, core.TailscaleMetadata{Enabled: true, IPv4: "100.64.1.9"})
			} else {
				override := time.Hour
				_, err = backend.Touch(ctx, core.TouchRequest{Lease: lease, IdleTimeoutOverride: &override})
			}
			after, readErr := core.ReadLeaseClaim(lease.LeaseID)
			wantUpdateCalls := 1
			if mode == "cancellation" {
				wantUpdateCalls = 0
			}
			if err == nil || readErr != nil || !reflect.DeepEqual(after, before) || api.updateCalls != wantUpdateCalls || len(api.updated) != 0 {
				t.Fatalf("error=%v before=%#v after=%#v attempted provider writes=%d successful writes=%v readErr=%v", err, before, after, api.updateCalls, api.updated, readErr)
			}
			if _, err := os.Stat(keyPath); err != nil {
				t.Fatalf("lease SSH key was not retained: %v", err)
			}
			if mode == "cancellation" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancellation error=%v", err)
			}
		})
	}
}

func TestFencedLinodeMetadataAndTouchPreserveReleaseContinuity(t *testing.T) {
	for _, mode := range []string{"touch-touch", "metadata-touch"} {
		t.Run(mode, func(t *testing.T) {
			backend, api, lease := setupFencedLinodeLease(t)
			initial, _, _ := core.ServerLeaseClaimSnapshot(lease.Server)
			if mode == "metadata-touch" {
				meta := core.TailscaleMetadata{Enabled: true, Hostname: "fenced", FQDN: "fenced.example.ts.net", IPv4: "100.64.1.9", Tags: []string{"tag:ci", "tag:crabbox"}, State: "ready", Error: "last probe failed: retrying"}
				updated, err := backend.UpdateTailscaleMetadata(context.Background(), lease, meta)
				if err != nil {
					t.Fatal(err)
				}
				claim, err := core.ReadLeaseClaim(lease.LeaseID)
				snapshot, exists, set := core.ServerLeaseClaimSnapshot(updated)
				if err != nil || !set || !exists || !reflect.DeepEqual(snapshot, claim) || claim.Revision == initial.Revision ||
					claim.LastUsedAt != initial.LastUsedAt || claim.IdleTimeoutSeconds != initial.IdleTimeoutSeconds ||
					claim.Labels["expires_at"] != initial.Labels["expires_at"] || claim.TailscaleIPv4 != meta.IPv4 ||
					updated.Labels["tailscale_tags"] != "tag:ci,tag:crabbox" || !containsString(api.updatedTags[0], "customer:production") {
					t.Fatalf("metadata snapshot=%#v claim=%#v initial=%#v labels=%v err=%v", snapshot, claim, initial, updated.Labels, err)
				}
				lease.Server = updated
			} else {
				updated, err := backend.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "running"})
				if err != nil {
					t.Fatal(err)
				}
				lease.Server = updated
			}
			override := 45 * time.Minute
			updated, err := backend.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "running", IdleTimeoutOverride: &override})
			if err != nil {
				t.Fatal(err)
			}
			lease.Server = updated
			claim, err := core.ReadLeaseClaim(lease.LeaseID)
			if err != nil || claim.IdleTimeoutSeconds != 2700 || claim.Labels["idle_timeout_secs"] != "2700" {
				t.Fatalf("touch claim=%#v err=%v", claim, err)
			}
			if mode == "metadata-touch" && updated.Labels["tailscale_tags"] != "tag:ci,tag:crabbox" {
				t.Fatalf("touch damaged metadata: %v", updated.Labels)
			}
			if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
			if len(api.deleted) != 1 || api.deleted[0] != 100 {
				t.Fatalf("released instances=%v", api.deleted)
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || exists {
				t.Fatalf("claim retained after release: exists=%t err=%v", exists, err)
			}
		})
	}
}

func setupFencedLinodeLease(t *testing.T) (*linodeLeaseBackend, *fakeLinodeAPI, core.LeaseTarget) {
	t.Helper()
	api := &fakeLinodeAPI{}
	backend := newTestBackend(t, api)
	backend.Cfg.IdleTimeout = 30 * time.Minute
	backend.Cfg.TTL = time.Hour
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "fenced"})
	if err != nil {
		t.Fatal(err)
	}
	api.created[0].Tags = append(api.created[0].Tags, "customer:production")
	api.updated = nil
	api.updatedTags = nil
	api.updateCalls = 0
	return backend, api, lease
}

func replaceFencedLinodeClaim(t *testing.T, lease *core.LeaseTarget, replacement core.LeaseClaim) {
	t.Helper()
	current, exists, set := core.ServerLeaseClaimSnapshot(lease.Server)
	if !set || !exists {
		t.Fatal("lease has no exact claim snapshot")
	}
	updated, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(lease.LeaseID, current, replacement)
	if err != nil {
		t.Fatal(err)
	}
	core.SetServerLeaseClaimSnapshot(&lease.Server, updated, true)
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func TestAmbiguousCreatePersistsRecoveryClaimAndRetainsKey(t *testing.T) {
	api := &fakeLinodeAPI{createErr: &linodeAPIError{Status: 500, Body: "server error"}}
	backend := newTestBackend(t, api)

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "ambiguous"})
	var ambiguous *ambiguousLinodeCreateError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("Acquire err=%v, want ambiguousLinodeCreateError", err)
	}
	if len(api.createRequests) != 1 || len(api.deleted) != 0 {
		t.Fatalf("createRequests=%d deleted=%v", len(api.createRequests), api.deleted)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("ambiguous", providerName)
	if claimErr != nil || !ok || claim.CloudID != "" || claim.Labels["recovery"] != "ambiguous-create" || claim.Labels[linodeAccountLabel] != "euuid:A1BC2DEF-34GH-567I-J890KLMN12O34P56" {
		t.Fatalf("recovery claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
	keyPath, pathErr := core.TestboxKeyPath(claim.LeaseID)
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("local key removed during ambiguous create: %v", statErr)
	}

	if _, resolveErr := backend.Resolve(context.Background(), core.ResolveRequest{ID: "ambiguous", ReleaseOnly: true}); resolveErr == nil || !strings.Contains(resolveErr.Error(), "still pending") {
		t.Fatalf("immediate recovery resolve err=%v", resolveErr)
	}
	createdAt, parseErr := strconv.ParseInt(claim.Labels["created_at"], 10, 64)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	backend.RT.Clock = fakeClock{t: time.Unix(createdAt, 0).Add(ambiguousCreateRecoveryGrace + time.Second)}
	backend.recoveryReconcilePolls = 1
	if _, resolveErr := backend.Resolve(context.Background(), core.ResolveRequest{ID: "ambiguous", ReleaseOnly: true}); resolveErr == nil || !strings.Contains(resolveErr.Error(), "remains indeterminate") {
		t.Fatalf("empty recovery resolve err=%v", resolveErr)
	}
}

func TestRollbackCleanupClaimResolvesWithSnapshotAndReleases(t *testing.T) {
	api := &fakeLinodeAPI{deleteErr: errors.New("rollback delete failed")}
	backend := newTestBackend(t, api)
	backend.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
		return errors.New("bootstrap failed")
	}
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "rollback"})
	if err == nil || !strings.Contains(err.Error(), "rollback delete failed") {
		t.Fatalf("Acquire err=%v", err)
	}
	claim, exists, err := core.ResolveLeaseClaimForProvider("rollback", providerName)
	if err != nil || !exists || claim.Labels["recovery"] != "rollback-cleanup" || claim.Revision == "" {
		t.Fatalf("claim=%#v exists=%v err=%v", claim, exists, err)
	}
	keyPath, err := core.TestboxKeyPath(claim.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	api.deleteErr = nil
	resolved, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, snapshotExists, set := core.ServerLeaseClaimSnapshot(resolved.Server)
	if !set || !snapshotExists || snapshot.Revision != claim.Revision {
		t.Fatalf("snapshot=%#v exists=%v set=%v claim=%#v", snapshot, snapshotExists, set, claim)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: resolved}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 2 || api.deleted[0] != 100 || api.deleted[1] != 100 {
		t.Fatalf("deleted=%v", api.deleted)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v", exists, err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stored key retained after rollback cleanup: %v", err)
	}
}
