package aws

import (
	"context"
	"errors"
	"io"
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
	deletedInstances []string
	deletedKeys      []string
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
	return nil
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
	west := &fakeAWSClient{servers: []Server{awsTestServer("i-west", "cbx_west", "west", "us-west-2")}}
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

func TestAWSCleanupDeletesFallbackRegionServer(t *testing.T) {
	stale := awsTestServer("i-stale", "cbx_stale", "stale", "us-west-2")
	stale.Labels["provider_key"] = "crabbox-cbx-111111111111"
	stale.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	east := &fakeAWSClient{}
	west := &fakeAWSClient{servers: []Server{stale}}
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
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(east.deletedInstances) != 0 || len(east.deletedKeys) != 0 {
		t.Fatalf("east cleanup should be untouched: instances=%v keys=%v", east.deletedInstances, east.deletedKeys)
	}
	if len(west.deletedInstances) != 1 || west.deletedInstances[0] != "i-stale" {
		t.Fatalf("west deleted instances=%v, want i-stale", west.deletedInstances)
	}
	if len(west.deletedKeys) != 1 || west.deletedKeys[0] != "crabbox-cbx-111111111111" {
		t.Fatalf("west deleted keys=%v, want stale provider key", west.deletedKeys)
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
			"lease":      leaseID,
			"slug":       slug,
			"provider":   "aws",
			"aws_region": region,
		},
	}
	server.PublicNet.IPv4.IP = "203.0.113.20"
	return server
}
