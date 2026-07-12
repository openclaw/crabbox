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
	ResolveCleanupSSHKeyID(context.Context, string) (string, error)
	DeleteCleanupSSHKeyID(context.Context, string) error
	CallerAccountID(context.Context) (string, error)
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
		return LeaseTarget{}, err
	}
	cfg = resolvedCfg
	rollback := true
	rollbackCloudID := server.CloudID
	rollbackKeyID := strings.TrimSpace(server.Labels["aws_key_pair_id"])
	defer func() {
		if !rollback {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), awsAcquireRollbackTimeout)
		defer cancel()
		cleanupAWSCreatedResources(cleanupCtx, b.RT.Stderr, cfg, rollbackCloudID, rollbackKeyID)
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
	if rollbackKeyID != "" {
		server.Labels["aws_key_pair_id"] = rollbackKeyID
	}
	accountID, err := client.CallerAccountID(ctx)
	if err != nil {
		return LeaseTarget{}, fmt.Errorf("bind AWS caller account: %w", err)
	}
	server.Labels["aws_account_id"] = accountID
	if cfg.Network == core.NetworkTailscale && cfg.Tailscale.Enabled && strings.TrimSpace(cfg.Tailscale.Hostname) == "" {
		cfg.Tailscale.Hostname = core.RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, leaseID, slug, cfg.Provider)
	}
	target := sshTargetForBootstrap(cfg, server.PublicNet.IPv4.IP, leaseID, slug)
	if err := bootstrapAWSWindowsDesktop(ctx, cfg, &target, publicKey, b.RT.Stderr); err != nil {
		return LeaseTarget{}, err
	}
	server.Labels["state"] = "ready"
	if err := client.SetTags(ctx, server.CloudID, server.Labels); err != nil {
		return LeaseTarget{}, fmt.Errorf("persist AWS lease identity tags: %w", err)
	}
	rollback = false
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
			if !isCrabboxAWSLease(server) {
				return LeaseTarget{}, exit(4, "lease/server not found: %s (instance exists but is not Crabbox-managed)", req.ID)
			}
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

