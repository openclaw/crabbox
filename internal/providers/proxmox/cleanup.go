package proxmox

import (
	"fmt"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func (b *leaseBackend) cleanupClaim(server Server, inventory []Server, claims []core.LeaseClaim) (core.LeaseClaim, shared.ClaimBinding, error) {
	leaseID := proxmoxClaimLabelLeaseID(server)
	scope := (Provider{}).ClaimScope(b.Cfg)
	binding := shared.ClaimBinding{
		Provider: "proxmox", ProviderScope: scope, ExactProviderScope: true,
		LeaseID: leaseID, Slug: server.Labels["slug"], CloudID: server.CloudID,
		RequiredLabels: map[string]string{"crabbox": "true", "provider_key": core.ProviderKeyForLease(leaseID)},
	}
	if scope == "" || leaseID == "" || binding.Slug == "" {
		return core.LeaseClaim{}, binding, fmt.Errorf("missing exact Proxmox claim scope or lease identity")
	}
	var claim core.LeaseClaim
	count := 0
	for _, candidate := range claims {
		if candidate.LeaseID == leaseID {
			claim = candidate
		}
		if candidate.Provider == "proxmox" && candidate.CloudID == server.CloudID && (candidate.ProviderScope == scope || candidate.ProviderScope == "") {
			count++
		}
	}
	if count != 1 {
		return claim, binding, fmt.Errorf("expected one exact local claim for VM, found %d", count)
	}
	remoteCount := 0
	for _, candidate := range inventory {
		if candidate.CloudID == server.CloudID || proxmoxClaimLabelLeaseID(candidate) == leaseID {
			remoteCount++
		}
	}
	if remoteCount != 1 {
		return claim, binding, fmt.Errorf("ambiguous Proxmox VM or lease inventory")
	}
	if err := shared.ValidateClaimBinding(claim, binding); err != nil {
		return claim, binding, err
	}
	return claim, binding, validateCleanupServer(server, claim, binding)
}

func validateCleanupServer(server Server, claim core.LeaseClaim, binding shared.ClaimBinding) error {
	vmid, err := strconv.Atoi(server.CloudID)
	if err != nil || vmid <= 0 || strconv.Itoa(vmid) != server.CloudID || server.CloudID != binding.CloudID || server.Provider != "proxmox" || server.HostID == "" || !strings.HasPrefix(server.Name, "crabbox-") {
		return fmt.Errorf("Proxmox VM identity or node mismatch")
	}
	if server.ImmutableID == "" || server.ImmutableID != claim.CloudImmutableID {
		return fmt.Errorf("Proxmox VM generation identity missing or changed")
	}
	for key, expected := range map[string]string{
		"crabbox": "true", "provider": "proxmox", "lease": binding.LeaseID,
		"slug": binding.Slug, "provider_key": binding.RequiredLabels["provider_key"],
	} {
		if server.Labels[key] != expected {
			return fmt.Errorf("Proxmox VM label %s mismatch", key)
		}
	}
	return nil
}

func (Provider) PrepareLeaseClaimEndpoint(existing core.LeaseClaim, provider, slug string, server core.Server, _ bool) (core.Server, error) {
	if provider != "proxmox" || slug != existing.Slug || server.Labels["lease"] != existing.LeaseID || server.Labels["slug"] != existing.Slug {
		return core.Server{}, core.Exit(2, "refusing to rewrite Proxmox lease=%s with mismatched identity", existing.LeaseID)
	}
	if existing.CloudID != "" && server.CloudID != "" && existing.CloudID != server.CloudID {
		return core.Server{}, core.Exit(2, "refusing to rewrite Proxmox lease=%s with a different VMID", existing.LeaseID)
	}
	if existing.CloudImmutableID != "" && server.ImmutableID != "" && existing.CloudImmutableID != server.ImmutableID {
		return core.Server{}, core.Exit(2, "refusing to rewrite Proxmox lease=%s with a different VM generation", existing.LeaseID)
	}
	// An endpoint refresh cannot promote legacy inventory into cleanup authority.
	if existing.CloudImmutableID == "" {
		server.ImmutableID = ""
	}
	labels := shared.CloneLabels(server.Labels)
	for _, key := range []string{"provider", "provider_key", "crabbox"} {
		value := existing.Labels[key]
		if live := labels[key]; value != "" && live != "" && live != value {
			return core.Server{}, core.Exit(2, "refusing to rewrite Proxmox lease=%s with mismatched %s", existing.LeaseID, key)
		}
		if value == "" {
			delete(labels, key)
		} else {
			labels[key] = value
		}
	}
	server.Labels = labels
	return server, nil
}
