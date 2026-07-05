package aws

import (
	"context"
	"errors"
	"io"
	"maps"
	"strings"
	"testing"
	"time"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeAWSClient struct {
	servers          []Server
	created          Server
	createCalls      int
	createSlugs      []string
	createCfg        Config
	createErr        error
	waitErr          error
	get              map[string]Server
	getErrs          map[string]error
	getErr           error
	getIDs           []string
	deletedInstances []string
	deletedKeys      []string
	deleteKeyErr     error
	validatedKeys    []string
	validateKeyErr   error
	tagged           []string
}

func (c *fakeAWSClient) ListCrabboxServers(context.Context) ([]Server, error) {
	return c.servers, nil
}

func (c *fakeAWSClient) CreateServerWithFallback(_ context.Context, _ Config, _, _, slug string, _ bool, _ func(string, ...any)) (Server, Config, error) {
	c.createCalls++
	c.createSlugs = append(c.createSlugs, slug)
	if c.createErr != nil {
		return Server{}, Config{}, c.createErr
	}
	if c.created.CloudID == "" {
		c.created = awsTestServer("i-created", "cbx_created", "created", "us-east-1")
	}
	if c.createCfg.AWSRegion == "" {
		c.createCfg = Config{Provider: "aws", AWSRegion: "us-east-1"}
	}
	return c.created, c.createCfg, nil
}

func (c *fakeAWSClient) WaitForServerIP(context.Context, string) (Server, error) {
	if c.waitErr != nil {
		return Server{}, c.waitErr
	}
	return c.created, nil
}

func (c *fakeAWSClient) GetServer(_ context.Context, id string) (Server, error) {
	c.getIDs = append(c.getIDs, id)
	if err := c.getErrs[id]; err != nil {
		return Server{}, err
	}
	if c.getErr != nil {
		return Server{}, c.getErr
	}
	if c.get != nil {
		if server, ok := c.get[id]; ok {
			return server, nil
		}
	}
	for _, server := range c.servers {
		if server.CloudID == id {
			return server, nil
		}
	}
	return Server{}, core.Exit(4, "aws instance not found: %s", id)
}

func (c *fakeAWSClient) DeleteServer(_ context.Context, id string) error {
	c.deletedInstances = append(c.deletedInstances, id)
	return nil
}

func (c *fakeAWSClient) DeleteSSHKey(_ context.Context, name string) error {
	c.deletedKeys = append(c.deletedKeys, name)
	return c.deleteKeyErr
}

func (c *fakeAWSClient) ValidateCleanupSSHKey(_ context.Context, name string) error {
	c.validatedKeys = append(c.validatedKeys, name)
	return c.validateKeyErr
}

func (c *fakeAWSClient) DeleteCleanupSSHKey(ctx context.Context, name string) error {
	if err := c.ValidateCleanupSSHKey(ctx, name); err != nil {
		return err
	}
	c.deletedKeys = append(c.deletedKeys, name)
	return c.deleteKeyErr
}

func (c *fakeAWSClient) SetTags(_ context.Context, id string, _ map[string]string) error {
	c.tagged = append(c.tagged, id)
	return nil
}

func (c *fakeAWSClient) CapacityDoctorChecks(context.Context, Config) []core.DoctorCheck {
	return nil
}

func (c *fakeAWSClient) SpotPlacementScores(context.Context, Config) ([]ec2types.SpotPlacementScore, error) {
	return nil, nil
}

func TestAWSAcquireCleansUpCreatedServerAndKeyOnIPFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ipErr := errors.New("ip unavailable")
	keyName := "crabbox-cbx-abcdef123456"
	fake := &fakeAWSClient{
		created:   awsTestServer("i-created", "cbx_created", "created", "us-west-2"),
		createCfg: Config{Provider: "aws", AWSRegion: "us-west-2", ProviderKey: keyName},
		waitErr:   ipErr,
	}
	fake.created.Labels["provider_key"] = keyName
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newAWSClient = oldClient })

	backend := NewAWSLeaseBackend(ProviderSpec{}, Config{Provider: "aws", AWSRegion: "us-west-2"}, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	_, err := backend.acquireOnce(context.Background(), false, "")
	if !errors.Is(err, ipErr) {
		t.Fatalf("err=%v, want IP failure", err)
	}
	if len(fake.deletedInstances) != 1 || fake.deletedInstances[0] != "i-created" {
		t.Fatalf("deleted instances=%v, want created instance cleanup", fake.deletedInstances)
	}
	if len(fake.deletedKeys) != 1 || fake.deletedKeys[0] != keyName {
		t.Fatalf("deleted keys=%v, want created key cleanup", fake.deletedKeys)
	}
}

