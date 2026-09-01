package daytona

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	api "github.com/daytonaio/daytona/libs/api-client-go"
	core "github.com/openclaw/crabbox/internal/cli"
)

var fixedDaytonaLeaseKind = core.FixedLeaseKind{ClaimProvider: daytonaProvider, IntentVersion: 1, Label: "Daytona"}

func (b *daytonaLeaseBackend) SupportsRequestedLeaseID() bool { return b.cfg.Coordinator == "" }
func (b *daytonaLeaseBackend) SupportsRequestedCheckpointID() bool {
	return b.SupportsRequestedLeaseID()
}

func (c *daytonaSDKClient) fixedContext() (string, string) {
	// API keys are bound to their issuing organization, even without a header.
	// Hash the exact client's credentials; also bind explicit organization headers
	// so a missing resource cannot hide a changed authentication context.
	data, _ := json.Marshal([]string{c.apiURL, c.token})
	return fmt.Sprintf("daytona:context:v1:%x", sha256.Sum256(data)), c.orgID
}

func fixedDaytonaContext(client daytonaAPI) (string, string, error) {
	identity, ok := client.(interface{ fixedContext() (string, string) })
	if !ok {
		return "", "", exit(4, "Daytona client has no bound credential context")
	}
	// Derive identity from the client that will perform the request. Rereading a
	// rotating CLI profile here could bind a different credential than that client.
	scope, organization := identity.fixedContext()
	return scope, organization, nil
}

