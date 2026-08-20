package aws

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

type fakeAWSClient struct {
	mu               sync.Mutex
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
	controlCreate    func(*core.AWSFixedCreateControl, Config, string, string) (Server, Config, error)
}

func (c *fakeAWSClient) ListCrabboxServers(context.Context) ([]Server, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Server(nil), c.servers...), nil
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

func (c *fakeAWSClient) CreateServerWithFallbackControl(ctx context.Context, cfg Config, publicKey, leaseID, slug string, keep bool, logf func(string, ...any), control *core.AWSFixedCreateControl) (Server, Config, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.controlCreate != nil {
		return c.controlCreate(control, cfg, leaseID, slug)
	}
	c.createCalls++
	c.createSlugs = append(c.createSlugs, slug)
	attempt := core.AWSLaunchAttempt{
		Region: cfg.AWSRegion, ServerType: cfg.ServerType, Market: cfg.Capacity.Market,
		ImageID: "ami-fixed", SecurityGroupID: "sg-fixed", KeyPairID: "key-id-for-" + cfg.ProviderKey,
		ClientToken: "fixed-token", ParametersSHA256: strings.Repeat("a", 64),
	}
	if control.BeforeAttempt != nil {
		if err := control.BeforeAttempt(attempt); err != nil {
			return Server{}, cfg, err
		}
	}
	server := fixedAWSTestServer(control, cfg, leaseID, slug, attempt)
	c.created = server
	c.servers = []Server{server}
	c.createCfg = cfg
	return server, cfg, nil
}

func (c *fakeAWSClient) WaitForServerIP(context.Context, string) (Server, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
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
	testutil.IsolateUserDirs(t)
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

func TestAWSFixedAcquireReplaysSameLeaseAndRejectsIntentDrift(t *testing.T) {
	testutil.IsolateUserDirs(t)
	fake := &fakeAWSClient{}
	restore := installFixedAWSTestClient(t, fake)
	defer restore()
	cfg := fixedAWSTestConfig()
	repo := t.TempDir()
	req := AcquireRequest{Repo: core.Repo{Root: repo}, Keep: true, RequestedLeaseID: "cbx_abcdef123456", RequestedSlug: "fixed-aws"}

	first := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	lease, err := first.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	replayed, err := second.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != req.RequestedLeaseID || replayed.Server.CloudID != lease.Server.CloudID || fake.createCalls != 1 {
		t.Fatalf("lease=%#v replay=%#v creates=%d", lease, replayed, fake.createCalls)
	}

	drifted := req
	drifted.RequestedSlug = "different-slug"
	if _, err := second.Acquire(context.Background(), drifted); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("drift err=%v", err)
	}
	if fake.createCalls != 1 {
		t.Fatalf("creates=%d after drift, want 1", fake.createCalls)
	}

	duplicate := fake.servers[0]
	duplicate.CloudID = "i-fixed-duplicate"
	fake.servers = append(fake.servers, duplicate)
	if _, err := second.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "multiple AWS resources") {
		t.Fatalf("duplicate err=%v", err)
	}
	if fake.createCalls != 1 {
		t.Fatalf("creates=%d after duplicate detection, want 1", fake.createCalls)
	}
}

func TestAWSFixedAcquireRejectsDifferentAcquiredCloudIDWithCopiedTags(t *testing.T) {
	testutil.IsolateUserDirs(t)
	fake := &fakeAWSClient{}
	restore := installFixedAWSTestClient(t, fake)
	defer restore()
	cfg := fixedAWSTestConfig()
	req := AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true, RequestedLeaseID: "cbx_abcdef123481", RequestedSlug: "cloud-id-bound"}
	lease, err := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend).Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	impostor := lease.Server
	impostor.CloudID = "i-copied-fixed-tags"
	impostor.Labels = maps.Clone(lease.Server.Labels)
	fake.servers = []Server{impostor}
	creates := fake.createCalls

	_, err = NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend).Acquire(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") || !strings.Contains(err.Error(), "acquired CloudID") {
		t.Fatalf("different acquired CloudID replay err=%v", err)
	}
	if fake.createCalls != creates {
		t.Fatalf("different acquired CloudID replay called create: before=%d after=%d", creates, fake.createCalls)
	}
}

