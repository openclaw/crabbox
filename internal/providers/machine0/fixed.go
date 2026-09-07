package machine0

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	fixedMachine0CreateIntentVersion = core.FixedMachine0CreateIntentVersion
	fixedMachine0IntentPrepared      = "prepared"
	fixedMachine0IntentAcquired      = "acquired"
	fixedMachine0IntentReleased      = "released"
)

var fixedMachine0LeaseKind = core.FixedLeaseKind{
	ClaimProvider: core.FixedMachine0ClaimProvider,
	IntentVersion: fixedMachine0CreateIntentVersion,
	Label:         "Machine0",
}

type machine0CreateAttempt struct {
	Name         string `json:"name"`
	Size         string `json:"size"`
	Region       string `json:"region"`
	Image        string `json:"image"`
	ImageVersion int    `json:"imageVersion"`
	Key          string `json:"key"`
	CreatedAt    string `json:"createdAt"`
}

func (b *backend) acquireFixed(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	leaseID := strings.TrimSpace(req.RequestedLeaseID)
	cfg := b.configForRun()
	var fingerprint string
	var freshClaim bool
	acquired, err := core.AcquireFixedLease(core.FixedAcquireOptions{
		Kind:         fixedMachine0LeaseKind,
		LeaseID:      leaseID,
		CheckpointID: req.RequestedCheckpointID,
		RepoRoot:     req.Repo.Root,
		Reclaim:      req.Reclaim,
		TargetOS:     cfg.TargetOS,
		WindowsMode:  cfg.WindowsMode,
		TTL:          cfg.TTL,
		IdleTimeout:  cfg.IdleTimeout,
		Now:          b.now,
	}, func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
		freshClaim = !exists
		var err error
		fingerprint, err = core.FixedMachine0CreateIntentFingerprint(cfg, core.FixedMachine0CreateIntentRequest{
			RequestedSlug: core.NormalizeLeaseSlug(req.RequestedSlug),
			Keep:          req.Keep,
		})
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		binding := core.FixedLeaseBinding{Fingerprint: fingerprint}
		if exists {
			if claim.FixedCreateIntent != nil {
				binding.ProviderScope = machine0NameScope(machine0MachineName(leaseID, claim.FixedCreateIntent.Slug))
			}
		} else {
			machines, err := b.api.List(ctx)
			if err != nil {
				return core.FixedLeaseBinding{}, err
			}
			claims, err := machine0Claims()
			if err != nil {
				return core.FixedLeaseBinding{}, err
			}
			servers := make([]Server, 0, len(machines))
			for _, item := range machines {
				servers = append(servers, b.serverFromMachine(item, claims[item.ID], cfg))
			}
			slug, err := allocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
			if err != nil {
				return core.FixedLeaseBinding{}, err
			}
			name := machine0MachineName(leaseID, slug)
			binding.ProviderScope = machine0NameScope(name)
			binding.Slug = slug
		}
		return binding, nil
	}, func(ctx context.Context, claim *core.LeaseClaim, intent *core.FixedCreateIntent, persist func() error) (_ LeaseTarget, acquireErr error) {
		if err := core.AuthorizeCheckpointRelease(*claim, ""); err != nil {
			return LeaseTarget{}, err
		}
		name := machine0MachineName(leaseID, intent.Slug)
		defer func() {
			if acquireErr != nil && len(intent.Attempt) != 0 {
				fmt.Fprintf(b.rt.Stderr, "machine0 fixed create attempt for %s is retained; inspect or stop lease %s after provider inventory converges with `crabbox stop --provider machine0 --id %s`\n", name, leaseID, leaseID)
			}
		}()
		item, err := b.resolveFixedMachine0(ctx, *claim)
		if err != nil {
			return LeaseTarget{}, err
		}
		replay := item.ID != ""
		if !replay {
			if claim.CloudID != "" {
				return LeaseTarget{}, exit(4, "lease_id_conflict: acquired fixed lease %s is missing its bound Machine0 machine", leaseID)
			}
			// Older clients could erase an ambiguous attempt. Only this invocation
			// can prove it created the empty claim while holding the create lock.
			if !freshClaim {
				return LeaseTarget{}, exit(4, "lease_id_conflict: fixed Machine0 lease %s has no observed resource or provably unsubmitted attempt; retain its claim", leaseID)
			}
			// Capacity gates new creation, never replay of an attested VM.
			if err := b.validateCatalogSelection(ctx, cfg.Machine0.Size, cfg.Machine0.Region); err != nil {
				return LeaseTarget{}, err
			}
			if err := b.preflightSSHKey(ctx, cfg.Machine0.Key); err != nil {
				return LeaseTarget{}, err
			}
			image := cfg.Machine0.Image
			if req.Options.Desktop && strings.TrimSpace(cfg.Machine0.DesktopImage) != "" {
				image = cfg.Machine0.DesktopImage
			}
			attempt := machine0CreateAttempt{
				Name: name, Size: cfg.Machine0.Size, Region: cfg.Machine0.Region,
				Image: image, ImageVersion: cfg.Machine0.ImageVersion, Key: cfg.Machine0.Key,
				CreatedAt: b.now().Format(time.RFC3339Nano),
			}
			data, err := json.Marshal(attempt)
			if err != nil {
				return LeaseTarget{}, err
			}
			intent.Attempt = map[string]string{"machine0": string(data)}
			if err := persist(); err != nil {
				return LeaseTarget{}, err
			}
			fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s name=%s size=%s region=%s image=%s keep=%v fixed=true\n", providerName, leaseID, intent.Slug, name, cfg.Machine0.Size, cfg.Machine0.Region, image, req.Keep)
			if createErr := b.api.Create(ctx, createMachineRequest{Name: name, Size: attempt.Size, Region: attempt.Region, Image: image, ImageVersion: attempt.ImageVersion, Key: attempt.Key}); createErr != nil {
				// An empty inventory cannot prove a failed request will never create a VM.
				item, err = b.resolveFixedMachine0(ctx, *claim)
				if err != nil {
					return LeaseTarget{}, errors.Join(createErr, err)
				}
			} else {
				// Native new prints no UUID; the bounded poll attests the first get.
				item = machine{Name: name}
			}
		}
		if item.ID != "" {
			if err := b.bindFixedMachine0(claim, item, req.Keep, persist); err != nil {
				return LeaseTarget{}, err
			}
		}
		item, err = b.waitForResolveRunning(ctx, item, cfg.Machine0.CreateTimeout, replay, func(previous, observed machine) (machine, error) {
			item, err := attestFixedMachine0Detail(*claim, previous, observed)
			if err != nil {
				return machine{}, err
			}
			return item, b.bindFixedMachine0(claim, item, req.Keep, persist)
		})
		if err != nil {
			return LeaseTarget{}, err
		}
		server := b.serverFromMachine(item, *claim, cfg)
		server.Labels = machineLabels(cfg, item, leaseID, intent.Slug, req.Keep, b.now())
		return b.prepareLease(ctx, item, server, leaseID, true)
	}, ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s machine=%s resource=%s state=ready\n", leaseID, acquired.Server.Name, acquired.Server.CloudID)
	if req.OnAcquired != nil {
		if err := req.OnAcquired(acquired); err != nil {
			return LeaseTarget{}, fmt.Errorf("acknowledge fixed Machine0 acquisition: %w", err)
		}
	}
	return acquired, nil
}

