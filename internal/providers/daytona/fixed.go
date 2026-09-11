package daytona

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"time"

	api "github.com/daytonaio/daytona/libs/api-client-go"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

var fixedDaytonaLeaseKind = core.FixedLeaseKind{ClaimProvider: core.FixedDaytonaClaimProvider, IntentVersion: 1, Label: "Daytona"}

// A digest can be opaque metadata on an ordinary lease. The fixed producer
// explicitly marks its owner dialect; a nonce without that marker is invalid
// fixed state and must not be adopted as an ordinary lease.
func hasFixedDaytonaOwnershipLabels(labels map[string]string) bool {
	return labels["fixed_claim_provider"] != "" || labels["fixed_attempt"] != ""
}

func (b *daytonaLeaseBackend) SupportsRequestedLeaseID() bool {
	return !core.ShouldUseCoordinator(b.cfg, (Provider{}).Spec())
}
func (b *daytonaLeaseBackend) SupportsRequestedCheckpointID() bool {
	return b.SupportsRequestedLeaseID()
}

func fixedDaytonaContext(ctx context.Context, client daytonaAPI) (string, string, error) {
	identity, ok := client.(interface {
		fixedOrganization(context.Context) (string, string, error)
	})
	if !ok {
		return "", "", exit(4, "Daytona client has no organization identity contract")
	}
	endpoint, organization, err := identity.fixedOrganization(ctx)
	if err != nil {
		return "", "", err
	}
	return fixedDaytonaScope(endpoint, organization)
}

func fixedDaytonaResourceContext(client daytonaAPI, organization string) (string, string, error) {
	identity, ok := client.(interface{ fixedSelection() (string, string) })
	if !ok {
		return "", "", exit(4, "Daytona client has no frozen selection identity")
	}
	endpoint, selected := identity.fixedSelection()
	if selected != "" && selected != organization {
		return "", "", exit(4, "Daytona resource organization differs from the selected organization")
	}
	return fixedDaytonaScope(endpoint, organization)
}

func fixedDaytonaScope(endpoint, organization string) (string, string, error) {
	if endpoint == "" || organization == "" {
		return "", "", exit(4, "Daytona fixed leases require an identified organization")
	}
	data, err := json.Marshal([]string{endpoint, organization})
	if err != nil {
		return "", "", err
	}
	return fmt.Sprintf("daytona:organization:v1:%x", sha256.Sum256(data)), organization, nil
}