func TestAWSAcquireCleansUpProviderKeyAcrossRegionsOnCreateFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	createErr := errors.New("capacity unavailable")
	east := &fakeAWSClient{createErr: createErr}
	west := &fakeAWSClient{}
	oldClient := newAWSClient
	newAWSClient = func(_ context.Context, cfg Config) (awsClient, error) {
		switch cfg.AWSRegion {
		case "us-east-1":
			return east, nil
		case "us-west-2":
			return west, nil
		default:
			t.Fatalf("unexpected region %q", cfg.AWSRegion)
			return nil, nil
		}
	}
	t.Cleanup(func() { newAWSClient = oldClient })

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	cfg.Capacity.Regions = []string{"us-east-1", "us-west-2"}
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	_, err := backend.acquireOnce(context.Background(), false, "")
	if !errors.Is(err, createErr) {
		t.Fatalf("err=%v, want create failure", err)
	}
	if len(east.deletedKeys) != 1 || len(west.deletedKeys) != 1 || west.deletedKeys[0] != east.deletedKeys[0] {
		t.Fatalf("east keys=%v west keys=%v, want key cleanup in both regions", east.deletedKeys, west.deletedKeys)
	}
}

func TestAWSResolveAndReleaseUseFallbackRegion(t *testing.T) {
	east := &fakeAWSClient{}
	west := &fakeAWSClient{servers: []Server{awsTestServer("i-west", "cbx_fedcba654321", "west", "us-west-2")}}
	west.servers[0].Labels["provider_key"] = "crabbox-cbx-fedcba654321"
	oldClient := newAWSClient
	newAWSClient = func(_ context.Context, cfg Config) (awsClient, error) {
		switch cfg.AWSRegion {
		case "us-east-1":
			return east, nil
		case "us-west-2":
			return west, nil
		default:
			t.Fatalf("unexpected region %q", cfg.AWSRegion)
			return nil, nil
		}
	}
	t.Cleanup(func() { newAWSClient = oldClient })

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	cfg.Capacity.Regions = []string{"us-east-1", "us-west-2"}
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "west"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "i-west" || lease.Server.Labels["aws_region"] != "us-west-2" {
		t.Fatalf("lease=%#v, want west-region server", lease.Server)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(east.deletedInstances) != 0 || len(east.deletedKeys) != 0 {
		t.Fatalf("east cleanup should be untouched: instances=%v keys=%v", east.deletedInstances, east.deletedKeys)
	}
	if len(west.deletedInstances) != 1 || west.deletedInstances[0] != "i-west" {
		t.Fatalf("west deleted instances=%v, want i-west", west.deletedInstances)
	}
	if len(west.deletedKeys) != 1 || west.deletedKeys[0] != "crabbox-cbx-fedcba654321" {
		t.Fatalf("west deleted keys=%v, want provider key", west.deletedKeys)
	}
}

func TestAWSResolveRawInstanceRejectsExternalServer(t *testing.T) {
	external := Server{
		CloudID:  "i-external",
		Provider: "aws",
		Name:     "prod-db",
		Labels:   map[string]string{},
	}
	external.PublicNet.IPv4.IP = "203.0.113.44"
	fake := &fakeAWSClient{servers: []Server{external}}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newAWSClient = oldClient })

	backend := NewAWSLeaseBackend(ProviderSpec{}, Config{Provider: "aws", AWSRegion: "us-east-1"}, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "i-external", ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "not Crabbox-managed") {
		t.Fatalf("lease=%#v err=%v, want not Crabbox-managed rejection", lease, err)
	}

	if err == nil {
		_ = backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease})
	}
	if len(fake.deletedInstances) != 0 {
		t.Fatalf("deleted instances=%v, want no release for external raw instance", fake.deletedInstances)
	}
}

