package machine0

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	fixedMachine0CreateIntentVersion   = core.FixedMachine0CreateIntentVersion
	fixedMachine0IntentPrepared        = "prepared"
	fixedMachine0IntentAcquired        = "acquired"
	fixedMachine0IntentReleased        = "released"
	machine0FixedCreateReconcileWindow = 60 * time.Second
)

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
	var acquired LeaseTarget
	err := core.WithDurableLeaseClaimLock(leaseID, func(claim *core.LeaseClaim, exists bool, persist func() error) error {
		if exists && isFixedMachine0Claim(*claim) && claim.FixedCreateIntent.State == fixedMachine0IntentReleased {
			return exit(4, "lease_id_conflict: fixed lease %s is terminal and cannot be replayed", leaseID)
		}
		cfg := b.configForRun()
		if err := b.validateCatalogSelection(ctx, cfg.Machine0.Size, cfg.Machine0.Region); err != nil {
			return err
		}
		fingerprint, err := core.FixedMachine0CreateIntentFingerprint(cfg, core.FixedMachine0CreateIntentRequest{
			RequestedSlug: core.NormalizeLeaseSlug(req.RequestedSlug),
			Keep:          req.Keep,
		})
		if err != nil {
			return err
		}

		if exists {
			if claim.FixedCreateIntent == nil ||
				claim.FixedCreateIntent.Version != fixedMachine0CreateIntentVersion ||
				claim.FixedCreateIntent.Fingerprint != fingerprint ||
				claim.FixedCreateIntent.ProviderScope != machine0NameScope(machine0MachineName(leaseID, claim.FixedCreateIntent.Slug)) {
				return exit(4, "lease_id_conflict: lease %s is bound to another create intent", leaseID)
			}
			if claim.Provider != core.FixedMachine0ClaimProvider {
				return exit(4, "lease_id_conflict: lease %s is bound to provider=%s", leaseID, claim.Provider)
			}
			if claim.RepoRoot != "" && claim.RepoRoot != req.Repo.Root && !req.Reclaim {
				return exit(4, "lease_id_conflict: lease %s is bound to another repository", leaseID)
			}
		} else {
			machines, err := b.api.List(ctx)
			if err != nil {
				return err
			}
			claims, err := machine0Claims()
			if err != nil {
				return err
			}
			servers := make([]Server, 0, len(machines))
			for _, item := range machines {
				servers = append(servers, b.serverFromMachine(item, claims[item.ID], cfg))
			}
			slug, err := allocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
			if err != nil {
				return err
			}
			name := machine0MachineName(leaseID, slug)
			now := b.now()
			claim.LeaseID = leaseID
			claim.Slug = slug
			claim.Provider = core.FixedMachine0ClaimProvider
			claim.ProviderScope = machine0NameScope(name)
			claim.TargetOS = cfg.TargetOS
			claim.RepoRoot = req.Repo.Root
			claim.ClaimedAt = now.Format(time.RFC3339)
			claim.LastUsedAt = claim.ClaimedAt
			claim.IdleTimeoutSeconds = int(cfg.IdleTimeout.Seconds())
			claim.FixedCreateIntent = &core.FixedCreateIntent{
				Version:       fixedMachine0CreateIntentVersion,
				Fingerprint:   fingerprint,
				ProviderScope: machine0NameScope(name),
				Slug:          slug,
				CreatedAt:     now.Format(time.RFC3339Nano),
				State:         fixedMachine0IntentPrepared,
			}
			if err := persist(); err != nil {
				return err
			}
		}

		intent := claim.FixedCreateIntent
		if intent.State != fixedMachine0IntentPrepared && intent.State != fixedMachine0IntentAcquired {
			return exit(4, "lease_id_conflict: fixed lease %s has invalid create state %q", leaseID, intent.State)
		}
		createdAt, err := time.Parse(time.RFC3339Nano, intent.CreatedAt)
		if err != nil {
			return exit(4, "lease_id_conflict: lease %s has invalid fixed create timestamp", leaseID)
		}
		if cfg.TTL > 0 && b.now().After(createdAt.Add(cfg.TTL)) {
			return exit(4, "lease_id_conflict: fixed create intent for lease %s has expired", leaseID)
		}
		name := machine0MachineName(leaseID, intent.Slug)
		if machine0NameScope(name) != intent.ProviderScope {
			return exit(4, "lease_id_conflict: lease %s is bound to another create intent", leaseID)
		}

		machines, err := b.api.List(ctx)
		if err != nil {
			return err
		}
		if duplicateID := duplicateMachine0ID(machines); duplicateID != "" {
			return exit(4, "lease_id_conflict: Machine0 inventory contains duplicate resource ID %s", duplicateID)
		}
		matching := matchingMachine0Machines(machines, name)
		if len(matching) > 1 {
			return exit(4, "lease_id_conflict: multiple Machine0 machines match fixed lease %s", leaseID)
		}
		pinned, err := fixedMachine0AttemptFromIntent(intent)
		if err != nil {
			return exit(4, "lease_id_conflict: invalid fixed Machine0 attempt for lease %s: %v", leaseID, err)
		}

		var item machine
		if len(matching) == 1 {
			item = matching[0]
			if strings.TrimSpace(item.ID) == "" {
				return exit(4, "lease_id_conflict: Machine0 machine %s matching fixed lease %s has no resource ID", item.Name, leaseID)
			}
			if intent.State == fixedMachine0IntentAcquired || claim.CloudID != "" {
				if claim.CloudID == "" || item.ID != claim.CloudID {
					return exit(4, "lease_id_conflict: fixed lease %s machine %s does not match acquired CloudID %s", leaseID, blank(item.ID, "<empty>"), blank(claim.CloudID, "<empty>"))
				}
			}
			if pinned == nil && claim.CloudID == "" {
				return exit(4, "lease_id_conflict: Machine0 machine %s matches fixed lease %s but has no durable create attempt", item.Name, leaseID)
			}
			if pinned != nil {
				if mismatch := validateFixedMachine0Attempt(item, *pinned); mismatch != "" {
					return exit(4, "lease_id_conflict: Machine0 machine for lease %s does not match its durable create attempt: %s", leaseID, mismatch)
				}
			}
			if machineTerminal(item.Status) {
				return terminalMachineError(item)
			}
			if !machineRunning(item.Status) {
				item, err = b.waitForResolveRunning(ctx, item, cfg.Machine0.CreateTimeout)
				if err != nil {
					return err
				}
			}
		} else {
			if intent.State == fixedMachine0IntentAcquired || claim.CloudID != "" {
				return exit(4, "lease_id_conflict: acquired fixed lease %s is missing its bound Machine0 machine", leaseID)
			}
			if pinned != nil {
				return exit(4, "lease_id_conflict: fixed Machine0 lease %s has an unresolved create attempt; retry after provider inventory converges", leaseID)
			}
			image := cfg.Machine0.Image
			if req.Options.Desktop && strings.TrimSpace(cfg.Machine0.DesktopImage) != "" {
				image = cfg.Machine0.DesktopImage
			}
			attempt := machine0CreateAttempt{
				Name:         name,
				Size:         cfg.Machine0.Size,
				Region:       cfg.Machine0.Region,
				Image:        image,
				ImageVersion: cfg.Machine0.ImageVersion,
				Key:          cfg.Machine0.Key,
				CreatedAt:    b.now().Format(time.RFC3339Nano),
			}
			data, err := json.Marshal(attempt)
			if err != nil {
				return err
			}
			intent.Attempt = map[string]string{"machine0": string(data)}
			if err := persist(); err != nil {
				return err
			}
			fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s name=%s size=%s region=%s image=%s keep=%v fixed=true\n", providerName, leaseID, intent.Slug, name, cfg.Machine0.Size, cfg.Machine0.Region, image, req.Keep)
			createErr := b.api.Create(ctx, createMachineRequest{Name: name, Size: cfg.Machine0.Size, Region: cfg.Machine0.Region, Image: image, ImageVersion: cfg.Machine0.ImageVersion, Key: cfg.Machine0.Key})
			if createErr != nil {
				// Bounded reconciliation preserves at-most-once creation without wedging the lease on an ordinary capacity error.
				item, err = b.reconcileFixedMachine0Create(ctx, name, cfg.Machine0.PollInterval)
				if err != nil {
					return err
				}
				if item.ID == "" {
					intent.Attempt = nil
					if err := persist(); err != nil {
						return err
					}
					return createErr
				}
			}
			item, err = b.waitForRunning(ctx, name, cfg.Machine0.CreateTimeout)
			if err != nil {
				fmt.Fprintf(b.rt.Stderr, "machine0 fixed lease machine %s is retained after readiness failure; release it with `crabbox stop --provider machine0 --id %s`\n", name, leaseID)
				return err
			}
		}

		if item.Name != name {
			return exit(5, "machine0 create returned mismatched machine name: expected %s, found %s", name, item.Name)
		}
		cfg = effectiveMachine0Config(cfg, item)
		claim2 := LeaseClaim{LeaseID: leaseID, Slug: intent.Slug, Provider: providerName, ProviderScope: machineScope(item.ID)}
		server := b.serverFromMachine(item, claim2, cfg)
		server.Labels = machineLabels(cfg, item, leaseID, intent.Slug, req.Keep, b.now())
		lease, err := b.prepareLease(ctx, item, server, leaseID, true)
		if err != nil {
			return err
		}

		claim.CloudID = item.ID
		claim.CloudImmutableID = item.ID
		claim.Slug = intent.Slug
		claim.Provider = core.FixedMachine0ClaimProvider
		claim.ProviderScope = machineScope(item.ID)
		claim.Labels = maps.Clone(lease.Server.Labels)
		claim.SSHHost = lease.SSH.Host
		claim.LastUsedAt = b.now().Format(time.RFC3339)
		if port, parseErr := strconv.Atoi(strings.TrimSpace(lease.SSH.Port)); parseErr == nil {
			claim.SSHPort = port
		}
		intent.State = fixedMachine0IntentAcquired
		if err := persist(); err != nil {
			return err
		}
		acquired = lease
		fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s machine=%s resource=%s state=ready\n", leaseID, item.Name, item.ID)
		return nil
	})
	if err != nil {
		return LeaseTarget{}, err
	}
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