func TestAWSFixedAcquireConflictsOnExplicitSSHCIDRDrift(t *testing.T) {
	testutil.IsolateUserDirs(t)
	fake := &fakeAWSClient{}
	restore := installFixedAWSTestClient(t, fake)
	defer restore()
	repo := t.TempDir()
	req := AcquireRequest{Repo: core.Repo{Root: repo}, Keep: true, RequestedLeaseID: "cbx_abcdef123468", RequestedSlug: "cidr-drift"}
	firstCfg := fixedAWSTestConfig()
	firstCfg.AWSSSHCIDRs = []string{"198.51.100.10/32"}
	if _, err := NewAWSLeaseBackend(ProviderSpec{}, firstCfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend).Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	creates := fake.createCalls
	secondCfg := fixedAWSTestConfig()
	secondCfg.AWSSSHCIDRs = []string{"203.0.113.20/32"}
	if _, err := NewAWSLeaseBackend(ProviderSpec{}, secondCfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend).Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("explicit CIDR drift err=%v", err)
	}
	if fake.createCalls != creates {
		t.Fatalf("explicit CIDR drift called create: before=%d after=%d", creates, fake.createCalls)
	}
}

func TestAWSFixedAcquireIgnoresDiscoveredUnpinnedSSHCIDRDrift(t *testing.T) {
	testutil.IsolateUserDirs(t)
	fake := &fakeAWSClient{}
	restore := installFixedAWSTestClient(t, fake)
	defer restore()
	oldEnsure := ensureAWSSSHCIDRs
	detected := "198.51.100.10/32"
	var pinnedValues []bool
	ensureAWSSSHCIDRs = func(_ context.Context, cfg *Config) {
		pinnedValues = append(pinnedValues, cfg.AWSSSHCIDRsPinned)
		if len(cfg.AWSSSHCIDRs) == 0 {
			cfg.AWSSSHCIDRs = []string{detected}
		}
	}
	defer func() { ensureAWSSSHCIDRs = oldEnsure }()
	req := AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true, RequestedLeaseID: "cbx_abcdef123469", RequestedSlug: "cidr-discovered"}
	if _, err := NewAWSLeaseBackend(ProviderSpec{}, fixedAWSTestConfig(), Runtime{Stderr: io.Discard}).(*awsLeaseBackend).Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	detected = "203.0.113.20/32"
	if _, err := NewAWSLeaseBackend(ProviderSpec{}, fixedAWSTestConfig(), Runtime{Stderr: io.Discard}).(*awsLeaseBackend).Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(pinnedValues) != 2 || pinnedValues[0] || pinnedValues[1] {
		t.Fatalf("discovered CIDR pin states=%v, want two unpinned calls", pinnedValues)
	}
	if fake.createCalls != 1 || len(fake.servers) != 1 {
		t.Fatalf("discovered CIDR replay creates=%d resources=%d, want one", fake.createCalls, len(fake.servers))
	}
}

func TestAWSFixedAcquireConflictsOnCacheVolumeDrift(t *testing.T) {
	for _, test := range []struct {
		name    string
		leaseID string
		mutate  func(*core.CacheVolumeConfig)
	}{
		{name: "name", leaseID: "cbx_abcdef12347a", mutate: func(volume *core.CacheVolumeConfig) { volume.Name = "npm" }},
		{name: "key", leaseID: "cbx_abcdef12347b", mutate: func(volume *core.CacheVolumeConfig) { volume.Key = "repo-npm" }},
		{name: "path", leaseID: "cbx_abcdef12347c", mutate: func(volume *core.CacheVolumeConfig) { volume.Path = "/var/cache/npm" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			fake := &fakeAWSClient{}
			restore := installFixedAWSTestClient(t, fake)
			defer restore()
			req := AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true, RequestedLeaseID: test.leaseID, RequestedSlug: "cache-drift"}
			firstCfg := fixedAWSTestConfig()
			firstCfg.Cache.Volumes = []core.CacheVolumeConfig{{
				Name: "pnpm", Key: "repo-pnpm", Path: "/var/cache/pnpm", SizeGB: 40, Required: true,
			}}
			if _, err := NewAWSLeaseBackend(ProviderSpec{}, firstCfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend).Acquire(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			creates := fake.createCalls
			secondCfg := firstCfg
			secondCfg.Cache.Volumes = append([]core.CacheVolumeConfig(nil), firstCfg.Cache.Volumes...)
			test.mutate(&secondCfg.Cache.Volumes[0])
			if _, err := NewAWSLeaseBackend(ProviderSpec{}, secondCfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend).Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
				t.Fatalf("cache %s drift err=%v", test.name, err)
			}
			if fake.createCalls != creates {
				t.Fatalf("cache %s drift called create: before=%d after=%d", test.name, creates, fake.createCalls)
			}
		})
	}
}

