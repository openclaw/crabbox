package lambda

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeLambdaAPI struct {
	instances        []Instance
	sshKeys          []SSHKey
	listKeyErr       error
	addKeyErr        error
	launchErr        error
	terminateErr     error
	deleteKeyErr     error
	launchRequests   []LaunchInstanceRequest
	addKeyRequests   []AddSSHKeyRequest
	terminatedIDs    [][]string
	deletedKeyIDs    []string
	nextKeyID        int
	nextInstanceID   int
	listInstancesHit int
}

func (f *fakeLambdaAPI) ListInstances(context.Context) ([]Instance, error) {
	f.listInstancesHit++
	return append([]Instance(nil), f.instances...), nil
}

func (f *fakeLambdaAPI) GetInstance(_ context.Context, id string) (Instance, error) {
	for _, item := range f.instances {
		if item.ID == id {
			return item, nil
		}
	}
	return Instance{}, &APIError{Status: 404}
}

func (f *fakeLambdaAPI) LaunchInstance(_ context.Context, req LaunchInstanceRequest) (LaunchInstanceResponse, error) {
	f.launchRequests = append(f.launchRequests, req)
	if f.launchErr != nil {
		return LaunchInstanceResponse{}, f.launchErr
	}
	if f.nextInstanceID == 0 {
		f.nextInstanceID = 100
	}
	id := "i-" + string(rune('0'+f.nextInstanceID%10))
	if f.nextInstanceID >= 100 {
		id = "i-100"
	}
	item := Instance{
		ID:          id,
		Name:        id,
		Status:      "active",
		Region:      Region{Name: req.RegionName},
		Type:        req.InstanceTypeName,
		IP:          "203.0.113.25",
		SSHKeyNames: append([]string(nil), req.SSHKeyNames...),
	}
	f.instances = append(f.instances, item)
	f.nextInstanceID++
	return LaunchInstanceResponse{InstanceIDs: []string{id}}, nil
}

func (f *fakeLambdaAPI) TerminateInstances(_ context.Context, ids []string) error {
	f.terminatedIDs = append(f.terminatedIDs, append([]string(nil), ids...))
	if f.terminateErr != nil {
		return f.terminateErr
	}
	return nil
}

func (f *fakeLambdaAPI) ListSSHKeys(context.Context) ([]SSHKey, error) {
	if f.listKeyErr != nil {
		return nil, f.listKeyErr
	}
	return append([]SSHKey(nil), f.sshKeys...), nil
}

func (f *fakeLambdaAPI) AddSSHKey(_ context.Context, req AddSSHKeyRequest) (SSHKey, error) {
	f.addKeyRequests = append(f.addKeyRequests, req)
	if f.addKeyErr != nil {
		return SSHKey{Name: req.Name}, f.addKeyErr
	}
	if f.nextKeyID == 0 {
		f.nextKeyID = 700
	}
	key := SSHKey{ID: "key-700", Name: req.Name, PublicKey: req.PublicKey}
	f.sshKeys = append(f.sshKeys, key)
	f.nextKeyID++
	return key, nil
}

func (f *fakeLambdaAPI) DeleteSSHKey(_ context.Context, id string) error {
	f.deletedKeyIDs = append(f.deletedKeyIDs, id)
	return f.deleteKeyErr
}

func (f *fakeLambdaAPI) ListRegions(context.Context) ([]Region, error) {
	return []Region{{Name: defaultRegion}}, nil
}

func (f *fakeLambdaAPI) ListInstanceTypes(context.Context) ([]InstanceType, error) {
	return []InstanceType{{Name: defaultType, RegionsWithCapacityAvailable: []string{defaultRegion}}}, nil
}

func (f *fakeLambdaAPI) ListImages(context.Context) ([]Image, error) {
	return []Image{{Family: defaultImageFamily, Region: defaultRegion}}, nil
}

func (f *fakeLambdaAPI) ListFilesystems(context.Context) ([]Filesystem, error) { return nil, nil }

func (f *fakeLambdaAPI) ListFirewallRulesets(context.Context) ([]FirewallRuleset, error) {
	return nil, nil
}

