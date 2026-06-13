package agentsandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	labelLeaseID  = "crabbox.openclaw.dev/lease-id"
	labelSlug     = "crabbox.openclaw.dev/slug"
	labelProvider = "crabbox.openclaw.dev/provider"

	annotationScope     = "crabbox.openclaw.dev/provider-scope"
	annotationWorkdir   = "crabbox.openclaw.dev/workdir"
	annotationContainer = "crabbox.openclaw.dev/container"
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
	name := namePrefix + base
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	return strings.TrimRight(name[:63-len(suffix)], "-") + suffix
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