func TestAWSFixedAcquireClaimDoesNotPersistSecrets(t *testing.T) {
	testutil.IsolateUserDirs(t)
	fake := &fakeAWSClient{}
	restore := installFixedAWSTestClient(t, fake)
	defer restore()
	const secret = "sentinel-fixed-aws-secret"
	cfg := fixedAWSTestConfig()
	cfg.CoordToken = secret
	cfg.CoordAdminToken = secret
	cfg.Access.ClientID = secret
	cfg.Access.ClientSecret = secret
	cfg.Access.Token = secret
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.AuthKey = secret
	cfg.Tailscale.AuthKeyEnv = "SECRET_ENV"
	req := AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true, RequestedLeaseID: "cbx_abcdef12347d", RequestedSlug: "secret-free"}
	if _, err := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend).Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists {
		t.Fatalf("read fixed claim: exists=%t err=%v", exists, err)
	}
	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("fixed claim persisted secret material: %s", data)
	}
	if fake.createCalls != 1 {
		t.Fatalf("creates=%d, want one", fake.createCalls)
	}
}

func TestAWSFixedAcquireRejectsPriorFingerprintVersion(t *testing.T) {
	testutil.IsolateUserDirs(t)
	fake := &fakeAWSClient{}
	restore := installFixedAWSTestClient(t, fake)
	defer restore()
	cfg := fixedAWSTestConfig()
	req := AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true, RequestedLeaseID: "cbx_abcdef12347e", RequestedSlug: "old-version"}
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	if _, err := backend.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := core.WithDurableLeaseClaimLock(req.RequestedLeaseID, func(claim *core.LeaseClaim, exists bool, persist func() error) error {
		if !exists || claim.FixedCreateIntent == nil {
			t.Fatal("fixed claim missing")
		}
		claim.FixedCreateIntent.Version = fixedAWSCreateIntentVersion - 1
		return persist()
	}); err != nil {
		t.Fatal(err)
	}
	creates := fake.createCalls
	if _, err := backend.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("old fingerprint version replay err=%v", err)
	}
	if fake.createCalls != creates {
		t.Fatalf("old fingerprint version replay called create: before=%d after=%d", creates, fake.createCalls)
	}
}

func TestAWSFixedAcquireRejectsAcquiredLeaseWhenBoundInstanceIsMissing(t *testing.T) {
	testutil.IsolateUserDirs(t)
	fake := &fakeAWSClient{}
	restore := installFixedAWSTestClient(t, fake)
	defer restore()
	cfg := fixedAWSTestConfig()
	req := AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true, RequestedLeaseID: "cbx_abcdef123460", RequestedSlug: "acquired-missing"}
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	if _, err := backend.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	creates := fake.createCalls
	fake.servers = nil

	restarted := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	if _, err := restarted.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") || !strings.Contains(err.Error(), "missing its bound AWS instance") {
		t.Fatalf("missing acquired replay err=%v", err)
	}
	if fake.createCalls != creates {
		t.Fatalf("missing acquired replay called create: before=%d after=%d", creates, fake.createCalls)
	}
	if err := restarted.cleanupOrphanedAWSClaims(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != fixedAWSIntentReleased {
		t.Fatalf("missing acquired cleanup did not retain terminal tombstone: exists=%t claim=%#v err=%v", exists, claim, err)
	}
	if fake.createCalls != creates {
		t.Fatalf("missing acquired cleanup called create: before=%d after=%d", creates, fake.createCalls)
	}
}

