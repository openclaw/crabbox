package coder

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	coderReleaseActionLabel  = "coder_release_action"
	coderReleaseActionStop   = "stop"
	coderReleaseActionDelete = "delete"
)

type coderLeaseBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func NewCoderLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) (Backend, error) {
	if strings.TrimSpace(cfg.Coder.CLIPath) == "" {
		cfg.Coder.CLIPath = "coder"
	}
	if strings.TrimSpace(cfg.Coder.WorkspacePrefix) == "" {
		cfg.Coder.WorkspacePrefix = "crabbox-"
	}
	if strings.TrimSpace(cfg.Coder.WorkRoot) == "" {
		cfg.Coder.WorkRoot = "/home/coder/crabbox"
	}
	if strings.TrimSpace(cfg.Coder.Wait) == "" {
		cfg.Coder.Wait = "yes"
	}
	cfg.Provider = coderProvider
	cfg.TargetOS = targetLinux
	cfg.SSHUser = "coder"
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = nil
	cfg.Network = networkPublic
	cfg.WorkRoot = coderWorkRoot(cfg)
	if err := validateCoderConfig(cfg); err != nil {
		return nil, err
	}
	return &coderLeaseBackend{spec: spec, cfg: cfg, rt: rt}, nil
}

func (b *coderLeaseBackend) Spec() ProviderSpec { return b.spec }