func isCrabboxAWSLease(server Server) bool {
	labels := server.Labels
	return labels != nil &&
		labels["crabbox"] == "true" &&
		labels["created_by"] == "crabbox" &&
		labels["provider"] == "aws" &&
		core.IsCanonicalLeaseID(labels["lease"]) &&
		strings.TrimSpace(labels["slug"]) != ""
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
	if !isCrabboxAWSLease(req.Lease.Server) || req.Lease.LeaseID != req.Lease.Server.Labels["lease"] {
		return exit(4, "refusing to release AWS instance %s without matching canonical Crabbox ownership tags", req.Lease.Server.DisplayID())
	}
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
		if !isCrabboxAWSLease(server) {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=canonical Crabbox ownership tags missing\n", server.DisplayID(), server.Name)
			continue
		}
		shouldDelete, reason := core.ShouldCleanupServer(server, now)
		if !shouldDelete {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.RT.Stderr, "delete server id=%s name=%s\n", server.DisplayID(), server.Name)
			continue
		}
		claim, claimErr := requireExactAWSClaim(server, server.Labels["lease"])
		if claimErr != nil {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=exact local claim missing or stale\n", server.DisplayID(), server.Name)
			continue
		}
		cfg := awsConfigForServer(b.Cfg, server)
		client, err := newAWSClient(ctx, cfg)
		if err != nil {
			return err
		}
		if matches, err := awsClaimMatchesCurrentAccount(ctx, client, claim); err != nil {
			return fmt.Errorf("verify AWS cleanup account for %s: %w", server.DisplayID(), err)
		} else if !matches && strings.TrimSpace(claim.Labels["aws_account_id"]) != "" {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=current AWS account differs from exact local claim\n", server.DisplayID(), server.Name)
			continue
		}
		live, err := client.GetServer(ctx, server.CloudID)
		if err != nil {
			if isAWSResolveNotFound(err) {
				cleanupKeyID, keyErr := resolveAWSCleanupKeyID(ctx, client, server, claim)
				if keyErr != nil {
					if core.IsAWSCleanupKeyOwnershipError(keyErr) {
						fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%v\n", server.DisplayID(), server.Name, keyErr)
						continue
					}
					return fmt.Errorf("re-read AWS cleanup key for missing instance %s: %w", server.DisplayID(), keyErr)
				}
				if err := deleteMissingClaimedAWSResourcesWithClient(ctx, client, claim, cleanupKeyID); err != nil {
					return err
				}
				fmt.Fprintf(b.RT.Stderr, "delete missing server recovery id=%s name=%s\n", server.DisplayID(), server.Name)
				continue
			}
			return fmt.Errorf("re-read AWS cleanup candidate %s: %w", server.DisplayID(), err)
		}
		if err := validateAWSCleanupLiveServer(server, live); err != nil {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%v\n", server.DisplayID(), server.Name, err)
			continue
		}
		if shouldDelete, reason := core.ShouldCleanupServer(live, now); !shouldDelete {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=live instance %s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		cleanupKeyID, err := resolveAWSCleanupKeyID(ctx, client, live, claim)
		if err != nil {
			if core.IsAWSCleanupKeyOwnershipError(err) {
				fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%v\n", server.DisplayID(), server.Name, err)
				continue
			}
			return fmt.Errorf("re-read AWS cleanup key %s: %w", core.ServerProviderKey(live), err)
		}
		fmt.Fprintf(b.RT.Stderr, "delete server id=%s name=%s\n", live.DisplayID(), live.Name)
		if err := deleteClaimedAWSServerWithClient(ctx, client, live, claim, cleanupKeyID); err != nil {
			return err
		}
	}
	return b.cleanupOrphanedAWSClaims(ctx, req.DryRun)
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

// bootstrapSSHHost returns the address used for the acquisition readiness probe.
// Strict Tailscale mode cannot fall back to the public address, which can be
// unreachable from same-account EC2 operators even after cloud-init succeeds.
func bootstrapSSHHost(cfg Config, publicIP, leaseID, slug string) string {
	if cfg.Network != core.NetworkTailscale || !cfg.Tailscale.Enabled {
		return publicIP
	}
	hostname := strings.TrimSpace(cfg.Tailscale.Hostname)
	if hostname == "" {
		hostname = core.RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, leaseID, slug, cfg.Provider)
	}
	if hostname == "" {
		return publicIP
	}
	return hostname
}

func sshTargetForBootstrap(cfg Config, publicIP, leaseID, slug string) SSHTarget {
	return sshTargetFromConfig(cfg, bootstrapSSHHost(cfg, publicIP, leaseID, slug))
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
	if !isCrabboxAWSLease(server) {
		return exit(4, "refusing to delete AWS instance %s without canonical Crabbox ownership tags", server.DisplayID())
	}
	client, err := newAWSClient(ctx, cfg)
	if err != nil {
		return err
	}
	return deleteAWSServerWithClient(ctx, client, server)
}

