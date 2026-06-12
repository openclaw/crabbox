package islo

import (
	"context"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const isloShareReuseSkew = 30 * time.Second

// PublishPeer implements core.BridgeProvider. It is idempotent — if a share
// for the requested port already exists, the existing share is returned
// rather than minting another one. This matters because every call to the
// islo create-share endpoint counts as a published URL on the user's tenant.
func (b *isloBackend) PublishPeer(ctx context.Context, leaseID string, port int, ttl time.Duration) (core.BridgePeerTarget, error) {
	if port <= 0 || port > 65535 {
		return core.BridgePeerTarget{}, exit(2, "islo bridge: port %d out of range", port)
	}
	name, err := isloSandboxNameFromLeaseID(leaseID)
	if err != nil {
		return core.BridgePeerTarget{}, err
	}
	client, err := newIsloClient(b.cfg, b.rt)
	if err != nil {
		return core.BridgePeerTarget{}, err
	}
	existing, err := client.ListShares(ctx, name)
	if err != nil {
		return core.BridgePeerTarget{}, err
	}
	now := b.now()
	for _, share := range existing {
		if isloShareReusable(share, port, now) {
			return bridgeTargetFromShare(share), nil
		}
	}
	share, err := client.CreateShare(ctx, name, port, ttl)
	if err != nil {
		return core.BridgePeerTarget{}, err
	}
	return bridgeTargetFromShare(share), nil
}

func isloShareReusable(share IsloShare, port int, now time.Time) bool {
	if share.Port != port || share.URL == "" {
		return false
	}
	if share.ExpiresAt.IsZero() {
		return !share.ExpiresAtSet
	}
	return share.ExpiresAt.After(now.Add(isloShareReuseSkew))
}

// ListPeerTargets implements core.BridgeProvider. It is side-effect free —
// the doctor probe and the no-flag `pond peers` view both call it.
func (b *isloBackend) ListPeerTargets(ctx context.Context, leaseID string) ([]core.BridgePeerTarget, error) {
	name, err := isloSandboxNameFromLeaseID(leaseID)
	if err != nil {
		return nil, err
	}
	client, err := newIsloClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	shares, err := client.ListShares(ctx, name)
	if err != nil {
		return nil, err
	}
	out := make([]core.BridgePeerTarget, 0, len(shares))
	for _, share := range shares {
		if share.URL == "" {
			continue
		}
		out = append(out, bridgeTargetFromShare(share))
	}
	return out, nil
}

func bridgeTargetFromShare(share IsloShare) core.BridgePeerTarget {
	return core.BridgePeerTarget{
		Port:      share.Port,
		URL:       share.URL,
		ShareID:   share.ShareID,
		ExpiresAt: share.ExpiresAt,
	}
}

// isloSandboxNameFromLeaseID accepts either the `isb_<sandbox>` lease id used
// by the rest of the islo backend or a bare sandbox name and returns the
// canonical sandbox name. Anything else is rejected up front so the bridge
// plane does not silently make share-API calls against an unrelated tenant
// resource.
func isloSandboxNameFromLeaseID(leaseID string) (string, error) {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return "", exit(2, "islo bridge: missing lease id")
	}
	if strings.HasPrefix(leaseID, isloLeasePrefix) {
		name := strings.TrimPrefix(leaseID, isloLeasePrefix)
		if !isCrabboxIsloSandboxName(name) {
			return "", exit(2, "islo bridge: lease %q is not a Crabbox-owned sandbox", leaseID)
		}
		return name, nil
	}
	if isCrabboxIsloSandboxName(leaseID) {
		return leaseID, nil
	}
	return "", exit(2, "islo bridge: lease %q is not a Crabbox-owned islo sandbox", leaseID)
}
