package tenki

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	tenkiMetadataAttempt  = "crabbox_create_attempt"
	tenkiMetadataIntent   = "crabbox_intent_sha256"
	tenkiFixedIntentLabel = "fixed_intent_sha256"
	tenkiFixedRouteLabel  = "tenki_fixed_route"
)

var tenkiFixedKind = core.FixedLeaseKind{
	ClaimProvider: tenkiProvider, IntentVersion: 1, Label: "Tenki",
	TerminalIdentityLabels: []string{"tenki_session_id", tenkiFixedIntentLabel, tenkiFixedRouteLabel, "project_id"},
}

var _ core.IdempotentLeaseIDBackend = (*tenkiBackend)(nil)
var _ core.ReleaseLeaseClaimRetentionVerifier = (*tenkiBackend)(nil)
var _ core.ReleaseLeaseOutcomeBackend = (*tenkiBackend)(nil)

func (*tenkiBackend) SupportsRequestedLeaseID() bool { return true }

// The Tenki CLI create contract exposes no caller ID or idempotency input.
// The random token is evidence from a durable local submission, not permission
// to adopt a session found by its name or lease metadata alone.
type tenkiCreateAttempt struct {
	Name      string `json:"name"`
	Token     string `json:"token"`
	Route     string `json:"route"`
	SessionID string `json:"sessionId,omitempty"`
	Image     string `json:"image,omitempty"`
	Snapshot  string `json:"snapshot,omitempty"`
	CPUs      int    `json:"cpus,omitempty"`
	MemoryMB  int    `json:"memoryMB,omitempty"`
	DiskGB    int    `json:"diskGB,omitempty"`
	Keep      bool   `json:"keep"`
}