func TestIsCrabboxAWSLeaseRequiresCanonicalTags(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{name: "crabbox", mutate: func(labels map[string]string) { delete(labels, "crabbox") }},
		{name: "created by", mutate: func(labels map[string]string) { delete(labels, "created_by") }},
		{name: "provider", mutate: func(labels map[string]string) { delete(labels, "provider") }},
		{name: "lease", mutate: func(labels map[string]string) { labels["lease"] = "cbx_not-canonical" }},
		{name: "slug", mutate: func(labels map[string]string) { labels["slug"] = " " }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := awsTestServer("i-managed", "cbx_123456abcdef", "managed", "us-east-1")
			test.mutate(server.Labels)
			if isCrabboxAWSLease(server) {
				t.Fatalf("labels=%v, want ownership rejection", server.Labels)
			}
		})
	}
}

func TestAWSResolveRawInstanceRejectsWrongProviderLabel(t *testing.T) {
	server := awsTestServer("i-wrong-provider", "cbx_123456abcdef", "wrong-provider", "us-east-1")
	server.Labels["provider"] = "gcp"
	fake := &fakeAWSClient{servers: []Server{server}}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newAWSClient = oldClient })

	backend := NewAWSLeaseBackend(ProviderSpec{}, Config{Provider: "aws", AWSRegion: "us-east-1"}, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: "i-wrong-provider", ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "not Crabbox-managed") {
		t.Fatalf("err=%v, want wrong-provider rejection", err)
	}
}

func TestAWSResolveRawInstanceRejectsMissingProviderLabelForRelease(t *testing.T) {
	server := awsTestServer("i-managed", "cbx_123456abcdef", "managed", "us-east-1")
	delete(server.Labels, "provider")
	fake := &fakeAWSClient{servers: []Server{server}}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newAWSClient = oldClient })

	backend := NewAWSLeaseBackend(ProviderSpec{}, Config{Provider: "aws", AWSRegion: "us-east-1"}, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: "i-managed", ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "not Crabbox-managed") {
		t.Fatalf("err=%v, want missing-provider rejection", err)
	}
}

func TestAWSReleaseRejectsForgedOrMismatchedOwnership(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*LeaseTarget)
		message string
	}{
		{
			name: "missing created-by tag",
			mutate: func(lease *LeaseTarget) {
				delete(lease.Server.Labels, "created_by")
			},
			message: "canonical Crabbox ownership tags",
		},
		{
			name: "mismatched lease tag",
			mutate: func(lease *LeaseTarget) {
				lease.LeaseID = "cbx_fedcba654321"
			},
			message: "matching canonical Crabbox ownership tags",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeAWSClient{}
			oldClient := newAWSClient
			newAWSClient = func(context.Context, Config) (awsClient, error) {
				return fake, nil
			}
			t.Cleanup(func() { newAWSClient = oldClient })

			lease := LeaseTarget{
				Server:  awsTestServer("i-managed", "cbx_123456abcdef", "managed", "us-east-1"),
				LeaseID: "cbx_123456abcdef",
			}
			test.mutate(&lease)
			backend := NewAWSLeaseBackend(ProviderSpec{}, Config{Provider: "aws", AWSRegion: "us-east-1"}, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
			err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease})
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("err=%v, want %q", err, test.message)
			}
			if len(fake.deletedInstances) != 0 {
				t.Fatalf("deleted instances=%v, want no destructive call", fake.deletedInstances)
			}
		})
	}
}

