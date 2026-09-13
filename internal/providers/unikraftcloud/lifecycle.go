package unikraftcloud

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	ukcStateCreatePreflight = "create-preflight"
	ukcStateCreateIntent    = "create-intent"
	ukcStateCreateConflict  = "create-conflict"
	ukcStateReady           = "ready"
	ukcStateDeleteAttempt   = "delete-attempt"
	ukcStateDeleteAccepted  = "delete-accepted"

	ukcLabelResourceName  = "resource_name"
	ukcLabelRequestHash   = "create_request_sha256"
	ukcLabelInstanceUUID  = "instance_uuid"
	ukcLabelAccountUUID   = "account_uuid"
	ukcLabelProviderState = "provider_state"
)

func unikraftCloudClaimScope(baseURL, accountUUID string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	accountUUID = strings.TrimSpace(accountUUID)
	if baseURL == "" {
		return "", core.Exit(2, "provider=%s API endpoint is unavailable", providerName)
	}
	if !unikraftCloudUUIDPattern.MatchString(accountUUID) {
		return "", core.Exit(3, "provider=%s account identity is unavailable", providerName)
	}
	return "endpoint:" + baseURL + "|account:" + accountUUID, nil
}

func unikraftCloudCreateRequestHash(req createInstanceRequest) string {
	value := req.Name + "\x00" + req.Image + "\x00" + fmt.Sprint(req.MemoryMB) + "\x00" + fmt.Sprint(req.Autostart)
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:])
}

func cloneLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		out[key] = value
	}
	return out
}

func (b *backend) createIntentClaim(leaseID, slug, scope, accountUUID string, req core.WarmupRequest, createReq createInstanceRequest) (core.LeaseClaim, error) {
	labels := directLeaseLabels(b.cfg, leaseID, slug, req.Keep, core.ClockNow(b.rt.Clock))
	labels["state"] = ukcStateCreatePreflight
	labels[ukcLabelResourceName] = createReq.Name
	labels[ukcLabelRequestHash] = unikraftCloudCreateRequestHash(createReq)
	labels[ukcLabelAccountUUID] = accountUUID
	intent, err := claimLeaseTargetForRepoConfigScopeIfUnchangedDurable(
		leaseID,
		slug,
		b.cfg,
		scope, core.Server{Provider: providerName, Name: createReq.Name, Status: ukcStateCreatePreflight, Labels: labels}, req.Repo.Root,
		b.cfg.IdleTimeout,
		req.Reclaim, core.LeaseClaim{}, false,
	)
	if err != nil {
		cause := fmt.Errorf("persist %s create preflight %s: %w", providerName, leaseID, err)
		if intent.LeaseID != "" {
			return core.LeaseClaim{}, discardUnmutatedUnikraftCloudCreateClaim(intent, cause)
		}
		return core.LeaseClaim{}, cause
	}
	return intent, nil
}

func transitionUnikraftCloudCreateState(claim core.LeaseClaim, state string) (core.LeaseClaim, error) {
	if claim.CloudID != "" {
		return core.LeaseClaim{}, core.Exit(5, "%s lease %s is already bound to instance %s", providerName, claim.LeaseID, claim.CloudID)
	}
	updated := claim
	updated.Labels = cloneLabels(claim.Labels)
	updated.Labels["state"] = state
	written, err := replaceLeaseClaimIfUnchangedDurable(claim.LeaseID, claim, updated)
	if err != nil {
		if written.Revision == "" {
			return core.LeaseClaim{}, err
		}
		current, exists, readErr := core.ReadLeaseClaimWithPresence(claim.LeaseID)
		if readErr != nil {
			return core.LeaseClaim{}, errors.Join(err, readErr)
		}
		if !exists || !reflect.DeepEqual(current, written) {
			return core.LeaseClaim{}, err
		}
		retryWritten, retryErr := replaceLeaseClaimIfUnchangedDurable(claim.LeaseID, current, current)
		if retryErr != nil {
			return core.LeaseClaim{}, errors.Join(err, retryErr)
		}
		return retryWritten, nil
	}
	return written, nil
}

