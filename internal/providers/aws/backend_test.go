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
	deleteServerErr  error
	deletedKeys      []string
	deleteKeyErr     error
	validatedKeys    []string
	validateKeyErr   error
	resolvedKeyID    string
	accountID        string
	accountErr       error
	tagged           []string
	setTagsErr       error
}

func (c *fakeAWSClient) ListCrabboxServers(context.Context) ([]Server, error) {
	return c.servers, nil
}

func (c *fakeAWSClient) CreateServerWithFallback(_ context.Context, cfg Config, _, leaseID, slug string, _ bool, _ func(string, ...any)) (Server, Config, error) {
	c.createCalls++
	c.createSlugs = append(c.createSlugs, slug)
	if c.createErr != nil {
		return Server{}, Config{}, c.createErr
	}
	if c.created.CloudID == "" {
		c.created = awsTestServer("i-created", leaseID, slug, "us-east-1")
	}
	if c.created.Labels["aws_key_pair_id"] == "" {
		c.created.Labels["aws_key_pair_id"] = "key-id-for-" + cfg.ProviderKey
	}
	if c.createCfg.AWSRegion == "" {
		c.createCfg = cfg
		c.createCfg.AWSRegion = "us-east-1"
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
	return c.deleteServerErr
}

func (c *fakeAWSClient) DeleteSSHKey(_ context.Context, name string) error {
	c.deletedKeys = append(c.deletedKeys, name)
	return c.deleteKeyErr
}

func (c *fakeAWSClient) ResolveCleanupSSHKeyID(_ context.Context, name string) (string, error) {
	c.validatedKeys = append(c.validatedKeys, name)
	if c.validateKeyErr != nil {
		return "", c.validateKeyErr
	}
	if c.resolvedKeyID != "" {
		return c.resolvedKeyID, nil
	}
	return "key-id-for-" + name, nil
}

func (c *fakeAWSClient) DeleteCleanupSSHKeyID(_ context.Context, keyPairID string) error {
	c.deletedKeys = append(c.deletedKeys, keyPairID)
	return c.deleteKeyErr
}

func (c *fakeAWSClient) CallerAccountID(context.Context) (string, error) {
	if c.accountErr != nil {
		return "", c.accountErr
	}
	if c.accountID != "" {
		return c.accountID, nil
	}
	return "123456789012", nil
}

func (c *fakeAWSClient) SetTags(_ context.Context, id string, _ map[string]string) error {
	c.tagged = append(c.tagged, id)
	return c.setTagsErr
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
	fake := &fakeAWSClient{
		created:   awsTestServer("i-created", "cbx_created", "created", "us-west-2"),
		createCfg: Config{Provider: "aws", AWSRegion: "us-west-2"},
		waitErr:   ipErr,
	}
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
	if len(fake.deletedKeys) != 1 || fake.deletedKeys[0] != fake.created.Labels["aws_key_pair_id"] {
		t.Fatalf("deleted keys=%v, want immutable created key cleanup", fake.deletedKeys)
	}
}

func TestAWSAcquireBindsImmutableProviderKeyID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fake := &fakeAWSClient{}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
	oldBootstrap := bootstrapAWSWindowsDesktop
	bootstrapAWSWindowsDesktop = func(context.Context, Config, *SSHTarget, string, io.Writer) error { return nil }
	t.Cleanup(func() {
		newAWSClient = oldClient
		bootstrapAWSWindowsDesktop = oldBootstrap
	})

	backend := NewAWSLeaseBackend(ProviderSpec{}, Config{Provider: "aws", TargetOS: "linux", AWSRegion: "us-east-1"}, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	lease, err := backend.acquireOnce(context.Background(), false, "bound-key")
	if err != nil {
		t.Fatal(err)
	}
	keyName := core.ServerProviderKey(lease.Server)
	if got, want := lease.Server.Labels["aws_key_pair_id"], "key-id-for-"+keyName; got != want {
		t.Fatalf("key pair id=%q, want %q", got, want)
	}
	if got := lease.Server.Labels["aws_account_id"]; got != "123456789012" {
		t.Fatalf("account id=%q, want acquisition account binding", got)
	}
	if len(fake.validatedKeys) != 0 {
		t.Fatalf("validated keys=%v, want create-time key binding without name re-resolution", fake.validatedKeys)
	}
}

func TestAWSAcquireRollsBackWhenCleanupIdentityTagsFail(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tagErr := errors.New("tag write failed")
	fake := &fakeAWSClient{setTagsErr: tagErr}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
	oldBootstrap := bootstrapAWSWindowsDesktop
	bootstrapAWSWindowsDesktop = func(context.Context, Config, *SSHTarget, string, io.Writer) error { return nil }
	t.Cleanup(func() {
		newAWSClient = oldClient
		bootstrapAWSWindowsDesktop = oldBootstrap
	})

	backend := NewAWSLeaseBackend(ProviderSpec{}, Config{Provider: "aws", TargetOS: "linux", AWSRegion: "us-east-1"}, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	_, err := backend.acquireOnce(context.Background(), false, "tag-failure")
	if !errors.Is(err, tagErr) {
		t.Fatalf("err=%v, want tag failure", err)
	}
	if len(fake.deletedInstances) != 1 || len(fake.deletedKeys) != 1 || fake.deletedKeys[0] != fake.created.Labels["aws_key_pair_id"] {
		t.Fatalf("rollback instances=%v keys=%v, want exact instance and key cleanup", fake.deletedInstances, fake.deletedKeys)
	}
}

func TestAWSAcquireDoesNotDeleteProviderKeyByNameOnCreateFailure(t *testing.T) {
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
	if len(east.deletedKeys) != 0 || len(west.deletedKeys) != 0 {
		t.Fatalf("east keys=%v west keys=%v, want no unsafe name-based cleanup", east.deletedKeys, west.deletedKeys)
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
	owned.Labels["aws_key_pair_id"] = "key-id-for-crabbox-cbx-444444444444"
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
	if len(west.deletedKeys) != 1 || west.deletedKeys[0] != "key-id-for-crabbox-cbx-444444444444" {
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

func TestAWSCleanupRejectsProviderKeyChangedFromExactClaim(t *testing.T) {
	isolateAWSClaimState(t)
	original := awsTestServer("i-stale", "cbx_666666666666", "stale", "us-east-1")
	original.Labels["provider_key"] = "crabbox-cbx-666666666666"
	original.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	candidate := original
	candidate.Labels = maps.Clone(original.Labels)
	candidate.Labels["provider_key"] = "crabbox-cbx-777777777777"
	fake := &fakeAWSClient{servers: []Server{candidate}}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
	t.Cleanup(func() { newAWSClient = oldClient })

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	claimAWSCleanupServer(t, cfg, original)
	var stderr strings.Builder
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: &stderr}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.getIDs) != 1 || fake.getIDs[0] != original.CloudID || len(fake.deletedInstances) != 0 || len(fake.deletedKeys) != 0 {
		t.Fatalf("cleanup crossed changed claim key: gets=%v instances=%v keys=%v", fake.getIDs, fake.deletedInstances, fake.deletedKeys)
	}
	if !strings.Contains(stderr.String(), "exact local claim missing or stale") {
		t.Fatalf("stderr=%q, want stale exact-claim skip", stderr.String())
	}
	assertAWSClaimCloudID(t, original.Labels["lease"], original.CloudID)
}

func TestAWSCleanupRevalidatesLiveOwnershipBeforeDelete(t *testing.T) {
	isolateAWSClaimState(t)
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

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	claimAWSCleanupServer(t, cfg, snapshot)
	var stderr strings.Builder
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: &stderr}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.getIDs) != 2 || fake.getIDs[0] != snapshot.CloudID || fake.getIDs[1] != snapshot.CloudID {
		t.Fatalf("live lookups=%v, want destructive revalidation plus exact orphan check for %s", fake.getIDs, snapshot.CloudID)
	}
	if len(fake.deletedInstances) != 0 || len(fake.deletedKeys) != 0 {
		t.Fatalf("cleanup crossed changed ownership: instances=%v keys=%v", fake.deletedInstances, fake.deletedKeys)
	}
	if !strings.Contains(stderr.String(), "live instance no longer has canonical Crabbox ownership tags") {
		t.Fatalf("stderr=%q, want changed-ownership skip", stderr.String())
	}
	assertAWSClaimCloudID(t, snapshot.Labels["lease"], snapshot.CloudID)
}

func TestAWSCleanupRejectsChangedLiveProviderKey(t *testing.T) {
	isolateAWSClaimState(t)
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

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	claimAWSCleanupServer(t, cfg, snapshot)
	var stderr strings.Builder
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: &stderr}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedInstances) != 0 || len(fake.deletedKeys) != 0 {
		t.Fatalf("cleanup trusted changed provider key: instances=%v keys=%v", fake.deletedInstances, fake.deletedKeys)
	}
	if !strings.Contains(stderr.String(), "live instance provider key") {
		t.Fatalf("stderr=%q, want changed-provider-key skip", stderr.String())
	}
	assertAWSClaimCloudID(t, snapshot.Labels["lease"], snapshot.CloudID)
}