func TestAWSReleaseRemovesClaimWhenProviderKeyDeletionFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	keyName := "crabbox-cbx-abcdef123456"
	if err := core.ClaimLeaseForRepoProvider(leaseID, "partial-release", "aws", t.TempDir(), time.Minute, false); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	keyErr := errors.New("iam denied key deletion")
	fake := &fakeAWSClient{deleteKeyErr: keyErr}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newAWSClient = oldClient })

	server := awsTestServer("i-partial", leaseID, "partial-release", "us-west-2")
	server.Labels["provider_key"] = keyName
	backend := NewAWSLeaseBackend(ProviderSpec{}, Config{Provider: "aws", AWSRegion: "us-west-2"}, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{Server: server, LeaseID: leaseID}})
	if !errors.Is(err, keyErr) {
		t.Fatalf("err=%v, want wrapped key deletion error", err)
	}
	if !strings.Contains(err.Error(), "provider key may be orphaned") || !strings.Contains(err.Error(), keyName) {
		t.Fatalf("err=%q, want orphaned provider key diagnostic", err)
	}
	if len(fake.deletedInstances) != 1 || fake.deletedInstances[0] != "i-partial" {
		t.Fatalf("deleted instances=%v, want terminated instance", fake.deletedInstances)
	}
	if len(fake.deletedKeys) != 1 || fake.deletedKeys[0] != keyName {
		t.Fatalf("deleted keys=%v, want failed provider key cleanup attempt", fake.deletedKeys)
	}
	if claim, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || ok || claim.LeaseID != "" {
		t.Fatalf("claim=%+v ok=%v err=%v, want removed claim after instance termination", claim, ok, err)
	}
}

func TestAWSTouchUsesFallbackRegion(t *testing.T) {
	east := &fakeAWSClient{}
	west := &fakeAWSClient{}
	oldClient := newAWSClient
	newAWSClient = func(_ context.Context, cfg Config) (awsClient, error) {
		switch cfg.AWSRegion {
		case "us-east-1":
			return east, nil
		case "us-west-2":
			return west, nil
		default:
			t.Fatalf("unexpected region %q", cfg.AWSRegion)
			return nil, nil
		}
	}
	t.Cleanup(func() { newAWSClient = oldClient })

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	server := awsTestServer("i-west", "cbx_west", "west", "us-west-2")
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	if _, err := backend.Touch(context.Background(), TouchRequest{Lease: LeaseTarget{Server: server, LeaseID: "cbx_west"}, State: "ready"}); err != nil {
		t.Fatal(err)
	}
	if len(east.tagged) != 0 {
		t.Fatalf("east tagged=%v, want untouched", east.tagged)
	}
	if len(west.tagged) != 1 || west.tagged[0] != "i-west" {
		t.Fatalf("west tagged=%v, want i-west", west.tagged)
	}
}

