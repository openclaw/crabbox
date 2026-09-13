package agentsandbox

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	labelLeaseID  = "crabbox.dev/lease-id"
	labelSlug     = "crabbox.dev/slug"
	labelProvider = "crabbox.dev/provider"

	annotationScope     = "crabbox.dev/provider-scope"
	annotationWorkdir   = "crabbox.dev/workdir"
	annotationContainer = "crabbox.dev/container"
	annotationRecovery  = "crabbox.dev/recovery-nonce"

	claimLabelClaimName       = "claim"
	claimLabelClaimUID        = "claim_uid"
	claimLabelClaimUIDPending = "claim_uid_pending"
	claimLabelRecoveryNonce   = "claim_recovery_nonce"
	claimLabelSandboxName     = "sandbox"
	claimLabelPodName         = "pod"
	claimLabelNamespace       = "namespace"
	claimLabelWarmPool        = "warm_pool"
	claimLabelContainer       = "container"
	claimLabelContainerPinned = "container_pinned"
	claimLabelWorkdir         = "workdir"
	claimLabelExpiresAt       = "expires_at"
)

type claimIdentity struct {
	LeaseID       string
	ProviderScope string
	UID           string
	WarmPool      string
	ExpiresAt     string
	Container     string
}

var dns1123LabelPattern = regexp.MustCompile(`[^a-z0-9-]+`)

func claimName(leaseID, slug string) string {
	base := normalizeKubernetesName(slug)
	if base == "" {
		base = normalizeKubernetesName(core.NewLeaseSlug(leaseID))
	}
	if base == "" {
		base = "sandbox"
	}
	sum := sha256.Sum256([]byte(leaseID))
	leaseSuffix := "-" + hex.EncodeToString(sum[:])[:8]
	name := namePrefix + base + leaseSuffix
	if len(name) <= 63 {
		return name
	}
	return strings.TrimRight(name[:63-len(leaseSuffix)], "-") + leaseSuffix
}

func normalizeKubernetesName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")
	value = dns1123LabelPattern.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if len(value) > 63 {
		value = strings.TrimRight(value[:63], "-")
	}
	return value
}

func claimScope(cfg core.Config) string {
	values := cfg.AgentSandbox
	container := strings.TrimSpace(values.Container)
	containerMode := "implicit"
	if container != "" {
		containerMode = "explicit"
	}
	return strings.Join([]string{
		"kubeconfig:" + effectiveKubeconfigIdentity(values),
		"context:" + strings.TrimSpace(values.Context),
		"namespace:" + strings.TrimSpace(values.Namespace),
		"warmPool:" + strings.TrimSpace(values.WarmPool),
		"containerMode:" + containerMode,
		"container:" + container,
	}, "|")
}

func claimLabels(leaseID, slug string) map[string]string {
	return map[string]string{
		labelLeaseID:  safeLabelValue(leaseID),
		labelSlug:     safeLabelValue(slug),
		labelProvider: providerName,
	}
}

func claimAnnotations(cfg core.Config) map[string]string {
	return claimAnnotationsWithRecoveryNonce(cfg, "")
}

func claimAnnotationsWithRecoveryNonce(cfg core.Config, recoveryNonce string) map[string]string {
	container := strings.TrimSpace(cfg.AgentSandbox.Container)
	if container == "" {
		container = "default"
	}
	annotations := map[string]string{
		annotationScope:     scopeFingerprint(claimScope(cfg)),
		annotationWorkdir:   cfg.AgentSandbox.Workdir,
		annotationContainer: container,
	}
	if recoveryNonce != "" {
		annotations[annotationRecovery] = recoveryNonce
	}
	return annotations
}

func scopeFingerprint(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return hex.EncodeToString(sum[:])
}

func safeLabelValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 63 {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return strings.TrimRight(value[:54], "-_.") + "-" + hex.EncodeToString(sum[:])[:8]
}

func claimLeaseForRepo(cfg core.Config, leaseID, slug string, repo core.Repo, reclaim bool) error {
	return core.ClaimLeaseForRepoProviderScopePond(leaseID, slug, providerName, claimScope(cfg), cfg.Pond, repo.Root, cfg.IdleTimeout, reclaim)
}

