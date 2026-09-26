package daytona

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	api "github.com/daytona/clients/api-client-go"
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

func daytonaAccountContext(ctx context.Context, client daytonaAPI) (string, string, error) {
	identity, ok := client.(interface {
		fixedOrganization(context.Context) (string, string, error)
	})
	if !ok {
		return "", "", core.Exit(4, "Daytona client has no organization identity contract")
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
		return "", "", core.Exit(4, "Daytona client has no frozen selection identity")
	}
	endpoint, selected := identity.fixedSelection()
	if selected != "" && selected != organization {
		return "", "", core.Exit(4, "Daytona resource organization differs from the selected organization")
	}
	return fixedDaytonaScope(endpoint, organization)
}

func fixedDaytonaScope(endpoint, organization string) (string, string, error) {
	if endpoint == "" || organization == "" {
		return "", "", core.Exit(4, "Daytona fixed leases require an identified organization")
	}
	digest, err := core.FixedIntentFingerprint("", []string{endpoint, organization})
	if err != nil {
		return "", "", err
	}
	return "daytona:organization:v1:" + digest, organization, nil
}

func fixedDaytonaFingerprint(cfg core.Config, req core.AcquireRequest, snapshotID string) (string, error) {
	return core.FixedIntentFingerprint("", struct {
		Snapshot, Selector, User, Target, WorkRoot, Slug, Class, Architecture string
		Keep                                                                  bool
		TTL, Idle                                                             time.Duration
	}{snapshotID, cfg.Daytona.Snapshot, daytonaUser(cfg), cfg.Daytona.Target, daytonaWorkRoot(cfg), core.NormalizeLeaseSlug(req.RequestedSlug), cfg.Class, cfg.Architecture, req.Keep, cfg.TTL, cfg.IdleTimeout})
}