func machine0NameScope(name string) string {
	return providerName + ":name:" + strings.TrimSpace(name)
}

// Fixed ownership comes from the durable attempt and, once observed, the UUID.
// Resolution never starts a machine or needs a usable SSH endpoint.
func (b *backend) resolveFixedMachine0(ctx context.Context, claim LeaseClaim) (machine, error) {
	if claim.FixedCreateIntent != nil && claim.FixedCreateIntent.State == fixedMachine0IntentReleased {
		return machine{}, fixedMachine0LeaseKind.ValidateTerminalClaim(claim, LeaseClaim{}, claim.LeaseID, validateFixedMachine0TerminalClaimExtra)
	}
	attempt, err := fixedMachine0ClaimAttempt(claim)
	if err != nil {
		return machine{}, err
	}
	machines, err := b.api.List(ctx)
	if err != nil {
		return machine{}, err
	}
	name := machine0MachineName(claim.LeaseID, claim.Slug)
	seen := make(map[string]bool, len(machines))
	var found *machine
	for _, item := range machines {
		if item.ID != "" && seen[item.ID] {
			return machine{}, exit(4, "lease_id_conflict: Machine0 inventory contains duplicate resource ID %s", item.ID)
		}
		seen[item.ID] = true
		if item.Name == name {
			if found != nil {
				return machine{}, exit(4, "lease_id_conflict: multiple Machine0 machines match fixed lease %s", claim.LeaseID)
			}
			found = &item
		} else if item.ID == claim.CloudID && claim.CloudID != "" {
			return machine{}, exit(4, "lease_id_conflict: fixed Machine0 lease %s resource name changed", claim.LeaseID)
		}
	}
	if found != nil {
		if attempt == nil {
			return machine{}, exit(4, "lease_id_conflict: Machine0 machine %s has no durable create attempt", name)
		}
		if strings.TrimSpace(found.ID) == "" {
			return machine{}, exit(4, "lease_id_conflict: Machine0 machine %s has no resource ID", name)
		}
		// Inventory discovers identity; only full detail can attest pinned fields.
		detail, err := b.api.Get(ctx, name)
		if err != nil {
			return machine{}, err
		}
		return attestFixedMachine0Detail(claim, *found, detail)
	}
	if claim.FixedCreateIntent.State == fixedMachine0IntentPrepared && len(claim.FixedCreateIntent.Attempt) != 0 {
		return machine{}, exit(4, "lease_id_conflict: fixed Machine0 lease %s has an unresolved create attempt; retain the claim and retry inspection or stop after provider inventory converges", claim.LeaseID)
	}
	return machine{}, nil
}