func TestAWSCleanupRequiresExactClaimForFallbackRegionServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tagOnly := awsTestServer("i-tag-only", "cbx_111111111111", "tag-only", "us-west-2")
	tagOnly.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	staleClaim := awsTestServer("i-stale", "cbx_333333333333", "stale", "us-west-2")
	staleClaim.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	owned := awsTestServer("i-owned", "cbx_444444444444", "owned", "us-west-2")
	owned.Labels["provider_key"] = "crabbox-cbx-444444444444"
	owned.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	unowned := awsTestServer("i-unowned", "cbx_222222222222", "unowned", "us-west-2")
	delete(unowned.Labels, "created_by")
	unowned.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	east := &fakeAWSClient{}
	west := &fakeAWSClient{servers: []Server{unowned, tagOnly, staleClaim, owned}}
	oldClient := newAWSClient
	newAWSClient = func(_ context.Context, cfg Config) (awsClient, error) {
		switch cfg.AWSRegion {
		case "us-east-1":
			return east, nil
		case "us-west-2":
			return west, nil
		default:
			t.Fatalf("unexpected region %q", cfg.AWSRegion)
			return nil, nil
		}
	}
	t.Cleanup(func() { newAWSClient = oldClient })

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	cfg.Capacity.Regions = []string{"us-east-1", "us-west-2"}
	claimCfg := Config{Provider: "aws", AWSRegion: "us-west-2"}
	staleOriginal := awsTestServer("i-stale-original", staleClaim.Labels["lease"], staleClaim.Labels["slug"], "us-west-2")
	if err := core.ClaimLeaseTargetForConfig(staleClaim.Labels["lease"], staleClaim.Labels["slug"], claimCfg, staleOriginal, SSHTarget{}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := core.ClaimLeaseTargetForConfig(owned.Labels["lease"], owned.Labels["slug"], claimCfg, owned, SSHTarget{}, time.Hour); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: &stderr}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "skip server id=i-unowned") || !strings.Contains(stderr.String(), "canonical Crabbox ownership tags missing") {
		t.Fatalf("stderr=%q, want unowned skip diagnostic", stderr.String())
	}
	if !strings.Contains(stderr.String(), "skip server id=i-tag-only") || !strings.Contains(stderr.String(), "skip server id=i-stale") || !strings.Contains(stderr.String(), "exact local claim missing or stale") {
		t.Fatalf("stderr=%q, want missing and stale claim skip diagnostics", stderr.String())
	}
	if len(east.deletedInstances) != 0 || len(east.deletedKeys) != 0 {
		t.Fatalf("east cleanup should be untouched: instances=%v keys=%v", east.deletedInstances, east.deletedKeys)
	}
	if len(west.deletedInstances) != 1 || west.deletedInstances[0] != "i-owned" {
		t.Fatalf("west deleted instances=%v, want i-owned", west.deletedInstances)
	}
	if len(west.deletedKeys) != 1 || west.deletedKeys[0] != "crabbox-cbx-444444444444" {
		t.Fatalf("west deleted keys=%v, want owned provider key", west.deletedKeys)
	}
	if _, ok, err := core.ResolveLeaseClaim(owned.Labels["lease"]); err != nil || ok {
		t.Fatalf("owned claim ok=%v err=%v, want removed after deletion", ok, err)
	}
	claim, ok, err := core.ResolveLeaseClaim(staleClaim.Labels["lease"])
	if err != nil || !ok || claim.CloudID != "i-stale-original" {
		t.Fatalf("stale claim=%+v ok=%v err=%v, want unchanged", claim, ok, err)
	}
}

func TestAWSCleanupDryRunRetainsExactClaim(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := awsTestServer("i-dry-run", "cbx_555555555555", "dry-run", "us-west-2")
	server.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	fake := &fakeAWSClient{servers: []Server{server}}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newAWSClient = oldClient })

	cfg := Config{Provider: "aws", AWSRegion: "us-west-2"}
	if err := core.ClaimLeaseTargetForConfig(server.Labels["lease"], server.Labels["slug"], cfg, server, SSHTarget{}, time.Hour); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: &stderr}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedInstances) != 0 || len(fake.deletedKeys) != 0 {
		t.Fatalf("dry-run cleanup mutated provider: instances=%v keys=%v", fake.deletedInstances, fake.deletedKeys)
	}
	claim, ok, err := core.ResolveLeaseClaim(server.Labels["lease"])
	if err != nil || !ok || claim.CloudID != server.CloudID {
		t.Fatalf("claim=%+v ok=%v err=%v, want retained", claim, ok, err)
	}
	if !strings.Contains(stderr.String(), "delete server id=i-dry-run") {
		t.Fatalf("stderr=%q, want report-only delete candidate", stderr.String())
	}
}

