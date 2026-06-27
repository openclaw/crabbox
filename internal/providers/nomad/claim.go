package nomad

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
)

const (
	claimLabelJobID        = "job_id"
	claimLabelAllocationID = "allocation_id"
	claimLabelTask         = "task"
	claimLabelWorkdir      = "workdir"
	claimLabelNamespace    = "namespace"
	claimLabelRegion       = "region"
	claimLabelNodeID       = "node_id"
	claimLabelNodeName     = "node_name"
	claimLabelClientStatus = "client_status"
	claimLabelDesired      = "desired_status"
	claimLabelExpiresAt    = "expires_at"
	claimLabelState        = "state"
)

func newLeaseID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", exit(5, "generate nomad lease id: %v", err)
	}
	return leasePrefix + hex.EncodeToString(b[:]), nil
}

func claimScope(cfg Config) string {
	return strings.Join([]string{
		"address:" + strings.TrimSpace(cfg.Nomad.Address),
		"namespace:" + normalizeNamespace(cfg.Nomad.Namespace),
		"region:" + normalizeRegion(cfg.Nomad.Region),
		"task:" + strings.TrimSpace(cfg.Nomad.Task),
	}, "|")
}

func writeNomadClaim(cfg Config, leaseID, slug string, repo Repo, reclaim bool, ready allocationReadiness, expiresAt time.Time) (LeaseClaim, error) {
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, claimScope(cfg), cfg.Pond, repo.Root, cfg.IdleTimeout, reclaim); err != nil {
		return LeaseClaim{}, err
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return LeaseClaim{}, err
	}
	return updateLeaseClaimLabelsIfUnchanged(leaseID, claim, claimLabels(cfg, leaseID, slug, ready, expiresAt))
}

func claimLabels(cfg Config, leaseID, slug string, ready allocationReadiness, expiresAt time.Time) map[string]string {
	labels := map[string]string{
		"provider":                providerName,
		"lease":                   leaseID,
		"slug":                    slug,
		"target":                  targetLinux,
		claimLabelJobID:           ready.JobID,
		claimLabelAllocationID:    ready.AllocationID,
		claimLabelTask:            cfg.Nomad.Task,
		claimLabelWorkdir:         cfg.Nomad.Workdir,
		claimLabelNamespace:       normalizeNamespace(cfg.Nomad.Namespace),
		claimLabelRegion:          normalizeRegion(cfg.Nomad.Region),
		claimLabelNodeID:          ready.NodeID,
		claimLabelNodeName:        ready.NodeName,
		claimLabelClientStatus:    ready.ClientStatus,
		claimLabelDesired:         ready.DesiredStatus,
		claimLabelState:           ready.State(),
		"ownership_scope_sha256":  scopeFingerprint(claimScope(cfg)),
		"ownership_scope_version": "1",
	}
	if !expiresAt.IsZero() {
		labels[claimLabelExpiresAt] = expiresAt.UTC().Format(time.RFC3339)
	}
	return labels
}

func resolveNomadClaim(cfg Config, id string) (LeaseClaim, error) {
	claim, ok, err := resolveLeaseClaimForProvider(id, providerName)
	if err != nil {
		return LeaseClaim{}, err
	}
	if !ok || claim.LeaseID == "" {
		return LeaseClaim{}, exit(4, "nomad lease %q is not claimed by Crabbox; use a Crabbox slug or lease id", id)
	}
	if err := authorizeClaimScope(cfg, claim); err != nil {
		return LeaseClaim{}, err
	}
	if strings.TrimSpace(claim.Labels[claimLabelJobID]) == "" {
		return LeaseClaim{}, exit(4, "nomad lease %s has no pinned job id", claim.LeaseID)
	}
	return claim, nil
}

func authorizeClaimScope(cfg Config, claim LeaseClaim) error {
	if claim.Provider != "" && claim.Provider != providerName {
		return exit(2, "lease %s belongs to provider=%s, not %s", claim.LeaseID, claim.Provider, providerName)
	}
	if got, want := strings.TrimSpace(claim.ProviderScope), claimScope(cfg); got != "" && got != want {
		return exit(2, "lease %s belongs to a different nomad scope", claim.LeaseID)
	}
	if labels := claim.Labels; labels != nil {
		if got, want := strings.TrimSpace(labels[claimLabelNamespace]), normalizeNamespace(cfg.Nomad.Namespace); got != "" && got != want {
			return exit(2, "lease %s belongs to nomad namespace=%s, not %s", claim.LeaseID, got, want)
		}
		if got, want := strings.TrimSpace(labels[claimLabelRegion]), normalizeRegion(cfg.Nomad.Region); got != "" && got != want {
			return exit(2, "lease %s belongs to nomad region=%s, not %s", claim.LeaseID, got, want)
		}
	}
	return nil
}

func listNomadLeaseClaims() ([]LeaseClaim, error) {
	return listLeaseClaimsWithPrefix(leasePrefix)
}

func validateRemoteOwnership(cfg Config, claim LeaseClaim, job *nomadapi.Job) error {
	if job == nil {
		return exit(4, "nomad job for lease %s is missing or inaccessible", claim.LeaseID)
	}
	jobID := claim.Labels[claimLabelJobID]
	if got := stringValue(job.ID); got != "" && got != jobID {
		return exit(4, "nomad job %q does not match local claim job %q", got, jobID)
	}
	want := map[string]string{
		metadataManaged:   "true",
		metadataLeaseID:   claim.LeaseID,
		metadataProvider:  providerName,
		metadataScope:     scopeFingerprint(claimScope(cfg)),
		metadataNamespace: normalizeNamespace(cfg.Nomad.Namespace),
		metadataRegion:    normalizeRegion(cfg.Nomad.Region),
		metadataJobID:     jobID,
		metadataTask:      claim.Labels[claimLabelTask],
		metadataWorkdir:   claim.Labels[claimLabelWorkdir],
	}
	if claim.Slug != "" {
		want[metadataSlug] = claim.Slug
	}
	if !metadataMatches(job.Meta, want) {
		return exit(4, "nomad job %s is missing matching Crabbox ownership metadata", jobID)
	}
	return nil
}

func claimCleanupDue(claim LeaseClaim, now time.Time) (bool, string) {
	if expiresAt := strings.TrimSpace(claim.Labels[claimLabelExpiresAt]); expiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, expiresAt)
		if err == nil && !parsed.After(now) {
			return true, "ttl_expired"
		}
	}
	if claim.IdleTimeoutSeconds > 0 && claim.LastUsedAt != "" {
		usedAt, err := time.Parse(time.RFC3339, claim.LastUsedAt)
		if err == nil && !usedAt.Add(time.Duration(claim.IdleTimeoutSeconds)*time.Second).After(now) {
			return true, "idle_expired"
		}
	}
	return false, "retained"
}