func (b *coderLeaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	if strings.TrimSpace(b.cfg.Coder.Template) == "" {
		return LeaseTarget{}, exit(2, "provider=coder requires --coder-template or coder.template to create a workspace")
	}
	client, err := newCoderClient(b.cfg, b.rt)
	if err != nil {
		return LeaseTarget{}, err
	}
	existing, err := client.list(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	leaseID := newLeaseID()
	slug, err := allocateDirectLeaseSlug(leaseID, req.RequestedSlug, coderWorkspacesToServers(existing, b.cfg))
	if err != nil {
		return LeaseTarget{}, err
	}
	slug, workspaceName, err := coderUniqueWorkspaceName(existing, b.cfg.Coder.WorkspacePrefix, slug, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if err := claimLeaseForRepoProvider(leaseID, slug, coderProvider, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
		return LeaseTarget{}, err
	}
	_ = updateLeaseClaimEndpoint(leaseID, coderWorkspaceToServer(coderWorkspace{Name: workspaceName}, b.cfg, leaseID, slug, req.Keep), SSHTarget{})
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=coder lease=%s slug=%s workspace=%s template=%s keep=%v\n", leaseID, slug, workspaceName, b.cfg.Coder.Template, req.Keep)
	if err := client.create(ctx, b.cfg, workspaceName); err != nil {
		if !req.Keep {
			err = b.rollbackCreateError(workspaceName, leaseID, client, err)
		}
		return LeaseTarget{}, err
	}
	workspaces, err := client.list(ctx)
	if err != nil {
		if !req.Keep {
			err = b.rollbackCreatedWorkspace(workspaceName, leaseID, client, err)
		}
		return LeaseTarget{}, err
	}
	workspace, ok := findCoderWorkspace(workspaces, workspaceName)
	if !ok {
		if !req.Keep {
			err = b.rollbackCreatedWorkspace(workspaceName, leaseID, client, exit(5, "coder workspace %s was created but not found in coder list", workspaceName))
			return LeaseTarget{}, err
		}
		return LeaseTarget{}, exit(5, "coder workspace %s was created but not found in coder list", workspaceName)
	}
	server := coderWorkspaceToServer(workspace, b.cfg, leaseID, slug, req.Keep)
	target := coderSSHTarget(b.cfg, workspaceName, workspace.ID)
	if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "coder ssh", bootstrapWaitTimeout(b.cfg)); err != nil {
		if !req.Keep {
			err = b.rollbackCreatedWorkspace(workspaceName, leaseID, client, err)
		}
		return LeaseTarget{}, err
	}
	server.Status = "ready"
	server.Labels["state"] = "ready"
	_ = updateLeaseClaimEndpoint(leaseID, server, target)
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *coderLeaseBackend) rollbackCreateError(name, leaseID string, client *coderClient, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workspaces, err := client.list(cleanupCtx)
	if err != nil {
		return cause
	}
	if _, ok := findCoderWorkspace(workspaces, name); ok {
		return b.rollbackCreatedWorkspace(name, leaseID, client, cause)
	}
	removeLeaseClaim(leaseID)
	return cause
}

func (b *coderLeaseBackend) rollbackCreatedWorkspace(name, leaseID string, client *coderClient, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.releaseWorkspace(cleanupCtx, client, name); err != nil {
		return exit(coderExitCode(cause), "%v; coder rollback %s failed for workspace %s; manual cleanup: %s: %v", cause, coderReleaseActionFromConfig(b.cfg), name, coderManualCleanupCommand(b.cfg, name), err)
	}
	removeLeaseClaim(leaseID)
	return cause
}

func (b *coderLeaseBackend) releaseWorkspace(ctx context.Context, client *coderClient, name string) error {
	if b.cfg.Coder.DeleteOnRelease {
		return client.delete(ctx, name)
	}
	return client.stop(ctx, name)
}

func coderExitCode(err error) int {
	var exitErr ExitError
	if errors.As(err, &exitErr) && exitErr.Code != 0 {
		return exitErr.Code
	}
	return 1
}

func (b *coderLeaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	client, err := newCoderClient(b.cfg, b.rt)
	if err != nil {
		return LeaseTarget{}, err
	}
	useListAll, err := b.resolveNeedsListAll(req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	claims, err := listCoderClaimsByWorkspace(b.cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	if coderClaimsNeedListAllForIdentifier(claims, req.ID) {
		useListAll = true
	}
	listFn := client.list
	if useListAll {
		listFn = client.listAll
	}
	workspaces, err := listFn(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	nameCounts := coderWorkspaceNameCounts(workspaces)
	workspace, leaseID, slug, err := b.resolveWorkspace(req.ID, workspaces, claims, nameCounts)
	if err != nil {
		if req.ReleaseOnly {
			if claim, ok := coderClaimForIdentifier(claims, req.ID); ok {
				return coderStaleClaimLeaseTarget(b.cfg, claim), nil
			}
		}
		return LeaseTarget{}, err
	}
	keep, err := b.resolveKeepLabel(leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	claim, hasClaim := coderClaimForWorkspaceInInventory(claims, workspace, nameCounts)
	server := coderWorkspaceToServerWithClaim(workspace, b.cfg, leaseID, slug, keep, claim, hasClaim)
	workspaceRef := coderWorkspaceCommandName(workspace)
	if req.ReleaseOnly || req.StatusOnly {
		lease := LeaseTarget{Server: server, LeaseID: leaseID}
		if coderWorkspaceReady(workspace) {
			lease.SSH = coderSSHTarget(b.cfg, workspaceRef, workspace.ID)
		}
		return lease, nil
	}
	if !coderWorkspaceReady(workspace) {
		if err := client.start(ctx, workspaceRef); err != nil {
			return LeaseTarget{}, err
		}
		refreshList := client.list
		if useListAll {
			refreshList = client.listAll
		}
		workspaces, err = refreshList(ctx)
		if err != nil {
			return LeaseTarget{}, err
		}
		if refreshed, found := findCoderWorkspace(workspaces, workspaceRef); found {
			workspace = refreshed
			nameCounts = coderWorkspaceNameCounts(workspaces)
			claim, hasClaim = coderClaimForWorkspaceInInventory(claims, workspace, nameCounts)
			server = coderWorkspaceToServerWithClaim(workspace, b.cfg, leaseID, slug, keep, claim, hasClaim)
			workspaceRef = coderWorkspaceCommandName(workspace)
		}
	}
	target := coderSSHTarget(b.cfg, workspaceRef, workspace.ID)
	if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "coder ssh", bootstrapWaitTimeout(b.cfg)); err != nil {
		return LeaseTarget{}, err
	}
	if req.Repo.Root != "" && leaseID != "" {
		if err := claimLeaseForRepoProvider(leaseID, slug, coderProvider, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
			return LeaseTarget{}, err
		}
		_ = updateLeaseClaimEndpoint(leaseID, server, target)
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *coderLeaseBackend) resolveNeedsListAll(identifier string) (bool, error) {
	identifier = strings.TrimSpace(identifier)
	if strings.Contains(identifier, "/") {
		return true, nil
	}
	claim, ok, err := resolveLeaseClaimForProvider(identifier, coderProvider)
	if err != nil || !ok {
		return false, err
	}
	return strings.Contains(coderClaimWorkspaceRef(claim), "/"), nil
}

func (b *coderLeaseBackend) resolveKeepLabel(leaseID string) (bool, error) {
	if leaseID == "" {
		return false, nil
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, coderProvider)
	if err != nil || !ok {
		return false, err
	}
	return coderClaimKeep(claim), nil
}

func (b *coderLeaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	client, err := newCoderClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	claims, err := listCoderClaimsByWorkspace(b.cfg)
	if err != nil {
		return nil, err
	}
	listFn := client.list
	if coderClaimsNeedListAll(claims) {
		listFn = client.listAll
	}
	workspaces, err := listFn(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(workspaces))
	nameCounts := coderWorkspaceNameCounts(workspaces)
	for _, workspace := range workspaces {
		leaseID, slug, owned := coderWorkspaceLeaseMetadata(workspace, b.cfg)
		claim, hasClaim := coderClaimForWorkspaceInInventory(claims, workspace, nameCounts)
		if !owned && !hasClaim && !req.All {
			continue
		}
		if hasClaim {
			if leaseID == "" {
				leaseID = claim.LeaseID
			}
			if slug == "" {
				slug = claim.Slug
			}
		}
		servers = append(servers, coderWorkspaceToServerWithClaim(workspace, b.cfg, leaseID, slug, hasClaim && coderClaimKeep(claim), claim, hasClaim))
	}
	return servers, nil
}

func (b *coderLeaseBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	client, err := newCoderClient(b.cfg, b.rt)
	if err != nil {
		return DoctorResult{}, err
	}
	checks := []DoctorCheck{}
	if err := client.version(ctx); err != nil {
		checks = append(checks, DoctorCheck{Status: "fail", Check: "cli", Message: err.Error(), Details: map[string]string{"mutation": "false"}})
		return DoctorResult{Provider: coderProvider, Status: "fail", Message: "cli=missing auth=unchecked inventory=unchecked mutation=false", Checks: checks}, err
	}
	checks = append(checks, DoctorCheck{Status: "pass", Check: "cli", Message: "coder CLI available", Details: map[string]string{"mutation": "false"}})
	if err := client.whoami(ctx); err != nil {
		authStatus := "failed"
		classification := "auth_failed"
		if coderWhoamiMissingLogin(err.Error()) {
			authStatus = "missing_login"
			classification = "missing_login"
		}
		checks = append(checks, DoctorCheck{Status: "fail", Check: "auth", Message: err.Error(), Details: map[string]string{"mutation": "false", "classification": classification}})
		return DoctorResult{Provider: coderProvider, Status: "fail", Message: fmt.Sprintf("cli=ready auth=%s inventory=unchecked mutation=false", authStatus), Checks: checks}, err
	}
	checks = append(checks, DoctorCheck{Status: "pass", Check: "auth", Message: "coder login ready", Details: map[string]string{"mutation": "false"}})
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		checks = append(checks, DoctorCheck{Status: "fail", Check: "inventory", Message: err.Error(), Details: map[string]string{"mutation": "false"}})
		return DoctorResult{Provider: coderProvider, Status: "fail", Message: "cli=ready auth=ready inventory=failed mutation=false", Checks: checks}, err
	}
	checks = append(checks, DoctorCheck{Status: "pass", Check: "inventory", Message: fmt.Sprintf("listed %d Crabbox-owned Coder workspaces", len(servers)), Details: map[string]string{"mutation": "false"}})
	return DoctorResult{Provider: coderProvider, Status: "pass", Message: fmt.Sprintf("cli=ready auth=ready inventory=ready api=list mutation=false leases=%d runtime=unchecked", len(servers)), Checks: checks}, nil
}

func (b *coderLeaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	client, err := newCoderClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(req.Lease.Server.Labels["coder_workspace_ref"])
	if name == "" {
		name = strings.TrimSpace(req.Lease.Server.Labels["coder_workspace"])
	}
	if name == "" {
		name = strings.TrimSpace(req.Lease.Server.Name)
	}
	if name == "" {
		name = strings.TrimSpace(req.Lease.Server.CloudID)
	}
	if name == "" {
		return exit(2, "coder release requires a workspace name")
	}
	err = b.releaseWorkspace(ctx, client, name)
	if err != nil {
		if coderWorkspaceMissingError(err) && req.Lease.LeaseID != "" {
			removeLeaseClaim(req.Lease.LeaseID)
			return nil
		}
		return err
	}
	removeLeaseClaim(req.Lease.LeaseID)
	return nil
}

func (b *coderLeaseBackend) ReleaseLeaseMessage(lease LeaseTarget) string {
	action := "stopped"
	if b.cfg.Coder.DeleteOnRelease {
		action = "deleted"
	}
	return fmt.Sprintf("%s coder workspace lease=%s workspace=%s", action, lease.LeaseID, lease.Server.Name)
}

func (b *coderLeaseBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = touchDirectLeaseLabels(server.Labels, b.cfg, req.State, time.Now().UTC())
	return server, nil
}

func (b *coderLeaseBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	client, err := newCoderClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claims, err := listCoderClaimsByWorkspace(b.cfg)
	if err != nil {
		return err
	}
	listFn := client.list
	if coderClaimsNeedListAll(claims) {
		listFn = client.listAll
	}
	workspaces, err := listFn(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	nameCounts := coderWorkspaceNameCounts(workspaces)
	for _, workspace := range workspaces {
		leaseID, slug, owned := coderWorkspaceLeaseMetadata(workspace, b.cfg)
		claim, hasClaim := coderClaimForWorkspaceInInventory(claims, workspace, nameCounts)
		if !owned && !hasClaim {
			continue
		}
		if leaseID == "" && hasClaim {
			leaseID = claim.LeaseID
		}
		if slug == "" && hasClaim {
			slug = claim.Slug
		}
		server := coderWorkspaceToServerWithClaim(workspace, b.cfg, leaseID, slug, false, claim, hasClaim)
		shouldAct, reason := shouldCleanupCoder(server, claim, hasClaim, now)
		if !shouldAct {
			fmt.Fprintf(b.rt.Stderr, "skip coder workspace=%s reason=%s\n", workspace.Name, reason)
			continue
		}
		action := coderCleanupReleaseAction(claim, hasClaim)
		fmt.Fprintf(b.rt.Stdout, "coder cleanup %s workspace=%s lease=%s reason=%s dry_run=%t\n", action, workspace.Name, blank(leaseID, "-"), reason, req.DryRun)
		if req.DryRun {
			continue
		}
		if action == coderReleaseActionDelete {
			if err := client.delete(ctx, coderWorkspaceCommandName(workspace)); err != nil {
				return err
			}
		} else if err := client.stop(ctx, coderWorkspaceCommandName(workspace)); err != nil {
			return err
		}
		if leaseID != "" {
			removeLeaseClaim(leaseID)
		}
	}
	return nil
}

func listCoderClaimsByWorkspace(cfg Config) (map[string]LeaseClaim, error) {
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	out := map[string]LeaseClaim{}
	for _, claim := range claims {
		if claim.Provider != coderProvider {
			continue
		}
		name := coderClaimWorkspaceRef(claim)
		if name == "" {
			name, err = coderClaimWorkspaceName(cfg, claim)
			if err != nil {
				continue
			}
		}
		if name != "" {
			out[coderClaimKey(name)] = claim
		}
	}
	return out, nil
}

func coderClaimForWorkspace(claims map[string]LeaseClaim, workspace coderWorkspace) (LeaseClaim, bool) {
	return coderClaimForWorkspaceInInventory(claims, workspace, nil)
}

func coderClaimForWorkspaceInInventory(claims map[string]LeaseClaim, workspace coderWorkspace, nameCounts map[string]int) (LeaseClaim, bool) {
	if claim, ok := claims[coderClaimKey(coderWorkspaceCommandName(workspace))]; ok {
		return claim, true
	}
	if strings.TrimSpace(workspace.Owner) != "" {
		if nameCounts == nil || nameCounts[coderClaimKey(workspace.Name)] != 1 {
			return LeaseClaim{}, false
		}
	}
	if claim, ok := claims[coderClaimKey(workspace.Name)]; ok {
		return claim, true
	}
	return LeaseClaim{}, false
}

func coderClaimKey(ref string) string {
	return normalizeCoderWorkspaceIdentifier(strings.TrimSpace(ref))
}

func coderClaimsNeedListAll(claims map[string]LeaseClaim) bool {
	for key := range claims {
		if strings.Contains(key, "/") {
			return true
		}
	}
	return false
}

func coderClaimsNeedListAllForIdentifier(claims map[string]LeaseClaim, identifier string) bool {
	identifier = strings.TrimSpace(identifier)
	normalized := normalizeCoderWorkspaceIdentifier(identifier)
	normalizedSlug := normalizeLeaseSlug(identifier)
	for _, claim := range claims {
		ref := coderClaimWorkspaceRef(claim)
		if !strings.Contains(ref, "/") {
			continue
		}
		if normalizeCoderWorkspaceIdentifier(ref) == normalized || normalizeCoderWorkspaceIdentifier(coderWorkspaceNameFromRef(ref)) == normalized {
			return true
		}
		if claim.LeaseID == identifier {
			return true
		}
		if normalizedSlug != "" && normalizeLeaseSlug(claim.Slug) == normalizedSlug {
			return true
		}
	}
	return false
}

func coderWorkspaceNameCounts(workspaces []coderWorkspace) map[string]int {
	counts := map[string]int{}
	for _, workspace := range workspaces {
		counts[coderClaimKey(workspace.Name)]++
	}
	return counts
}

func shouldCleanupCoder(server Server, claim LeaseClaim, hasClaim bool, now time.Time) (bool, string) {
	if strings.EqualFold(server.Labels["keep"], "true") || (hasClaim && coderClaimKeep(claim)) {
		return false, "keep=true"
	}
	if hasClaim {
		lastUsed, err := time.Parse(time.RFC3339, strings.TrimSpace(claim.LastUsedAt))
		if err != nil || lastUsed.IsZero() {
			return false, "claim active"
		}
		idle := time.Duration(claim.IdleTimeoutSeconds) * time.Second
		if idle <= 0 {
			return false, "claim active"
		}
		if now.After(lastUsed.Add(idle).Add(12 * time.Hour)) {
			return true, "claim expired"
		}
		return false, "claim active"
	}
	return false, "missing claim"
}

func coderWorkspaceHasCrabboxLabel(workspace coderWorkspace) bool {
	if strings.EqualFold(strings.TrimSpace(workspace.Labels["crabbox"]), "true") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(workspace.Labels["created_by"]), "crabbox")
}

func (b *coderLeaseBackend) resolveWorkspace(identifier string, workspaces []coderWorkspace, claims map[string]LeaseClaim, nameCounts map[string]int) (coderWorkspace, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return coderWorkspace{}, "", "", exit(2, "coder resolve requires a lease id, slug, workspace, or owner/workspace")
	}
	if claim, ok := coderClaimByLeaseID(claims, identifier); ok {
		if workspace, found, err := b.resolveClaimWorkspace(claim, workspaces, nameCounts); err != nil {
			return coderWorkspace{}, "", "", err
		} else if found {
			return workspace, claim.LeaseID, claim.Slug, nil
		}
	}
	if claim, ok, err := resolveLeaseClaimForProvider(identifier, coderProvider); err != nil {
		return coderWorkspace{}, "", "", err
	} else if ok {
		if workspace, found, err := b.resolveClaimWorkspace(claim, workspaces, nameCounts); err != nil {
			return coderWorkspace{}, "", "", err
		} else if found {
			return workspace, claim.LeaseID, claim.Slug, nil
		}
	}
	normalized := normalizeCoderWorkspaceIdentifier(identifier)
	normalizedSlug := normalizeLeaseSlug(identifier)
	exactMatches := []coderWorkspace{}
	for _, workspace := range workspaces {
		if normalizeCoderWorkspaceIdentifier(workspace.Name) == normalized || normalizeCoderWorkspaceIdentifier(coderOwnerWorkspace(workspace)) == normalized {
			exactMatches = append(exactMatches, workspace)
		}
	}
	if len(exactMatches) > 1 {
		return coderWorkspace{}, "", "", exit(5, "coder workspace %q is ambiguous", identifier)
	}
	if len(exactMatches) == 1 {
		workspace := exactMatches[0]
		if claim, ok := coderClaimForWorkspaceInInventory(claims, workspace, nameCounts); ok {
			return workspace, claim.LeaseID, claim.Slug, nil
		}
		leaseID, slug, _ := coderWorkspaceLeaseMetadata(workspace, b.cfg)
		return workspace, leaseID, slug, nil
	}
	matches := []coderWorkspace{}
	for _, workspace := range workspaces {
		leaseID, slug, owned := coderWorkspaceLeaseMetadata(workspace, b.cfg)
		if owned && leaseID != "" && leaseID == identifier {
			matches = append(matches, workspace)
			continue
		}
		if owned && normalizedSlug != "" && normalizeLeaseSlug(slug) == normalizedSlug {
			matches = append(matches, workspace)
		}
	}
	if len(matches) == 0 {
		return coderWorkspace{}, "", "", exit(5, "coder workspace %q not found", identifier)
	}
	if len(matches) > 1 {
		return coderWorkspace{}, "", "", exit(5, "coder workspace %q is ambiguous", identifier)
	}
	if claim, ok := coderClaimForWorkspaceInInventory(claims, matches[0], nameCounts); ok {
		return matches[0], claim.LeaseID, claim.Slug, nil
	}
	leaseID, slug, _ := coderWorkspaceLeaseMetadata(matches[0], b.cfg)
	return matches[0], leaseID, slug, nil
}

func (b *coderLeaseBackend) resolveClaimWorkspace(claim LeaseClaim, workspaces []coderWorkspace, nameCounts map[string]int) (coderWorkspace, bool, error) {
	name := coderClaimWorkspaceRef(claim)
	if name == "" {
		var err error
		name, err = coderClaimWorkspaceName(b.cfg, claim)
		if err != nil {
			return coderWorkspace{}, false, err
		}
	}
	if !strings.Contains(name, "/") {
		counts := nameCounts
		if counts == nil {
			counts = coderWorkspaceNameCounts(workspaces)
		}
		if counts[coderClaimKey(name)] > 1 {
			return coderWorkspace{}, false, exit(5, "coder workspace %q is ambiguous", name)
		}
	}
	workspace, found := findCoderWorkspace(workspaces, name)
	return workspace, found, nil
}

func coderClaimByLeaseID(claims map[string]LeaseClaim, leaseID string) (LeaseClaim, bool) {
	for _, claim := range claims {
		if claim.LeaseID == leaseID {
			return claim, true
		}
	}
	return LeaseClaim{}, false
}

func coderClaimForIdentifier(claims map[string]LeaseClaim, identifier string) (LeaseClaim, bool) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return LeaseClaim{}, false
	}
	if claim, ok := coderClaimByLeaseID(claims, identifier); ok {
		return claim, true
	}
	normalized := normalizeCoderWorkspaceIdentifier(identifier)
	normalizedSlug := normalizeLeaseSlug(identifier)
	for _, claim := range claims {
		if normalizeCoderWorkspaceIdentifier(coderClaimWorkspaceRef(claim)) == normalized {
			return claim, true
		}
		if normalizedSlug != "" && normalizeLeaseSlug(claim.Slug) == normalizedSlug {
			return claim, true
		}
	}
	return LeaseClaim{}, false
}

func coderStaleClaimLeaseTarget(cfg Config, claim LeaseClaim) LeaseTarget {
	name := coderClaimWorkspaceRef(claim)
	if name == "" {
		generated, err := coderWorkspaceName(cfg.Coder.WorkspacePrefix, claim.Slug, claim.LeaseID)
		if err == nil {
			name = generated
		}
	}
	labels := map[string]string{}
	for k, v := range claim.Labels {
		labels[k] = v
	}
	if name != "" {
		labels["coder_workspace_ref"] = name
		labels["coder_workspace"] = coderWorkspaceNameFromRef(name)
	}
	if claim.Slug != "" {
		labels["slug"] = claim.Slug
	}
	if claim.LeaseID != "" {
		labels["lease"] = claim.LeaseID
	}
	return LeaseTarget{
		Server:  Server{CloudID: name, Provider: coderProvider, Name: coderWorkspaceNameFromRef(name), Status: "missing", Labels: labels},
		LeaseID: claim.LeaseID,
	}
}

func coderClaimWorkspaceRef(claim LeaseClaim) string {
	name := strings.TrimSpace(claim.Labels["coder_workspace_ref"])
	if name == "" {
		name = strings.TrimSpace(claim.Labels["coder_workspace"])
	}
	return name
}

func coderClaimWorkspaceName(cfg Config, claim LeaseClaim) (string, error) {
	return coderWorkspaceName(cfg.Coder.WorkspacePrefix, coderCollisionSlug(claim.Slug, claim.LeaseID), claim.LeaseID)
}

func coderClaimKeep(claim LeaseClaim) bool {
	return strings.EqualFold(claim.Labels["keep"], "true")
}

func coderReleaseActionFromConfig(cfg Config) string {
	if cfg.Coder.DeleteOnRelease {
		return coderReleaseActionDelete
	}
	return coderReleaseActionStop
}

func coderManualCleanupCommand(cfg Config, name string) string {
	if cfg.Coder.DeleteOnRelease {
		return fmt.Sprintf("crabbox stop --provider coder --coder-delete-on-release --id %s", name)
	}
	return fmt.Sprintf("crabbox stop --provider coder --id %s", name)
}

func coderReleaseAction(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case coderReleaseActionDelete, "true":
		return coderReleaseActionDelete, true
	case coderReleaseActionStop, "false":
		return coderReleaseActionStop, true
	default:
		return "", false
	}
}

