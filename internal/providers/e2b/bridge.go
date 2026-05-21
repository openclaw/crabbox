package e2b

import (
	"context"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// E2B's bridge plane is built around the native per-sandbox preview URL
// convention `https://<port>-<sandboxID>.<domain>`. The domain is read from
// the provider config (set by `--e2b-domain` or E2B_DOMAIN, defaulting to
// `e2b.app`) — exactly the same source the rest of the e2b backend already
// uses for `envdURL`, so no new fields are introduced on the lease record.
//
// Listing peer targets without a port hint is impossible against E2B's API
// (there is no public "list active previews" endpoint), so ListPeerTargets is
// intentionally an empty success: the caller learns that the peer exists but
// has no published-by-name preview yet. PublishPeer, on the other hand, is a
// deterministic URL builder: ask for port N, get the canonical URL back. The
// E2B preview is always live as long as the sandbox is running, so the URL is
// usable immediately — no idempotent "create share" round-trip needed.

// PublishPeer implements core.BridgeProvider for E2B. It deterministically
// synthesizes the preview URL for the requested port; no API call is made,
// matching the way E2B itself documents per-port ingress.
func (b *e2bBackend) PublishPeer(ctx context.Context, leaseID string, port int, ttl time.Duration) (core.BridgePeerTarget, error) {
	_ = ctx
	_ = ttl
	if port <= 0 || port > 65535 {
		return core.BridgePeerTarget{}, exit(2, "e2b bridge: port %d out of range", port)
	}
	sandboxID, domain, err := b.bridgeSandboxCoords(leaseID)
	if err != nil {
		return core.BridgePeerTarget{}, err
	}
	return core.BridgePeerTarget{
		Port: port,
		URL:  e2bPreviewURL(domain, sandboxID, port),
	}, nil
}

// ListPeerTargets implements core.BridgeProvider for E2B. Because E2B does
// not expose a "list active previews" endpoint, the side-effect-free view is
// empty — callers learn the peer exists but no targets are surfaced until
// `--share-port` is passed.
func (b *e2bBackend) ListPeerTargets(ctx context.Context, leaseID string) ([]core.BridgePeerTarget, error) {
	_ = ctx
	if _, _, err := b.bridgeSandboxCoords(leaseID); err != nil {
		return nil, err
	}
	return nil, nil
}

// bridgeSandboxCoords returns the (sandboxID, domain) pair the URL builder
// needs, validating that the lease id is a Crabbox-managed E2B lease before
// returning anything. The domain falls back to the same default used by the
// rest of the backend so the bridge plane stays consistent with `envdURL`.
func (b *e2bBackend) bridgeSandboxCoords(leaseID string) (string, string, error) {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return "", "", exit(2, "e2b bridge: missing lease id")
	}
	if !strings.HasPrefix(leaseID, "cbx_") && !strings.HasPrefix(leaseID, "e2b_") {
		return "", "", exit(2, "e2b bridge: lease %q is not an E2B-claimed Crabbox lease", leaseID)
	}
	client, err := newE2BClient(b.cfg, b.rt)
	if err != nil {
		return "", "", err
	}
	sandbox, err := resolveE2BSandboxByLease(context.Background(), client, leaseID)
	if err != nil {
		return "", "", err
	}
	if sandbox.SandboxID == "" {
		return "", "", exit(4, "e2b bridge: no sandbox bound to lease %q", leaseID)
	}
	domain := strings.TrimSpace(sandbox.Domain)
	if domain == "" {
		domain = strings.TrimSpace(blank(b.cfg.E2B.Domain, "e2b.app"))
	}
	return sandbox.SandboxID, domain, nil
}

// e2bPreviewURL builds the canonical per-port preview URL. Kept package-local
// (and exported only via PublishPeer) so the URL convention has a single
// source of truth — if E2B ever rotates the scheme, only this builder
// changes.
func e2bPreviewURL(domain, sandboxID string, port int) string {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		domain = "e2b.app"
	}
	return "https://" + strconv.Itoa(port) + "-" + sandboxID + "." + domain
}
