package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// BridgePeer is the cross-provider shape returned by `crabbox pond peers`.
// One row per pond member, regardless of which plane carries that member:
//
//   - Bidirectional Tailscale providers surface with Transport="tailnet" and
//     Endpoint=tailnet IPv4/FQDN. Outbound-only userspace integrations such as
//     Islo keep their dialable URL transport and report Tailscale in Note.
//   - SSH-lease providers (exe.dev / RunPod / Daytona / Sprites / Namespace /
//     Semaphore) surface with Transport="ssh" and Endpoint=ssh://host:port.
//   - Delegated-with-URL providers (Islo, E2B, Modal, Cloudflare, Railway,
//     Tensorlake) surface with Transport="url" and a per-port public URL.
//   - Blacksmith and any provider without a Crabbox bridge adapter surface
//     with Transport="none" and an honest Note so doctor reports the gap
//     instead of pretending the peer is reachable.
//
// BridgeState is retained for backward compatibility with
// https://github.com/openclaw/crabbox/pull/136's first shape: it carries
// "unsupported" / "unsupported-provider" for URL-transport peers whose
// per-provider adapter explicitly cannot bridge.
type BridgePeer struct {
	Slug     string `json:"slug"`
	LeaseID  string `json:"leaseID"`
	Provider string `json:"provider"`
	Pond     string `json:"pond"`
	// Transport is the *primary* (recommended) plane this peer reports —
	// derived from providerCapabilities(provider).Primary(). Kept for
	// backward compatibility with callers that want a single value.
	Transport string `json:"transport"`
	// Transports lists *every* plane this peer's provider supports (peer
	// mesh, native HTTPS, operator-routed SSH), in preference order. Callers
	// that need the full reachability surface (e.g. `pond connect` deciding
	// SSH-mesh eligibility regardless of whether the provider also has
	// Tailscale) read this list. Empty for providers with no networking
	// (e.g. Blacksmith).
	Transports  []string           `json:"transports,omitempty"`
	Endpoint    string             `json:"endpoint"`
	Labels      map[string]string  `json:"labels,omitempty"`
	Note        string             `json:"note,omitempty"`
	Targets     []BridgePeerTarget `json:"targets,omitempty"`
	BridgeState string             `json:"bridgeState,omitempty"`
}

// Transport classes used by the unified `pond peers` view and by the doctor
// reachability matrix. The five values cover every shape the resolver can
// emit; see bridgePeerFromClaim for the per-provider mapping.
const (
	TransportTailnet = "tailnet"
	TransportURL     = "url"
	TransportSSH     = "ssh"
	TransportNone    = "none"
	TransportPending = "pending"
)

var (
	ErrTailnetPeerUnavailable           = errors.New("tailnet peer unavailable")
	ErrTailnetPeerValidationUnavailable = errors.New("tailnet peer validation unavailable")
)