func fixedDaytonaFingerprint(cfg Config, req AcquireRequest, snapshotID string) (string, error) {
	data, err := json.Marshal(struct {
		Snapshot, Selector, User, Target, WorkRoot, Slug, Class, Architecture string
		Keep                                                                  bool
		TTL, Idle                                                             time.Duration
	}{snapshotID, cfg.Daytona.Snapshot, daytonaUser(cfg), cfg.Daytona.Target, daytonaWorkRoot(cfg), normalizeLeaseSlug(req.RequestedSlug), cfg.Class, cfg.Architecture, req.Keep, cfg.TTL, cfg.IdleTimeout})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func (b *daytonaLeaseBackend) acquireFixed(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	cfg := b.cfg
	if err := validateDaytonaCreateConfig(cfg); err != nil {
		return LeaseTarget{}, err
	}
	var client daytonaAPI
	var snapshot *api.SnapshotDto
	var observed *api.Sandbox
	var scope, organization string
	fresh := false
	lease, err := core.AcquireFixedLease(core.FixedAcquireOptions{
		Kind: fixedDaytonaLeaseKind, LeaseID: req.RequestedLeaseID, CheckpointID: req.RequestedCheckpointID,
		RepoRoot: req.Repo.Root, Reclaim: req.Reclaim, TargetOS: targetLinux, TTL: cfg.TTL, IdleTimeout: cfg.IdleTimeout,
	}, func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
		// This distinct format never erases submitted attempts. Its pristine
		// prepared row therefore proves a crash happened before POST admission.
		fresh = !exists || neverSubmittedDaytonaClaim(*claim)
		var err error
		client, err = newDaytonaClient(cfg, b.rt)
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		switch {
		case !fresh:
			// Carry the exact native child through replay, independent of its retired source.
			observed, err = loadFixedDaytonaSandbox(ctx, client, *claim)
			if err == nil {
				scope, organization, err = fixedDaytonaResourceContext(client, observed.GetOrganizationId())
			}
		case req.CheckpointSource != nil:
			// The required private source read also attests organization after all
			// live workers drain. A general snapshot cannot establish that scope.
			snapshot, err = selectClassSnapshot(ctx, client, cfg)
			if err == nil && snapshot == nil {
				snapshot, err = client.GetSnapshot(ctx, strings.TrimSpace(cfg.Daytona.Snapshot))
			}
			if err == nil {
				err = validateDaytonaForkSnapshot(snapshot, req.CheckpointSource)
			}
			if err == nil {
				scope, organization, err = fixedDaytonaResourceContext(client, snapshot.GetOrganizationId())
			}
		default:
			scope, organization, err = fixedDaytonaContext(ctx, client)
		}
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		if exists && (claim.Provider != core.FixedDaytonaClaimProvider || claim.ProviderScope != scope || claim.FixedCreateIntent == nil ||
			(len(claim.FixedCreateIntent.Attempt) != 0 && organization != claim.FixedCreateIntent.Attempt["organization"])) {
			return core.FixedLeaseBinding{}, exit(4, "lease_id_conflict: Daytona API, organization, or credential context changed")
		}
		snapshotID := ""
		if exists {
			// A submitted attempt pins its immutable source. Completed acquisition
			// permits replay after retirement; prepared attempts still attest below.
			snapshotID = claim.FixedCreateIntent.Attempt["snapshot_id"]
			if source := req.CheckpointSource; source != nil && snapshotID != "" {
				attempt := claim.FixedCreateIntent.Attempt
				if snapshotID != source.ImageID || attempt["snapshot"] != source.Name || attempt["organization"] != source.Metadata["organization"] {
					return core.FixedLeaseBinding{}, exit(4, "Daytona checkpoint source differs from the fixed create attempt")
				}
			}
		}
		if snapshotID == "" || claim.FixedCreateIntent.State == "prepared" {
			if snapshot == nil {
				selection := cfg
				if snapshotID != "" {
					// Only completed acquisition records successful shape attestation.
					// Incomplete retries must verify their pinned source again.
					selection.Daytona.Snapshot = snapshotID
				}
				snapshot, err = selectClassSnapshot(ctx, client, selection)
				if err == nil && snapshot == nil {
					snapshot, err = client.GetSnapshot(ctx, strings.TrimSpace(selection.Daytona.Snapshot))
				}
			}
			if err != nil {
				if snapshotID != "" {
					return core.FixedLeaseBinding{}, fmt.Errorf("Daytona fixed acquisition remains incomplete: source snapshot %s is unavailable; retry when available or stop fixed lease %s: %w", snapshotID, claim.LeaseID, err)
				}
				return core.FixedLeaseBinding{}, err
			}
			if snapshot == nil || snapshot.GetId() == "" || snapshot.GetName() == "" || snapshot.GetState() != api.SNAPSHOTSTATE_ACTIVE {
				return core.FixedLeaseBinding{}, exit(4, "Daytona fixed acquisition requires an active, identified snapshot")
			}
			if snapshotID != "" && (snapshot.GetId() != snapshotID || snapshot.GetName() != claim.FixedCreateIntent.Attempt["snapshot"]) {
				return core.FixedLeaseBinding{}, exit(4, "Daytona incomplete acquisition source differs from its fixed create attempt")
			}
			if organization != "" && !snapshot.GetGeneral() && snapshot.GetOrganizationId() != organization {
				return core.FixedLeaseBinding{}, exit(4, "Daytona snapshot organization mismatch")
			}
			if req.CheckpointSource != nil {
				if err := validateDaytonaForkSnapshot(snapshot, req.CheckpointSource); err != nil {
					return core.FixedLeaseBinding{}, err
				}
			}
			snapshotID = snapshot.GetId()
		}
		fingerprint, err := fixedDaytonaFingerprint(cfg, req, snapshotID)
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		binding := core.FixedLeaseBinding{ProviderScope: scope, Fingerprint: fingerprint}
		if !exists {
			sandboxes, err := client.ListCrabboxSandboxes(ctx)
			if err != nil {
				return binding, err
			}
			for i := range sandboxes {
				if id, owned := daytonaSandboxOwnership(&sandboxes[i]); owned && id == req.RequestedLeaseID {
					return binding, exit(4, "lease_id_conflict: Daytona lease already exists without its create intent")
				}
			}
			binding.Slug, err = allocateDirectLeaseSlug(req.RequestedLeaseID, req.RequestedSlug, daytonaSandboxesToServers(sandboxes))
			if err != nil {
				return binding, err
			}
		}
		return binding, nil
	}, func(ctx context.Context, claim *core.LeaseClaim, intent *core.FixedCreateIntent, persist func() error) (LeaseTarget, error) {
		if err := core.AuthorizeCheckpointRelease(*claim, ""); err != nil {
			return LeaseTarget{}, err
		}
		var sandbox *api.Sandbox
		if fresh {
			createdAt, _ := time.Parse(time.RFC3339Nano, intent.CreatedAt)
			remaining := time.Until(createdAt.Add(cfg.TTL))
			if remaining <= 0 {
				return LeaseTarget{}, exit(4, "Daytona fixed create intent expired before submission")
			}
			cfg.Daytona.Snapshot = snapshot.GetId()
			body := daytonaCreateBody(cfg, req.RequestedLeaseID, intent.Slug, req.Keep, createdAt)
			// Replay retains the original lease deadline, including native TTL.
			body.AdditionalProperties["ttlMinutes"] = durationMinutesCeil(remaining)
			labels := body.GetLabels()
			labels["fixed_claim_provider"] = core.FixedDaytonaClaimProvider
			labels["fixed_intent_sha256"], labels["fixed_attempt"] = intent.Fingerprint, rand.Text()
			body.SetLabels(labels)
			intent.Attempt = map[string]string{"name": body.GetName(), "snapshot": snapshot.GetName(), "snapshot_id": snapshot.GetId(), "user": daytonaUser(cfg), "target": cfg.Daytona.Target, "organization": organization, "nonce": labels["fixed_attempt"]}
			claim.Labels = maps.Clone(labels)
			// Persist before POST; a missing response never authorizes another create.
			if err := persist(); err != nil {
				return LeaseTarget{}, err
			}
			var err error
			sandbox, err = client.CreateSandbox(ctx, *body)
			if err != nil {
				return LeaseTarget{}, fmt.Errorf("Daytona create outcome uncertain; replay or stop fixed lease %s: %w", claim.LeaseID, err)
			}

			// Pin an observed UUID before attestation so a rejected response cannot
			// later adopt a different sandbox that reuses this attempt's name.
			if sandbox != nil && sandbox.GetId() != "" {
				claim.CloudID = sandbox.GetId()
				if err := persist(); err != nil {
					return LeaseTarget{}, err
				}
			}
		} else {
			sandbox = observed
		}
		if err := validateFixedDaytonaSandbox(*claim, sandbox); err != nil {
			return LeaseTarget{}, err
		}
		if intent.State == "prepared" {
			if err := validateClassSandbox(sandbox, snapshot); err != nil {
				return LeaseTarget{}, err
			}
		}
		if intent.Attempt["resolved_target"] == "" {
			// Native create resolves region names. Freeze its returned region ID;
			// replay must not compare a requested name to a different native dialect.
			intent.Attempt["resolved_target"] = sandbox.GetTarget()
		}
		claim.CloudID, claim.CloudImmutableID = sandbox.GetId(), sandbox.GetId()
		intent.Attempt["organization"] = sandbox.GetOrganizationId()
		if err := persist(); err != nil {
			return LeaseTarget{}, err
		}
		if !daytonaStateReady(daytonaSandboxState(sandbox)) {
			if sandbox.GetState() == api.SANDBOXSTATE_STOPPED || sandbox.GetState() == api.SANDBOXSTATE_ARCHIVED {
				if _, err := client.StartSandbox(ctx, sandbox.GetId()); err != nil {
					return LeaseTarget{}, err
				}
			}
			var err error
			sandbox, err = waitForDaytonaReady(ctx, client, sandbox.GetId(), 5*time.Minute)
			if err != nil {
				return LeaseTarget{}, err
			}
			if err := validateFixedDaytonaSandbox(*claim, sandbox); err != nil {
				return LeaseTarget{}, err
			}
		}
		server := daytonaSandboxToServer(sandbox)
		server.ImmutableID = sandbox.GetId()
		target, err := daytonaSSHTargetFor(ctx, client, cfg, server)
		if err != nil {
			return LeaseTarget{}, err
		}
		if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "daytona ssh", bootstrapWaitTimeout(cfg)); err != nil {
			return LeaseTarget{}, err
		}
		return LeaseTarget{Server: server, SSH: target, LeaseID: claim.LeaseID}, nil
	}, ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.OnAcquired != nil {
		if err := req.OnAcquired(lease); err != nil {
			return LeaseTarget{}, err
		}
	}
	return lease, nil
}