func coderCleanupReleaseAction(claim LeaseClaim, hasClaim bool) string {
	if hasClaim {
		if action, ok := coderReleaseAction(claim.Labels[coderReleaseActionLabel]); ok {
			return action
		}
	}
	return coderReleaseActionStop
}

func coderWorkspacesToServers(workspaces []coderWorkspace, cfg Config) []Server {
	servers := make([]Server, 0, len(workspaces))
	for _, workspace := range workspaces {
		leaseID, slug, owned := coderWorkspaceLeaseMetadata(workspace, cfg)
		if !owned {
			continue
		}
		servers = append(servers, coderWorkspaceToServer(workspace, cfg, leaseID, slug, true))
	}
	return servers
}

func coderWorkspaceToServer(workspace coderWorkspace, cfg Config, leaseID, slug string, keep bool) Server {
	return coderWorkspaceToServerWithClaim(workspace, cfg, leaseID, slug, keep, LeaseClaim{}, false)
}

func coderWorkspaceToServerWithClaim(workspace coderWorkspace, cfg Config, leaseID, slug string, keep bool, claim LeaseClaim, hasClaim bool) Server {
	if slug == "" {
		slug = coderSlugFromWorkspace(workspace.Name, cfg.Coder.WorkspacePrefix)
	}
	labels := directLeaseLabels(cfg, leaseID, slug, coderProvider, "", keep, time.Now().UTC())
	if labels == nil {
		labels = map[string]string{}
	}
	labels[coderReleaseActionLabel] = coderReleaseActionFromConfig(cfg)
	for k, v := range workspace.Labels {
		if strings.TrimSpace(v) != "" {
			labels[k] = v
		}
	}
	if hasClaim {
		for k, v := range claim.Labels {
			if strings.TrimSpace(v) != "" {
				labels[k] = v
			}
		}
	}
	if leaseID != "" {
		labels["lease"] = leaseID
	}
	if slug != "" {
		labels["slug"] = slug
	}
	labels["coder_workspace"] = workspace.Name
	labels["coder_workspace_ref"] = coderWorkspaceCommandName(workspace)
	labels["work_root"] = coderWorkRoot(cfg)
	labels["state"] = coderWorkspaceState(workspace)
	server := Server{CloudID: coderWorkspaceCommandName(workspace), Provider: coderProvider, Name: workspace.Name, Status: labels["state"], Labels: labels}
	server.ServerType.Name = blank(workspace.Template, "coder-workspace")
	return server
}

