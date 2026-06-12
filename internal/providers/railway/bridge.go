package railway

import (
	"context"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Railway exposes a single public HTTPS endpoint per service deployment via
// `railwayDeployment.URL`. That URL is already populated by the existing
// `LatestDeployment` query the rest of the backend uses for `Status` and
// `Stop`, so the bridge adapter is a thin read on top of fields the provider
// already surfaces — no new lease record state is introduced.
//
// Two consequences flow from Railway's "one URL per service" model:
//
//   - PublishPeer cannot honor an arbitrary port. The Railway service binds
//     a single port internally; the external URL is fixed to whatever
//     Railway routed in front of it. The adapter accepts the port flag for
//     interface parity, surfaces it on the target for the JSON consumer, and
//     uses the deployment URL verbatim. Callers asking for a port that is
//     not the service port get an HTTPS URL that simply will not resolve at
//     the layer-4 level — which is honest behaviour.
//   - ListPeerTargets returns the deployment URL whenever one is live; an
//     empty slice when the service has no deployment yet (a sleeping service
//     is not a bridge target).

// PublishPeer implements core.BridgeProvider for Railway by surfacing the
// latest deployment URL for the lease's service id.
func (b *railwayBackend) PublishPeer(ctx context.Context, leaseID string, port int, ttl time.Duration) (core.BridgePeerTarget, error) {
	_ = ttl
	if port <= 0 || port > 65535 {
		return core.BridgePeerTarget{}, exit(2, "railway bridge: port %d out of range", port)
	}
	url, status, err := b.bridgeDeploymentURL(ctx, leaseID)
	if err != nil {
		return core.BridgePeerTarget{}, err
	}
	if url == "" {
		statusText := string(status.Normalized())
		if statusText == "" {
			statusText = "unknown"
		}
		return core.BridgePeerTarget{}, exit(4, "railway bridge: deployment for %q is not ready (status=%s)", leaseID, statusText)
	}
	return core.BridgePeerTarget{Port: port, URL: url}, nil
}

// ListPeerTargets implements core.BridgeProvider for Railway. The slice is
// empty when the service has no live deployment, which keeps the doctor probe
// honest instead of pretending a sleeping service is bridge-reachable.
func (b *railwayBackend) ListPeerTargets(ctx context.Context, leaseID string) ([]core.BridgePeerTarget, error) {
	url, _, err := b.bridgeDeploymentURL(ctx, leaseID)
	if err != nil {
		return nil, err
	}
	if url == "" {
		return nil, nil
	}
	return []core.BridgePeerTarget{{URL: url}}, nil
}

// bridgeDeploymentURL resolves a Railway lease (the service id, by Railway's
// convention) to its current ready deployment URL. Returns an empty string
// when the service has no live deployment — callers translate that to "no
// targets" or a not-ready publish error as appropriate.
func (b *railwayBackend) bridgeDeploymentURL(ctx context.Context, leaseID string) (string, railwayDeploymentStatus, error) {
	serviceID := strings.TrimSpace(leaseID)
	if serviceID == "" {
		return "", "", exit(2, "railway bridge: missing lease id")
	}
	projectID, environmentID, err := b.requireProjectEnv()
	if err != nil {
		return "", "", err
	}
	client, err := b.api()
	if err != nil {
		return "", "", err
	}
	deployment, err := client.LatestDeployment(ctx, projectID, environmentID, serviceID)
	if err != nil {
		return "", "", err
	}
	if !deployment.Status.IsReady() {
		return "", deployment.Status, nil
	}
	return strings.TrimSpace(deployment.URL), deployment.Status, nil
}
