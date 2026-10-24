package proxmox

import (
	"context"
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

var readTailnetMetadata = core.ReadRemoteTailscaleMetadata

func proxmoxNetworkTarget(cfg core.Config, server core.Server) core.SSHTarget {
	target := core.SSHTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if server.Labels["tailscale"] == "true" {
		target.AuthoritativeNetwork = true
		target.NetworkKind = core.NetworkTailscale
		target.Port = "22"
		target.FallbackPorts = nil
	}
	return target
}

func validateTailnetMetadata(server core.Server, meta core.TailscaleMetadata, expectedID string) error {
	if !meta.Enabled || meta.State != "ready" || meta.IPv4 != server.PublicNet.IPv4.IP || strings.TrimSpace(meta.DeviceID) == "" || strings.ContainsAny(meta.DeviceID, "\x00\r\n\t ") || expectedID != "" && meta.DeviceID != expectedID {
		return core.Exit(4, "lease_id_conflict: Proxmox enrollment receipt does not match the current Tailnet address and bound device identity")
	}
	return nil
}

func (b *leaseBackend) TailscaleMetadataRequired(lease core.LeaseTarget) bool {
	return b.Cfg.Tailscale.Enabled || lease.Server.Labels["tailscale"] == "true" ||
		lease.SSH.AuthoritativeNetwork && lease.SSH.NetworkKind == core.NetworkTailscale
}

// UpdateTailscaleMetadata never accepts routing or device ownership from mutable
// descriptions. The first receipt was bound inside PrepareAccess; refresh may
// only confirm it against the same canonical lease and live VM generation.
func (b *leaseBackend) UpdateTailscaleMetadata(ctx context.Context, lease core.LeaseTarget, meta core.TailscaleMetadata) (core.Server, error) {
	claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil {
		return core.Server{}, err
	}
	if !exists || !fixedProxmoxLeaseKind.IsFixedClaim(claim) || claim.Labels["tailscale"] != "true" || claim.ProviderScope != core.ProviderClaimScope("proxmox", b.Cfg) || claim.CloudImmutableID == "" || claim.FixedCreateIntent.State != "acquired" {
		return core.Server{}, core.Exit(4, "lease_id_conflict: Proxmox metadata requires an active generation-bound Tailnet claim")
	}
	vmid, node, err := fixedProxmoxAttempt(claim)
	if err != nil {
		return core.Server{}, err
	}
	if err := validateFixedProxmoxServer(lease.Server, claim, vmid, node); err != nil {
		return core.Server{}, err
	}
	client, err := newClient(b.Cfg)
	if err != nil {
		return core.Server{}, err
	}
	live, err := client.GetServerOnNode(ctx, lease.Server.HostID, lease.Server.CloudID)
	if err != nil {
		return core.Server{}, err
	}
	if err := b.validateTailnetBinding(&live); err != nil {
		return core.Server{}, err
	}
	if err := validateFixedProxmoxServer(live, claim, vmid, node); err != nil {
		return core.Server{}, err
	}
	if err := ValidateProxmoxTailnetAccess(b.Cfg, live); err != nil {
		return core.Server{}, err
	}
	if err := validateTailnetMetadata(live, meta, claim.Labels["tailscale_device_id"]); err != nil {
		return core.Server{}, err
	}
	target := proxmoxNetworkTarget(b.Cfg, live)
	updated, err := core.UpdateLeaseClaimEndpointIfUnchanged(lease.LeaseID, claim, live, target)
	if err != nil {
		return core.Server{}, fmt.Errorf("persist Proxmox enrollment receipt: %w", err)
	}
	core.SetServerLeaseClaimSnapshot(&live, updated, true)
	return live, nil
}

var _ core.RequiredTailscaleMetadataBackend = (*leaseBackend)(nil)