func TestAWSFixedReleasePersistsTerminalTombstoneAndRejectsReplay(t *testing.T) {
	testutil.IsolateUserDirs(t)
	fake := &fakeAWSClient{}
	restore := installFixedAWSTestClient(t, fake)
	defer restore()
	cfg := fixedAWSTestConfig()
	req := AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true, RequestedLeaseID: "cbx_abcdef123461", RequestedSlug: "released-fixed"}
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	lease, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.ClaimLeaseTargetForRepoConfig(lease.LeaseID, req.RequestedSlug, cfg, lease.Server, lease.SSH, req.Repo.Root, cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	liveClaim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists {
		t.Fatalf("read fixed claim: exists=%t err=%v", exists, err)
	}
	if liveClaim.Provider != core.FixedAWSClaimProvider {
		t.Fatalf("fixed claim provider=%q, want downgrade-safe marker", liveClaim.Provider)
	}
	if resolved, ok, err := core.ResolveLeaseClaimForProvider(req.RequestedLeaseID, "aws"); err != nil || !ok || resolved.Provider != core.FixedAWSClaimProvider {
		t.Fatalf("new client did not resolve fixed marker as AWS: claim=%#v ok=%t err=%v", resolved, ok, err)
	}
	legacyVisible := 0
	for _, candidate := range mustListAWSClaims(t) {
		if candidate.Provider == "aws" {
			legacyVisible++
		}
	}
	if legacyVisible != 0 {
		t.Fatalf("legacy-style Provider==aws cleanup saw %d fixed claim(s)", legacyVisible)
	}
	strippedLease := lease
	strippedLease.Server.Labels = maps.Clone(lease.Server.Labels)
	delete(strippedLease.Server.Labels, "fixed_intent_sha256")
	creates := fake.createCalls
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: strippedLease}); err != nil {
		t.Fatal(err)
	}
	retained, err := backend.RetainLeaseClaimAfterReleaseWithClaim(strippedLease, liveClaim)
	if err != nil {
		t.Fatal(err)
	}
	if !retained {
		core.RemoveLeaseClaim(strippedLease.LeaseID)
		t.Fatal("fixed AWS outer release cleanup would erase its terminal tombstone")
	}
	if !backend.RetainLeaseClaimAfterRelease(strippedLease) {
		t.Fatal("legacy retention hook did not fail closed on the durable fixed tombstone")
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || claim.FixedCreateIntent == nil {
		t.Fatalf("fixed release removed its tombstone: exists=%t claim=%#v", exists, claim)
	}
	if claim.Provider != core.FixedAWSClaimProvider {
		t.Fatalf("terminal provider=%q, want downgrade-safe marker", claim.Provider)
	}
	intent := claim.FixedCreateIntent
	if intent.Version != fixedAWSCreateIntentVersion || intent.Fingerprint == "" || intent.ProviderScope == "" || intent.Slug != req.RequestedSlug || intent.State != fixedAWSIntentReleased {
		t.Fatalf("terminal intent=%#v", intent)
	}
	if claim.CloudID != "" || claim.Labels != nil || claim.SSHHost != "" || len(intent.Attempt) != 0 || len(intent.FailedAttempts) != 0 {
		t.Fatalf("terminal tombstone retained live resource state: claim=%#v", claim)
	}

	if err := backend.cleanupOrphanedAWSClaims(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, stillExists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID); err != nil || !stillExists {
		t.Fatalf("automatic orphan cleanup removed fixed tombstone: exists=%t err=%v", stillExists, err)
	}
	restarted := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	if _, err := restarted.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("released fixed replay err=%v", err)
	}
	if fake.createCalls != creates {
		t.Fatalf("released fixed replay called create: before=%d after=%d", creates, fake.createCalls)
	}
	if len(fake.deletedInstances) != 1 {
		t.Fatalf("deleted instances=%v, want one release", fake.deletedInstances)
	}
}

func TestAWSOrdinaryReleaseKeepsLegacyClaimDeletionBehavior(t *testing.T) {
	testutil.IsolateUserDirs(t)
	server := awsTestServer("i-ordinary-release", "cbx_abcdef12347f", "ordinary-release", "us-east-1")
	server.Labels["provider_key"] = core.ProviderKeyForLease(server.Labels["lease"])
	fake := &fakeAWSClient{servers: []Server{server}}
	restore := installFixedAWSTestClient(t, fake)
	defer restore()
	cfg := fixedAWSTestConfig()
	if err := core.ClaimLeaseTargetForConfig(server.Labels["lease"], server.Labels["slug"], cfg, server, SSHTarget{}, time.Hour); err != nil {
		t.Fatal(err)
	}
	previous, exists, err := core.ReadLeaseClaimWithPresence(server.Labels["lease"])
	if err != nil || !exists {
		t.Fatalf("read ordinary claim: exists=%t err=%v", exists, err)
	}
	backend := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	lease := LeaseTarget{LeaseID: server.Labels["lease"], Server: server}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	retained, err := backend.RetainLeaseClaimAfterReleaseWithClaim(lease, previous)
	if err != nil {
		t.Fatal(err)
	}
	if retained {
		t.Fatal("ordinary AWS release unexpectedly retained its claim")
	}
	if backend.RetainLeaseClaimAfterRelease(lease) {
		t.Fatal("ordinary AWS legacy retention hook unexpectedly retained its claim")
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(server.Labels["lease"]); err != nil || exists {
		t.Fatalf("ordinary AWS claim exists=%t err=%v, want deleted", exists, err)
	}
	if len(fake.deletedInstances) != 1 || fake.deletedInstances[0] != server.CloudID {
		t.Fatalf("deleted instances=%v, want ordinary instance", fake.deletedInstances)
	}
}

func TestAWSReleaseRejectsFixedTagWithoutDurableClaim(t *testing.T) {
	testutil.IsolateUserDirs(t)
	server := awsTestServer("i-fixed-no-claim", "cbx_abcdef123480", "fixed-no-claim", "us-east-1")
	server.Labels["fixed_intent_sha256"] = strings.Repeat("a", 64)
	fake := &fakeAWSClient{servers: []Server{server}}
	restore := installFixedAWSTestClient(t, fake)
	defer restore()
	backend := NewAWSLeaseBackend(ProviderSpec{}, fixedAWSTestConfig(), Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: server.Labels["lease"], Server: server}})
	if err == nil || !strings.Contains(err.Error(), "without its durable create intent") {
		t.Fatalf("release err=%v", err)
	}
	if len(fake.deletedInstances) != 0 {
		t.Fatalf("fixed-looking server deleted without durable claim: %v", fake.deletedInstances)
	}
}

