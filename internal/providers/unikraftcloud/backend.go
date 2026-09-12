package unikraftcloud

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	statusPollInterval = 250 * time.Millisecond
	defaultWaitTimeout = 5 * time.Minute
)

func newBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt, newClient: newUnikraftCloudClient, pollInterval: statusPollInterval, deleteConfirmationTimeout: 30 * time.Second}
}

type backend struct {
	spec      core.ProviderSpec
	cfg       core.Config
	rt        core.Runtime
	newClient func(core.Config, core.Runtime) (unikraftCloudAPI, error)

	pollInterval              time.Duration
	deleteConfirmationTimeout time.Duration
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

func (b *backend) client() (unikraftCloudAPI, error) {
	if b.newClient != nil {
		return b.newClient(b.cfg, b.rt)
	}
	return newUnikraftCloudClient(b.cfg, b.rt)
}

func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	api, err := b.client()
	if err != nil {
		return core.DoctorResult{}, err
	}
	if _, err := api.UserUUID(ctx); err != nil {
		if isUnauthorized(err) {
			return core.DoctorResult{}, core.Exit(3, "provider=%s API key was rejected; check UKC_TOKEN / UNIKRAFT_CLOUD_API_KEY and the configured metro: %v", providerName, err)
		}
		return core.DoctorResult{}, err
	}
	instances, err := api.ListInstances(ctx)
	if err != nil {
		if isUnauthorized(err) {
			return core.DoctorResult{}, core.Exit(3, "provider=%s API key was rejected; check UKC_TOKEN / UNIKRAFT_CLOUD_API_KEY and the configured metro: %v", providerName, err)
		}
		return core.DoctorResult{}, err
	}
	if _, err := indexUnikraftCloudInventory(instances); err != nil {
		return core.DoctorResult{}, err
	}
	return core.InventoryDoctorResult(providerName, len(instances)), nil
}

