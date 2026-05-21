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

// BridgePeer is the cross-provider shape returned by `crabbox crew peers`.
// One row per crew member, regardless of which plane carries that member:
//
//   - Managed-Linux providers (AWS / Azure / GCP / Hetzner / Proxmox / SSH)
//     surface with Transport="tailnet" and Endpoint=tailnet IPv4.
//   - SSH-lease providers (exe.dev / RunPod / Daytona / Sprites / Namespace /
//     Semaphore) surface with Transport="ssh" and Endpoint=ssh://host:port.
//   - Delegated-with-URL providers (Islo, E2B, Modal, Cloudflare, Railway,
//     Tensorlake) surface with Transport="url" and a per-port public URL.
//   - Blacksmith and any provider without a Crabbox bridge adapter surface
//     with Transport="none" and an honest Note so doctor reports the gap
//     instead of pretending the peer is reachable.
//
// BridgeState is retained for backward compatibility with PR #136's first
// shape: it carries "unsupported" / "unsupported-provider" for URL-transport
// peers whose per-provider adapter explicitly cannot bridge.
type BridgePeer struct {
	Slug        string             `json:"slug"`
	LeaseID     string             `json:"leaseID"`
	Provider    string             `json:"provider"`
	Crew        string             `json:"crew"`
	Transport   string             `json:"transport"`
	Endpoint    string             `json:"endpoint"`
	Labels      map[string]string  `json:"labels,omitempty"`
	Note        string             `json:"note,omitempty"`
	Targets     []BridgePeerTarget `json:"targets,omitempty"`
	BridgeState string             `json:"bridgeState,omitempty"`
}

// Transport classes used by the unified `crew peers` view and by the doctor
// reachability matrix. The five values cover every shape the resolver can
// emit; see bridgePeerFromClaim for the per-provider mapping.
const (
	TransportTailnet = "tailnet"
	TransportURL     = "url"
	TransportSSH     = "ssh"
	TransportNone    = "none"
	TransportPending = "pending"
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
// surface peer endpoints to the crew bridge plane. A backend that does not
// implement BridgeProvider is treated as "metadata-only" — the crew label is
// still honored, peers are listed without targets, and `crew peers` reports
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
// `--share-port` to `crabbox crew peers`. ListPeerTargets is always cheap and
// safe — it is what powers the doctor probe.
type BridgeProvider interface {
	PublishPeer(ctx context.Context, leaseID string, port int, ttl time.Duration) (BridgePeerTarget, error)
	ListPeerTargets(ctx context.Context, leaseID string) ([]BridgePeerTarget, error)
}

// crewPeersFlags holds the parsed flags for `crabbox crew peers`. It is
// extracted so the command can be unit tested without touching the global
// flag set.
type crewPeersFlags struct {
	Crew      string
	Provider  string
	JSON      bool
	SharePort int
	ShareTTL  time.Duration
}

func (a App) crew(ctx context.Context, args []string) error {
	if len(args) == 0 {
		a.crewHelp()
		return exit(2, "missing crew subcommand")
	}
	switch args[0] {
	case "peers":
		return a.crewPeers(ctx, args[1:])
	case "-h", "--help", "help":
		a.crewHelp()
		return nil
	default:
		a.crewHelp()
		return exit(2, "unknown crew subcommand %q", args[0])
	}
}

func (a App) crewHelp() {
	fmt.Fprintln(a.Stdout, `Crew bridge plane — list peer endpoints for delegated providers.

Usage:
  crabbox crew peers --crew <name> [flags]

Flags:
  --crew <name>          Required. Crew label to resolve.
  --provider <name>      Restrict to a single provider (default: all delegated
                         providers in the crew).
  --json                 Emit machine-readable JSON instead of text.
  --share-port <port>    Publish a per-peer public URL for this port (idempotent).
  --share-ttl <duration> TTL for shares created with --share-port (default 24h).

Examples:
  crabbox crew peers --crew alpha
  crabbox crew peers --crew alpha --provider islo
  crabbox crew peers --crew alpha --json
  crabbox crew peers --crew alpha --share-port 8080 --json

The bridge plane is HTTP-only by design: peers are reachable via the per-
provider native ingress (islo shares, e2b sandbox previews, railway deploy
URLs, …). Non-HTTP protocols are out of scope — use the Tailscale plane for
arbitrary TCP/UDP. Providers that do not expose a per-sandbox HTTPS ingress
(modal, cloudflare, tensorlake) are honestly reported as unsupported instead
of pretending to bridge.`)
}

func (a App) crewPeers(ctx context.Context, args []string) error {
	fs := newFlagSet("crew peers", a.Stderr)
	flags := crewPeersFlags{ShareTTL: 24 * time.Hour}
	fs.StringVar(&flags.Crew, "crew", "", "crew label to resolve (required)")
	fs.StringVar(&flags.Provider, "provider", "", "restrict to a single provider (default: all delegated providers in the crew)")
	fs.BoolVar(&flags.JSON, "json", false, "emit machine-readable JSON")
	fs.IntVar(&flags.SharePort, "share-port", 0, "if set, publish a public URL for this port on each peer")
	fs.DurationVar(&flags.ShareTTL, "share-ttl", 24*time.Hour, "TTL for shares created with --share-port")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	crewName, err := requestedCrewName(flags.Crew)
	if err != nil {
		return err
	}
	if crewName == "" {
		return exit(2, "--crew is required")
	}
	if flags.SharePort != 0 && (flags.SharePort < 1 || flags.SharePort > 65535) {
		return exit(2, "--share-port must be between 1 and 65535")
	}
	// Empty --provider means "every provider represented in the crew"; the
	// resolver fans out per provider and concatenates the result. Non-empty
	// preserves the original single-provider semantics so existing scripts
	// (and the islo live-demo in PR #136) keep working unchanged.
	provider := strings.TrimSpace(flags.Provider)
	peers, err := resolveCrewPeers(ctx, runtimeForApp(a), crewName, provider, flags)
	if err != nil {
		return err
	}
	if flags.JSON {
		return json.NewEncoder(a.Stdout).Encode(crewPeersJSON{Members: peers})
	}
	renderBridgePeers(a.Stdout, peers)
	return nil
}

// crewPeersJSON wraps the peer list so the JSON output matches the
// documented `{ "members": [...] }` shape. Callers that need a raw slice
// can decode either form — the wrapper carries no other fields.
type crewPeersJSON struct {
	Members []BridgePeer `json:"members"`
}

// resolveCrewPeers builds the BridgePeer list for a crew. The resolver is
// split out so unit tests can swap in fakes for the provider backend and the
// claim store without going through the full kong/flag stack.
//
// When provider is non-empty the resolver behaves as in the original PR #136
// design: a single backend is configured and every matching claim is fanned
// out against it. When provider is empty the resolver groups matching claims
// by provider, configures each backend exactly once, and concatenates the
// results — this is the path that gives `crabbox crew peers --crew <name>`
// honest cross-provider output without making the caller enumerate providers
// by hand.
func resolveCrewPeers(ctx context.Context, rt Runtime, crew, provider string, flags crewPeersFlags) ([]BridgePeer, error) {
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	matches := filterClaimsForCrew(claims, crew, provider)
	if len(matches) == 0 {
		return []BridgePeer{}, nil
	}
	// Bucket claims by provider so each backend is configured at most once,
	// even when the same provider has several leases in the crew.
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
	for _, p := range order {
		providerPeers, err := resolveCrewPeersForProvider(ctx, rt, p, byProvider[p], flags)
		if err != nil {
			return nil, err
		}
		peers = append(peers, providerPeers...)
	}
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].Slug == peers[j].Slug {
			return peers[i].LeaseID < peers[j].LeaseID
		}
		return peers[i].Slug < peers[j].Slug
	})
	return peers, nil
}