func mustListAWSClaims(t *testing.T) []core.LeaseClaim {
	t.Helper()
	claims, err := core.ListLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	return claims
}

func TestAWSFixedAcquireRecoversCommitThenTimeoutAfterFreshBackend(t *testing.T) {
	testutil.IsolateUserDirs(t)
	fake := &fakeAWSClient{}
	resourceCount := 0
	fake.controlCreate = func(control *core.AWSFixedCreateControl, cfg Config, leaseID, slug string) (Server, Config, error) {
		fake.createCalls++
		attempt := fixedAWSTestAttempt(cfg)
		if err := control.BeforeAttempt(attempt); err != nil {
			return Server{}, cfg, err
		}
		server := fixedAWSTestServer(control, cfg, leaseID, slug, attempt)
		fake.created = server
		fake.servers = []Server{server}
		resourceCount = 1
		return Server{}, cfg, errors.New("transport closed after RunInstances committed")
	}
	restore := installFixedAWSTestClient(t, fake)
	defer restore()
	req := AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true, RequestedLeaseID: "cbx_abcdef123457", RequestedSlug: "commit-timeout"}
	cfg := fixedAWSTestConfig()
	cfg.Tailscale.AuthKey = "fixed-intent-secret-must-not-persist"

	first := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	if _, err := first.Acquire(context.Background(), req); err == nil {
		t.Fatal("expected uncertain create error")
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	claimJSON, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(claimJSON), cfg.Tailscale.AuthKey) {
		t.Fatalf("fixed create claim persisted a secret: %s", claimJSON)
	}
	if claim.FixedCreateIntent == nil || len(claim.FixedCreateIntent.Attempt) == 0 {
		t.Fatalf("fixed create attempt was not persisted before the provider error: %#v", claim.FixedCreateIntent)
	}
	attempt, err := fixedAWSAttemptFromIntent(claim.FixedCreateIntent)
	if err != nil || attempt == nil {
		t.Fatalf("read persisted fixed attempt: attempt=%#v err=%v", attempt, err)
	}
	if err := core.ValidateAWSFixedAttemptAttestation(fake.servers[0].Labels, *attempt); err != nil {
		t.Fatalf("created resource lacks exact attempt attestation: %v", err)
	}
	fake.controlCreate = nil
	second := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	lease, err := second.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "i-fixed" || fake.createCalls != 1 || resourceCount != 1 {
		t.Fatalf("lease=%#v creates=%d resources=%d", lease, fake.createCalls, resourceCount)
	}
}