func TestAWSCleanupRevalidatesLiveOwnershipBeforeDelete(t *testing.T) {
	snapshot := awsTestServer("i-stale", "cbx_111111111111", "stale", "us-east-1")
	snapshot.Labels["provider_key"] = "crabbox-cbx-111111111111"
	snapshot.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	live := snapshot
	live.Labels = maps.Clone(snapshot.Labels)
	delete(live.Labels, "created_by")
	fake := &fakeAWSClient{
		servers: []Server{snapshot},
		get:     map[string]Server{snapshot.CloudID: live},
	}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
	t.Cleanup(func() { newAWSClient = oldClient })

	var stderr strings.Builder
	backend := NewAWSLeaseBackend(ProviderSpec{}, Config{Provider: "aws", AWSRegion: "us-east-1"}, Runtime{Stderr: &stderr}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.getIDs) != 1 || fake.getIDs[0] != snapshot.CloudID {
		t.Fatalf("live lookups=%v, want %s", fake.getIDs, snapshot.CloudID)
	}
	if len(fake.deletedInstances) != 0 || len(fake.deletedKeys) != 0 {
		t.Fatalf("cleanup crossed changed ownership: instances=%v keys=%v", fake.deletedInstances, fake.deletedKeys)
	}
	if !strings.Contains(stderr.String(), "live instance no longer has canonical Crabbox ownership tags") {
		t.Fatalf("stderr=%q, want changed-ownership skip", stderr.String())
	}
}

func TestAWSCleanupRejectsChangedLiveProviderKey(t *testing.T) {
	snapshot := awsTestServer("i-stale", "cbx_111111111111", "stale", "us-east-1")
	snapshot.Labels["provider_key"] = "crabbox-cbx-111111111111"
	snapshot.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	live := snapshot
	live.Labels = maps.Clone(snapshot.Labels)
	live.Labels["provider_key"] = "crabbox-cbx-222222222222"
	fake := &fakeAWSClient{
		servers: []Server{snapshot},
		get:     map[string]Server{snapshot.CloudID: live},
	}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
	t.Cleanup(func() { newAWSClient = oldClient })

	var stderr strings.Builder
	backend := NewAWSLeaseBackend(ProviderSpec{}, Config{Provider: "aws", AWSRegion: "us-east-1"}, Runtime{Stderr: &stderr}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedInstances) != 0 || len(fake.deletedKeys) != 0 {
		t.Fatalf("cleanup trusted changed provider key: instances=%v keys=%v", fake.deletedInstances, fake.deletedKeys)
	}
	if !strings.Contains(stderr.String(), "live instance provider key") {
		t.Fatalf("stderr=%q, want changed-provider-key skip", stderr.String())
	}
}

func TestAWSCleanupSkipsUnownedLiveProviderKey(t *testing.T) {
	server := awsTestServer("i-stale", "cbx_111111111111", "stale", "us-east-1")
	server.Labels["provider_key"] = "crabbox-cbx-111111111111"
	server.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	fake := &fakeAWSClient{
		servers:        []Server{server},
		get:            map[string]Server{server.CloudID: server},
		validateKeyErr: core.NewAWSCleanupKeyOwnershipError("provider key ownership changed"),
	}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
	t.Cleanup(func() { newAWSClient = oldClient })

	var stderr strings.Builder
	backend := NewAWSLeaseBackend(ProviderSpec{}, Config{Provider: "aws", AWSRegion: "us-east-1"}, Runtime{Stderr: &stderr}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedInstances) != 0 || len(fake.deletedKeys) != 0 {
		t.Fatalf("cleanup crossed provider-key ownership mismatch: instances=%v keys=%v", fake.deletedInstances, fake.deletedKeys)
	}
	if !strings.Contains(stderr.String(), "provider key ownership changed") {
		t.Fatalf("stderr=%q, want provider-key ownership skip", stderr.String())
	}
}