func validateFixedDaytonaSandbox(claim core.LeaseClaim, sandbox *api.Sandbox) error {
	intent := claim.FixedCreateIntent
	if intent == nil || intent.Version != 1 || intent.Fingerprint == "" || (intent.State != "prepared" && intent.State != "acquired") || claim.Provider != core.FixedDaytonaClaimProvider || claim.ProviderScope != intent.ProviderScope || claim.Slug != intent.Slug || sandbox == nil {
		return exit(4, "Daytona fixed lease has an invalid ownership claim")
	}
	attempt := intent.Attempt
	id, owned := daytonaSandboxOwnership(sandbox)
	if !owned || id != claim.LeaseID || sandbox.GetName() != attempt["name"] || sandbox.GetId() == "" || sandbox.GetOrganizationId() == "" ||
		(claim.CloudID != "" && sandbox.GetId() != claim.CloudID) || (claim.CloudImmutableID != "" && sandbox.GetId() != claim.CloudImmutableID) ||
		sandbox.GetLabels()["fixed_claim_provider"] != core.FixedDaytonaClaimProvider ||
		sandbox.GetLabels()["fixed_intent_sha256"] != intent.Fingerprint || attempt["nonce"] == "" || sandbox.GetLabels()["fixed_attempt"] != attempt["nonce"] ||
		(sandbox.GetSnapshot() != attempt["snapshot"] && sandbox.GetSnapshot() != attempt["snapshot_id"]) || sandbox.GetUser() != attempt["user"] ||
		(attempt["organization"] != "" && sandbox.GetOrganizationId() != attempt["organization"]) || sandbox.GetTarget() == "" ||
		(attempt["resolved_target"] != "" && sandbox.GetTarget() != attempt["resolved_target"]) {
		return exit(4, "lease_id_conflict: Daytona sandbox does not match the exact fixed create attempt")
	}
	return nil
}

