package unikraftcloud

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	statusPollInterval = 250 * time.Millisecond
	cleanupTimeout     = 15 * time.Second
	defaultWaitTimeout = 5 * time.Minute
)

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt, newClient: newUnikraftCloudClient}
}

type backend struct {
	spec      ProviderSpec
	cfg       Config
	rt        Runtime
	newClient func(Config, Runtime) (unikraftCloudAPI, error)
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) client() (unikraftCloudAPI, error) {
	if b.newClient != nil {
		return b.newClient(b.cfg, b.rt)
	}
	return newUnikraftCloudClient(b.cfg, b.rt)
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	api, err := b.client()
	if err != nil {
		return DoctorResult{}, err
	}
	instances, err := api.ListInstances(ctx)
	if err != nil {
		if isUnauthorized(err) {
			return DoctorResult{}, exit(3, "provider=%s API key was rejected; check UKC_TOKEN / UNIKRAFT_CLOUD_API_KEY and the configured metro: %v", providerName, err)
		}
		return DoctorResult{}, err
	}
	return inventoryDoctorResult(providerName, len(instances)), nil
}

// Warmup creates an instance from the configured OCI image and starts it.
// Unikraft Cloud instances run their image entrypoint as a microVM service;
// there is no exec or SSH surface, so warmup is the create-and-claim step and
// stop deletes the instance.
func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return exit(2, "provider=%s is service-control only and does not support Tailscale options", providerName)
	}
	image := strings.TrimSpace(b.cfg.UnikraftCloud.Image)
	if image == "" {
		return exit(2, "provider=%s warmup requires an OCI image; set --unikraft-cloud-image, UNIKRAFT_CLOUD_IMAGE, or unikraftCloud.image", providerName)
	}
	started := b.now()
	api, err := b.client()
	if err != nil {
		return err
	}
	instance, err := api.CreateInstance(ctx, createInstanceRequest{
		Image:     image,
		MemoryMB:  b.cfg.UnikraftCloud.MemoryMB,
		Autostart: true,
	})
	if err != nil {
		return err
	}
	leaseID := leasePrefix + instance.UUID
	slug, err := allocateClaimLeaseSlug(leaseID, req.RequestedSlug)
	if err != nil {
		return b.cleanupCreateFailure(ctx, api, instance.UUID, err)
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, claimScope(api.BaseURL()), b.cfg.Pond, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
		return b.cleanupCreateFailure(ctx, api, instance.UUID, err)
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s instance=%s state=%s fqdn=%s\n",
		leaseID, slug, providerName, instance.UUID, normalizedInstanceState(instance.State), blank(instanceFQDN(instance), "-"))
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: %s warmup keeps the instance until explicit stop\n", providerName)
	}
	total := b.now().Sub(started)
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", total.Round(time.Millisecond))
	if req.TimingJSON {
		return writeTimingJSON(b.rt.Stderr, timingReport{
			Provider: providerName,
			LeaseID:  leaseID,
			Slug:     slug,
			TotalMs:  total.Milliseconds(),
			ExitCode: 0,
		})
	}
	return nil
}