func TestAWSCleanupSkipsUnownedLiveProviderKey(t *testing.T) {
	isolateAWSClaimState(t)
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

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	claimAWSCleanupServer(t, cfg, server)
	var stderr strings.Builder
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: &stderr}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedInstances) != 0 || len(fake.deletedKeys) != 0 {
		t.Fatalf("cleanup crossed provider-key ownership mismatch: instances=%v keys=%v", fake.deletedInstances, fake.deletedKeys)
	}
	if !strings.Contains(stderr.String(), "provider key ownership changed") {
		t.Fatalf("stderr=%q, want provider-key ownership skip", stderr.String())
	}
	assertAWSClaimCloudID(t, server.Labels["lease"], server.CloudID)
}

func TestAWSCleanupSkipsSameNameReplacementProviderKey(t *testing.T) {
	isolateAWSClaimState(t)
	server := awsTestServer("i-stale", "cbx_111111111111", "stale", "us-east-1")
	server.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	fake := &fakeAWSClient{
		servers:       []Server{server},
		get:           map[string]Server{server.CloudID: server},
		resolvedKeyID: "key-replacement-id",
	}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
	t.Cleanup(func() { newAWSClient = oldClient })

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	claimAWSCleanupServer(t, cfg, server)
	var stderr strings.Builder
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: &stderr}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedInstances) != 0 || len(fake.deletedKeys) != 0 {
		t.Fatalf("cleanup deleted replacement resources: instances=%v keys=%v", fake.deletedInstances, fake.deletedKeys)
	}
	if !strings.Contains(stderr.String(), "does not match exact claim identity") {
		t.Fatalf("stderr=%q, want immutable key mismatch", stderr.String())
	}
	assertAWSClaimCloudID(t, server.Labels["lease"], server.CloudID)
}