func writeClaimLease(cfg core.Config, leaseID, slug string, repo core.Repo, reclaim bool, ready sandboxReadiness, claimName, expiresAt, recoveryNonce string) (core.LeaseClaim, error) {
	if err := claimLeaseForRepo(cfg, leaseID, slug, repo, reclaim); err != nil {
		return core.LeaseClaim{}, err
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	return core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, claim, claimMetadataLabels(cfg, leaseID, ready, claimName, expiresAt, recoveryNonce))
}

func refreshClaimLeaseActivity(cfg core.Config, claim core.LeaseClaim) error {
	idleTimeout := cfg.IdleTimeout
	if idleTimeout <= 0 && claim.IdleTimeoutSeconds > 0 {
		idleTimeout = time.Duration(claim.IdleTimeoutSeconds) * time.Second
	}
	if err := core.ClaimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, claim.Pond, claim.RepoRoot, idleTimeout, false); err != nil {
		return err
	}
	updated, err := core.ReadLeaseClaim(claim.LeaseID)
	if err != nil {
		return err
	}
	_, err = core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, updated, claim.Labels)
	return err
}

func claimMetadataLabels(cfg core.Config, leaseID string, ready sandboxReadiness, claimName, expiresAt, recoveryNonce string) map[string]string {
	container := strings.TrimSpace(ready.Container)
	if container == "" {
		container = strings.TrimSpace(cfg.AgentSandbox.Container)
	}
	if container == "" {
		container = "pending"
	}
	containerPinned := "false"
	if ready.Container != "" {
		containerPinned = "true"
	}
	state := statusViewReady
	if ready.SandboxName == "" || ready.PodName == "" {
		state = "not-ready"
	}
	labels := map[string]string{
		"provider":                providerName,
		"lease":                   leaseID,
		claimLabelClaimName:       claimName,
		claimLabelClaimUID:        ready.ClaimUID,
		claimLabelClaimUIDPending: fmt.Sprintf("%t", strings.TrimSpace(ready.ClaimUID) == ""),
		claimLabelSandboxName:     ready.SandboxName,
		claimLabelPodName:         ready.PodName,
		claimLabelNamespace:       cfg.AgentSandbox.Namespace,
		claimLabelWarmPool:        cfg.AgentSandbox.WarmPool,
		claimLabelContainer:       container,
		claimLabelContainerPinned: containerPinned,
		claimLabelWorkdir:         cfg.AgentSandbox.Workdir,
		"target":                  targetLinux,
		"state":                   state,
	}
	if expiresAt != "" {
		labels[claimLabelExpiresAt] = expiresAt
	}
	if recoveryNonce != "" {
		labels[claimLabelRecoveryNonce] = recoveryNonce
	}
	return labels
}

func claimReadinessLabels(labels map[string]string, ready sandboxReadiness) map[string]string {
	updated := shared.CloneLabels(labels)
	updated[claimLabelSandboxName] = ready.SandboxName
	updated[claimLabelPodName] = ready.PodName
	updated[claimLabelContainer] = ready.Container
	updated[claimLabelContainerPinned] = "true"
	updated["state"] = statusViewReady
	return updated
}

func claimIdentityFromLocalClaim(claim core.LeaseClaim) (claimIdentity, error) {
	uid := ""
	if claim.Labels != nil {
		uid = strings.TrimSpace(claim.Labels[claimLabelClaimUID])
	}
	if uid == "" {
		return claimIdentity{}, core.Exit(4, "agent-sandbox lease %s has no pinned Kubernetes claim UID", claim.LeaseID)
	}
	return claimIdentityFromLocalClaimWithUID(claim, uid)
}

func claimIdentityFromLocalClaimWithUID(claim core.LeaseClaim, uid string) (claimIdentity, error) {
	warmPool := ""
	expiresAt := ""
	container := ""
	containerPinned := false
	if claim.Labels != nil {
		warmPool = strings.TrimSpace(claim.Labels[claimLabelWarmPool])
		expiresAt = strings.TrimSpace(claim.Labels[claimLabelExpiresAt])
		container = strings.TrimSpace(claim.Labels[claimLabelContainer])
		containerPinned = strings.EqualFold(strings.TrimSpace(claim.Labels[claimLabelContainerPinned]), "true")
	}
	if warmPool == "" {
		return claimIdentity{}, core.Exit(4, "agent-sandbox lease %s has no pinned SandboxWarmPool", claim.LeaseID)
	}
	if !containerPinned && strings.Contains(claim.ProviderScope, "containerMode:implicit|") {
		container = ""
	}
	return claimIdentity{LeaseID: claim.LeaseID, ProviderScope: claim.ProviderScope, UID: uid, WarmPool: warmPool, ExpiresAt: expiresAt, Container: container}, nil
}