func (b *daytonaLeaseBackend) acquireFixed(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	cfg := b.cfg
	if err := validateDaytonaCreateConfig(cfg); err != nil {
		return core.LeaseTarget{}, err
	}
	var client daytonaAPI
	var snapshot *api.SnapshotDto
	var observed *api.Sandbox
	var createBody *api.CreateSandbox
	var scope, organization string
	fresh := false
	lease, err := core.AcquireFixedResource(ctx, core.FixedAcquireOptions{
		Kind: fixedDaytonaLeaseKind, LeaseID: req.RequestedLeaseID, CheckpointID: req.RequestedCheckpointID,
		RepoRoot: req.Repo.Root, Reclaim: req.Reclaim, TargetOS: targetLinux, TTL: cfg.TTL, IdleTimeout: cfg.IdleTimeout,
	}, core.FixedLeaseOperations[*api.Sandbox]{Admission: &core.FixedAdmission{}, DescribeIntent: func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
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
			scope, organization, err = daytonaAccountContext(ctx, client)
		}
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		if exists && (claim.Provider != core.FixedDaytonaClaimProvider || claim.ProviderScope != scope || claim.FixedCreateIntent == nil ||
			(len(claim.FixedCreateIntent.Attempt) != 0 && organization != claim.FixedCreateIntent.Attempt["organization"])) {
			return core.FixedLeaseBinding{}, core.Exit(4, "lease_id_conflict: Daytona API, organization, or credential context changed")
		}
		snapshotID := ""
		if exists {
			// A submitted attempt pins its immutable source. Completed acquisition
			// permits replay after retirement; prepared attempts still attest below.
			snapshotID = claim.FixedCreateIntent.Attempt["snapshot_id"]
			if source := req.CheckpointSource; source != nil && snapshotID != "" {
				attempt := claim.FixedCreateIntent.Attempt
				if snapshotID != source.ImageID || attempt["snapshot"] != source.Name || attempt["organization"] != source.Metadata["organization"] {
					return core.FixedLeaseBinding{}, core.Exit(4, "Daytona checkpoint source differs from the fixed create attempt")
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
				return core.FixedLeaseBinding{}, core.Exit(4, "Daytona fixed acquisition requires an active, identified snapshot")
			}
			if snapshotID != "" && (snapshot.GetId() != snapshotID || snapshot.GetName() != claim.FixedCreateIntent.Attempt["snapshot"]) {
				return core.FixedLeaseBinding{}, core.Exit(4, "Daytona incomplete acquisition source differs from its fixed create attempt")
			}
			if organization != "" && !snapshot.GetGeneral() && snapshot.GetOrganizationId() != organization {
				return core.FixedLeaseBinding{}, core.Exit(4, "Daytona snapshot organization mismatch")
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
					return binding, core.Exit(4, "lease_id_conflict: Daytona lease already exists without its create intent")
				}
			}
			binding.AllocateSlug, binding.RequestedSlug, binding.Inventory = true, req.RequestedSlug, daytonaSandboxesToServers(sandboxes)
		}
		return binding, nil
	}, ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[*api.Sandbox], error) {
		if err := core.AuthorizeCheckpointRelease(*tx.Claim, ""); err != nil {
			return core.FixedObservation[*api.Sandbox]{}, err
		}
		if fresh {
			return core.FixedObservation[*api.Sandbox]{CanSubmit: true}, nil
		}
		return core.FixedObservation[*api.Sandbox]{Candidates: []*api.Sandbox{observed}}, nil
	}, Plan: func(ctx context.Context, claim core.LeaseClaim) (core.FixedAttemptPlan, error) {
		intent := claim.FixedCreateIntent
		createdAt, _ := time.Parse(time.RFC3339Nano, intent.CreatedAt)
		remaining := time.Until(createdAt.Add(cfg.TTL))
		if remaining <= 0 {
			return core.FixedAttemptPlan{}, core.Exit(4, "Daytona fixed create intent expired before submission")
		}
		body := daytonaCreateBody(cfg, req.RequestedLeaseID, intent.Slug, req.Keep, createdAt)
		cfg.Daytona.Snapshot = snapshot.GetId()
		body.SetSnapshot(cfg.Daytona.Snapshot)
		// Replay retains the original lease deadline, including native TTL.
		body.SetTtlMinutes(int32(core.DurationMinutesCeil(remaining)))
		createBody = body
		return core.FixedAttemptPlan{NonceKey: "nonce", OwnerLabel: "fixed_claim_provider", Labels: body.GetLabels(), FingerprintLabel: "fixed_intent_sha256", NonceLabel: "fixed_attempt",
			Values: map[string]string{"name": body.GetName(), "snapshot": snapshot.GetName(), "snapshot_id": snapshot.GetId(), "user": daytonaUser(cfg), "target": cfg.Daytona.Target, "organization": organization},
		}, nil
	}, Submit: func(ctx context.Context, tx *core.FixedTransaction) (*api.Sandbox, error) {
		createBody.SetLabels(maps.Clone(tx.Claim.Labels))
		sandbox, err := client.CreateSandbox(ctx, *createBody)
		if err != nil {
			return nil, fmt.Errorf("Daytona create outcome uncertain; replay or stop fixed lease %s: %w", tx.Claim.LeaseID, err)
		}
		// Returned UUID evidence is durable before a rejected attestation can lose it.
		if sandbox != nil && sandbox.GetId() != "" {
			if err := tx.Observe(core.FixedResourceBinding{CloudID: sandbox.GetId()}); err != nil {
				return nil, err
			}
		}
		return sandbox, nil
	}, PrepareAccess: func(ctx context.Context, tx *core.FixedTransaction, sandbox *api.Sandbox) (core.LeaseTarget, error) {
		claim, intent := tx.Claim, tx.Claim.FixedCreateIntent
		if err := validateFixedDaytonaSandbox(*claim, sandbox); err != nil {
			return core.LeaseTarget{}, err
		}
		if intent.State == "prepared" {
			if err := validateClassSandbox(sandbox, snapshot); err != nil {
				return core.LeaseTarget{}, err
			}
		}
		// The native target is a resolved region ID, not the requested name.
		if err := tx.Bind(core.FixedResourceBinding{CloudID: sandbox.GetId(), ImmutableID: sandbox.GetId(),
			AttemptValues: map[string]string{"resolved_target": sandbox.GetTarget(), "organization": sandbox.GetOrganizationId()},
		}); err != nil {
			return core.LeaseTarget{}, err
		}

		if !daytonaStateReady(daytonaSandboxState(sandbox)) {
			if sandbox.GetState() == api.SANDBOXSTATE_STOPPED || sandbox.GetState() == api.SANDBOXSTATE_ARCHIVED {
				if _, err := client.StartSandbox(ctx, sandbox.GetId()); err != nil {
					return core.LeaseTarget{}, err
				}
			}
			var err error
			sandbox, err = waitForDaytonaReady(ctx, client, sandbox.GetId(), 5*time.Minute)
			if err != nil {
				return core.LeaseTarget{}, err
			}
			if err := validateFixedDaytonaSandbox(*claim, sandbox); err != nil {
				return core.LeaseTarget{}, err
			}
		}
		server := daytonaSandboxToServer(sandbox)
		server.ImmutableID = sandbox.GetId()
		target, err := daytonaSSHTargetFor(ctx, client, cfg, server)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if err := core.WaitForSSHReady(ctx, &target, b.rt.Stderr, "daytona ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
			return core.LeaseTarget{}, err
		}
		return core.LeaseTarget{Server: server, SSH: target, LeaseID: claim.LeaseID}, nil
	}})
	return core.CompleteFixedAcquisition(lease, err, req)
}