func newTestBackend(t *testing.T, api *fakeLambdaAPI) *backend {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.SSHUser = defaultUser
	cfg.SSHPort = defaultPort
	cfg.WorkRoot = "/work/crabbox"
	cfg.ServerType = defaultType
	cfg.Lambda.Region = defaultRegion
	cfg.Lambda.Type = defaultType
	cfg.Lambda.ImageFamily = defaultImageFamily
	b := &backend{spec: Provider{}.Spec(), cfg: cfg, rt: core.Runtime{Stderr: io.Discard}}
	b.clientFactory = func(core.Runtime) (lambdaAPI, error) { return api, nil }
	b.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error { return nil }
	return b
}

func TestListShowsOnlyCompleteOwnedLambdaInstances(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = defaultType
	owned := Instance{ID: "i-owned", Name: "owned", Status: "active", IP: "203.0.113.10", Type: defaultType, Tags: leaseTags(cfg, "cbx_abcdef123456", "owned", "ready", false, time.Now())}
	api := &fakeLambdaAPI{instances: []Instance{
		owned,
		{ID: "i-foreign", Name: "foreign", Tags: map[string]string{"crabbox": "true", "provider": "other"}},
		{ID: "i-partial", Name: "partial", Tags: map[string]string{"crabbox": "true", "provider": providerName}},
	}}
	views, err := newTestBackend(t, api).List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].CloudID != "i-owned" || views[0].Status != "ready" {
		t.Fatalf("views=%#v", views)
	}
}

