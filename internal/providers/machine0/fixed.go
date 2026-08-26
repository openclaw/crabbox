package machine0

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		if err := b.validateCatalogSelection(ctx, cfg.Machine0.Size, cfg.Machine0.Region); err != nil {
			return core.FixedLeaseBinding{}, err
		}
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
	}, func(ctx context.Context, claim *core.LeaseClaim, intent *core.FixedCreateIntent, persist func() error) (LeaseTarget, error) {
		var acquired LeaseTarget
		err := func() error {
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
				if pinned != nil && strings.TrimSpace(pinned.Key) != "" &&
					(item.Key == nil || strings.TrimSpace(item.Key.Name) == "" && strings.TrimSpace(item.Key.FileName) == "") {
					detail, err := b.api.Get(ctx, item.Name)
					if err != nil {
						return err
					}
					if detail.ID != item.ID || detail.Name != item.Name {
						return exit(4, "lease_id_conflict: Machine0 machine detail for fixed lease %s does not match its inventory resource identity", leaseID)
					}
					if detail.Key == nil || strings.TrimSpace(detail.Key.Name) != strings.TrimSpace(pinned.Key) {
						return exit(4, "lease_id_conflict: Machine0 machine detail for fixed lease %s does not match its durable selected SSH key %q", leaseID, pinned.Key)
					}
					if item.Key != nil && strings.TrimSpace(item.Key.Type) != "" {
						if strings.TrimSpace(detail.Key.Type) != "" && !strings.EqualFold(strings.TrimSpace(detail.Key.Type), strings.TrimSpace(item.Key.Type)) {
							return exit(4, "lease_id_conflict: Machine0 machine detail for fixed lease %s does not match its inventory SSH key type", leaseID)
						}
						detail.Key.Type = item.Key.Type
					}
					item.Key = detail.Key
				}
			} else {
				if intent.State == fixedMachine0IntentAcquired || claim.CloudID != "" {
					return exit(4, "lease_id_conflict: acquired fixed lease %s is missing its bound Machine0 machine", leaseID)
				}
				if pinned != nil {
					return exit(4, "lease_id_conflict: fixed Machine0 lease %s has an unresolved create attempt; retry after provider inventory converges", leaseID)
				}
				if err := b.preflightSSHKey(ctx, cfg.Machine0.Key); err != nil {
					return err
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

			claim.ProviderScope = machineScope(item.ID)
			acquired = lease
			return nil
		}()
		return acquired, err
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
	if normalizeReleasePolicy(b.configForRun().Machine0.ReleasePolicy) == "suspend" {
		return true, nil
	}
	return fixedMachine0LeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, false, validateFixedMachine0TerminalClaimExtra, nil)
}