func validateFixedDaytonaSandbox(claim core.LeaseClaim, sandbox *api.Sandbox) error {
	intent := claim.FixedCreateIntent
	if intent == nil || intent.Version != 1 || intent.Fingerprint == "" || (intent.State != "prepared" && intent.State != "acquired") || claim.Provider != core.FixedDaytonaClaimProvider || claim.ProviderScope != intent.ProviderScope || claim.Slug != intent.Slug || sandbox == nil {
		return core.Exit(4, "Daytona fixed lease has an invalid ownership claim")
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
		return core.Exit(4, "lease_id_conflict: Daytona sandbox does not match the exact fixed create attempt")
	}
	return nil
}

func loadFixedDaytonaSandbox(ctx context.Context, client daytonaAPI, claim core.LeaseClaim) (*api.Sandbox, error) {
	return core.LookupFixedResource(ctx, fixedDaytonaLeaseKind, claim, func(ctx context.Context, claim core.LeaseClaim) (*api.Sandbox, error) {
		if err := core.CheckFixedAttemptActive(claim, "name", "deletion_indexed_id", "deletion_acknowledged_id"); err != nil {
			return nil, err
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
			return nil, core.Exit(4, "Daytona fixed resource was not returned")
		}
		scope, organization, err := fixedDaytonaResourceContext(client, sandbox.GetOrganizationId())
		if err != nil {
			return nil, err
		}
		if claim.ProviderScope != scope || organization != claim.FixedCreateIntent.Attempt["organization"] {
			return nil, core.Exit(4, "Daytona fixed lease API or organization changed")
		}
		if err := validateFixedDaytonaSandbox(claim, sandbox); err != nil {
			return nil, err
		}
		return sandbox, nil
	})
}