func fixedMachine0ClaimAttempt(claim LeaseClaim) (*machine0CreateAttempt, error) {
	intent := claim.FixedCreateIntent
	name := machine0MachineName(claim.LeaseID, claim.Slug)
	if !core.IsCanonicalLeaseID(claim.LeaseID) || !fixedMachine0LeaseKind.IsFixedClaim(claim) ||
		intent.Version != fixedMachine0CreateIntentVersion || intent.Fingerprint == "" || intent.Slug != claim.Slug ||
		intent.ProviderScope != machine0NameScope(name) ||
		(intent.State != fixedMachine0IntentPrepared && intent.State != fixedMachine0IntentAcquired) {
		return nil, exit(4, "lease_id_conflict: invalid fixed Machine0 create intent for lease %s", claim.LeaseID)
	}
	if claim.CloudNumericID != 0 || claim.CloudImmutableID != claim.CloudID ||
		(claim.CloudID == "" && (claim.ProviderScope != intent.ProviderScope || intent.State == fixedMachine0IntentAcquired)) ||
		(claim.CloudID != "" && claim.ProviderScope != machineScope(claim.CloudID)) {
		return nil, exit(4, "lease_id_conflict: fixed Machine0 lease %s has inconsistent immutable identity or provider scope", claim.LeaseID)
	}
	if _, err := time.Parse(time.RFC3339Nano, intent.CreatedAt); err != nil || len(intent.FailedAttempts) != 0 {
		return nil, exit(4, "lease_id_conflict: invalid fixed Machine0 attempt history for lease %s", claim.LeaseID)
	}
	if len(intent.Attempt) == 0 {
		if claim.CloudID != "" || len(claim.Labels) != 0 || claim.SSHHost != "" || claim.SSHPort != 0 {
			return nil, exit(4, "lease_id_conflict: fixed Machine0 lease %s has no durable create attempt", claim.LeaseID)
		}
		return nil, nil
	}
	var attempt machine0CreateAttempt
	if err := json.Unmarshal([]byte(intent.Attempt["machine0"]), &attempt); err != nil ||
		attempt.Name != name || attempt.Size == "" || attempt.Region == "" || attempt.Image == "" {
		return nil, exit(4, "lease_id_conflict: invalid fixed Machine0 attempt for lease %s", claim.LeaseID)
	}
	return &attempt, nil
}