func TestAWSFixedAcquireTerminalizesDefinitiveProviderRejection(t *testing.T) {
	testutil.IsolateUserDirs(t)
	fake := &fakeAWSClient{}
	fake.controlCreate = func(control *core.AWSFixedCreateControl, cfg Config, leaseID, _ string) (Server, Config, error) {
		fake.createCalls++
		attempt := fixedAWSTestAttempt(cfg)
		if err := control.BeforeAttempt(attempt); err != nil {
			return Server{}, cfg, err
		}
		control.PinnedAttempt = &attempt
		control.TerminalRejection = true
		if err := control.DefiniteFailure(attempt); err != nil {
			return Server{}, cfg, err
		}
		control.PinnedAttempt = nil
		return Server{}, cfg, errors.New("AWS RunInstances rejected request (Blocked)")
	}
	restore := installFixedAWSTestClient(t, fake)
	defer restore()
	req := AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true, RequestedLeaseID: "cbx_abcdef123488", RequestedSlug: "terminal-rejection"}
	backend := NewAWSLeaseBackend(ProviderSpec{}, fixedAWSTestConfig(), Runtime{Stderr: io.Discard}).(*awsLeaseBackend)

	_, err := backend.Acquire(context.Background(), req)
	var exitErr core.ExitError
	if !core.AsExitError(err, &exitErr) || exitErr.Code != 4 || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("err=%v, want terminal lease conflict", err)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.FixedCreateIntent == nil {
		t.Fatalf("claim=%#v exists=%t err=%v", claim, exists, err)
	}
	if err := fixedAWSLeaseKind.ValidateTerminalClaim(claim, core.LeaseClaim{}, req.RequestedLeaseID, nil); err != nil {
		t.Fatalf("terminal claim: %v", err)
	}
	keyPath, err := core.TestboxKeyPath(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("stored key survived terminal no-allocation result: %v", err)
	}
	creates := fake.createCalls
	if _, err := backend.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("terminal replay err=%v", err)
	}
	if fake.createCalls != creates {
		t.Fatalf("terminal replay called create: before=%d after=%d", creates, fake.createCalls)
	}
}

func TestAWSFixedAcquireRejectsMismatchedOrMissingAttemptAttestation(t *testing.T) {
	for _, tag := range []string{
		"fixed_attempt_region",
		"fixed_attempt_az",
		"fixed_attempt_subnet",
		"fixed_attempt_type",
		"fixed_attempt_market",
		"fixed_attempt_image",
		"fixed_attempt_sg",
		"fixed_attempt_host",
		"fixed_attempt_key_pair",
		"fixed_attempt_token_sha256",
		"fixed_attempt_sha256",
	} {
		for _, mutation := range []string{"missing", "mismatched"} {
			t.Run(strings.TrimPrefix(tag, "fixed_attempt_")+"/"+mutation, func(t *testing.T) {
				testutil.IsolateUserDirs(t)
				fake := &fakeAWSClient{}
				fake.controlCreate = func(control *core.AWSFixedCreateControl, cfg Config, leaseID, slug string) (Server, Config, error) {
					fake.createCalls++
					attempt := fixedAWSTestAttempt(cfg)
					attempt.AvailabilityZone = "us-east-1a"
					attempt.SubnetID = "subnet-fixed"
					attempt.HostID = "h-fixed"
					if err := control.BeforeAttempt(attempt); err != nil {
						return Server{}, cfg, err
					}
					fake.created = fixedAWSTestServer(control, cfg, leaseID, slug, attempt)
					return Server{}, cfg, errors.New("transport closed after ambiguous fixed create")
				}
				restore := installFixedAWSTestClient(t, fake)
				defer restore()
				cfg := fixedAWSTestConfig()
				req := AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true, RequestedLeaseID: "cbx_abcdef123482", RequestedSlug: "attempt-attested"}
				if _, err := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend).Acquire(context.Background(), req); err == nil {
					t.Fatal("expected ambiguous create error")
				}
				candidate := fake.created
				candidate.Labels = maps.Clone(fake.created.Labels)
				if mutation == "missing" {
					delete(candidate.Labels, tag)
				} else {
					candidate.Labels[tag] += "-other"
				}
				fake.servers = []Server{candidate}
				fake.controlCreate = nil
				creates := fake.createCalls

				_, err := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend).Acquire(context.Background(), req)
				if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") || !strings.Contains(err.Error(), "durable launch attempt") {
					t.Fatalf("%s %s attestation replay err=%v", tag, mutation, err)
				}
				if fake.createCalls != creates {
					t.Fatalf("%s %s attestation replay called create: before=%d after=%d", tag, mutation, creates, fake.createCalls)
				}
			})
		}
	}
}