// Warmup creates an instance from the configured OCI image and starts it.
// Unikraft Cloud instances run their image entrypoint as a microVM service;
// there is no exec or SSH surface, so warmup is the create-and-claim step and
// stop deletes the instance.
func (b *backend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	if req.ActionsRunner {
		return core.Exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return core.Exit(2, "provider=%s is service-control only and does not support Tailscale options", providerName)
	}
	image := strings.TrimSpace(b.cfg.UnikraftCloud.Image)
	if image == "" {
		return core.Exit(2, "provider=%s warmup requires an OCI image; set --unikraft-cloud-image, UNIKRAFT_CLOUD_IMAGE, or unikraftCloud.image", providerName)
	}
	if b.cfg.UnikraftCloud.MemoryMB < 0 {
		return core.Exit(2, "provider=%s memory must be zero or greater", providerName)
	}
	started := core.ClockNow(b.rt.Clock)
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
	slug, err := core.AllocateClaimLeaseSlug(leaseID, req.RequestedSlug)
	if err != nil {
		unlockSlug()
		return err
	}
	createReq := createInstanceRequest{
		Name:      core.LeaseProviderName(leaseID, ""),
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
			if removeErr := core.RemoveLeaseClaimIfUnchanged(conflict.LeaseID, conflict); removeErr != nil {
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

func (b *backend) preflightCreateIntent(ctx context.Context, api unikraftCloudAPI, preflight core.LeaseClaim) (core.LeaseClaim, error) {
	resourceName := strings.TrimSpace(preflight.Labels[ukcLabelResourceName])
	instances, listErr := api.ListInstances(ctx)
	if listErr == nil {
		_, listErr = indexUnikraftCloudInventory(instances)
	}
	if listErr == nil {
		for _, instance := range instances {
			if instance.Name == resourceName {
				cleanupErr := core.RemoveLeaseClaimIfUnchanged(preflight.LeaseID, preflight)
				conflictErr := core.Exit(4, "%s instance name %q already exists before create; refusing to claim or mutate it", providerName, resourceName)
				if cleanupErr != nil {
					return core.LeaseClaim{}, errors.Join(conflictErr, fmt.Errorf("remove unused preflight claim %s: %w", preflight.LeaseID, cleanupErr))
				}
				return core.LeaseClaim{}, conflictErr
			}
		}
	}
	if listErr != nil {
		if cleanupErr := core.RemoveLeaseClaimIfUnchanged(preflight.LeaseID, preflight); cleanupErr != nil {
			return core.LeaseClaim{}, errors.Join(listErr, fmt.Errorf("remove unused preflight claim %s: %w", preflight.LeaseID, cleanupErr))
		}
		return core.LeaseClaim{}, fmt.Errorf("preflight %s instance inventory: %w", providerName, listErr)
	}
	intent, err := transitionUnikraftCloudCreateState(preflight, ukcStateCreateIntent)
	if err != nil {
		cause := fmt.Errorf("arm %s create intent %s: %w", providerName, preflight.LeaseID, err)
		return core.LeaseClaim{}, discardUnmutatedUnikraftCloudCreateClaim(preflight, cause)
	}
	return intent, nil
}

func (b *backend) finishWarmup(started time.Time, claim core.LeaseClaim, instance ukcInstance, req core.WarmupRequest) error {
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s instance=%s state=%s fqdn=%s\n",
		claim.LeaseID, claim.Slug, providerName, instance.UUID, normalizedInstanceState(instance.State), core.Blank(instanceFQDN(instance), "-"))
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: %s warmup keeps the instance until explicit stop or eligible cleanup\n", providerName)
	}
	total := core.ClockNow(b.rt.Clock).Sub(started)
	return shared.CompleteWarmup(b.rt, req.TimingJSON, shared.WarmupCompletion{
		Provider: providerName,
		LeaseID:  claim.LeaseID,
		Slug:     claim.Slug,
		Total:    total,
	})
}

func (b *backend) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	_ = ctx
	if err := shared.RejectServiceRunOptions(req, providerName, "cannot run commands", "cannot open an interactive shell"); err != nil {
		return core.RunResult{}, err
	}
	if len(req.Command) == 0 {
		return core.RunResult{}, core.Exit(2, "missing command")
	}
	return core.RunResult{}, core.Exit(2, "provider=%s cannot execute arbitrary run commands; Unikraft Cloud instances run their OCI image entrypoint", providerName)
}

func (b *backend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
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
	servers := make([]core.Server, 0, len(instances)+len(claims))
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
			current, exists, readErr := core.ReadLeaseClaimWithPresence(claim.LeaseID)
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
			return nil, core.Exit(5, "%s instance %s is claimed by both %s and %s", providerName, claim.CloudID, previous, claim.LeaseID)
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
			servers = append(servers, unikraftCloudServer(instance, core.LeaseClaim{}))
		}
	}
	return servers, nil
}

