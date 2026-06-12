package proxmox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

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

type leaseBackend struct{ shared.DirectSSHBackend }

const proxmoxReleaseAbsentMarker = "proxmox-release-absent"

type proxmoxClient interface {
	DoctorReadiness(context.Context, Config) ([]core.ProxmoxReadinessCheck, error)
	ListCrabboxServers(context.Context) ([]Server, error)
	CreateServer(context.Context, Config, string, string, string, bool) (Server, error)
	GetServer(context.Context, string) (Server, error)
	VMExistsInCluster(context.Context, string) (bool, error)
	DeleteServer(context.Context, string) error
	SetLabels(context.Context, string, map[string]string) error
}

func NewLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = "proxmox"
	if cfg.Proxmox.User != "" {
		cfg.SSHUser = cfg.Proxmox.User
	}
	if cfg.Proxmox.WorkRoot != "" {
		cfg.WorkRoot = cfg.Proxmox.WorkRoot
	}
	return &leaseBackend{DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt, StoredLeaseKeys: true}}
}

func (b *leaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	return shared.AcquireAttemptsRetry(b.RT, req.Keep, func() (LeaseTarget, error) {
		return b.acquireOnce(ctx, req.Keep, req.RequestedSlug)
	})
}

func (b *leaseBackend) acquireOnce(ctx context.Context, keep bool, requestedSlug string) (LeaseTarget, error) {
	if b.Cfg.Proxmox.TemplateID <= 0 {
		return LeaseTarget{}, exit(3, "proxmox templateId is required (set proxmox.templateId or CRABBOX_PROXMOX_TEMPLATE_ID)")
	}
	client, err := newClient(b.Cfg)
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
	cfg := b.Cfg
	keyPath, publicKey, err := ensureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg.SSHKey = keyPath
	cfg.ProviderKey = providerKeyForLease(leaseID)
	cfg.ServerType = proxmoxServerTypeForConfig(cfg)
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=proxmox lease=%s slug=%s node=%s template=%d keep=%v\n",
		leaseID, slug, cfg.Proxmox.Node, cfg.Proxmox.TemplateID, keep)
	server, err := client.CreateServer(ctx, cfg, publicKey, leaseID, slug, keep)
	if err != nil {
		return LeaseTarget{}, err
	}
	if server.PublicNet.IPv4.IP == "" {
		cloudID := server.CloudID
		server, err = b.waitForServerIP(ctx, client, cloudID, bootstrapWaitTimeout(cfg))
		if err != nil {
			if deleteErr := client.DeleteServer(context.Background(), cloudID); deleteErr == nil {
				removeLocalLeaseResidue(leaseID)
			}
			return LeaseTarget{}, err
		}
	}
	target := sshTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if err := waitForSSHReadyFunc(ctx, &target, b.RT.Stderr, "bootstrap", bootstrapWaitTimeout(cfg)); err != nil {
		if deleteErr := client.DeleteServer(context.Background(), server.CloudID); deleteErr == nil {
			removeLocalLeaseResidue(leaseID)
		}
		return LeaseTarget{}, err
	}
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels["state"] = "ready"
	if err := client.SetLabels(ctx, server.CloudID, server.Labels); err != nil {
		fmt.Fprintf(b.RT.Stderr, "warning: set proxmox labels: %v\n", err)
	}
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s server=%s node=%s ip=%s\n", leaseID, server.DisplayID(), cfg.Proxmox.Node, server.PublicNet.IPv4.IP)
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *leaseBackend) waitForServerIP(ctx context.Context, client proxmoxClient, cloudID string, timeout time.Duration) (Server, error) {
	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(proxmoxIPPollInterval)
	defer ticker.Stop()
	for {
		server, err := client.GetServer(deadlineCtx, cloudID)
		if err != nil {
			return Server{}, err
		}
		if server.PublicNet.IPv4.IP != "" {
			return server, nil
		}
		select {
		case <-deadlineCtx.Done():
			return Server{}, deadlineCtx.Err()
		case <-ticker.C:
		}
	}
}