func tenkiFixedRoute(cfg core.Config) string {
	data, _ := json.Marshal([]string{(Provider{}).ClaimScope(cfg), tenkiCLIPath(cfg), strings.TrimSpace(cfg.Tenki.Gateway), tenkiWorkRoot(cfg)})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func tenkiFixedFingerprint(cfg core.Config, req core.AcquireRequest) (string, error) {
	// Labels are computed from configuration, never from renewed claim labels.
	// No clock, generated provider key, credential, or SSH certificate is identity.
	labels := core.DirectLeaseLabels(cfg, "", "", tenkiProvider, "", req.Keep, time.Unix(0, 0))
	for _, key := range []string{"created_at", "last_touched_at", "expires_at", "provider_key"} {
		delete(labels, key)
	}
	return core.FixedIntentFingerprint("crabbox-fixed-tenki-v1\x00", struct {
		Version                                             int
		Slug, Route, Image, Snapshot, Architecture, OSImage string
		CPUs, MemoryMB, DiskGB                              int
		Keep                                                bool
		TTL, Idle                                           time.Duration
		Labels                                              map[string]string
		Cache                                               core.CacheConfig
	}{1, core.NormalizeLeaseSlug(req.RequestedSlug), tenkiFixedRoute(cfg), cfg.Tenki.Image, cfg.Tenki.Snapshot, cfg.Architecture, cfg.OSImage,
		cfg.Tenki.CPUs, cfg.Tenki.MemoryMB, cfg.Tenki.DiskGB, req.Keep, cfg.TTL, cfg.IdleTimeout, labels, cfg.Cache})
}

func (b *tenkiBackend) acquireFixed(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	leaseID := strings.TrimSpace(req.RequestedLeaseID)
	if !core.IsCanonicalLeaseID(leaseID) {
		return core.LeaseTarget{}, core.Exit(2, "invalid fixed Tenki lease ID %q", leaseID)
	}
	if req.RequestedCheckpointID != "" {
		return core.LeaseTarget{}, core.Exit(2, "provider=tenki does not support fixed checkpoint IDs")
	}
	cfg := b.configForRun()
	lease, err := core.AcquireFixedResource(ctx, core.FixedAcquireOptions{
		Kind: tenkiFixedKind, LeaseID: leaseID, RepoRoot: req.Repo.Root,
		TargetOS: targetLinux, TTL: cfg.TTL, IdleTimeout: cfg.IdleTimeout, Now: tenkiNow,
	}, core.FixedLeaseOperations[tenkiSession]{Admission: &core.FixedAdmission{FreshOnly: true}, DescribeIntent: func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
		if err := ctx.Err(); err != nil {
			return core.FixedLeaseBinding{}, err
		}
		if exists && (claim.Provider != tenkiProvider || claim.FixedCreateIntent == nil || claim.RepoRoot != req.Repo.Root) {
			return core.FixedLeaseBinding{}, core.Exit(4, "lease_id_conflict: fixed Tenki lease %s belongs to another provider, repository, or create mode", leaseID)
		}
		fingerprint, err := tenkiFixedFingerprint(cfg, req)
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		binding := core.FixedLeaseBinding{ProviderScope: (Provider{}).ClaimScope(cfg), Fingerprint: fingerprint}
		if !exists {
			binding.Slug, err = core.AllocateClaimLeaseSlug(leaseID, req.RequestedSlug)
		}
		return binding, err
	}, ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, mode core.FixedObserveMode) (core.FixedObservation[tenkiSession], error) {
		if err := core.AuthorizeCheckpointRelease(*tx.Claim, ""); err != nil {
			return core.FixedObservation[tenkiSession]{}, err
		}
		return b.observeFixedSession(ctx, tx, mode)
	}, Plan: func(ctx context.Context, claim core.LeaseClaim) (core.FixedAttemptPlan, error) {
		return core.FixedAttemptPlan{JSONKey: "tenki", NonceKey: "token", NonceBytes: 32,
			Payload: tenkiCreateAttempt{Name: core.LeaseProviderName(leaseID, claim.Slug), Route: tenkiFixedRoute(cfg),
				Image: cfg.Tenki.Image, Snapshot: cfg.Tenki.Snapshot, CPUs: cfg.Tenki.CPUs, MemoryMB: cfg.Tenki.MemoryMB, DiskGB: cfg.Tenki.DiskGB, Keep: req.Keep}}, ctx.Err()
	}, Submit: func(ctx context.Context, tx *core.FixedTransaction) (tenkiSession, error) {
		intent := tx.Claim.FixedCreateIntent
		attempt, err := b.fixedAttempt(*tx.Claim)
		if err != nil {
			return tenkiSession{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "provisioning provider=tenki lease=%s slug=%s session=%s keep=%v fixed=true\n", leaseID, intent.Slug, attempt.Name, req.Keep)
		created, createErr := b.submitCreateSession(ctx, cfg, attempt.Name, leaseID, intent.Slug, req.Keep, []string{
			tenkiMetadataAttempt + "=" + attempt.Token, tenkiMetadataIntent + "=" + intent.Fingerprint,
		})
		if created.ID != "" {
			// Returned-ID evidence must survive cancellation before full attestation.
			if err := tx.Bind(core.FixedResourceBinding{AttemptJSONKey: "tenki", AttemptValues: map[string]string{"sessionId": created.ID}}); err != nil {
				return tenkiSession{}, errors.Join(createErr, err)
			}
		}
		if createErr != nil {
			return tenkiSession{}, createErr
		}
		return b.resolveFixedSession(ctx, *tx.Claim)
	}, PrepareAccess: func(ctx context.Context, tx *core.FixedTransaction, session tenkiSession) (core.LeaseTarget, error) {
		if err := rejectTerminalTenkiSession(session); err != nil {
			return core.LeaseTarget{}, err
		}
		if err := ctx.Err(); err != nil {
			return core.LeaseTarget{}, err
		}
		binding, err := b.fixedSessionBinding(*tx.Claim, session)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if err := tx.Bind(binding); err != nil {
			return core.LeaseTarget{}, err
		}
		return b.prepareFixedLease(ctx, *tx.Claim, session)
	}})
	return core.CompleteFixedAcquisition(lease, err, req)
}