func (b *backend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	api, err := b.client()
	if err != nil {
		return core.StatusView{}, err
	}
	accountUUID, err := api.UserUUID(ctx)
	if err != nil {
		return core.StatusView{}, err
	}
	scope, err := unikraftCloudClaimScope(api.BaseURL(), accountUUID)
	if err != nil {
		return core.StatusView{}, err
	}
	claim, claimed, claimErr := b.resolveClaim(req.ID, scope)
	if claimErr != nil {
		var notClaimed *unikraftCloudClaimNotFoundError
		if !errors.As(claimErr, &notClaimed) || !unikraftCloudUUIDPattern.MatchString(strings.TrimSpace(req.ID)) {
			return core.StatusView{}, claimErr
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
				return core.StatusView{}, err
			}
			current, exists, readErr := core.ReadLeaseClaimWithPresence(claim.LeaseID)
			if readErr != nil {
				unlock()
				return core.StatusView{}, readErr
			}
			if !exists {
				unlock()
				return core.StatusView{}, core.Exit(4, "%s lease %s no longer exists", providerName, claim.LeaseID)
			}
			if err := validateUnikraftCloudClaim(current, scope); err != nil {
				unlock()
				return core.StatusView{}, err
			}
			claim = current
			if claim.CloudID == "" {
				claim, _, _, err = b.reconcileCreateIntent(pollCtx, api, current, false)
				if err != nil {
					unlock()
					return core.StatusView{}, err
				}
			}
			unlock()
			if claim.CloudID != "" {
				break
			}
			if !req.Wait {
				return core.StatusView{
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
				return core.StatusView{}, core.Exit(5, "%s lease %s reached non-adoptable state=%s before an instance was created", providerName, claim.LeaseID, state)
			}
			select {
			case <-pollCtx.Done():
				return core.StatusView{}, core.Exit(5, "timed out waiting for %s lease %s create outcome", providerName, claim.LeaseID)
			case <-time.After(pollInterval):
			}
		}
		leaseID = claim.LeaseID
		slug = claim.Slug
		resourceName = claim.Labels[ukcLabelResourceName]
		instanceID = claim.CloudID
	}
	if instanceID == "" {
		return core.StatusView{}, core.Exit(2, "provider=%s requires --id <lease-id, slug, or instance uuid>", providerName)
	}
	for {
		instance, getErr := api.GetInstance(pollCtx, instanceID)
		if getErr != nil {
			if req.Wait && ctx.Err() == nil && pollCtx.Err() != nil {
				return core.StatusView{}, core.Exit(5, "timed out waiting for %s instance %s to become ready", providerName, instanceID)
			}
			if ctx.Err() != nil {
				return core.StatusView{}, ctx.Err()
			}
			return core.StatusView{}, getErr
		}
		expectedName := ""
		expectedUUID := instanceID
		if claimed {
			expectedUUID = claim.CloudID
			expectedName = resourceName
		}
		if err := validateUnikraftCloudInstanceIdentity(instance, expectedUUID, expectedName); err != nil {
			return core.StatusView{}, err
		}
		state := normalizedInstanceState(instance.State)
		labels := unikraftCloudLabels(instance)
		if claimed {
			for key, value := range claim.Labels {
				labels[key] = value
			}
			labels[ukcLabelProviderState] = state
		}
		view := core.StatusView{
			ID:         core.Blank(leaseID, instance.UUID),
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
			return core.StatusView{}, core.Exit(5, "%s instance %s reached terminal state=%s before becoming ready", providerName, instanceID, state)
		}
		select {
		case <-pollCtx.Done():
			if ctx.Err() == nil {
				return core.StatusView{}, core.Exit(5, "timed out waiting for %s instance %s to become ready", providerName, instanceID)
			}
			return core.StatusView{}, pollCtx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func (b *backend) Stop(ctx context.Context, req core.StopRequest) error {
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
	claim, exists, err := core.ReadLeaseClaimWithPresence(snapshot.LeaseID)
	if err != nil {
		return err
	}
	if !exists {
		return core.Exit(4, "%s lease %s no longer exists", providerName, snapshot.LeaseID)
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
		fmt.Fprintf(b.rt.Stderr, "warning: %s instance=%s was already gone; removed local claim\n", providerName, core.Blank(instanceID, "pending"))
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s instance=%s\n", claim.LeaseID, core.Blank(instanceID, "pending"))
	return nil
}

func unikraftCloudServer(instance ukcInstance, claim core.LeaseClaim) core.Server {
	labels := unikraftCloudLabels(instance)
	if claim.LeaseID != "" {
		providerState := labels["state"]
		for key, value := range claim.Labels {
			labels[key] = value
		}
		labels[ukcLabelProviderState] = providerState
	}
	return core.Server{
		CloudID:  instance.UUID,
		Provider: providerName,
		Name:     core.Blank(instance.Name, instance.UUID),
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
	return strings.ToLower(core.Blank(strings.TrimSpace(state), "unknown"))
}