func validateFixedMachine0Ownership(claim LeaseClaim, item machine) error {
	attempt, err := fixedMachine0ClaimAttempt(claim)
	if err != nil {
		return err
	}
	if attempt == nil {
		return exit(4, "lease_id_conflict: Machine0 machine %s has no durable create attempt", item.Name)
	}
	if strings.TrimSpace(item.ID) == "" || (claim.CloudID != "" && item.ID != claim.CloudID) {
		return exit(4, "lease_id_conflict: fixed lease %s machine %s does not match acquired CloudID %s", claim.LeaseID, blank(item.ID, "<empty>"), blank(claim.CloudID, "<empty>"))
	}
	if strings.TrimSpace(attempt.Key) != "" && (item.Key == nil || strings.TrimSpace(item.Key.Name) != strings.TrimSpace(attempt.Key)) {
		return exit(4, "lease_id_conflict: Machine0 machine detail for fixed lease %s does not match its durable selected SSH key %q", claim.LeaseID, attempt.Key)
	}
	if mismatch := validateFixedMachine0Attempt(item, *attempt); mismatch != "" {
		return exit(4, "lease_id_conflict: Machine0 machine for lease %s does not match its durable create attempt: %s", claim.LeaseID, mismatch)
	}
	return nil
}

func (b *backend) bindFixedMachine0(claim *LeaseClaim, item machine, keep bool, persist func() error) error {
	if err := validateFixedMachine0Ownership(*claim, item); err != nil {
		return err
	}
	if claim.CloudID != "" {
		return nil
	}
	claim.CloudID, claim.CloudImmutableID = item.ID, item.ID
	claim.ProviderScope = machineScope(item.ID)
	claim.Labels = machineLabels(b.configForRun(), item, claim.LeaseID, claim.Slug, keep, b.now())
	return persist()
}

func (b *backend) bindFixedMachine0Claim(claim LeaseClaim, item machine) (LeaseClaim, error) {
	expected := claim
	err := b.bindFixedMachine0(&claim, item, false, func() error {
		var err error
		claim, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, expected, claim)
		return err
	})
	return claim, err
}

func (b *backend) destroyClaimedMachine(ctx context.Context, expected LeaseClaim, lease LeaseTarget) error {
	return b.destroyClaimedMachineWithOutcome(ctx, expected, lease, &core.ReleaseLeaseOutcome{})
}

func (b *backend) destroyClaimedMachineWithOutcome(ctx context.Context, expected LeaseClaim, lease LeaseTarget, outcome *core.ReleaseLeaseOutcome) error {
	if expected.Provider != core.FixedMachine0ClaimProvider && expected.FixedCreateIntent == nil {
		return fixedMachine0LeaseKind.FinalizeAfterCleanup(expected, func() error {
			// Reservation holds the source before it changes the claim revision.
			if err := core.AuthorizeCheckpointRelease(expected, ""); err != nil {
				return err
			}
			if lease.Server.Name != "" {
				err := b.api.Remove(ctx, lease.Server.Name)
				outcome.Terminal = err == nil
				return err
			}
			outcome.Terminal = true
			return nil
		})
	}
	if snapshot, exists, set := core.ServerLeaseClaimSnapshot(lease.Server); set && (!exists || !reflect.DeepEqual(snapshot, expected)) {
		return exit(4, "fixed Machine0 lease %s claim changed after resolution; retry", expected.LeaseID)
	}
	return core.WithDurableLeaseClaimLock(expected.LeaseID, func(claim *LeaseClaim, exists bool, persist func() error) error {
		if !exists || !reflect.DeepEqual(*claim, expected) {
			return exit(4, "fixed Machine0 lease %s claim changed before release; retry", expected.LeaseID)
		}
		if err := core.AuthorizeCheckpointRelease(*claim, ""); err != nil {
			return err
		}
		item, err := b.resolveFixedMachine0(ctx, *claim)
		if err != nil || claim.FixedCreateIntent.State == fixedMachine0IntentReleased {
			outcome.Terminal = err == nil
			return err
		}
		resourceID := firstNonBlank(item.ID, claim.CloudID)
		if (lease.Server.CloudID != "" && lease.Server.CloudID != resourceID) || (lease.Server.ImmutableID != "" && lease.Server.ImmutableID != resourceID) {
			return exit(4, "lease_id_conflict: fixed Machine0 resource changed before release")
		}
		if item.ID != "" {
			if err := b.bindFixedMachine0(claim, item, false, persist); err != nil {
				return err
			}
			// Native rm resolves by name. Re-attest that name under the claim lock
			// immediately before deletion; never pass a Crabbox ID to the native CLI.
			detail, err := b.api.Get(ctx, item.Name)
			if err != nil {
				return err
			}
			if _, err := attestFixedMachine0Detail(*claim, item, detail); err != nil {
				return err
			}
			if err := b.api.Remove(ctx, item.Name); err != nil {
				return err
			}
			outcome.Terminal = true
		} else {
			return exit(4, "fixed Machine0 lease %s is not visible in the current account; absence is unverified, retain its claim and inspect the original account", claim.LeaseID)
		}
		*claim = fixedMachine0LeaseKind.TerminalClaim(*claim, b.now())
		return persist()
	})
}