func (b *daytonaLeaseBackend) releaseFixed(ctx context.Context, expected core.LeaseClaim, checkpointID string, absenceOnly bool, repoRoot string) error {
	var client fixedDaytonaDeletionAPI
	policy := &core.FixedReleasePolicy{RepoRoot: repoRoot, CheckpointID: &checkpointID, SkipTerminalObservation: true}
	if !absenceOnly {
		policy.PristineScopePrefix = "daytona:organization:v1:"
	}
	return core.DeleteFixedResource(ctx, fixedDaytonaLeaseKind, expected, core.FixedLeaseOperations[struct{}]{
		Release: policy,
		ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[struct{}], error) {
			var result core.FixedObservation[struct{}]
			if absenceOnly && tx.Claim.FixedCreateIntent.State != "acquired" {
				return result, core.Exit(4, "Daytona absence reconciliation requires a completed fixed acquisition")
			}
			apiClient, err := newDaytonaClient(b.cfg, b.rt)
			if err != nil {
				return result, err
			}
			var ok bool
			client, ok = apiClient.(fixedDaytonaDeletionAPI)
			if !ok {
				return result, core.Exit(4, "Daytona client cannot attest fixed resource deletion")
			}
			if tx.Claim.CloudID == "" {
				sandbox, err := loadFixedDaytonaSandbox(ctx, client, *tx.Claim)
				if err != nil {
					return result, err
				}
				result.Binding = &core.FixedResourceBinding{CloudID: sandbox.GetId(), ImmutableID: sandbox.GetId()}
			}
			if absenceOnly {
				// This native proof never submits DELETE or writes witnesses.
				err := deleteFixedDaytonaSandbox(ctx, client, tx.Claim, tx.PersistDeletionEvidence, true)
				result.AbsenceProven = err == nil
				return result, err
			}
			result.Candidates = []struct{}{{}}
			return result, nil
		},
		DeleteExact: func(ctx context.Context, tx *core.FixedTransaction, _ struct{}) error {
			return deleteFixedDaytonaSandbox(ctx, client, tx.Claim, tx.PersistDeletionEvidence, false)
		},
	})
}

func (b *daytonaLeaseBackend) reclaimFixed(ctx context.Context, claim core.LeaseClaim, repoRoot string, reclaim bool) (core.LeaseClaim, error) {
	return core.ReclaimFixedLease(ctx, claim, b.cfg, repoRoot, reclaim, func(ctx context.Context, claim core.LeaseClaim) (core.Server, error) {
		client, err := newDaytonaClient(b.cfg, b.rt)
		if err != nil {
			return core.Server{}, err
		}
		sandbox, err := loadFixedDaytonaSandbox(ctx, client, claim)
		if err != nil {
			return core.Server{}, err
		}
		return daytonaSandboxToServer(sandbox), nil
	})
}

type fixedDaytonaDeletionAPI interface {
	daytonaAPI
	fixedSelection() (endpoint, organization string)
	getSandboxForCleanup(context.Context, string) (*api.Sandbox, error)
	attestDeletionOrganization(context.Context, string) error
	requestSandboxDeletion(context.Context, string) (*api.Sandbox, error)
}

func validateFixedDaytonaDeletionIdentity(client fixedDaytonaDeletionAPI, claim core.LeaseClaim, sandbox *api.Sandbox) error {
	endpoint, selectedOrganization := client.fixedSelection()
	attempt := claim.FixedCreateIntent.Attempt
	if claim.ProviderScope != claim.FixedCreateIntent.ProviderScope || claim.Slug != claim.FixedCreateIntent.Slug ||
		(claim.CloudImmutableID != "" && claim.CloudImmutableID != claim.CloudID) || attempt["organization"] == "" ||
		sandbox == nil || sandbox.GetId() == "" || (claim.CloudID != "" && sandbox.GetId() != claim.CloudID) || sandbox.GetOrganizationId() != attempt["organization"] ||
		(selectedOrganization != "" && selectedOrganization != sandbox.GetOrganizationId()) ||
		sandbox.GetLabels()["fixed_claim_provider"] != core.FixedDaytonaClaimProvider ||
		sandbox.GetLabels()["lease"] != claim.LeaseID || sandbox.GetLabels()["fixed_intent_sha256"] != claim.FixedCreateIntent.Fingerprint ||
		attempt["nonce"] == "" || sandbox.GetLabels()["fixed_attempt"] != attempt["nonce"] {
		return core.Exit(4, "Daytona deletion response does not match the exact fixed resource")
	}
	if leaseID, owned := daytonaSandboxOwnership(sandbox); !owned || leaseID != claim.LeaseID {
		return core.Exit(4, "Daytona deletion response lost fixed lease ownership")
	}
	scope, _, err := fixedDaytonaScope(endpoint, sandbox.GetOrganizationId())
	if err != nil {
		return err
	}
	if claim.ProviderScope != scope {
		return core.Exit(4, "Daytona deletion endpoint or organization changed")
	}
	return nil
}