func sameUnikraftCloudCreateIdentity(left, right core.LeaseClaim) bool {
	return left.LeaseID == right.LeaseID &&
		left.Provider == right.Provider &&
		left.ProviderScope == right.ProviderScope && core.NormalizeLeaseSlug(left.Slug) == core.NormalizeLeaseSlug(right.Slug) &&
		left.Labels[ukcLabelResourceName] == right.Labels[ukcLabelResourceName] &&
		left.Labels[ukcLabelRequestHash] == right.Labels[ukcLabelRequestHash] &&
		left.Labels[ukcLabelAccountUUID] == right.Labels[ukcLabelAccountUUID]
}

func discardUnmutatedUnikraftCloudCreateClaim(expected core.LeaseClaim, cause error) error {
	current, exists, readErr := core.ReadLeaseClaimWithPresence(expected.LeaseID)
	if readErr != nil {
		return errors.Join(cause, readErr)
	}
	if !exists {
		return cause
	}
	if current.CloudID != "" || !sameUnikraftCloudCreateIdentity(current, expected) {
		return errors.Join(cause, fmt.Errorf("%s preflight claim %s changed; refusing cleanup", providerName, expected.LeaseID))
	}
	state := current.Labels["state"]
	if state != ukcStateCreatePreflight && state != ukcStateCreateIntent {
		return cause
	}
	removeErr := core.RemoveLeaseClaimIfUnchanged(current.LeaseID, current)
	if removeErr == nil {
		return cause
	}
	after, afterExists, afterErr := core.ReadLeaseClaimWithPresence(current.LeaseID)
	if afterErr != nil {
		return errors.Join(cause, removeErr, afterErr)
	}
	if !afterExists || after.Labels["state"] != ukcStateCreateIntent || !sameUnikraftCloudCreateIdentity(after, expected) {
		return errors.Join(cause, removeErr)
	}
	_, quarantineErr := transitionUnikraftCloudCreateState(after, ukcStateCreateConflict)
	return errors.Join(cause, removeErr, quarantineErr)
}

func quarantineRejectedUnikraftCloudCreateClaim(expected core.LeaseClaim, cause error) error {
	current, exists, readErr := core.ReadLeaseClaimWithPresence(expected.LeaseID)
	if readErr != nil {
		return errors.Join(cause, readErr)
	}
	if !exists {
		return errors.Join(cause, fmt.Errorf("%s rejected create recovery claim %s is missing", providerName, expected.LeaseID))
	}
	if current.CloudID != "" || !sameUnikraftCloudCreateIdentity(current, expected) {
		return errors.Join(cause, fmt.Errorf("%s rejected create recovery claim %s changed; refusing cleanup", providerName, expected.LeaseID))
	}
	if current.Labels["state"] == ukcStateCreateConflict {
		if _, err := replaceLeaseClaimIfUnchangedDurable(current.LeaseID, current, current); err != nil {
			return errors.Join(cause, err)
		}
		return cause
	}
	if current.Labels["state"] != ukcStateCreateIntent && current.Labels["state"] != ukcStateCreatePreflight {
		return errors.Join(cause, fmt.Errorf("%s rejected create recovery claim %s has unsafe state %q", providerName, expected.LeaseID, current.Labels["state"]))
	}
	_, quarantineErr := transitionUnikraftCloudCreateState(current, ukcStateCreateConflict)
	if quarantineErr == nil {
		return cause
	}
	after, afterExists, afterErr := core.ReadLeaseClaimWithPresence(current.LeaseID)
	if afterErr != nil {
		return errors.Join(cause, quarantineErr, afterErr)
	}
	if afterExists && after.Labels["state"] == ukcStateCreateConflict && sameUnikraftCloudCreateIdentity(after, expected) {
		return errors.Join(cause, quarantineErr)
	}
	return errors.Join(cause, quarantineErr, fmt.Errorf("%s rejected create recovery claim %s remains adoptable", providerName, expected.LeaseID))
}

func validateUnikraftCloudReadyClaimReadback(intent, current core.LeaseClaim, instance ukcInstance) error {
	if err := validateUnikraftCloudClaim(current, intent.ProviderScope); err != nil {
		return err
	}
	if !sameUnikraftCloudCreateIdentity(current, intent) {
		return core.Exit(4, "%s lease %q ready claim changed recovery identity", providerName, intent.LeaseID)
	}
	if current.Labels["state"] != ukcStateReady || current.CloudID != instance.UUID || current.Labels[ukcLabelResourceName] != instance.Name {
		return core.Exit(4, "%s lease %q does not contain the expected ready binding", providerName, intent.LeaseID)
	}
	return nil
}