func (b *leaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	client, err := newClient(b.Cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.ID != "" {
		if _, err := strconv.Atoi(req.ID); err == nil || strings.HasPrefix(req.ID, "crabbox-") {
			server, err := client.GetServer(ctx, req.ID)
			if err != nil {
				if !core.IsProxmoxNotFound(err) {
					return LeaseTarget{}, err
				}
			} else {
				if !isCrabboxLease(server) {
					return LeaseTarget{}, exit(4, "lease/server not found: %s (VM exists but is not Crabbox-managed)", req.ID)
				}
				return b.targetForServer(server), nil
			}
		}
	}
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	if server, leaseID, err := findServerByAlias(servers, req.ID); err != nil {
		return LeaseTarget{}, err
	} else if leaseID != "" {
		target := b.targetForServer(server)
		target.LeaseID = leaseID
		return target, nil
	}
	if req.ReleaseOnly {
		return b.releaseTargetFromClaim(ctx, client, req.ID)
	}
	return LeaseTarget{}, exit(4, "lease/server not found: %s", req.ID)
}

func (b *leaseBackend) releaseTargetFromClaim(ctx context.Context, client proxmoxClient, id string) (LeaseTarget, error) {
	var (
		claim core.LeaseClaim
		ok    bool
		err   error
	)
	if _, numeric := strconv.ParseInt(strings.TrimSpace(id), 10, 64); numeric == nil {
		claim, ok, err = b.resolveNumericClaim(id)
	} else {
		var exact bool
		claim, ok, exact, err = core.ResolveLeaseClaimForProviderWithExact(id, "proxmox")
		if err == nil && exact && (!ok || claim.LeaseID != id) {
			return LeaseTarget{}, exit(2, "proxmox exact lease identifier %q does not match a valid Proxmox claim", id)
		}
	}
	if err != nil {
		return LeaseTarget{}, err
	}
	if !ok || claim.LeaseID == "" || !core.LeaseClaimMatchesIdentifier(claim, id) {
		return LeaseTarget{}, exit(4, "lease/server not found: %s", id)
	}
	cloudID := strings.TrimSpace(claim.CloudID)
	vmid, err := strconv.ParseInt(cloudID, 10, 64)
	if err != nil || vmid <= 0 {
		return LeaseTarget{}, exit(2, "proxmox lease claim has invalid VM identity for lease=%s", claim.LeaseID)
	}
	if server, err := client.GetServer(ctx, cloudID); err == nil {
		if !isCrabboxLease(server) || strings.TrimSpace(server.Labels["lease"]) != claim.LeaseID {
			return LeaseTarget{}, exit(2, "refusing to release Proxmox VM %s from stale local claim lease=%s", cloudID, claim.LeaseID)
		}
		return LeaseTarget{LeaseID: claim.LeaseID, Server: server}, nil
	} else if !core.IsProxmoxNotFound(err) {
		return LeaseTarget{}, err
	}
	claimScope := strings.TrimSpace(claim.ProviderScope)
	currentScope := strings.TrimSpace(core.ProviderClaimScope("proxmox", b.Cfg))
	if claimScope == "" || currentScope == "" || claimScope != currentScope {
		return LeaseTarget{}, exit(2, "refusing to accept missing Proxmox VM %s from lease=%s with unverified cluster scope", cloudID, claim.LeaseID)
	}
	exists, err := client.VMExistsInCluster(ctx, cloudID)
	if err != nil {
		return LeaseTarget{}, fmt.Errorf("verify Proxmox VM %s cluster absence: %w", cloudID, err)
	}
	if exists {
		return LeaseTarget{}, exit(2, "refusing to accept missing Proxmox VM %s from lease=%s because it still exists in the cluster", cloudID, claim.LeaseID)
	}
	labels := make(map[string]string, len(claim.Labels)+2)
	for key, value := range claim.Labels {
		labels[key] = value
	}
	if leaseLabel := strings.TrimSpace(labels["lease"]); leaseLabel != "" && leaseLabel != claim.LeaseID {
		return LeaseTarget{}, exit(2, "proxmox lease claim label mismatch for lease=%s", claim.LeaseID)
	}
	if providerLabel := strings.TrimSpace(labels["provider"]); providerLabel != "" && providerLabel != "proxmox" {
		return LeaseTarget{}, exit(2, "proxmox lease claim provider label mismatch for lease=%s", claim.LeaseID)
	}
	labels["lease"] = claim.LeaseID
	labels["provider"] = "proxmox"
	return LeaseTarget{
		LeaseID: claim.LeaseID,
		Server: Server{
			CloudID:  cloudID,
			Provider: "proxmox",
			HostID:   proxmoxReleaseAbsentMarker,
			ID:       vmid,
			Name:     claim.Slug,
			Labels:   labels,
		},
	}, nil
}

func (b *leaseBackend) resolveNumericClaim(cloudID string) (core.LeaseClaim, bool, error) {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	currentScope := strings.TrimSpace(core.ProviderClaimScope("proxmox", b.Cfg))
	var scoped, legacy []core.LeaseClaim
	for _, claim := range claims {
		if claim.Provider != "proxmox" || strings.TrimSpace(claim.CloudID) != cloudID {
			continue
		}
		scope := strings.TrimSpace(claim.ProviderScope)
		switch {
		case scope != "" && scope == currentScope:
			scoped = append(scoped, claim)
		case scope == "":
			legacy = append(legacy, claim)
		}
	}
	if len(scoped) > 1 {
		return core.LeaseClaim{}, false, exit(2, "multiple provider=proxmox claims in the current scope match cloud id %s", cloudID)
	}
	if len(scoped) == 1 {
		return scoped[0], true, nil
	}
	if len(legacy) > 1 {
		return core.LeaseClaim{}, false, exit(2, "multiple unscoped provider=proxmox claims match cloud id %s", cloudID)
	}
	if len(legacy) == 1 {
		return legacy[0], true, nil
	}
	return core.LeaseClaim{}, false, nil
}

func (b *leaseBackend) targetForServer(server Server) LeaseTarget {
	cfg := b.Cfg
	target := sshTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	leaseID := core.Blank(server.Labels["lease"], server.CloudID)
	useStoredTestboxKey(&target, leaseID)
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}
}