func TestAcquireCreatesKeyLaunchesPollsAndClaimsLease(t *testing.T) {
	api := &fakeLambdaAPI{}
	b := newTestBackend(t, api)
	b.cfg.Lambda.FirewallRuleset = "default"
	b.cfg.Lambda.FilesystemNames = []string{"cache"}
	b.cfg.Lambda.FilesystemMounts = []core.LambdaFilesystemMount{{Name: "cache", MountPath: "/mnt/cache"}}
	b.cfg.Tailscale.Enabled = true
	b.cfg.Tailscale.AuthKey = "tskey-auth-test"
	b.cfg.Tailscale.HostnameTemplate = "{{slug}}-{{lease}}"
	b.cfg.Tailscale.Tags = []string{"tag:ci", "tag:crabbox"}

	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "my-app", Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID == "" || lease.Server.CloudID != "i-100" || lease.SSH.Host != "203.0.113.25" || lease.SSH.User != defaultUser || lease.SSH.Port != defaultPort {
		t.Fatalf("lease=%#v", lease)
	}
	if len(api.addKeyRequests) != 1 || !strings.HasPrefix(api.addKeyRequests[0].Name, "crabbox-cbx-") {
		t.Fatalf("addKeyRequests=%#v", api.addKeyRequests)
	}
	if len(api.launchRequests) != 1 {
		t.Fatalf("launchRequests=%#v", api.launchRequests)
	}
	req := api.launchRequests[0]
	if req.RegionName != defaultRegion || req.InstanceTypeName != defaultType || req.Quantity != 1 || len(req.SSHKeyNames) != 1 || req.SSHKeyNames[0] != api.addKeyRequests[0].Name {
		t.Fatalf("launch request=%#v", req)
	}
	if req.ImageFamily != defaultImageFamily || req.ImageID != "" || req.UserData == "" || strings.Contains(req.UserData, "base64") {
		t.Fatalf("launch image/user_data shape=%#v", req)
	}
	if req.FirewallRulesetName != "default" || len(req.FileSystemNames) != 1 || len(req.FileSystemMounts) != 1 {
		t.Fatalf("launch optional resources=%#v", req)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider("my-app", providerName)
	if err != nil || !ok || claim.CloudID != "i-100" || claim.Labels[lambdaKeyOwnedLabel] != "true" || claim.Labels[lambdaKeyIDLabel] != "key-700" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
	if claim.Labels["expires_at"] == "" || claim.Labels["last_touched_at"] == "" || claim.Labels["state"] != "ready" {
		t.Fatalf("local claim should carry ready cleanup timing: %v", claim.Labels)
	}
}

func TestAcquirePreservesExplicitSSHUserAndPort(t *testing.T) {
	api := &fakeLambdaAPI{}
	b := newTestBackend(t, api)
	b.cfg.SSHUser = "alice"
	b.cfg.SSHPort = "2222"
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "custom-ssh"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.User != "alice" || lease.SSH.Port != "2222" {
		t.Fatalf("ssh target=%#v", lease.SSH)
	}
	if len(api.launchRequests) != 1 || !strings.Contains(api.launchRequests[0].UserData, `Port 2222`) || !strings.Contains(api.launchRequests[0].UserData, `name: "alice"`) {
		t.Fatalf("user_data did not preserve explicit SSH settings: %q", api.launchRequests[0].UserData)
	}
}

func TestAcquireRollsBackWhenOnAcquiredFails(t *testing.T) {
	api := &fakeLambdaAPI{}
	b := newTestBackend(t, api)
	var observed core.LeaseTarget
	_, err := b.Acquire(context.Background(), core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "callback-fail",
		OnAcquired: func(acquired core.LeaseTarget) error {
			observed = acquired
			return errors.New("controller unavailable")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "controller unavailable") {
		t.Fatalf("err=%v", err)
	}
	if observed.Server.CloudID != "i-100" || observed.SSH.Host != "203.0.113.25" || observed.LeaseID == "" {
		t.Fatalf("observed=%#v", observed)
	}
	if len(api.terminatedIDs) != 1 || api.terminatedIDs[0][0] != "i-100" {
		t.Fatalf("terminated=%v", api.terminatedIDs)
	}
	if len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != "key-700" {
		t.Fatalf("deletedKeyIDs=%v", api.deletedKeyIDs)
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("callback-fail", providerName); claimErr != nil || ok {
		t.Fatalf("claim ok=%v err=%v", ok, claimErr)
	}
}

func TestAcquireReusesMatchingSSHKeyAndReleaseDoesNotDeleteIt(t *testing.T) {
	api := &fakeLambdaAPI{sshKeys: []SSHKey{{ID: "key-existing", Name: "placeholder", PublicKey: "will be replaced"}}}
	b := newTestBackend(t, api)
	var capturedPublicKey string
	b.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error { return nil }
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(b.cfg, "cbx_abcdef123456")
	if err != nil {
		t.Fatal(err)
	}
	_ = keyPath
	capturedPublicKey = publicKey
	api.sshKeys = []SSHKey{{ID: "key-existing", Name: providerKeyForLease("cbx_abcdef123456"), PublicKey: capturedPublicKey}}

	leaseID := "cbx_abcdef123456"
	slug := "reuse"
	labels := leaseTags(b.cfg, leaseID, slug, "ready", false, time.Now())
	labels[lambdaKeyIDLabel] = "key-existing"
	labels[lambdaKeyNameLabel] = providerKeyForLease(leaseID)
	labels[lambdaKeyOwnedLabel] = "false"
	server := core.Server{Provider: providerName, CloudID: "i-existing", Name: "existing", Labels: labels}
	api.instances = []Instance{{ID: "i-existing", Name: "existing", Status: "active", IP: "203.0.113.40", Type: defaultType, Tags: labels}}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, b.cfg, server, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	if len(api.terminatedIDs) != 1 || api.terminatedIDs[0][0] != "i-existing" {
		t.Fatalf("terminated=%v", api.terminatedIDs)
	}
	if len(api.deletedKeyIDs) != 0 {
		t.Fatalf("deletedKeyIDs=%v", api.deletedKeyIDs)
	}
}

func TestAcquireRejectsMismatchedDuplicateSSHKey(t *testing.T) {
	api := &fakeLambdaAPI{sshKeys: []SSHKey{{ID: "key-existing", Name: providerKeyForLease("cbx_any"), PublicKey: "ssh-ed25519 other"}}}
	b := newTestBackend(t, api)
	leaseID := "cbx_any"
	keyPath, _, err := core.EnsureTestboxKeyForConfig(b.cfg, leaseID)
	if err != nil {
		t.Fatal(err)
	}
	_ = keyPath
	api.sshKeys[0].Name = providerKeyForLease(leaseID)
	_, err = b.ensureSSHKey(context.Background(), api, providerKeyForLease(leaseID), "ssh-ed25519 expected")
	if err == nil || !strings.Contains(err.Error(), "different public key") {
		t.Fatalf("err=%v", err)
	}
}

func TestAcquireDoesNotPersistRecoveryClaimForPreCreateKeyError(t *testing.T) {
	api := &fakeLambdaAPI{listKeyErr: errors.New("list keys denied")}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "key-list-fail"})
	if err == nil || !strings.Contains(err.Error(), "list keys denied") {
		t.Fatalf("err=%v", err)
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("key-list-fail", providerName); claimErr != nil || ok {
		t.Fatalf("claim ok=%v err=%v", ok, claimErr)
	}
}

func TestAcquireDoesNotPersistRecoveryClaimForDefiniteKeyCreateAPIError(t *testing.T) {
	api := &fakeLambdaAPI{addKeyErr: &APIError{Status: 400, Code: "global/invalid-parameters", Message: "bad key"}}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "key-api-fail"})
	if err == nil || !strings.Contains(err.Error(), "invalid-parameters") {
		t.Fatalf("err=%v", err)
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("key-api-fail", providerName); claimErr != nil || ok {
		t.Fatalf("claim ok=%v err=%v", ok, claimErr)
	}
}

func TestAcquireRollsBackKeyForDefiniteLaunchAPIError(t *testing.T) {
	api := &fakeLambdaAPI{launchErr: &APIError{Status: 400, Code: "global/invalid-parameters", Message: "bad launch"}}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "launch-api-fail"})
	if err == nil || !strings.Contains(err.Error(), "invalid-parameters") {
		t.Fatalf("err=%v", err)
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("launch-api-fail", providerName); claimErr != nil || ok {
		t.Fatalf("claim ok=%v err=%v", ok, claimErr)
	}
	if len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != "key-700" {
		t.Fatalf("deletedKeyIDs=%v", api.deletedKeyIDs)
	}
}

