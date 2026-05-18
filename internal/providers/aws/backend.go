package aws

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type Config = core.Config
type Runtime = core.Runtime
type ProviderSpec = core.ProviderSpec
type Backend = core.Backend
type AcquireRequest = core.AcquireRequest
type ResolveRequest = core.ResolveRequest
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type ReleaseLeaseRequest = core.ReleaseLeaseRequest
type TouchRequest = core.TouchRequest
type CleanupRequest = core.CleanupRequest
type LeaseTarget = core.LeaseTarget
type Server = core.Server
type SSHTarget = core.SSHTarget

type awsLeaseBackend struct{ shared.DirectSSHBackend }

func NewAWSLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = "aws"
	return &awsLeaseBackend{DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt}}
}

func (b *awsLeaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	return acquireAttemptsRetry(b.RT, req.Keep, func() (LeaseTarget, error) {
		return b.acquireOnce(ctx, req.Keep, req.RequestedSlug)
	})
}

func (b *awsLeaseBackend) acquireOnce(ctx context.Context, keep bool, requestedSlug string) (LeaseTarget, error) {
	if b.Cfg.Tailscale.Enabled && b.Cfg.Tailscale.AuthKey == "" {
		return LeaseTarget{}, exit(2, "direct --tailscale requires %s to contain a Tailscale auth key; brokered mode uses coordinator OAuth secrets", b.Cfg.Tailscale.AuthKeyEnv)
	}
	cfg := chooseAWSRegion(ctx, b.Cfg, b.RT.Stderr)
	client, err := newAWSClient(ctx, cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	leaseID := newLeaseID()
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	slug, err := allocateDirectLeaseSlug(leaseID, requestedSlug, servers)
	if err != nil {
		return LeaseTarget{}, err
	}
	keyPath, publicKey, err := ensureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg.SSHKey = keyPath
	cfg.ProviderKey = providerKeyForLease(leaseID)
	ensureAWSSSHCIDRs(ctx, &cfg)
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=aws lease=%s slug=%s class=%s preferred_type=%s region=%s keep=%v market=%s strategy=%s\n", leaseID, slug, cfg.Class, cfg.ServerType, cfg.AWSRegion, keep, cfg.Capacity.Market, cfg.Capacity.Strategy)
	server, cfg, err := client.CreateServerWithFallback(ctx, cfg, publicKey, leaseID, slug, keep, func(format string, args ...any) {
		fmt.Fprintf(b.RT.Stderr, format, args...)
	})
	if err != nil {
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s server=%s type=%s\n", leaseID, server.DisplayID(), cfg.ServerType)
	server, err = client.WaitForServerIP(ctx, server.CloudID)
	if err != nil {
		return LeaseTarget{}, err
	}
	target := sshTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if err := bootstrapAWSWindowsDesktop(ctx, cfg, &target, publicKey, b.RT.Stderr); err != nil {
		_ = client.DeleteServer(context.Background(), server.CloudID)
		return LeaseTarget{}, err
	}
	server.Labels["state"] = "ready"
	if err := client.SetTags(ctx, server.CloudID, server.Labels); err != nil {
		fmt.Fprintf(b.RT.Stderr, "warning: set tags: %v\n", err)
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *awsLeaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	client, err := newAWSClient(ctx, b.Cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	if strings.HasPrefix(req.ID, "i-") {
		server, err := client.GetServer(ctx, req.ID)
		if err != nil {
			return LeaseTarget{}, err
		}
		leaseID := blank(server.Labels["lease"], req.ID)
		target := sshTargetFromConfig(b.Cfg, server.PublicNet.IPv4.IP)
		useStoredTestboxKey(&target, leaseID)
		return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
	}
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	if server, leaseID, err := findServerByAlias(servers, req.ID); err != nil {
		return LeaseTarget{}, err
	} else if leaseID != "" {
		target := sshTargetFromConfig(b.Cfg, server.PublicNet.IPv4.IP)
		useStoredTestboxKey(&target, leaseID)
		return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
	}
	return LeaseTarget{}, exit(4, "lease/server not found: %s", req.ID)
}

func (b *awsLeaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := newAWSClient(ctx, b.Cfg)
	if err != nil {
		return nil, err
	}
	return client.ListCrabboxServers(ctx)
}

func (b *awsLeaseBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	result := core.InventoryDoctorResult("aws", len(servers))
	result.Message += fmt.Sprintf(" region=%s default_type=%s", b.Cfg.AWSRegion, b.Cfg.ServerType)
	return result, nil
}

func (b *awsLeaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	if err := deleteServer(ctx, b.Cfg, req.Lease.Server); err != nil {
		return err
	}
	removeLeaseClaim(req.Lease.LeaseID)
	return nil
}

func (b *awsLeaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	return b.DirectSSHBackend.Touch(ctx, req.Lease.Server, req.State), nil
}

func (b *awsLeaseBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	servers, err := b.List(ctx, ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	return b.CleanupServers(ctx, req, servers)
}

func acquireAttemptsRetry(rt Runtime, keep bool, acquire func() (LeaseTarget, error)) (LeaseTarget, error) {
	return shared.AcquireAttemptsRetry(rt, keep, acquire)
}

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func chooseAWSRegion(ctx context.Context, cfg Config, stderr io.Writer) Config {
	if cfg.Provider != "aws" || cfg.Capacity.Market != "spot" || len(cfg.Capacity.Regions) < 2 {
		return cfg
	}
	client, err := core.NewAWSClient(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "warning: spot placement score unavailable: %v\n", err)
		return cfg
	}
	scores, err := client.SpotPlacementScores(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "warning: spot placement score unavailable: %v\n", err)
		return cfg
	}
	if len(scores) == 0 {
		return cfg
	}
	best := ""
	if scores[0].Region != nil {
		best = *scores[0].Region
	}
	score := int32(0)
	if scores[0].Score != nil {
		score = *scores[0].Score
	}
	if best != "" && best != cfg.AWSRegion {
		fmt.Fprintf(stderr, "selected aws region=%s spot_score=%d previous=%s\n", best, score, cfg.AWSRegion)
		cfg.AWSRegion = best
	}
	return cfg
}

func newAWSClient(ctx context.Context, cfg Config) (*core.AWSClient, error) {
	return core.NewAWSClient(ctx, cfg)
}

func newLeaseID() string { return core.NewLeaseID() }
func allocateDirectLeaseSlug(id, requested string, servers []Server) (string, error) {
	return core.AllocateDirectLeaseSlug(id, requested, servers)
}
func ensureTestboxKeyForConfig(cfg Config, leaseID string) (string, string, error) {
	return core.EnsureTestboxKeyForConfig(cfg, leaseID)
}
func providerKeyForLease(leaseID string) string          { return core.ProviderKeyForLease(leaseID) }
func ensureAWSSSHCIDRs(ctx context.Context, cfg *Config) { core.EnsureAWSSSHCIDRs(ctx, cfg) }
func sshTargetFromConfig(cfg Config, host string) SSHTarget {
	return core.SSHTargetFromConfig(cfg, host)
}
func bootstrapAWSWindowsDesktop(ctx context.Context, cfg Config, target *SSHTarget, publicKey string, stderr io.Writer) error {
	return core.BootstrapAWSWindowsDesktop(ctx, cfg, target, publicKey, stderr)
}
func blank(value, fallback string) string { return core.Blank(value, fallback) }
func useStoredTestboxKey(target *SSHTarget, leaseID string) {
	if keyPath, err := core.TestboxKeyPath(leaseID); err == nil {
		if _, statErr := os.Stat(keyPath); statErr == nil {
			target.Key = keyPath
		}
	}
}
func findServerByAlias(servers []Server, id string) (Server, string, error) {
	return core.FindServerByAlias(servers, id)
}
func deleteServer(ctx context.Context, cfg Config, server Server) error {
	return core.DeleteServer(ctx, cfg, server)
}
func removeLeaseClaim(leaseID string) { core.RemoveLeaseClaim(leaseID) }
