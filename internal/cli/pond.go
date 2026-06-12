package cli

import (
	"crypto/sha256"
	"encoding/base32"
	"strings"
)

// pondLabelKey is the reserved provider-label key used to group leases into a
// pond. The key is part of the provider label index that every direct and
// brokered backend already writes, so `list --pond <name>` can select peers
// without growing a new verb tree.
//
// The label is emergent: there is no top-level pond object. A pond exists for
// as long as at least one active lease carries this label.
const pondLabelKey = "pond"

// pondTailscaleTagPrefix is the ACL tag namespace used for pond peers. Every
// member of pond `<name>` owned by `<owner>` is advertised as
// `tag:cbx-pond-<owner>-<name>` so one concrete ACL row gates that pond.
const pondTailscaleTagPrefix = "tag:cbx-pond-"

// pondHostsFile is written on every Tailscale-capable Linux peer. A timer
// refreshes it and a managed /etc/hosts block every 30s from the box-local
// `tailscale status --json` output, so peers stay reachable as `<slug>.cbx`
// without the broker ever seeing a Tailscale credential.
const pondHostsFile = "/etc/hosts.cbx"

// pondHostsRefreshPeriod is the refresh cadence baked into the systemd timer
// that rewrites pond host entries. Kept low so a new peer is discoverable
// within a single user-visible interaction window.
const pondHostsRefreshPeriod = "30s"

// maxRequestedPondNameLength bounds the user-visible portion of the label.
// Provider label values are already capped at 63 characters by
// sanitizeProviderLabelValue; we use a stricter limit here so the same name
// also fits inside future hostname-derived identifiers (e.g. `<pond>.<peer>`)
// without truncation.
const maxRequestedPondNameLength = 41

// maxPondTailscaleTagOwnerLength bounds the owner segment of the pond ACL
// tag. Tailscale tags are limited to 63 characters; with the `tag:cbx-pond-`
// prefix (14 chars) and the pond name suffix (up to 41 chars plus a `-`
// separator) we reserve seven characters for the owner. The owner is
// derived from the operator's git email local-part and truncated rather than
// rejected so the same email shape works for personal and shared tailnets.
const maxPondTailscaleTagOwnerLength = 7

// normalizePondName lowercases the requested name and replaces every character
// outside `[a-z0-9-]` with `-`, collapsing runs and trimming leading/trailing
// dashes. The shape matches normalizeLeaseSlug; pond names participate in the
// same DNS-ish identifier space so peer hostnames stay regular.
func normalizePondName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			out.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			out.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(out.String(), "-")
}

// requestedPondName validates a user-supplied `--pond <name>` flag value.
// Empty input is allowed: callers treat that as "no pond assignment".
func requestedPondName(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	name := normalizePondName(value)
	if name == "" {
		return "", exit(2, "--pond must contain at least one letter or digit")
	}
	if len(name) > maxRequestedPondNameLength {
		return "", exit(2, "--pond must be %d characters or fewer after normalization", maxRequestedPondNameLength)
	}
	return name, nil
}

// serverPond returns the pond label value attached to a server, normalized for
// comparison. Servers without a pond label return the empty string.
func serverPond(server Server) string {
	if server.Labels == nil {
		return ""
	}
	return normalizePondName(server.Labels[pondLabelKey])
}

// filterServersByPond returns the subset of servers whose pond label matches
// the requested name. The filter is a no-op when pond is empty so callers can
// pass `--pond` through unconditionally.
func filterServersByPond(servers []Server, pond string) []Server {
	pond = normalizePondName(pond)
	if pond == "" {
		return servers
	}
	out := make([]Server, 0, len(servers))
	for _, server := range servers {
		if serverPond(server) == pond {
			out = append(out, server)
		}
	}
	return out
}

// pondTagOwner derives the tag-safe owner segment from an operator identity
// (typically `localCoordinatorOwner()` — a git email). Anything outside
// `[a-z0-9-]` collapses to `-`; the result is trimmed and bounded so the full
// tag stays within Tailscale's 63-character ceiling. Returns "" when the
// input does not yield any valid characters; callers fall back to the
// hard-coded "user" segment in that case so the tag prefix stays stable.
func pondTagOwner(identity string) string {
	identity = strings.ToLower(strings.TrimSpace(identity))
	if at := strings.IndexByte(identity, '@'); at > 0 {
		identity = identity[:at]
	}
	owner := normalizePondName(identity)
	if owner == "" {
		return ""
	}
	// Stable hash avoids collisions for similarly named operators on shared
	// tailnets.
	if len(owner) > maxPondTailscaleTagOwnerLength {
		sum := sha256.Sum256([]byte(owner))
		encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
		if maxPondTailscaleTagOwnerLength > 0 && maxPondTailscaleTagOwnerLength <= len(encoded) {
			return strings.ToLower(encoded[:maxPondTailscaleTagOwnerLength])
		}
		return strings.ToLower(encoded)
	}
	return owner
}

// pondTailscaleTag renders the ACL tag advertised by every pond peer. Returns
// "" when either argument is empty so callers can compose the value
// unconditionally and skip emission when the lease is not actually a pond
// member.
func pondTailscaleTag(owner, pond string) string {
	owner = pondTagOwner(owner)
	if owner == "" {
		owner = "user"
	}
	pond = normalizePondName(pond)
	if pond == "" {
		return ""
	}
	return pondTailscaleTagPrefix + owner + "-" + pond
}