func authorizeClaimScope(cfg core.Config, claim core.LeaseClaim) error {
	if claim.Provider != "" && claim.Provider != providerName {
		return core.Exit(2, "lease %s belongs to provider=%s, not %s", claim.LeaseID, claim.Provider, providerName)
	}
	if got, want := strings.TrimSpace(claim.ProviderScope), claimScope(cfg); got != "" && got != want {
		return core.Exit(2, "lease %s belongs to a different agent-sandbox scope", claim.LeaseID)
	}
	return nil
}

func authorizeAgentSandboxRepoClaim(claim core.LeaseClaim, repoRoot string, reclaim bool) error {
	if repoRoot == "" || claim.RepoRoot == "" || claim.RepoRoot == repoRoot || reclaim {
		return nil
	}
	return core.Exit(2, "lease %s is claimed by repo %s; use --reclaim to claim it for %s", claim.LeaseID, claim.RepoRoot, repoRoot)
}

func retainMissingClaim(cfg core.Config, claim core.LeaseClaim) error {
	if cfg.AgentSandbox.ForgetMissing {
		if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
			return fmt.Errorf("remove forgotten agent-sandbox lease %s: %w", claim.LeaseID, err)
		}
		return nil
	}
	return fmt.Errorf("agent-sandbox claim %s is missing in Kubernetes; local claim retained because forgetMissing=false", claim.LeaseID)
}

func resolveLocalClaim(identifier string) (core.LeaseClaim, error) {
	claim, ok, err := core.ResolveLeaseClaimForProvider(identifier, providerName)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if !ok {
		claim, ok, err = resolveLocalClaimByClaimName(identifier)
		if err != nil {
			return core.LeaseClaim{}, err
		}
	}
	if !ok {
		return core.LeaseClaim{}, core.Exit(4, "agent-sandbox lease %q is not claimed by Crabbox", identifier)
	}
	return claim, nil
}

func resolveLocalClaimByClaimName(identifier string) (core.LeaseClaim, bool, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return core.LeaseClaim{}, false, nil
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	var match core.LeaseClaim
	for _, claim := range claims {
		if claim.Provider != providerName || strings.TrimSpace(claim.Labels[claimLabelClaimName]) != identifier {
			continue
		}
		if match.LeaseID != "" {
			return core.LeaseClaim{}, false, core.Exit(2, "multiple agent-sandbox claims match claim name %s", identifier)
		}
		match = claim
	}
	return match, match.LeaseID != "", nil
}

func listAgentSandboxLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}

func claimCleanupDue(claim core.LeaseClaim, now time.Time) (bool, string) {
	if claimTTLExpired(claim, now) {
		return true, "ttl"
	}
	return shared.ClaimIdleCleanupDue(claim, now)
}

func claimTTLExpired(claim core.LeaseClaim, now time.Time) bool {
	expiresAt := strings.TrimSpace(claim.Labels[claimLabelExpiresAt])
	if expiresAt == "" {
		return false
	}
	deadline, err := time.Parse(time.RFC3339, expiresAt)
	return err == nil && !now.Before(deadline)
}

func claimNameFromLocalClaim(claim core.LeaseClaim) string {
	if claim.Labels != nil {
		if value := strings.TrimSpace(claim.Labels[claimLabelClaimName]); value != "" {
			return value
		}
	}
	return strings.TrimPrefix(claim.LeaseID, leasePrefix)
}

func newClaimRecoveryNonce() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate agent-sandbox recovery nonce: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func isNotFound(err error) bool {
	return errors.Is(err, errKubernetesNotFound)
}

func newLeaseID() string {
	return leasePrefix + core.NewLeaseID()[4:]
}

func readinessTimeout(cfg core.Config) time.Duration {
	timeout := cfg.AgentSandbox.SandboxReadyTimeout
	if timeout <= 0 {
		timeout = core.AgentSandboxConfigDefaultSandboxReadyTimeout
	}
	return timeout
}

func podReadinessTimeout(cfg core.Config) time.Duration {
	timeout := cfg.AgentSandbox.PodReadyTimeout
	if timeout <= 0 {
		timeout = core.AgentSandboxConfigDefaultPodReadyTimeout
	}
	return timeout
}