func (b *backend) cleanupCreateFailure(ctx context.Context, api unikraftCloudAPI, instanceID string, cause error) error {
	if strings.TrimSpace(instanceID) == "" {
		return cause
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := api.DeleteInstance(cleanupCtx, instanceID); err != nil && !isNotFound(err) {
		return errors.Join(cause, fmt.Errorf("%s cleanup failed for instance %s; delete it with the Unikraft Cloud console or kraft CLI: %w", providerName, instanceID, err))
	}
	return cause
}

func (b *backend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	_ = ctx
	if err := rejectUnikraftCloudRunOptions(req); err != nil {
		return RunResult{}, err
	}
	if len(req.Command) == 0 {
		return RunResult{}, exit(2, "missing command")
	}
	return RunResult{}, exit(2, "provider=%s cannot execute arbitrary run commands; Unikraft Cloud instances run their OCI image entrypoint", providerName)
}

func (b *backend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	api, err := b.client()
	if err != nil {
		return nil, err
	}
	instances, err := api.ListInstances(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := listUnikraftCloudLeaseClaims()
	if err != nil {
		return nil, err
	}
	scope := claimScope(api.BaseURL())
	claimByInstance := map[string]LeaseClaim{}
	for _, claim := range claims {
		if claim.Provider == providerName && claim.ProviderScope == scope {
			claimByInstance[instanceIDFromLease(claim.LeaseID)] = claim
		}
	}
	servers := make([]Server, 0, len(instances))
	for _, instance := range instances {
		servers = append(servers, unikraftCloudServer(instance, claimByInstance[instance.UUID]))
	}
	return servers, nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	api, err := b.client()
	if err != nil {
		return StatusView{}, err
	}
	leaseID, instanceID, slug, err := b.resolveInstanceID(req.ID, api.BaseURL())
	if err != nil {
		return StatusView{}, err
	}
	waitTimeout := req.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = defaultWaitTimeout
	}
	pollCtx := ctx
	cancel := func() {}
	if req.Wait {
		pollCtx, cancel = context.WithTimeout(ctx, waitTimeout)
	}
	defer cancel()
	for {
		instance, getErr := api.GetInstance(pollCtx, instanceID)
		if getErr != nil {
			if req.Wait && ctx.Err() == nil && pollCtx.Err() != nil {
				return StatusView{}, exit(5, "timed out waiting for %s instance %s to become ready", providerName, instanceID)
			}
			if ctx.Err() != nil {
				return StatusView{}, ctx.Err()
			}
			return StatusView{}, getErr
		}
		state := normalizedInstanceState(instance.State)
		view := StatusView{
			ID:         blank(leaseID, instance.UUID),
			Slug:       slug,
			Provider:   providerName,
			TargetOS:   targetLinux,
			State:      state,
			ServerID:   instance.UUID,
			ServerType: "unikraft-cloud-instance",
			Host:       instanceFQDN(instance),
			Network:    networkPublic,
			Ready:      state == "running",
			Labels:     unikraftCloudLabels(instance),
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		select {
		case <-pollCtx.Done():
			if ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for %s instance %s to become ready", providerName, instanceID)
			}
			return StatusView{}, pollCtx.Err()
		case <-time.After(statusPollInterval):
		}
	}
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	api, err := b.client()
	if err != nil {
		return err
	}
	leaseID, instanceID, _, err := b.resolveClaimedInstanceID(req.ID, api.BaseURL())
	if err != nil {
		return err
	}
	missing := false
	if _, err := api.StopInstance(ctx, instanceID); err != nil {
		if isNotFound(err) {
			missing = true
		} else {
			return err
		}
	}
	if !missing {
		if err := api.DeleteInstance(ctx, instanceID); err != nil {
			if !isNotFound(err) {
				return err
			}
			missing = true
		}
	}
	if missing {
		fmt.Fprintf(b.rt.Stderr, "warning: %s instance=%s was already gone; removing local claim\n", providerName, instanceID)
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s instance=%s\n", leaseID, instanceID)
	return nil
}

// resolveInstanceID accepts a Crabbox lease id, slug, or raw instance
// UUID/name. Read paths (status) fall through to the raw identifier; a raw id
// yields no lease binding.
func (b *backend) resolveInstanceID(id, baseURL string) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", exit(2, "provider=%s requires --id <lease-id, slug, or instance uuid>", providerName)
	}
	leaseID, instanceID, slug, err := b.resolveClaimedInstanceID(id, baseURL)
	if err == nil {
		return leaseID, instanceID, slug, nil
	}
	var exitErr ExitError
	if errors.As(err, &exitErr) && exitErr.Code == 4 {
		// Not claimed locally: treat the identifier as a raw instance UUID/name.
		return "", id, "", nil
	}
	return "", "", "", err
}

// resolveClaimedInstanceID requires a Crabbox-created local claim. Stop uses
// it so Crabbox never deletes instances it does not own.
func (b *backend) resolveClaimedInstanceID(id, baseURL string) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", exit(2, "provider=%s requires --id <lease-id or slug>", providerName)
	}
	scope := claimScope(baseURL)
	exactLeaseID := id
	if !strings.HasPrefix(exactLeaseID, leasePrefix) {
		exactLeaseID = leasePrefix + exactLeaseID
	}
	if claim, err := readLeaseClaim(exactLeaseID); err == nil && claim.LeaseID == exactLeaseID && claim.Provider == providerName {
		return finishResolvedClaim(claim, scope)
	}
	claims, err := listUnikraftCloudLeaseClaims()
	if err != nil {
		return "", "", "", err
	}
	slug := normalizeLeaseSlug(id)
	for _, claim := range claims {
		if claim.Provider != providerName {
			continue
		}
		if claim.LeaseID == id || normalizeLeaseSlug(claim.Slug) == slug {
			return finishResolvedClaim(claim, scope)
		}
	}
	return "", "", "", exit(4, "%s instance %q is not claimed by Crabbox; warmup creates claimed instances, or use the Unikraft Cloud console or kraft CLI for unmanaged instances", providerName, id)
}