func TestAWSFixedAcquireRejectsCopiedAttemptTagsWhenProviderTypeDiffers(t *testing.T) {
	testutil.IsolateUserDirs(t)
	fake := &fakeAWSClient{}
	fake.controlCreate = func(control *core.AWSFixedCreateControl, cfg Config, leaseID, slug string) (Server, Config, error) {
		fake.createCalls++
		attempt := fixedAWSTestAttempt(cfg)
		if err := control.BeforeAttempt(attempt); err != nil {
			return Server{}, cfg, err
		}
		fake.created = fixedAWSTestServer(control, cfg, leaseID, slug, attempt)
		return Server{}, cfg, errors.New("transport closed after ambiguous fixed create")
	}
	restore := installFixedAWSTestClient(t, fake)
	defer restore()
	cfg := fixedAWSTestConfig()
	req := AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true, RequestedLeaseID: "cbx_abcdef123484", RequestedSlug: "provider-attested"}
	if _, err := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend).Acquire(context.Background(), req); err == nil {
		t.Fatal("expected ambiguous create error")
	}
	candidate := fake.created
	candidate.Labels = maps.Clone(fake.created.Labels)
	candidate.ServerType.Name = "m7i.xlarge"
	fake.servers = []Server{candidate}
	fake.controlCreate = nil
	creates := fake.createCalls

	_, err := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend).Acquire(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") || !strings.Contains(err.Error(), "provider identity") {
		t.Fatalf("copied attempt tags with wrong provider type err=%v", err)
	}
	if fake.createCalls != creates {
		t.Fatalf("copied attempt tags replay called create: before=%d after=%d", creates, fake.createCalls)
	}
}

func TestAWSFixedAcquireWaitsForDelayedVisibilityWithoutResubmitting(t *testing.T) {
	testutil.IsolateUserDirs(t)
	fake := &fakeAWSClient{}
	providerCalls := 0
	resourceCount := 0
	fake.controlCreate = func(control *core.AWSFixedCreateControl, cfg Config, leaseID, slug string) (Server, Config, error) {
		providerCalls++
		attempt := fixedAWSTestAttempt(cfg)
		if providerCalls != 1 {
			return Server{}, cfg, errors.New("persisted fixed attempt was resubmitted")
		}
		if err := control.BeforeAttempt(attempt); err != nil {
			return Server{}, cfg, err
		}
		fake.created = fixedAWSTestServer(control, cfg, leaseID, slug, attempt)
		resourceCount = 1
		return Server{}, cfg, errors.New("transport closed before instance became visible")
	}
	restore := installFixedAWSTestClient(t, fake)
	defer restore()
	req := AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true, RequestedLeaseID: "cbx_abcdef123458", RequestedSlug: "delayed-visible"}

	if _, err := NewAWSLeaseBackend(ProviderSpec{}, fixedAWSTestConfig(), Runtime{Stderr: io.Discard}).(*awsLeaseBackend).Acquire(context.Background(), req); err == nil {
		t.Fatal("expected first transport error")
	}
	if _, err := NewAWSLeaseBackend(ProviderSpec{}, fixedAWSTestConfig(), Runtime{Stderr: io.Discard}).(*awsLeaseBackend).Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") || !strings.Contains(err.Error(), "inventory converges") {
		t.Fatalf("first replay err=%v", err)
	}
	if providerCalls != 1 || resourceCount != 1 {
		t.Fatalf("first replay providerCalls=%d resources=%d", providerCalls, resourceCount)
	}
	fake.servers = []Server{fake.created}
	lease, err := NewAWSLeaseBackend(ProviderSpec{}, fixedAWSTestConfig(), Runtime{Stderr: io.Discard}).(*awsLeaseBackend).Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "i-fixed" || providerCalls != 1 || resourceCount != 1 {
		t.Fatalf("lease=%#v providerCalls=%d resources=%d", lease, providerCalls, resourceCount)
	}
}

func TestAWSFixedAcquireSerializesConcurrentReplay(t *testing.T) {
	testutil.IsolateUserDirs(t)
	fake := &fakeAWSClient{}
	restore := installFixedAWSTestClient(t, fake)
	defer restore()
	req := AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true, RequestedLeaseID: "cbx_abcdef123459", RequestedSlug: "concurrent"}
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := NewAWSLeaseBackend(ProviderSpec{}, fixedAWSTestConfig(), Runtime{Stderr: io.Discard}).(*awsLeaseBackend).Acquire(context.Background(), req)
			results <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if fake.createCalls != 1 || len(fake.servers) != 1 {
		t.Fatalf("creates=%d servers=%d", fake.createCalls, len(fake.servers))
	}
}

