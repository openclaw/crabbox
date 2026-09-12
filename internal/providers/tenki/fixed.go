package tenki

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strconv"
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

func tenkiFixedRoute(cfg Config) string {
	data, _ := json.Marshal([]string{(Provider{}).ClaimScope(cfg), tenkiCLIPath(cfg), strings.TrimSpace(cfg.Tenki.Gateway), tenkiWorkRoot(cfg)})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func tenkiFixedFingerprint(cfg Config, req AcquireRequest) (string, error) {
	// Labels are computed from configuration, never from renewed claim labels.
	// No clock, generated provider key, credential, or SSH certificate is identity.
	labels := directLeaseLabels(cfg, "", "", tenkiProvider, "", req.Keep, time.Unix(0, 0))
	for _, key := range []string{"created_at", "last_touched_at", "expires_at", "provider_key"} {
		delete(labels, key)
	}
	data, err := json.Marshal(struct {
		Version                                             int
		Slug, Route, Image, Snapshot, Architecture, OSImage string
		CPUs, MemoryMB, DiskGB                              int
		Keep                                                bool
		TTL, Idle                                           time.Duration
		Labels                                              map[string]string
		Cache                                               core.CacheConfig
	}{1, normalizeLeaseSlug(req.RequestedSlug), tenkiFixedRoute(cfg), cfg.Tenki.Image, cfg.Tenki.Snapshot, cfg.Architecture, cfg.OSImage,
		cfg.Tenki.CPUs, cfg.Tenki.MemoryMB, cfg.Tenki.DiskGB, req.Keep, cfg.TTL, cfg.IdleTimeout, labels, cfg.Cache})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(append([]byte("crabbox-fixed-tenki-v1\x00"), data...))), nil
}