func (b *backend) publishReadyClaim(intent core.LeaseClaim, instance ukcInstance) (core.LeaseClaim, error) {
	resourceName := strings.TrimSpace(intent.Labels[ukcLabelResourceName])
	if err := validateUnikraftCloudInstanceIdentity(instance, strings.TrimSpace(instance.UUID), resourceName); err != nil {
		return core.LeaseClaim{}, err
	}
	labels := cloneLabels(intent.Labels)
	labels["state"] = ukcStateReady
	labels[ukcLabelInstanceUUID] = instance.UUID
	labels[ukcLabelProviderState] = normalizedInstanceState(instance.State)
	ready, err := claimLeaseTargetForRepoConfigScopeIfUnchangedDurable(
		intent.LeaseID,
		intent.Slug,
		b.cfg,
		intent.ProviderScope, core.Server{
			CloudID:  instance.UUID,
			Provider: providerName,
			Name:     instance.Name,
			Status:   normalizedInstanceState(instance.State),
			Labels:   labels,
		}, intent.RepoRoot,
		time.Duration(intent.IdleTimeoutSeconds)*time.Second,
		false,
		intent,
		true,
	)
	if err != nil {
		return b.reconcileReadyClaimWrite(intent, instance, err)
	}
	return ready, nil
}

func (b *backend) reconcileReadyClaimWrite(intent core.LeaseClaim, instance ukcInstance, writeErr error) (core.LeaseClaim, error) {
	current, exists, readErr := core.ReadLeaseClaimWithPresence(intent.LeaseID)
	if readErr != nil {
		return core.LeaseClaim{}, errors.Join(writeErr, readErr)
	}
	if exists {
		if err := validateUnikraftCloudReadyClaimReadback(intent, current, instance); err != nil {
			return core.LeaseClaim{}, errors.Join(writeErr, err)
		}
		written, err := replaceLeaseClaimIfUnchangedDurable(intent.LeaseID, current, current)
		if err != nil {
			return core.LeaseClaim{}, errors.Join(writeErr, err)
		}
		return written, nil
	}
	unlockSlug, lockErr := lockUnikraftCloudSlugAllocation(context.Background())
	if lockErr != nil {
		return core.LeaseClaim{}, errors.Join(writeErr, fmt.Errorf("reserve restored %s lease slug: %w", providerName, lockErr))
	}
	recoveredSlug, slugErr := core.AllocateClaimLeaseSlug(intent.LeaseID, intent.Slug)
	if slugErr != nil {
		unlockSlug()
		return core.LeaseClaim{}, errors.Join(writeErr, fmt.Errorf("reserve restored %s lease slug: %w", providerName, slugErr))
	}
	labels := cloneLabels(intent.Labels)
	labels["slug"] = recoveredSlug
	labels["state"] = ukcStateReady
	labels[ukcLabelInstanceUUID] = instance.UUID
	labels[ukcLabelProviderState] = normalizedInstanceState(instance.State)
	recovered, recoverErr := claimLeaseTargetForRepoConfigScopeIfUnchangedDurable(
		intent.LeaseID,
		recoveredSlug,
		b.cfg,
		intent.ProviderScope, core.Server{CloudID: instance.UUID, Provider: providerName, Name: instance.Name, Status: normalizedInstanceState(instance.State), Labels: labels}, intent.RepoRoot,
		time.Duration(intent.IdleTimeoutSeconds)*time.Second,
		false, core.LeaseClaim{}, false,
	)
	unlockSlug()
	if recoverErr != nil {
		return core.LeaseClaim{}, errors.Join(writeErr, fmt.Errorf("restore known %s ownership: %w", providerName, recoverErr))
	}
	return recovered, nil
}

