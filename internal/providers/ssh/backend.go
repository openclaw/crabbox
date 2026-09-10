package ssh

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
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

type staticLeaseBackend struct {
	shared.DirectSSHBackend
	mu            sync.Mutex
	acquired      LeaseTarget
	acquiredRoute string
}

const staticProvider = "ssh"

func NewStaticSSHLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = "ssh"
	return &staticLeaseBackend{DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt}}
}

func (b *staticLeaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	cfg := b.Cfg
	if req.RequestedSlug != "" {
		_, _, leaseID, err := staticLease(cfg)
		if err != nil {
			return LeaseTarget{}, err
		}
		slug, err := allocateClaimLeaseSlug(leaseID, req.RequestedSlug)
		if err != nil {
			return LeaseTarget{}, err
		}
		cfg.Static.Name = slug
	}
	server, target, leaseID, err := staticLease(cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	expected, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if exists {
		if err := core.VerifyLeaseClaimUnchanged(leaseID, expected); err != nil {
			return LeaseTarget{}, err
		}
		if req.Repo.Root != "" {
			if err := core.CheckLeaseClaimRepositoryOwner(leaseID, expected, req.Repo.Root, req.Reclaim); err != nil {
				return LeaseTarget{}, err
			}
		}
	}
	if exists && expected.Provider == staticProvider && staticLeaseClaimMatchesConfig(cfg, expected) {
		// Reacquisition must not erase lifecycle history or unrelated labels.
		labels := staticLeaseLabelsFromClaim(expected)
		labels["slug"] = server.Labels["slug"]
		labels["target"] = target.TargetOS
		delete(labels, "windows_mode")
		if target.TargetOS == core.TargetWindows {
			labels["windows_mode"] = target.WindowsMode
		}
		server.Labels = labels
		if state := labels["state"]; state != "" {
			server.Status = state
		}
	}
	fmt.Fprintf(b.RT.Stderr, "using static target lease=%s slug=%s target=%s windows_mode=%s host=%s keep=%v\n", leaseID, serverSlug(server), b.Cfg.TargetOS, b.Cfg.WindowsMode, target.Host, req.Keep)
	route := architectureEndpoint(target)
	if err := waitForSSH(ctx, &target, b.RT.Stderr); err != nil {
		return LeaseTarget{}, err
	}
	lease := LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}
	if err := b.observeArchitecture(ctx, &lease); err != nil {
		return LeaseTarget{}, err
	}
	lease.Server.Labels["architecture_route"] = route
	if !exists {
		lease.Server.Labels["state"] = "ready"
	}
	claim, err := core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, serverSlug(lease.Server), cfg, lease.Server, lease.SSH, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, expected, exists)
	if err != nil {
		return LeaseTarget{}, err
	}
	if claim.LeaseID != "" {
		core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
	} else if req.Repo.Root != "" {
		return LeaseTarget{}, exit(4, "static lease %s claim changed after acquisition", leaseID)
	}
	b.rememberAcquiredLease(lease)
	return lease, nil
}