func (b *leaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := newClient(b.Cfg)
	if err != nil {
		return nil, err
	}
	return client.ListCrabboxServers(ctx)
}

func (b *leaseBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	client, err := newClient(b.Cfg)
	if err != nil {
		return core.DoctorResult{}, err
	}
	checks, err := client.DoctorReadiness(ctx, b.Cfg)
	if err != nil {
		return core.DoctorResult{}, err
	}
	result := core.DoctorResult{Provider: "proxmox", Checks: make([]core.DoctorCheck, 0, len(checks))}
	for _, check := range checks {
		result.Checks = append(result.Checks, core.DoctorCheck{
			Status:  check.Status,
			Check:   check.Check,
			Message: check.Message,
			Details: check.Details,
		})
	}
	return result, nil
}

func (b *leaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	client, err := newClient(b.Cfg)
	if err != nil {
		return err
	}
	id := req.Lease.Server.CloudID
	if id == "" {
		id = req.Lease.LeaseID
	}
	leaseID := proxmoxClaimLeaseID(req.Lease.Server, req.Lease.LeaseID)
	if req.Lease.Server.HostID != proxmoxReleaseAbsentMarker {
		if err := b.backfillReleaseClaimScope(leaseID, id, req.Lease.Server); err != nil {
			return err
		}
		if err := client.DeleteServer(ctx, id); err != nil && !core.IsProxmoxNotFound(err) {
			return err
		}
	}
	remaining, err := client.ListCrabboxServers(ctx)
	if err != nil {
		fmt.Fprintf(b.RT.Stderr, "warning: preserve local lease residue lease=%s reason=inventory_refresh_failed error=%v\n", leaseID, err)
		return fmt.Errorf("reconcile Proxmox lease after release: %w", err)
	}
	deleted := req.Lease.Server
	deleted.CloudID = id
	if deleted.Labels == nil {
		deleted.Labels = map[string]string{}
	}
	if deleted.Labels["lease"] == "" {
		deleted.Labels["lease"] = leaseID
	}
	removeCleanupLeaseResidue(ctx, client, deleted, remaining, b.Cfg, b.RT.Stderr)
	return nil
}

func (b *leaseBackend) backfillReleaseClaimScope(leaseID, cloudID string, server Server) error {
	if leaseID == "" || proxmoxClaimLabelLeaseID(server) != leaseID {
		return nil
	}
	claim, found, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	if !found || strings.TrimSpace(claim.ProviderScope) != "" {
		return nil
	}
	if claim.Provider != "proxmox" || (claim.CloudID != "" && claim.CloudID != cloudID) {
		return nil
	}
	scope := strings.TrimSpace(core.ProviderClaimScope("proxmox", b.Cfg))
	if scope == "" {
		return exit(2, "cannot safely release legacy Proxmox claim lease=%s without configured cluster scope", leaseID)
	}
	replacement := claim
	replacement.ProviderScope = scope
	if replacement.CloudID == "" {
		replacement.CloudID = cloudID
	}
	return core.ReplaceLeaseClaimIfUnchanged(leaseID, claim, replacement)
}

func (b *leaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	client, err := newClient(b.Cfg)
	if err != nil {
		return Server{}, err
	}
	server := req.Lease.Server
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, b.Cfg, req.State, time.Now().UTC())
	if err := client.SetLabels(ctx, server.CloudID, server.Labels); err != nil {
		return Server{}, err
	}
	return server, nil
}