func TestAcquirePreservesRecoveryForAmbiguousKeyCreateAPIError(t *testing.T) {
	api := &fakeLambdaAPI{addKeyErr: &APIError{Status: 500, Code: "provider/upstream", Message: "server error"}}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "key-api-ambiguous"})
	if err == nil || !strings.Contains(err.Error(), "indeterminate") {
		t.Fatalf("err=%v", err)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("key-api-ambiguous", providerName)
	if claimErr != nil || !ok || claim.CloudID != "" || claim.Labels[lambdaRecoveryKeyLabel] != "ambiguous-key-create" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
}

func TestAcquirePreservesRecoveryForAmbiguousLaunchAPIError(t *testing.T) {
	api := &fakeLambdaAPI{launchErr: &APIError{Status: 500, Code: "provider/upstream", Message: "server error"}}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "launch-api-ambiguous"})
	if err == nil || !strings.Contains(err.Error(), "indeterminate") {
		t.Fatalf("err=%v", err)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("launch-api-ambiguous", providerName)
	if claimErr != nil || !ok || claim.CloudID != "" || claim.Labels[lambdaRecoveryKeyLabel] != "ambiguous-create" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
}

func TestReleaseTerminatesThenDeletesOwnedKeyAndLocalArtifacts(t *testing.T) {
	api := &fakeLambdaAPI{}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "release"})
	if err != nil {
		t.Fatal(err)
	}
	keyPath := lease.SSH.Key
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.terminatedIDs) != 1 || api.terminatedIDs[0][0] != "i-100" {
		t.Fatalf("terminated=%v", api.terminatedIDs)
	}
	if len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != "key-700" {
		t.Fatalf("deletedKeyIDs=%v", api.deletedKeyIDs)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("release", providerName); err != nil || ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stored key still exists: %v", err)
	}
}

func TestReleaseMissingInstanceStillDeletesOwnedKeyAndClaim(t *testing.T) {
	api := &fakeLambdaAPI{}
	b := newTestBackend(t, api)
	leaseID := "cbx_abcdef123457"
	slug := "missing-instance"
	labels := leaseTags(b.cfg, leaseID, slug, "ready", false, time.Now())
	labels[lambdaKeyIDLabel] = "key-missing"
	labels[lambdaKeyNameLabel] = providerKeyForLease(leaseID)
	labels[lambdaKeyOwnedLabel] = "true"
	server := core.Server{Provider: providerName, CloudID: "i-missing", Name: "missing", Labels: labels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, b.cfg, server, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	target, err := b.releaseTargetFromClaim(slug)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: target}); err != nil {
		t.Fatal(err)
	}
	if len(api.terminatedIDs) != 0 {
		t.Fatalf("terminated missing instance=%v", api.terminatedIDs)
	}
	if len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != "key-missing" {
		t.Fatalf("deletedKeyIDs=%v", api.deletedKeyIDs)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName); err != nil || ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
}

