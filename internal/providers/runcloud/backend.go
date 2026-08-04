package runcloud

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type backend struct {
	spec   ProviderSpec
	cfg    Config
	rt     Runtime
	client api
}

func NewBackend(spec ProviderSpec, cfg Config, rt Runtime) (Backend, error) {
	cleaned, err := cleanWorkdir(cfg.RunCloud.Workdir)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.RunCloud.Image) == "" {
		return nil, exit(2, "provider=%s requires runCloud.image", providerName)
	}
	cfg.Provider = providerName
	cfg.TargetOS = targetLinux
	cfg.Network = networkPublic
	cfg.SSHUser = "runcloud"
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = nil
	cfg.WorkRoot = cleaned
	client, err := newClient(cfg, rt)
	if err != nil {
		return nil, err
	}
	return &backend{spec: spec, cfg: cfg, rt: rt, client: client}, nil
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	leaseID := newLeaseID()
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	slug, err := allocateClaimLeaseSlug(leaseID, req.RequestedSlug)
	if err != nil {
		return LeaseTarget{}, err
	}
	name := leaseProviderName(leaseID, slug)
	ttl := req.Options.TTL
	if ttl <= 0 {
		ttl = b.cfg.TTL
	}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s ttl=%s\n", providerName, leaseID, slug, blank(ttl.String(), "-"))
	sandbox, err := b.client.CreateSandbox(ctx, createRequest{
		Name:   name,
		Image:  b.cfg.RunCloud.Image,
		Region: b.cfg.RunCloud.Region,
		TTL:    ttl,
	})
	if err != nil {
		if sandbox.ID != "" && !req.Keep {
			_ = b.client.DeleteSandbox(context.Background(), sandbox.ID)
		}
		return LeaseTarget{}, err
	}
	cleanupFailedAcquire := func() {
		if req.Keep {
			return
		}
		if err := b.client.DeleteSandbox(context.Background(), sandbox.ID); err == nil {
			removeLeaseClaim(leaseID)
			removeStoredTestboxKey(leaseID)
		}
	}
	box, err := b.client.ExposeSandbox(ctx, sandbox.ID, name)
	if err != nil {
		cleanupFailedAcquire()
		return LeaseTarget{}, err
	}
	sandbox.Box = &box
	if req.OnAcquired != nil {
		provisional := LeaseTarget{
			Server:  b.sandboxToServer(sandbox, leaseID, slug, req.Keep),
			SSH:     b.sandboxSSHTarget(sandbox.ID, keyPath),
			LeaseID: leaseID,
		}
		if err := req.OnAcquired(provisional); err != nil {
			if cleanupErr := b.client.DeleteSandbox(context.Background(), sandbox.ID); cleanupErr != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: failed to roll back Run Cloud sandbox %s after acquisition rejection: %v\n", sandbox.ID, cleanupErr)
			}
			return LeaseTarget{}, err
		}
	}
	if err := claimLeaseForRepoProviderScope(leaseID, slug, providerName, sandboxScope(sandbox.ID), req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
		cleanupFailedAcquire()
		return LeaseTarget{}, err
	}
	keyPath, publicKey, err := ensureTestboxKeyFunc(leaseID)
	if err != nil {
		cleanupFailedAcquire()
		return LeaseTarget{}, err
	}
	if err := b.client.InstallSSHKey(ctx, sandbox.ID, publicKey); err != nil {
		cleanupFailedAcquire()
		return LeaseTarget{}, err
	}
	lease, err := b.leaseFromSandbox(ctx, sandbox, leaseID, slug, req.Keep, keyPath, true)
	if err != nil {
		cleanupFailedAcquire()
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s slug=%s sandbox=%s state=ready\n", leaseID, slug, sandbox.ID)
	return lease, nil
}

func (b *backend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	sandbox, leaseID, slug, err := b.resolveSandbox(ctx, req.ID, req.Repo.Root, req.Reclaim)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		return LeaseTarget{Server: b.sandboxToServer(sandbox, leaseID, slug, true), LeaseID: leaseID}, nil
	}
	if state := normalizedState(sandbox.State); state == "paused" || state == "archived" {
		if err := b.client.ResumeSandbox(ctx, sandbox.ID); err != nil {
			return LeaseTarget{}, err
		}
		sandbox, err = b.client.GetSandbox(ctx, sandbox.ID)
		if err != nil {
			return LeaseTarget{}, err
		}
	}
	keyPath, publicKey, err := ensureTestboxKeyFunc(leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if err := b.client.InstallSSHKey(ctx, sandbox.ID, publicKey); err != nil {
		return LeaseTarget{}, err
	}
	return b.leaseFromSandbox(ctx, sandbox, leaseID, slug, true, keyPath, true)
}