func (b *staticLeaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	lease, err := b.resolveOffline(req)
	if err != nil {
		return LeaseTarget{}, err
	}
	if !req.Prepare {
		b.reportHistoricalArchitecture(lease.Server)
		return lease, nil
	}
	expected, exists, set := core.ServerLeaseClaimSnapshot(lease.Server)
	if set && exists {
		if err := validateStaticTouchIdentity(b.Cfg, lease, expected); err != nil {
			return LeaseTarget{}, err
		}
	} else if !set && req.Repo.Root != "" {
		// Endpoint overrides can hide a known claim from target selection, but
		// cannot bypass its ID's owner. Do not attach it as endpoint identity.
		expected, exists, err = core.ReadLeaseClaimWithPresence(lease.LeaseID)
		if err != nil {
			return LeaseTarget{}, err
		}
	}
	if exists {
		// Cached ownership must still match disk before any credentialed SSH.
		if err := core.VerifyLeaseClaimUnchanged(lease.LeaseID, expected); err != nil {
			return LeaseTarget{}, err
		}
		if req.Repo.Root != "" {
			if err := core.CheckLeaseClaimRepositoryOwner(lease.LeaseID, expected, req.Repo.Root, req.Reclaim); err != nil {
				return LeaseTarget{}, err
			}
		}
	}
	route := architectureEndpoint(lease.SSH)
	// Claimed routes may already use the previously selected fallback port.
	if lease.Server.Labels["architecture_route"] != "" {
		route = lease.Server.Labels["architecture_route"]
	}
	if err := waitForSSH(ctx, &lease.SSH, b.RT.Stderr); err != nil {
		return LeaseTarget{}, err
	}
	if err := b.observeArchitecture(ctx, &lease); err != nil {
		return LeaseTarget{}, err
	}
	lease.Server.Labels["architecture_route"] = route
	if exists {
		if err := core.VerifyLeaseClaimUnchanged(lease.LeaseID, expected); err != nil {
			return LeaseTarget{}, err
		}
	}
	// Run/pond own claim publication after resolution. Keep fresh evidence in
	// the return value and retain the original CAS snapshot, including on reclaim.
	// Early publication would invalidate the caller's guarded adoption snapshot.
	return lease, nil
}

