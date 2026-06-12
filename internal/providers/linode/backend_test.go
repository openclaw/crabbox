package linode

import (
	"context"
	"errors"
	"io"
	"os"
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
	linodes        []linodeInstance
	accountID      string
	accountErr     error
	nextID         int64
	createErr      error
	getErr         error
	deleteErr      error
	created        []linodeInstance
	deleted        []int64
	updated        []int64
	updatedTags    [][]string
	createRequests []createLinodeRequest
}

func (f *fakeLinodeAPI) AccountID(context.Context) (string, error) {
	if f.accountErr != nil {
		return "", f.accountErr
	}
	if f.accountID != "" {
		return f.accountID, nil
	}
	return "email:alice@example.com", nil
}

func (f *fakeLinodeAPI) ListLinodes(context.Context) ([]linodeInstance, error) {
	return append(append([]linodeInstance(nil), f.linodes...), f.created...), nil
}

func (f *fakeLinodeAPI) GetLinode(_ context.Context, id int64) (linodeInstance, error) {
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

func (f *fakeLinodeAPI) DeleteLinode(_ context.Context, id int64) error {
	f.deleted = append(f.deleted, id)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.linodes = removeLinodeByID(f.linodes, id)
	f.created = removeLinodeByID(f.created, id)
	return nil
}

func (f *fakeLinodeAPI) UpdateLinodeTags(_ context.Context, id int64, tags []string) error {
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
	if req.Metadata == nil || req.Metadata.UserData == "" {
		t.Fatalf("metadata=%#v", req.Metadata)
	}
	if labels := labelsFromTags(req.Tags); labels["lease"] != lease.LeaseID || labels["slug"] != "my-app" || labels["state"] != "provisioning" {
		t.Fatalf("create labels=%v", labels)
	}
	if lease.Server.Labels["state"] != "ready" || lease.Server.Labels[linodeAccountLabel] != "email:alice@example.com" {
		t.Fatalf("lease labels=%v", lease.Server.Labels)
	}
	if len(api.updated) != 1 || api.updated[0] != 100 {
		t.Fatalf("updated=%v", api.updated)
	}
	if labels := labelsFromTags(api.updatedTags[0]); labels["state"] != "ready" || labels["lease"] != lease.LeaseID {
		t.Fatalf("ready tags labels=%v", labels)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider("my-app", providerName)
	if err != nil || !ok || claim.CloudID != "100" || claim.Labels[linodeAccountLabel] != "email:alice@example.com" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestListFiltersForeignAndPartialLinodes(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	owned := linodeInstance{ID: 1, Label: "crabbox-cbx_one-owned", Status: "running", Type: "g6-standard-1", IPv4: []string{"203.0.113.1"}, Tags: leaseTags(cfg, "cbx_one", "owned", "ready", false, time.Unix(1, 0))}
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

func TestReleaseRefusesAccountMismatch(t *testing.T) {
	api := &fakeLinodeAPI{accountID: "email:first@example.com"}
	backend := newTestBackend(t, api)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "mismatch"})
	if err != nil {
		t.Fatal(err)
	}

	api.accountID = "email:second@example.com"
	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
	if err == nil || !strings.Contains(err.Error(), "account mismatch") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("deleted=%v", api.deleted)
	}
}

func TestCleanupDryRunSkipsKeepAndDeletesExpiredWhenLive(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	expiredLabels := core.DirectLeaseLabels(cfg, "cbx_expired", "expired", providerName, "", false, now.Add(-2*time.Hour))
	expiredLabels["state"] = "ready"
	expiredLabels[linodeAccountLabel] = "email:alice@example.com"
	expiredLabels["expires_at"] = "1"
	keepLabels := core.DirectLeaseLabels(cfg, "cbx_keep", "keep", providerName, "", true, now.Add(-2*time.Hour))
	keepLabels["state"] = "ready"
	keepLabels[linodeAccountLabel] = "email:alice@example.com"
	api := &fakeLinodeAPI{linodes: []linodeInstance{
		{ID: 10, Label: core.LeaseProviderName("cbx_expired", "expired"), Status: "running", Type: "g6-standard-1", IPv4: []string{"203.0.113.10"}, Tags: tagsFromLabels(expiredLabels)},
		{ID: 11, Label: core.LeaseProviderName("cbx_keep", "keep"), Status: "running", Type: "g6-standard-1", IPv4: []string{"203.0.113.11"}, Tags: tagsFromLabels(keepLabels)},
	}}
	backend := newTestBackend(t, api)
	backend.RT.Clock = fakeClock{t: now}

	if err := backend.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("dry-run deleted=%v", api.deleted)
	}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 10 {
		t.Fatalf("deleted=%v", api.deleted)
	}
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
	if claimErr != nil || !ok || claim.CloudID != "" || claim.Labels["recovery"] != "ambiguous-create" || claim.Labels[linodeAccountLabel] != "email:alice@example.com" {
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