func (b *backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	sandboxes, err := b.client.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := runCloudClaimsBySandboxID()
	if err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		claim, ok := claims[sandbox.ID]
		if !ok {
			continue
		}
		out = append(out, b.sandboxToServer(sandbox, claim.LeaseID, claim.Slug, true))
	}
	return out, nil
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	if err := b.client.Check(ctx); err != nil {
		return DoctorResult{}, err
	}
	return DoctorResult{Provider: providerName, Message: "auth=ready cli=ready control_plane=ready mutation=false runtime=unchecked"}, nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	sandbox, leaseID, slug, err := b.resolveSandbox(ctx, req.ID, "", false)
	if err != nil {
		return StatusView{}, err
	}
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(20 * time.Minute)
	}
	for {
		view := b.statusFromSandbox(sandbox, leaseID, slug)
		if !req.Wait || view.Ready || sandboxStateFailed(view.State) {
			return view, nil
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for Run Cloud sandbox %s to become ready", sandbox.ID)
		}
		select {
		case <-ctx.Done():
			return StatusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		sandbox, err = b.client.GetSandbox(ctx, sandbox.ID)
		if err != nil {
			return StatusView{}, err
		}
	}
}

func (b *backend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	sandboxID := strings.TrimSpace(req.Lease.Server.CloudID)
	if sandboxID == "" {
		sandboxID = strings.TrimSpace(req.Lease.Server.Labels["sandbox_id"])
	}
	if sandboxID == "" && req.Lease.LeaseID != "" {
		sandbox, _, _, err := b.resolveSandbox(ctx, req.Lease.LeaseID, "", false)
		if err != nil {
			return err
		}
		sandboxID = sandbox.ID
	}
	if sandboxID == "" {
		return exit(2, "provider=%s requires a Run Cloud sandbox id to release", providerName)
	}
	if err := b.client.DeleteSandbox(ctx, sandboxID); err != nil {
		return err
	}
	removeLeaseClaim(req.Lease.LeaseID)
	removeStoredTestboxKey(req.Lease.LeaseID)
	return nil
}

func (b *backend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("released lease=%s sandbox=%s", lease.LeaseID, blank(lease.Server.CloudID, lease.Server.Labels["sandbox_id"]))
}

func (b *backend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = touchDirectLeaseLabels(server.Labels, b.cfg, req.State, b.now())
	server.Status = req.State
	return server, nil
}

func (b *backend) leaseFromSandbox(ctx context.Context, sandbox sandboxData, leaseID, slug string, keep bool, keyPath string, waitSSH bool) (LeaseTarget, error) {
	server := b.sandboxToServer(sandbox, leaseID, slug, keep)
	target := b.sandboxSSHTarget(sandbox.ID, keyPath)
	if waitSSH {
		if err := waitForSSHReadyFunc(ctx, &target, b.rt.Stderr, "run-cloud ssh", bootstrapWaitTimeout(b.cfg)); err != nil {
			return LeaseTarget{}, err
		}
		server.Labels["state"] = "ready"
		server.Status = "ready"
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *backend) resolveSandbox(ctx context.Context, identifier, repoRoot string, reclaim bool) (sandboxData, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return sandboxData{}, "", "", exit(2, "provider=%s requires a Crabbox lease id, slug, or Run Cloud sandbox id", providerName)
	}
	if claim, ok, err := resolveLeaseClaimForProvider(identifier, providerName); err != nil {
		return sandboxData{}, "", "", err
	} else if ok {
		sandboxID := sandboxIDFromScope(claim.ProviderScope)
		if sandboxID == "" {
			sandboxes, listErr := b.client.ListSandboxes(ctx)
			if listErr != nil {
				return sandboxData{}, "", "", listErr
			}
			name := leaseProviderName(claim.LeaseID, claim.Slug)
			for _, candidate := range sandboxes {
				if candidate.Name == name {
					sandboxID = candidate.ID
					break
				}
			}
		}
		if sandboxID == "" {
			return sandboxData{}, "", "", exit(4, "Run Cloud lease %q has no sandbox id", identifier)
		}
		sandbox, getErr := b.client.GetSandbox(ctx, sandboxID)
		if getErr != nil {
			return sandboxData{}, "", "", getErr
		}
		if repoRoot != "" {
			if err := claimLeaseForRepoProviderScope(claim.LeaseID, claim.Slug, providerName, sandboxScope(sandbox.ID), repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim); err != nil {
				return sandboxData{}, "", "", err
			}
		}
		return sandbox, claim.LeaseID, claim.Slug, nil
	}

	sandboxes, err := b.client.ListSandboxes(ctx)
	if err != nil {
		return sandboxData{}, "", "", err
	}
	var matched *sandboxData
	for i := range sandboxes {
		if sandboxes[i].ID == identifier || sandboxes[i].Name == identifier {
			matched = &sandboxes[i]
			break
		}
	}
	if matched == nil {
		return sandboxData{}, "", "", exit(4, "Run Cloud sandbox %q was not found", identifier)
	}
	if !reclaim {
		return sandboxData{}, "", "", exit(4, "Run Cloud sandbox %q is not claimed by Crabbox; use --reclaim to adopt it", identifier)
	}
	leaseID := newLeaseID()
	slug := normalizeLeaseSlug(matched.Name)
	if slug == "" {
		slug = newLeaseSlug(leaseID)
	}
	if err := claimLeaseForRepoProviderScope(leaseID, slug, providerName, sandboxScope(matched.ID), repoRoot, b.cfg.IdleTimeout, true); err != nil {
		return sandboxData{}, "", "", err
	}
	return *matched, leaseID, slug, nil
}