func (b *tenkiBackend) fixedClaimRules(claim core.LeaseClaim) core.FixedClaimRules {
	return core.FixedClaimRules{Kind: tenkiFixedKind, States: []string{"prepared", "acquired"},
		Scope: (Provider{}).ClaimScope(b.configForRun()), IntentScope: claim.ProviderScope,
		RequireCanonicalID: true, RequireSlug: true, RequireTimestamp: true, RequireSHA256: true,
		NoCheckpoint: true, NoFailedAttempts: true, NoNumericID: true, SameImmutableID: true,
		EmptyAttemptMustBePristine: true, UnboundMustBeBare: true,
		BoundLabels: map[string]string{"tenki_session_id": claim.CloudID, tenkiFixedIntentLabel: claim.FixedCreateIntent.Fingerprint, tenkiFixedRouteLabel: tenkiFixedRoute(b.configForRun())},
	}
}

func (b *tenkiBackend) fixedAttempt(claim core.LeaseClaim) (*tenkiCreateAttempt, error) {
	if claim.FixedCreateIntent == nil {
		return nil, core.FixedUncertainCustody(claim.LeaseID)
	}
	if err := core.ValidateFixedClaim(claim, b.fixedClaimRules(claim)); err != nil {
		return nil, err
	}
	equal := map[string]string{"name": core.LeaseProviderName(claim.LeaseID, claim.Slug), "route": tenkiFixedRoute(b.configForRun())}
	if claim.CloudID != "" {
		equal["sessionId"] = claim.CloudID
	}
	return core.ReadFixedAttempt[tenkiCreateAttempt](claim.FixedCreateIntent, core.FixedAttemptFormat{
		JSONKey: "tenki", ExactKeys: 1, Equal: equal, SHA256: []string{"token"}, Trimmed: []string{"sessionId"},
	})
}

func (b *tenkiBackend) validateFixedSession(claim core.LeaseClaim, session tenkiSession) error {
	attempt, err := b.fixedAttempt(claim)
	if err != nil {
		return err
	}
	if attempt == nil {
		return core.Exit(4, "lease_id_conflict: Tenki session %s has no durable local attempt", session.ID)
	}
	if strings.TrimSpace(session.ID) == "" || session.ID != strings.TrimSpace(session.ID) ||
		(attempt.SessionID != "" && session.ID != attempt.SessionID) || (claim.CloudID != "" && session.ID != claim.CloudID) ||
		session.Name != attempt.Name || !tenkiHasExactOwnership(session, claim.LeaseID, claim.Slug) ||
		session.Metadata[tenkiMetadataAttempt] != attempt.Token || session.Metadata[tenkiMetadataIntent] != claim.FixedCreateIntent.Fingerprint {
		return core.Exit(4, "lease_id_conflict: fixed Tenki lease %s session identity or ownership metadata does not match its durable attempt", claim.LeaseID)
	}
	if (claim.Labels["project_id"] != "" && session.ProjectID != claim.Labels["project_id"]) || session.Sticky != attempt.Keep ||
		(attempt.Image != "" && session.SourceImageRef != attempt.Image) || (attempt.Snapshot != "" && session.SourceSnapshotID != attempt.Snapshot) ||
		(attempt.CPUs > 0 && session.CPUCores != attempt.CPUs) || (attempt.MemoryMB > 0 && session.MemoryMB != attempt.MemoryMB) || (attempt.DiskGB > 0 && session.DiskSizeGB != attempt.DiskGB) {
		return core.Exit(4, "lease_id_conflict: fixed Tenki lease %s session configuration differs from its durable attempt", claim.LeaseID)
	}
	return nil
}

func tenkiHasFixedMetadata(session tenkiSession) bool {
	return session.Metadata[tenkiMetadataAttempt] != "" || session.Metadata[tenkiMetadataIntent] != ""
}