func validateUnikraftCloudClaim(claim core.LeaseClaim, scope string) error {
	if claim.LeaseID == "" || !strings.HasPrefix(claim.LeaseID, leasePrefix) {
		return core.Exit(4, "%s claim has an invalid lease identity", providerName)
	}
	if claim.Provider != providerName {
		return core.Exit(4, "%s lease %q belongs to provider=%s", providerName, claim.LeaseID, claim.Provider)
	}
	if claim.ProviderScope != scope {
		return core.Exit(4, "%s lease %q belongs to a different API endpoint or account", providerName, claim.LeaseID)
	}
	if claim.Labels["provider"] != providerName || claim.Labels["lease"] != claim.LeaseID || core.NormalizeLeaseSlug(claim.Labels["slug"]) != core.NormalizeLeaseSlug(claim.Slug) {
		return core.Exit(4, "%s lease %q ownership labels do not match its local claim", providerName, claim.LeaseID)
	}
	resourceName := claim.Labels[ukcLabelResourceName]
	accountUUID := claim.Labels[ukcLabelAccountUUID]
	if strings.TrimSpace(resourceName) == "" || strings.TrimSpace(accountUUID) == "" || strings.TrimSpace(claim.Labels[ukcLabelRequestHash]) == "" {
		return core.Exit(4, "%s lease %q has incomplete recovery identity", providerName, claim.LeaseID)
	}
	if resourceName != core.LeaseProviderName(claim.LeaseID, "") {
		return core.Exit(4, "%s lease %q has an unexpected recovery resource name", providerName, claim.LeaseID)
	}
	if accountUUID != unikraftCloudScopeAccountUUID(scope) {
		return core.Exit(4, "%s lease %q account identity does not match its local claim scope", providerName, claim.LeaseID)
	}
	if claim.CloudID != "" {
		if !unikraftCloudUUIDPattern.MatchString(claim.CloudID) {
			return core.Exit(4, "%s lease %q has an invalid instance UUID", providerName, claim.LeaseID)
		}
		if claim.Labels[ukcLabelInstanceUUID] != claim.CloudID {
			return core.Exit(4, "%s lease %q has a mismatched instance binding", providerName, claim.LeaseID)
		}
	}
	return nil
}

func verifyUnikraftCloudClaimSnapshot(snapshot, current core.LeaseClaim) error {
	if !reflect.DeepEqual(snapshot, current) {
		return core.Exit(4, "%s lease %q changed while waiting for its operation lock; retry with the current lease identity", providerName, snapshot.LeaseID)
	}
	return nil
}

func validateUnikraftCloudInstanceIdentity(instance ukcInstance, expectedUUID, expectedName string) error {
	if !unikraftCloudUUIDPattern.MatchString(instance.UUID) {
		return core.Exit(5, "%s instance response has an invalid UUID", providerName)
	}
	if expectedUUID != "" && !strings.EqualFold(instance.UUID, expectedUUID) {
		return core.Exit(5, "%s instance identity changed: got %s, want %s", providerName, instance.UUID, expectedUUID)
	}
	if expectedName != "" && instance.Name != expectedName {
		return core.Exit(5, "%s instance %s name changed: got %q, want %q", providerName, instance.UUID, instance.Name, expectedName)
	}
	return nil
}

func (b *backend) reconcileCreateIntent(ctx context.Context, api unikraftCloudAPI, claim core.LeaseClaim, removeWhenAbsent bool) (core.LeaseClaim, *ukcInstance, bool, error) {
	if claim.CloudID != "" {
		return claim, nil, false, nil
	}
	if claim.Labels["state"] != ukcStateCreateIntent {
		return claim, nil, false, nil
	}
	instances, err := api.ListInstances(ctx)
	if err != nil {
		return claim, nil, false, err
	}
	reconciled, recovered, err := b.reconcileCreateIntentFromInventory(claim, instances)
	if err != nil || recovered != nil {
		return reconciled, recovered, false, err
	}
	if !removeWhenAbsent {
		return reconciled, nil, false, nil
	}
	if err := b.proveCreateIntentAbsent(ctx, api, claim); err != nil {
		return claim, nil, false, err
	}
	if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
		return claim, nil, false, err
	}
	return core.LeaseClaim{}, nil, true, nil
}