func deleteFixedDaytonaSandbox(ctx context.Context, client fixedDaytonaDeletionAPI, claim *core.LeaseClaim, persist func() error, absenceOnly bool) error {
	intent := claim.FixedCreateIntent
	if claim.CloudID == "" || intent == nil || (intent.State != "prepared" && intent.State != "acquired") {
		return core.Exit(4, "Daytona fixed cleanup requires its durably observed resource UUID")
	}
	if claim.ProviderScope != intent.ProviderScope || claim.Slug != intent.Slug || intent.Fingerprint == "" || intent.Attempt["nonce"] == "" {
		return core.Exit(4, "Daytona fixed cleanup has an invalid ownership claim")
	}
	for _, key := range []string{"deletion_indexed_id", "deletion_acknowledged_id"} {
		if value := intent.Attempt[key]; value != "" && value != claim.CloudID {
			return core.Exit(4, "Daytona deletion witness does not match the fixed resource UUID")
		}
	}
	endpoint, selected := client.fixedSelection()
	scope, _, err := fixedDaytonaScope(endpoint, intent.Attempt["organization"])
	if err != nil || scope != claim.ProviderScope || selected != "" && selected != intent.Attempt["organization"] {
		return core.Exit(4, "Daytona deletion endpoint or organization changed")
	}
	if err := client.attestDeletionOrganization(ctx, intent.Attempt["organization"]); err != nil {
		return err
	}
	if absenceOnly || intent.Attempt["deletion_acknowledged_id"] == "" {
		sandbox, err := client.getSandboxForCleanup(ctx, claim.CloudID)
		if err != nil {
			return err
		}
		if sandbox == nil {
			// Native TTL or external deletion can finish without our DELETE witness.
			// Only a completed exact acquisition can retire from authenticated absence.
			if intent.State == "acquired" && claim.CloudImmutableID == claim.CloudID &&
				claim.Labels["fixed_claim_provider"] == core.FixedDaytonaClaimProvider &&
				claim.Labels["lease"] == claim.LeaseID && claim.Labels["fixed_intent_sha256"] == intent.Fingerprint &&
				claim.Labels["fixed_attempt"] == intent.Attempt["nonce"] {
				return nil
			}
			return core.Exit(4, "Daytona fixed deletion has no acknowledged outcome; retain lease %s", claim.LeaseID)
		}
		if err := validateFixedDaytonaDeletionIdentity(client, *claim, sandbox); err != nil {
			return err
		}
		// Spot preemption can leave an exact DESTROYED tombstone visible for 24h.
		if sandbox.GetState() == api.SANDBOXSTATE_DESTROYED {
			return nil
		}
		if absenceOnly {
			return core.Exit(4, "Daytona fixed resource is still present; inspection cannot delete it")
		}
		// Retain the persisted cleanup-entry key for existing claims. Its identity
		// now comes from the exact database lookup, before admitting DELETE.
		if err := core.RecordFixedWitness(claim, "deletion_indexed_id", claim.CloudID, persist); err != nil {
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
			return core.Exit(4, "Daytona did not acknowledge destruction of the fixed resource")
		}
		if err := core.RecordFixedWitness(claim, "deletion_acknowledged_id", claim.CloudID, persist); err != nil {
			return err
		}
	}
	if intent.Attempt["deletion_indexed_id"] != claim.CloudID {
		return core.Exit(4, "Daytona fixed deletion has no prior inventory identity; retain its claim")
	}
	for {
		sandbox, err := client.getSandboxForCleanup(ctx, claim.CloudID)
		if err != nil {
			return err
		}
		if sandbox == nil {
			// Reattest the original account, then reread the exact resource so a
			// stale credential context cannot convert inaccessible state to absence.
			if err := client.attestDeletionOrganization(ctx, intent.Attempt["organization"]); err != nil {
				return err
			}
			sandbox, err = client.getSandboxForCleanup(ctx, claim.CloudID)
			if err != nil {
				return err
			}
			if sandbox == nil {
				return nil
			}
		}
		if err := validateFixedDaytonaDeletionIdentity(client, *claim, sandbox); err != nil {
			return err
		}
		if sandbox.GetState() == api.SANDBOXSTATE_DESTROYED {
			return nil
		}
		if sandbox.GetState() == api.SANDBOXSTATE_ERROR || sandbox.GetState() == api.SANDBOXSTATE_BUILD_FAILED {
			return core.Exit(4, "Daytona fixed deletion failed with state=%s; retain lease %s for reconciliation", sandbox.GetState(), claim.LeaseID)
		}
		if err := shared.SleepContext(ctx, time.Second); err != nil {
			return err
		}
	}
}

