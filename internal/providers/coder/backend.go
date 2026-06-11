package coder

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
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
	workspaceName, err := coderWorkspaceName(b.cfg.Coder.WorkspacePrefix, slug, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=coder lease=%s slug=%s workspace=%s template=%s keep=%v\n", leaseID, slug, workspaceName, b.cfg.Coder.Template, req.Keep)
	if err := client.create(ctx, b.cfg, workspaceName); err != nil {
		if !req.Keep {
			err = b.rollbackCreateError(workspaceName, client, err)
		}
		return LeaseTarget{}, err
	}
	workspaces, err := client.list(ctx)
	if err != nil {
		if !req.Keep {
			err = b.rollbackCreatedWorkspace(workspaceName, client, err)
		}
		return LeaseTarget{}, err
	}
	workspace, ok := findCoderWorkspace(workspaces, workspaceName)
	if !ok {
		if !req.Keep {
			err = b.rollbackCreatedWorkspace(workspaceName, client, exit(5, "coder workspace %s was created but not found in coder list", workspaceName))
			return LeaseTarget{}, err
		}
		return LeaseTarget{}, exit(5, "coder workspace %s was created but not found in coder list", workspaceName)
	}
	server := coderWorkspaceToServer(workspace, b.cfg, leaseID, slug, req.Keep)
	target := coderSSHTarget(b.cfg, workspaceName)
	if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "coder ssh", bootstrapWaitTimeout(b.cfg)); err != nil {
		if !req.Keep {
			err = b.rollbackCreatedWorkspace(workspaceName, client, err)
		}
		return LeaseTarget{}, err
	}
	server.Status = "ready"
	server.Labels["state"] = "ready"
	if err := claimLeaseForRepoProvider(leaseID, slug, coderProvider, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
		if !req.Keep {
			err = b.rollbackCreatedWorkspace(workspaceName, client, err)
		}
		return LeaseTarget{}, err
	}
	_ = updateLeaseClaimEndpoint(leaseID, server, target)
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *coderLeaseBackend) rollbackCreatedWorkspace(name string, client *coderClient, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.releaseWorkspace(cleanupCtx, client, name); err != nil {
		return exit(coderExitCode(cause), "%v; coder cleanup failed for workspace %s; manual cleanup: crabbox stop --provider coder %s: %v", cause, name, name, err)
	}
	return cause
}