func (b *backend) reconcileCreateIntentFromInventory(claim core.LeaseClaim, instances []ukcInstance) (core.LeaseClaim, *ukcInstance, error) {
	if claim.CloudID != "" || claim.Labels["state"] != ukcStateCreateIntent {
		return claim, nil, nil
	}
	if _, err := indexUnikraftCloudInventory(instances); err != nil {
		return claim, nil, err
	}
	resourceName := strings.TrimSpace(claim.Labels[ukcLabelResourceName])
	matches := make([]ukcInstance, 0, 1)
	for _, instance := range instances {
		if instance.Name == resourceName {
			matches = append(matches, instance)
		}
	}
	if len(matches) > 1 {
		return claim, nil, core.Exit(5, "%s create recovery found %d instances named %q; claim retained", providerName, len(matches), resourceName)
	}
	if len(matches) == 1 {
		if err := validateUnikraftCloudInstanceIdentity(matches[0], "", resourceName); err != nil {
			return claim, nil, err
		}
		ready, err := b.publishReadyClaim(claim, matches[0])
		if err != nil {
			return claim, nil, err
		}
		instance := matches[0]
		return ready, &instance, nil
	}
	return claim, nil, nil
}

func definiteUnikraftCloudCreateRejection(err error) bool {
	var apiErr *unikraftCloudAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

type unikraftCloudClaimNotFoundError struct {
	cause core.ExitError
}

func (e *unikraftCloudClaimNotFoundError) Error() string { return e.cause.Error() }
func (e *unikraftCloudClaimNotFoundError) Unwrap() error { return e.cause }

func newUnikraftCloudClaimNotFoundError(identifier string) error {
	return &unikraftCloudClaimNotFoundError{cause: core.Exit(4, "%s instance %q is not claimed by Crabbox; warmup creates claimed instances, or use the Unikraft Cloud console or kraft CLI for unmanaged instances", providerName, identifier)}
}

func (b *backend) resolveClaim(identifier, scope string) (core.LeaseClaim, bool, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return core.LeaseClaim{}, false, core.Exit(2, "provider=%s requires --id <lease-id or slug>", providerName)
	}
	if strings.HasPrefix(identifier, leasePrefix) {
		claim, exists, err := core.ReadLeaseClaimWithPresence(identifier)
		if err != nil {
			return core.LeaseClaim{}, false, err
		}
		if exists {
			if err := validateUnikraftCloudClaim(claim, scope); err != nil {
				return core.LeaseClaim{}, false, err
			}
			return claim, true, nil
		}
		return core.LeaseClaim{}, false, newUnikraftCloudClaimNotFoundError(identifier)
	}
	claims, err := listUnikraftCloudLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	if unikraftCloudUUIDPattern.MatchString(identifier) {
		var matched core.LeaseClaim
		for _, claim := range claims {
			if claim.Provider != providerName || claim.ProviderScope != scope || !strings.EqualFold(claim.CloudID, identifier) {
				continue
			}
			if matched.LeaseID != "" {
				return core.LeaseClaim{}, false, core.Exit(4, "%s instance %q is claimed by multiple local leases; use an exact lease ID", providerName, identifier)
			}
			matched = claim
		}
		if matched.LeaseID == "" {
			return core.LeaseClaim{}, false, newUnikraftCloudClaimNotFoundError(identifier)
		}
		if err := validateUnikraftCloudClaim(matched, scope); err != nil {
			return core.LeaseClaim{}, false, err
		}
		return matched, true, nil
	}

	slug := core.NormalizeLeaseSlug(identifier)
	var matched core.LeaseClaim
	for _, claim := range claims {
		if claim.Provider != providerName || claim.ProviderScope != scope {
			continue
		}
		if claim.LeaseID != identifier && core.NormalizeLeaseSlug(claim.Slug) != slug {
			continue
		}
		if matched.LeaseID != "" {
			return core.LeaseClaim{}, false, core.Exit(4, "%s identifier %q matches multiple local claims; use an exact lease ID", providerName, identifier)
		}
		matched = claim
	}
	if matched.LeaseID == "" {
		return core.LeaseClaim{}, false, newUnikraftCloudClaimNotFoundError(identifier)
	}
	if err := validateUnikraftCloudClaim(matched, scope); err != nil {
		return core.LeaseClaim{}, false, err
	}
	return matched, true, nil
}

