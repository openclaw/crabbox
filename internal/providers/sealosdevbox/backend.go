package sealosdevbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type backend struct {
	spec                 core.ProviderSpec
	cfg                  core.Config
	rt                   core.Runtime
	sshReady             func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error
	sshRun               func(context.Context, core.SSHTarget, string) error
	pollIntervalOverride time.Duration
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	checks := []core.DoctorCheck{}
	add := func(check core.DoctorCheck) {
		checks = append(checks, check)
	}
	if _, err := b.kubectl(ctx, nil, false, "version", "--client=true", "-o", "json"); err != nil {
		add(doctorCheck("failed", "kubectl", err.Error(), nil))
		return doctorResult(checks), nil
	}
	add(doctorCheck("ok", "kubectl", "client=ready", nil))
	if _, err := b.kubectl(ctx, nil, false, "config", "get-contexts", b.cfg.SealosDevbox.Context, "-o", "name"); err != nil {
		add(doctorCheck("failed", "context", err.Error(), map[string]string{"context": b.cfg.SealosDevbox.Context}))
		return doctorResult(checks), nil
	}
	add(doctorCheck("ok", "context", "found", map[string]string{"context": b.cfg.SealosDevbox.Context}))
	if _, err := b.kubectl(ctx, nil, false, "get", "namespace", b.cfg.SealosDevbox.Namespace, "-o", "name"); err != nil {
		add(doctorCheck("failed", "namespace", err.Error(), map[string]string{"namespace": b.cfg.SealosDevbox.Namespace}))
		return doctorResult(checks), nil
	}
	add(doctorCheck("ok", "namespace", "found", map[string]string{"namespace": b.cfg.SealosDevbox.Namespace}))
	discovery, err := b.kubectl(ctx, nil, false, "get", "--raw", "/apis/"+devboxGroupVersion)
	if err != nil {
		add(doctorCheck("failed", "crd.devboxes", err.Error(), map[string]string{"groupVersion": devboxGroupVersion}))
		return doctorResult(checks), nil
	}
	var resources struct {
		GroupVersion string `json:"groupVersion"`
		Resources    []struct {
			Name string `json:"name"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(discovery), &resources); err != nil {
		add(doctorCheck("failed", "crd.devboxes", "DevBox API discovery returned invalid JSON", map[string]string{"groupVersion": devboxGroupVersion}))
		return doctorResult(checks), nil
	}
	if resources.GroupVersion != devboxGroupVersion || !apiResourceExists(resources.Resources, "devboxes") {
		add(doctorCheck("failed", "crd.devboxes", "DevBox API discovery does not serve devboxes at "+devboxGroupVersion, map[string]string{"groupVersion": resources.GroupVersion}))
		return doctorResult(checks), nil
	}
	add(doctorCheck("ok", "crd.devboxes", "found", map[string]string{"groupVersion": resources.GroupVersion, "resource": "devboxes"}))
	rules, err := b.permissionRules(ctx)
	if err != nil {
		add(doctorCheck("failed", "rbac.rules", err.Error(), map[string]string{"namespace": b.cfg.SealosDevbox.Namespace}))
		return doctorResult(checks), nil
	}
	for _, resource := range []string{devboxResource, "secrets", "pods", "events"} {
		for _, verb := range []string{"get", "list"} {
			add(canIWithRules(rules, verb, resource))
		}
	}
	for _, verb := range []string{"create", "update", "patch", "delete"} {
		add(canIWithRules(rules, verb, devboxResource))
	}
	creationDetails := map[string]string{
		"image_configured":       boolString(strings.TrimSpace(b.cfg.SealosDevbox.Image) != ""),
		"template_id_configured": boolString(strings.TrimSpace(b.cfg.SealosDevbox.TemplateID) != ""),
	}
	if strings.TrimSpace(b.cfg.SealosDevbox.Image) == "" {
		add(doctorCheck("failed", "devbox.source", "sealos-devbox requires image before creating a DevBox", creationDetails))
	} else {
		add(doctorCheck("ok", "devbox.source", "configured", creationDetails))
	}
	networkDetails := map[string]string{"network": normalizeNetwork(b.cfg.SealosDevbox.Network)}
	switch normalizeNetwork(b.cfg.SealosDevbox.Network) {
	case networkSSHGate:
		hostConfigured := strings.TrimSpace(b.cfg.SealosDevbox.SSHGatewayHost) != ""
		portConfigured := strings.TrimSpace(b.cfg.SealosDevbox.SSHGatewayPort) != ""
		networkDetails["host_configured"] = boolString(hostConfigured)
		networkDetails["port_configured"] = boolString(portConfigured)
		if !hostConfigured || !portConfigured {
			add(doctorCheck("failed", "network", "SSHGate requires sshGatewayHost and sshGatewayPort", networkDetails))
		} else {
			add(doctorCheck("ok", "network", "SSHGate configured", networkDetails))
		}
	case networkNodePort:
		nodeHostConfigured := strings.TrimSpace(b.cfg.SealosDevbox.NodeHost) != ""
		networkDetails["node_host_configured"] = boolString(nodeHostConfigured)
		if !nodeHostConfigured {
			add(doctorCheck("failed", "network", "NodePort requires nodeHost until live discovery is implemented", networkDetails))
		} else {
			add(doctorCheck("ok", "network", "NodePort configured", networkDetails))
		}
	default:
		add(doctorCheck("failed", "network", "network must be SSHGate or NodePort", networkDetails))
	}
	add(doctorCheck("ok", "automation_surface", AutomationSurfaceDecision, map[string]string{"surface": AutomationSurfaceDecision}))
	return doctorResult(checks), nil
}

func doctorResult(checks []core.DoctorCheck) core.DoctorResult {
	status := "ready"
	for _, check := range checks {
		if check.Status == "failed" || check.Status == "missing" {
			status = "blocked"
			break
		}
	}
	return core.DoctorResult{
		Provider: providerName,
		Status:   status,
		Message:  formatDoctorSummary(checks),
		Checks:   checks,
	}
}

func apiResourceExists(resources []struct {
	Name string `json:"name"`
}, name string) bool {
	for _, resource := range resources {
		if resource.Name == name {
			return true
		}
	}
	return false
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func (b *backend) Acquire(ctx context.Context, req core.AcquireRequest) (lease core.LeaseTarget, err error) {
	if AutomationSurfaceDecision != "crd_first" {
		return core.LeaseTarget{}, core.Exit(2, "sealos-devbox lifecycle plan requires automation_surface=crd_first, found %s", AutomationSurfaceDecision)
	}
	leaseID := strings.TrimSpace(req.RequestedLeaseID)
	if leaseID == "" {
		leaseID = core.NewLeaseID()
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil {
		return core.LeaseTarget{}, err
	} else if exists {
		return core.LeaseTarget{}, core.Exit(2, "Sealos lease %s already has a local claim; reuse the existing lease or choose another lease ID", leaseID)
	}
	slug, err := b.allocateLeaseSlug(ctx, leaseID, req.RequestedSlug)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	name := core.LeaseProviderName(leaseID, slug)
	now := b.now()
	manifest, err := b.renderDevboxManifest(name, leaseID, slug, req.Keep, now)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if b.rt.Stderr != nil {
		fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s devbox=%s namespace=%s keep=%v\n", providerName, leaseID, slug, name, b.cfg.SealosDevbox.Namespace, req.Keep)
	}
	applied := false
	if createErr := b.createDevbox(ctx, manifest); createErr != nil {
		if kubectlAlreadyExists(createErr) {
			return core.LeaseTarget{}, core.Exit(4, "refusing to overwrite existing Sealos DevBox %q: %v", name, createErr)
		}
		item, reconcileErr := b.getDevbox(ctx, name)
		if reconcileErr != nil {
			return core.LeaseTarget{}, errors.Join(createErr, fmt.Errorf("reconcile Sealos DevBox create: %w", reconcileErr))
		}
		actualName, actualLeaseID, actualSlug, identityErr := identityFromDevbox(item, name)
		if identityErr != nil || !b.itemMatchesScope(item) || actualName != name || actualLeaseID != leaseID || actualSlug != slug {
			return core.LeaseTarget{}, errors.Join(createErr, core.Exit(4, "refusing to adopt Sealos DevBox %q after an ambiguous create", name), identityErr)
		}
		applied = true
	} else {
		applied = true
	}
	keyPersisted := false
	claimPersisted := false
	var persistedClaim core.LeaseClaim
	forceRollback := false
	defer func() {
		if err == nil || (req.Keep && !forceRollback) || !applied {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cleanupAction := func() error {
			rollbackItem, _, rollbackLeaseID, rollbackSlug, rollbackErr := b.validateDevboxIdentity(cleanupCtx, name, leaseID, slug)
			if rollbackErr != nil || !b.itemMatchesScope(rollbackItem) || rollbackLeaseID != leaseID || rollbackSlug != slug {
				return errors.Join(core.Exit(4, "refusing to delete unverified Sealos DevBox %s after acquire failure for lease %s", name, leaseID), rollbackErr)
			}
			if err := b.deleteDevbox(cleanupCtx, rollbackItem); err != nil {
				return err
			}
			if keyPersisted {
				core.RemoveStoredTestboxKey(leaseID)
			}
			return nil
		}
		var cleanupErr error
		if claimPersisted {
			cleanupErr = core.RemoveLeaseClaimIfUnchangedAfter(leaseID, persistedClaim, cleanupAction)
		} else {
			cleanupErr = core.CleanupLeaseClaimIfUnchangedAfter(leaseID, core.LeaseClaim{}, false, cleanupAction)
		}
		if cleanupErr != nil {
			if b.rt.Stderr != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: failed to roll back Sealos DevBox %s after acquire failure for lease %s: %v\n", name, leaseID, cleanupErr)
			}
			return
		}
	}()
	item, err := b.waitForDevboxPrepared(ctx, name, bootstrapWaitTimeout(b.cfg))
	if err != nil {
		return core.LeaseTarget{}, err
	}
	server := b.serverFromDevbox(item)
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	target, err := b.sshTarget(item, keyPath, true)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.OnAcquired != nil {
		if err := req.OnAcquired(core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}); err != nil {
			forceRollback = true
			return core.LeaseTarget{}, err
		}
	}
	previousClaim, previousClaimExists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if err := b.validateClaimAdoption(previousClaim, previousClaimExists, item); err != nil {
		return core.LeaseTarget{}, err
	}
	if previousClaimExists && !req.Reclaim {
		if err := b.validateClaimBinding(previousClaim, item); err != nil {
			return core.LeaseTarget{}, unclaimedDevboxError(name)
		}
	}
	persistedClaim, err = b.claimExactTarget(leaseID, slug, req.Repo.Root, server, core.SSHTarget{}, b.cfg.IdleTimeout, req.Reclaim, previousClaim, previousClaimExists)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	claimPersisted = true
	secret, err := b.waitForDevboxSecret(ctx, item, bootstrapWaitTimeout(b.cfg))
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keys, err := parseDevboxSecretKeys(secret)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	var updatedClaim core.LeaseClaim
	updatedClaim, keyPath, err = persistDevboxKeyIfClaimUnchanged(leaseID, persistedClaim, server, keys)
	keyPersisted = keyPath != ""
	if err != nil {
		return core.LeaseTarget{}, err
	}
	persistedClaim = updatedClaim
	if err := b.prepareSSH(ctx, &target, "Sealos DevBox SSH"); err != nil {
		events, _ := b.listEvents(ctx, name)
		return core.LeaseTarget{}, core.Exit(5, "%v; Sealos DevBox diagnostics: %s", err, devboxDiagnostics(item, events, nil))
	}
	updatedClaim, err = core.UpdateLeaseClaimEndpointIfUnchanged(leaseID, persistedClaim, server, target)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	persistedClaim = updatedClaim
	core.SetServerLeaseClaimSnapshot(&server, persistedClaim, true)
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *backend) Resolve(ctx context.Context, req core.ResolveRequest) (lease core.LeaseTarget, err error) {
	if req.ReleaseOnly {
		return b.resolveClaimedReleaseTarget(ctx, req.ID)
	}
	item, _, leaseID, slug, err := b.resolveDevbox(ctx, req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	server := b.serverFromDevbox(item)
	target := core.SSHTarget{}
	if req.ReleaseOnly {
		return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
	}
	if req.StatusOnly || req.NoLocalStateMutations {
		resolved, err := b.sshTarget(item, b.statusSSHKey(leaseID), false)
		if err != nil {
			if !req.StatusOnly {
				return core.LeaseTarget{}, err
			}
		} else {
			target = resolved
		}
		if req.NoLocalStateMutations {
			return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
		}
		return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
	}
	var previousClaim, preflightClaim core.LeaseClaim
	var previousClaimExists, rollbackClaim bool
	previousClaim, previousClaimExists, err = core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if err := b.validateClaimAdoption(previousClaim, previousClaimExists, item); err != nil {
		return core.LeaseTarget{}, err
	}
	if previousClaimExists {
		if bindErr := b.validateClaimBinding(previousClaim, item); bindErr != nil && !req.Reclaim {
			return core.LeaseTarget{}, bindErr
		}
	} else if !req.Reclaim {
		return core.LeaseTarget{}, unclaimedDevboxError(item.Metadata.Name)
	}
	if req.Reclaim && strings.TrimSpace(req.Repo.Root) == "" {
		return core.LeaseTarget{}, core.Exit(2, "Sealos DevBox %q cannot be reclaimed without a repository root", item.Metadata.Name)
	}
	defer func() {
		if err == nil || !rollbackClaim {
			return
		}
		if restoreErr := core.RestoreLeaseClaimIfUnchanged(leaseID, preflightClaim, previousClaim, previousClaimExists); restoreErr != nil && b.rt.Stderr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: restore Sealos DevBox lease claim %s after resolve failure: %v\n", leaseID, restoreErr)
		}
	}()
	if req.Repo.Root != "" {
		preflightClaim, err = b.claimExactTarget(leaseID, slug, req.Repo.Root, server, core.SSHTarget{}, b.cfg.IdleTimeout, req.Reclaim, previousClaim, previousClaimExists)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		rollbackClaim = true
	} else {
		preflightClaim = previousClaim
	}
	item, server, err = b.resumeDevboxIfPaused(ctx, item, server)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	secret, err := b.getSecret(ctx, devboxSecretName(item))
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if err := validateDevboxSecretOwner(secret, item); err != nil {
		return core.LeaseTarget{}, err
	}
	keys, err := parseDevboxSecretKeys(secret)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	var updatedClaim core.LeaseClaim
	updatedClaim, keyPath, err := persistDevboxKeyIfClaimUnchanged(leaseID, preflightClaim, server, keys)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	preflightClaim = updatedClaim
	target, err = b.sshTarget(item, keyPath, true)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if err := b.prepareSSH(ctx, &target, "Sealos DevBox SSH"); err != nil {
		events, _ := b.listEvents(ctx, item.Metadata.Name)
		return core.LeaseTarget{}, core.Exit(5, "%v; Sealos DevBox diagnostics: %s", err, devboxDiagnostics(item, events, nil))
	}
	if req.Repo.Root != "" {
		updatedClaim, err = core.UpdateLeaseClaimEndpointIfUnchanged(leaseID, preflightClaim, server, target)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		preflightClaim = updatedClaim
		rollbackClaim = false
	}
	core.SetServerLeaseClaimSnapshot(&server, preflightClaim, true)
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *backend) resolveClaimedReleaseTarget(ctx context.Context, identifier string) (core.LeaseTarget, error) {
	claim, ok, err := b.resolveClaim(identifier)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if !ok {
		return core.LeaseTarget{}, unclaimedDevboxError(identifier)
	}
	name := devboxNameFromClaim(claim, b.cfg)
	if name == "" {
		return core.LeaseTarget{}, unclaimedDevboxError(identifier)
	}
	if err := b.validateStoredClaimResource(claim, name); err != nil {
		return core.LeaseTarget{}, err
	}
	if item, err := b.getDevbox(ctx, name); err != nil {
		if kubernetesObjectNotFound(err) {
			server := serverFromClaim(claim, b.cfg)
			core.SetServerLeaseClaimSnapshot(&server, claim, true)
			return core.LeaseTarget{Server: server, LeaseID: claim.LeaseID}, nil
		}
		return core.LeaseTarget{}, err
	} else {
		if !b.itemMatchesScope(item) {
			return core.LeaseTarget{}, core.Exit(4, "Sealos DevBox %q is outside the active provider scope (expected %s, found %s)", name, b.claimScopeID(), devboxScopeID(item))
		}
		if err := b.validateClaimBinding(claim, item); err != nil {
			return core.LeaseTarget{}, err
		}
		server := b.serverFromDevbox(item)
		core.SetServerLeaseClaimSnapshot(&server, claim, true)
		return core.LeaseTarget{Server: server, LeaseID: claim.LeaseID}, nil
	}
}

func (b *backend) resumeDevboxIfPaused(ctx context.Context, item devboxItem, server core.Server) (devboxItem, core.Server, error) {
	if normalizeDevboxState(item) != devboxStatePaused {
		return item, server, nil
	}
	name := strings.TrimSpace(item.Metadata.Name)
	if name == "" {
		return item, server, core.Exit(5, "Sealos DevBox has no metadata.name")
	}
	if err := b.patchDevboxState(ctx, name, item.Metadata.ResourceVersion, devboxStateRun, nil); err != nil {
		return item, server, err
	}
	resumed, err := b.waitForDevboxPrepared(ctx, name, bootstrapWaitTimeout(b.cfg))
	if err != nil {
		return item, server, err
	}
	return resumed, b.serverFromDevbox(resumed), nil
}

func (b *backend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	id := strings.TrimSpace(req.Lease.LeaseID)
	if id == "" {
		id = req.Lease.Server.Name
	}
	item, name, leaseID, slug, err := b.resolveDevbox(ctx, id)
	if err != nil {
		return core.Server{}, err
	}
	if expectedLeaseID := strings.TrimSpace(req.Lease.LeaseID); expectedLeaseID != "" && leaseID != expectedLeaseID {
		return core.Server{}, core.Exit(4, "Sealos DevBox %q lease identity changed: expected %s, found %s", name, expectedLeaseID, leaseID)
	}
	if expectedSlug := core.NormalizeLeaseSlug(req.Lease.Server.Labels["slug"]); expectedSlug != "" && slug != expectedSlug {
		return core.Server{}, core.Exit(4, "Sealos DevBox %q slug identity changed: expected %s, found %s", name, expectedSlug, slug)
	}
	server := b.serverFromDevbox(item)
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, b.cfg, req.State, b.now())
	// TouchDirectLeaseLabels sanitizes values for provider label limits. Keep the
	// authoritative ownership annotations lossless so a touch cannot orphan the
	// resource by truncating its SHA-256 scope fingerprint.
	server.Labels["provider-scope"] = b.claimScopeID()
	server.Labels["provider_scope_id"] = b.claimScopeID()
	server.Labels["provider_scope"] = b.claimScope()
	claim, err := b.revalidateClaimSnapshot(req.Lease.Server, leaseID)
	if err != nil {
		return core.Server{}, err
	}
	if err := b.validateClaimBinding(claim, item); err != nil {
		return core.Server{}, err
	}
	action := func() error {
		return b.patchDevboxAnnotations(ctx, name, item.Metadata.ResourceVersion, annotationsFromLeaseLabels(server.Labels))
	}
	updated, err := core.UpdateLeaseClaimEndpointIfUnchangedAfter(leaseID, claim, server, req.Lease.SSH, action)
	if err != nil {
		return core.Server{}, err
	}
	core.SetServerLeaseClaimSnapshot(&server, updated, true)
	return server, nil
}

func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	items, err := b.listDevboxes(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]core.LeaseView, 0, len(items))
	for _, item := range items {
		if !b.itemMatchesScope(item) {
			continue
		}
		servers = append(servers, b.serverFromDevbox(item))
	}
	return servers, nil
}

func (b *backend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	statusCtx := ctx
	cancel := func() {}
	if req.Wait && req.WaitTimeout > 0 {
		statusCtx, cancel = context.WithTimeout(ctx, req.WaitTimeout)
	}
	defer cancel()
	for {
		view, target, item, err := b.statusView(statusCtx, req.ID)
		if err != nil {
			return core.StatusView{}, err
		}
		if !req.Wait {
			return view, nil
		}
		if view.Ready {
			if err := b.waitForSSH(statusCtx, &target, "Sealos DevBox status"); err != nil {
				return core.StatusView{}, err
			}
			return view, nil
		}
		if devboxTerminalFailure(item) {
			return core.StatusView{}, core.Exit(5, "Sealos DevBox %s reached terminal state before readiness: %s", item.Metadata.Name, view.State)
		}
		if err := sleepContext(statusCtx, b.pollInterval(2*time.Second)); err != nil {
			return core.StatusView{}, err
		}
	}
}

func (b *backend) statusView(ctx context.Context, id string) (core.StatusView, core.SSHTarget, devboxItem, error) {
	item, _, leaseID, slug, err := b.resolveDevbox(ctx, id)
	if err != nil {
		return core.StatusView{}, core.SSHTarget{}, devboxItem{}, err
	}
	server := b.serverFromDevbox(item)
	target, _ := b.sshTarget(item, b.statusSSHKey(leaseID), false)
	return core.StatusView{
		ID:            leaseID,
		Slug:          slug,
		Provider:      providerName,
		TargetOS:      core.TargetLinux,
		State:         devboxStatusLabel(item),
		ServerID:      server.DisplayID(),
		ServerType:    server.ServerType.Name,
		Host:          target.Host,
		Network:       core.NetworkPublic,
		SSHHost:       target.Host,
		SSHUser:       target.User,
		SSHPort:       target.Port,
		SSHKey:        target.Key,
		LastTouchedAt: core.Blank(core.LeaseLabelTimeDisplay(server.Labels["last_touched_at"]), server.Labels["last_touched_at"]),
		IdleFor:       core.IdleForString(server.Labels["last_touched_at"], b.now()),
		IdleTimeout:   core.LeaseLabelDurationDisplay(server.Labels["idle_timeout_secs"], server.Labels["idle_timeout"]),
		ExpiresAt:     core.Blank(core.LeaseLabelTimeDisplay(server.Labels["expires_at"]), server.Labels["expires_at"]),
		Labels:        server.Labels,
		HasHost:       target.Host != "",
		Ready:         devboxReady(item) && target.Host != "",
	}, target, item, nil
}

func (b *backend) statusSSHKey(leaseID string) string {
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		return ""
	}
	return keyPath
}

func (b *backend) waitForDevboxPrepared(ctx context.Context, name string, timeout time.Duration) (devboxItem, error) {
	deadline := b.now().Add(timeout)
	var last devboxItem
	var lastErr error
	for {
		item, err := b.getDevbox(ctx, name)
		if err == nil {
			last = item
			if b.devboxPrepared(item) {
				return item, nil
			}
			if devboxReady(item) {
				lastErr = core.Exit(5, "Sealos DevBox %s is running but has no SSH NodePort in status.network", name)
			}
			if devboxTerminalFailure(item) {
				events, _ := b.listEvents(ctx, name)
				return item, core.Exit(5, "Sealos DevBox %s reached terminal state before readiness: %s", name, devboxDiagnostics(item, events, nil))
			}
		} else if kubectlNotFound(err) {
			lastErr = err
		} else {
			return devboxItem{}, err
		}
		if !b.now().Before(deadline) {
			events, _ := b.listEvents(ctx, name)
			return last, core.Exit(5, "timed out waiting for Sealos DevBox %s readiness: %s", name, devboxDiagnostics(last, events, lastErr))
		}
		if err := sleepContext(ctx, b.pollInterval(5*time.Second)); err != nil {
			return last, err
		}
	}
}

func (b *backend) waitForDevboxSecret(ctx context.Context, item devboxItem, timeout time.Duration) (devboxSecret, error) {
	name := devboxSecretName(item)
	devboxName := strings.TrimSpace(item.Metadata.Name)
	deadline := b.now().Add(timeout)
	var lastErr error
	for {
		secret, err := b.getSecret(ctx, name)
		if err == nil {
			if ownerErr := validateDevboxSecretOwner(secret, item); ownerErr == nil {
				return secret, nil
			} else {
				lastErr = ownerErr
			}
		} else if !kubectlNotFound(err) {
			return devboxSecret{}, err
		} else {
			lastErr = err
		}
		if devboxName != "" {
			refreshed, refreshErr := b.getDevbox(ctx, devboxName)
			if refreshErr == nil {
				item = refreshed
				previousName := name
				name = devboxSecretName(item)
				if name != previousName {
					continue
				}
			} else if !kubectlNotFound(refreshErr) {
				return devboxSecret{}, refreshErr
			} else {
				lastErr = refreshErr
			}
		}
		if !b.now().Before(deadline) {
			return devboxSecret{}, core.Exit(5, "timed out waiting for Sealos DevBox Secret %s: %s", name, redactSensitive(lastErr.Error()))
		}
		if err := sleepContext(ctx, b.pollInterval(5*time.Second)); err != nil {
			return devboxSecret{}, err
		}
	}
}

func (b *backend) devboxPrepared(item devboxItem) bool {
	if !devboxReady(item) {
		return false
	}
	if normalizeNetwork(b.cfg.SealosDevbox.Network) != networkNodePort {
		return true
	}
	_, ok := devboxSSHNodePort(item)
	return ok
}

func (b *backend) pollInterval(fallback time.Duration) time.Duration {
	if b.pollIntervalOverride > 0 {
		return b.pollIntervalOverride
	}
	return fallback
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

func bootstrapWaitTimeout(cfg core.Config) time.Duration {
	return core.BootstrapWaitTimeout(cfg)
}