func TestListResolveAndReleaseUseLocalClaimForUntaggedLambdaInstance(t *testing.T) {
	api := &fakeLambdaAPI{}
	b := newTestBackend(t, api)
	leaseID := "cbx_abcdef123459"
	slug := "untagged"
	labels := lambdaLabelsWithKey(leaseTags(b.cfg, leaseID, slug, "ready", false, time.Now()), lambdaSSHKeyIdentity{ID: "key-untagged", Name: providerKeyForLease(leaseID), Created: true})
	api.instances = []Instance{{ID: "i-untagged", Name: "untagged", Status: "active", IP: "203.0.113.61", Type: defaultType}}
	server := core.Server{Provider: providerName, CloudID: "i-untagged", Name: slug, Labels: labels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, b.cfg, server, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	views, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].CloudID != "i-untagged" || views[0].Labels["lease"] != leaseID {
		t.Fatalf("views=%#v", views)
	}
	target, err := b.Resolve(context.Background(), core.ResolveRequest{ID: slug, Repo: core.Repo{Root: t.TempDir()}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	if target.Server.CloudID != "i-untagged" || target.SSH.Host != "203.0.113.61" || target.Server.Labels["slug"] != slug {
		t.Fatalf("target=%#v", target)
	}
	releaseTarget, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "i-untagged", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: releaseTarget}); err != nil {
		t.Fatal(err)
	}
	if len(api.terminatedIDs) != 1 || api.terminatedIDs[0][0] != "i-untagged" || len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != "key-untagged" {
		t.Fatalf("terminated=%v deletedKeyIDs=%v", api.terminatedIDs, api.deletedKeyIDs)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName); err != nil || ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
}

func TestReleaseFromClaimOnlyCloudIDTerminatesSafely(t *testing.T) {
	api := &fakeLambdaAPI{}
	b := newTestBackend(t, api)
	leaseID := "cbx_abcdef123456"
	slug := "claim-only"
	labels := leaseTags(b.cfg, leaseID, slug, "ready", false, time.Now())
	labels[lambdaKeyIDLabel] = "key-claim"
	labels[lambdaKeyNameLabel] = providerKeyForLease(leaseID)
	labels[lambdaKeyOwnedLabel] = "true"
	api.instances = []Instance{{ID: "i-claim", Name: "claim", Status: "active", IP: "203.0.113.60", Type: defaultType, Tags: labels}}
	server := core.Server{Provider: providerName, CloudID: "i-claim", Name: "claim", Labels: labels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, b.cfg, server, core.SSHTarget{}, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	target, err := b.releaseTargetFromClaim(slug)
	if err != nil {
		t.Fatal(err)
	}
	target.Server.CloudID = ""
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: target}); err != nil {
		t.Fatal(err)
	}
	if len(api.terminatedIDs) != 1 || api.terminatedIDs[0][0] != "i-claim" || len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != "key-claim" {
		t.Fatalf("terminated=%v deletedKeyIDs=%v", api.terminatedIDs, api.deletedKeyIDs)
	}
}

func TestCleanupDryRunDoesNotMutate(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = defaultType
	old := time.Now().Add(-48 * time.Hour)
	labels := leaseTags(cfg, "cbx_abcdef123456", "old", "ready", false, old)
	labels["last_touched_at"] = core.LeaseLabelTime(old)
	labels["expires_at"] = core.LeaseLabelTime(old.Add(time.Minute))
	api := &fakeLambdaAPI{instances: []Instance{{ID: "i-old", Name: "old", Status: "active", IP: "203.0.113.50", Type: defaultType, Tags: labels}}}
	if err := newTestBackend(t, api).Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(api.terminatedIDs) != 0 || len(api.deletedKeyIDs) != 0 {
		t.Fatalf("mutated during dry run: terminated=%v keys=%v", api.terminatedIDs, api.deletedKeyIDs)
	}
}

func TestCleanupDeletesProviderOnlyExpiredLambdaInstance(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = defaultType
	old := time.Now().Add(-48 * time.Hour)
	labels := lambdaProviderLaunchTags(leaseTags(cfg, "cbx_abcdef123456", "old", "provisioning", false, old), lambdaSSHKeyIdentity{ID: "key-old", Name: "crabbox-cbx-old", Created: true})
	api := &fakeLambdaAPI{instances: []Instance{{ID: "i-old", Name: "old", Status: "active", IP: "203.0.113.50", Type: defaultType, Tags: labels}}}
	if err := newTestBackend(t, api).Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(api.terminatedIDs) != 1 || len(api.terminatedIDs[0]) != 1 || api.terminatedIDs[0][0] != "i-old" {
		t.Fatalf("terminated=%v", api.terminatedIDs)
	}
}