func (b *tenkiBackend) acquireFixed(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	leaseID := strings.TrimSpace(req.RequestedLeaseID)
	if !core.IsCanonicalLeaseID(leaseID) {
		return LeaseTarget{}, exit(2, "invalid fixed Tenki lease ID %q", leaseID)
	}
	if req.RequestedCheckpointID != "" {
		return LeaseTarget{}, exit(2, "provider=tenki does not support fixed checkpoint IDs")
	}
	cfg := b.configForRun()
	freshClaim := false
	lease, err := core.AcquireFixedLease(core.FixedAcquireOptions{
		Kind: tenkiFixedKind, LeaseID: leaseID, RepoRoot: req.Repo.Root,
		TargetOS: targetLinux, TTL: cfg.TTL, IdleTimeout: cfg.IdleTimeout, Now: tenkiNow,
	}, func(ctx context.Context, claim *LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
		if err := ctx.Err(); err != nil {
			return core.FixedLeaseBinding{}, err
		}
		freshClaim = !exists
		if exists && (claim.Provider != tenkiProvider || claim.FixedCreateIntent == nil || claim.RepoRoot != req.Repo.Root) {
			return core.FixedLeaseBinding{}, exit(4, "lease_id_conflict: fixed Tenki lease %s belongs to another provider, repository, or create mode", leaseID)
		}
		fingerprint, err := tenkiFixedFingerprint(cfg, req)
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		binding := core.FixedLeaseBinding{ProviderScope: (Provider{}).ClaimScope(cfg), Fingerprint: fingerprint}
		if !exists {
			binding.Slug, err = allocateClaimLeaseSlug(leaseID, req.RequestedSlug)
		}
		return binding, err
	}, func(ctx context.Context, claim *LeaseClaim, intent *core.FixedCreateIntent, persist func() error) (LeaseTarget, error) {
		if err := ctx.Err(); err != nil {
			return LeaseTarget{}, err
		}
		if err := core.AuthorizeCheckpointRelease(*claim, ""); err != nil {
			return LeaseTarget{}, err
		}
		session, err := b.resolveFixedSession(ctx, *claim)
		if err != nil {
			return LeaseTarget{}, err
		}
		if session.ID == "" {
			// An old prepared claim does not prove the submission never happened.
			// Only the invocation that made this claim may submit, once.
			if !freshClaim {
				return LeaseTarget{}, exit(4, "lease_id_conflict: fixed Tenki lease %s has no provably unsubmitted attempt; retain its claim", leaseID)
			}
			var token [32]byte
			if _, err := rand.Read(token[:]); err != nil {
				return LeaseTarget{}, err
			}
			attempt := tenkiCreateAttempt{Name: leaseProviderName(leaseID, intent.Slug), Token: hex.EncodeToString(token[:]), Route: tenkiFixedRoute(cfg),
				Image: cfg.Tenki.Image, Snapshot: cfg.Tenki.Snapshot, CPUs: cfg.Tenki.CPUs, MemoryMB: cfg.Tenki.MemoryMB, DiskGB: cfg.Tenki.DiskGB, Keep: req.Keep}
			if err := ctx.Err(); err != nil {
				return LeaseTarget{}, err
			}
			if err := saveTenkiAttempt(intent, attempt, persist); err != nil {
				return LeaseTarget{}, err
			}
			fmt.Fprintf(b.rt.Stderr, "provisioning provider=tenki lease=%s slug=%s session=%s keep=%v fixed=true\n", leaseID, intent.Slug, attempt.Name, req.Keep)
			created, createErr := b.submitCreateSession(ctx, cfg, attempt.Name, leaseID, intent.Slug, req.Keep, []string{
				tenkiMetadataAttempt + "=" + attempt.Token, tenkiMetadataIntent + "=" + intent.Fingerprint,
			})
			if created.ID != "" {
				// This is only returned-ID evidence, not an attested CloudID. Persist
				// even on cancellation or command failure before any following get.
				attempt.SessionID = created.ID
				if err := saveTenkiAttempt(intent, attempt, persist); err != nil {
					return LeaseTarget{}, errors.Join(createErr, err)
				}
			}
			if createErr != nil {
				return LeaseTarget{}, createErr
			}
			session, err = b.resolveFixedSession(ctx, *claim)
			if err != nil {
				return LeaseTarget{}, err
			}
		}
		if err := rejectTerminalTenkiSession(session); err != nil {
			return LeaseTarget{}, err
		}
		if err := ctx.Err(); err != nil {
			return LeaseTarget{}, err
		}
		if err := b.bindFixedSession(claim, session, persist); err != nil {
			return LeaseTarget{}, err
		}
		return b.prepareFixedLease(ctx, *claim, session)
	}, ctx)
	if err != nil {
		fmt.Fprintf(b.rt.Stderr, "fixed Tenki lease %s acquisition failed; any recorded attempt is retained. Retry the same request or inspect/stop this ID after provider inventory converges\n", leaseID)
		return LeaseTarget{}, err
	}
	// The callback can use claim APIs. It must run after the acquisition fence
	// is released, and failure must not delete this single-use identity.
	if req.OnAcquired != nil {
		if err := req.OnAcquired(lease); err != nil {
			return LeaseTarget{}, fmt.Errorf("acknowledge fixed Tenki acquisition: %w", err)
		}
	}
	return lease, nil
}

func saveTenkiAttempt(intent *core.FixedCreateIntent, attempt tenkiCreateAttempt, persist func() error) error {
	data, err := json.Marshal(attempt)
	if err != nil {
		return err
	}
	intent.Attempt = map[string]string{"tenki": string(data)}
	return persist()
}

func tenkiSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func (b *tenkiBackend) fixedAttempt(claim LeaseClaim) (*tenkiCreateAttempt, error) {
	intent := claim.FixedCreateIntent
	if !core.IsCanonicalLeaseID(claim.LeaseID) || !tenkiFixedKind.IsFixedClaim(claim) || intent.Version != 1 || !tenkiSHA256(intent.Fingerprint) ||
		intent.CheckpointID != "" || intent.Slug == "" || intent.Slug != claim.Slug || intent.ProviderScope != claim.ProviderScope ||
		claim.ProviderScope != (Provider{}).ClaimScope(b.configForRun()) || (intent.State != "prepared" && intent.State != "acquired") ||
		claim.CloudNumericID != 0 || claim.CloudImmutableID != claim.CloudID || len(intent.FailedAttempts) != 0 {
		return nil, exit(4, "lease_id_conflict: invalid fixed Tenki identity or provider scope for %s", claim.LeaseID)
	}
	if _, err := time.Parse(time.RFC3339Nano, intent.CreatedAt); err != nil {
		return nil, exit(4, "lease_id_conflict: invalid fixed Tenki timestamp for %s", claim.LeaseID)
	}
	if len(intent.Attempt) == 0 {
		if intent.State != "prepared" || claim.CloudID != "" || len(claim.Labels) != 0 || claim.SSHHost != "" || claim.SSHPort != 0 {
			return nil, exit(4, "lease_id_conflict: fixed Tenki lease %s has no durable attempt", claim.LeaseID)
		}
		return nil, nil
	}
	var attempt tenkiCreateAttempt
	if len(intent.Attempt) != 1 || json.Unmarshal([]byte(intent.Attempt["tenki"]), &attempt) != nil ||
		attempt.Name != leaseProviderName(claim.LeaseID, claim.Slug) || !tenkiSHA256(attempt.Token) || attempt.Route != tenkiFixedRoute(b.configForRun()) ||
		(attempt.SessionID != "" && strings.TrimSpace(attempt.SessionID) != attempt.SessionID) ||
		(claim.CloudID != "" && attempt.SessionID != claim.CloudID) ||
		(claim.CloudID == "" && (intent.State == "acquired" || len(claim.Labels) != 0 || claim.SSHHost != "" || claim.SSHPort != 0)) {
		return nil, exit(4, "lease_id_conflict: invalid fixed Tenki attempt or changed connection configuration for %s", claim.LeaseID)
	}
	if claim.CloudID != "" && (claim.Labels["tenki_session_id"] != claim.CloudID || claim.Labels[tenkiFixedIntentLabel] != intent.Fingerprint || claim.Labels[tenkiFixedRouteLabel] != attempt.Route) {
		return nil, exit(4, "lease_id_conflict: fixed Tenki lease %s has inconsistent bound labels", claim.LeaseID)
	}
	return &attempt, nil
}

func (b *tenkiBackend) validateFixedSession(claim LeaseClaim, session tenkiSession) error {
	attempt, err := b.fixedAttempt(claim)
	if err != nil {
		return err
	}
	if attempt == nil {
		return exit(4, "lease_id_conflict: Tenki session %s has no durable local attempt", session.ID)
	}
	if strings.TrimSpace(session.ID) == "" || session.ID != strings.TrimSpace(session.ID) ||
		(attempt.SessionID != "" && session.ID != attempt.SessionID) || (claim.CloudID != "" && session.ID != claim.CloudID) ||
		session.Name != attempt.Name || !tenkiHasExactOwnership(session, claim.LeaseID, claim.Slug) ||
		session.Metadata[tenkiMetadataAttempt] != attempt.Token || session.Metadata[tenkiMetadataIntent] != claim.FixedCreateIntent.Fingerprint {
		return exit(4, "lease_id_conflict: fixed Tenki lease %s session identity or ownership metadata does not match its durable attempt", claim.LeaseID)
	}
	if (claim.Labels["project_id"] != "" && session.ProjectID != claim.Labels["project_id"]) || session.Sticky != attempt.Keep ||
		(attempt.Image != "" && session.SourceImageRef != attempt.Image) || (attempt.Snapshot != "" && session.SourceSnapshotID != attempt.Snapshot) ||
		(attempt.CPUs > 0 && session.CPUCores != attempt.CPUs) || (attempt.MemoryMB > 0 && session.MemoryMB != attempt.MemoryMB) || (attempt.DiskGB > 0 && session.DiskSizeGB != attempt.DiskGB) {
		return exit(4, "lease_id_conflict: fixed Tenki lease %s session configuration differs from its durable attempt", claim.LeaseID)
	}
	return nil
}

func tenkiHasFixedMetadata(session tenkiSession) bool {
	return session.Metadata[tenkiMetadataAttempt] != "" || session.Metadata[tenkiMetadataIntent] != ""
}