func matchingMachine0Machines(machines []machine, name string) []machine {
	var matching []machine
	for _, item := range machines {
		if item.Name == name {
			matching = append(matching, item)
		}
	}
	return matching
}

func duplicateMachine0ID(machines []machine) string {
	seen := make(map[string]bool, len(machines))
	for _, item := range machines {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		if seen[id] {
			return id
		}
		seen[id] = true
	}
	return ""
}

func fixedMachine0AttemptFromIntent(intent *core.FixedCreateIntent) (*machine0CreateAttempt, error) {
	if len(intent.Attempt) == 0 {
		return nil, nil
	}
	value := intent.Attempt["machine0"]
	if value == "" {
		return nil, errors.New("missing Machine0 attempt payload")
	}
	var attempt machine0CreateAttempt
	if err := json.Unmarshal([]byte(value), &attempt); err != nil {
		return nil, err
	}
	if attempt.Name == "" || attempt.Size == "" || attempt.Region == "" || attempt.Image == "" {
		return nil, errors.New("incomplete Machine0 attempt payload")
	}
	return &attempt, nil
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

func (b *backend) reconcileFixedMachine0Create(ctx context.Context, name string, interval time.Duration) (machine, error) {
	if interval <= 0 {
		interval = time.Second
	}
	for elapsed := time.Duration(0); ; elapsed += interval {
		machines, err := b.api.List(ctx)
		if err != nil {
			return machine{}, err
		}
		if duplicateID := duplicateMachine0ID(machines); duplicateID != "" {
			return machine{}, exit(4, "lease_id_conflict: Machine0 inventory contains duplicate resource ID %s", duplicateID)
		}
		matching := matchingMachine0Machines(machines, name)
		if len(matching) > 1 {
			return machine{}, exit(4, "lease_id_conflict: multiple Machine0 machines match fixed create name %s", name)
		}
		if len(matching) == 1 {
			if strings.TrimSpace(matching[0].ID) == "" {
				return machine{}, exit(4, "lease_id_conflict: Machine0 machine %s matching fixed create name has no resource ID", name)
			}
			return matching[0], nil
		}
		if elapsed >= machine0FixedCreateReconcileWindow {
			return machine{}, nil
		}
		delay := min(interval, machine0FixedCreateReconcileWindow-elapsed)
		if err := b.sleep(ctx, delay); err != nil {
			return machine{}, err
		}
	}
}

func isFixedMachine0Claim(claim core.LeaseClaim) bool {
	return claim.Provider == core.FixedMachine0ClaimProvider && claim.FixedCreateIntent != nil
}

func isMachine0ClaimProvider(provider string) bool {
	return provider == providerName || provider == core.FixedMachine0ClaimProvider
}

func finalizeMachine0ClaimAfterCleanup(claim core.LeaseClaim, action func() error) error {
	if !isFixedMachine0Claim(claim) {
		return core.RemoveLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, action)
	}
	tombstone := fixedMachine0TerminalClaim(claim, time.Now().UTC())
	_, err := core.ReplaceLeaseClaimIfUnchangedDurableAfter(claim.LeaseID, claim, tombstone, action)
	return err
}

