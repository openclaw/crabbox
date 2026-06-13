package nvidiabrev

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	brevAcquirePollInterval = 2 * time.Second
	brevAcquirePollTimeout  = 12 * time.Minute
)

var normalizeBrevSlugPattern = regexp.MustCompile(`[^a-z0-9]+`)

type nvidiaBrevBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func NewNvidiaBrevBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	applyNvidiaBrevDefaults(&cfg)
	cfg.Provider = providerName
	return &nvidiaBrevBackend{spec: spec, cfg: cfg, rt: rt}
}

func (b *nvidiaBrevBackend) Spec() ProviderSpec { return b.spec }

func (b *nvidiaBrevBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	client, err := b.client()
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg := b.configForRun()
	leaseID := newLeaseID()
	existing, err := b.listServers(ctx, client, true)
	if err != nil {
		return LeaseTarget{}, err
	}
	slug := allocateBrevLeaseSlug(leaseID, req.RequestedSlug, existing)
	name := brevProviderName(leaseID, slug)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s name=%s gpu=%s keep=%v\n", providerName, leaseID, slug, name, cfg.NvidiaBrev.GPUName, req.Keep)

	if err := client.create(ctx, name); err != nil {
		return LeaseTarget{}, err
	}
	workspace, err := b.waitForWorkspaceReady(ctx, client, name)
	if err != nil {
		if !req.Keep {
			err = b.rollbackCreatedWorkspace(name, err)
		}
		return LeaseTarget{}, err
	}
	lease, err := b.prepareLease(ctx, client, cfg, workspace, leaseID, slug, req.Keep, true)
	if err != nil {
		if !req.Keep {
			err = b.rollbackCreatedWorkspace(workspaceIdentifier(workspace), err)
		}
		return LeaseTarget{}, err
	}
	if err := claimLeaseTargetForRepoConfig(leaseID, slug, cfg, lease.Server, lease.SSH, req.Repo.Root, req.Reclaim); err != nil {
		if !req.Keep {
			err = b.rollbackCreatedWorkspace(workspaceIdentifier(workspace), err)
		}
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s workspace=%s state=ready\n", leaseID, safeWorkspaceRef(workspace))
	return lease, nil
}

func (b *nvidiaBrevBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	client, err := b.client()
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg := b.configForRun()
	workspace, leaseID, slug, _, err := b.resolveWorkspace(ctx, client, req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	lease := LeaseTarget{Server: workspaceToServer(cfg, workspace, leaseID, slug, false), LeaseID: leaseID}
	if req.ReleaseOnly || req.StatusOnly {
		if req.ReadyProbe && brevWorkspaceReady(workspace) {
			target, targetErr := b.resolveSSHTarget(ctx, client, cfg, workspace)
			if targetErr != nil {
				return LeaseTarget{}, targetErr
			}
			lease.SSH = target
		}
		return lease, nil
	}
	target, err := b.resolveSSHTarget(ctx, client, cfg, workspace)
	if err != nil {
		return LeaseTarget{}, err
	}
	lease.SSH = target
	if req.Repo.Root != "" && isCrabboxBrevWorkspace(workspace) {
		if err := claimLeaseTargetForRepoConfig(leaseID, slug, cfg, lease.Server, lease.SSH, req.Repo.Root, req.Reclaim); err != nil {
			return LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *nvidiaBrevBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	client, err := b.client()
	if err != nil {
		return nil, err
	}
	return b.listServers(ctx, client, req.All)
}

func (b *nvidiaBrevBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	client, err := b.client()
	if err != nil {
		return err
	}
	workspace, leaseID, _, claim, err := b.resolveWorkspace(ctx, client, firstNonEmpty(req.Lease.LeaseID, req.Lease.Server.CloudID, req.Lease.Server.Name))
	if err != nil {
		return err
	}
	if claim.LeaseID == "" {
		return exit(2, "refusing to release nvidia-brev workspace %s without a local Crabbox claim", safeWorkspaceRef(workspace))
	}
	if claim.Provider != providerName {
		return exit(2, "lease=%s is claimed by provider=%s; refusing nvidia-brev release", claim.LeaseID, claim.Provider)
	}
	if !claimMatchesWorkspace(claim, workspace) {
		return exit(2, "lease=%s claim does not match nvidia-brev workspace %s", claim.LeaseID, safeWorkspaceRef(workspace))
	}
	action := b.releaseAction()
	if err := b.releaseWorkspace(ctx, client, workspace); err != nil {
		return err
	}
	if action == "stop" {
		if err := b.persistStoppedClaim(workspace, claim); err != nil {
			return err
		}
		return nil
	}
	removeLeaseClaim(leaseID)
	return nil
}

func (b *nvidiaBrevBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	cfg := b.configForRun()
	if req.IdleTimeout > 0 {
		cfg.IdleTimeout = req.IdleTimeout
	}
	server.Labels = touchDirectLeaseLabels(server.Labels, cfg, req.State)
	if strings.TrimSpace(req.State) != "" {
		server.Status = strings.TrimSpace(req.State)
	} else if state := strings.TrimSpace(server.Labels["state"]); state != "" {
		server.Status = state
	}
	if req.Lease.LeaseID != "" {
		claim, ok, err := resolveLeaseClaimForProvider(req.Lease.LeaseID)
		if err != nil {
			return server, err
		}
		if ok && claim.RepoRoot != "" {
			if err := claimLeaseTargetForRepoConfig(claim.LeaseID, claim.Slug, cfg, server, req.Lease.SSH, claim.RepoRoot, false); err != nil {
				return server, err
			}
		}
	}
	return server, nil
}

func (b *nvidiaBrevBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	client, err := b.client()
	if err != nil {
		return err
	}
	workspaces, err := client.list(ctx, true)
	if err != nil {
		return err
	}
	var errs []error
	for _, workspace := range workspaces {
		claim, claimed, err := resolveLeaseClaimForProviderCloudID(workspace.ID)
		if err != nil {
			return err
		}
		nameOwned := isCrabboxBrevWorkspace(workspace)
		if !claimed && !nameOwned {
			fmt.Fprintf(b.rt.Stderr, "skip provider=%s workspace=%s reason=not-crabbox-owned\n", providerName, safeWorkspaceRef(workspace))
			continue
		}
		leaseID, slug := brevLeaseIdentity(workspace, claim)
		eligible, reason := b.cleanupEligible(workspace, claim, claimed)
		if !eligible {
			fmt.Fprintf(b.rt.Stderr, "skip provider=%s lease=%s workspace=%s reason=%s\n", providerName, leaseID, safeWorkspaceRef(workspace), reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stderr, "would release provider=%s lease=%s slug=%s workspace=%s action=%s\n", providerName, leaseID, slug, safeWorkspaceRef(workspace), b.releaseAction())
			continue
		}
		if err := b.releaseWorkspace(ctx, client, workspace); err != nil {
			errs = append(errs, err)
			continue
		}
		if claimed {
			if b.releaseAction() == "stop" {
				if err := b.persistStoppedClaim(workspace, claim); err != nil {
					errs = append(errs, err)
				}
			} else {
				removeLeaseClaim(claim.LeaseID)
			}
		}
	}
	return errors.Join(errs...)
}

func (b *nvidiaBrevBackend) cleanupEligible(workspace brevWorkspace, claim LeaseClaim, claimed bool) (bool, string) {
	if !claimed {
		return false, "no-local-cleanup-claim"
	}
	if claim.Provider != providerName || !claimMatchesWorkspace(claim, workspace) {
		return false, "claim-mismatch"
	}
	if strings.EqualFold(strings.TrimSpace(claim.Labels["keep"]), "true") {
		return false, "keep"
	}
	if expiresAt := strings.TrimSpace(claim.Labels["expires_at"]); expiresAt != "" {
		if expires, ok := parseBrevClaimTime(expiresAt); ok && time.Now().After(expires) {
			return true, "expired-ttl"
		}
	}
	if claim.IdleTimeoutSeconds <= 0 || strings.TrimSpace(claim.LastUsedAt) == "" {
		return false, "active-claim"
	}
	lastUsed, ok := parseBrevClaimTime(claim.LastUsedAt)
	if !ok {
		return false, "active-claim"
	}
	if time.Since(lastUsed) <= time.Duration(claim.IdleTimeoutSeconds)*time.Second {
		return false, "active-claim"
	}
	return true, "expired"
}

func parseBrevClaimTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return time.Unix(seconds, 0).UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

func (b *nvidiaBrevBackend) persistStoppedClaim(workspace brevWorkspace, claim LeaseClaim) error {
	stopped := workspaceToServer(b.configForRun(), workspace, claim.LeaseID, claim.Slug, false)
	stopped.Status = "stopped"
	stopped.Labels["state"] = "stopped"
	if err := claimLeaseTargetForRepoConfig(claim.LeaseID, claim.Slug, b.configForRun(), stopped, SSHTarget{}, claim.RepoRoot, false); err != nil {
		return fmt.Errorf("persist stopped nvidia-brev lease claim: %w", err)
	}
	return nil
}

func (b *nvidiaBrevBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	client, err := b.client()
	if err != nil {
		return DoctorResult{}, err
	}
	if _, err := client.version(ctx); err != nil {
		return DoctorResult{}, exit(2, "nvidia-brev CLI check failed: %v", err)
	}
	workspaces, err := client.list(ctx, false)
	if err != nil {
		return DoctorResult{}, exit(1, "nvidia-brev auth/list check failed: %v", err)
	}
	return cliDoctorResult(providerName, len(workspaces), "unchecked"), nil
}

func (b *nvidiaBrevBackend) client() (*brevClient, error) {
	return newBrevClient(b.configForRun(), b.rt)
}

func (b *nvidiaBrevBackend) configForRun() Config {
	cfg := b.cfg
	applyNvidiaBrevDefaults(&cfg)
	return cfg
}

func applyNvidiaBrevDefaults(cfg *Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = targetLinux
	}
	if cfg.NvidiaBrev.CLI == "" {
		cfg.NvidiaBrev.CLI = "brev"
	}
	if cfg.NvidiaBrev.GPUName == "" {
		cfg.NvidiaBrev.GPUName = "A100"
	}
	if cfg.NvidiaBrev.Mode == "" {
		cfg.NvidiaBrev.Mode = "vm"
	}
	if cfg.NvidiaBrev.ReleaseAction == "" {
		cfg.NvidiaBrev.ReleaseAction = "delete"
	}
	if cfg.NvidiaBrev.Target == "" {
		cfg.NvidiaBrev.Target = "container"
	}
	if cfg.NvidiaBrev.WorkRoot == "" {
		cfg.NvidiaBrev.WorkRoot = "/tmp/crabbox"
	}
	if cfg.NvidiaBrev.User != "" {
		cfg.SSHUser = cfg.NvidiaBrev.User
	}
	if cfg.NvidiaBrev.WorkRoot != "" {
		cfg.WorkRoot = cfg.NvidiaBrev.WorkRoot
	}
	cfg.SSHPort = ""
	cfg.SSHFallbackPorts = nil
}

func (b *nvidiaBrevBackend) waitForWorkspaceReady(ctx context.Context, client *brevClient, name string) (brevWorkspace, error) {
	deadline := time.Now().Add(brevAcquirePollTimeout)
	for {
		workspaces, err := client.list(ctx, true)
		if err != nil {
			return brevWorkspace{}, err
		}
		workspace, found, err := findBrevWorkspace(workspaces, name)
		if err != nil {
			return brevWorkspace{}, err
		}
		if found && brevWorkspaceReady(workspace) {
			return workspace, nil
		}
		if time.Now().After(deadline) {
			if found {
				return brevWorkspace{}, exit(5, "timed out waiting for nvidia-brev workspace %s to become ready (status=%s build=%s shell=%s health=%s)", safeWorkspaceRef(workspace), workspace.Status, workspace.BuildStatus, workspace.ShellStatus, workspace.HealthStatus)
			}
			return brevWorkspace{}, exit(5, "timed out waiting for nvidia-brev workspace %q to appear", name)
		}
		timer := time.NewTimer(brevAcquirePollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return brevWorkspace{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (b *nvidiaBrevBackend) prepareLease(ctx context.Context, client *brevClient, cfg Config, workspace brevWorkspace, leaseID, slug string, keep, probeSSH bool) (LeaseTarget, error) {
	target, err := b.resolveSSHTarget(ctx, client, cfg, workspace)
	if err != nil {
		return LeaseTarget{}, err
	}
	server := workspaceToServer(cfg, workspace, leaseID, slug, keep)
	if probeSSH {
		if err := waitForSSH(ctx, &target, b.rt.Stderr); err != nil {
			return LeaseTarget{}, err
		}
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *nvidiaBrevBackend) resolveSSHTarget(ctx context.Context, client *brevClient, cfg Config, workspace brevWorkspace) (SSHTarget, error) {
	if strings.TrimSpace(cfg.NvidiaBrev.Org) != "" {
		return SSHTarget{}, exit(2, "nvidiaBrev.org scopes read-only Brev inventory only; brev refresh does not support --org, so SSH lifecycle resolution is unsafe. Run `brev set` for the desired active org or remove nvidiaBrev.org before using nvidia-brev SSH lifecycle commands")
	}
	if err := client.refresh(ctx); err != nil {
		return SSHTarget{}, err
	}
	path := defaultBrevSSHConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return SSHTarget{}, exit(2, "read nvidia-brev SSH config %s: %v", path, err)
	}
	alias := brevSSHConfigAlias(workspace.Name, cfg.NvidiaBrev.Target)
	target, err := selectBrevSSHTarget(cfg, string(data), alias)
	if err != nil {
		return SSHTarget{}, err
	}
	return target, nil
}

func (b *nvidiaBrevBackend) rollbackCreatedWorkspace(idOrName string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := b.client()
	if err != nil {
		return errors.Join(cause, fmt.Errorf("nvidia-brev rollback client unavailable: %w", err))
	}
	if err := client.delete(ctx, idOrName); err != nil {
		return errors.Join(cause, fmt.Errorf("nvidia-brev rollback cleanup failed for workspace %s: %w", idOrName, err))
	}
	return cause
}

func (b *nvidiaBrevBackend) resolveWorkspace(ctx context.Context, client *brevClient, id string) (brevWorkspace, string, string, LeaseClaim, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return brevWorkspace{}, "", "", LeaseClaim{}, exit(2, "nvidia-brev resolve requires lease id, slug, workspace id, or workspace name")
	}
	claim, claimed, err := resolveLeaseClaimForProvider(id)
	if err != nil {
		return brevWorkspace{}, "", "", LeaseClaim{}, err
	}
	if !claimed {
		if byCloudID, ok, err := resolveLeaseClaimForProviderCloudID(id); err != nil {
			return brevWorkspace{}, "", "", LeaseClaim{}, err
		} else if ok {
			claim, claimed = byCloudID, true
		}
	}
	workspaces, err := client.list(ctx, true)
	if err != nil {
		return brevWorkspace{}, "", "", LeaseClaim{}, err
	}
	if claimed && claim.CloudID != "" {
		if workspace, found, err := findBrevWorkspace(workspaces, claim.CloudID); err != nil {
			return brevWorkspace{}, "", "", LeaseClaim{}, err
		} else if found {
			leaseID, slug := brevLeaseIdentity(workspace, claim)
			return workspace, leaseID, slug, claim, nil
		}
		return brevWorkspace{}, "", "", LeaseClaim{}, exit(4, "nvidia-brev workspace for lease=%s not found", claim.LeaseID)
	}
	workspace, found, err := findBrevWorkspace(workspaces, id)
	if err != nil {
		return brevWorkspace{}, "", "", LeaseClaim{}, err
	}
	if !found {
		return brevWorkspace{}, "", "", LeaseClaim{}, exit(4, "nvidia-brev workspace not found: %s", id)
	}
	if !claimed {
		claim, claimed, err = resolveLeaseClaimForProviderCloudID(workspace.ID)
		if err != nil {
			return brevWorkspace{}, "", "", LeaseClaim{}, err
		}
	}
	leaseID, slug := brevLeaseIdentity(workspace, claim)
	return workspace, leaseID, slug, claim, nil
}

func (b *nvidiaBrevBackend) listServers(ctx context.Context, client *brevClient, all bool) ([]LeaseView, error) {
	workspaces, err := client.list(ctx, all)
	if err != nil {
		return nil, err
	}
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	claimsByCloudID := map[string]LeaseClaim{}
	for _, claim := range claims {
		if claim.Provider == providerName && claim.CloudID != "" {
			claimsByCloudID[claim.CloudID] = claim
		}
	}
	servers := make([]LeaseView, 0, len(workspaces))
	for _, workspace := range workspaces {
		claim := claimsByCloudID[workspace.ID]
		if !all && claim.LeaseID == "" && !isCrabboxBrevWorkspace(workspace) {
			continue
		}
		leaseID, slug := brevLeaseIdentity(workspace, claim)
		servers = append(servers, workspaceToServer(b.configForRun(), workspace, leaseID, slug, false))
	}
	return servers, nil
}

func (b *nvidiaBrevBackend) releaseWorkspace(ctx context.Context, client *brevClient, workspace brevWorkspace) error {
	id := workspaceIdentifier(workspace)
	if id == "" {
		return exit(2, "nvidia-brev release requires workspace id or name")
	}
	switch b.releaseAction() {
	case "stop":
		if err := client.stop(ctx, id); err != nil {
			return err
		}
	default:
		if err := client.delete(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (b *nvidiaBrevBackend) releaseAction() string {
	action := strings.ToLower(strings.TrimSpace(b.configForRun().NvidiaBrev.ReleaseAction))
	if action == "stop" {
		return "stop"
	}
	return "delete"
}

func findBrevWorkspace(workspaces []brevWorkspace, idOrName string) (brevWorkspace, bool, error) {
	idOrName = strings.TrimSpace(idOrName)
	if idOrName == "" {
		return brevWorkspace{}, false, nil
	}
	var match brevWorkspace
	for _, workspace := range workspaces {
		if workspace.ID != idOrName && workspace.Name != idOrName {
			continue
		}
		if match.ID != "" || match.Name != "" {
			return brevWorkspace{}, false, exit(2, "nvidia-brev workspace %q is ambiguous", idOrName)
		}
		match = workspace
	}
	return match, match.ID != "" || match.Name != "", nil
}

func workspaceToServer(cfg Config, workspace brevWorkspace, leaseID, slug string, keep bool) Server {
	labels := directLeaseLabels(cfg, leaseID, slug, providerName, "", keep)
	labels["state"] = normalizeBrevState(workspace)
	labels["brev_workspace_id"] = workspace.ID
	labels["brev_workspace_name"] = workspace.Name
	labels["brev_status"] = workspace.Status
	labels["brev_build_status"] = workspace.BuildStatus
	labels["brev_shell_status"] = workspace.ShellStatus
	labels["brev_health_status"] = workspace.HealthStatus
	labels["brev_target"] = strings.TrimSpace(cfg.NvidiaBrev.Target)
	if workspace.GPU != "" {
		labels["gpu"] = workspace.GPU
	}
	if workspace.InstanceKind != "" {
		labels["instance_kind"] = workspace.InstanceKind
	}
	server := Server{
		CloudID:  workspace.ID,
		Provider: providerName,
		Name:     workspace.Name,
		Status:   labels["state"],
		Labels:   labels,
	}
	server.ServerType.Name = firstNonEmpty(workspace.InstanceType, workspace.WorkspaceClass, cfg.NvidiaBrev.Type, cfg.NvidiaBrev.GPUName)
	return server
}

func normalizeBrevState(workspace brevWorkspace) string {
	if brevWorkspaceReady(workspace) {
		return "ready"
	}
	status := strings.ToLower(strings.TrimSpace(workspace.Status))
	switch status {
	case "running", "starting", "building", "queued", "pending":
		return "booting"
	case "stopped", "deleted", "failed", "error":
		return status
	case "":
		return "unknown"
	default:
		return status
	}
}

func brevWorkspaceReady(workspace brevWorkspace) bool {
	status := strings.ToLower(strings.TrimSpace(workspace.Status))
	build := strings.ToLower(strings.TrimSpace(workspace.BuildStatus))
	shell := strings.ToLower(strings.TrimSpace(workspace.ShellStatus))
	health := strings.ToLower(strings.TrimSpace(workspace.HealthStatus))
	return oneOf(status, "running", "ready") &&
		(build == "" || oneOf(build, "ready", "complete", "completed", "success", "succeeded")) &&
		(shell == "" || oneOf(shell, "ready", "running", "connected")) &&
		(health == "" || oneOf(health, "healthy", "ready", "ok"))
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func workspaceIdentifier(workspace brevWorkspace) string {
	return firstNonEmpty(workspace.ID, workspace.Name)
}

func safeWorkspaceRef(workspace brevWorkspace) string {
	if workspace.ID != "" {
		return workspace.ID
	}
	return workspace.Name
}

func claimMatchesWorkspace(claim LeaseClaim, workspace brevWorkspace) bool {
	if claim.CloudID != "" {
		return claim.CloudID == workspace.ID
	}
	if claim.Labels != nil {
		if id := strings.TrimSpace(claim.Labels["brev_workspace_id"]); id != "" {
			return id == workspace.ID
		}
		if name := strings.TrimSpace(claim.Labels["brev_workspace_name"]); name != "" {
			return name == workspace.Name
		}
	}
	return false
}

func brevLeaseIdentity(workspace brevWorkspace, claim LeaseClaim) (string, string) {
	if claim.LeaseID != "" {
		return claim.LeaseID, claim.Slug
	}
	leaseID, slug := parseBrevProviderName(workspace.Name)
	if leaseID != "" {
		return leaseID, slug
	}
	if workspace.ID != "" {
		return "nbrev_" + normalizeBrevSlug(workspace.ID), normalizeBrevSlug(workspace.Name)
	}
	return "", normalizeBrevSlug(workspace.Name)
}

func isCrabboxBrevWorkspace(workspace brevWorkspace) bool {
	leaseID, _ := parseBrevProviderName(workspace.Name)
	return leaseID != ""
}

func brevProviderName(leaseID, slug string) string {
	slug = normalizeBrevSlug(slug)
	if slug == "" {
		slug = "lease"
	}
	return fmt.Sprintf("crabbox-%s-%s", slug, strings.TrimPrefix(leaseID, "cbx_"))
}

func parseBrevProviderName(name string) (string, string) {
	name = strings.TrimSpace(name)
	if !strings.HasPrefix(name, "crabbox-") {
		return "", ""
	}
	rest := strings.TrimPrefix(name, "crabbox-")
	lastDash := strings.LastIndex(rest, "-")
	if lastDash < 0 || lastDash == len(rest)-1 {
		return "", ""
	}
	slug := rest[:lastDash]
	id := rest[lastDash+1:]
	if len(id) < 8 {
		return "", ""
	}
	return "cbx_" + id, slug
}

func allocateBrevLeaseSlug(leaseID, requested string, servers []LeaseView) string {
	base := normalizeBrevSlug(requested)
	if base == "" {
		base = normalizeBrevSlug(strings.TrimPrefix(leaseID, "cbx_"))
	}
	if base == "" {
		base = "lease"
	}
	slug := base
	for attempt := 0; attempt < 20; attempt++ {
		if !brevSlugInUse(slug, servers) {
			return slug
		}
		slug = fmt.Sprintf("%s-%02d", base, attempt+1)
	}
	return fmt.Sprintf("%s-%s", base, strings.TrimPrefix(leaseID, "cbx_"))
}

func brevSlugInUse(slug string, servers []LeaseView) bool {
	for _, server := range servers {
		if normalizeBrevSlug(server.Labels["slug"]) == slug {
			return true
		}
	}
	return false
}

func normalizeBrevSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = normalizeBrevSlugPattern.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if len(value) > 48 {
		value = strings.Trim(value[:48], "-")
	}
	return value
}