func (b *coderLeaseBackend) rollbackCreateError(name string, client *coderClient, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workspaces, err := client.list(cleanupCtx)
	if err != nil {
		return cause
	}
	if _, ok := findCoderWorkspace(workspaces, name); !ok {
		return cause
	}
	return b.rollbackCreatedWorkspace(name, client, cause)
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
	listFn := client.list
	if strings.Contains(strings.TrimSpace(req.ID), "/") {
		listFn = client.listAll
	}
	workspaces, err := listFn(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	workspace, leaseID, slug, err := b.resolveWorkspace(req.ID, workspaces)
	if err != nil {
		return LeaseTarget{}, err
	}
	server := coderWorkspaceToServer(workspace, b.cfg, leaseID, slug, true)
	workspaceRef := coderWorkspaceCommandName(workspace)
	if req.ReleaseOnly || req.StatusOnly {
		lease := LeaseTarget{Server: server, LeaseID: leaseID}
		if !req.ReadyProbe || !coderWorkspaceReady(workspace) {
			return lease, nil
		}
		lease.SSH = coderSSHTarget(b.cfg, workspaceRef)
		return lease, nil
	}
	if !coderWorkspaceReady(workspace) {
		if err := client.start(ctx, workspaceRef); err != nil {
			return LeaseTarget{}, err
		}
		workspaces, err = client.list(ctx)
		if err != nil {
			return LeaseTarget{}, err
		}
		if refreshed, found := findCoderWorkspace(workspaces, workspaceRef); found {
			workspace = refreshed
			server = coderWorkspaceToServer(workspace, b.cfg, leaseID, slug, true)
			workspaceRef = coderWorkspaceCommandName(workspace)
		}
	}
	target := coderSSHTarget(b.cfg, workspaceRef)
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

func (b *coderLeaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	client, err := newCoderClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	workspaces, err := client.list(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(workspaces))
	for _, workspace := range workspaces {
		leaseID, slug, owned := coderWorkspaceLeaseMetadata(workspace, b.cfg)
		if !owned && !req.All {
			continue
		}
		servers = append(servers, coderWorkspaceToServer(workspace, b.cfg, leaseID, slug, true))
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
		checks = append(checks, DoctorCheck{Status: "fail", Check: "auth", Message: err.Error(), Details: map[string]string{"mutation": "false", "classification": "missing_login"}})
		return DoctorResult{Provider: coderProvider, Status: "fail", Message: "cli=ready auth=missing_login inventory=unchecked mutation=false", Checks: checks}, err
	}
	checks = append(checks, DoctorCheck{Status: "pass", Check: "auth", Message: "coder login ready", Details: map[string]string{"mutation": "false"}})
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		checks = append(checks, DoctorCheck{Status: "fail", Check: "inventory", Message: err.Error(), Details: map[string]string{"mutation": "false"}})
		return DoctorResult{Provider: coderProvider, Status: "fail", Message: "cli=ready auth=ready inventory=failed mutation=false"}, err
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
	if b.cfg.Coder.DeleteOnRelease {
		err = client.delete(ctx, name)
	} else {
		err = client.stop(ctx, name)
	}
	if err != nil {
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
	workspaces, err := client.list(ctx)
	if err != nil {
		return err
	}
	claims, err := listCoderClaimsByWorkspace(b.cfg)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, workspace := range workspaces {
		leaseID, slug, owned := coderWorkspaceLeaseMetadata(workspace, b.cfg)
		if !owned {
			continue
		}
		server := coderWorkspaceToServer(workspace, b.cfg, leaseID, slug, false)
		claim, hasClaim := claims[workspace.Name]
		if leaseID == "" && hasClaim {
			leaseID = claim.LeaseID
		}
		shouldAct, reason := shouldCleanupCoder(server, claim, hasClaim, now)
		if !shouldAct {
			fmt.Fprintf(b.rt.Stderr, "skip coder workspace=%s reason=%s\n", workspace.Name, reason)
			continue
		}
		action := "stop"
		if b.cfg.Coder.DeleteOnRelease {
			action = "delete"
		}
		fmt.Fprintf(b.rt.Stdout, "coder cleanup %s workspace=%s lease=%s reason=%s dry_run=%t\n", action, workspace.Name, blank(leaseID, "-"), reason, req.DryRun)
		if req.DryRun {
			continue
		}
		if b.cfg.Coder.DeleteOnRelease {
			if err := client.delete(ctx, workspace.Name); err != nil {
				return err
			}
		} else if err := client.stop(ctx, workspace.Name); err != nil {
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
		name := strings.TrimSpace(claim.Labels["coder_workspace"])
		if name == "" {
			name = coderWorkspaceNameFromRef(claim.Labels["coder_workspace_ref"])
		}
		if name == "" {
			name, err = coderWorkspaceName(cfg.Coder.WorkspacePrefix, claim.Slug, claim.LeaseID)
			if err != nil {
				continue
			}
		}
		if name != "" {
			out[name] = claim
		}
	}
	return out, nil
}

func shouldCleanupCoder(server Server, claim LeaseClaim, hasClaim bool, now time.Time) (bool, string) {
	if strings.EqualFold(server.Labels["keep"], "true") {
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
	if !coderServerRunning(server.Status) && server.Status != "ready" {
		return true, "workspace state=" + blank(server.Status, "unknown")
	}
	return false, "missing claim"
}

func coderWorkspaceHasCrabboxLabel(workspace coderWorkspace) bool {
	if strings.EqualFold(strings.TrimSpace(workspace.Labels["crabbox"]), "true") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(workspace.Labels["created_by"]), "crabbox")
}

func coderServerRunning(status string) bool {
	status = strings.ToLower(strings.TrimSpace(status))
	return status == "running" || status == "ready" || status == "starting"
}

func (b *coderLeaseBackend) resolveWorkspace(identifier string, workspaces []coderWorkspace) (coderWorkspace, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return coderWorkspace{}, "", "", exit(2, "coder resolve requires a lease id, slug, workspace, or owner/workspace")
	}
	if claim, ok, err := resolveLeaseClaimForProvider(identifier, coderProvider); err != nil {
		return coderWorkspace{}, "", "", err
	} else if ok {
		name := strings.TrimSpace(claim.Labels["coder_workspace_ref"])
		if name == "" {
			name = strings.TrimSpace(claim.Labels["coder_workspace"])
		}
		if name == "" {
			var err error
			name, err = coderWorkspaceName(b.cfg.Coder.WorkspacePrefix, claim.Slug, claim.LeaseID)
			if err != nil {
				return coderWorkspace{}, "", "", err
			}
		}
		if workspace, found := findCoderWorkspace(workspaces, name); found {
			return workspace, claim.LeaseID, claim.Slug, nil
		}
	}
	normalized := normalizeCoderWorkspaceIdentifier(identifier)
	normalizedSlug := normalizeLeaseSlug(identifier)
	matches := []coderWorkspace{}
	for _, workspace := range workspaces {
		leaseID, slug, owned := coderWorkspaceLeaseMetadata(workspace, b.cfg)
		if normalizeCoderWorkspaceIdentifier(workspace.Name) == normalized || normalizeCoderWorkspaceIdentifier(coderOwnerWorkspace(workspace)) == normalized {
			matches = append(matches, workspace)
			continue
		}
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
	leaseID, slug, _ := coderWorkspaceLeaseMetadata(matches[0], b.cfg)
	return matches[0], leaseID, slug, nil
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
	if slug == "" {
		slug = coderSlugFromWorkspace(workspace.Name, cfg.Coder.WorkspacePrefix)
	}
	labels := directLeaseLabels(cfg, leaseID, slug, coderProvider, "", keep, time.Now().UTC())
	if labels == nil {
		labels = map[string]string{}
	}
	labels["coder_workspace"] = workspace.Name
	labels["coder_workspace_ref"] = coderWorkspaceCommandName(workspace)
	labels["state"] = coderWorkspaceState(workspace)
	server := Server{CloudID: workspace.Name, Provider: coderProvider, Name: workspace.Name, Status: labels["state"], Labels: labels}
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
		return leaseID, slug, true
	}
	leaseID = strings.TrimSpace(workspace.Labels["lease"])
	slug = normalizeLeaseSlug(workspace.Labels["slug"])
	if leaseID != "" || slug != "" {
		if hasCrabboxLabel {
			return leaseID, slug, true
		}
	}
	if hasCrabboxLabel {
		return "", "", true
	}
	slug = coderSlugFromWorkspace(workspace.Name, cfg.Coder.WorkspacePrefix)
	if slug == "" {
		return "", "", false
	}
	return "", slug, true
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

func coderSSHTarget(cfg Config, workspaceName string) SSHTarget {
	return SSHTarget{
		User:           "coder",
		Host:           workspaceName,
		Port:           "22",
		TargetOS:       targetLinux,
		NetworkKind:    networkPublic,
		ReadyCheck:     "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null",
		SSHConfigProxy: true,
		ProxyCommand:   shellQuote(cfg.Coder.CLIPath) + " ssh --stdio --wait " + shellQuote(blank(cfg.Coder.Wait, "yes")) + " " + shellQuote(workspaceName),
	}
}

func coderWorkspaceReady(workspace coderWorkspace) bool {
	state := coderWorkspaceState(workspace)
	if state == "ready" || state == "running" {
		return true
	}
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
		case "running", "ready", "started", "start":
			return "ready"
		case "stopped", "stop", "stopping":
			return "stopped"
		case "starting", "pending":
			return "starting"
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
		base = strings.Trim(base[:maxBase], "-")
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