func loadFixedDaytonaSandbox(ctx context.Context, client daytonaAPI, claim core.LeaseClaim) (*api.Sandbox, error) {
	if claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State == "released" || claim.FixedCreateIntent.Attempt["name"] == "" {
		return nil, exit(4, "Daytona fixed lease has no active create attempt; it cannot allocate a replacement")
	}
	// The indexed marker is persisted before DELETE admission. A lost response
	// may leave the native sandbox looking ready while deletion is still queued.
	if claim.FixedCreateIntent.Attempt["deletion_indexed_id"] != "" || claim.FixedCreateIntent.Attempt["deletion_acknowledged_id"] != "" {
		return nil, exit(4, "Daytona fixed lease %s has entered cleanup and cannot be reused; retry stop to reconcile deletion", claim.LeaseID)
	}

	lookup := claim.CloudID
	if lookup == "" {
		lookup = claim.FixedCreateIntent.Attempt["name"]
	}
	sandbox, err := client.GetSandbox(ctx, lookup)
	if err != nil {
		return nil, fmt.Errorf("Daytona fixed lease %s remains unresolved; no replacement allocated: %w", claim.LeaseID, err)
	}
	if sandbox == nil {
		return nil, exit(4, "Daytona fixed resource was not returned")
	}
	scope, organization, err := fixedDaytonaResourceContext(client, sandbox.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	if claim.ProviderScope != scope || organization != claim.FixedCreateIntent.Attempt["organization"] {
		return nil, exit(4, "Daytona fixed lease API or organization changed")
	}
	return sandbox, validateFixedDaytonaSandbox(claim, sandbox)
}

func (b *daytonaLeaseBackend) releaseFixed(ctx context.Context, expected core.LeaseClaim, checkpointID string, absenceOnly bool) error {
	return core.WithDurableLeaseClaimLockContext(ctx, expected.LeaseID, func(claim *LeaseClaim, exists bool, persist func() error) error {
		if !exists || !reflect.DeepEqual(*claim, expected) {
			return exit(4, "Daytona fixed lease claim changed before release; retry")
		}
		if !fixedDaytonaLeaseKind.IsFixedClaim(*claim) || claim.FixedCreateIntent.Version != fixedDaytonaLeaseKind.IntentVersion {
			return exit(4, "Daytona fixed lease format is not recognized; retain its ownership record for reconciliation")
		}
		if claim.FixedCreateIntent.State == "released" {
			return fixedDaytonaLeaseKind.ValidateTerminalClaim(*claim, expected, claim.LeaseID, nil)
		}
		if err := core.AuthorizeCheckpointRelease(*claim, checkpointID); err != nil {
			return err
		}
		if absenceOnly && claim.FixedCreateIntent.State != "acquired" {
			return exit(4, "Daytona absence reconciliation requires a completed fixed acquisition")
		}
		if !neverSubmittedDaytonaClaim(*claim) {
			apiClient, err := newDaytonaClient(b.cfg, b.rt)
			if err != nil {
				return err
			}
			client, ok := apiClient.(fixedDaytonaDeletionAPI)
			if !ok {
				return exit(4, "Daytona client cannot attest fixed resource deletion")
			}
			if claim.CloudID == "" {
				sandbox, err := loadFixedDaytonaSandbox(ctx, client, *claim)
				if err != nil {
					return err
				}
				// DELETE can rename/hide this child before returning. A lost create
				// response must bind the observed UUID before any cleanup request.
				claim.CloudID, claim.CloudImmutableID = sandbox.GetId(), sandbox.GetId()
				if err := persist(); err != nil {
					return err
				}
			}
			if err := deleteFixedDaytonaSandbox(ctx, client, claim, persist, absenceOnly); err != nil {
				return err
			}
		}
		*claim = fixedDaytonaLeaseKind.TerminalClaim(*claim, time.Now().UTC())
		return persist()
	})
}

func (b *daytonaLeaseBackend) reclaimFixed(ctx context.Context, claim LeaseClaim, repoRoot string, reclaim bool) (LeaseClaim, error) {
	if !reclaim {
		return claim, nil
	}
	if repoRoot == "" {
		return claim, exit(2, "Daytona fixed reclaim requires the current repository")
	}
	if err := core.AuthorizeCheckpointRelease(claim, ""); err != nil {
		return claim, err
	}
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return claim, err
	}
	sandbox, err := loadFixedDaytonaSandbox(ctx, client, claim)
	if err != nil {
		return claim, err
	}
	// Publish the explicit transfer before taking a shared execution fence.
	// The original claim CAS prevents an awaited native read from stealing a
	// lease whose owner changed while the request was being validated.
	return core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfterContext(
		ctx, claim.LeaseID, claim.Slug, b.cfg, claim.ProviderScope, daytonaSandboxToServer(sandbox), SSHTarget{},
		repoRoot, b.cfg.IdleTimeout, true, claim, true,
		func() error { return core.AuthorizeCheckpointRelease(claim, "") },
	)
}