func neverSubmittedDaytonaClaim(claim core.LeaseClaim) bool {
	return core.FixedPristineRecord(claim, fixedDaytonaLeaseKind, "daytona:organization:v1:")
}

func (b *daytonaLeaseBackend) RetainLeaseClaimAfterReleaseWithClaim(lease core.LeaseTarget, previous core.LeaseClaim) (bool, error) {
	return fixedDaytonaLeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, hasFixedDaytonaOwnershipLabels(lease.Server.Labels), nil, nil)
}

func (Provider) PrepareLeaseClaimEndpoint(existing core.LeaseClaim, provider, slug string, server core.Server, _ bool) (core.Server, error) {
	if existing.FixedCreateIntent == nil && existing.ProviderScope != "" && (provider != daytonaProvider || server.CloudID != existing.CloudID || server.ImmutableID != existing.CloudImmutableID) {
		return core.Server{}, core.Exit(4, "refusing to retarget account-bound Daytona claim")
	}
	if existing.FixedCreateIntent != nil {
		if err := core.AuthorizeCheckpointRelease(existing, ""); err != nil {
			return core.Server{}, err
		}
		if !fixedDaytonaLeaseKind.IsFixedClaim(existing) || provider != daytonaProvider || slug != existing.Slug || existing.FixedCreateIntent.State != "acquired" || server.CloudID != existing.CloudID || server.Labels["fixed_claim_provider"] != core.FixedDaytonaClaimProvider || server.Labels["fixed_intent_sha256"] != existing.FixedCreateIntent.Fingerprint || server.Labels["fixed_attempt"] != existing.FixedCreateIntent.Attempt["nonce"] {
			return core.Server{}, core.Exit(4, "refusing to rewrite Daytona fixed lease identity")
		}
	}
	return server, nil
}

func (b *daytonaLeaseBackend) AuthorizeStatusTouchClaim(ctx context.Context, lease core.LeaseTarget, claim core.LeaseClaim) error {
	if claim.LeaseID != lease.LeaseID || (claim.Provider != daytonaProvider && claim.Provider != core.FixedDaytonaClaimProvider) || lease.Server.CloudID == "" || lease.Server.CloudID != claim.CloudID {
		return core.Exit(4, "Daytona lifecycle touch requires an exact source lease claim")
	}
	if claim.FixedCreateIntent == nil {
		if claim.ProviderScope == "" {
			return nil
		}
		client, err := newDaytonaClient(b.cfg, b.rt)
		if err != nil {
			return err
		}
		scope, _, err := daytonaAccountContext(ctx, client)
		if err != nil {
			return err
		}
		if scope != claim.ProviderScope {
			return core.Exit(4, "Daytona lifecycle touch provider scope mismatch")
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
