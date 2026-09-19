package agentsandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	fixedPoolUIDLabel = "fixed_warm_pool_uid"
	// Foreground completion includes the workload termination grace period.
	fixedCleanupTimeout = 2 * time.Minute
)

var fixedClaimKind = core.FixedLeaseKind{
	ClaimProvider: core.FixedAgentSandboxClaimProvider, IntentVersion: 1, Label: "agent-sandbox",
	TerminalIdentityLabels: []string{"provider", "lease", claimLabelClaimName, claimLabelClaimUID,
		claimLabelNamespace, claimLabelWarmPool, fixedPoolUIDLabel},
}

func (b *backend) ValidateConfirmedAbsentTerminalReceipt(claim core.LeaseClaim, req core.ConfirmedAbsentLocalCleanupRequest) error {
	expected := req.ExpectedProviderIdentity
	if expected.LeaseID == "" || expected.AttemptLeaseID == "" || expected.Slug == "" || expected.ResourceID == "" || req.ProviderScope != claimScope(b.cfg) || claim.ProviderScope != req.ProviderScope {
		return core.Exit(4, "agent-sandbox terminal receipt requires complete matching identity and scope")
	}
	if err := core.ValidateProviderIdentityExpectation(expected); err != nil {
		return err
	}
	if err := validateFixedClaimShape(claim); err != nil {
		return err
	}
	if claim.FixedCreateIntent.State != "released" {
		return core.Exit(4, "agent-sandbox fixed receipt is not terminal")
	}
	return validateFixedExpectation(claim, expected)
}

func (b *backend) SupportsRequestedLeaseID() bool { return true }

func isFixedClaim(claim core.LeaseClaim) bool {
	return claim.Provider == core.FixedAgentSandboxClaimProvider
}

func validateFixedClaimShape(claim core.LeaseClaim) error {
	intent := claim.FixedCreateIntent
	if !isFixedClaim(claim) || intent == nil || intent.Version != fixedClaimKind.IntentVersion ||
		!core.IsCanonicalLeaseID(claim.LeaseID) || intent.ProviderScope != claim.ProviderScope ||
		intent.Slug != claim.Slug || intent.Fingerprint == "" {
		return core.Exit(4, "agent-sandbox fixed claim identity is unresolved")
	}
	if intent.State == "released" {
		if claim.CloudID == "" || claim.CloudImmutableID != claim.CloudID || claim.Labels[claimLabelClaimUID] != claim.CloudID || claim.Labels[claimLabelClaimName] == "" || claim.Labels[fixedPoolUIDLabel] == "" {
			return core.Exit(4, "agent-sandbox terminal receipt has incomplete resource identity")
		}
		return fixedClaimKind.ValidateTerminalClaim(claim, core.LeaseClaim{}, claim.LeaseID, nil)
	}
	if intent.State != "prepared" && intent.State != "acquired" {
		return core.Exit(4, "agent-sandbox fixed claim state is unresolved")
	}
	return nil
}

func (b *backend) fixedAnchor(ctx context.Context, client kubernetesClient, claim core.LeaseClaim) error {
	if err := validateFixedClaimShape(claim); err != nil {
		return err
	}
	if claim.ProviderScope != claimScope(b.cfg) {
		return core.Exit(4, "agent-sandbox fixed scope changed")
	}
	pool, err := client.Get(ctx, warmPoolGVR(), b.cfg.AgentSandbox.Namespace, b.cfg.AgentSandbox.WarmPool)
	if err != nil {
		return fmt.Errorf("agent-sandbox fixed pool authority unresolved: %w", err)
	}
	if pool == nil || pool.Metadata.Name != b.cfg.AgentSandbox.WarmPool || pool.Metadata.UID == "" || pool.Metadata.UID != claim.Labels[fixedPoolUIDLabel] {
		return core.Exit(4, "agent-sandbox fixed warm pool incarnation changed")
	}
	return nil
}