type fixedDaytonaDeletionAPI interface {
	daytonaAPI
	fixedSelection() (endpoint, organization string)
	findPendingDeletion(context.Context, string) (*api.Sandbox, error)
	attestDeletionOrganization(context.Context, string) error
	requestSandboxDeletion(context.Context, string) (*api.Sandbox, error)
}

func validateFixedDaytonaDeletionIdentity(client fixedDaytonaDeletionAPI, claim LeaseClaim, sandbox *api.Sandbox) error {
	endpoint, selectedOrganization := client.fixedSelection()
	attempt := claim.FixedCreateIntent.Attempt
	if claim.ProviderScope != claim.FixedCreateIntent.ProviderScope || claim.Slug != claim.FixedCreateIntent.Slug ||
		(claim.CloudImmutableID != "" && claim.CloudImmutableID != claim.CloudID) || attempt["organization"] == "" ||
		sandbox == nil || sandbox.GetId() == "" || (claim.CloudID != "" && sandbox.GetId() != claim.CloudID) || sandbox.GetOrganizationId() != attempt["organization"] ||
		(selectedOrganization != "" && selectedOrganization != sandbox.GetOrganizationId()) ||
		sandbox.GetLabels()["fixed_claim_provider"] != core.FixedDaytonaClaimProvider ||
		sandbox.GetLabels()["lease"] != claim.LeaseID || sandbox.GetLabels()["fixed_intent_sha256"] != claim.FixedCreateIntent.Fingerprint ||
		attempt["nonce"] == "" || sandbox.GetLabels()["fixed_attempt"] != attempt["nonce"] {
		return exit(4, "Daytona deletion response does not match the exact fixed resource")
	}
	if leaseID, owned := daytonaSandboxOwnership(sandbox); !owned || leaseID != claim.LeaseID {
		return exit(4, "Daytona deletion response lost fixed lease ownership")
	}
	data, err := json.Marshal([]string{endpoint, sandbox.GetOrganizationId()})
	if err != nil {
		return err
	}
	if claim.ProviderScope != fmt.Sprintf("daytona:organization:v1:%x", sha256.Sum256(data)) {
		return exit(4, "Daytona deletion endpoint or organization changed")
	}
	return nil
}