// resolveCrewPeersForProvider configures one provider backend and fans the
// per-provider claim list through it. The caller is responsible for stitching
// the per-provider slices together into the final, slug-sorted view returned
// to the user.
//
// The transport class is determined per-provider:
//
//   - tailnet — managed Linux providers (AWS / Azure / GCP / Hetzner /
//     Proxmox / Static SSH). The resolver does not invoke a bridge backend
//     at all for these; the endpoint is read straight off the claim sidecar
//     (TailscaleIPv4 / TailscaleFQDN), and missing endpoints surface as
//     transport=pending with an honest note.
//   - ssh — SSH-lease providers (exe.dev / RunPod / Daytona / Sprites /
//     Namespace / Semaphore). Endpoint is built from SSHHost+SSHPort; an
//     unset host surfaces as transport=pending.
//   - url — delegated-with-URL providers. The per-provider BridgeProvider is
//     invoked for live target discovery, preserving the original PR #136
//     behavior (publish/list/honest-unsupported).
//   - none — Blacksmith and providers with no Crabbox bridge adapter.
//     Surfaced with a documented note so doctor stays honest.
func resolveCrewPeersForProvider(ctx context.Context, rt Runtime, provider string, claims []leaseClaim, flags crewPeersFlags) ([]BridgePeer, error) {
	class := providerTransportClass(provider)
	peers := make([]BridgePeer, 0, len(claims))
	var bridge BridgeProvider
	bridgeLoaded := false
	for _, claim := range claims {
		peer := bridgePeerFromClaim(claim, class)
		if peer.Transport == TransportURL {
			if !bridgeLoaded {
				b, err := loadBridgeProvider(provider, rt)
				if err != nil {
					return nil, err
				}
				bridge = b
				bridgeLoaded = true
			}
			// We invoke the bridge backend when the caller asked us to mint
			// a share (--share-port) or when no canonical endpoint is yet
			// recorded on the claim. Skipping the lookup when an endpoint
			// is already known keeps `crew peers` cheap for read-only
			// listings on already-published shares.
			needBridge := flags.SharePort > 0 || peer.Endpoint == ""
			if bridge == nil && needBridge {
				peer.BridgeState = "unsupported-provider"
				peer.Transport = TransportNone
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
						if errors.Is(lerr, ErrBridgeNotImplemented) {
							peer.BridgeState = "unsupported"
							peer.Note = fmt.Sprintf("bridge adapter for provider %s reports unsupported", peer.Provider)
							peers = append(peers, peer)
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
	return peers, nil
}

// bridgePeerFromClaim turns a single lease claim into the unified peer row.
// The transport class is provided by the caller (it has already classified
// the provider once for the fan-out path); the rest of the row is filled in
// from the claim sidecar without any provider API calls.
func bridgePeerFromClaim(claim leaseClaim, class string) BridgePeer {
	peer := BridgePeer{
		Slug:     claim.Slug,
		LeaseID:  claim.LeaseID,
		Provider: claim.Provider,
		Crew:     claim.Crew,
		Labels:   cloneStringMap(claim.Labels),
	}
	switch class {
	case TransportTailnet:
		endpoint := firstNonEmpty(claim.TailscaleIPv4, claim.TailscaleFQDN)
		if endpoint == "" {
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
		port := claim.SSHPort
		if port == 0 {
			port = 22
		}
		peer.Transport = TransportSSH
		peer.Endpoint = fmt.Sprintf("ssh://%s:%d", claim.SSHHost, port)
	case TransportURL:
		peer.Transport = TransportURL
		peer.Endpoint = claim.BridgeURL
	case TransportNone:
		peer.Transport = TransportNone
		if isBlacksmithProvider(claim.Provider) {
			peer.Note = "blacksmith owns connectivity"
		} else {
			peer.Note = fmt.Sprintf("no bridge adapter for provider %s", claim.Provider)
		}
	default:
		peer.Transport = TransportNone
		peer.Note = fmt.Sprintf("no bridge adapter for provider %s", claim.Provider)
	}
	return peer
}

// providerTransportClass maps a provider name to its transport class. The
// mapping is authoritative here (rather than derived from provider feature
// flags) so the classification is stable when a future provider's Go-side
// adapter lags behind the lease-record format.
func providerTransportClass(provider string) string {
	switch normalizeProviderName(provider) {
	case "aws", "azure", "gcp", "hetzner", "proxmox", "ssh":
		return TransportTailnet
	case "exe-dev", "exedev", "runpod", "daytona", "sprites", "namespace", "namespace-devbox", "semaphore":
		return TransportSSH
	case "islo", "e2b", "modal", "cloudflare", "railway", "tensorlake":
		return TransportURL
	case "blacksmith", "blacksmith-testbox":
		return TransportNone
	default:
		return TransportNone
	}
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

// filterClaimsForCrew returns the subset of claims that belong to the named
// crew and (when provider is non-empty) the named provider. Empty crew
// returns no matches — crews are never implicit.
func filterClaimsForCrew(claims []leaseClaim, crew, provider string) []leaseClaim {
	crew = normalizeCrewName(crew)
	if crew == "" {
		return nil
	}
	provider = strings.TrimSpace(provider)
	out := make([]leaseClaim, 0, len(claims))
	for _, claim := range claims {
		if normalizeCrewName(claim.Crew) != crew {
			continue
		}
		if provider != "" && claim.Provider != provider {
			continue
		}
		out = append(out, claim)
	}
	return out
}

// loadBridgeProviderFunc is the factory used by resolveCrewPeers; it is a
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
	// loadConfig (not baseConfig) is used so provider credentials picked up
	// from the user config file and from environment variables (ISLO_API_KEY,
	// E2B_API_KEY, …) flow through to the bridge backend. Without this the
	// bridge plane would refuse to call provider APIs even when the rest of
	// the CLI is happy.
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	cfg.Provider = provider
	resolved, err := ProviderFor(provider)
	if err != nil {
		return nil, exit(2, "unknown provider %q for crew bridge", provider)
	}
	backend, err := resolved.Configure(cfg, rt)
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
var ErrBridgeNotImplemented = errors.New("crew bridge plane not implemented for this provider")

func renderBridgePeers(w interface{ Write([]byte) (int, error) }, peers []BridgePeer) {
	if len(peers) == 0 {
		fmt.Fprintln(w, "no peers found")
		return
	}
	for _, peer := range peers {
		fmt.Fprintf(w, "%s\tlease=%s\tprovider=%s\tcrew=%s\ttransport=%s", peer.Slug, peer.LeaseID, peer.Provider, peer.Crew, peer.Transport)
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
