package sealosdevbox

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type backend struct {
	spec     core.ProviderSpec
	cfg      core.Config
	rt       core.Runtime
	sshReady func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error
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
	if _, err := b.kubectl(ctx, nil, false, "get", "customresourcedefinition", devboxCRD, "-o", "jsonpath={.spec.versions[*].name}"); err != nil {
		add(doctorCheck("failed", "crd.devboxes", err.Error(), map[string]string{"groupVersion": devboxGroupVersion}))
		return doctorResult(checks), nil
	}
	add(doctorCheck("ok", "crd.devboxes", "found", map[string]string{"groupVersion": devboxGroupVersion}))
	for _, resource := range []string{devboxResource, "secrets", "pods", "events"} {
		for _, verb := range []string{"get", "list"} {
			add(b.canI(ctx, verb, resource))
		}
	}
	for _, verb := range []string{"create", "update", "delete"} {
		add(b.canI(ctx, verb, devboxResource))
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
	if err := b.applyDevbox(ctx, manifest); err != nil {
		return core.LeaseTarget{}, err
	}
	applied := true
	keyPersisted := false
	claimPersisted := false
	forceRollback := false
	defer func() {
		if err == nil || (req.Keep && !forceRollback) || !applied {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cleanupErr := b.deleteDevbox(cleanupCtx, name); cleanupErr != nil {
			if b.rt.Stderr != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: failed to delete Sealos DevBox %s after acquire failure for lease %s: %v\n", name, leaseID, cleanupErr)
			}
			return
		}
		if claimPersisted {
			core.RemoveLeaseClaim(leaseID)
		}
		if keyPersisted {
			core.RemoveStoredTestboxKey(leaseID)
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
	secret, err := b.waitForDevboxSecret(ctx, item, bootstrapWaitTimeout(b.cfg))
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keys, err := parseDevboxSecretKeys(secret)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, err = persistDevboxKey(leaseID, keys)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPersisted = true
	if err := b.claimLeaseForRepo(leaseID, slug, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
		return core.LeaseTarget{}, err
	}
	claimPersisted = true
	if err := b.waitForSSH(ctx, &target, "Sealos DevBox SSH"); err != nil {
		events, _ := b.listEvents(ctx, name)
		return core.LeaseTarget{}, core.Exit(5, "%v; Sealos DevBox diagnostics: %s", err, devboxDiagnostics(item, events, nil))
	}
	if err := core.UpdateLeaseClaimEndpoint(leaseID, server, target); err != nil {
		return core.LeaseTarget{}, err
	}
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	item, _, leaseID, slug, err := b.resolveDevbox(ctx, req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	server := b.serverFromDevbox(item)
	target := core.SSHTarget{}
	if !req.ReleaseOnly {
		resolved, err := b.sshTarget(item, b.statusSSHKey(leaseID), false)
		if err != nil {
			if !req.StatusOnly {
				return core.LeaseTarget{}, err
			}
		} else {
			target = resolved
		}
	}
	if !req.StatusOnly && !req.ReleaseOnly {
		if req.NoLocalStateMutations {
			return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
		}
		item, server, err = b.resumeDevboxIfPaused(ctx, item, server)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		secret, err := b.getSecret(ctx, devboxSecretName(item))
		if err != nil {
			return core.LeaseTarget{}, err
		}
		keys, err := parseDevboxSecretKeys(secret)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		keyPath, err := persistDevboxKey(leaseID, keys)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		target, err = b.sshTarget(item, keyPath, true)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if req.Repo.Root != "" && !req.NoLocalStateMutations {
			if err := b.claimLeaseForRepo(leaseID, slug, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
				return core.LeaseTarget{}, err
			}
		}
		if err := b.waitForSSH(ctx, &target, "Sealos DevBox SSH"); err != nil {
			events, _ := b.listEvents(ctx, item.Metadata.Name)
			return core.LeaseTarget{}, core.Exit(5, "%v; Sealos DevBox diagnostics: %s", err, devboxDiagnostics(item, events, nil))
		}
		if req.Repo.Root != "" && !req.NoLocalStateMutations {
			if err := core.UpdateLeaseClaimEndpoint(leaseID, server, target); err != nil {
				return core.LeaseTarget{}, err
			}
		}
	}
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *backend) resumeDevboxIfPaused(ctx context.Context, item devboxItem, server core.Server) (devboxItem, core.Server, error) {
	if normalizeDevboxState(item) != devboxStatePaused {
		return item, server, nil
	}
	name := strings.TrimSpace(item.Metadata.Name)
	if name == "" {
		return item, server, core.Exit(5, "Sealos DevBox has no metadata.name")
	}
	if err := b.patchDevboxState(ctx, name, devboxStateRun, nil); err != nil {
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
	lease, err := b.Resolve(ctx, core.ResolveRequest{ID: id, StatusOnly: true})
	if err != nil {
		return core.Server{}, err
	}
	server := lease.Server
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, b.cfg, req.State, b.now())
	_ = core.UpdateLeaseClaimEndpoint(lease.LeaseID, server, lease.SSH)
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
		if err := sleepContext(statusCtx, 2*time.Second); err != nil {
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
			if devboxReady(item) {
				return item, nil
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
		if err := sleepContext(ctx, 5*time.Second); err != nil {
			return last, err
		}
	}
}

func (b *backend) waitForDevboxSecret(ctx context.Context, item devboxItem, timeout time.Duration) (devboxSecret, error) {
	name := devboxSecretName(item)
	deadline := b.now().Add(timeout)
	var lastErr error
	for {
		secret, err := b.getSecret(ctx, name)
		if err == nil {
			return secret, nil
		}
		if !kubectlNotFound(err) {
			return devboxSecret{}, err
		}
		lastErr = err
		if !b.now().Before(deadline) {
			return devboxSecret{}, core.Exit(5, "timed out waiting for Sealos DevBox Secret %s: %s", name, redactSensitive(lastErr.Error()))
		}
		if err := sleepContext(ctx, 5*time.Second); err != nil {
			return devboxSecret{}, err
		}
	}
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