// BridgePeerTarget is a single externally reachable HTTPS endpoint published
// for a sandbox port. Different providers will populate it from different
// native primitives (islo shares, modal web endpoints, e2b previews, …); the
// shape stays uniform so client tooling can dial peers without knowing the
// provider.
type BridgePeerTarget struct {
	Port      int       `json:"port"`
	URL       string    `json:"url"`
	ShareID   string    `json:"shareID,omitempty"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
}

// BridgeProvider is the interface delegated-provider backends implement to
// surface peer endpoints to the pond bridge plane. A backend that does not
// implement BridgeProvider is treated as "metadata-only" — the pond label is
// still honored, peers are listed without targets, and `pond peers` reports
// the gap honestly instead of pretending to bridge.
//
// Implementations are responsible for:
//
//   - PublishPeer: idempotently turning a (sandbox, port) pair into a public
//     URL. Implementations may cache existing shares and return them rather
//     than minting new ones.
//   - ListPeerTargets: returning whatever public URLs are currently live for
//     a sandbox, without side effects.
//
// The PublishPeer flow is opt-in: it is only invoked when the user passes
// `--share-port` to `crabbox pond peers`. ListPeerTargets is always cheap and
// safe — it is what powers the doctor probe.
type BridgeProvider interface {
	PublishPeer(ctx context.Context, leaseID string, port int, ttl time.Duration) (BridgePeerTarget, error)
	ListPeerTargets(ctx context.Context, leaseID string) ([]BridgePeerTarget, error)
}

// TailnetPeerValidator lets dual-plane delegated providers revalidate a local
// tailnet claim before it is advertised as reachable.
type TailnetPeerValidator interface {
	ValidateTailnetPeer(ctx context.Context, leaseID string) (TailscaleMetadata, error)
}

// pondPeersFlags holds the parsed flags for `crabbox pond peers`. It is
// extracted so the command can be unit tested without touching the global
// flag set.
type pondPeersFlags struct {
	Pond      string
	Provider  string
	JSON      bool
	SharePort int
	ShareTTL  time.Duration
}

func (a App) pondPeers(ctx context.Context, args []string) error {
	fs := newFlagSet("pond peers", a.Stderr)
	flags := pondPeersFlags{ShareTTL: 24 * time.Hour}
	fs.StringVar(&flags.Pond, "pond", "", "pond label to resolve (required)")
	fs.StringVar(&flags.Provider, "provider", "", "restrict to a single provider (default: all delegated providers in the pond)")
	fs.BoolVar(&flags.JSON, "json", false, "emit machine-readable JSON")
	fs.IntVar(&flags.SharePort, "share-port", 0, "if set, publish a public URL for this port on each peer")
	fs.DurationVar(&flags.ShareTTL, "share-ttl", 24*time.Hour, "TTL for shares created with --share-port")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	pondName, err := requestedPondName(flags.Pond)
	if err != nil {
		return err
	}
	if pondName == "" {
		return exit(2, "--pond is required")
	}
	if flags.SharePort != 0 && (flags.SharePort < 1 || flags.SharePort > 65535) {
		return exit(2, "--share-port must be between 1 and 65535")
	}
	// Empty --provider means "every provider represented in the pond"; the
	// resolver fans out per provider and concatenates the result. Non-empty
	// preserves the original single-provider semantics so existing scripts and
	// the islo live-demo in https://github.com/openclaw/crabbox/pull/136 keep
	// working unchanged.
	provider := strings.TrimSpace(flags.Provider)
	peers, err := resolvePondPeers(ctx, runtimeForApp(a), pondName, provider, flags)
	if err != nil {
		return err
	}
	if flags.JSON {
		return json.NewEncoder(a.Stdout).Encode(pondPeersJSON{Members: peers})
	}
	renderBridgePeers(a.Stdout, peers)
	return nil
}

// pondRelease stops every lease in the named pond and removes claims for
// providers whose release destroys the resource. Providers that retain a
// reusable stopped resource keep their claims.
// It iterates across all providers represented in the pond — the caller does
// not need to pass --provider. Individual stop failures are logged as warnings
// and do not block the remaining peers; the function returns the first error
// encountered so callers can decide whether the release was clean.
func (a App) pondRelease(ctx context.Context, args []string) error {
	pond, err := requestedPondName(strings.Join(args, " "))
	if err != nil {
		return err
	}
	if pond == "" {
		return exit(2, "usage: crabbox pond release <name>")
	}
	claims, err := listLeaseClaims()
	if err != nil {
		return err
	}
	matches := filterClaimsForPond(claims, pond, "")
	if len(matches) == 0 {
		fmt.Fprintf(a.Stdout, "pond %q has no active leases\n", pond)
		return nil
	}
	fmt.Fprintf(a.Stderr, "releasing pond %q (%d lease(s))\n", pond, len(matches))
	var firstErr error
	for _, claim := range matches {
		cfg, cerr := loadConfig()
		if cerr != nil {
			if firstErr == nil {
				firstErr = cerr
			}
			fmt.Fprintf(a.Stderr, "warning: skip %s/%s: load config: %v\n", claim.Provider, claim.LeaseID, cerr)
			continue
		}
		cfg.Provider = claim.Provider
		var routeErr error
		if claim.Provider == "external" {
			routeErr = routeExternalLeaseClaim(&cfg, claim.LeaseID)
		} else {
			routeErr = autoRouteExternalLeaseForConfig(&cfg, claim.LeaseID)
		}
		if routeErr != nil {
			if firstErr == nil {
				firstErr = routeErr
			}
			fmt.Fprintf(a.Stderr, "warning: skip %s/%s: route lease: %v\n", claim.Provider, claim.LeaseID, routeErr)
			continue
		}
		backend, berr := loadBackend(cfg, runtimeForApp(a))
		if berr != nil {
			if firstErr == nil {
				firstErr = berr
			}
			fmt.Fprintf(a.Stderr, "warning: skip %s/%s: load backend: %v\n", claim.Provider, claim.LeaseID, berr)
			continue
		}
		if delegated, ok := backend.(DelegatedRunBackend); ok {
			if serr := delegated.Stop(ctx, StopRequest{Options: leaseOptionsFromConfig(cfg), ID: claim.LeaseID}); serr != nil {
				if firstErr == nil {
					firstErr = serr
				}
				fmt.Fprintf(a.Stderr, "warning: %s/%s stop failed: %v\n", claim.Provider, claim.LeaseID, serr)
			} else {
				removeLeaseClaim(claim.LeaseID)
				fmt.Fprintf(a.Stderr, "released %s/%s slug=%s\n", claim.Provider, claim.LeaseID, blank(claim.Slug, "-"))
			}
			continue
		}
		sshBackend, ok := backend.(SSHLeaseBackend)
		if !ok {
			fmt.Fprintf(a.Stderr, "warning: skip %s/%s: provider does not support stop\n", claim.Provider, claim.LeaseID)
			continue
		}
		lease, rerr := sshBackend.Resolve(ctx, ResolveRequest{Options: leaseOptionsFromConfig(cfg), ID: claim.LeaseID, ReleaseOnly: true})
		if rerr != nil {
			if backendCoordinator(backend) != nil {
				fmt.Fprintf(a.Stderr, "warning: could not inspect %s/%s before release: %v\n", claim.Provider, claim.LeaseID, rerr)
				lease = LeaseTarget{LeaseID: claim.LeaseID}
			} else {
				if firstErr == nil {
					firstErr = rerr
				}
				fmt.Fprintf(a.Stderr, "warning: %s/%s resolve failed: %v\n", claim.Provider, claim.LeaseID, rerr)
				continue
			}
		}
		if lerr := sshBackend.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: lease, Force: true}); lerr != nil {
			if firstErr == nil {
				firstErr = lerr
			}
			fmt.Fprintf(a.Stderr, "warning: %s/%s release failed: %v\n", claim.Provider, claim.LeaseID, lerr)
		} else {
			retained := finalizePondReleaseClaim(backend, lease, claim)
			fmt.Fprintf(a.Stderr, "released %s/%s slug=%s claim_retained=%t\n", claim.Provider, claim.LeaseID, blank(claim.Slug, "-"), retained)
		}
	}
	return firstErr
}

func finalizePondReleaseClaim(backend Backend, lease LeaseTarget, claim leaseClaim) bool {
	retainer, ok := backend.(ReleaseLeaseClaimRetainer)
	retained := ok && retainer.RetainLeaseClaimAfterRelease(lease)
	if !retained {
		removeLeaseClaim(claim.LeaseID)
	}
	return retained
}

// pondPeersJSON wraps the peer list so the JSON output matches the
// documented `{ "members": [...] }` shape. Callers that need a raw slice
// can decode either form — the wrapper carries no other fields.
type pondPeersJSON struct {
	Members []BridgePeer `json:"members"`
}

// resolvePondPeers builds the BridgePeer list for a pond. The resolver is
// split out so unit tests can swap in fakes for the provider backend and the
// claim store without going through the full kong/flag stack.
//
// When provider is non-empty the resolver behaves as in the original
// https://github.com/openclaw/crabbox/pull/136 design: a single backend is
// configured and every matching claim is fanned out against it. When provider
// is empty the resolver groups matching claims by provider, configures each
// backend exactly once, and concatenates the results — this is the path that
// gives `crabbox pond peers --pond <name>` honest cross-provider output without
// making the caller enumerate providers by hand.
func resolvePondPeers(ctx context.Context, rt Runtime, pond, provider string, flags pondPeersFlags) ([]BridgePeer, error) {
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	matches := filterClaimsForPond(claims, pond, provider)
	if len(matches) == 0 {
		return []BridgePeer{}, nil
	}
	// Bucket claims by provider so each backend is configured at most once,
	// even when the same provider has several leases in the pond.
	byProvider := make(map[string][]leaseClaim)
	order := make([]string, 0, 4)
	for _, claim := range matches {
		key := strings.TrimSpace(claim.Provider)
		if _, ok := byProvider[key]; !ok {
			order = append(order, key)
		}
		byProvider[key] = append(byProvider[key], claim)
	}
	sort.Strings(order)
	peers := make([]BridgePeer, 0, len(matches))
	allProviders := strings.TrimSpace(provider) == ""
	var firstErr error
	successes := 0
	for _, p := range order {
		providerPeers, err := resolvePondPeersForProvider(ctx, rt, p, byProvider[p], flags)
		if err != nil {
			err = fmt.Errorf("%s: %w", p, err)
			if !allProviders {
				return nil, err
			}
			peers = append(peers, providerPeers...)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		successes++
		peers = append(peers, providerPeers...)
	}
	if successes == 0 && firstErr != nil {
		return nil, firstErr
	}
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].Slug == peers[j].Slug {
			return peers[i].LeaseID < peers[j].LeaseID
		}
		return peers[i].Slug < peers[j].Slug
	})
	return peers, nil
}

// resolvePondPeersForProvider configures one provider backend and fans the
// per-provider claim list through it. The caller is responsible for stitching
// the per-provider slices together into the final, slug-sorted view returned
// to the user.
//
// The transport class is determined per-provider:
//
//   - tailnet — bidirectional providers that recorded a tailnet endpoint on
//     their claim. The resolver does not invoke a bridge backend for tailnet
//     endpoints; the endpoint is read straight off the claim sidecar
//     (TailscaleIPv4 / TailscaleFQDN), and missing endpoints surface as
//     transport=pending with an honest note.
//   - ssh — SSH-lease providers (exe.dev / RunPod / Daytona / Sprites /
//     Namespace / Semaphore). Endpoint is built from SSHHost+SSHPort; an
//     unset host surfaces as transport=pending.
//   - url — delegated-with-URL providers. The per-provider BridgeProvider is
//     invoked for live target discovery, preserving the original
//     https://github.com/openclaw/crabbox/pull/136 behavior
//     (publish/list/honest-unsupported).
//   - none — Blacksmith and providers with no Crabbox bridge adapter.
//     Surfaced with a documented note so doctor stays honest.
func resolvePondPeersForProvider(ctx context.Context, rt Runtime, provider string, claims []leaseClaim, flags pondPeersFlags) ([]BridgePeer, error) {
	class := providerTransportClass(provider)
	peers := make([]BridgePeer, 0, len(claims))
	var deferredBridgeErr error
	var bridge BridgeProvider
	bridgeLoaded := false
	var bridgeLoadErr error
	for _, claim := range claims {
		peer := bridgePeerFromClaim(claim, class)
		caps := providerCapabilities(claim.Provider)
		urlCapable := caps.URLBridge
		useBridge := peer.Transport == TransportURL || urlCapable
		if useBridge {
			if !bridgeLoaded {
				bridgeLoaded = true
				bridge, bridgeLoadErr = loadBridgeProvider(provider, rt)
			}
			if (caps.Tailscale || caps.TailscaleEgress) && claimHasTailscaleMetadata(claim) && bridgeLoadErr == nil {
				if validator, ok := bridge.(TailnetPeerValidator); ok {
					meta, validateErr := validator.ValidateTailnetPeer(ctx, claim.LeaseID)
					if validateErr != nil {
						if !errors.Is(validateErr, ErrTailnetPeerValidationUnavailable) {
							clearLeaseClaimTailscaleFields(&claim)
						}
						peer = bridgePeerFromClaim(claim, class)
						peer.Note = fmt.Sprintf("tailnet validation failed: %v", validateErr)
					} else {
						setLeaseClaimTailscale(&claim, meta.IPv4, meta.FQDN)
						peer = bridgePeerFromClaim(claim, class)
						if peer.Transport == TransportTailnet {
							peer.Endpoint = firstNonEmpty(meta.IPv4, meta.FQDN, meta.Hostname)
						}
					}
				}
			}
			urlPrimary := peer.Transport == TransportURL
			secondaryRead := !urlPrimary && flags.SharePort == 0
			if bridgeLoadErr != nil {
				if secondaryRead {
					if peer.Transport == TransportTailnet {
						peer.Note = fmt.Sprintf("tailnet validation unavailable: %v", bridgeLoadErr)
					}
					peers = append(peers, peer)
					continue
				}
				if flags.SharePort == 0 {
					peer.BridgeState = "error"
					peer.Transport = TransportNone
					peer.Note = fmt.Sprintf("bridge lookup failed for provider %s: %v", peer.Provider, bridgeLoadErr)
					peers = append(peers, peer)
					deferredBridgeErr = bridgeLoadErr
					continue
				}
				return nil, bridgeLoadErr
			}
			// We invoke the bridge backend when the caller asked us to mint
			// a share (--share-port) or when no canonical endpoint is yet
			// recorded on the claim. Skipping the lookup when an endpoint
			// is already known keeps `pond peers` cheap for read-only
			// listings on already-published shares.
			needBridge := flags.SharePort > 0 || (urlPrimary && peer.Endpoint == "") || (!urlPrimary && urlCapable)
			if bridge == nil && needBridge {
				if secondaryRead {
					peers = append(peers, peer)
					continue
				}
				peer.BridgeState = "unsupported-provider"
				if urlPrimary {
					peer.Transport = TransportNone
				}
				peer.Note = fmt.Sprintf("no bridge adapter for provider %s", peer.Provider)
				peers = append(peers, peer)
				continue
			}
			if needBridge {
				if flags.SharePort > 0 {
					target, perr := bridge.PublishPeer(ctx, claim.LeaseID, flags.SharePort, flags.ShareTTL)
					if perr != nil {
						if errors.Is(perr, ErrBridgeNotImplemented) {
							peer.BridgeState = "unsupported"
							if urlPrimary {
								peer.Transport = TransportNone
							}
							peer.Note = fmt.Sprintf("bridge adapter for provider %s reports unsupported", peer.Provider)
							peers = append(peers, peer)
							continue
						}
						return nil, fmt.Errorf("publish peer %s port=%d: %w", claim.LeaseID, flags.SharePort, perr)
					}
					peer.Targets = append(peer.Targets, target)
					if peer.Endpoint == "" {
						peer.Endpoint = target.URL
					}
				} else {
					targets, lerr := bridge.ListPeerTargets(ctx, claim.LeaseID)
					if lerr != nil {
						if secondaryRead {
							peer.BridgeState = "error"
							peer.Note = fmt.Sprintf("list secondary bridge targets failed: %v", lerr)
							peers = append(peers, peer)
							continue
						}
						if errors.Is(lerr, ErrBridgeNotImplemented) {
							peer.BridgeState = "unsupported"
							if urlPrimary {
								peer.Transport = TransportNone
							}
							peer.Note = fmt.Sprintf("bridge adapter for provider %s reports unsupported", peer.Provider)
							peers = append(peers, peer)
							continue
						}
						if flags.SharePort == 0 {
							peer.BridgeState = "error"
							peer.Transport = TransportNone
							peer.Note = fmt.Sprintf("list bridge targets failed: %v", lerr)
							peers = append(peers, peer)
							deferredBridgeErr = fmt.Errorf("list peer targets %s: %w", claim.LeaseID, lerr)
							continue
						}
						return nil, fmt.Errorf("list peer targets %s: %w", claim.LeaseID, lerr)
					}
					peer.Targets = targets
					if peer.Endpoint == "" && len(targets) > 0 {
						peer.Endpoint = targets[0].URL
					}
				}
			}
		}
		peers = append(peers, peer)
	}
	if deferredBridgeErr != nil && !hasResolvedPrimary(peers) {
		return peers, deferredBridgeErr
	}
	return peers, nil
}

func hasResolvedPrimary(peers []BridgePeer) bool {
	for _, peer := range peers {
		if peer.Transport == TransportTailnet || peer.Transport == TransportSSH {
			return true
		}
		if peer.Transport == TransportURL && (peer.Endpoint != "" || len(peer.Targets) > 0) {
			return true
		}
	}
	return false
}

// bridgePeerFromClaim turns a single lease claim into the unified peer row.
// The transport class is provided by the caller (it has already classified
// the provider once for the fan-out path); the rest of the row is filled in
// from the claim sidecar without any provider API calls.
func bridgePeerFromClaim(claim leaseClaim, class string) BridgePeer {
	caps := providerCapabilities(claim.Provider)
	peer := BridgePeer{
		Slug:       claim.Slug,
		LeaseID:    claim.LeaseID,
		Provider:   claim.Provider,
		Pond:       claim.Pond,
		Labels:     cloneStringMap(claim.Labels),
		Transports: caps.Available(),
	}
	if caps.TailscaleEgress && claimHasTailscaleMetadata(claim) {
		peer.Note = "tailnet available for outbound proxy traffic only"
	}
	switch class {
	case TransportTailnet:
		endpoint := firstNonEmpty(claim.TailscaleIPv4, claim.TailscaleFQDN)
		if endpoint == "" {
			// The provider is tailnet-primary but this lease has no tailnet
			// endpoint recorded. For providers that ALSO advertise the URL
			// bridge (e.g. islo, which only records a tailnet IP when the lease
			// was warmed with --tailscale), fall back to the bridge plane
			// instead of stranding the member as "pending".
			if providerCapabilities(claim.Provider).URLBridge {
				peer.Transport = TransportURL
				peer.Endpoint = claim.BridgeURL
				return peer
			}
			peer.Transport = TransportPending
			peer.Note = "tailnet endpoint not yet recorded for this lease"
			return peer
		}
		peer.Transport = TransportTailnet
		peer.Endpoint = endpoint
	case TransportSSH:
		if claim.SSHHost == "" {
			peer.Transport = TransportPending
			peer.Note = "ssh endpoint not yet recorded for this lease"
			return peer
		}
		if claim.SSHPort == 0 {
			peer.Transport = TransportPending
			peer.Note = "ssh port not yet recorded for this lease"
			return peer
		}
		peer.Transport = TransportSSH
		peer.Endpoint = fmt.Sprintf("ssh://%s:%d", claim.SSHHost, claim.SSHPort)
	case TransportURL:
		peer.Transport = TransportURL
		peer.Endpoint = claim.BridgeURL
	case TransportNone:
		peer.Transport = TransportNone
		if isBlacksmithProvider(claim.Provider) {
			peer.Note = "blacksmith owns connectivity"
		} else {
			peer.Note = fmt.Sprintf("no advertised pond transport for provider %s", claim.Provider)
		}
	default:
		peer.Transport = TransportNone
		peer.Note = fmt.Sprintf("no advertised pond transport for provider %s", claim.Provider)
	}
	return peer
}

// providerTransportClass returns the *primary* (recommended) transport for a
// provider. It used to hardcode a 1:1 mapping; that's now derived from the
// provider's capability set so a single provider can advertise multiple
// planes (Hetzner has both Tailscale and SSH-mesh) and the recommended one is
// picked deterministically.
//
// Most call sites in `pond peers` and the doctor reachability matrix still
// want one value per peer for single-valued reporting; those keep using this
// function. The full set of available transports is exposed via
// `providerCapabilities(p).Available()` and surfaced on
// `BridgePeer.Transports` so callers that want the multi-transport view
// (e.g. `pond connect` deciding which members it can SSH into regardless of
// whether they ALSO support Tailscale) read the capabilities directly.
func providerTransportClass(provider string) string {
	return providerCapabilities(provider).Primary()
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// filterClaimsForPond returns the subset of claims that belong to the named
// pond and (when provider is non-empty) the named provider. Empty pond
// returns no matches — ponds are never implicit.
func filterClaimsForPond(claims []leaseClaim, pond, provider string) []leaseClaim {
	pond = normalizePondName(pond)
	if pond == "" {
		return nil
	}
	provider = strings.TrimSpace(provider)
	canonProvider := canonicalClaimProvider(provider)
	out := make([]leaseClaim, 0, len(claims))
	for _, claim := range claims {
		if normalizePondName(claim.Pond) != pond {
			continue
		}
		if provider != "" && claim.Provider != canonProvider {
			continue
		}
		out = append(out, claim)
	}
	return out
}

// loadBridgeProviderFunc is the factory used by resolvePondPeers; it is a
// package var so unit tests can inject a fake bridge without going through
// provider registration. Production code lets it default to the real lookup.
var loadBridgeProviderFunc = realLoadBridgeProvider

func loadBridgeProvider(provider string, rt Runtime) (BridgeProvider, error) {
	return loadBridgeProviderFunc(provider, rt)
}

func realLoadBridgeProvider(provider string, rt Runtime) (BridgeProvider, error) {
	if strings.TrimSpace(provider) == "" {
		return nil, nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	cfg.Provider = provider
	resolved, err := ProviderFor(provider)
	if err != nil {
		return nil, exit(2, "unknown provider %q for pond bridge", provider)
	}
	backend, err := configureProviderBackend(resolved, &cfg, rt)
	if err != nil {
		return nil, err
	}
	bridge, ok := backend.(BridgeProvider)
	if !ok {
		// Backends without bridge support still expose metadata; surface a
		// stable error so callers (and the JSON consumer) can detect that
		// peer URLs are not available for this provider.
		return nil, nil
	}
	return bridge, nil
}

// ErrBridgeNotImplemented is returned by helpers that need an explicit signal
// that the requested provider does not implement BridgeProvider yet.
var ErrBridgeNotImplemented = errors.New("pond bridge plane not implemented for this provider")

func renderBridgePeers(w interface{ Write([]byte) (int, error) }, peers []BridgePeer) {
	if len(peers) == 0 {
		fmt.Fprintln(w, "no peers found")
		return
	}
	for _, peer := range peers {
		fmt.Fprintf(w, "%s\tlease=%s\tprovider=%s\tpond=%s\ttransport=%s", peer.Slug, peer.LeaseID, peer.Provider, peer.Pond, peer.Transport)
		if peer.Endpoint != "" {
			fmt.Fprintf(w, "\tendpoint=%s", peer.Endpoint)
		}
		if peer.BridgeState != "" {
			fmt.Fprintf(w, "\tbridge=%s", peer.BridgeState)
		}
		if peer.Note != "" {
			fmt.Fprintf(w, "\tnote=%q", peer.Note)
		}
		for _, target := range peer.Targets {
			fmt.Fprintf(w, "\n  bridge target port=%d url=%s", target.Port, target.URL)
			if target.ShareID != "" {
				fmt.Fprintf(w, " share=%s", target.ShareID)
			}
			if !target.ExpiresAt.IsZero() {
				fmt.Fprintf(w, " expires=%s", target.ExpiresAt.Format(time.RFC3339))
			}
		}
		fmt.Fprintln(w)
	}
}