func (b *staticLeaseBackend) resolveOffline(req ResolveRequest) (LeaseTarget, error) {
	if lease, ok := b.acquiredLeaseForID(req.ID); ok {
		return lease, nil
	}
	if claim, ok, err := staticLeaseClaimForID(b.Cfg, req.ID); err != nil {
		return LeaseTarget{}, err
	} else if ok {
		server, target, leaseID, err := staticLeaseFromClaim(b.Cfg, claim)
		if err != nil {
			return LeaseTarget{}, err
		}
		return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
	}
	server, target, leaseID, err := staticLease(b.Cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.ID == "" || req.ID == leaseID || req.ID == server.Name || req.ID == serverSlug(server) || req.ID == b.Cfg.Static.Host {
		if claim, ok, err := staticLeaseClaimForConfig(b.Cfg); err != nil {
			return LeaseTarget{}, err
		} else if ok {
			server, target, leaseID, err := staticLeaseFromClaim(b.Cfg, claim)
			if err != nil {
				return LeaseTarget{}, err
			}
			return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
		}
	}
	if req.ID == "" || req.ID == leaseID || req.ID == server.Name || req.ID == serverSlug(server) || req.ID == b.Cfg.Static.Host {
		return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
	}
	return LeaseTarget{}, exit(4, "static lease not found: %s", req.ID)
}

func (b *staticLeaseBackend) List(_ context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	if lease, ok := b.acquiredLeaseView(); ok {
		b.reportHistoricalArchitecture(lease.Server)
		return []LeaseView{lease.Server}, nil
	}
	if claim, ok, err := staticLeaseClaimForConfig(b.Cfg); err != nil {
		return nil, err
	} else if ok {
		server, _, _, err := staticLeaseFromClaim(b.Cfg, claim)
		if err != nil {
			return nil, err
		}
		b.reportHistoricalArchitecture(server)
		return []LeaseView{server}, nil
	}
	server, _, _, err := staticLease(b.Cfg)
	if err != nil {
		return nil, err
	}
	b.reportHistoricalArchitecture(server)
	return []LeaseView{server}, nil
}

func (b *staticLeaseBackend) Doctor(ctx context.Context, req core.DoctorRequest) (core.DoctorResult, error) {
	if b.Cfg.Static.Host == "" {
		return core.DoctorResult{}, exit(3, "missing static.host")
	}
	wsl2 := b.Cfg.TargetOS == core.TargetWindows && b.Cfg.WindowsMode == "wsl2"
	if wsl2 && !req.ProbeSSH {
		return core.DoctorResult{Provider: "ssh", Checks: []core.DoctorCheck{{Status: "skip", Check: "wsl2-sftp", Message: "runtime=unchecked transport=sftp_required mutation=false rerun=crabbox_doctor_--provider_ssh_--target_windows_--windows-mode_wsl2_--doctor-probe-ssh"}}}, nil
	}
	runtime := "unchecked"
	api := "static_config"
	if req.ProbeSSH {
		_, target, _, err := staticLease(b.Cfg)
		if err != nil {
			return core.DoctorResult{}, err
		}
		if err := waitForSSHReady(ctx, &target, b.RT.Stderr, "doctor", 10*time.Second); err != nil {
			if wsl2 && isWSLSFTPUnavailable(err) {
				return core.DoctorResult{Provider: "ssh", Checks: []core.DoctorCheck{{Status: "failed", Check: "wsl2-sftp", Message: "runtime=ssh_reachable transport=sftp_required mutation=false remediation=enable_internal-sftp_restart_sshd_then_rerun_with_--doctor-probe-ssh"}}}, nil
			}
			return core.DoctorResult{}, err
		}
		api = "ssh_probe"
		runtime = "ssh_reachable"
	}
	if wsl2 {
		return core.DoctorResult{Provider: "ssh", Checks: []core.DoctorCheck{{Status: "ok", Check: "wsl2-sftp", Message: "runtime=ssh_reachable transport=sftp_ready mutation=false"}}}, nil
	}
	return core.DoctorResult{
		Provider: "ssh",
		Message:  fmt.Sprintf("target=%s windows_mode=%s host=%s api=%s mutation=false runtime=%s", b.Cfg.TargetOS, b.Cfg.WindowsMode, b.Cfg.Static.Host, api, runtime),
	}, nil
}

func (b *staticLeaseBackend) ReleaseLease(_ context.Context, req ReleaseLeaseRequest) error {
	removeLeaseClaim(req.Lease.LeaseID)
	b.clearAcquiredLease(req.Lease.LeaseID)
	return nil
}

func (b *staticLeaseBackend) PreservesSSHWorkspaceAfterRelease() bool { return true }

func (b *staticLeaseBackend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("released static lease=%s host=%s", lease.LeaseID, lease.SSH.Host)
}

func (b *staticLeaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	updated, err := shared.CommitClaimTouch(ctx, req, shared.ClaimTouchPolicy{
		Provider: "static",
		Authorize: func(_ context.Context, lease LeaseTarget, claim core.LeaseClaim) error {
			return validateStaticTouchIdentity(b.Cfg, lease, claim)
		},
		Prepare: func(expected core.LeaseClaim) (map[string]string, time.Time) {
			now := time.Now().UTC()
			if b.RT.Clock != nil {
				now = b.RT.Clock.Now().UTC()
			}
			cfg := b.Cfg
			if expected.IdleTimeoutSeconds > 0 {
				cfg.IdleTimeout = time.Duration(expected.IdleTimeoutSeconds) * time.Second
			}
			labels := core.TouchDirectLeaseLabelsWithIdleTimeoutOverride(staticLeaseLabelsFromClaim(expected), cfg, req.State, now, req.IdleTimeoutOverride)
			return labels, now
		},
	})
	if err != nil {
		return Server{}, err
	}
	server := req.Lease.Server
	server.Labels = updated.Labels
	server.ServerType.Architecture = ""
	historicalArchitecture(&server, req.Lease.SSH)
	if state := strings.TrimSpace(server.Labels["state"]); state != "" {
		server.Status = state
	}
	core.SetServerLeaseClaimSnapshot(&server, updated, true)
	b.refreshAcquiredLeaseServer(req.Lease.LeaseID, server)
	return server, nil
}

func (b *staticLeaseBackend) Cleanup(context.Context, CleanupRequest) error {
	return exit(2, "machine cleanup is not supported for provider=%s", b.Cfg.Provider)
}

func staticLease(cfg Config) (Server, SSHTarget, string, error) { return core.StaticLease(cfg) }
func serverSlug(server Server) string                           { return core.ServerSlug(server) }

var waitForSSH = core.WaitForSSH
var waitForSSHReady = core.WaitForSSHReady
var isWSLSFTPUnavailable = core.IsWSLSFTPUnavailable

func (b *staticLeaseBackend) rememberAcquiredLease(lease LeaseTarget) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.acquired = lease
	_, configured, _, _ := staticLease(b.Cfg)
	b.acquiredRoute = architectureEndpoint(configured)
}