func TestAWSCleanupLegacyClaimSkipsUnboundProviderKey(t *testing.T) {
	isolateAWSClaimState(t)
	server := awsTestServer("i-legacy", "cbx_111111111111", "legacy", "us-east-1")
	server.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	fake := &fakeAWSClient{servers: []Server{server}, get: map[string]Server{server.CloudID: server}}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
	t.Cleanup(func() { newAWSClient = oldClient })

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	if err := core.ClaimLeaseTargetForConfig(server.Labels["lease"], server.Labels["slug"], cfg, server, SSHTarget{}, time.Hour); err != nil {
		t.Fatal(err)
	}
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedInstances) != 1 || len(fake.validatedKeys) != 0 || len(fake.deletedKeys) != 0 {
		t.Fatalf("legacy cleanup instances=%v validated_keys=%v deleted_keys=%v", fake.deletedInstances, fake.validatedKeys, fake.deletedKeys)
	}
	assertAWSClaimMissing(t, server.Labels["lease"])
}

func TestAWSCleanupRevalidatesLiveEligibilityBeforeDelete(t *testing.T) {
	isolateAWSClaimState(t)
	snapshot := awsTestServer("i-renewed", "cbx_111111111111", "renewed", "us-east-1")
	snapshot.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	live := snapshot
	live.Labels = maps.Clone(snapshot.Labels)
	live.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(time.Hour))
	fake := &fakeAWSClient{servers: []Server{snapshot}, get: map[string]Server{snapshot.CloudID: live}}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
	t.Cleanup(func() { newAWSClient = oldClient })

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	claimAWSCleanupServer(t, cfg, snapshot)
	var stderr strings.Builder
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: &stderr}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedInstances) != 0 {
		t.Fatalf("cleanup deleted renewed instance: %v", fake.deletedInstances)
	}
	if !strings.Contains(stderr.String(), "reason=live instance") {
		t.Fatalf("stderr=%q, want renewed-live skip", stderr.String())
	}
	assertAWSClaimCloudID(t, snapshot.Labels["lease"], snapshot.CloudID)
}