func validateFixedMachine0Attempt(item machine, attempt machine0CreateAttempt) string {
	switch {
	case item.Name != attempt.Name:
		return fmt.Sprintf("name=%q, want %q", item.Name, attempt.Name)
	case item.Size != attempt.Size:
		return fmt.Sprintf("size=%q, want %q", item.Size, attempt.Size)
	case item.Region != attempt.Region:
		return fmt.Sprintf("region=%q, want %q", item.Region, attempt.Region)
	case item.Image != attempt.Image:
		return fmt.Sprintf("image=%q, want %q", item.Image, attempt.Image)
	case attempt.ImageVersion != 0 && item.ImageVersion != attempt.ImageVersion:
		return fmt.Sprintf("imageVersion=%d, want %d", item.ImageVersion, attempt.ImageVersion)
	default:
		return ""
	}
}

func isMachine0ClaimProvider(provider string) bool {
	return provider == providerName || provider == core.FixedMachine0ClaimProvider
}

func validateFixedMachine0TerminalClaimExtra(claim core.LeaseClaim) error {
	intent := claim.FixedCreateIntent
	if intent.ProviderScope != machine0NameScope(machine0MachineName(claim.LeaseID, intent.Slug)) ||
		claim.CloudNumericID != 0 || claim.CloudImmutableID != "" {
		return exit(4, "lease_id_conflict: fixed Machine0 lease %s has an invalid terminal tombstone", claim.LeaseID)
	}
	return nil
}

func (b *backend) retainLeaseClaimAfterRelease(lease LeaseTarget, previous core.LeaseClaim) (bool, error) {
	claim, exists, err := resolveClaim(lease.LeaseID)
	if err != nil {
		return false, err
	}
	if normalizeReleasePolicy(b.configForRun().Machine0.ReleasePolicy) == "suspend" && exists &&
		(!fixedMachine0LeaseKind.IsFixedClaim(claim) || claim.FixedCreateIntent.State != fixedMachine0IntentReleased) {
		return true, nil
	}
	return fixedMachine0LeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, fixedMachine0LeaseKind.IsFixedClaim(claim), validateFixedMachine0TerminalClaimExtra, nil)
}

func attestFixedMachine0Detail(claim LeaseClaim, previous, detail machine) (machine, error) {
	if previous.ID != "" && (detail.ID != previous.ID || detail.Name != previous.Name) {
		return machine{}, exit(4, "lease_id_conflict: Machine0 machine detail for fixed lease %s does not match its inventory resource identity", claim.LeaseID)
	}
	if err := validateFixedMachine0Ownership(claim, detail); err != nil {
		return machine{}, err
	}
	if previous.Key != nil && detail.Key != nil {
		if strings.TrimSpace(previous.Key.Name) != "" && strings.TrimSpace(previous.Key.Name) != strings.TrimSpace(detail.Key.Name) {
			return machine{}, exit(4, "lease_id_conflict: Machine0 machine detail for fixed lease %s does not match its inventory SSH key name", claim.LeaseID)
		}
		if strings.TrimSpace(previous.Key.Type) != "" && strings.TrimSpace(detail.Key.Type) != "" && !strings.EqualFold(strings.TrimSpace(previous.Key.Type), strings.TrimSpace(detail.Key.Type)) {
			return machine{}, exit(4, "lease_id_conflict: Machine0 machine detail for fixed lease %s does not match its inventory SSH key type", claim.LeaseID)
		}
		if strings.TrimSpace(detail.Key.Type) == "" {
			key := *detail.Key
			key.Type = previous.Key.Type
			detail.Key = &key
		}
	}
	return detail, nil
}