func rejectTerminalTenkiSession(session tenkiSession) error {
	switch tenkiNormalizedState(session.State) {
	case "terminating", "terminated":
		return core.Exit(4, "lease_id_conflict: fixed Tenki session %s is terminal (%s)", session.ID, session.State)
	}
	return nil
}

// Inventory is discovery only. Empty inventory, errors, or a process restart
// never authorize another create. A known ID can still be attested by get while
// list converges. Every matching candidate must resolve to one exact session.
func (b *tenkiBackend) resolveFixedSession(ctx context.Context, claim core.LeaseClaim) (tenkiSession, error) {
	observed, err := core.InspectFixedResource(ctx, tenkiFixedKind, claim, core.FixedLeaseOperations[tenkiSession]{ObserveExact: b.observeFixedSession})
	if err != nil {
		return tenkiSession{}, err
	}
	if len(observed.Candidates) == 0 {
		return tenkiSession{}, nil
	}
	return observed.Candidates[0], nil
}

func (b *tenkiBackend) observeFixedSession(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[tenkiSession], error) {
	claim := *tx.Claim
	result := core.FixedObservation[tenkiSession]{CanSubmit: true}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	attempt, err := b.fixedAttempt(claim)
	if err != nil {
		return result, err
	}
	sessions, err := b.listSessions(ctx, true)
	if cause := ctx.Err(); cause != nil {
		return result, cause
	}
	if err != nil {
		return result, err
	}
	name := core.LeaseProviderName(claim.LeaseID, claim.Slug)
	selected, found, err := core.SelectFixedCandidate(tenkiFixedKind, claim.LeaseID, sessions, func(session tenkiSession) bool {
		return session.Name == name || session.Metadata[tenkiMetadataLease] == claim.LeaseID || (claim.CloudID != "" && session.ID == claim.CloudID) ||
			(attempt != nil && (session.Metadata[tenkiMetadataAttempt] == attempt.Token || (attempt.SessionID != "" && session.ID == attempt.SessionID)))
	})
	if err != nil {
		return result, err
	}
	var candidate *tenkiSession
	if found {
		candidate = &selected
	}

	if attempt == nil {
		if candidate != nil {
			return result, core.Exit(4, "lease_id_conflict: Tenki session matches %s without a durable local attempt", claim.LeaseID)
		}
		return result, nil
	}
	id := attempt.SessionID
	if candidate != nil {
		if candidate.ID == "" || (id != "" && candidate.ID != id) {
			return result, core.Exit(4, "lease_id_conflict: Tenki inventory identity differs for %s", claim.LeaseID)
		}
		id = candidate.ID
	}
	if id == "" {
		return result, core.Exit(4, "lease_id_conflict: fixed Tenki lease %s has an unresolved create attempt; retain its claim", claim.LeaseID)
	}
	detail, err := b.getSession(ctx, id)
	if cause := ctx.Err(); cause != nil {
		return result, cause
	}
	if err != nil {
		return result, err
	}
	if detail.ID != id {
		return result, core.Exit(4, "lease_id_conflict: Tenki detail session identity differs from requested ID %s", id)
	}
	if candidate != nil && (candidate.Name != detail.Name || !maps.Equal(candidate.Metadata, detail.Metadata)) {
		return result, core.Exit(4, "lease_id_conflict: Tenki detail ownership differs from inventory for %s", claim.LeaseID)
	}
	if err := b.validateFixedSession(claim, detail); err != nil {
		return result, err
	}
	return core.FixedObservation[tenkiSession]{Candidates: []tenkiSession{detail}}, nil
}