func deleteFixedDaytonaSandbox(ctx context.Context, client fixedDaytonaDeletionAPI, claim *LeaseClaim, persist func() error, absenceOnly bool) error {
	intent := claim.FixedCreateIntent
	if claim.CloudID == "" || intent == nil || (intent.State != "prepared" && intent.State != "acquired") {
		return exit(4, "Daytona fixed cleanup requires its durably observed resource UUID")
	}
	if claim.ProviderScope != intent.ProviderScope || claim.Slug != intent.Slug || intent.Fingerprint == "" || intent.Attempt["nonce"] == "" {
		return exit(4, "Daytona fixed cleanup has an invalid ownership claim")
	}
	for _, key := range []string{"deletion_indexed_id", "deletion_acknowledged_id"} {
		if value := intent.Attempt[key]; value != "" && value != claim.CloudID {
			return exit(4, "Daytona deletion witness does not match the fixed resource UUID")
		}
	}
	endpoint, selected := client.fixedSelection()
	scope, _, err := fixedDaytonaScope(endpoint, intent.Attempt["organization"])
	if err != nil || scope != claim.ProviderScope || selected != "" && selected != intent.Attempt["organization"] {
		return exit(4, "Daytona deletion endpoint or organization changed")
	}
	if err := client.attestDeletionOrganization(ctx, intent.Attempt["organization"]); err != nil {
		return err
	}
	if absenceOnly || intent.Attempt["deletion_acknowledged_id"] == "" {
		sandbox, err := client.GetSandbox(ctx, claim.CloudID)
		if err != nil {
			// Native TTL or external deletion can finish without our DELETE witness.
			// Only a completed exact acquisition plus current scoped database absence
			// can retire that claim. A clock deadline or GET alone proves neither.
			if daytonaIsNotFoundError(err) && intent.State == "acquired" && claim.CloudImmutableID == claim.CloudID &&
				claim.Labels["fixed_claim_provider"] == core.FixedDaytonaClaimProvider &&
				claim.Labels["lease"] == claim.LeaseID && claim.Labels["fixed_intent_sha256"] == intent.Fingerprint &&
				claim.Labels["fixed_attempt"] == intent.Attempt["nonce"] {
				pending, lookupErr := client.findPendingDeletion(ctx, claim.CloudID)
				if lookupErr != nil {
					return lookupErr
				}
				if pending == nil {
					return nil
				}
				return exit(4, "Daytona fixed resource remains in database inventory; retain its claim")
			}
			return fmt.Errorf("Daytona fixed deletion has no acknowledged outcome; retain lease %s: %w", claim.LeaseID, err)
		}
		if absenceOnly {
			return exit(4, "Daytona fixed resource is still present; inspection cannot delete it")
		}
		if err := validateFixedDaytonaDeletionIdentity(client, *claim, sandbox); err != nil {
			return err
		}
		// Keep the existing durable cleanup-entry marker, now attested against
		// the database inventory before DELETE rather than the search index.
		indexed, err := client.findPendingDeletion(ctx, claim.CloudID)
		if err != nil {
			return err
		}
		if indexed == nil {
			return exit(4, "Daytona fixed resource is missing from database inventory; retain its claim and retry stop")
		}
		if err := validateFixedDaytonaDeletionIdentity(client, *claim, indexed); err != nil {
			return err
		}
		intent.Attempt["deletion_indexed_id"] = claim.CloudID
		if err := persist(); err != nil {
			return err
		}
		if sandbox.GetDesiredState() != api.SANDBOXDESIREDSTATE_DESTROYED {
			if err := validateFixedDaytonaSandbox(*claim, sandbox); err != nil {
				return err
			}
			sandbox, err = client.requestSandboxDeletion(ctx, claim.CloudID)
			if err != nil {
				return err
			}
			if err := validateFixedDaytonaDeletionIdentity(client, *claim, sandbox); err != nil {
				return err
			}
		}
		if sandbox.GetDesiredState() != api.SANDBOXDESIREDSTATE_DESTROYED {
			return exit(4, "Daytona did not acknowledge destruction of the fixed resource")
		}
		intent.Attempt["deletion_acknowledged_id"] = claim.CloudID
		if err := persist(); err != nil {
			return err
		}
	}
	if intent.Attempt["deletion_indexed_id"] != claim.CloudID {
		return exit(4, "Daytona fixed deletion has no prior inventory identity; retain its claim")
	}
	for {
		sandbox, err := client.GetSandbox(ctx, claim.CloudID)
		if err != nil && !daytonaIsNotFoundError(err) {
			return err
		}
		if err == nil {
			if err := validateFixedDaytonaDeletionIdentity(client, *claim, sandbox); err != nil {
				return err
			}
		} else {
			// GET also hides failed deletions. Freshly attest the original account
			// and inspect failure-inclusive inventory before accepting API-visible
			// removal; this is not a positive physical-destruction observation.
			if err := client.attestDeletionOrganization(ctx, intent.Attempt["organization"]); err != nil {
				return err
			}
			sandbox, err = client.findPendingDeletion(ctx, claim.CloudID)
			if err != nil {
				return err
			}
			if sandbox == nil {
				return nil
			}
			if err := validateFixedDaytonaDeletionIdentity(client, *claim, sandbox); err != nil {
				return err
			}
		}
		if sandbox.GetState() == api.SANDBOXSTATE_ERROR || sandbox.GetState() == api.SANDBOXSTATE_BUILD_FAILED {
			return exit(4, "Daytona fixed deletion failed with state=%s; retain lease %s for reconciliation", sandbox.GetState(), claim.LeaseID)
		}
		if err := shared.SleepContext(ctx, time.Second); err != nil {
			return err
		}
	}
}