func TestCleanupUsesFreshLocalClaimLabelsWhenTouchIsLocalOnly(t *testing.T) {
	api := &fakeLambdaAPI{}
	b := newTestBackend(t, api)
	leaseID := "cbx_abcdef123456"
	slug := "fresh-claim"
	old := time.Now().Add(-48 * time.Hour)
	staleLabels := leaseTags(b.cfg, leaseID, slug, "ready", false, old)
	staleLabels["last_touched_at"] = core.LeaseLabelTime(old)
	staleLabels["expires_at"] = core.LeaseLabelTime(old.Add(time.Minute))
	api.instances = []Instance{{ID: "i-fresh", Name: "fresh", Status: "active", IP: "203.0.113.70", Type: defaultType, Tags: staleLabels}}
	freshLabels := leaseTags(b.cfg, leaseID, slug, "ready", false, time.Now())
	server := core.Server{Provider: providerName, CloudID: "i-fresh", Name: "fresh", Labels: freshLabels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, b.cfg, server, core.SSHTarget{}, t.TempDir(), b.cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(api.terminatedIDs) != 0 {
		t.Fatalf("cleanup used stale provider tags: terminated=%v", api.terminatedIDs)
	}
}

func TestResolvePreservesFreshLocalClaimLabels(t *testing.T) {
	api := &fakeLambdaAPI{}
	b := newTestBackend(t, api)
	leaseID := "cbx_abcdef123458"
	slug := "resolve-fresh"
	key := lambdaSSHKeyIdentity{ID: "key-resolve", Name: providerKeyForLease(leaseID), Created: true}
	providerTags := lambdaProviderLaunchTags(leaseTags(b.cfg, leaseID, slug, "provisioning", false, time.Now()), key)
	api.instances = []Instance{{ID: "i-resolve", Name: "resolve", Status: "active", IP: "203.0.113.80", Type: defaultType, Tags: providerTags}}
	claimLabels := lambdaLabelsWithKey(leaseTags(b.cfg, leaseID, slug, "ready", false, time.Now()), key)
	server := core.Server{Provider: providerName, CloudID: "i-resolve", Name: "resolve", Labels: claimLabels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, b.cfg, server, core.SSHTarget{}, t.TempDir(), b.cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	target, err := b.Resolve(context.Background(), core.ResolveRequest{ID: slug, Repo: core.Repo{Root: t.TempDir()}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	if target.Server.Labels["state"] != "ready" || target.Server.Labels["expires_at"] == "" {
		t.Fatalf("resolved labels=%v", target.Server.Labels)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(slug, providerName)
	if err != nil || !ok || claim.Labels["state"] != "ready" || claim.Labels["expires_at"] == "" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestAmbiguousLaunchPreservesRecoveryClaimAndStoredKey(t *testing.T) {
	api := &fakeLambdaAPI{launchErr: errors.New("transport closed")}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "ambiguous"})
	if err == nil || !strings.Contains(err.Error(), "indeterminate") {
		t.Fatalf("err=%v", err)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("ambiguous", providerName)
	if claimErr != nil || !ok || claim.CloudID != "" || claim.Labels[lambdaRecoveryKeyLabel] != "ambiguous-create" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
	keyPath, pathErr := core.TestboxKeyPath(claim.LeaseID)
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("stored recovery key missing: %v", statErr)
	}
}

func TestAmbiguousLaunchRecoveryFindsInstanceBySSHKeyName(t *testing.T) {
	api := &fakeLambdaAPI{launchErr: errors.New("transport closed")}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "ambiguous-keyed"})
	if err == nil || !strings.Contains(err.Error(), "indeterminate") {
		t.Fatalf("err=%v", err)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("ambiguous-keyed", providerName)
	if claimErr != nil || !ok || claim.CloudID != "" || claim.Labels[lambdaRecoveryKeyLabel] != "ambiguous-create" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
	api.launchErr = nil
	api.instances = append(api.instances, Instance{
		ID:          "i-late",
		Name:        "late",
		Status:      "active",
		IP:          "203.0.113.90",
		Type:        defaultType,
		SSHKeyNames: []string{claim.Labels[lambdaKeyNameLabel]},
	})
	target, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "ambiguous-keyed", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if target.Server.CloudID != "i-late" || target.Server.Labels[lambdaRecoveryKeyLabel] != "ambiguous-create" {
		t.Fatalf("target=%#v", target)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: target}); err != nil {
		t.Fatal(err)
	}
	if len(api.terminatedIDs) != 1 || api.terminatedIDs[0][0] != "i-late" || len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != "key-700" {
		t.Fatalf("terminated=%v deletedKeyIDs=%v", api.terminatedIDs, api.deletedKeyIDs)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("ambiguous-keyed", providerName); err != nil || ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
}

func TestFailedRollbackRecoveryClaimKeepsInstanceIDForRetry(t *testing.T) {
	api := &fakeLambdaAPI{terminateErr: errors.New("terminate unavailable")}
	b := newTestBackend(t, api)
	b.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
		return errors.New("ssh never became ready")
	}
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "retry-cleanup"})
	if err == nil || !strings.Contains(err.Error(), "terminate unavailable") {
		t.Fatalf("err=%v", err)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("retry-cleanup", providerName)
	if claimErr != nil || !ok || claim.CloudID != "i-100" || claim.Labels[lambdaRecoveryKeyLabel] != "rollback-cleanup" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
	api.terminateErr = nil
	target, err := b.releaseTargetFromClaim("retry-cleanup")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: target}); err != nil {
		t.Fatal(err)
	}
	if len(api.terminatedIDs) < 2 || api.terminatedIDs[len(api.terminatedIDs)-1][0] != "i-100" {
		t.Fatalf("terminated=%v", api.terminatedIDs)
	}
}