func fixedMachine0TerminalClaim(claim core.LeaseClaim, now time.Time) core.LeaseClaim {
	intent := *claim.FixedCreateIntent
	intent.State = fixedMachine0IntentReleased
	intent.Attempt = nil
	intent.FailedAttempts = nil
	return core.LeaseClaim{
		LeaseID:           claim.LeaseID,
		Slug:              intent.Slug,
		Provider:          core.FixedMachine0ClaimProvider,
		ProviderScope:     intent.ProviderScope,
		ClaimedAt:         claim.ClaimedAt,
		LastUsedAt:        now.Format(time.RFC3339),
		FixedCreateIntent: &intent,
	}
}

func validateFixedMachine0TerminalClaim(claim, previous core.LeaseClaim, leaseID string) error {
	intent := claim.FixedCreateIntent
	if claim.LeaseID != leaseID || !isFixedMachine0Claim(claim) ||
		intent.Version != fixedMachine0CreateIntentVersion ||
		strings.TrimSpace(intent.Fingerprint) == "" ||
		strings.TrimSpace(intent.ProviderScope) == "" ||
		intent.ProviderScope != machine0NameScope(machine0MachineName(leaseID, intent.Slug)) ||
		claim.ProviderScope != intent.ProviderScope ||
		claim.Slug != intent.Slug ||
		intent.State != fixedMachine0IntentReleased ||
		claim.CloudID != "" || claim.CloudNumericID != 0 || claim.CloudImmutableID != "" ||
		len(claim.Labels) != 0 || claim.SSHHost != "" || claim.SSHPort != 0 ||
		len(intent.Attempt) != 0 || len(intent.FailedAttempts) != 0 {
		return exit(4, "lease_id_conflict: fixed Machine0 lease %s has an invalid terminal tombstone", leaseID)
	}
	if isFixedMachine0Claim(previous) {
		previousIntent := previous.FixedCreateIntent
		if previous.LeaseID != leaseID ||
			previousIntent.Version != intent.Version ||
			previousIntent.Fingerprint != intent.Fingerprint ||
			previousIntent.ProviderScope != intent.ProviderScope ||
			previousIntent.Slug != intent.Slug {
			return exit(4, "lease_id_conflict: fixed Machine0 lease %s terminal tombstone changed identity", leaseID)
		}
	}
	return nil
}

func (b *backend) retainLeaseClaimAfterRelease(lease LeaseTarget, previous core.LeaseClaim) (bool, error) {
	if normalizeReleasePolicy(b.configForRun().Machine0.ReleasePolicy) == "suspend" {
		return true, nil
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil {
		return false, fmt.Errorf("re-attest released Machine0 claim %s: %w", lease.LeaseID, err)
	}
	if !exists || !isFixedMachine0Claim(claim) {
		if isFixedMachine0Claim(previous) {
			return false, exit(4, "lease_id_conflict: fixed Machine0 lease %s has no valid terminal tombstone after release", lease.LeaseID)
		}
		return false, nil
	}
	if err := validateFixedMachine0TerminalClaim(claim, previous, lease.LeaseID); err != nil {
		return false, err
	}
	return true, nil
}
