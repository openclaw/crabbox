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
	if claim.FixedCreateIntent.Attempt[fixedDaytonaDeletionAcknowledged] != "" {
		return nil, exit(4, "Daytona fixed lease %s has an acknowledged deletion; stop it to finalize instead of replaying", claim.LeaseID)
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

func (b *daytonaLeaseBackend) releaseFixed(ctx context.Context, claim core.LeaseClaim, checkpointID string) error {
	if !fixedDaytonaLeaseKind.IsFixedClaim(claim) || claim.FixedCreateIntent.Version != fixedDaytonaLeaseKind.IntentVersion {
		return exit(4, "Daytona fixed lease format is not recognized; retain its ownership record for reconciliation")
	}
	if claim.FixedCreateIntent.State == "released" {
		return fixedDaytonaLeaseKind.ValidateTerminalClaim(claim, claim, claim.LeaseID, nil)
	}
	if err := core.AuthorizeCheckpointRelease(claim, checkpointID); err != nil {
		return err
	}
	finalize := func(current core.LeaseClaim, cleanup func() error) error {
		return fixedDaytonaLeaseKind.FinalizeAfterCleanup(current, func() error {
			if err := core.AuthorizeCheckpointRelease(current, checkpointID); err != nil {
				return err
			}
			return cleanup()
		})
	}
	// Version 1 never clears a submitted attempt. An empty initial claim
	// therefore proves the durable authorization preceding POST never existed.
	if neverSubmittedDaytonaClaim(claim) {
		return finalize(claim, func() error { return nil })
	}
	apiClient, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	client, ok := apiClient.(fixedDaytonaDeletionAPI)
	if !ok {
		return exit(4, "Daytona client cannot attest fixed resource deletion")
	}
	if claim.CloudID == "" {
		sandbox, err := loadFixedDaytonaSandbox(ctx, client, claim)
		if err != nil {
			if !daytonaIsNotFoundError(err) {
				return err
			}
			// Native deletion renames the sandbox and a lost create response never
			// recorded its UUID. Daytona never lists or returns destroyed sandboxes,
			// so an authorized exact-attempt search over live inventory is the only
			// supported way to establish whether the attempt still holds a resource.
			if err := verifyFixedDaytonaOrganization(ctx, client, claim); err != nil {
				return err
			}
			sandbox, err = client.findFixedAttemptSandbox(ctx, claim)
			if err != nil {
				return err
			}
			if sandbox == nil {
				// The attempt never landed or was already destroyed natively; a
				// destroyed sandbox holds no resource, so nothing remains to delete.
				return finalize(claim, func() error { return nil })
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Deletion can rename/hide the resource before returning. Persist
		// the first observed UUID before it can become unreachable by name.
		claim, err = bindFixedDaytonaClaim(client, claim, sandbox)
		if err != nil {
			return err
		}
	}
	// The native DELETE and its acknowledgement run under the exclusive
	// unchanged-claim fence, so a live run or a repository transfer cannot race
	// the destructive call; a changed claim retries without native effects.
	expected := claim
	acknowledged := claim
	if err := core.WithDurableLeaseClaimLockContext(ctx, claim.LeaseID, func(current *core.LeaseClaim, exists bool, persist func() error) error {
		if !exists || !reflect.DeepEqual(*current, expected) {
			return exit(4, "fixed Daytona lease %s claim changed before release; retry", expected.LeaseID)
		}
		next, err := acknowledgeFixedDaytonaDeletion(ctx, client, *current, checkpointID)
		if err != nil {
			return err
		}
		if reflect.DeepEqual(next, *current) {
			acknowledged = *current
			return nil
		}
		*current = next
		if err := persist(); err != nil {
			return err
		}
		acknowledged = *current
		return nil
	}); err != nil {
		return err
	}
	return finalize(acknowledged, func() error { return awaitFixedDaytonaDeletion(ctx, client, acknowledged) })
}

func bindFixedDaytonaClaim(client fixedDaytonaDeletionAPI, claim LeaseClaim, sandbox *api.Sandbox) (LeaseClaim, error) {
	var err error
	if fixedDaytonaSandboxDestroying(sandbox) {
		err = validateFixedDaytonaDeletionIdentity(client, claim, sandbox)
	} else {
		err = validateFixedDaytonaSandbox(claim, sandbox)
	}
	if err != nil {
		return claim, err
	}
	if claim.CloudID != "" {
		return claim, nil
	}
	expected := claim
	claim.CloudID, claim.CloudImmutableID = sandbox.GetId(), sandbox.GetId()
	return core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, expected, claim)
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
	findFixedAttemptSandbox(context.Context, LeaseClaim) (*api.Sandbox, error)
	requestSandboxDeletion(context.Context, string) (*api.Sandbox, error)
}

// The acknowledgement records the exact UUID whose deletion Daytona confirmed,
// or whose absence an authorized live-inventory search established.
const fixedDaytonaDeletionAcknowledged = "deletion_acknowledged"

func fixedDaytonaSandboxDestroying(sandbox *api.Sandbox) bool {
	return sandbox != nil && (sandbox.GetDesiredState() == api.SANDBOXDESIREDSTATE_DESTROYED ||
		sandbox.GetState() == api.SANDBOXSTATE_DESTROYED || sandbox.GetState() == api.SANDBOXSTATE_DESTROYING)
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

// verifyFixedDaytonaOrganization proves the current credentials still select the
// claim's exact API endpoint and organization before an inventory absence may
// stand in for a resource. A GET 404 alone never establishes that context.
func verifyFixedDaytonaOrganization(ctx context.Context, client daytonaAPI, claim LeaseClaim) error {
	scope, organization, err := fixedDaytonaContext(ctx, client)
	if err != nil {
		return fmt.Errorf("Daytona fixed lease %s retains its ownership record; organization context could not be established: %w", claim.LeaseID, err)
	}
	if scope != claim.ProviderScope || organization != claim.FixedCreateIntent.Attempt["organization"] {
		return exit(4, "Daytona fixed lease API or organization changed; retain its ownership record")
	}
	return nil
}

// acknowledgeFixedDaytonaDeletion obtains and durably records the deletion witness
// for the claim's exact UUID before any terminal finalization. Daytona v0.190.0
// never lists or returns destroyed sandboxes, so the witness is either the DELETE
// response naming the owned resource, an owned resource already observed as being
// destroyed, or an authorized organization-scoped exact-attempt search that finds
// no resource-holding sandbox. A bare 404 is never accepted on its own.
func acknowledgeFixedDaytonaDeletion(ctx context.Context, client fixedDaytonaDeletionAPI, claim LeaseClaim, checkpointID string) (LeaseClaim, error) {
	if claim.CloudID == "" {
		return claim, exit(4, "Daytona fixed cleanup requires its durably observed resource UUID")
	}
	attempt := claim.FixedCreateIntent.Attempt
	if acknowledged := attempt[fixedDaytonaDeletionAcknowledged]; acknowledged != "" {
		if acknowledged != claim.CloudID {
			return claim, exit(4, "Daytona deletion acknowledgement names a different resource; retain its ownership record")
		}
		// A recorded acknowledgement only authorizes absence under the same
		// endpoint and organization; other credentials could see a 404 for a
		// resource that still exists.
		if err := verifyFixedDaytonaOrganization(ctx, client, claim); err != nil {
			return claim, err
		}
		return claim, nil
	}
	sandbox, err := client.GetSandbox(ctx, claim.CloudID)
	if err != nil {
		if !daytonaIsNotFoundError(err) {
			return claim, err
		}
		// A 404 may mask failed access, and a DELETE whose response was lost
		// left no acknowledgement. Only an authorized live search decides.
		if err := verifyFixedDaytonaOrganization(ctx, client, claim); err != nil {
			return claim, err
		}
		live, err := client.findFixedAttemptSandbox(ctx, claim)
		if err != nil {
			return claim, err
		}
		if live == nil {
			return recordFixedDaytonaDeletionAcknowledgement(claim), nil
		}
		if live.GetId() != claim.CloudID {
			return claim, exit(4, "Daytona fixed attempt inventory names a different resource; retain its ownership record")
		}
		sandbox = live
	}
	if err := validateFixedDaytonaDeletionIdentity(client, claim, sandbox); err != nil {
		return claim, err
	}
	if !fixedDaytonaSandboxDestroying(sandbox) {
		if err := validateFixedDaytonaSandbox(claim, sandbox); err != nil {
			return claim, err
		}
		if err := core.AuthorizeCheckpointRelease(claim, checkpointID); err != nil {
			return claim, err
		}
		// A lost DELETE response records nothing; replay re-reads the resource.
		sandbox, err = client.requestSandboxDeletion(ctx, claim.CloudID)
		if err != nil {
			return claim, err
		}
		if err := validateFixedDaytonaDeletionIdentity(client, claim, sandbox); err != nil {
			return claim, err
		}
		if !fixedDaytonaSandboxDestroying(sandbox) {
			return claim, exit(4, "Daytona did not acknowledge destruction of the fixed resource")
		}
	}
	return recordFixedDaytonaDeletionAcknowledgement(claim), nil
}

// The caller persists the returned claim under the exclusive claim fence.
func recordFixedDaytonaDeletionAcknowledgement(claim LeaseClaim) LeaseClaim {
	intent := *claim.FixedCreateIntent
	intent.Attempt = maps.Clone(intent.Attempt)
	intent.Attempt[fixedDaytonaDeletionAcknowledged] = claim.CloudID
	intent.Attempt[fixedDaytonaDeletionAcknowledged+"_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	claim.FixedCreateIntent = &intent
	return claim
}

// awaitFixedDaytonaDeletion waits for the acknowledged resource to disappear.
// After an acknowledgement, a 404 for that exact UUID under the claim's
// verified endpoint and organization is the terminal witness; native soft
// deletion may keep the renamed resource visible for a while.
func awaitFixedDaytonaDeletion(ctx context.Context, client fixedDaytonaDeletionAPI, claim LeaseClaim) error {
	if claim.FixedCreateIntent.Attempt[fixedDaytonaDeletionAcknowledged] != claim.CloudID {
		return exit(4, "Daytona fixed cleanup requires an acknowledged deletion")
	}
	for {
		sandbox, err := client.GetSandbox(ctx, claim.CloudID)
		if err != nil {
			if daytonaIsNotFoundError(err) {
				return verifyFixedDaytonaOrganization(ctx, client, claim)
			}
			return err
		}
		if err := validateFixedDaytonaDeletionIdentity(client, claim, sandbox); err != nil {
			return err
		}
		if sandbox.GetState() == api.SANDBOXSTATE_DESTROYED {
			return nil
		}
		if !fixedDaytonaSandboxDestroying(sandbox) {
			return exit(4, "Daytona fixed resource is no longer being destroyed; retain its ownership record")
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