func TestAWSCleanupContinuesWhenLiveCandidateAlreadyGone(t *testing.T) {
	isolateAWSClaimState(t)
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

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	claimAWSCleanupServer(t, cfg, missing)
	claimAWSCleanupServer(t, cfg, remaining)
	var stderr strings.Builder
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: &stderr}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedInstances) != 1 || fake.deletedInstances[0] != remaining.CloudID {
		t.Fatalf("deleted=%v, want only remaining candidate", fake.deletedInstances)
	}
	if !strings.Contains(stderr.String(), "delete missing server recovery") {
		t.Fatalf("stderr=%q, want already-gone recovery", stderr.String())
	}
	wantKeys := []string{
		"key-id-for-" + core.ServerProviderKey(missing),
		"key-id-for-" + core.ServerProviderKey(remaining),
	}
	if len(fake.deletedKeys) != len(wantKeys) || fake.deletedKeys[0] != wantKeys[0] || fake.deletedKeys[1] != wantKeys[1] {
		t.Fatalf("deleted keys=%v, want %v", fake.deletedKeys, wantKeys)
	}
	assertAWSClaimMissing(t, missing.Labels["lease"])
	assertAWSClaimMissing(t, remaining.Labels["lease"])
}

func TestAWSCleanupRetainsMissingInstanceClaimWhenKeyOwnershipDrifts(t *testing.T) {
	isolateAWSClaimState(t)
	server := awsTestServer("i-missing", "cbx_111111111111", "missing", "us-east-1")
	server.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	fake := &fakeAWSClient{
		servers:        []Server{server},
		getErrs:        map[string]error{server.CloudID: core.Exit(4, "aws instance not found: %s", server.CloudID)},
		validateKeyErr: core.NewAWSCleanupKeyOwnershipError("replacement key"),
	}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
	t.Cleanup(func() { newAWSClient = oldClient })

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	claimAWSCleanupServer(t, cfg, server)
	var stderr strings.Builder
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: &stderr}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedKeys) != 0 {
		t.Fatalf("deleted replacement key: %v", fake.deletedKeys)
	}
	if !strings.Contains(stderr.String(), "replacement key") {
		t.Fatalf("stderr=%q, want key ownership skip", stderr.String())
	}
	assertAWSClaimCloudID(t, server.Labels["lease"], server.CloudID)
}

func TestAWSCleanupRetainsMissingInstanceClaimWhenKeyDeleteFails(t *testing.T) {
	isolateAWSClaimState(t)
	server := awsTestServer("i-missing", "cbx_111111111111", "missing", "us-east-1")
	server.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	deleteErr := errors.New("delete key failed")
	fake := &fakeAWSClient{
		servers:      []Server{server},
		getErrs:      map[string]error{server.CloudID: core.Exit(4, "aws instance not found: %s", server.CloudID)},
		deleteKeyErr: deleteErr,
	}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
	t.Cleanup(func() { newAWSClient = oldClient })

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	claimAWSCleanupServer(t, cfg, server)
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); !errors.Is(err, deleteErr) {
		t.Fatalf("error=%v, want %v", err, deleteErr)
	}
	assertAWSClaimCloudID(t, server.Labels["lease"], server.CloudID)

	fake.servers = nil
	fake.deleteKeyErr = nil
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	assertAWSClaimMissing(t, server.Labels["lease"])
}