func coderWorkspaceLeaseMetadata(workspace coderWorkspace, cfg Config) (string, string, bool) {
	hasCrabboxLabel := coderWorkspaceHasCrabboxLabel(workspace)
	leaseID := strings.TrimSpace(workspace.Labels["crabbox_lease_id"])
	slug := normalizeLeaseSlug(workspace.Labels["crabbox_slug"])
	if leaseID != "" || slug != "" {
		if leaseID == "" {
			leaseID = strings.TrimSpace(workspace.Labels["lease"])
		}
		if slug == "" {
			slug = normalizeLeaseSlug(workspace.Labels["slug"])
		}
		if leaseID == "" {
			leaseID = coderAdoptedWorkspaceLeaseID(workspace)
		}
		return leaseID, slug, true
	}
	leaseID = strings.TrimSpace(workspace.Labels["lease"])
	slug = normalizeLeaseSlug(workspace.Labels["slug"])
	if leaseID != "" || slug != "" {
		if hasCrabboxLabel {
			if leaseID == "" {
				leaseID = coderAdoptedWorkspaceLeaseID(workspace)
			}
			return leaseID, slug, true
		}
	}
	if hasCrabboxLabel {
		return coderAdoptedWorkspaceLeaseID(workspace), "", true
	}
	slug = coderSlugFromWorkspace(workspace.Name, cfg.Coder.WorkspacePrefix)
	if slug == "" {
		return "", "", false
	}
	return coderAdoptedWorkspaceLeaseID(workspace), slug, true
}