func (b *staticLeaseBackend) refreshAcquiredLeaseServer(leaseID string, server Server) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.acquired.LeaseID == leaseID {
		b.acquired.Server = server
	}
}

func (b *staticLeaseBackend) acquiredLeaseForID(id string) (LeaseTarget, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, configured, _, _ := staticLease(b.Cfg)
	if b.acquiredRoute != architectureEndpoint(configured) || !staticLeaseTargetMatchesID(b.acquired, id) {
		return LeaseTarget{}, false
	}
	lease := b.acquired
	historicalArchitecture(&lease.Server, lease.SSH)
	return lease, true
}

func (b *staticLeaseBackend) acquiredLeaseView() (LeaseTarget, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, configured, _, _ := staticLease(b.Cfg)
	if b.acquired.LeaseID == "" || b.acquiredRoute != architectureEndpoint(configured) {
		return LeaseTarget{}, false
	}
	lease := b.acquired
	historicalArchitecture(&lease.Server, lease.SSH)
	return lease, true
}

func (b *staticLeaseBackend) clearAcquiredLease(leaseID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.acquired.LeaseID == leaseID {
		b.acquired = LeaseTarget{}
	}
}

func staticLeaseTargetMatchesID(lease LeaseTarget, id string) bool {
	return id != "" && lease.LeaseID != "" && (id == lease.LeaseID || id == lease.Server.Name || id == serverSlug(lease.Server) || id == lease.SSH.Host)
}

func staticLeaseClaimForID(cfg Config, id string) (core.LeaseClaim, bool, error) {
	claim, ok, err := resolveLeaseClaimForProvider(id, staticProvider)
	if err != nil || !ok {
		return claim, ok, err
	}
	if !staticLeaseClaimMatchesConfig(cfg, claim) {
		return core.LeaseClaim{}, false, nil
	}
	return claim, true, nil
}

func staticLeaseClaimForConfig(cfg Config) (core.LeaseClaim, bool, error) {
	_, _, leaseID, err := staticLease(cfg)
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	claim, ok, err := staticLeaseClaimForID(cfg, leaseID)
	if err != nil || !ok {
		return claim, ok, err
	}
	return claim, true, nil
}

func staticLeaseFromClaim(cfg Config, claim core.LeaseClaim) (Server, SSHTarget, string, error) {
	if claim.LeaseID != "" {
		cfg.Static.ID = claim.LeaseID
	}
	if claim.Slug != "" {
		cfg.Static.Name = claim.Slug
	}
	if claim.StaticHost != "" {
		cfg.Static.Host = claim.StaticHost
	}
	if claim.StaticUser != "" && cfg.Static.User == "" {
		cfg.Static.User = claim.StaticUser
	}
	if claim.StaticPort != "" && cfg.Static.Port == "" {
		cfg.Static.Port = claim.StaticPort
	}
	if claim.StaticWorkRoot != "" && cfg.Static.WorkRoot == "" {
		cfg.Static.WorkRoot = claim.StaticWorkRoot
	}
	if claim.TargetOS != "" && !core.IsTargetExplicit(&cfg) {
		cfg.TargetOS = claim.TargetOS
		if claim.WindowsMode != "" && !core.IsWindowsModeExplicit(cfg) {
			cfg.WindowsMode = claim.WindowsMode
		}
	}
	if claim.IdleTimeoutSeconds > 0 {
		cfg.IdleTimeout = time.Duration(claim.IdleTimeoutSeconds) * time.Second
	}
	server, target, leaseID, err := staticLease(cfg)
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	server.Labels = staticLeaseLabelsFromClaim(claim)
	server.Labels["target"] = target.TargetOS
	delete(server.Labels, "windows_mode")
	if target.TargetOS == core.TargetWindows {
		server.Labels["windows_mode"] = target.WindowsMode
	}
	if server.Labels["architecture_route"] == architectureEndpoint(target) && claim.SSHPort > 0 {
		target.Port = strconv.Itoa(claim.SSHPort)
	}
	historicalArchitecture(&server, target)
	if state := strings.TrimSpace(server.Labels["state"]); state != "" {
		server.Status = state
	}
	core.SetServerLeaseClaimSnapshot(&server, claim, true)
	return server, target, leaseID, nil
}