func (b *backend) fixedReusable(ctx context.Context, client kubernetesClient, claim core.LeaseClaim) error {
	if err := b.fixedAnchor(ctx, client, claim); err != nil {
		return err
	}
	if claim.FixedCreateIntent.State != "acquired" || claim.FixedCreateIntent.Attempt["delete_pending"] != "" {
		return core.Exit(4, "agent-sandbox fixed lease is not available for reuse")
	}
	if claim.Labels["fixed_sandbox_uid"] == "" || claim.Labels["fixed_pod_uid"] == "" || claim.Labels[claimLabelContainerPinned] != "true" {
		return core.Exit(4, "agent-sandbox fixed workload identity is incomplete")
	}
	return nil
}

func fixedFingerprint(cfg core.Config, req core.FixedWarmupRequest) (string, error) {
	data, err := json.Marshal(struct {
		Scope, Workdir, Container, Slug string
		TTL, Idle                       time.Duration
		Keep, DeleteOnRelease           bool
	}{claimScope(cfg), cfg.AgentSandbox.Workdir, cfg.AgentSandbox.Container, req.RequestedSlug,
		cfg.TTL, cfg.IdleTimeout, req.Keep, cfg.AgentSandbox.DeleteOnRelease})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (b *backend) WarmupFixed(ctx context.Context, req core.FixedWarmupRequest) error {
	if !core.IsCanonicalLeaseID(req.RequestedLeaseID) || req.OnAcquired == nil {
		return core.Exit(2, "agent-sandbox fixed warmup requires an exact lease ID and acquisition observer")
	}
	if req.ActionsRunner || req.Options.Tailscale.Enabled {
		return core.Exit(2, "agent-sandbox fixed warmup does not support SSH-only options")
	}
	if b.cfg.TTL <= 0 {
		return core.Exit(2, "agent-sandbox fixed warmup requires a positive TTL")
	}
	client, err := b.client(ctx)
	if err != nil {
		return err
	}
	unlockSlug, err := lockAgentSandboxSlugAllocation(ctx, req.RequestedSlug)
	if err != nil {
		return err
	}
	defer unlockSlug()
	unlock, err := lockAgentSandboxLeaseOperation(ctx, req.RequestedLeaseID)
	if err != nil {
		return err
	}
	defer unlock()
	fingerprint, err := fixedFingerprint(b.cfg, req)
	if err != nil {
		return err
	}
	started := core.ClockNow(b.rt.Clock)
	var ready sandboxReadiness
	claim, err := core.AcquireFixedIntent(core.FixedAcquireOptions{
		Kind: fixedClaimKind, LeaseID: req.RequestedLeaseID, RepoRoot: req.Repo.Root, Reclaim: req.Reclaim,
		TargetOS: targetLinux, TTL: b.cfg.TTL, IdleTimeout: b.cfg.IdleTimeout, Now: func() time.Time { return core.ClockNow(b.rt.Clock) },
	}, func(_ context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
		slug := claim.Slug
		if !exists {
			var err error
			slug, err = allocateClaimLeaseSlug(req.RequestedLeaseID, req.RequestedSlug)
			if err != nil {
				return core.FixedLeaseBinding{}, err
			}
		}
		return core.FixedLeaseBinding{ProviderScope: claimScope(b.cfg), Fingerprint: fingerprint, Slug: slug}, nil
	}, func(ctx context.Context, claim *core.LeaseClaim, intent *core.FixedCreateIntent, persist func() error) error {
		if intent.Attempt == nil {
			pool, err := client.Get(ctx, warmPoolGVR(), b.cfg.AgentSandbox.Namespace, b.cfg.AgentSandbox.WarmPool)
			if err != nil {
				return err
			}
			if pool == nil || pool.Metadata.Name != b.cfg.AgentSandbox.WarmPool || pool.Metadata.UID == "" {
				return core.Exit(4, "agent-sandbox warm pool has no immutable UID")
			}
			nonce, err := newClaimRecoveryNonce()
			if err != nil {
				return err
			}
			createdAt, err := time.Parse(time.RFC3339Nano, intent.CreatedAt)
			if err != nil {
				return err
			}
			expires := createdAt.Add(b.cfg.TTL)
			if expires.Nanosecond() != 0 {
				expires = expires.Truncate(time.Second).Add(time.Second)
			}
			name := claimName(claim.LeaseID, intent.Slug)
			intent.Attempt = map[string]string{"name": name, "nonce": nonce, "expires": expires.UTC().Format(time.RFC3339)}
			claim.Labels = claimMetadataLabels(b.cfg, claim.LeaseID, sandboxReadiness{}, name, intent.Attempt["expires"], nonce)
			claim.Labels[fixedPoolUIDLabel] = pool.Metadata.UID
			claim.Pond = b.cfg.Pond
			if err := persist(); err != nil {
				return err
			}
		}
		if err := b.fixedAnchor(ctx, client, *claim); err != nil {
			return err
		}
		if intent.Attempt["delete_pending"] != "" {
			return core.Exit(4, "agent-sandbox fixed cleanup remains pending")
		}
		name, nonce, expires := intent.Attempt["name"], intent.Attempt["nonce"], intent.Attempt["expires"]
		if name == "" || nonce == "" || expires == "" {
			return core.Exit(4, "agent-sandbox fixed attempt is incomplete")
		}
		var live *kubernetesObject
		var err error
		if intent.Attempt["submitted"] == "" {
			intent.Attempt["submitted"] = "true"
			if err := persist(); err != nil {
				return err
			}
			obj := &kubernetesObject{APIVersion: agentSandboxExtensionsGroupVersion, Kind: "SandboxClaim",
				Metadata: objectMeta{Name: name, Namespace: b.cfg.AgentSandbox.Namespace, Labels: claimLabels(claim.LeaseID, intent.Slug), Annotations: claimAnnotationsWithRecoveryNonce(b.cfg, nonce)},
				Spec:     map[string]any{"warmPoolRef": map[string]any{"name": b.cfg.AgentSandbox.WarmPool}, "lifecycle": map[string]any{"shutdownPolicy": "Retain", "shutdownTime": expires}}}
			live, err = client.Create(ctx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, obj)
			if err != nil && !createMayHaveSucceeded(err) {
				return err
			}
			if err != nil || live == nil || live.Metadata.UID == "" {
				live, err = b.reconcileCreatedClaim(ctx, client, claim.LeaseID, name, expires, nonce, fmt.Errorf("fixed claim creation requires reconciliation"))
			}
		} else {
			live, err = client.Get(ctx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, name)
		}
		if err != nil {
			return fmt.Errorf("fixed claim attempt remains unresolved; no replacement created: %w", err)
		}
		if live == nil || live.Metadata.UID == "" || live.Metadata.Name != name {
			return core.Exit(4, "agent-sandbox fixed claim response has no exact identity")
		}
		uid := claim.CloudImmutableID
		if uid == "" {
			uid = live.Metadata.UID
		}
		identity := claimIdentity{LeaseID: claim.LeaseID, ProviderScope: claim.ProviderScope, UID: uid, WarmPool: b.cfg.AgentSandbox.WarmPool, ExpiresAt: expires, Container: b.cfg.AgentSandbox.Container}
		if err := validateClaimIdentity(live, identity); err != nil {
			return err
		}
		if err := validateClaimRecoveryNonce(live, nonce); err != nil {
			return err
		}
		claim.CloudID, claim.CloudImmutableID = uid, uid
		claim.Labels[claimLabelClaimUID], claim.Labels[claimLabelClaimUIDPending] = uid, "false"
		if err := persist(); err != nil {
			return err
		}
		if err := req.OnAcquired(core.FixedAcquisitionReceipt{LeaseID: claim.LeaseID, Slug: claim.Slug, Provider: providerName, ResourceID: uid}); err != nil {
			return err
		}
		ready, err = b.waitForClaimReadiness(ctx, client, name, identity)
		if err != nil {
			return err
		}
		if claimTTLExpired(*claim, core.ClockNow(b.rt.Clock).UTC()) {
			return core.Exit(4, "agent-sandbox fixed lease reached its original TTL before readiness publication")
		}
		if err := validateFixedWorkloadPins(*claim, ready); err != nil {
			return err
		}
		claim.Labels = claimReadinessLabels(claim.Labels, ready)
		claim.Labels["fixed_sandbox_uid"], claim.Labels["fixed_pod_uid"] = ready.SandboxUID, ready.PodUID
		claim.RepoRoot = req.Repo.Root
		return nil
	}, ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s claim=%s sandbox=%s pod=%s\n", claim.LeaseID, claim.Slug, providerName, claimNameFromLocalClaim(claim), ready.SandboxName, ready.PodName)
	if req.BeforeComplete != nil {
		req.BeforeComplete()
	}
	return shared.CompleteWarmup(b.rt, req.TimingJSON, shared.WarmupCompletion{Provider: providerName, LeaseID: claim.LeaseID, Slug: claim.Slug, Total: core.ClockNow(b.rt.Clock).Sub(started)})
}

func validateFixedExpectation(claim core.LeaseClaim, expected core.ProviderIdentityExpectation) error {
	if expected.LeaseID != "" && expected.LeaseID != claim.LeaseID ||
		expected.AttemptLeaseID != "" && expected.AttemptLeaseID != claim.LeaseID ||
		expected.Slug != "" && expected.Slug != claim.Slug ||
		expected.ResourceID != "" && expected.ResourceID != claim.CloudImmutableID {
		return core.Exit(4, "agent-sandbox fixed release identity changed")
	}
	return nil
}

func (b *backend) StopFixed(ctx context.Context, req core.FixedStopRequest) error {
	if err := core.ValidateProviderIdentityExpectation(req.ExpectedProviderIdentity); err != nil {
		return err
	}
	return b.stopFixed(ctx, req.ID, req.ExpectedProviderIdentity, "")
}

func (b *backend) stopFixed(ctx context.Context, id string, expected core.ProviderIdentityExpectation, repo string) error {
	claim, err := resolveLocalClaim(id)
	if err != nil {
		return err
	}
	if !isFixedClaim(claim) {
		return core.Exit(2, "agent-sandbox expected release requires a fixed claim")
	}
	unlock, err := lockAgentSandboxLeaseOperation(ctx, claim.LeaseID)
	if err != nil {
		return err
	}
	defer unlock()
	return b.releaseFixedLocked(ctx, nil, claim, expected, repo)
}

func (b *backend) releaseFixedLocked(ctx context.Context, client kubernetesClient, expectedClaim core.LeaseClaim, expected core.ProviderIdentityExpectation, repo string) error {
	cleanupCtx, cancel := context.WithTimeout(ctx, fixedCleanupTimeout)
	defer cancel()
	return core.WithDurableLeaseClaimLockContext(cleanupCtx, expectedClaim.LeaseID, func(claim *core.LeaseClaim, exists bool, persist func() error) error {
		if !exists || !reflect.DeepEqual(*claim, expectedClaim) {
			return core.Exit(4, "agent-sandbox fixed claim changed before release")
		}
		if err := validateFixedClaimShape(*claim); err != nil {
			return err
		}
		if err := validateFixedExpectation(*claim, expected); err != nil {
			return err
		}
		if repo != "" && claim.RepoRoot != repo {
			return core.Exit(4, "agent-sandbox fixed repository changed")
		}
		if err := authorizeClaimScope(b.cfg, *claim); err != nil {
			return err
		}
		intent := claim.FixedCreateIntent
		if intent.State == "released" {
			return nil
		}
		if client == nil {
			var err error
			client, err = b.client(cleanupCtx)
			if err != nil {
				return err
			}
		}
		if err := b.fixedAnchor(cleanupCtx, client, *claim); err != nil {
			return err
		}
		if claim.CloudImmutableID == "" {
			return core.Exit(4, "agent-sandbox fixed creation remains unresolved")
		}
		name := claimNameFromLocalClaim(*claim)
		live, err := client.Get(cleanupCtx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, name)
		if err != nil && !(isNotFound(err) && intent.Attempt["delete_ack"] == claim.CloudImmutableID) {
			return err
		}
		if err == nil {
			if _, _, err := b.claimIdentityForLiveClaim(*claim, live, false); err != nil {
				return err
			}
			deleter, ok := client.(interface {
				DeleteForeground(context.Context, resourceRef, string, string, string) error
			})
			if !ok {
				return core.Exit(4, "agent-sandbox client cannot confirm foreground deletion")
			}
			intent.Attempt["delete_pending"] = claim.CloudImmutableID
			if err := persist(); err != nil {
				return err
			}
			if err := deleter.DeleteForeground(cleanupCtx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, name, claim.CloudImmutableID); err != nil {
				return err
			}
			intent.Attempt["delete_ack"] = claim.CloudImmutableID
			if err := persist(); err != nil {
				return err
			}
		}
		for {
			if err := b.fixedAnchor(cleanupCtx, client, *claim); err != nil {
				return err
			}
			live, err := client.Get(cleanupCtx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, name)
			if isNotFound(err) {
				break
			}
			if err != nil {
				return err
			}
			if live == nil || live.Metadata.UID != claim.CloudImmutableID {
				return core.Exit(4, "agent-sandbox fixed deletion identity changed")
			}
			if err := shared.SleepContext(cleanupCtx, time.Second); err != nil {
				return err
			}
		}
		for _, child := range []struct {
			name, uid string
			ref       resourceRef
		}{
			{claim.Labels[claimLabelSandboxName], claim.Labels["fixed_sandbox_uid"], sandboxGVR()},
			{claim.Labels[claimLabelPodName], claim.Labels["fixed_pod_uid"], resourceRef{GroupVersion: "v1", Resource: "pods"}},
		} {
			if child.name == "" || child.uid == "" {
				continue
			}
			_, err := client.Get(cleanupCtx, child.ref, b.cfg.AgentSandbox.Namespace, child.name)
			if !isNotFound(err) {
				if err != nil {
					return err
				}
				return core.Exit(4, "agent-sandbox fixed descendant still present")
			}
		}
		*claim = fixedClaimKind.TerminalClaim(*claim, core.ClockNow(b.rt.Clock))
		return persist()
	})
}

func (b *backend) StopForRepository(ctx context.Context, req core.StopRequest, repo string) error {
	claim, err := resolveLocalClaim(req.ID)
	if err != nil {
		return err
	}
	if !isFixedClaim(claim) {
		return core.Exit(2, "agent-sandbox repository-scoped stop requires a fixed claim")
	}
	return b.stopFixed(ctx, req.ID, core.ProviderIdentityExpectation{}, repo)
}

// Previously published workload identity cannot be replaced by a later readiness read.
func validateFixedWorkloadPins(claim core.LeaseClaim, ready sandboxReadiness) error {
	for _, pin := range []struct{ recorded, observed string }{
		{claim.Labels["fixed_sandbox_uid"], ready.SandboxUID},
		{claim.Labels["fixed_pod_uid"], ready.PodUID},
	} {
		if pin.recorded != "" && pin.recorded != pin.observed {
			return core.Exit(4, "agent-sandbox fixed workload identity changed")
		}
	}
	if claim.Labels[claimLabelContainerPinned] == "true" && claim.Labels[claimLabelContainer] != ready.Container {
		return core.Exit(4, "agent-sandbox fixed workload container changed")
	}
	return nil
}