func (b *backend) deleteClaimedInstance(ctx context.Context, api unikraftCloudAPI, claim core.LeaseClaim) (bool, error) {
	if claim.CloudID == "" {
		state := claim.Labels["state"]
		if state == ukcStateCreatePreflight || state == ukcStateCreateConflict {
			resourceName := strings.TrimSpace(claim.Labels[ukcLabelResourceName])
			if err := b.proveInstanceAbsent(ctx, api, resourceName, resourceName); err != nil {
				return false, fmt.Errorf("%s lease %s is non-adoptable and exact-name absence is unconfirmed; claim retained: %w", providerName, claim.LeaseID, err)
			}
			if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
				return false, err
			}
			return true, nil
		}
		resolved, _, absent, err := b.reconcileCreateIntent(ctx, api, claim, true)
		if err != nil || absent {
			return absent, err
		}
		claim = resolved
	}
	if claim.CloudID == "" {
		return false, core.Exit(5, "%s lease %s has no resolved instance; recovery claim retained", providerName, claim.LeaseID)
	}
	instanceID := claim.CloudID
	resourceName := claim.Labels[ukcLabelResourceName]
	state := claim.Labels["state"]
	if state != ukcStateDeleteAccepted {
		live, err := api.GetInstance(ctx, instanceID)
		if err != nil {
			if isNotFound(err) {
				if proofErr := b.proveInstanceAbsent(ctx, api, instanceID, resourceName); proofErr != nil {
					return false, errors.Join(err, proofErr)
				}
				return true, core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim)
			}
			return false, err
		}
		if err := validateUnikraftCloudInstanceIdentity(live, instanceID, resourceName); err != nil {
			return false, err
		}
		if state != ukcStateDeleteAttempt {
			labels := cloneLabels(claim.Labels)
			labels["state"] = ukcStateDeleteAttempt
			updated := claim
			updated.Labels = labels
			written, err := replaceLeaseClaimIfUnchangedDurable(claim.LeaseID, claim, updated)
			if err != nil {
				return false, err
			}
			claim = written
		}
		deleted, deleteErr := api.DeleteInstance(ctx, instanceID)
		if deleteErr != nil {
			if proofErr := b.proveInstanceAbsent(ctx, api, instanceID, resourceName); proofErr == nil {
				return true, core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim)
			}
			return false, fmt.Errorf("delete %s instance %s was not confirmed; claim retained: %w", providerName, instanceID, deleteErr)
		}
		if err := validateUnikraftCloudDeleteIdentity(deleted, instanceID, resourceName); err != nil {
			return false, err
		}
		labels := cloneLabels(claim.Labels)
		labels["state"] = ukcStateDeleteAccepted
		labels[ukcLabelProviderState] = normalizedInstanceState(deleted.State)
		updated := claim
		updated.Labels = labels
		written, err := replaceLeaseClaimIfUnchangedDurable(claim.LeaseID, claim, updated)
		if err != nil {
			return false, err
		}
		claim = written
	}
	if err := b.proveInstanceAbsent(ctx, api, instanceID, resourceName); err != nil {
		return false, fmt.Errorf("%s deletion accepted for instance %s but absence is unconfirmed; claim retained: %w", providerName, instanceID, err)
	}
	if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
		return false, err
	}
	return false, nil
}

func (b *backend) proveInstanceAbsent(ctx context.Context, api unikraftCloudAPI, identifier, resourceName string) error {
	timeout := b.deleteConfirmationTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	poll := b.pollInterval
	if poll <= 0 {
		poll = statusPollInterval
	}
	proofCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		_, firstErr := api.GetInstance(proofCtx, identifier)
		if isNotFound(firstErr) {
			instances, listErr := api.ListInstances(proofCtx)
			if listErr == nil {
				if _, inventoryErr := indexUnikraftCloudInventory(instances); inventoryErr != nil {
					return inventoryErr
				}
			}
			if listErr == nil && !unikraftCloudInventoryContains(instances, identifier, resourceName) {
				_, secondErr := api.GetInstance(proofCtx, identifier)
				if isNotFound(secondErr) {
					return nil
				}
			}
		}
		select {
		case <-proofCtx.Done():
			return proofCtx.Err()
		case <-time.After(poll):
		}
	}
}