func TestAWSCleanupRecoversKeyForTerminalInstance(t *testing.T) {
	isolateAWSClaimState(t)
	server := awsTestServer("i-terminal", "cbx_111111111111", "terminal", "us-east-1")
	server.Status = "terminated"
	server.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	fake := &fakeAWSClient{get: map[string]Server{server.CloudID: server}}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
	t.Cleanup(func() { newAWSClient = oldClient })

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	claimAWSCleanupServer(t, cfg, server)
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedKeys) != 1 {
		t.Fatalf("deleted keys=%v, want terminal instance key recovery", fake.deletedKeys)
	}
	assertAWSClaimMissing(t, server.Labels["lease"])
}

func TestAWSCleanupOrphanRecoverySkipsDifferentAWSAccount(t *testing.T) {
	isolateAWSClaimState(t)
	server := awsTestServer("i-other-account", "cbx_111111111111", "other-account", "us-east-1")
	server.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	fake := &fakeAWSClient{accountID: "999999999999"}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
	t.Cleanup(func() { newAWSClient = oldClient })

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	claimAWSCleanupServer(t, cfg, server)
	var stderr strings.Builder
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: &stderr}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.getIDs) != 0 || len(fake.deletedKeys) != 0 {
		t.Fatalf("cross-account recovery read/deleted resources: gets=%v keys=%v", fake.getIDs, fake.deletedKeys)
	}
	if !strings.Contains(stderr.String(), "current AWS account differs") {
		t.Fatalf("stderr=%q, want account mismatch diagnostic", stderr.String())
	}
	assertAWSClaimCloudID(t, server.Labels["lease"], server.CloudID)
}

func TestAWSCleanupCopiedLeaseTagDoesNotSuppressOrphanRecovery(t *testing.T) {
	isolateAWSClaimState(t)
	claimed := awsTestServer("i-claimed", "cbx_111111111111", "claimed", "us-east-1")
	claimed.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	copy := awsTestServer("i-copy", claimed.Labels["lease"], "copy", "us-east-1")
	copy.Labels["expires_at"] = claimed.Labels["expires_at"]
	fake := &fakeAWSClient{
		servers: []Server{copy},
		getErrs: map[string]error{claimed.CloudID: core.Exit(4, "aws instance not found: %s", claimed.CloudID)},
	}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
	t.Cleanup(func() { newAWSClient = oldClient })

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	claimAWSCleanupServer(t, cfg, claimed)
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedInstances) != 0 || len(fake.deletedKeys) != 1 {
		t.Fatalf("cleanup instances=%v keys=%v, want only exact orphan key recovery", fake.deletedInstances, fake.deletedKeys)
	}
	assertAWSClaimMissing(t, claimed.Labels["lease"])
}

func TestAWSCleanupTreatsInstanceMissingAtDeleteBoundaryAsRemoved(t *testing.T) {
	isolateAWSClaimState(t)
	server := awsTestServer("i-raced", "cbx_333333333333", "raced", "us-east-1")
	server.Labels["provider_key"] = "crabbox-cbx-333333333333"
	server.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	fake := &fakeAWSClient{
		servers:         []Server{server},
		get:             map[string]Server{server.CloudID: server},
		deleteServerErr: core.Exit(4, "aws instance not found: %s", server.CloudID),
	}
	oldClient := newAWSClient
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
	t.Cleanup(func() { newAWSClient = oldClient })

	cfg := Config{Provider: "aws", AWSRegion: "us-east-1"}
	claimAWSCleanupServer(t, cfg, server)
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedKeys) != 1 || fake.deletedKeys[0] != "key-id-for-"+server.Labels["provider_key"] {
		t.Fatalf("deleted keys=%v, want raced instance key cleanup", fake.deletedKeys)
	}
	assertAWSClaimMissing(t, server.Labels["lease"])
}