func coderAdoptedWorkspaceLeaseID(workspace coderWorkspace) string {
	sum := sha1.Sum([]byte("coder:" + coderWorkspaceCommandName(workspace)))
	return "cbx_" + hex.EncodeToString(sum[:])[:12]
}

func coderSlugFromWorkspace(name, prefix string) string {
	cleanPrefix, err := cleanCoderWorkspacePrefix(prefix)
	if err != nil {
		return ""
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if !strings.HasPrefix(name, cleanPrefix) {
		return ""
	}
	return normalizeLeaseSlug(strings.TrimPrefix(name, cleanPrefix))
}

func coderSSHTarget(cfg Config, workspaceName, workspaceID string) SSHTarget {
	host := coderWorkspaceSSHHost(workspaceName)
	return SSHTarget{
		User:           "coder",
		Host:           host,
		Port:           "22",
		KnownHostsFile: coderKnownHostsFile(workspaceName, workspaceID),
		TargetOS:       targetLinux,
		NetworkKind:    networkPublic,
		ReadyCheck:     "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null",
		SSHConfigProxy: true,
		ProxyCommand:   shellQuote(cfg.Coder.CLIPath) + " ssh --stdio --wait " + shellQuote(blank(cfg.Coder.Wait, "yes")) + " " + shellQuote(workspaceName),
	}
}

func coderKnownHostsFile(workspaceName, workspaceID string) string {
	configDir, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(configDir) == "" {
		configDir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	dir := filepath.Join(configDir, "crabbox", coderProvider, "known_hosts.d")
	_ = os.MkdirAll(dir, 0o700)
	identity := strings.TrimSpace(workspaceID)
	if identity == "" {
		identity = strings.TrimSpace(workspaceName)
	}
	sum := sha1.Sum([]byte(strings.TrimSpace(workspaceName) + "\x00" + identity))
	return filepath.Join(dir, hex.EncodeToString(sum[:])[:12])
}

func coderWorkspaceSSHHost(ref string) string {
	ref = strings.TrimSpace(ref)
	if !strings.Contains(ref, "/") {
		return blank(coderWorkspaceNameFromRef(ref), "coder-workspace")
	}
	base := normalizeLeaseSlug(ref)
	hash := coderWorkspaceHash(ref)
	maxBase := 63 - len("coder-") - len(hash) - 1
	if maxBase < 1 {
		return "coder-" + hash
	}
	if len(base) > maxBase {
		base = strings.Trim(base[:maxBase], "-")
	}
	if base == "" {
		return "coder-" + hash
	}
	return "coder-" + base + "-" + hash
}

func coderWorkspaceReady(workspace coderWorkspace) bool {
	for _, agent := range workspace.Agents {
		if strings.EqualFold(agent.OS, "linux") && (strings.EqualFold(agent.Status, "connected") || strings.EqualFold(agent.Status, "ready")) && (agent.Lifecycle == "" || strings.EqualFold(agent.Lifecycle, "ready")) {
			return true
		}
	}
	return false
}

func coderWorkspaceState(workspace coderWorkspace) string {
	for _, agent := range workspace.Agents {
		if strings.EqualFold(agent.OS, "linux") && strings.EqualFold(agent.Status, "connected") && (agent.Lifecycle == "" || strings.EqualFold(agent.Lifecycle, "ready")) {
			return "ready"
		}
	}
	for _, value := range []string{workspace.Status, workspace.Transition} {
		value = strings.ToLower(strings.TrimSpace(value))
		switch value {
		case "ready":
			return "ready"
		case "running", "started":
			return "running"
		case "starting", "pending", "start":
			return "starting"
		case "stopped", "stop", "stopping":
			return "stopped"
		case "failed", "error", "canceled", "cancelled":
			return value
		}
	}
	return blank(strings.ToLower(strings.TrimSpace(workspace.Status)), "unknown")
}

func findCoderWorkspace(workspaces []coderWorkspace, name string) (coderWorkspace, bool) {
	for _, workspace := range workspaces {
		if normalizeCoderWorkspaceIdentifier(workspace.Name) == normalizeCoderWorkspaceIdentifier(name) || normalizeCoderWorkspaceIdentifier(coderOwnerWorkspace(workspace)) == normalizeCoderWorkspaceIdentifier(name) {
			return workspace, true
		}
	}
	return coderWorkspace{}, false
}

func coderOwnerWorkspace(workspace coderWorkspace) string {
	if workspace.Owner == "" {
		return workspace.Name
	}
	return workspace.Owner + "/" + workspace.Name
}

func coderWorkspaceCommandName(workspace coderWorkspace) string {
	return coderOwnerWorkspace(workspace)
}

func coderWorkspaceNameFromRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if _, name, ok := strings.Cut(ref, "/"); ok {
		return strings.TrimSpace(name)
	}
	return ref
}

func normalizeCoderWorkspaceIdentifier(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

var coderWorkspaceInvalidChars = regexp.MustCompile(`[^a-z0-9-]+`)

const coderMaxRequestedSlugLength = 41
const coderWorkspaceHashLength = 6

func coderUniqueWorkspaceName(workspaces []coderWorkspace, prefix, slug, leaseID string) (string, string, error) {
	workspaceSlug := coderCollisionSlug(slug, leaseID)
	name, err := coderWorkspaceName(prefix, workspaceSlug, leaseID)
	if err != nil {
		return "", "", err
	}
	if coderWorkspaceNameExists(workspaces, name) {
		return "", "", exit(5, "coder workspace name %q collides with existing inventory", name)
	}
	return slug, name, nil
}

func coderWorkspaceNameExists(workspaces []coderWorkspace, name string) bool {
	for _, workspace := range workspaces {
		if normalizeCoderWorkspaceIdentifier(workspace.Name) == normalizeCoderWorkspaceIdentifier(name) {
			return true
		}
	}
	return false
}

func coderWorkspaceName(prefix, slug, leaseID string) (string, error) {
	cleanPrefix, err := cleanCoderWorkspacePrefix(prefix)
	if err != nil {
		return "", err
	}
	base := strings.ToLower(normalizeLeaseSlug(slug))
	base = coderWorkspaceInvalidChars.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = strings.Trim(strings.ToLower(strings.ReplaceAll(leaseID, "_", "-")), "-")
	}
	if base == "new" || base == "create" {
		base = "cbx-" + base
	}
	maxBase := 32 - len(cleanPrefix)
	if maxBase < 1 {
		return "", exit(2, "coder.workspacePrefix %q leaves no room for a workspace name", cleanPrefix)
	}
	if len(base) > maxBase {
		hash := coderWorkspaceHash(base)
		if maxBase <= len(hash)+1 {
			base = hash[:maxBase]
		} else {
			prefixPart := strings.Trim(base[:maxBase-len(hash)-1], "-")
			if prefixPart == "" {
				base = hash[:maxBase]
			} else {
				base = prefixPart + "-" + hash
			}
		}
	}
	name := strings.Trim(cleanPrefix+base, "-")
	if len(name) < 1 || len(name) > 32 {
		return "", exit(2, "coder workspace name %q must be 1-32 characters", name)
	}
	if name == "new" || name == "create" {
		name = "cbx-" + name
	}
	if name[0] == '-' || name[len(name)-1] == '-' {
		return "", exit(2, "coder workspace name %q must start and end with a letter or number", name)
	}
	return name, nil
}

func coderCollisionSlug(slug, leaseID string) string {
	slug = normalizeLeaseSlug(slug)
	suffix := coderWorkspaceHash(leaseID)
	maxBase := coderMaxRequestedSlugLength - len(suffix) - 1
	if maxBase < 1 {
		return suffix[:coderMaxRequestedSlugLength]
	}
	if len(slug) > maxBase {
		slug = strings.Trim(slug[:maxBase], "-")
	}
	if slug == "" {
		return suffix
	}
	return slug + "-" + suffix
}

func coderWorkspaceHash(value string) string {
	sum := sha1.Sum([]byte(value))
	return hex.EncodeToString(sum[:])[:coderWorkspaceHashLength]
}

func coderWorkspaceMissingError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{"not found", "does not exist", "no such workspace", "unknown workspace"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func coderWhoamiMissingLogin(value string) bool {
	msg := strings.ToLower(value)
	for _, needle := range []string{"not logged in", "not authenticated", "no active session", "login required", "please log in"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