func (b *daytonaLeaseBackend) acquireFixed(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	cfg := b.cfg
	if err := validateDaytonaCreateConfig(cfg); err != nil {
		return LeaseTarget{}, err
	}
	client, err := snapshotClient(cfg, b.rt)
	if err != nil {
		return LeaseTarget{}, err
	}
	scope, organization, err := fixedDaytonaContext(client)
	if err != nil {
		return LeaseTarget{}, err
	}
	var snapshot *api.SnapshotDto
	fresh := false
	lease, err := core.AcquireFixedLease(core.FixedAcquireOptions{
		Kind: fixedDaytonaLeaseKind, LeaseID: req.RequestedLeaseID, CheckpointID: req.RequestedCheckpointID,
		RepoRoot: req.Repo.Root, Reclaim: req.Reclaim, TargetOS: targetLinux, TTL: cfg.TTL, IdleTimeout: cfg.IdleTimeout,
	}, func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
		fresh = !exists
		if exists && (claim.Provider != daytonaProvider || claim.ProviderScope != scope || claim.FixedCreateIntent == nil || organization != "" && organization != claim.FixedCreateIntent.Attempt["organization"]) {
			return core.FixedLeaseBinding{}, exit(4, "lease_id_conflict: Daytona API, organization, or credential context changed")
		}
		var err error
		snapshotID := ""
		if exists {
			// A submitted attempt pins its immutable source. Its sandbox remains
			// replayable after that source snapshot is retired or becomes unavailable.
			snapshotID = claim.FixedCreateIntent.Attempt["snapshot_id"]
			if source := req.CheckpointSource; source != nil && snapshotID != "" {
				attempt := claim.FixedCreateIntent.Attempt
				if snapshotID != source.ImageID || attempt["snapshot"] != source.Name || attempt["organization"] != source.Metadata["organization"] {
					return core.FixedLeaseBinding{}, exit(4, "lease_id_conflict: Daytona checkpoint source does not match the fixed create attempt")
				}
			}
		}
		if snapshotID == "" {
			snapshot, err = client.GetSnapshot(ctx, strings.TrimSpace(cfg.Daytona.Snapshot))
			if err != nil {
				return core.FixedLeaseBinding{}, err
			}
			if snapshot == nil || snapshot.GetId() == "" || snapshot.GetName() == "" || snapshot.GetState() != api.SNAPSHOTSTATE_ACTIVE {
				return core.FixedLeaseBinding{}, exit(4, "Daytona fixed acquisition requires an active, identified snapshot")
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
		data, err := json.Marshal(struct {
			Snapshot, Selector, User, Target, WorkRoot, Slug string
			Keep                                             bool
			TTL, Idle                                        time.Duration
		}{snapshotID, cfg.Daytona.Snapshot, daytonaUser(cfg), cfg.Daytona.Target, daytonaWorkRoot(cfg), normalizeLeaseSlug(req.RequestedSlug), req.Keep, cfg.TTL, cfg.IdleTimeout})
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		binding := core.FixedLeaseBinding{ProviderScope: scope, Fingerprint: fmt.Sprintf("%x", sha256.Sum256(data))}
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
			binding.Slug, err = allocateDirectLeaseSlug(req.RequestedLeaseID, req.RequestedSlug, daytonaSandboxesToServers(sandboxes, cfg))
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
			cfg.Daytona.Snapshot = snapshot.GetId()
			body := daytonaCreateBody(cfg, req.RequestedLeaseID, intent.Slug, req.Keep, createdAt)
			labels := body.GetLabels()
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
			var err error
			sandbox, err = loadFixedDaytonaSandbox(ctx, client, *claim)
			if err != nil {
				return LeaseTarget{}, err
			}
		}
		if err := validateFixedDaytonaSandbox(*claim, sandbox); err != nil {
			return LeaseTarget{}, err
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
		server := daytonaSandboxToServer(sandbox, cfg)
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
	if intent == nil || intent.Version != 1 || intent.Fingerprint == "" || (intent.State != "prepared" && intent.State != "acquired") || claim.Provider != daytonaProvider || claim.ProviderScope != intent.ProviderScope || claim.Slug != intent.Slug || sandbox == nil {
		return exit(4, "Daytona fixed lease has an invalid ownership claim")
	}
	attempt := intent.Attempt
	id, owned := daytonaSandboxOwnership(sandbox)
	if !owned || id != claim.LeaseID || sandbox.GetName() != attempt["name"] || sandbox.GetId() == "" || sandbox.GetOrganizationId() == "" ||
		(claim.CloudID != "" && sandbox.GetId() != claim.CloudID) || (claim.CloudImmutableID != "" && sandbox.GetId() != claim.CloudImmutableID) ||
		sandbox.GetLabels()["fixed_intent_sha256"] != intent.Fingerprint || attempt["nonce"] == "" || sandbox.GetLabels()["fixed_attempt"] != attempt["nonce"] ||
		sandbox.GetSnapshot() != attempt["snapshot"] || sandbox.GetUser() != attempt["user"] ||
		(attempt["organization"] != "" && sandbox.GetOrganizationId() != attempt["organization"]) || (attempt["target"] != "" && sandbox.GetTarget() != attempt["target"]) {
		return exit(4, "lease_id_conflict: Daytona sandbox does not match the exact fixed create attempt")
	}
	return nil
}

func loadFixedDaytonaSandbox(ctx context.Context, client daytonaAPI, claim core.LeaseClaim) (*api.Sandbox, error) {
	scope, organization, err := fixedDaytonaContext(client)
	if err != nil {
		return nil, err
	}
	if claim.ProviderScope != scope || claim.FixedCreateIntent == nil || organization != "" && organization != claim.FixedCreateIntent.Attempt["organization"] {
		return nil, exit(4, "Daytona fixed lease API, organization, or credential context changed")
	}
	if claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State == "released" || claim.FixedCreateIntent.Attempt["name"] == "" {
		return nil, exit(4, "Daytona fixed lease has no active create attempt; it cannot allocate a replacement")
	}
	lookup := claim.CloudID
	if lookup == "" {
		lookup = claim.FixedCreateIntent.Attempt["name"]
	}
	sandbox, err := client.GetSandbox(ctx, lookup)
	if err != nil {
		return nil, fmt.Errorf("Daytona fixed lease %s remains unresolved; no replacement allocated: %w", claim.LeaseID, err)
	}
	return sandbox, validateFixedDaytonaSandbox(claim, sandbox)
}

func (b *daytonaLeaseBackend) releaseFixed(ctx context.Context, claim core.LeaseClaim, checkpointID string) error {
	if claim.FixedCreateIntent.State == "released" {
		return fixedDaytonaLeaseKind.ValidateTerminalClaim(claim, claim, claim.LeaseID, nil)
	}
	return fixedDaytonaLeaseKind.FinalizeAfterCleanup(claim, func() error {
		if err := core.AuthorizeCheckpointRelease(claim, checkpointID); err != nil {
			return err
		}
		// Version 1 never clears a submitted attempt. An empty initial claim
		// therefore proves the durable authorization preceding POST never existed.
		if neverSubmittedDaytonaClaim(claim) {
			return nil
		}
		client, err := newDaytonaClient(b.cfg, b.rt)
		if err != nil {
			return err
		}
		sandbox, err := loadFixedDaytonaSandbox(ctx, client, claim)
		// A confirmed UUID is never adopted by name. Absence in its bound API and
		// credential context is safe after allocation, including native TTL expiry.
		if daytonaIsNotFoundError(err) && claim.CloudID != "" && claim.CloudImmutableID == claim.CloudID {
			return nil
		}
		if err != nil {
			return err
		}
		return deleteOwnedDaytonaSandbox(ctx, client, sandbox.GetId(), claim.LeaseID)
	})
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
	scope, ok := strings.CutPrefix(intent.ProviderScope, "daytona:context:v1:")
	scopeHash, scopeErr := hex.DecodeString(scope)
	fingerprint, fingerprintErr := hex.DecodeString(intent.Fingerprint)
	_, timeErr := time.Parse(time.RFC3339Nano, intent.CreatedAt)
	return ok && scopeErr == nil && len(scopeHash) == sha256.Size && fingerprintErr == nil && len(fingerprint) == sha256.Size && timeErr == nil
}

func (b *daytonaLeaseBackend) RetainLeaseClaimAfterReleaseWithClaim(lease LeaseTarget, previous core.LeaseClaim) (bool, error) {
	return fixedDaytonaLeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, lease.Server.Labels["fixed_intent_sha256"] != "", nil, nil)
}

func (Provider) PrepareLeaseClaimEndpoint(existing core.LeaseClaim, provider, slug string, server core.Server, _ bool) (core.Server, error) {
	if existing.FixedCreateIntent != nil {
		if err := core.AuthorizeCheckpointRelease(existing, ""); err != nil {
			return Server{}, err
		}
		if provider != daytonaProvider || slug != existing.Slug || existing.FixedCreateIntent.State != "acquired" || server.CloudID != existing.CloudID || server.Labels["fixed_intent_sha256"] != existing.FixedCreateIntent.Fingerprint || server.Labels["fixed_attempt"] != existing.FixedCreateIntent.Attempt["nonce"] {
			return Server{}, exit(4, "refusing to rewrite Daytona fixed lease identity")
		}
	}
	return server, nil
}

func (b *daytonaLeaseBackend) AuthorizeStatusTouchClaim(ctx context.Context, lease LeaseTarget, claim core.LeaseClaim) error {
	if claim.LeaseID != lease.LeaseID || claim.Provider != daytonaProvider || lease.Server.CloudID == "" || lease.Server.CloudID != claim.CloudID {
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