func isolateAWSClaimState(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func claimAWSCleanupServer(t *testing.T, cfg Config, server Server) {
	t.Helper()
	server.Labels = maps.Clone(server.Labels)
	if server.Labels["aws_key_pair_id"] == "" {
		server.Labels["aws_key_pair_id"] = "key-id-for-" + core.ServerProviderKey(server)
	}
	if server.Labels["aws_account_id"] == "" {
		server.Labels["aws_account_id"] = "123456789012"
	}
	if err := core.ClaimLeaseTargetForConfig(server.Labels["lease"], server.Labels["slug"], cfg, server, SSHTarget{}, time.Hour); err != nil {
		t.Fatal(err)
	}
}

func assertAWSClaimCloudID(t *testing.T, leaseID, cloudID string) {
	t.Helper()
	claim, ok, err := core.ResolveLeaseClaim(leaseID)
	if err != nil || !ok || claim.CloudID != cloudID {
		t.Fatalf("claim=%+v ok=%v err=%v, want cloud id %q", claim, ok, err, cloudID)
	}
}

func assertAWSClaimMissing(t *testing.T, leaseID string) {
	t.Helper()
	if claim, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || ok {
		t.Fatalf("claim=%+v ok=%v err=%v, want removed", claim, ok, err)
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

func TestBootstrapSSHHostRespectsNetworkMode(t *testing.T) {
	t.Parallel()
	base := Config{Provider: "aws"}
	base.Tailscale.Enabled = true
	base.Tailscale.HostnameTemplate = "crabbox-{slug}"
	for _, test := range []struct {
		name    string
		network core.NetworkMode
		enabled bool
		want    string
	}{
		{name: "auto", network: core.NetworkAuto, enabled: true, want: "203.0.113.10"},
		{name: "public", network: core.NetworkPublic, enabled: true, want: "203.0.113.10"},
		{name: "strict tailscale", network: core.NetworkTailscale, enabled: true, want: "crabbox-blue"},
		{name: "tailscale not provisioned", network: core.NetworkTailscale, enabled: false, want: "203.0.113.10"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			cfg.Network = test.network
			cfg.Tailscale.Enabled = test.enabled
			if got := bootstrapSSHHost(cfg, "203.0.113.10", "cbx_testlease", "blue"); got != test.want {
				t.Fatalf("got %q want %q", got, test.want)
			}
		})
	}
}

func TestAWSAcquireUsesTailscaleHostnameOnlyForStrictMode(t *testing.T) {
	for _, test := range []struct {
		name    string
		network core.NetworkMode
		want    string
	}{
		{name: "auto", network: core.NetworkAuto, want: "203.0.113.20"},
		{name: "public", network: core.NetworkPublic, want: "203.0.113.20"},
		{name: "tailscale", network: core.NetworkTailscale, want: "crabbox-bootstrap"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			fake := &fakeAWSClient{}
			oldClient := newAWSClient
			newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
			oldBootstrap := bootstrapAWSWindowsDesktop
			var bootstrapHost string
			bootstrapAWSWindowsDesktop = func(_ context.Context, _ Config, target *SSHTarget, _ string, _ io.Writer) error {
				bootstrapHost = target.Host
				return nil
			}
			t.Cleanup(func() {
				newAWSClient = oldClient
				bootstrapAWSWindowsDesktop = oldBootstrap
			})

			cfg := Config{Provider: "aws", TargetOS: "linux", AWSRegion: "us-east-1", Network: test.network}
			cfg.Tailscale.Enabled = true
			cfg.Tailscale.AuthKey = "test-auth-key"
			cfg.Tailscale.HostnameTemplate = "crabbox-{slug}"
			backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
			if _, err := backend.acquireOnce(context.Background(), false, "bootstrap"); err != nil {
				t.Fatal(err)
			}
			if bootstrapHost != test.want {
				t.Fatalf("bootstrap host=%q, want %q", bootstrapHost, test.want)
			}
		})
	}
}