func deleteAWSServerWithClient(ctx context.Context, client awsClient, server Server) error {
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

func requireExactAWSClaim(server Server, expectedLeaseID string) (core.LeaseClaim, error) {
	claim, exists, err := core.ReadLeaseClaimWithPresence(expectedLeaseID)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if !exists {
		return core.LeaseClaim{}, exit(2, "aws lease=%s has no exact local claim; refusing destructive operation", expectedLeaseID)
	}
	if !isCrabboxAWSLease(server) ||
		claim.LeaseID != expectedLeaseID ||
		claim.Provider != "aws" ||
		claim.CloudID == "" ||
		claim.CloudID != server.CloudID ||
		claim.Slug == "" ||
		claim.Slug != server.Labels["slug"] ||
		server.Labels["lease"] != expectedLeaseID ||
		awsServerRegion(server) == "" ||
		strings.TrimSpace(claim.Labels["aws_region"]) != awsServerRegion(server) {
		return core.LeaseClaim{}, exit(2, "refusing to operate on AWS instance %s from a missing or stale exact local claim", server.DisplayID())
	}
	expectedProviderKey := strings.TrimSpace(claim.Labels["provider_key"])
	if expectedProviderKey == "" {
		expectedProviderKey = core.ProviderKeyForLease(expectedLeaseID)
	}
	if strings.TrimSpace(core.ServerProviderKey(server)) != expectedProviderKey {
		return core.LeaseClaim{}, exit(2, "refusing to operate on AWS instance %s whose provider key differs from its exact local claim", server.DisplayID())
	}
	return claim, nil
}

func deleteClaimedAWSServerWithClient(ctx context.Context, client awsClient, server Server, claim core.LeaseClaim, cleanupKeyID string) error {
	var cleanupErr error
	err := core.RemoveLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, func() error {
		cleanupErr = deleteAWSCleanupServerWithClient(ctx, client, server, cleanupKeyID)
		return cleanupErr
	})
	if err != nil {
		return err
	}
	return cleanupErr
}

func resolveAWSCleanupKeyID(ctx context.Context, client awsClient, server Server, claim core.LeaseClaim) (string, error) {
	keyName := core.ServerProviderKey(server)
	expectedKeyID := strings.TrimSpace(claim.Labels["aws_key_pair_id"])
	if !core.ValidCrabboxProviderKey(keyName) || expectedKeyID == "" {
		return "", nil
	}
	keyPairID, err := client.ResolveCleanupSSHKeyID(ctx, keyName)
	if err != nil || keyPairID == "" {
		return keyPairID, err
	}
	if keyPairID != expectedKeyID {
		return "", core.NewAWSCleanupKeyOwnershipError(fmt.Sprintf("AWS cleanup key %q identity %q does not match exact claim identity %q", keyName, keyPairID, expectedKeyID))
	}
	return keyPairID, nil
}

func awsClaimMatchesCurrentAccount(ctx context.Context, client awsClient, claim core.LeaseClaim) (bool, error) {
	expectedAccountID := strings.TrimSpace(claim.Labels["aws_account_id"])
	if expectedAccountID == "" {
		return false, nil
	}
	accountID, err := client.CallerAccountID(ctx)
	if err != nil {
		return false, err
	}
	return accountID == expectedAccountID, nil
}

func isAWSTerminalServer(server Server) bool {
	switch strings.ToLower(strings.TrimSpace(server.Status)) {
	case "shutting-down", "terminated":
		return true
	default:
		return false
	}
}