// appendPondTailscaleTag mutates cfg.Tailscale.Tags to include the pond tag
// when both `--pond` and direct-provider Tailscale are in play. Brokered
// coordinators enforce their own Tailscale tag allowlist, so generated pond
// tags stay out of brokered requests until the Worker owns an explicit policy.
func appendPondTailscaleTag(cfg *Config, dynamicTagAllowed bool) {
	if cfg == nil || !cfg.Tailscale.Enabled || !dynamicTagAllowed {
		return
	}
	tag := pondTailscaleTag(localCoordinatorOwner(), cfg.Pond)
	if tag == "" {
		return
	}
	cfg.Tailscale.Tags = appendUniqueStrings(cfg.Tailscale.Tags, tag)
}

func pondDynamicTailscaleTagAllowed(cfg Config) bool {
	if !providerCapableOfTailscale(cfg.Provider) {
		return false
	}
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return false
	}
	return !shouldUseCoordinator(cfg, provider.Spec())
}

// providerCapableOfTailscale reports whether the named provider advertises
// FeatureTailscale in the registered provider spec.
func providerCapableOfTailscale(provider string) bool {
	caps := providerCapabilities(provider)
	return caps.Tailscale || caps.TailscaleEgress
}

func pondClaimProviderSummary(pond string) (bool, bool) {
	pond = normalizePondName(pond)
	if pond == "" {
		return false, false
	}
	claims, err := listLeaseClaims()
	if err != nil {
		return false, false
	}
	hasClaims := false
	hasTailscale := false
	for _, claim := range claims {
		if normalizePondName(claim.Pond) != pond {
			continue
		}
		hasClaims = true
		caps := providerCapabilities(claim.Provider)
		if (caps.Tailscale || caps.TailscaleEgress) && (!caps.URLBridge || claimHasTailscaleMetadata(claim)) {
			hasTailscale = true
		}
	}
	return hasClaims, hasTailscale
}

func claimHasTailscaleMetadata(claim leaseClaim) bool {
	return claim.TailscaleIPv4 != "" ||
		claim.TailscaleFQDN != "" ||
		claim.TailscaleHostname != "" ||
		len(claim.TailscaleTags) > 0 ||
		claim.TailscaleLoginURL != "" ||
		claim.TailscaleExitNode != "" ||
		claim.TailscaleExitLAN ||
		labelBool(claim.Labels["tailscale"]) ||
		strings.TrimSpace(claim.Labels["tailscale_state"]) != ""
}

// ProviderCapabilities is the per-provider truth about which pond transport
// planes are *physically* possible on its leases. Each plane is independent —
// some providers advertise more than one (Hetzner / Azure / GCP support both
// the Tailscale peer mesh and the operator-side SSH-mesh). Islo separately
// advertises URL ingress plus outbound-only userspace Tailscale access. Older
// code that asked "which one transport does this provider use"
// (providerTransportClass) is now a thin Primary() picker; the capability set
// is the source of truth and the `pond peers` + `pond connect` paths fan out
// across whichever dialable planes the operator actually wants.
//
// Capabilities are derived from the provider's own FeatureSet, so a provider
// opts in to a transport plane by declaring the feature, not by being added
// to a static table. SSH mesh is narrower than FeatureSSH because delegated
// providers may expose a direct login helper without supporting Crabbox-managed
// SSH tunnels as their pond transport.
type ProviderCapabilities struct {
	Tailscale       bool // bidirectional tailnet peer plane
	TailscaleEgress bool // outbound-only userspace tailnet access
	SSHMesh         bool // operator-side `ssh -L` against the lease's SSH endpoint
	URLBridge       bool // native HTTPS endpoint surface (shares, preview URLs, deployments)
}

// providerCapabilities returns the capability set for the named provider.
// The provider's own FeatureSet is the only source of truth: a provider opts
// into a transport plane by declaring the right Feature.
func providerCapabilities(provider string) ProviderCapabilities {
	if p, err := ProviderFor(provider); err == nil {
		spec := p.Spec()
		features := spec.Features
		tailscale := featureSetHas(features, FeatureTailscale)
		return ProviderCapabilities{
			Tailscale:       tailscale && !spec.TailscaleEgressOnly,
			TailscaleEgress: tailscale && spec.TailscaleEgressOnly,
			SSHMesh:         spec.Kind == ProviderKindSSHLease && featureSetHas(features, FeatureSSH),
			URLBridge:       featureSetHas(features, FeatureURLBridge),
		}
	}
	return ProviderCapabilities{}
}

// Available returns every transport this provider can serve, in preference
// order (peer-to-peer first, then native HTTPS, then operator-routed SSH).
// The list is consumed by `pond peers` to populate the per-member
// `transports` field so callers see the full reachability surface.
func (c ProviderCapabilities) Available() []string {
	out := make([]string, 0, 3)
	if c.Tailscale {
		out = append(out, TransportTailnet)
	}
	if c.URLBridge {
		out = append(out, TransportURL)
	}
	if c.SSHMesh {
		out = append(out, TransportSSH)
	}
	return out
}

// Primary picks the single recommended transport for single-valued surfaces
// (BridgePeer.Transport, doctor reachability matrix). The preference order
// matches Available(): peer-to-peer beats native HTTPS beats operator-routed
// SSH. Providers with no declared capability return TransportNone.
func (c ProviderCapabilities) Primary() string {
	if c.Tailscale {
		return TransportTailnet
	}
	if c.URLBridge {
		return TransportURL
	}
	if c.SSHMesh {
		return TransportSSH
	}
	return TransportNone
}