func rejectTerminalTenkiSession(session tenkiSession) error {
	switch tenkiNormalizedState(session.State) {
	case "terminating", "terminated":
		return exit(4, "lease_id_conflict: fixed Tenki session %s is terminal (%s)", session.ID, session.State)
	}
	return nil
}

// Inventory is discovery only. Empty inventory, errors, or a process restart
// never authorize another create. A known ID can still be attested by get while
// list converges. Every matching candidate must resolve to one exact session.
func (b *tenkiBackend) resolveFixedSession(ctx context.Context, claim LeaseClaim) (tenkiSession, error) {
	if err := ctx.Err(); err != nil {
		return tenkiSession{}, err
	}
	attempt, err := b.fixedAttempt(claim)
	if err != nil {
		return tenkiSession{}, err
	}
	sessions, err := b.listSessions(ctx, true)
	if cause := ctx.Err(); cause != nil {
		return tenkiSession{}, cause
	}
	if err != nil {
		return tenkiSession{}, err
	}
	name := leaseProviderName(claim.LeaseID, claim.Slug)
	var candidate *tenkiSession
	for _, session := range sessions {
		matches := session.Name == name || session.Metadata[tenkiMetadataLease] == claim.LeaseID || (claim.CloudID != "" && session.ID == claim.CloudID)
		if attempt != nil {
			matches = matches || session.Metadata[tenkiMetadataAttempt] == attempt.Token || (attempt.SessionID != "" && session.ID == attempt.SessionID)
		}
		if !matches {
			continue
		}
		if candidate != nil {
			return tenkiSession{}, exit(4, "lease_id_conflict: multiple Tenki sessions match fixed lease %s", claim.LeaseID)
		}
		copy := session
		candidate = &copy
	}
	if attempt == nil {
		if candidate != nil {
			return tenkiSession{}, exit(4, "lease_id_conflict: Tenki session matches %s without a durable local attempt", claim.LeaseID)
		}
		return tenkiSession{}, nil
	}
	id := attempt.SessionID
	if candidate != nil {
		if candidate.ID == "" || (id != "" && candidate.ID != id) {
			return tenkiSession{}, exit(4, "lease_id_conflict: Tenki inventory identity differs for %s", claim.LeaseID)
		}
		id = candidate.ID
	}
	if id == "" {
		return tenkiSession{}, exit(4, "lease_id_conflict: fixed Tenki lease %s has an unresolved create attempt; retain its claim", claim.LeaseID)
	}
	detail, err := b.getSession(ctx, id)
	if cause := ctx.Err(); cause != nil {
		return tenkiSession{}, cause
	}
	if err != nil {
		return tenkiSession{}, err
	}
	if detail.ID != id {
		return tenkiSession{}, exit(4, "lease_id_conflict: Tenki detail session identity differs from requested ID %s", id)
	}
	if candidate != nil && (candidate.Name != detail.Name || !maps.Equal(candidate.Metadata, detail.Metadata)) {
		return tenkiSession{}, exit(4, "lease_id_conflict: Tenki detail ownership differs from inventory for %s", claim.LeaseID)
	}
	if err := b.validateFixedSession(claim, detail); err != nil {
		return tenkiSession{}, err
	}
	return detail, nil
}

func (b *tenkiBackend) bindFixedSession(claim *LeaseClaim, session tenkiSession, persist func() error) error {
	if err := b.validateFixedSession(*claim, session); err != nil {
		return err
	}
	if claim.CloudID != "" {
		return nil
	}
	attempt, err := b.fixedAttempt(*claim)
	if err != nil {
		return err
	}
	attempt.SessionID = session.ID
	claim.CloudID, claim.CloudImmutableID = session.ID, session.ID
	claim.Labels = b.sessionToServer(b.configForRun(), session, claim.LeaseID, claim.Slug, attempt.Keep).Labels
	claim.Labels[tenkiFixedIntentLabel], claim.Labels[tenkiFixedRouteLabel] = claim.FixedCreateIntent.Fingerprint, attempt.Route
	return saveTenkiAttempt(claim.FixedCreateIntent, *attempt, persist)
}