func (b *awsLeaseBackend) cleanupOrphanedAWSClaims(ctx context.Context, dryRun bool) error {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return err
	}
	for _, claim := range claims {
		if claim.Provider != "aws" || !core.IsCanonicalLeaseID(claim.LeaseID) {
			continue
		}
		labels := claim.Labels
		if labels == nil || labels["lease"] != claim.LeaseID || labels["provider"] != "aws" || strings.TrimSpace(labels["aws_region"]) == "" ||
			!strings.HasPrefix(claim.CloudID, "i-") || !core.ValidCrabboxProviderKey(core.ServerProviderKey(Server{Labels: labels})) ||
			strings.TrimSpace(labels["aws_key_pair_id"]) == "" || strings.TrimSpace(labels["aws_account_id"]) == "" {
			continue
		}
		server := Server{CloudID: claim.CloudID, Provider: "aws", Name: claim.Slug, Labels: labels}
		client, err := newAWSClient(ctx, awsConfigForServer(b.Cfg, server))
		if err != nil {
			return err
		}
		matchesAccount, err := awsClaimMatchesCurrentAccount(ctx, client, claim)
		if err != nil {
			return fmt.Errorf("verify AWS account for orphaned claim %s: %w", claim.LeaseID, err)
		}
		if !matchesAccount {
			fmt.Fprintf(b.RT.Stderr, "skip orphaned AWS claim lease=%s reason=current AWS account differs from exact local claim\n", claim.LeaseID)
			continue
		}
		if live, err := client.GetServer(ctx, claim.CloudID); err == nil {
			if !isAWSTerminalServer(live) {
				continue
			}
		} else if !isAWSResolveNotFound(err) {
			return fmt.Errorf("re-read orphaned AWS claim %s: %w", claim.LeaseID, err)
		}
		cleanupKeyID, err := resolveAWSCleanupKeyID(ctx, client, server, claim)
		if err != nil {
			if core.IsAWSCleanupKeyOwnershipError(err) {
				fmt.Fprintf(b.RT.Stderr, "skip orphaned AWS claim lease=%s reason=%v\n", claim.LeaseID, err)
				continue
			}
			return fmt.Errorf("re-read AWS cleanup key for orphaned claim %s: %w", claim.LeaseID, err)
		}
		if dryRun {
			fmt.Fprintf(b.RT.Stderr, "delete orphaned AWS key recovery lease=%s key=%s\n", claim.LeaseID, core.ServerProviderKey(server))
			continue
		}
		if err := deleteMissingClaimedAWSResourcesWithClient(ctx, client, claim, cleanupKeyID); err != nil {
			return err
		}
		fmt.Fprintf(b.RT.Stderr, "delete orphaned AWS key recovery lease=%s key=%s\n", claim.LeaseID, core.ServerProviderKey(server))
	}
	return nil
}

func deleteMissingClaimedAWSResourcesWithClient(ctx context.Context, client awsClient, claim core.LeaseClaim, cleanupKeyID string) error {
	return core.RemoveLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, func() error {
		return client.DeleteCleanupSSHKeyID(ctx, cleanupKeyID)
	})
}

func deleteAWSCleanupServerWithClient(ctx context.Context, client awsClient, server Server, cleanupKeyID string) error {
	if err := client.DeleteServer(ctx, server.CloudID); err != nil && !isAWSResolveNotFound(err) {
		return err
	}
	if cleanupKeyID != "" {
		if err := client.DeleteCleanupSSHKeyID(ctx, cleanupKeyID); err != nil {
			return &awsProviderKeyCleanupError{keyName: core.ServerProviderKey(server), err: err}
		}
	}
	return nil
}

func validateAWSCleanupLiveServer(expected, live Server) error {
	cloudID := strings.TrimSpace(expected.CloudID)
	if cloudID == "" || strings.TrimSpace(live.CloudID) != cloudID {
		return fmt.Errorf("live cloud id %q does not match cleanup candidate %q", live.CloudID, expected.CloudID)
	}
	if !isCrabboxAWSLease(live) {
		return errors.New("live instance no longer has canonical Crabbox ownership tags")
	}
	expectedLeaseID := strings.TrimSpace(expected.Labels["lease"])
	if liveLeaseID := strings.TrimSpace(live.Labels["lease"]); liveLeaseID != expectedLeaseID {
		return fmt.Errorf("live instance lease %q does not match cleanup candidate lease %q", liveLeaseID, expectedLeaseID)
	}
	expectedProviderKey := strings.TrimSpace(core.ServerProviderKey(expected))
	if liveProviderKey := strings.TrimSpace(core.ServerProviderKey(live)); liveProviderKey != expectedProviderKey {
		return fmt.Errorf("live instance provider key %q does not match cleanup candidate provider key %q", liveProviderKey, expectedProviderKey)
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

func cleanupAWSCreatedResources(ctx context.Context, stderr io.Writer, cfg Config, cloudID, keyPairID string) {
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
	if strings.TrimSpace(keyPairID) != "" {
		if err := client.DeleteCleanupSSHKeyID(ctx, keyPairID); err != nil {
			fmt.Fprintf(stderr, "warning: cleanup aws key pair %s after acquire failure: %v\n", keyPairID, err)
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
