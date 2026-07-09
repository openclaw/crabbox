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
	defaultWaitTimeout = 5 * time.Minute
)

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt, newClient: newUnikraftCloudClient, pollInterval: statusPollInterval, deleteConfirmationTimeout: 30 * time.Second}
}

type backend struct {
	spec      ProviderSpec
	cfg       Config
	rt        Runtime
	newClient func(Config, Runtime) (unikraftCloudAPI, error)

	pollInterval              time.Duration
	deleteConfirmationTimeout time.Duration
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
	if _, err := api.UserUUID(ctx); err != nil {
		if isUnauthorized(err) {
			return DoctorResult{}, exit(3, "provider=%s API key was rejected; check UKC_TOKEN / UNIKRAFT_CLOUD_API_KEY and the configured metro: %v", providerName, err)
		}
		return DoctorResult{}, err
	}
	instances, err := api.ListInstances(ctx)
	if err != nil {
		if isUnauthorized(err) {
			return DoctorResult{}, exit(3, "provider=%s API key was rejected; check UKC_TOKEN / UNIKRAFT_CLOUD_API_KEY and the configured metro: %v", providerName, err)
		}
		return DoctorResult{}, err
	}
	if _, err := indexUnikraftCloudInventory(instances); err != nil {
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
	if b.cfg.UnikraftCloud.MemoryMB < 0 {
		return exit(2, "provider=%s memory must be zero or greater", providerName)
	}
	started := b.now()
	api, err := b.client()
	if err != nil {
		return err
	}
	accountUUID, err := api.UserUUID(ctx)
	if err != nil {
		return err
	}
	scope, err := unikraftCloudClaimScope(api.BaseURL(), accountUUID)
	if err != nil {
		return err
	}
	leaseID := newLeaseID()
	unlockLease, err := lockUnikraftCloudLeaseOperation(ctx, leaseID)
	if err != nil {
		return err
	}
	defer unlockLease()
	unlockSlug, err := lockUnikraftCloudSlugAllocation(ctx)
	if err != nil {
		return err
	}
	slug, err := allocateClaimLeaseSlug(leaseID, req.RequestedSlug)
	if err != nil {
		unlockSlug()
		return err
	}
	createReq := createInstanceRequest{
		Name:      leaseProviderName(leaseID, ""),
		Image:     image,
		MemoryMB:  b.cfg.UnikraftCloud.MemoryMB,
		Autostart: true,
	}
	intent, err := b.createIntentClaim(leaseID, slug, scope, accountUUID, req, createReq)
	unlockSlug()
	if err != nil {
		return err
	}
	intent, err = b.preflightCreateIntent(ctx, api, intent)
	if err != nil {
		return err
	}
	instance, createErr := api.CreateInstance(ctx, createReq)
	if createErr != nil {
		if definiteUnikraftCloudCreateRejection(createErr) {
			conflict, transitionErr := transitionUnikraftCloudCreateState(intent, ukcStateCreateConflict)
			if transitionErr != nil {
				cause := errors.Join(createErr, fmt.Errorf("%s create rejection could not quarantine recovery claim %s: %w", providerName, leaseID, transitionErr))
				return quarantineRejectedUnikraftCloudCreateClaim(intent, cause)
			}
			proofCtx := context.WithoutCancel(ctx)
			resourceName := conflict.Labels[ukcLabelResourceName]
			if proofErr := b.proveInstanceAbsent(proofCtx, api, resourceName, resourceName); proofErr != nil {
				return errors.Join(createErr, fmt.Errorf("%s create rejection could not prove zero residue; non-adoptable recovery claim %s retained: %w", providerName, leaseID, proofErr))
			}
			if removeErr := removeLeaseClaimIfUnchanged(conflict.LeaseID, conflict); removeErr != nil {
				return errors.Join(createErr, fmt.Errorf("remove rejected %s create claim %s: %w", providerName, leaseID, removeErr))
			}
			return createErr
		}
		reconciled, recovered, _, reconcileErr := b.reconcileCreateIntent(context.WithoutCancel(ctx), api, intent, false)
		if reconcileErr != nil {
			return errors.Join(createErr, fmt.Errorf("%s create outcome is ambiguous; recovery claim %s retained: %w", providerName, leaseID, reconcileErr))
		}
		if recovered != nil {
			return b.finishWarmup(started, reconciled, *recovered, req)
		}
		return fmt.Errorf("%s create outcome is ambiguous; recovery claim %s retained for exact-name reconciliation: %w", providerName, leaseID, createErr)
	}
	ready, err := b.publishReadyClaim(intent, instance)
	if err != nil {
		return fmt.Errorf("%s instance %s was created but ready ownership could not be finalized; recovery claim %s retained: %w", providerName, instance.UUID, leaseID, err)
	}
	return b.finishWarmup(started, ready, instance, req)
}

func (b *backend) preflightCreateIntent(ctx context.Context, api unikraftCloudAPI, preflight LeaseClaim) (LeaseClaim, error) {
	resourceName := strings.TrimSpace(preflight.Labels[ukcLabelResourceName])
	instances, listErr := api.ListInstances(ctx)
	if listErr == nil {
		_, listErr = indexUnikraftCloudInventory(instances)
	}
	if listErr == nil {
		for _, instance := range instances {
			if instance.Name == resourceName {
				cleanupErr := removeLeaseClaimIfUnchanged(preflight.LeaseID, preflight)
				conflictErr := exit(4, "%s instance name %q already exists before create; refusing to claim or mutate it", providerName, resourceName)
				if cleanupErr != nil {
					return LeaseClaim{}, errors.Join(conflictErr, fmt.Errorf("remove unused preflight claim %s: %w", preflight.LeaseID, cleanupErr))
				}
				return LeaseClaim{}, conflictErr
			}
		}
	}
	if listErr != nil {
		if cleanupErr := removeLeaseClaimIfUnchanged(preflight.LeaseID, preflight); cleanupErr != nil {
			return LeaseClaim{}, errors.Join(listErr, fmt.Errorf("remove unused preflight claim %s: %w", preflight.LeaseID, cleanupErr))
		}
		return LeaseClaim{}, fmt.Errorf("preflight %s instance inventory: %w", providerName, listErr)
	}
	intent, err := transitionUnikraftCloudCreateState(preflight, ukcStateCreateIntent)
	if err != nil {
		cause := fmt.Errorf("arm %s create intent %s: %w", providerName, preflight.LeaseID, err)
		return LeaseClaim{}, discardUnmutatedUnikraftCloudCreateClaim(preflight, cause)
	}
	return intent, nil
}

func (b *backend) finishWarmup(started time.Time, claim LeaseClaim, instance ukcInstance, req WarmupRequest) error {
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s instance=%s state=%s fqdn=%s\n",
		claim.LeaseID, claim.Slug, providerName, instance.UUID, normalizedInstanceState(instance.State), blank(instanceFQDN(instance), "-"))
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: %s warmup keeps the instance until explicit stop or eligible cleanup\n", providerName)
	}
	total := b.now().Sub(started)
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", total.Round(time.Millisecond))
	if req.TimingJSON {
		return writeTimingJSON(b.rt.Stderr, timingReport{
			Provider: providerName,
			LeaseID:  claim.LeaseID,
			Slug:     claim.Slug,
			TotalMs:  total.Milliseconds(),
			ExitCode: 0,
		})
	}
	return nil
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
	api, err := b.client()
	if err != nil {
		return nil, err
	}
	accountUUID, err := api.UserUUID(ctx)
	if err != nil {
		return nil, err
	}
	scope, err := unikraftCloudClaimScope(api.BaseURL(), accountUUID)
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
	if err := preflightUnikraftCloudClaimOwnership(claims, scope); err != nil {
		return nil, err
	}
	instanceByUUID, err := indexUnikraftCloudInventory(instances)
	if err != nil {
		return nil, err
	}
	claimedUUIDs := make(map[string]string)
	servers := make([]Server, 0, len(instances)+len(claims))
	for _, snapshot := range claims {
		if snapshot.Provider != providerName || snapshot.ProviderScope != scope {
			continue
		}
		if err := validateUnikraftCloudClaim(snapshot, scope); err != nil {
			return nil, err
		}
		claim := snapshot
		if claim.CloudID == "" {
			unlock, err := lockUnikraftCloudLeaseOperation(ctx, claim.LeaseID)
			if err != nil {
				return nil, err
			}
			current, exists, readErr := readLeaseClaimWithPresence(claim.LeaseID)
			if readErr != nil {
				unlock()
				return nil, readErr
			}
			if !exists {
				unlock()
				continue
			}
			if err := validateUnikraftCloudClaim(current, scope); err != nil {
				unlock()
				return nil, err
			}
			claim, _, err = b.reconcileCreateIntentFromInventory(current, instances)
			unlock()
			if err != nil {
				return nil, err
			}
		}
		if claim.CloudID == "" {
			servers = append(servers, serverFromClaim(claim))
			continue
		}
		instanceKey := strings.ToLower(claim.CloudID)
		if previous, exists := claimedUUIDs[instanceKey]; exists {
			return nil, exit(5, "%s instance %s is claimed by both %s and %s", providerName, claim.CloudID, previous, claim.LeaseID)
		}
		claimedUUIDs[instanceKey] = claim.LeaseID
		instance, exists := instanceByUUID[instanceKey]
		if !exists {
			missing := serverFromClaim(claim)
			missing.Status = "missing"
			servers = append(servers, missing)
			continue
		}
		if err := validateUnikraftCloudInstanceIdentity(instance, claim.CloudID, claim.Labels[ukcLabelResourceName]); err != nil {
			return nil, err
		}
		servers = append(servers, unikraftCloudServer(instance, claim))
	}
	if req.All {
		for _, instance := range instances {
			if _, claimed := claimedUUIDs[strings.ToLower(instance.UUID)]; claimed {
				continue
			}
			servers = append(servers, unikraftCloudServer(instance, LeaseClaim{}))
		}
	}
	return servers, nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	api, err := b.client()
	if err != nil {
		return StatusView{}, err
	}
	accountUUID, err := api.UserUUID(ctx)
	if err != nil {
		return StatusView{}, err
	}
	scope, err := unikraftCloudClaimScope(api.BaseURL(), accountUUID)
	if err != nil {
		return StatusView{}, err
	}
	claim, claimed, claimErr := b.resolveClaim(req.ID, scope)
	if claimErr != nil {
		var notClaimed *unikraftCloudClaimNotFoundError
		if !errors.As(claimErr, &notClaimed) || !unikraftCloudUUIDPattern.MatchString(strings.TrimSpace(req.ID)) {
			return StatusView{}, claimErr
		}
	}
	leaseID := ""
	slug := ""
	instanceID := strings.TrimSpace(req.ID)
	resourceName := ""
	waitTimeout := req.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = defaultWaitTimeout
	}
	pollCtx := ctx
	pollInterval := b.pollInterval
	if pollInterval <= 0 {
		pollInterval = statusPollInterval
	}
	cancel := func() {}
	if req.Wait {
		pollCtx, cancel = context.WithTimeout(ctx, waitTimeout)
	}
	defer cancel()
	if claimed {
		for {
			unlock, err := lockUnikraftCloudLeaseOperation(pollCtx, claim.LeaseID)
			if err != nil {
				return StatusView{}, err
			}
			current, exists, readErr := readLeaseClaimWithPresence(claim.LeaseID)
			if readErr != nil {
				unlock()
				return StatusView{}, readErr
			}
			if !exists {
				unlock()
				return StatusView{}, exit(4, "%s lease %s no longer exists", providerName, claim.LeaseID)
			}
			if err := validateUnikraftCloudClaim(current, scope); err != nil {
				unlock()
				return StatusView{}, err
			}
			claim = current
			if claim.CloudID == "" {
				claim, _, _, err = b.reconcileCreateIntent(pollCtx, api, current, false)
				if err != nil {
					unlock()
					return StatusView{}, err
				}
			}
			unlock()
			if claim.CloudID != "" {
				break
			}
			if !req.Wait {
				return StatusView{
					ID:         claim.LeaseID,
					Slug:       claim.Slug,
					Provider:   providerName,
					TargetOS:   targetLinux,
					State:      claim.Labels["state"],
					ServerType: "unikraft-cloud-instance",
					Network:    networkPublic,
					Labels:     cloneLabels(claim.Labels),
				}, nil
			}
			if state := claim.Labels["state"]; state == ukcStateCreatePreflight || state == ukcStateCreateConflict {
				return StatusView{}, exit(5, "%s lease %s reached non-adoptable state=%s before an instance was created", providerName, claim.LeaseID, state)
			}
			select {
			case <-pollCtx.Done():
				return StatusView{}, exit(5, "timed out waiting for %s lease %s create outcome", providerName, claim.LeaseID)
			case <-time.After(pollInterval):
			}
		}
		leaseID = claim.LeaseID
		slug = claim.Slug
		resourceName = claim.Labels[ukcLabelResourceName]
		instanceID = claim.CloudID
	}
	if instanceID == "" {
		return StatusView{}, exit(2, "provider=%s requires --id <lease-id, slug, or instance uuid>", providerName)
	}
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
		expectedName := ""
		expectedUUID := instanceID
		if claimed {
			expectedUUID = claim.CloudID
			expectedName = resourceName
		}
		if err := validateUnikraftCloudInstanceIdentity(instance, expectedUUID, expectedName); err != nil {
			return StatusView{}, err
		}
		state := normalizedInstanceState(instance.State)
		labels := unikraftCloudLabels(instance)
		if claimed {
			for key, value := range claim.Labels {
				labels[key] = value
			}
			labels[ukcLabelProviderState] = state
		}
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
			Labels:     labels,
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if unikraftCloudTerminalState(state) {
			return StatusView{}, exit(5, "%s instance %s reached terminal state=%s before becoming ready", providerName, instanceID, state)
		}
		select {
		case <-pollCtx.Done():
			if ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for %s instance %s to become ready", providerName, instanceID)
			}
			return StatusView{}, pollCtx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	api, err := b.client()
	if err != nil {
		return err
	}
	accountUUID, err := api.UserUUID(ctx)
	if err != nil {
		return err
	}
	scope, err := unikraftCloudClaimScope(api.BaseURL(), accountUUID)
	if err != nil {
		return err
	}
	snapshot, _, err := b.resolveClaim(req.ID, scope)
	if err != nil {
		return err
	}
	unlock, err := lockUnikraftCloudLeaseOperation(ctx, snapshot.LeaseID)
	if err != nil {
		return err
	}
	defer unlock()
	claim, exists, err := readLeaseClaimWithPresence(snapshot.LeaseID)
	if err != nil {
		return err
	}
	if !exists {
		return exit(4, "%s lease %s no longer exists", providerName, snapshot.LeaseID)
	}
	if err := validateUnikraftCloudClaim(claim, scope); err != nil {
		return err
	}
	if err := verifyUnikraftCloudClaimSnapshot(snapshot, claim); err != nil {
		return err
	}
	claims, err := listUnikraftCloudLeaseClaims()
	if err != nil {
		return err
	}
	if err := preflightUnikraftCloudClaimOwnership(claims, scope); err != nil {
		return err
	}
	instanceID := claim.CloudID
	missing, err := b.deleteClaimedInstance(ctx, api, claim)
	if err != nil {
		return err
	}
	if missing {
		fmt.Fprintf(b.rt.Stderr, "warning: %s instance=%s was already gone; removed local claim\n", providerName, blank(instanceID, "pending"))
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s instance=%s\n", claim.LeaseID, blank(instanceID, "pending"))
	return nil
}

func unikraftCloudServer(instance ukcInstance, claim LeaseClaim) Server {
	labels := unikraftCloudLabels(instance)
	if claim.LeaseID != "" {
		providerState := labels["state"]
		for key, value := range claim.Labels {
			labels[key] = value
		}
		labels[ukcLabelProviderState] = providerState
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
