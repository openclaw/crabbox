package aws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
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

const awsAcquireRollbackTimeout = 2 * time.Minute

type awsClient interface {
	ListCrabboxServers(context.Context) ([]Server, error)
	CreateServerWithFallback(context.Context, Config, string, string, string, bool, func(string, ...any)) (Server, Config, error)
	WaitForServerIP(context.Context, string) (Server, error)
	GetServer(context.Context, string) (Server, error)
	DeleteServer(context.Context, string) error
	DeleteSSHKey(context.Context, string) error
	SetTags(context.Context, string, map[string]string) error
	CapacityDoctorChecks(context.Context, Config) []core.DoctorCheck
	SpotPlacementScores(context.Context, Config) ([]ec2types.SpotPlacementScore, error)
}

func NewAWSLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = "aws"
	return &awsLeaseBackend{DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt, Delete: deleteServer, StoredLeaseKeys: true}}
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
	servers, err := b.listAcrossRegions(ctx)
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
	server, resolvedCfg, err := client.CreateServerWithFallback(ctx, cfg, publicKey, leaseID, slug, keep, func(format string, args ...any) {
		fmt.Fprintf(b.RT.Stderr, format, args...)
	})
	if err != nil {
		cleanupAWSProviderKeyAcrossRegions(b.RT.Stderr, cfg, cfg.ProviderKey)
		return LeaseTarget{}, err
	}
	cfg = resolvedCfg
	rollback := true
	rollbackCloudID := server.CloudID
	rollbackKeyName := firstNonBlank(core.ServerProviderKey(server), cfg.ProviderKey)
	defer func() {
		if !rollback {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), awsAcquireRollbackTimeout)
		defer cancel()
		cleanupAWSCreatedResources(cleanupCtx, b.RT.Stderr, cfg, rollbackCloudID, rollbackKeyName)
	}()
	client, err = newAWSClient(ctx, cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s server=%s type=%s\n", leaseID, server.DisplayID(), cfg.ServerType)
	server, err = client.WaitForServerIP(ctx, server.CloudID)
	if err != nil {
		return LeaseTarget{}, err
	}
	server = annotateAWSServerRegion(server, cfg.AWSRegion)
	if keyName := core.ServerProviderKey(server); keyName != "" {
		rollbackKeyName = keyName
	}
	target := sshTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if err := bootstrapAWSWindowsDesktop(ctx, cfg, &target, publicKey, b.RT.Stderr); err != nil {
		return LeaseTarget{}, err
	}
	server.Labels["state"] = "ready"
	rollback = false
	if err := client.SetTags(ctx, server.CloudID, server.Labels); err != nil {
		fmt.Fprintf(b.RT.Stderr, "warning: set tags: %v\n", err)
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *awsLeaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	if strings.HasPrefix(req.ID, "i-") {
		var lastErr error
		for _, cfg := range awsRegionConfigs(b.Cfg) {
			client, err := newAWSClient(ctx, cfg)
			if err != nil {
				return LeaseTarget{}, err
			}
			server, err := client.GetServer(ctx, req.ID)
			if err != nil {
				lastErr = err
				if isAWSResolveNotFound(err) {
					continue
				}
				return LeaseTarget{}, err
			}
			server = annotateAWSServerRegion(server, cfg.AWSRegion)
			leaseID := blank(server.Labels["lease"], req.ID)
			target := sshTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
			useStoredTestboxKey(&target, leaseID)
			return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
		}
		if lastErr != nil {
			return LeaseTarget{}, lastErr
		}
	}
	servers, err := b.listAcrossRegions(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	if server, leaseID, err := findServerByAlias(servers, req.ID); err != nil {
		return LeaseTarget{}, err
	} else if leaseID != "" {
		cfg := awsConfigForServer(b.Cfg, server)
		target := sshTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
		useStoredTestboxKey(&target, leaseID)
		return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
	}
	return LeaseTarget{}, exit(4, "lease/server not found: %s", req.ID)
}

func (b *awsLeaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	return b.listAcrossRegions(ctx)
}

func (b *awsLeaseBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	client, err := newAWSClient(ctx, b.Cfg)
	if err != nil {
		return core.DoctorResult{}, err
	}
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return core.DoctorResult{}, err
	}
	result := core.InventoryDoctorResult("aws", len(servers))
	result.Message += fmt.Sprintf(" region=%s default_type=%s", b.Cfg.AWSRegion, b.Cfg.ServerType)
	result.Checks = append(result.Checks, core.DoctorCheck{
		Status:  "ok",
		Check:   "provider",
		Message: result.Message,
		Details: map[string]string{
			"provider":     "aws",
			"region":       b.Cfg.AWSRegion,
			"default_type": b.Cfg.ServerType,
		},
	})
	result.Checks = append(result.Checks, client.CapacityDoctorChecks(ctx, b.Cfg)...)
	return result, nil
}

func (b *awsLeaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	if err := deleteServer(ctx, awsConfigForServer(b.Cfg, req.Lease.Server), req.Lease.Server); err != nil {
		var keyErr *awsProviderKeyCleanupError
		if errors.As(err, &keyErr) {
			removeLeaseClaim(req.Lease.LeaseID)
		}
		return err
	}
	removeLeaseClaim(req.Lease.LeaseID)
	return nil
}

func (b *awsLeaseBackend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("deleted lease=%s server=%s name=%s", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
}

func (b *awsLeaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	cfg := awsConfigForServer(b.Cfg, server)
	if req.IdleTimeout > 0 {
		cfg.IdleTimeout = req.IdleTimeout
	}
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, cfg, req.State, time.Now().UTC())
	client, err := newAWSClient(ctx, cfg)
	if err != nil {
		return server, err
	}
	if err := client.SetTags(ctx, server.CloudID, server.Labels); err != nil {
		return server, err
	}
	return server, nil
}

func (b *awsLeaseBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	servers, err := b.List(ctx, ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, server := range servers {
		shouldDelete, reason := core.ShouldCleanupServer(server, now)
		if !shouldDelete {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		fmt.Fprintf(b.RT.Stderr, "delete server id=%s name=%s\n", server.DisplayID(), server.Name)
		if req.DryRun {
			continue
		}
		if err := deleteServer(ctx, awsConfigForServer(b.Cfg, server), server); err != nil {
			return err
		}
	}
	return nil
}

func (b *awsLeaseBackend) listAcrossRegions(ctx context.Context) ([]LeaseView, error) {
	var all []LeaseView
	for _, cfg := range awsRegionConfigs(b.Cfg) {
		client, err := newAWSClient(ctx, cfg)
		if err != nil {
			return nil, err
		}
		servers, err := client.ListCrabboxServers(ctx)
		if err != nil {
			return nil, err
		}
		for _, server := range servers {
			all = append(all, annotateAWSServerRegion(server, cfg.AWSRegion))
		}
	}
	return all, nil
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

var newAWSClient = func(ctx context.Context, cfg Config) (awsClient, error) {
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

var bootstrapAWSWindowsDesktop = core.BootstrapAWSWindowsDesktop

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
	client, err := newAWSClient(ctx, cfg)
	if err != nil {
		return err
	}
	if err := client.DeleteServer(ctx, server.CloudID); err != nil {
		return err
	}
	if keyName := core.ServerProviderKey(server); core.ValidCrabboxProviderKey(keyName) {
		if err := client.DeleteSSHKey(ctx, keyName); err != nil {
			return &awsProviderKeyCleanupError{keyName: keyName, err: err}
		}
	}
	return nil
}

type awsProviderKeyCleanupError struct {
	keyName string
	err     error
}

func (e *awsProviderKeyCleanupError) Error() string {
	return fmt.Sprintf("deleted aws instance but failed to delete provider key %s; provider key may be orphaned: %v", e.keyName, e.err)
}

func (e *awsProviderKeyCleanupError) Unwrap() error { return e.err }

func removeLeaseClaim(leaseID string) { core.RemoveLeaseClaim(leaseID) }

func cleanupAWSProviderKeyAcrossRegions(stderr io.Writer, cfg Config, keyName string) {
	if !core.ValidCrabboxProviderKey(keyName) {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), awsAcquireRollbackTimeout)
	defer cancel()
	for _, regionCfg := range awsRegionConfigs(cfg) {
		client, err := newAWSClient(cleanupCtx, regionCfg)
		if err != nil {
			fmt.Fprintf(stderr, "warning: create aws cleanup client for key %s region=%s: %v\n", keyName, regionCfg.AWSRegion, err)
			continue
		}
		if err := client.DeleteSSHKey(cleanupCtx, keyName); err != nil {
			fmt.Fprintf(stderr, "warning: cleanup aws key %s region=%s after acquire failure: %v\n", keyName, regionCfg.AWSRegion, err)
		}
	}
}

func cleanupAWSCreatedResources(ctx context.Context, stderr io.Writer, cfg Config, cloudID, keyName string) {
	client, err := newAWSClient(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "warning: create aws cleanup client for %s region=%s: %v\n", cloudID, cfg.AWSRegion, err)
		return
	}
	if strings.TrimSpace(cloudID) != "" {
		if err := client.DeleteServer(ctx, cloudID); err != nil {
			fmt.Fprintf(stderr, "warning: cleanup aws instance %s after acquire failure: %v\n", cloudID, err)
		}
	}
	if core.ValidCrabboxProviderKey(keyName) {
		if err := client.DeleteSSHKey(ctx, keyName); err != nil {
			fmt.Fprintf(stderr, "warning: cleanup aws key %s after acquire failure: %v\n", keyName, err)
		}
	}
}

func awsRegionConfigs(cfg Config) []Config {
	regions := uniqueAWSRegions(append([]string{cfg.AWSRegion}, cfg.Capacity.Regions...))
	if len(regions) == 0 {
		regions = []string{cfg.AWSRegion}
	}
	configs := make([]Config, 0, len(regions))
	for _, region := range regions {
		next := cfg
		next.AWSRegion = region
		configs = append(configs, next)
	}
	return configs
}

func uniqueAWSRegions(regions []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, region := range regions {
		region = strings.TrimSpace(region)
		if region == "" {
			continue
		}
		if _, ok := seen[region]; ok {
			continue
		}
		seen[region] = struct{}{}
		out = append(out, region)
	}
	return out
}

func awsServerRegion(server Server) string {
	if server.Labels == nil {
		return ""
	}
	return strings.TrimSpace(server.Labels["aws_region"])
}

func annotateAWSServerRegion(server Server, region string) Server {
	region = strings.TrimSpace(region)
	if region == "" {
		return server
	}
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	if strings.TrimSpace(server.Labels["aws_region"]) == "" {
		server.Labels["aws_region"] = region
	}
	return server
}

func awsConfigForServer(cfg Config, server Server) Config {
	if region := awsServerRegion(server); region != "" {
		cfg.AWSRegion = region
	}
	return cfg
}

func isAWSResolveNotFound(err error) bool {
	var exitErr core.ExitError
	if core.AsExitError(err, &exitErr) && exitErr.Code == 4 {
		return true
	}
	message := err.Error()
	return strings.Contains(message, "InvalidInstanceID.NotFound") ||
		strings.Contains(message, "aws instance not found")
}