func finishResolvedClaim(claim LeaseClaim, scope string) (string, string, string, error) {
	if claim.ProviderScope != scope {
		return "", "", "", exit(4, "%s lease %q belongs to a different API endpoint or metro", providerName, claim.LeaseID)
	}
	slug := claim.Slug
	if strings.TrimSpace(slug) == "" {
		slug = newLeaseSlug(claim.LeaseID)
	}
	return claim.LeaseID, instanceIDFromLease(claim.LeaseID), slug, nil
}

func claimScope(baseURL string) string {
	return "endpoint:" + strings.TrimSpace(baseURL)
}

func instanceIDFromLease(leaseID string) string {
	return strings.TrimPrefix(leaseID, leasePrefix)
}

func unikraftCloudServer(instance ukcInstance, claim LeaseClaim) Server {
	labels := unikraftCloudLabels(instance)
	if claim.LeaseID != "" {
		labels["lease"] = claim.LeaseID
		if strings.TrimSpace(claim.Slug) != "" {
			labels["slug"] = claim.Slug
		}
	}
	return Server{
		CloudID:  instance.UUID,
		Provider: providerName,
		Name:     blank(instance.Name, instance.UUID),
		Status:   normalizedInstanceState(instance.State),
		Labels:   labels,
	}
}

func unikraftCloudLabels(instance ukcInstance) map[string]string {
	labels := map[string]string{
		"provider": providerName,
		"target":   targetLinux,
		"state":    normalizedInstanceState(instance.State),
	}
	addLabel(labels, "fqdn", instanceFQDN(instance))
	addLabel(labels, "privateFqdn", instance.PrivateFQDN)
	addLabel(labels, "createdAt", instance.CreatedAt)
	if instance.MemoryMB > 0 {
		labels["memoryMB"] = fmt.Sprint(instance.MemoryMB)
	}
	if len(instance.NetworkInterfaces) > 0 {
		addLabel(labels, "privateIp", instance.NetworkInterfaces[0].PrivateIP)
	}
	return labels
}

func addLabel(labels map[string]string, key, value string) {
	if strings.TrimSpace(value) != "" {
		labels[key] = value
	}
}

func instanceFQDN(instance ukcInstance) string {
	if instance.ServiceGroup != nil {
		for _, domain := range instance.ServiceGroup.Domains {
			if strings.TrimSpace(domain.FQDN) != "" {
				return domain.FQDN
			}
		}
	}
	return ""
}

func normalizedInstanceState(state string) string {
	return strings.ToLower(blank(strings.TrimSpace(state), "unknown"))
}

func rejectUnikraftCloudRunOptions(req RunRequest) error {
	if req.Keep {
		return exit(2, "provider=%s cannot run commands; --keep is not supported", providerName)
	}
	if req.Reclaim {
		return exit(2, "provider=%s cannot run commands; --reclaim is not supported", providerName)
	}
	if !req.NoSync {
		return exit(2, "provider=%s does not support workspace sync; pass --no-sync", providerName)
	}
	if req.SyncOnly {
		return exit(2, "provider=%s does not support sync; --sync-only is rejected", providerName)
	}
	if req.ChecksumSync {
		return exit(2, "provider=%s does not support sync; --checksum is rejected", providerName)
	}
	if req.ForceSyncLarge {
		return exit(2, "provider=%s does not support sync; --force-sync-large is rejected", providerName)
	}
	if req.FullResync {
		return exit(2, "provider=%s does not support sync; --full-resync is rejected", providerName)
	}
	if req.ShellMode {
		return exit(2, "provider=%s cannot open an interactive shell; --shell is not supported", providerName)
	}
	if req.EnvSummary {
		return exit(2, "provider=%s cannot forward per-run environment variables", providerName)
	}
	return nil
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}