func staticLeaseLabelsFromClaim(claim core.LeaseClaim) map[string]string {
	labels := shared.CloneLabels(claim.Labels)
	labels["lease"] = claim.LeaseID
	labels["slug"] = claim.Slug
	labels["provider"] = staticProvider
	if claim.TargetOS != "" {
		labels["target"] = claim.TargetOS
	}
	if claim.WindowsMode != "" {
		labels["windows_mode"] = claim.WindowsMode
	}
	if claim.IdleTimeoutSeconds > 0 {
		seconds := strconv.Itoa(claim.IdleTimeoutSeconds)
		labels["idle_timeout"] = seconds
		labels["idle_timeout_secs"] = seconds
	}
	if labels["created_at"] == "" {
		labels["created_at"] = persistedClaimTimeLabel(claim.ClaimedAt)
	}
	if labels["last_touched_at"] == "" {
		labels["last_touched_at"] = persistedClaimTimeLabel(claim.LastUsedAt)
	}
	return labels
}

func persistedClaimTimeLabel(value string) string {
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
			return core.LeaseLabelTime(parsed)
		}
	}
	return ""
}

func validateStaticTouchIdentity(cfg Config, lease LeaseTarget, claim core.LeaseClaim) error {
	leaseID := strings.TrimSpace(lease.LeaseID)
	if leaseID == "" || claim.LeaseID != leaseID {
		return exit(4, "static lease claim ID mismatch: expected %s, found %s", leaseID, claim.LeaseID)
	}
	if claim.Provider != staticProvider || lease.Server.Provider != staticProvider || claim.Labels["provider"] != staticProvider {
		return exit(4, "static lease %s provider identity mismatch", leaseID)
	}
	if claim.ProviderScope != core.ProviderClaimScope(staticProvider, cfg) {
		return exit(4, "static lease %s provider scope mismatch", leaseID)
	}
	if claim.CloudID == "" || claim.CloudID != leaseID || lease.Server.CloudID != claim.CloudID || claim.Labels["lease"] != leaseID {
		return exit(4, "static lease %s resource identity mismatch", leaseID)
	}
	host := strings.TrimSpace(claim.StaticHost)
	if host == "" || strings.TrimSpace(cfg.Static.Host) != host || strings.TrimSpace(lease.SSH.Host) != host || strings.TrimSpace(lease.Server.PublicNet.IPv4.IP) != host {
		return exit(4, "static lease %s host identity mismatch", leaseID)
	}
	return nil
}

func staticLeaseClaimMatchesConfig(cfg Config, claim core.LeaseClaim) bool {
	if claim.StaticHost != "" && cfg.Static.Host != "" {
		return claim.StaticHost == cfg.Static.Host
	}
	_, _, leaseID, err := staticLease(cfg)
	if err == nil && claim.LeaseID == leaseID {
		return true
	}
	return false
}
func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}
func removeLeaseClaim(leaseID string) { core.RemoveLeaseClaim(leaseID) }
func allocateClaimLeaseSlug(leaseID, requested string) (string, error) {
	return core.AllocateClaimLeaseSlug(leaseID, requested)
}
func resolveLeaseClaimForProvider(id, provider string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProvider(id, provider)
}
