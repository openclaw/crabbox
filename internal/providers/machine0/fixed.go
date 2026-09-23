package machine0

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	shared "github.com/openclaw/crabbox/internal/providers/shared"
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

func (b *backend) acquireFixed(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	leaseID := strings.TrimSpace(req.RequestedLeaseID)
	cfg := b.configForRun()
	var fingerprint string
	var replay bool
	var retainedIntent *core.FixedCreateIntent
	acquired, err := core.AcquireFixedResource(ctx, core.FixedAcquireOptions{
		Kind:         fixedMachine0LeaseKind,
		LeaseID:      leaseID,
		CheckpointID: req.RequestedCheckpointID,
		RepoRoot:     req.Repo.Root,
		Reclaim:      req.Reclaim,
		TargetOS:     cfg.TargetOS,
		WindowsMode:  cfg.WindowsMode,
		TTL:          cfg.TTL,
		IdleTimeout:  cfg.IdleTimeout,
		Now:          func() time.Time { return core.ClockNow(b.rt.Clock).UTC() },
	}, core.FixedLeaseOperations[machine]{Admission: &core.FixedAdmission{FreshOnly: true}, DescribeIntent: func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
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
			servers := make([]core.Server, 0, len(machines))
			for _, item := range machines {
				servers = append(servers, b.serverFromMachine(item, claims[item.ID], cfg))
			}
			slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
			if err != nil {
				return core.FixedLeaseBinding{}, err
			}
			name := machine0MachineName(leaseID, slug)
			binding.ProviderScope = machine0NameScope(name)
			binding.Slug = slug
		}
		return binding, nil
	}, ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[machine], error) {
		retainedIntent = tx.Claim.FixedCreateIntent
		var result core.FixedObservation[machine]
		if err := core.AuthorizeCheckpointRelease(*tx.Claim, ""); err != nil {
			return result, err
		}
		item, err := b.resolveFixedMachine0(ctx, *tx.Claim)
		if err != nil {
			return result, err
		}
		replay = item.ID != ""
		if replay {
			result.Candidates = []machine{item}
		} else {
			if tx.Claim.CloudID != "" {
				return result, core.Exit(4, "lease_id_conflict: acquired fixed lease %s is missing its bound Machine0 machine", leaseID)
			}
			result.CanSubmit = true
		}
		return result, nil
	}, Plan: func(ctx context.Context, claim core.LeaseClaim) (core.FixedAttemptPlan, error) {
		intent := claim.FixedCreateIntent
		name := machine0MachineName(leaseID, intent.Slug)
		// Capacity gates new creation, never replay of an attested VM.
		if err := b.validateCatalogSelection(ctx, cfg.Machine0.Size, cfg.Machine0.Region); err != nil {
			return core.FixedAttemptPlan{}, err
		}
		if err := b.preflightSSHKey(ctx, cfg.Machine0.Key); err != nil {
			return core.FixedAttemptPlan{}, err
		}
		image := cfg.Machine0.Image
		if req.Options.Desktop && strings.TrimSpace(cfg.Machine0.DesktopImage) != "" {
			image = cfg.Machine0.DesktopImage
		}
		attempt := machine0CreateAttempt{
			Name: name, Size: cfg.Machine0.Size, Region: cfg.Machine0.Region,
			Image: image, ImageVersion: cfg.Machine0.ImageVersion, Key: cfg.Machine0.Key,
			CreatedAt: core.ClockNow(b.rt.Clock).UTC().Format(time.RFC3339Nano),
		}
		return core.FixedAttemptPlan{JSONKey: "machine0", Payload: attempt}, nil
	}, Submit: func(ctx context.Context, tx *core.FixedTransaction) (machine, error) {
		intent := tx.Claim.FixedCreateIntent
		attempt, err := fixedMachine0ClaimAttempt(*tx.Claim)
		if err != nil {
			return machine{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s name=%s size=%s region=%s image=%s keep=%v fixed=true\n", providerName, leaseID, intent.Slug, attempt.Name, attempt.Size, attempt.Region, attempt.Image, req.Keep)
		if createErr := b.api.Create(ctx, createMachineRequest{Name: attempt.Name, Size: attempt.Size, Region: attempt.Region, Image: attempt.Image, ImageVersion: attempt.ImageVersion, Key: attempt.Key}); createErr != nil {
			item, err := b.resolveFixedMachine0(ctx, *tx.Claim)
			if err != nil {
				return machine{}, errors.Join(createErr, err)
			}
			return item, nil
		}
		return machine{Name: attempt.Name}, nil
	}, PrepareAccess: func(ctx context.Context, tx *core.FixedTransaction, item machine) (core.LeaseTarget, error) {
		claim, intent := tx.Claim, tx.Claim.FixedCreateIntent
		if item.ID != "" {
			if err := validateFixedMachine0Ownership(*claim, item); err != nil {
				return core.LeaseTarget{}, err
			}
			if err := tx.Bind(b.fixedMachine0Binding(*claim, item, req.Keep)); err != nil {
				return core.LeaseTarget{}, err
			}
		}
		item, err := b.waitForResolveRunning(ctx, item, cfg.Machine0.CreateTimeout, replay, func(previous, observed machine) (machine, error) {
			item, err := attestFixedMachine0Detail(*claim, previous, observed)
			if err != nil {
				return machine{}, err
			}
			return item, tx.Bind(b.fixedMachine0Binding(*claim, item, req.Keep))
		})
		if err != nil {
			return core.LeaseTarget{}, err
		}
		server := b.serverFromMachine(item, *claim, cfg)
		server.Labels = machineLabels(cfg, item, leaseID, intent.Slug, req.Keep, core.ClockNow(b.rt.Clock).UTC())
		return b.prepareLease(ctx, item, server, leaseID, true)
	}})
	if err != nil {
		if retainedIntent != nil && len(retainedIntent.Attempt) != 0 {
			fmt.Fprintf(b.rt.Stderr, "machine0 fixed create attempt for %s is retained; inspect or stop lease %s after provider inventory converges with `crabbox stop --provider machine0 --id %s`\n", machine0MachineName(leaseID, retainedIntent.Slug), leaseID, leaseID)
		}
		return core.LeaseTarget{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s machine=%s resource=%s state=ready\n", leaseID, acquired.Server.Name, acquired.Server.CloudID)
	return core.CompleteFixedAcquisition(acquired, nil, req)
}

func machine0NameScope(name string) string {
	return providerName + ":name:" + strings.TrimSpace(name)
}

// Fixed ownership comes from the durable attempt and, once observed, the UUID.
// Resolution never starts a machine or needs a usable SSH endpoint.
func (b *backend) resolveFixedMachine0(ctx context.Context, claim core.LeaseClaim) (machine, error) {
	observed, err := core.InspectFixedResource(ctx, fixedMachine0LeaseKind, claim, core.FixedLeaseOperations[machine]{ObserveExact: func(ctx context.Context, _ *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[machine], error) {
		if claim.FixedCreateIntent != nil && claim.FixedCreateIntent.State == fixedMachine0IntentReleased {
			return core.FixedObservation[machine]{}, fixedMachine0LeaseKind.ValidateTerminalClaim(claim, core.LeaseClaim{}, claim.LeaseID, validateFixedMachine0TerminalClaimExtra)
		}
		attempt, err := fixedMachine0ClaimAttempt(claim)
		if err != nil {
			return core.FixedObservation[machine]{}, err
		}
		machines, err := b.api.List(ctx)
		if err != nil {
			return core.FixedObservation[machine]{}, err
		}
		name := machine0MachineName(claim.LeaseID, claim.Slug)
		seen := make(map[string]bool, len(machines))
		var found *machine
		for _, item := range machines {
			if item.ID != "" && seen[item.ID] {
				return core.FixedObservation[machine]{}, core.Exit(4, "lease_id_conflict: Machine0 inventory contains duplicate resource ID %s", item.ID)
			}
			seen[item.ID] = true
			if item.Name == name {
				if found != nil {
					return core.FixedObservation[machine]{}, core.Exit(4, "lease_id_conflict: multiple Machine0 machines match fixed lease %s", claim.LeaseID)
				}
				found = &item
			} else if item.ID == claim.CloudID && claim.CloudID != "" {
				return core.FixedObservation[machine]{}, core.Exit(4, "lease_id_conflict: fixed Machine0 lease %s resource name changed", claim.LeaseID)
			}
		}
		if found != nil {
			if attempt == nil {
				return core.FixedObservation[machine]{}, core.Exit(4, "lease_id_conflict: Machine0 machine %s has no durable create attempt", name)
			}
			if strings.TrimSpace(found.ID) == "" {
				return core.FixedObservation[machine]{}, core.Exit(4, "lease_id_conflict: Machine0 machine %s has no resource ID", name)
			}
			// Inventory discovers identity; only full detail can attest pinned fields.
			detail, err := b.api.Get(ctx, name)
			if err != nil {
				return core.FixedObservation[machine]{}, err
			}
			attested, err := attestFixedMachine0Detail(claim, *found, detail)
			if err != nil {
				return core.FixedObservation[machine]{}, err
			}
			return core.FixedObservation[machine]{Candidates: []machine{attested}}, nil
		}
		if claim.FixedCreateIntent.State == fixedMachine0IntentPrepared && len(claim.FixedCreateIntent.Attempt) != 0 {
			return core.FixedObservation[machine]{}, core.Exit(4, "lease_id_conflict: fixed Machine0 lease %s has an unresolved create attempt; retain the claim and retry inspection or stop after provider inventory converges", claim.LeaseID)
		}
		return core.FixedObservation[machine]{}, nil
	}})
	if err != nil {
		return machine{}, err
	}
	if len(observed.Candidates) == 0 {
		return machine{}, nil
	}
	return observed.Candidates[0], nil
}

func fixedMachine0ClaimAttempt(claim core.LeaseClaim) (*machine0CreateAttempt, error) {
	name := machine0MachineName(claim.LeaseID, claim.Slug)
	scope := machine0NameScope(name)
	if claim.CloudID != "" {
		scope = machineScope(claim.CloudID)
	}
	if err := core.ValidateFixedClaim(claim, core.FixedClaimRules{Kind: fixedMachine0LeaseKind,
		States: []string{fixedMachine0IntentPrepared, fixedMachine0IntentAcquired}, Scope: scope, IntentScope: machine0NameScope(name),
		RequireCanonicalID: true, RequireTimestamp: true, NoNumericID: true, SameImmutableID: true,
		NoFailedAttempts: true, EmptyAttemptMustBePristine: true, GenerationAfterPrepared: true,
	}); err != nil {
		return nil, err
	}
	return core.ReadFixedAttempt[machine0CreateAttempt](claim.FixedCreateIntent, core.FixedAttemptFormat{
		JSONKey: "machine0", Equal: map[string]string{"name": name}, Required: []string{"size", "region", "image"},
	})
}

func validateFixedMachine0Ownership(claim core.LeaseClaim, item machine) error {
	attempt, err := fixedMachine0ClaimAttempt(claim)
	if err != nil {
		return err
	}
	if attempt == nil {
		return core.Exit(4, "lease_id_conflict: Machine0 machine %s has no durable create attempt", item.Name)
	}
	if strings.TrimSpace(item.ID) == "" || (claim.CloudID != "" && item.ID != claim.CloudID) {
		return core.Exit(4, "lease_id_conflict: fixed lease %s machine %s does not match acquired CloudID %s", claim.LeaseID, core.Blank(item.ID, "<empty>"), core.Blank(claim.CloudID, "<empty>"))
	}
	if strings.TrimSpace(attempt.Key) != "" && (item.Key == nil || strings.TrimSpace(item.Key.Name) != strings.TrimSpace(attempt.Key)) {
		return core.Exit(4, "lease_id_conflict: Machine0 machine detail for fixed lease %s does not match its durable selected SSH key %q", claim.LeaseID, attempt.Key)
	}
	if mismatch := validateFixedMachine0Attempt(item, *attempt); mismatch != "" {
		return core.Exit(4, "lease_id_conflict: Machine0 machine for lease %s does not match its durable create attempt: %s", claim.LeaseID, mismatch)
	}
	return nil
}

func (b *backend) bindFixedMachine0(claim *core.LeaseClaim, item machine, keep bool, persist func() error) error {
	if err := validateFixedMachine0Ownership(*claim, item); err != nil {
		return err
	}
	return core.BindFixedClaim(claim, b.fixedMachine0Binding(*claim, item, keep), persist)
}

func (b *backend) fixedMachine0Binding(claim core.LeaseClaim, item machine, keep bool) core.FixedResourceBinding {
	return core.FixedResourceBinding{OnlyUnbound: true,
		CloudID: item.ID, ImmutableID: item.ID, ProviderScope: machineScope(item.ID),
		Labels: machineLabels(b.configForRun(), item, claim.LeaseID, claim.Slug, keep, core.ClockNow(b.rt.Clock).UTC()),
	}
}

func (b *backend) bindFixedMachine0Claim(claim core.LeaseClaim, item machine) (core.LeaseClaim, error) {
	return core.CompareAndBindFixedClaim(claim, func(next *core.LeaseClaim, persist func() error) error {
		return b.bindFixedMachine0(next, item, false, persist)
	})
}

func (b *backend) destroyClaimedMachine(ctx context.Context, expected core.LeaseClaim, lease core.LeaseTarget) error {
	return b.destroyClaimedMachineWithOutcome(ctx, expected, lease, &core.ReleaseLeaseOutcome{})
}

func (b *backend) destroyClaimedMachineWithOutcome(ctx context.Context, expected core.LeaseClaim, lease core.LeaseTarget, outcome *core.ReleaseLeaseOutcome) error {
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
		return core.Exit(4, "fixed Machine0 lease %s claim changed after resolution; retry", expected.LeaseID)
	}
	checkpointID := ""
	return core.DeleteFixedResource(ctx, fixedMachine0LeaseKind, expected, core.FixedLeaseOperations[machine]{
		Release: &core.FixedReleasePolicy{CheckpointID: &checkpointID, Outcome: outcome},
		ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[machine], error) {
			claim := tx.Claim
			var result core.FixedObservation[machine]
			item, err := b.resolveFixedMachine0(ctx, *claim)
			if err != nil || claim.FixedCreateIntent.State == fixedMachine0IntentReleased {
				result.AbsenceProven = err == nil
				return result, err
			}
			resourceID := shared.FirstNonBlankTrimmed(item.ID, claim.CloudID)
			if (lease.Server.CloudID != "" && lease.Server.CloudID != resourceID) || (lease.Server.ImmutableID != "" && lease.Server.ImmutableID != resourceID) {
				return result, core.Exit(4, "lease_id_conflict: fixed Machine0 resource changed before release")
			}
			if item.ID == "" {
				return result, core.Exit(4, "fixed Machine0 lease %s is not visible in the current account; absence is unverified, retain its claim and inspect the original account", claim.LeaseID)
			}
			binding := b.fixedMachine0Binding(*claim, item, false)
			return core.FixedObservation[machine]{Candidates: []machine{item}, Binding: &binding}, nil
		},
		DeleteExact: func(ctx context.Context, tx *core.FixedTransaction, item machine) error {
			detail, err := b.api.Get(ctx, item.Name)
			if err != nil {
				return err
			}
			if _, err := attestFixedMachine0Detail(*tx.Claim, item, detail); err != nil {
				return err
			}
			return b.api.Remove(ctx, item.Name)
		},
	}, func() time.Time { return core.ClockNow(b.rt.Clock).UTC() })
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
		return core.Exit(4, "lease_id_conflict: fixed Machine0 lease %s has an invalid terminal tombstone", claim.LeaseID)
	}
	return nil
}