func TestAWSCleanupRevalidatesLiveEligibilityBeforeDelete(t *testing.T) {
	snapshot := awsTestServer("i-renewed", "cbx_111111111111", "renewed", "us-east-1")
	snapshot.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	live := snapshot
	live.Labels = maps.Clone(snapshot.Labels)
	live.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(time.Hour))
	fake := &fakeAWSClient{servers: []Server{snapshot}, get: map[string]Server{snapshot.CloudID: live}}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
	t.Cleanup(func() { newAWSClient = oldClient })

	var stderr strings.Builder
	backend := NewAWSLeaseBackend(ProviderSpec{}, Config{Provider: "aws", AWSRegion: "us-east-1"}, Runtime{Stderr: &stderr}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedInstances) != 0 {
		t.Fatalf("cleanup deleted renewed instance: %v", fake.deletedInstances)
	}
	if !strings.Contains(stderr.String(), "reason=live instance") {
		t.Fatalf("stderr=%q, want renewed-live skip", stderr.String())
	}
}

func TestAWSCleanupContinuesWhenLiveCandidateAlreadyGone(t *testing.T) {
	missing := awsTestServer("i-missing", "cbx_111111111111", "missing", "us-east-1")
	remaining := awsTestServer("i-remaining", "cbx_222222222222", "remaining", "us-east-1")
	for _, server := range []*Server{&missing, &remaining} {
		server.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	}
	fake := &fakeAWSClient{
		servers: []Server{missing, remaining},
		get:     map[string]Server{remaining.CloudID: remaining},
		getErrs: map[string]error{missing.CloudID: core.Exit(4, "aws instance not found: %s", missing.CloudID)},
	}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
	t.Cleanup(func() { newAWSClient = oldClient })

	var stderr strings.Builder
	backend := NewAWSLeaseBackend(ProviderSpec{}, Config{Provider: "aws", AWSRegion: "us-east-1"}, Runtime{Stderr: &stderr}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedInstances) != 1 || fake.deletedInstances[0] != remaining.CloudID {
		t.Fatalf("deleted=%v, want only remaining candidate", fake.deletedInstances)
	}
	if !strings.Contains(stderr.String(), "reason=live instance no longer exists") {
		t.Fatalf("stderr=%q, want already-gone skip", stderr.String())
	}
}

func TestAWSAcquireSuffixesSlugCollisionsAcrossRegions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	stopErr := errors.New("stop after create")
	east := &fakeAWSClient{waitErr: stopErr}
	west := &fakeAWSClient{servers: []Server{awsTestServer("i-west", "cbx_west", "taken", "us-west-2")}}
	oldClient := newAWSClient
	newAWSClient = func(_ context.Context, cfg Config) (awsClient, error) {
		switch cfg.AWSRegion {
		case "us-east-1":
			return east, nil
		case "us-west-2":
			return west, nil
		default:
			t.Fatalf("unexpected region %q", cfg.AWSRegion)
			return nil, nil
		}
	}
	t.Cleanup(func() { newAWSClient = oldClient })

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	cfg.Capacity.Regions = []string{"us-east-1", "us-west-2"}
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	_, err := backend.acquireOnce(context.Background(), false, "taken")
	if !errors.Is(err, stopErr) {
		t.Fatalf("err=%v, want controlled stop after create", err)
	}
	if len(east.createSlugs) != 1 {
		t.Fatalf("create slugs=%v, want one provisioning attempt", east.createSlugs)
	}
	if east.createSlugs[0] == "taken" || !strings.HasPrefix(east.createSlugs[0], "taken-") {
		t.Fatalf("create slug=%q, want suffixed collision slug", east.createSlugs[0])
	}
}

func awsTestServer(id, leaseID, slug, region string) Server {
	server := Server{
		CloudID:  id,
		Provider: "aws",
		Name:     slug,
		Labels: map[string]string{
			"crabbox":    "true",
			"created_by": "crabbox",
			"lease":      leaseID,
			"slug":       slug,
			"provider":   "aws",
			"aws_region": region,
		},
	}
	server.PublicNet.IPv4.IP = "203.0.113.20"
	return server
}