func (b *tenkiBackend) fixedSessionBinding(claim core.LeaseClaim, session tenkiSession) (core.FixedResourceBinding, error) {
	if err := b.validateFixedSession(claim, session); err != nil {
		return core.FixedResourceBinding{}, err
	}
	attempt, err := b.fixedAttempt(claim)
	if err != nil {
		return core.FixedResourceBinding{}, err
	}
	labels := b.sessionToServer(b.configForRun(), session, claim.LeaseID, claim.Slug, attempt.Keep).Labels
	labels[tenkiFixedRouteLabel] = attempt.Route
	return core.FixedResourceBinding{OnlyUnbound: true,
		CloudID: session.ID, ImmutableID: session.ID, AttemptJSONKey: "tenki", AttemptIdentityKey: "sessionId",
		Labels: labels, FingerprintLabel: tenkiFixedIntentLabel,
	}, nil
}

func (b *tenkiBackend) bindFixedSession(claim *core.LeaseClaim, session tenkiSession, persist func() error) error {
	binding, err := b.fixedSessionBinding(*claim, session)
	if err != nil {
		return err
	}
	return core.BindFixedClaim(claim, binding, persist)
}

func (b *tenkiBackend) fixedServer(claim core.LeaseClaim, session tenkiSession) core.Server {
	server := b.sessionToServer(b.configForRun(), session, claim.LeaseID, claim.Slug, session.Sticky)
	// Local heartbeat labels are newer than create-time provider metadata.
	maps.Copy(server.Labels, claim.Labels)
	server.Labels["state"] = tenkiState(session.State)
	server.Labels[tenkiFixedIntentLabel] = claim.FixedCreateIntent.Fingerprint
	server.Labels[tenkiFixedRouteLabel] = tenkiFixedRoute(b.configForRun())
	server.ImmutableID = session.ID
	return server
}