func TestAWSFixedAcquireOnAcquiredCanReenterClaimLock(t *testing.T) {
	testutil.IsolateUserDirs(t)
	fake := &fakeAWSClient{}
	restore := installFixedAWSTestClient(t, fake)
	defer restore()
	cfg := fixedAWSTestConfig()
	repo := t.TempDir()
	callbackCalled := make(chan struct{}, 1)
	req := AcquireRequest{
		Repo: core.Repo{Root: repo}, Keep: true, RequestedLeaseID: "cbx_abcdef123466", RequestedSlug: "callback-reentry",
		OnAcquired: func(acquired LeaseTarget) error {
			if err := core.ClaimLeaseTargetForRepoConfig(
				acquired.LeaseID, "callback-reentry", cfg, acquired.Server, acquired.SSH,
				repo, cfg.IdleTimeout, false,
			); err != nil {
				return err
			}
			callbackCalled <- struct{}{}
			return nil
		},
	}
	type acquireResult struct {
		lease LeaseTarget
		err   error
	}
	result := make(chan acquireResult, 1)
	go func() {
		lease, err := NewAWSLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend).Acquire(context.Background(), req)
		result <- acquireResult{lease: lease, err: err}
	}()

	// Acquire does real claim-lock and state-file work (~1.3s unloaded), so this budget
	// only has to sit far enough above that to survive a loaded runner and the race
	// detector's slowdown. A genuine deadlock still fails the test, just later.
	timeout := 60 * time.Second
	if testing.Short() {
		timeout = 10 * time.Second
	}
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.lease.LeaseID != req.RequestedLeaseID {
			t.Fatalf("lease=%#v", got.lease)
		}
	case <-time.After(timeout):
		t.Fatalf("fixed OnAcquired did not return within %s; the callback is likely deadlocked re-entering the claim lock", timeout)
	}
	select {
	case <-callbackCalled:
	default:
		t.Fatal("OnAcquired callback was not called")
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != fixedAWSIntentAcquired {
		t.Fatalf("claim=%#v exists=%t err=%v", claim, exists, err)
	}
	if fake.createCalls != 1 || len(fake.servers) != 1 {
		t.Fatalf("creates=%d resources=%d, want one", fake.createCalls, len(fake.servers))
	}
}

func fixedAWSTestConfig() Config {
	cfg := core.BaseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = "linux"
	cfg.AWSRegion = "us-east-1"
	cfg.ServerType = "m7i.large"
	cfg.Capacity.Market = "on-demand"
	return cfg
}

func fixedAWSTestAttempt(cfg Config) core.AWSLaunchAttempt {
	return core.AWSLaunchAttempt{
		Region: cfg.AWSRegion, ServerType: cfg.ServerType, Market: cfg.Capacity.Market,
		ImageID: "ami-fixed", SecurityGroupID: "sg-fixed", KeyPairID: "key-id-for-" + cfg.ProviderKey,
		ClientToken: "fixed-token", ParametersSHA256: strings.Repeat("a", 64),
	}
}

func fixedAWSTestServer(control *core.AWSFixedCreateControl, cfg Config, leaseID, slug string, attempt core.AWSLaunchAttempt) Server {
	server := awsTestServer("i-fixed", leaseID, slug, cfg.AWSRegion)
	server.ServerType.Name = attempt.ServerType
	server.HostID = attempt.HostID
	server.ProviderMetadata = map[string]any{
		"region":           attempt.Region,
		"availabilityZone": attempt.AvailabilityZone,
		"subnetID":         attempt.SubnetID,
		"imageID":          attempt.ImageID,
		"securityGroupIDs": []string{attempt.SecurityGroupID},
		"market":           attempt.Market,
	}
	server.Labels["fixed_intent_sha256"] = control.IntentFingerprint
	server.Labels["aws_account_id"] = control.AccountID
	server.Labels["aws_key_pair_id"] = attempt.KeyPairID
	server.Labels["provider_key"] = cfg.ProviderKey
	for key, value := range core.AWSFixedAttemptAttestationLabels(attempt) {
		server.Labels[key] = value
	}
	return server
}

func installFixedAWSTestClient(t *testing.T, fake *fakeAWSClient) func() {
	t.Helper()
	oldClient := newAWSClient
	oldBootstrap := bootstrapAWSWindowsDesktop
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fake, nil }
	bootstrapAWSWindowsDesktop = func(context.Context, Config, *SSHTarget, string, io.Writer) error { return nil }
	return func() {
		newAWSClient = oldClient
		bootstrapAWSWindowsDesktop = oldBootstrap
	}
}

func TestAWSAcquireBindsImmutableProviderKeyID(t *testing.T) {
	testutil.IsolateUserDirs(t)
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
	testutil.IsolateUserDirs(t)
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
	testutil.IsolateUserDirs(t)
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
	testutil.IsolateUserDirs(t)
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
	testutil.IsolateUserDirs(t)
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
	testutil.IsolateUserDirs(t)
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
	testutil.IsolateUserDirs(t)
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
	testutil.IsolateUserDirs(t)
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
			testutil.IsolateUserDirs(t)
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