func (b *backend) proveCreateIntentAbsent(ctx context.Context, api unikraftCloudAPI, claim core.LeaseClaim) error {
	resourceName := strings.TrimSpace(claim.Labels[ukcLabelResourceName])
	grace := b.deleteConfirmationTimeout
	if grace <= 0 {
		grace = 30 * time.Second
	}
	poll := b.pollInterval
	if poll <= 0 {
		poll = statusPollInterval
	}
	graceCtx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	for {
		if instance, err := api.GetInstance(graceCtx, resourceName); err == nil {
			return core.Exit(5, "%s ambiguous create for lease %s became visible as instance %s; claim retained, retry stop to reconcile exact ownership", providerName, claim.LeaseID, instance.UUID)
		} else if !isNotFound(err) {
			return fmt.Errorf("observe %s ambiguous create %s during absence grace: %w", providerName, claim.LeaseID, err)
		}
		instances, err := api.ListInstances(graceCtx)
		if err != nil {
			return fmt.Errorf("observe %s ambiguous create %s inventory during absence grace: %w", providerName, claim.LeaseID, err)
		}
		if _, err := indexUnikraftCloudInventory(instances); err != nil {
			return err
		}
		if unikraftCloudInventoryContains(instances, resourceName, resourceName) {
			return core.Exit(5, "%s ambiguous create for lease %s became visible during absence grace; claim retained, retry stop to reconcile exact ownership", providerName, claim.LeaseID)
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-graceCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return b.proveInstanceAbsent(ctx, api, resourceName, resourceName)
		case <-timer.C:
		}
	}
}

func unikraftCloudInventoryContains(instances []ukcInstance, identifier, resourceName string) bool {
	for _, instance := range instances {
		if strings.EqualFold(instance.UUID, identifier) || instance.Name == identifier || (resourceName != "" && instance.Name == resourceName) {
			return true
		}
	}
	return false
}

func unikraftCloudTerminalState(state string) bool {
	switch normalizedInstanceState(state) {
	case "stopped", "failed", "error", "exited", "deleted", "terminated":
		return true
	default:
		return false
	}
}

func serverFromClaim(claim core.LeaseClaim) core.Server {
	labels := cloneLabels(claim.Labels)
	return core.Server{
		CloudID:  claim.CloudID,
		Provider: providerName,
		Name:     core.Blank(labels[ukcLabelResourceName], claim.CloudID),
		Status:   labels["state"],
		Labels:   labels,
	}
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
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
	claims, err := listUnikraftCloudLeaseClaims()
	if err != nil {
		return err
	}
	if err := preflightUnikraftCloudClaimOwnership(claims, scope); err != nil {
		return err
	}
	for _, snapshot := range claims {
		if snapshot.Provider != providerName || snapshot.ProviderScope != scope {
			continue
		}
		unlock, err := lockUnikraftCloudLeaseOperation(ctx, snapshot.LeaseID)
		if err != nil {
			return err
		}
		current, exists, readErr := core.ReadLeaseClaimWithPresence(snapshot.LeaseID)
		if readErr != nil {
			unlock()
			return readErr
		}
		if !exists {
			unlock()
			continue
		}
		if err := validateUnikraftCloudClaim(current, scope); err != nil {
			unlock()
			return err
		}
		state := current.Labels["state"]
		remove, reason := core.ShouldCleanupServer(serverFromClaim(current), core.ClockNow(b.rt.Clock))
		if state == ukcStateDeleteAttempt || state == ukcStateDeleteAccepted || state == ukcStateCreatePreflight || state == ukcStateCreateConflict {
			remove, reason = true, "resume "+state
		}
		if !remove {
			fmt.Fprintf(b.rt.Stderr, "skip %s lease=%s reason=%s\n", providerName, current.LeaseID, reason)
			unlock()
			continue
		}
		action := "destroy"
		switch state {
		case ukcStateDeleteAttempt:
			action = "resume destroy"
		case ukcStateDeleteAccepted:
			action = "resume deletion confirmation"
		case ukcStateCreatePreflight, ukcStateCreateConflict:
			action = "reconcile"
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would %s %s lease=%s instance=%s reason=%s\n", action, providerName, current.LeaseID, core.Blank(current.CloudID, "pending"), reason)
			unlock()
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "%s %s lease=%s instance=%s reason=%s\n", action, providerName, current.LeaseID, core.Blank(current.CloudID, "pending"), reason)
		_, deleteErr := b.deleteClaimedInstance(ctx, api, current)
		unlock()
		if deleteErr != nil {
			return deleteErr
		}
	}
	return nil
}