func TestAmbiguousKeyCreateRecoveryDeletesOwnedKeyByName(t *testing.T) {
	api := &fakeLambdaAPI{addKeyErr: &APIError{Status: 500, Code: "provider/upstream", Message: "server error"}}
	b := newTestBackend(t, api)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "key-api-ambiguous"})
	if err == nil || !strings.Contains(err.Error(), "indeterminate") {
		t.Fatalf("err=%v", err)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("key-api-ambiguous", providerName)
	if claimErr != nil || !ok || claim.CloudID != "" || claim.Labels[lambdaRecoveryKeyLabel] != "ambiguous-key-create" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
	api.addKeyErr = nil
	api.sshKeys = append(api.sshKeys, SSHKey{ID: "key-late", Name: claim.Labels[lambdaKeyNameLabel]})
	target, err := b.releaseTargetFromClaim("key-api-ambiguous")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: target}); err != nil {
		t.Fatal(err)
	}
	if len(api.terminatedIDs) != 0 {
		t.Fatalf("terminated=%v", api.terminatedIDs)
	}
	if len(api.deletedKeyIDs) != 1 || api.deletedKeyIDs[0] != "key-late" {
		t.Fatalf("deletedKeyIDs=%v", api.deletedKeyIDs)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("key-api-ambiguous", providerName); err != nil || ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
}

func TestTouchAndTailscaleMetadataAreLocalOnly(t *testing.T) {
	api := &fakeLambdaAPI{}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "touch"})
	if err != nil {
		t.Fatal(err)
	}
	touched, err := b.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "running", IdleTimeout: 20 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if touched.Labels["state"] != "running" || touched.Labels[lambdaTouchLocalLabel] != "true" {
		t.Fatalf("touched labels=%v", touched.Labels)
	}
	updated, err := b.UpdateTailscaleMetadata(context.Background(), core.LeaseTarget{LeaseID: lease.LeaseID, Server: touched}, core.TailscaleMetadata{
		Enabled:  true,
		Hostname: "touch",
		FQDN:     "touch.example.ts.net",
		IPv4:     "100.64.1.10",
		Tags:     []string{"tag:ci", "tag:crabbox"},
		State:    "ready",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Labels["tailscale_ipv4"] != "100.64.1.10" || updated.Labels["tailscale_tags"] != "tag:ci,tag:crabbox" || updated.Labels[lambdaTouchLocalLabel] != "true" {
		t.Fatalf("updated labels=%v", updated.Labels)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider("touch", providerName)
	if err != nil || !ok || claim.Labels["tailscale_fqdn"] != "touch.example.ts.net" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestReleaseRemovesStoredKeyDirectory(t *testing.T) {
	api := &fakeLambdaAPI{}
	b := newTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "keydir"})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(lease.SSH.Key)
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("key dir still exists: %v", err)
	}
}