func neverSubmittedDaytonaClaim(claim core.LeaseClaim) bool {
	intent := claim.FixedCreateIntent
	if !fixedDaytonaLeaseKind.IsFixedClaim(claim) || intent.Version != 1 || intent.State != "prepared" ||
		!isCanonicalLeaseID(claim.LeaseID) || intent.Slug == "" || claim.Slug != intent.Slug || claim.ProviderScope != intent.ProviderScope ||
		len(intent.Attempt) != 0 || len(intent.FailedAttempts) != 0 || len(claim.Labels) != 0 ||
		claim.CloudID != "" || claim.CloudImmutableID != "" || claim.CloudNumericID != 0 || claim.SSHHost != "" || claim.SSHPort != 0 ||
		claim.StaticHost != "" || claim.StaticUser != "" || claim.StaticPort != "" || claim.StaticWorkRoot != "" {
		return false
	}
	scope, ok := strings.CutPrefix(intent.ProviderScope, "daytona:organization:v1:")
	scopeHash, scopeErr := hex.DecodeString(scope)
	fingerprint, fingerprintErr := hex.DecodeString(intent.Fingerprint)
	_, timeErr := time.Parse(time.RFC3339Nano, intent.CreatedAt)
	return ok && scopeErr == nil && len(scopeHash) == sha256.Size && fingerprintErr == nil && len(fingerprint) == sha256.Size && timeErr == nil
}