func (b *tenkiBackend) fixedServer(claim LeaseClaim, session tenkiSession) Server {
	server := b.sessionToServer(b.configForRun(), session, claim.LeaseID, claim.Slug, session.Sticky)
	// Local heartbeat labels are newer than create-time provider metadata.
	maps.Copy(server.Labels, claim.Labels)
	server.Labels["state"] = tenkiState(session.State)
	server.Labels[tenkiFixedIntentLabel] = claim.FixedCreateIntent.Fingerprint
	server.Labels[tenkiFixedRouteLabel] = tenkiFixedRoute(b.configForRun())
	server.ImmutableID = session.ID
	return server
}

func (b *tenkiBackend) prepareFixedLease(ctx context.Context, claim LeaseClaim, session tenkiSession) (LeaseTarget, error) {
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
		return LeaseTarget{}, err
	}
	if !tenkiSessionReady(session) {
		session, err = b.waitForSessionReady(ctx, session.ID, bootstrapWaitTimeout(cfg), validate)
		if err != nil {
			return LeaseTarget{}, err
		}
	}
	if err := validate(session); err != nil {
		return LeaseTarget{}, err
	}
	target, err := b.resolveSSHTarget(ctx, cfg, session.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if err := waitForSSHReadyFunc(ctx, &target, b.rt.Stderr, "tenki sandbox ssh", bootstrapWaitTimeout(cfg)); err != nil {
		return LeaseTarget{}, err
	}
	session, err = b.getSession(ctx, session.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if err := validate(session); err != nil {
		return LeaseTarget{}, err
	}
	if !tenkiSessionReady(session) {
		return LeaseTarget{}, exit(4, "fixed Tenki session %s is no longer ready", session.ID)
	}
	return LeaseTarget{LeaseID: claim.LeaseID, Server: b.fixedServer(claim, session), SSH: target}, nil
}

func fixedTenkiClaimEvidence(claim LeaseClaim) bool {
	return claim.FixedCreateIntent != nil || claim.Labels[tenkiFixedIntentLabel] != ""
}

func (b *tenkiBackend) fixedClaimForIdentifier(identifier string) (LeaseClaim, bool, error) {
	claim, exists, err := resolveLeaseClaim(identifier)
	if err != nil {
		return LeaseClaim{}, false, err
	}
	if !exists {
		claim, exists, err = core.ResolveLeaseClaimForProviderCloudID(identifier, tenkiProvider)
		if err != nil {
			return LeaseClaim{}, false, err
		}
	}
	return claim, exists && fixedTenkiClaimEvidence(claim), nil
}

func (b *tenkiBackend) resolveFixed(ctx context.Context, req ResolveRequest, expected LeaseClaim) (LeaseTarget, error) {
	var lease LeaseTarget
	err := core.WithDurableLeaseClaimLockContext(ctx, expected.LeaseID, func(claim *LeaseClaim, exists bool, persist func() error) error {
		if !exists || !reflect.DeepEqual(*claim, expected) {
			return exit(4, "lease_id_conflict: fixed Tenki claim changed during resolution; retry")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if req.Repo.Root != "" && claim.RepoRoot != req.Repo.Root {
			return exit(4, "lease_id_conflict: fixed Tenki lease %s belongs to another repository", claim.LeaseID)
		}
		if claim.FixedCreateIntent != nil && claim.FixedCreateIntent.State == "released" {
			if err := tenkiFixedKind.ValidateTerminalClaim(*claim, LeaseClaim{}, claim.LeaseID, b.validateTerminalClaim); err != nil {
				return err
			}
			if !req.StatusOnly && !req.ReleaseOnly {
				return exit(4, "lease_id_conflict: fixed Tenki lease %s is terminal", claim.LeaseID)
			}
			labels := maps.Clone(claim.Labels)
			labels["lease"], labels["slug"], labels["provider"], labels["state"] = claim.LeaseID, claim.Slug, tenkiProvider, "terminated"
			lease = LeaseTarget{LeaseID: claim.LeaseID, Server: Server{CloudID: claim.CloudID, ImmutableID: claim.CloudImmutableID, Provider: tenkiProvider, Name: leaseProviderName(claim.LeaseID, claim.Slug), Status: "terminated", Labels: labels}}
			core.SetServerLeaseClaimSnapshot(&lease.Server, *claim, true)
			return nil
		}
		session, err := b.resolveFixedSession(ctx, *claim)
		if err != nil {
			return err
		}
		if session.ID == "" {
			return exit(4, "lease_id_conflict: fixed Tenki lease %s has no observed session; retain its claim", claim.LeaseID)
		}
		if !req.StatusOnly && !req.ReleaseOnly {
			if err := rejectTerminalTenkiSession(session); err != nil {
				return err
			}
			if req.NoLocalStateMutations {
				return exit(4, "fixed Tenki command preparation requires durable identity binding")
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := core.AuthorizeCheckpointRelease(*claim, ""); err != nil {
				return err
			}
			if err := b.bindFixedSession(claim, session, persist); err != nil {
				return err
			}
			lease, err = b.prepareFixedLease(ctx, *claim, session)
			if err != nil {
				return err
			}
			claim.Labels, claim.SSHHost = maps.Clone(lease.Server.Labels), lease.SSH.Host
			claim.SSHPort, _ = strconv.Atoi(lease.SSH.Port)
			claim.LastUsedAt = tenkiNow().UTC().Format(time.RFC3339)
			claim.FixedCreateIntent.State = "acquired"
			if err := persist(); err != nil {
				return err
			}
		} else {
			// Inspection and release resolution do not resume, bind, or renew a claim.
			lease = LeaseTarget{LeaseID: claim.LeaseID, Server: b.fixedServer(*claim, session)}
			if claim.CloudID != "" && (req.ReadyProbe || req.IncludeDiagnostics) && tenkiSessionReady(session) {
				lease.SSH, err = b.resolveSSHTarget(ctx, b.configForRun(), session.ID)
				if err != nil {
					return err
				}
				observed, err := b.getSession(ctx, session.ID)
				if err != nil {
					return err
				}
				if observed.ID != session.ID {
					return exit(4, "lease_id_conflict: Tenki inspection session identity changed")
				}
				if err := b.validateFixedSession(*claim, observed); err != nil {
					return err
				}
				lease.Server = b.fixedServer(*claim, observed)
				if !tenkiSessionReady(observed) {
					lease.SSH = SSHTarget{}
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		core.SetServerLeaseClaimSnapshot(&lease.Server, *claim, true)
		return nil
	})
	return lease, err
}

// Renew only the exact durable local claim after a fresh identity check.
// This does not resume the session, prepare SSH, or change Tenki's idle timer.
func (b *tenkiBackend) touchFixed(ctx context.Context, req TouchRequest) (Server, error) {
	updated, err := shared.CommitClaimTouch(ctx, req, shared.ClaimTouchPolicy{
		Provider: tenkiProvider,
		Authorize: func(ctx context.Context, lease LeaseTarget, claim LeaseClaim) error {
			if err := core.AuthorizeCheckpointRelease(claim, ""); err != nil {
				return err
			}
			if claim.LeaseID != lease.LeaseID || claim.CloudID == "" || lease.Server.CloudID != claim.CloudID || lease.Server.ImmutableID != claim.CloudImmutableID {
				return exit(4, "lease_id_conflict: fixed Tenki heartbeat has no exact bound identity")
			}
			session, err := b.resolveFixedSession(ctx, claim)
			if err != nil {
				return err
			}
			return rejectTerminalTenkiSession(session)
		},
		Prepare: func(claim LeaseClaim) (map[string]string, time.Time) {
			now := tenkiNow().UTC()
			labels := core.TouchDirectLeaseLabelsWithIdleTimeoutOverride(claim.Labels, b.configForRun(), req.State, now, req.IdleTimeoutOverride)
			return labels, now
		},
	})
	if err != nil {
		return Server{}, err
	}
	server := req.Lease.Server
	server.Labels = maps.Clone(updated.Labels)
	core.SetServerLeaseClaimSnapshot(&server, updated, true)
	return server, nil
}

func (b *tenkiBackend) validateTerminalClaim(claim LeaseClaim) error {
	if !core.IsCanonicalLeaseID(claim.LeaseID) || claim.CloudID == "" || claim.CloudImmutableID != claim.CloudID || claim.CloudNumericID != 0 ||
		claim.ProviderScope != (Provider{}).ClaimScope(b.configForRun()) || claim.Labels["tenki_session_id"] != claim.CloudID ||
		claim.Labels[tenkiFixedIntentLabel] != claim.FixedCreateIntent.Fingerprint || !tenkiSHA256(claim.FixedCreateIntent.Fingerprint) ||
		claim.Labels[tenkiFixedRouteLabel] != tenkiFixedRoute(b.configForRun()) || claim.FixedCreateIntent.CheckpointID != "" {
		return exit(4, "lease_id_conflict: fixed Tenki lease %s has an invalid terminal receipt or changed connection configuration", claim.LeaseID)
	}
	if _, err := time.Parse(time.RFC3339Nano, claim.FixedCreateIntent.CreatedAt); err != nil {
		return exit(4, "lease_id_conflict: invalid terminal Tenki create timestamp")
	}
	return nil
}

func (b *tenkiBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	_, err := b.ReleaseLeaseWithOutcome(ctx, req)
	return err
}

func (b *tenkiBackend) ReleaseLeaseWithOutcome(ctx context.Context, req ReleaseLeaseRequest) (core.ReleaseLeaseOutcome, error) {
	var outcome core.ReleaseLeaseOutcome
	expected, exists, err := core.ReadLeaseClaimWithPresence(req.Lease.LeaseID)
	if err != nil {
		return outcome, err
	}
	if !exists || !fixedTenkiClaimEvidence(expected) {
		if req.Lease.Server.Labels[tenkiFixedIntentLabel] != "" {
			return outcome, exit(4, "lease_id_conflict: fixed Tenki release has no durable claim")
		}
		err := b.releaseOrdinaryLease(ctx, req)
		outcome.Terminal = err == nil
		return outcome, err
	}
	snapshot, snapshotExists, snapshotSet := core.ServerLeaseClaimSnapshot(req.Lease.Server)
	if !snapshotSet || !snapshotExists || !reflect.DeepEqual(snapshot, expected) {
		return outcome, exit(4, "lease_id_conflict: fixed Tenki claim changed after resolution; resolve again before release")
	}
	err = core.WithDurableLeaseClaimLockContext(ctx, expected.LeaseID, func(claim *LeaseClaim, exists bool, persist func() error) error {
		if !exists || !reflect.DeepEqual(*claim, expected) {
			return exit(4, "lease_id_conflict: fixed Tenki claim changed before release; retry")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := core.AuthorizeCheckpointRelease(*claim, req.CheckpointID); err != nil {
			return err
		}
		if claim.FixedCreateIntent != nil && claim.FixedCreateIntent.State == "released" {
			err := tenkiFixedKind.ValidateTerminalClaim(*claim, snapshot, claim.LeaseID, b.validateTerminalClaim)
			outcome.Terminal = err == nil
			return err
		}
		session, err := b.resolveFixedSession(ctx, *claim)
		if err != nil {
			return err
		}
		if session.ID == "" {
			return exit(4, "lease_id_conflict: fixed Tenki session absence is unverified; retain its claim")
		}
		if req.Lease.Server.CloudID != session.ID || req.Lease.Server.ImmutableID != session.ID || req.Lease.Server.Labels["slug"] != claim.Slug {
			return exit(4, "lease_id_conflict: fixed Tenki release target changed")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := b.bindFixedSession(claim, session, persist); err != nil {
			return err
		}
		// Re-attest full detail immediately before mutation under the same lock.
		session, err = b.getSession(ctx, claim.CloudID)
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
		outcome.Terminal = true
		*claim = tenkiFixedKind.TerminalClaim(*claim, tenkiNow().UTC())
		return persist()
	})
	return outcome, err
}

func (b *tenkiBackend) RetainLeaseClaimAfterReleaseWithClaim(lease LeaseTarget, previous LeaseClaim) (bool, error) {
	return tenkiFixedKind.RetainClaimAfterRelease(lease.LeaseID, previous, lease.Server.Labels[tenkiFixedIntentLabel] != "", b.validateTerminalClaim, nil)
}