func (b *backend) retainLeaseClaimAfterRelease(lease core.LeaseTarget, previous core.LeaseClaim) (bool, error) {
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

func attestFixedMachine0Detail(claim core.LeaseClaim, previous, detail machine) (machine, error) {
	if previous.ID != "" && (detail.ID != previous.ID || detail.Name != previous.Name) {
		return machine{}, core.Exit(4, "lease_id_conflict: Machine0 machine detail for fixed lease %s does not match its inventory resource identity", claim.LeaseID)
	}
	if err := validateFixedMachine0Ownership(claim, detail); err != nil {
		return machine{}, err
	}
	if previous.Key != nil && detail.Key != nil {
		if strings.TrimSpace(previous.Key.Name) != "" && strings.TrimSpace(previous.Key.Name) != strings.TrimSpace(detail.Key.Name) {
			return machine{}, core.Exit(4, "lease_id_conflict: Machine0 machine detail for fixed lease %s does not match its inventory SSH key name", claim.LeaseID)
		}
		if strings.TrimSpace(previous.Key.Type) != "" && strings.TrimSpace(detail.Key.Type) != "" && !strings.EqualFold(strings.TrimSpace(previous.Key.Type), strings.TrimSpace(detail.Key.Type)) {
			return machine{}, core.Exit(4, "lease_id_conflict: Machine0 machine detail for fixed lease %s does not match its inventory SSH key type", claim.LeaseID)
		}
		if strings.TrimSpace(detail.Key.Type) == "" {
			key := *detail.Key
			key.Type = previous.Key.Type
			detail.Key = &key
		}
	}
	return detail, nil
}