func (b *tenkiBackend) prepareFixedLease(ctx context.Context, claim core.LeaseClaim, session tenkiSession) (core.LeaseTarget, error) {
	validate := func(session tenkiSession) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := b.validateFixedSession(claim, session); err != nil {
			return err
		}
		return rejectTerminalTenkiSession(session)
	}
	cfg := b.configForRun()
	session, err := b.ensureSessionReadyForSSH(ctx, cfg, session, validate)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if !tenkiSessionReady(session) {
		session, err = b.waitForSessionReady(ctx, session.ID, core.BootstrapWaitTimeout(cfg), validate)
		if err != nil {
			return core.LeaseTarget{}, err
		}
	}
	if err := validate(session); err != nil {
		return core.LeaseTarget{}, err
	}
	target, err := b.resolveSSHTarget(ctx, cfg, session.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if err := waitForSSHReadyFunc(ctx, &target, b.rt.Stderr, "tenki sandbox ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
		return core.LeaseTarget{}, err
	}
	session, err = b.getSession(ctx, session.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if err := validate(session); err != nil {
		return core.LeaseTarget{}, err
	}
	if !tenkiSessionReady(session) {
		return core.LeaseTarget{}, core.Exit(4, "fixed Tenki session %s is no longer ready", session.ID)
	}
	return core.LeaseTarget{LeaseID: claim.LeaseID, Server: b.fixedServer(claim, session), SSH: target}, nil
}

func fixedTenkiClaimEvidence(claim core.LeaseClaim) bool {
	return claim.FixedCreateIntent != nil || claim.Labels[tenkiFixedIntentLabel] != ""
}

func (b *tenkiBackend) fixedClaimForIdentifier(identifier string) (core.LeaseClaim, bool, error) {
	claim, exists, err := core.ResolveLeaseClaim(identifier)
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	if !exists {
		claim, exists, err = core.ResolveLeaseClaimForProviderCloudID(identifier, tenkiProvider)
		if err != nil {
			return core.LeaseClaim{}, false, err
		}
	}
	return claim, exists && fixedTenkiClaimEvidence(claim), nil
}

func (b *tenkiBackend) resolveFixed(ctx context.Context, req core.ResolveRequest, expected core.LeaseClaim) (core.LeaseTarget, error) {
	return core.ResolveFixedLeaseTarget(ctx, core.FixedResolveOptions{Kind: tenkiFixedKind, Request: req, Expected: expected,
		Provider: tenkiProvider, ResourceName: core.LeaseProviderName(expected.LeaseID, expected.Slug), TerminalState: "terminated",
		ValidateTerminal: b.validateTerminalClaim, Now: tenkiNow,
	}, func(ctx context.Context, claim *core.LeaseClaim, persist func() error) (core.LeaseTarget, error) {
		session, err := b.resolveFixedSession(ctx, *claim)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if session.ID == "" {
			return core.LeaseTarget{}, core.FixedUncertainCustody(claim.LeaseID)
		}
		if err := rejectTerminalTenkiSession(session); err != nil {
			return core.LeaseTarget{}, err
		}
		if err := ctx.Err(); err != nil {
			return core.LeaseTarget{}, err
		}
		if err := b.bindFixedSession(claim, session, persist); err != nil {
			return core.LeaseTarget{}, err
		}
		return b.prepareFixedLease(ctx, *claim, session)
	}, func(ctx context.Context, claim core.LeaseClaim) (core.LeaseTarget, error) {
		session, err := b.resolveFixedSession(ctx, claim)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if session.ID == "" {
			return core.LeaseTarget{}, core.FixedUncertainCustody(claim.LeaseID)
		}
		lease := core.LeaseTarget{LeaseID: claim.LeaseID, Server: b.fixedServer(claim, session)}
		if claim.CloudID != "" && (req.ReadyProbe || req.IncludeDiagnostics) && tenkiSessionReady(session) {
			lease.SSH, err = b.resolveSSHTarget(ctx, b.configForRun(), session.ID)
			if err != nil {
				return core.LeaseTarget{}, err
			}
			observed, err := b.getSession(ctx, session.ID)
			if err != nil {
				return core.LeaseTarget{}, err
			}
			if observed.ID != session.ID {
				return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: Tenki inspection session identity changed")
			}
			if err := b.validateFixedSession(claim, observed); err != nil {
				return core.LeaseTarget{}, err
			}
			lease.Server = b.fixedServer(claim, observed)
			if !tenkiSessionReady(observed) {
				lease.SSH = core.SSHTarget{}
			}
		}
		return lease, nil
	})
}

// Renew only the exact durable local claim after a fresh identity check.
// This does not resume the session, prepare SSH, or change Tenki's idle timer.
func (b *tenkiBackend) touchFixed(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	updated, err := shared.CommitClaimTouch(ctx, req, shared.ClaimTouchPolicy{
		Provider: tenkiProvider,
		Authorize: func(ctx context.Context, lease core.LeaseTarget, claim core.LeaseClaim) error {
			if err := core.AuthorizeCheckpointRelease(claim, ""); err != nil {
				return err
			}
			if claim.LeaseID != lease.LeaseID || claim.CloudID == "" || lease.Server.CloudID != claim.CloudID || lease.Server.ImmutableID != claim.CloudImmutableID {
				return core.Exit(4, "lease_id_conflict: fixed Tenki heartbeat has no exact bound identity")
			}
			session, err := b.resolveFixedSession(ctx, claim)
			if err != nil {
				return err
			}
			return rejectTerminalTenkiSession(session)
		},
		Prepare: func(claim core.LeaseClaim) (map[string]string, time.Time) {
			now := tenkiNow().UTC()
			labels := core.TouchDirectLeaseLabelsWithIdleTimeoutOverride(claim.Labels, b.configForRun(), req.State, now, req.IdleTimeoutOverride)
			return labels, now
		},
	})
	if err != nil {
		return core.Server{}, err
	}
	server := req.Lease.Server
	server.Labels = maps.Clone(updated.Labels)
	core.SetServerLeaseClaimSnapshot(&server, updated, true)
	return server, nil
}

func (b *tenkiBackend) validateTerminalClaim(claim core.LeaseClaim) error {
	rules := b.fixedClaimRules(claim)
	rules.States, rules.RequireBound, rules.EmptyAttemptMustBePristine = []string{"released"}, true, false
	rules.RequireSlug = false
	return core.ValidateFixedClaim(claim, rules)
}

func (b *tenkiBackend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	_, err := b.ReleaseLeaseWithOutcome(ctx, req)
	return err
}

func (b *tenkiBackend) ReleaseLeaseWithOutcome(ctx context.Context, req core.ReleaseLeaseRequest) (core.ReleaseLeaseOutcome, error) {
	var outcome core.ReleaseLeaseOutcome
	expected, exists, err := core.ReadLeaseClaimWithPresence(req.Lease.LeaseID)
	if err != nil {
		return outcome, err
	}
	if !exists || !fixedTenkiClaimEvidence(expected) {
		if req.Lease.Server.Labels[tenkiFixedIntentLabel] != "" {
			return outcome, core.Exit(4, "lease_id_conflict: fixed Tenki release has no durable claim")
		}
		err := b.releaseOrdinaryLease(ctx, req)
		outcome.Terminal = err == nil
		return outcome, err
	}
	snapshot, snapshotExists, snapshotSet := core.ServerLeaseClaimSnapshot(req.Lease.Server)
	if !snapshotSet || !snapshotExists || !reflect.DeepEqual(snapshot, expected) {
		return outcome, core.Exit(4, "lease_id_conflict: fixed Tenki claim changed after resolution; resolve again before release")
	}
	err = core.DeleteFixedResource(ctx, tenkiFixedKind, expected, core.FixedLeaseOperations[tenkiSession]{
		Release: &core.FixedReleasePolicy{CheckpointID: &req.CheckpointID, Outcome: &outcome},
		ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[tenkiSession], error) {
			claim := tx.Claim
			var result core.FixedObservation[tenkiSession]
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if claim.FixedCreateIntent.State == "released" {
				err := tenkiFixedKind.ValidateTerminalClaim(*claim, snapshot, claim.LeaseID, b.validateTerminalClaim)
				return core.FixedObservation[tenkiSession]{AbsenceProven: err == nil}, err
			}
			session, err := b.resolveFixedSession(ctx, *claim)
			if err != nil {
				return result, err
			}
			if session.ID == "" {
				return result, core.Exit(4, "lease_id_conflict: fixed Tenki session absence is unverified; retain its claim")
			}
			if req.Lease.Server.CloudID != session.ID || req.Lease.Server.ImmutableID != session.ID || req.Lease.Server.Labels["slug"] != claim.Slug {
				return result, core.Exit(4, "lease_id_conflict: fixed Tenki release target changed")
			}
			binding, err := b.fixedSessionBinding(*claim, session)
			return core.FixedObservation[tenkiSession]{Candidates: []tenkiSession{session}, Binding: &binding}, err
		},
		DeleteExact: func(ctx context.Context, tx *core.FixedTransaction, session tenkiSession) error {
			claim := tx.Claim
			if err := ctx.Err(); err != nil {
				return err
			}
			// Re-attest full detail immediately before native mutation.
			session, err := b.getSession(ctx, claim.CloudID)
			if err != nil {
				return err
			}
			if err := b.validateFixedSession(*claim, session); err != nil {
				return err
			}
			if rejectTerminalTenkiSession(session) == nil {
				if err := b.terminateSession(ctx, session.ID); err != nil {
					return err
				}
				if err := b.waitForTerminationAcknowledged(ctx, session.ID, func(observed tenkiSession) error { return b.validateFixedSession(*claim, observed) }); err != nil {
					return err
				}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			return nil
		},
	}, tenkiNow)
	return outcome, err
}

func (b *tenkiBackend) RetainLeaseClaimAfterReleaseWithClaim(lease core.LeaseTarget, previous core.LeaseClaim) (bool, error) {
	return tenkiFixedKind.RetainClaimAfterRelease(lease.LeaseID, previous, lease.Server.Labels[tenkiFixedIntentLabel] != "", b.validateTerminalClaim, nil)
}