func (b *daytonaLeaseBackend) RetainLeaseClaimAfterReleaseWithClaim(lease LeaseTarget, previous core.LeaseClaim) (bool, error) {
	return fixedDaytonaLeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, hasFixedDaytonaOwnershipLabels(lease.Server.Labels), nil, nil)
}

func (Provider) PrepareLeaseClaimEndpoint(existing core.LeaseClaim, provider, slug string, server core.Server, _ bool) (core.Server, error) {
	if existing.FixedCreateIntent != nil {
		if err := core.AuthorizeCheckpointRelease(existing, ""); err != nil {
			return Server{}, err
		}
		if !fixedDaytonaLeaseKind.IsFixedClaim(existing) || provider != daytonaProvider || slug != existing.Slug || existing.FixedCreateIntent.State != "acquired" || server.CloudID != existing.CloudID || server.Labels["fixed_claim_provider"] != core.FixedDaytonaClaimProvider || server.Labels["fixed_intent_sha256"] != existing.FixedCreateIntent.Fingerprint || server.Labels["fixed_attempt"] != existing.FixedCreateIntent.Attempt["nonce"] {
			return Server{}, exit(4, "refusing to rewrite Daytona fixed lease identity")
		}
	}
	return server, nil
}

func (b *daytonaLeaseBackend) AuthorizeStatusTouchClaim(ctx context.Context, lease LeaseTarget, claim core.LeaseClaim) error {
	if claim.LeaseID != lease.LeaseID || (claim.Provider != daytonaProvider && claim.Provider != core.FixedDaytonaClaimProvider) || lease.Server.CloudID == "" || lease.Server.CloudID != claim.CloudID {
		return exit(4, "Daytona lifecycle touch requires an exact source lease claim")
	}
	if claim.FixedCreateIntent == nil {
		if claim.ProviderScope != core.ProviderClaimScope(daytonaProvider, b.cfg) {
			return exit(4, "Daytona lifecycle touch provider scope mismatch")
		}
		return nil
	}
	if err := core.AuthorizeCheckpointRelease(claim, ""); err != nil {
		return err
	}
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	_, err = loadFixedDaytonaSandbox(ctx, client, claim)
	return err
}