func (b *leaseBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	servers, err := b.List(ctx, ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	client, err := newClient(b.Cfg)
	if err != nil {
		return err
	}
	var deleted []Server
	var deleteErr error
	var failedDelete *Server
	remaining := append([]Server(nil), servers...)
	for _, server := range servers {
		shouldDelete, reason := core.ShouldCleanupServer(server, time.Now().UTC())
		if !shouldDelete {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		fmt.Fprintf(b.RT.Stderr, "delete server id=%s name=%s\n", server.DisplayID(), server.Name)
		if req.DryRun {
			continue
		}
		if err := client.DeleteServer(ctx, server.CloudID); err != nil {
			deleteErr = err
			failed := server
			failedDelete = &failed
			break
		}
		deleted = append(deleted, server)
		remaining = removeProxmoxServerByCloudID(remaining, server.CloudID)
	}
	var verifyErr error
	if failedDelete != nil {
		disappeared, err := verifyProxmoxDeleteFailure(ctx, client, failedDelete.CloudID, deleteErr)
		if err != nil {
			verifyErr = fmt.Errorf("verify Proxmox server after delete failure: %w", err)
		} else if disappeared {
			remaining = removeProxmoxServerByCloudID(remaining, failedDelete.CloudID)
			deleted = append(deleted, *failedDelete)
		}
	}
	for _, server := range deleted {
		if verifyErr != nil && failedDelete != nil && proxmoxClaimLabelLeaseID(server) == proxmoxClaimLabelLeaseID(*failedDelete) {
			fmt.Fprintf(b.RT.Stderr, "warning: preserve local lease residue lease=%s reason=ambiguous_delete_verification_failed error=%v\n", proxmoxClaimLabelLeaseID(server), verifyErr)
			continue
		}
		removeCleanupLeaseResidue(ctx, client, server, remaining, b.Cfg, b.RT.Stderr)
	}
	return errors.Join(deleteErr, verifyErr)
}

func verifyProxmoxDeleteFailure(ctx context.Context, client proxmoxClient, cloudID string, deleteErr error) (bool, error) {
	if core.IsProxmoxDeleteTaskError(deleteErr) || core.IsProxmoxDeleteRequestError(deleteErr) {
		return waitForProxmoxDeleteReconciliation(ctx, client, cloudID)
	}
	_, err := client.GetServer(ctx, cloudID)
	if err == nil {
		return false, nil
	}
	if core.IsProxmoxNotFound(err) {
		return true, nil
	}
	return false, err
}

func waitForProxmoxDeleteReconciliation(ctx context.Context, client proxmoxClient, cloudID string) (bool, error) {
	verifyCtx, cancel := context.WithTimeout(ctx, proxmoxDeleteVerifyTimeout)
	defer cancel()
	ticker := time.NewTicker(proxmoxDeleteVerifyPollInterval)
	defer ticker.Stop()
	for {
		if _, err := client.GetServer(verifyCtx, cloudID); err == nil {
			// The task may still be completing after its status poll failed.
		} else if core.IsProxmoxNotFound(err) {
			return true, nil
		} else {
			return false, err
		}
		select {
		case <-verifyCtx.Done():
			if errors.Is(verifyCtx.Err(), context.DeadlineExceeded) {
				return false, nil
			}
			return false, verifyCtx.Err()
		case <-ticker.C:
		}
	}
}

func removeProxmoxServerByCloudID(servers []Server, cloudID string) []Server {
	for i, server := range servers {
		if server.CloudID == cloudID {
			return append(servers[:i], servers[i+1:]...)
		}
	}
	return servers
}

func removeCleanupLeaseResidue(ctx context.Context, client proxmoxClient, deleted Server, inventory []Server, cfg Config, stderr io.Writer) {
	leaseID := proxmoxClaimLabelLeaseID(deleted)
	if leaseID == "" {
		return
	}
	missingCloudIDs := map[string]bool{deleted.CloudID: true}
	var survivors []Server
	for _, server := range inventory {
		if server.CloudID != deleted.CloudID && proxmoxClaimLabelLeaseID(server) == leaseID {
			survivors = append(survivors, server)
		}
	}
	claim, found, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_read_failed error=%v\n", leaseID, err)
		return
	}
	if len(survivors) == 1 {
		verified, err := client.GetServer(ctx, survivors[0].CloudID)
		if err == nil {
			if proxmoxClaimLabelLeaseID(verified) != leaseID {
				fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=survivor_ownership_unverified\n", leaseID)
				return
			}
			survivors[0] = verified
		} else if core.IsProxmoxNotFound(err) {
			missingCloudIDs[survivors[0].CloudID] = true
			survivors = nil
		} else {
			fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=survivor_verification_failed error=%v\n", leaseID, err)
			return
		}
	}
	if len(survivors) > 0 {
		if found && len(survivors) == 1 && claim.Provider == "proxmox" {
			canRetarget := claim.CloudID == "" || claim.CloudID == deleted.CloudID || claim.CloudID == survivors[0].CloudID
			if !canRetarget {
				if _, err := client.GetServer(ctx, claim.CloudID); core.IsProxmoxNotFound(err) {
					canRetarget = true
				} else if err != nil {
					fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_cloud_verification_failed error=%v\n", leaseID, err)
					return
				}
			}
			if canRetarget {
				target := sshTargetFromConfig(cfg, survivors[0].PublicNet.IPv4.IP)
				if claim.SSHPort > 0 {
					target.Port = strconv.Itoa(claim.SSHPort)
				}
				if _, err := core.ReplaceLeaseClaimEndpointIfUnchangedWithProviderMetadata(leaseID, claim, survivors[0], target); err != nil {
					fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_retarget_failed error=%v\n", leaseID, err)
					return
				}
			}
		}
		fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=duplicate_remote_lease_label\n", leaseID)
		return
	}
	if !found {
		removeStoredTestboxKey(leaseID)
		return
	}
	if claim.Provider != "proxmox" {
		fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_cloud_mismatch\n", leaseID)
		return
	}
	if claim.CloudID != "" && !missingCloudIDs[claim.CloudID] {
		if _, err := client.GetServer(ctx, claim.CloudID); err == nil {
			fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_cloud_still_exists\n", leaseID)
			return
		} else if core.IsProxmoxNotFound(err) {
			missingCloudIDs[claim.CloudID] = true
		} else {
			fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_cloud_verification_failed error=%v\n", leaseID, err)
			return
		}
	}
	if err := core.RemoveLeaseClaimIfUnchanged(leaseID, claim); err != nil {
		fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_changed error=%v\n", leaseID, err)
		return
	}
	removeStoredTestboxKey(leaseID)
}

func proxmoxClaimLeaseID(server Server, fallback string) string {
	if leaseID := proxmoxClaimLabelLeaseID(server); leaseID != "" {
		return leaseID
	}
	return strings.TrimSpace(fallback)
}

func proxmoxClaimLabelLeaseID(server Server) string {
	if server.Labels != nil {
		if leaseID := strings.TrimSpace(server.Labels["lease"]); leaseID != "" {
			return leaseID
		}
	}
	return ""
}

var newClient = func(cfg Config) (proxmoxClient, error) { return core.NewProxmoxClient(cfg) }

func newLeaseID() string { return core.NewLeaseID() }
func allocateDirectLeaseSlug(id, requested string, servers []Server) (string, error) {
	return core.AllocateDirectLeaseSlug(id, requested, servers)
}
func ensureTestboxKeyForConfig(cfg Config, leaseID string) (string, string, error) {
	return core.EnsureTestboxKeyForConfig(cfg, leaseID)
}
func providerKeyForLease(leaseID string) string { return core.ProviderKeyForLease(leaseID) }
func proxmoxServerTypeForConfig(cfg Config) string {
	return core.ProxmoxServerTypeForConfig(cfg)
}
func sshTargetFromConfig(cfg Config, host string) SSHTarget {
	return core.SSHTargetFromConfig(cfg, host)
}
func waitForSSHReady(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	return core.WaitForSSHReady(ctx, target, stderr, phase, timeout)
}

var waitForSSHReadyFunc = waitForSSHReady

var proxmoxIPPollInterval = 2 * time.Second
var proxmoxDeleteVerifyPollInterval = time.Second
var proxmoxDeleteVerifyTimeout = 30 * time.Second

func bootstrapWaitTimeout(cfg Config) time.Duration { return core.BootstrapWaitTimeout(cfg) }
func findServerByAlias(servers []Server, id string) (Server, string, error) {
	return core.FindServerByAlias(servers, id)
}
func isCrabboxLease(server Server) bool { return core.IsCrabboxProxmoxLease(server) }
func removeLeaseClaim(leaseID string)   { core.RemoveLeaseClaim(leaseID) }
func removeStoredTestboxKey(leaseID string) {
	core.RemoveStoredTestboxKey(leaseID)
}

func removeLocalLeaseResidue(leaseID string) {
	removeLeaseClaim(leaseID)
	removeStoredTestboxKey(leaseID)
}
func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func useStoredTestboxKey(target *SSHTarget, leaseID string) {
	if keyPath, err := core.TestboxKeyPath(leaseID); err == nil {
		if _, statErr := os.Stat(keyPath); statErr == nil {
			target.Key = keyPath
		}
	}
}
