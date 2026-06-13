package agentsandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	labelLeaseID  = "crabbox.openclaw.dev/lease-id"
	labelSlug     = "crabbox.openclaw.dev/slug"
	labelProvider = "crabbox.openclaw.dev/provider"

	annotationScope     = "crabbox.openclaw.dev/provider-scope"
	annotationWorkdir   = "crabbox.openclaw.dev/workdir"
	annotationContainer = "crabbox.openclaw.dev/container"

	claimLabelClaimName   = "claim"
	claimLabelSandboxName = "sandbox"
	claimLabelPodName     = "pod"
	claimLabelNamespace   = "namespace"
	claimLabelWarmPool    = "warm_pool"
	claimLabelContainer   = "container"
	claimLabelWorkdir     = "workdir"
)

var dns1123LabelPattern = regexp.MustCompile(`[^a-z0-9-]+`)

func claimName(leaseID, slug string) string {
	base := normalizeKubernetesName(slug)
	if base == "" {
		base = normalizeKubernetesName(newLeaseSlug(leaseID))
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

func claimScope(cfg Config) string {
	values := cfg.AgentSandbox
	container := strings.TrimSpace(values.Container)
	if container == "" {
		container = "default"
	}
	return strings.Join([]string{
		"kubeconfig:" + effectiveKubeconfigIdentity(values),
		"context:" + strings.TrimSpace(values.Context),
		"namespace:" + strings.TrimSpace(values.Namespace),
		"warmPool:" + strings.TrimSpace(values.WarmPool),
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

func claimAnnotations(cfg Config) map[string]string {
	container := strings.TrimSpace(cfg.AgentSandbox.Container)
	if container == "" {
		container = "default"
	}
	return map[string]string{
		annotationScope:     claimScope(cfg),
		annotationWorkdir:   cfg.AgentSandbox.Workdir,
		annotationContainer: container,
	}
}

func safeLabelValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 63 {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return strings.TrimRight(value[:54], "-_.") + "-" + hex.EncodeToString(sum[:])[:8]
}

func claimLeaseForRepo(cfg Config, leaseID, slug string, repo Repo, reclaim bool) error {
	return claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, claimScope(cfg), cfg.Pond, repo.Root, cfg.IdleTimeout, reclaim)
}

func writeClaimLease(cfg Config, leaseID, slug string, repo Repo, reclaim bool, ready sandboxReadiness, claimName string) error {
	if err := claimLeaseForRepo(cfg, leaseID, slug, repo, reclaim); err != nil {
		return err
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return err
	}
	_, err = updateLeaseClaimLabelsIfUnchanged(leaseID, claim, claimMetadataLabels(cfg, leaseID, ready, claimName))
	return err
}

func refreshClaimLeaseActivity(cfg Config, claim LeaseClaim) error {
	idleTimeout := cfg.IdleTimeout
	if idleTimeout <= 0 && claim.IdleTimeoutSeconds > 0 {
		idleTimeout = time.Duration(claim.IdleTimeoutSeconds) * time.Second
	}
	if err := claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, claim.Pond, claim.RepoRoot, idleTimeout, false); err != nil {
		return err
	}
	updated, err := readLeaseClaim(claim.LeaseID)
	if err != nil {
		return err
	}
	_, err = updateLeaseClaimLabelsIfUnchanged(claim.LeaseID, updated, claim.Labels)
	return err
}

func claimMetadataLabels(cfg Config, leaseID string, ready sandboxReadiness, claimName string) map[string]string {
	container := strings.TrimSpace(cfg.AgentSandbox.Container)
	if container == "" {
		container = "default"
	}
	return map[string]string{
		"provider":            providerName,
		"lease":               leaseID,
		claimLabelClaimName:   claimName,
		claimLabelSandboxName: ready.SandboxName,
		claimLabelPodName:     ready.PodName,
		claimLabelNamespace:   cfg.AgentSandbox.Namespace,
		claimLabelWarmPool:    cfg.AgentSandbox.WarmPool,
		claimLabelContainer:   container,
		claimLabelWorkdir:     cfg.AgentSandbox.Workdir,
		"target":              targetLinux,
		"state":               statusViewReady,
	}
}

func authorizeClaimScope(cfg Config, claim LeaseClaim) error {
	if claim.Provider != "" && claim.Provider != providerName {
		return exit(2, "lease %s belongs to provider=%s, not %s", claim.LeaseID, claim.Provider, providerName)
	}
	if got, want := strings.TrimSpace(claim.ProviderScope), claimScope(cfg); got != "" && got != want {
		return exit(2, "lease %s belongs to a different agent-sandbox scope", claim.LeaseID)
	}
	return nil
}

func retainMissingClaim(cfg Config, claim LeaseClaim) error {
	if cfg.AgentSandbox.ForgetMissing {
		removeLeaseClaim(claim.LeaseID)
		return nil
	}
	return fmt.Errorf("agent-sandbox claim %s is missing in Kubernetes; local claim retained because forgetMissing=false", claim.LeaseID)
}

func resolveLocalClaim(identifier string) (LeaseClaim, error) {
	claim, ok, err := resolveLeaseClaimForProvider(identifier, providerName)
	if err != nil {
		return LeaseClaim{}, err
	}
	if !ok {
		return LeaseClaim{}, exit(4, "agent-sandbox lease %q is not claimed by Crabbox", identifier)
	}
	return claim, nil
}

func listAgentSandboxLeaseClaims() ([]LeaseClaim, error) {
	return listLeaseClaimsWithPrefix(leasePrefix)
}

func claimCleanupDue(claim LeaseClaim, now time.Time) (bool, string) {
	if claim.IdleTimeoutSeconds <= 0 {
		return false, "idle timeout disabled"
	}
	lastUsed, err := time.Parse(time.RFC3339, strings.TrimSpace(claim.LastUsedAt))
	if err != nil {
		return false, "invalid last-used time"
	}
	deadline := lastUsed.Add(time.Duration(claim.IdleTimeoutSeconds) * time.Second)
	if now.Before(deadline) {
		return false, "idle timeout not reached"
	}
	return true, "idle timeout"
}

func claimNameFromLocalClaim(claim LeaseClaim) string {
	if claim.Labels != nil {
		if value := strings.TrimSpace(claim.Labels[claimLabelClaimName]); value != "" {
			return value
		}
	}
	return strings.TrimPrefix(claim.LeaseID, leasePrefix)
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func isNotFound(err error) bool {
	return apierrors.IsNotFound(err) || errors.Is(err, errKubernetesNotFound)
}

func newLeaseID() string {
	return leasePrefix + core.NewLeaseID()[4:]
}

func readinessTimeout(cfg Config) time.Duration {
	timeout := cfg.AgentSandbox.SandboxReadyTimeout
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	return timeout
}