func (b *backend) sandboxToServer(sandbox sandboxData, leaseID, slug string, keep bool) Server {
	labels := directLeaseLabels(b.cfg, leaseID, slug, providerName, b.cfg.RunCloud.Region, keep, b.now())
	labels["sandbox_id"] = sandbox.ID
	labels["sandbox_name"] = sandbox.Name
	labels["sandbox_state"] = normalizedState(sandbox.State)
	labels["work_root"] = b.cfg.WorkRoot
	if sandbox.Image != "" {
		labels["image"] = sandbox.Image
	}
	if host := sandboxHostname(sandbox); host != "" {
		labels["hostname"] = host
	}
	server := Server{
		Provider: providerName,
		CloudID:  sandbox.ID,
		Name:     blank(sandbox.Name, leaseProviderName(leaseID, slug)),
		Status:   normalizedState(sandbox.State),
		Labels:   labels,
	}
	server.ServerType.Name = "run-cloud-sandbox"
	server.PublicNet.IPv4.IP = sandboxHostname(sandbox)
	return server
}

func (b *backend) statusFromSandbox(sandbox sandboxData, leaseID, slug string) StatusView {
	server := b.sandboxToServer(sandbox, leaseID, slug, true)
	state := normalizedState(sandbox.State)
	return StatusView{
		ID:         leaseID,
		Slug:       slug,
		Provider:   providerName,
		TargetOS:   targetLinux,
		State:      state,
		ServerID:   sandbox.ID,
		ServerType: server.ServerType.Name,
		Host:       sandboxHostname(sandbox),
		Network:    networkPublic,
		SSHHost:    sandbox.ID,
		SSHUser:    "runcloud",
		SSHPort:    "22",
		Labels:     server.Labels,
		HasHost:    sandboxHostname(sandbox) != "",
		Ready:      state == "running" && sandboxHostname(sandbox) != "",
	}
}

func (b *backend) sandboxSSHTarget(sandboxID, keyPath string) SSHTarget {
	return SSHTarget{
		User:           "runcloud",
		Host:           sandboxID,
		Key:            keyPath,
		KnownHostsFile: filepath.Join(filepath.Dir(keyPath), "known_hosts"),
		Port:           "22",
		TargetOS:       targetLinux,
		NetworkKind:    networkPublic,
		SSHConfigProxy: true,
		ProxyCommand:   shellQuote(b.cfg.RunCloud.CLIPath) + " sandbox proxy " + shellQuote(sandboxID),
		ReadyCheck:     "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null && command -v python3 >/dev/null",
	}
}

func runCloudClaimsBySandboxID() (map[string]LeaseClaim, error) {
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	out := map[string]LeaseClaim{}
	for _, claim := range claims {
		if claim.Provider != providerName {
			continue
		}
		if id := sandboxIDFromScope(claim.ProviderScope); id != "" {
			out[id] = claim
		}
	}
	return out, nil
}

func sandboxScope(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	return "sandbox:" + id
}

func sandboxIDFromScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if !strings.HasPrefix(scope, "sandbox:") {
		return ""
	}
	return strings.TrimPrefix(scope, "sandbox:")
}

func sandboxHostname(sandbox sandboxData) string {
	if sandbox.Box != nil && strings.TrimSpace(sandbox.Box.Hostname) != "" {
		return strings.TrimSpace(sandbox.Box.Hostname)
	}
	return strings.TrimSpace(sandbox.Hostname)
}

func normalizedState(state string) string {
	state = strings.ToLower(strings.TrimSpace(state))
	if state == "" {
		return "provisioning"
	}
	return state
}

func sandboxStateFailed(state string) bool {
	switch normalizedState(state) {
	case "interrupted", "stopped", "destroyed", "failed", "error":
		return true
	default:
		return false
	}
}

func (b *backend) now() time.Time { return now(b.rt) }

var ensureTestboxKeyFunc = ensureTestboxKey
var waitForSSHReadyFunc = waitForSSHReady
