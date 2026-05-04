package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"
)

type NetworkMode string

const (
	NetworkAuto      NetworkMode = "auto"
	NetworkTailscale NetworkMode = "tailscale"
	NetworkPublic    NetworkMode = "public"
)

type TailscaleConfig struct {
	Enabled          bool
	Tags             []string
	HostnameTemplate string
	Hostname         string
	AuthKeyEnv       string
	AuthKey          string
}

type TailscaleMetadata struct {
	Enabled  bool     `json:"enabled"`
	Hostname string   `json:"hostname,omitempty"`
	FQDN     string   `json:"fqdn,omitempty"`
	IPv4     string   `json:"ipv4,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	State    string   `json:"state,omitempty"`
	Error    string   `json:"error,omitempty"`
}

type networkFlagValues struct {
	Network         *string
	Tailscale       *bool
	TailscaleTags   *string
	TailscaleHost   *string
	TailscaleKeyEnv *string
}

type networkModeFlagValues struct {
	Network *string
}

type resolvedNetworkTarget struct {
	Target         SSHTarget
	Network        NetworkMode
	FallbackReason string
}

func registerNetworkModeFlag(fs *flag.FlagSet, defaults Config) networkModeFlagValues {
	return networkModeFlagValues{
		Network: fs.String("network", string(defaults.Network), "network mode: auto, tailscale, or public"),
	}
}

func applyNetworkModeFlagOverride(cfg *Config, fs *flag.FlagSet, values networkModeFlagValues) error {
	if flagWasSet(fs, "network") {
		mode, err := parseNetworkMode(*values.Network)
		if err != nil {
			return err
		}
		cfg.Network = mode
	}
	if _, err := parseNetworkMode(string(cfg.Network)); err != nil {
		return err
	}
	return nil
}

func registerNetworkFlags(fs *flag.FlagSet, defaults Config) networkFlagValues {
	return networkFlagValues{
		Network:         fs.String("network", string(defaults.Network), "network mode: auto, tailscale, or public"),
		Tailscale:       fs.Bool("tailscale", defaults.Tailscale.Enabled, "join new managed leases to the configured tailnet"),
		TailscaleTags:   fs.String("tailscale-tags", strings.Join(defaults.Tailscale.Tags, ","), "comma-separated Tailscale tags for new managed leases"),
		TailscaleHost:   fs.String("tailscale-hostname-template", defaults.Tailscale.HostnameTemplate, "Tailscale hostname template for new managed leases"),
		TailscaleKeyEnv: fs.String("tailscale-auth-key-env", defaults.Tailscale.AuthKeyEnv, "environment variable containing a direct-provider Tailscale auth key"),
	}
}

func applyNetworkFlagOverrides(cfg *Config, fs *flag.FlagSet, values networkFlagValues) error {
	if flagWasSet(fs, "network") {
		mode, err := parseNetworkMode(*values.Network)
		if err != nil {
			return err
		}
		cfg.Network = mode
	}
	if flagWasSet(fs, "tailscale") {
		cfg.Tailscale.Enabled = *values.Tailscale
	}
	if flagWasSet(fs, "tailscale-tags") {
		cfg.Tailscale.Tags = normalizeTailscaleTags(splitCommaList(*values.TailscaleTags))
	}
	if flagWasSet(fs, "tailscale-hostname-template") {
		cfg.Tailscale.HostnameTemplate = strings.TrimSpace(*values.TailscaleHost)
	}
	if flagWasSet(fs, "tailscale-auth-key-env") {
		cfg.Tailscale.AuthKeyEnv = strings.TrimSpace(*values.TailscaleKeyEnv)
		cfg.Tailscale.AuthKey = getenv(cfg.Tailscale.AuthKeyEnv, "")
	}
	return validateNetworkConfig(*cfg)
}

func parseNetworkMode(value string) (NetworkMode, error) {
	switch NetworkMode(strings.ToLower(strings.TrimSpace(value))) {
	case "", NetworkAuto:
		return NetworkAuto, nil
	case NetworkTailscale:
		return NetworkTailscale, nil
	case NetworkPublic:
		return NetworkPublic, nil
	default:
		return "", exit(2, "network must be auto, tailscale, or public")
	}
}

func validateNetworkConfig(cfg Config) error {
	if _, err := parseNetworkMode(string(cfg.Network)); err != nil {
		return err
	}
	if cfg.Tailscale.Enabled {
		if len(cfg.Tailscale.Tags) == 0 {
			return exit(2, "tailscale.tags must include at least one tag")
		}
		for _, tag := range cfg.Tailscale.Tags {
			if !validTailscaleTag(tag) {
				return exit(2, "invalid Tailscale tag %q; tags must look like tag:crabbox", tag)
			}
		}
		if strings.TrimSpace(cfg.Tailscale.HostnameTemplate) == "" {
			return exit(2, "tailscale.hostnameTemplate must not be empty")
		}
		if cfg.TargetOS != targetLinux {
			return exit(2, "--tailscale managed provisioning currently supports target=linux only")
		}
		if isBlacksmithProvider(cfg.Provider) {
			return exit(2, "--tailscale is not supported for provider=%s; Blacksmith owns machine connectivity", cfg.Provider)
		}
		if isStaticProvider(cfg.Provider) {
			return exit(2, "--tailscale only provisions managed leases; set static.host to a MagicDNS name or 100.x address and use --network tailscale")
		}
	}
	return nil
}

func normalizeTailscaleTags(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	return appendUniqueStrings(nil, out...)
}

func validTailscaleTag(value string) bool {
	if !strings.HasPrefix(value, "tag:") {
		return false
	}
	name := strings.TrimPrefix(value, "tag:")
	if name == "" || len(name) > 63 {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func renderTailscaleHostname(template, leaseID, slug, provider string) string {
	value := strings.TrimSpace(template)
	if value == "" {
		value = "crabbox-{slug}"
	}
	replacements := map[string]string{
		"{id}":       strings.ReplaceAll(leaseID, "_", "-"),
		"{slug}":     normalizeLeaseSlug(slug),
		"{provider}": normalizeLeaseSlug(provider),
	}
	for key, replacement := range replacements {
		value = strings.ReplaceAll(value, key, replacement)
	}
	value = normalizeLeaseSlug(value)
	if value == "" {
		value = "crabbox-" + strings.ReplaceAll(leaseID, "_", "-")
	}
	return value
}

func resolveNetworkTarget(ctx context.Context, cfg Config, server Server, target SSHTarget) (resolvedNetworkTarget, error) {
	meta := serverTailscaleMetadata(server)
	switch cfg.Network {
	case NetworkPublic:
		return resolvedNetworkTarget{Target: target, Network: NetworkPublic}, nil
	case NetworkTailscale:
		host := tailscaleTargetHost(meta)
		if host == "" {
			if isStaticProvider(cfg.Provider) || server.Provider == staticProvider {
				if !probeSSHTransport(ctx, &target, 6*time.Second) {
					return resolvedNetworkTarget{}, exit(5, "network=tailscale requested for static host %s but SSH is not reachable; is this client joined to the tailnet?", target.Host)
				}
				return resolvedNetworkTarget{Target: target, Network: NetworkTailscale}, nil
			}
			return resolvedNetworkTarget{}, exit(5, "network=tailscale requested but lease %s has no tailnet address", blank(server.Labels["lease"], server.Name))
		}
		next := target
		next.Host = host
		if !probeSSHTransport(ctx, &next, 6*time.Second) {
			return resolvedNetworkTarget{}, exit(5, "network=tailscale requested but %s is not reachable over SSH; is this client joined to the tailnet?", host)
		}
		return resolvedNetworkTarget{Target: next, Network: NetworkTailscale}, nil
	default:
		host := tailscaleTargetHost(meta)
		if host == "" {
			return resolvedNetworkTarget{Target: target, Network: NetworkPublic}, nil
		}
		next := target
		next.Host = host
		if probeSSHTransport(ctx, &next, 5*time.Second) {
			return resolvedNetworkTarget{Target: next, Network: NetworkTailscale}, nil
		}
		return resolvedNetworkTarget{Target: target, Network: NetworkPublic, FallbackReason: "tailscale_unreachable"}, nil
	}
}

func tailscaleTargetHost(meta TailscaleMetadata) string {
	return firstNonEmpty(meta.FQDN, meta.IPv4, meta.Hostname)
}

func serverTailscaleMetadata(server Server) TailscaleMetadata {
	labels := server.Labels
	meta := TailscaleMetadata{
		Enabled:  labelBool(labels["tailscale"]),
		Hostname: labels["tailscale_hostname"],
		FQDN:     labels["tailscale_fqdn"],
		IPv4:     labels["tailscale_ipv4"],
		State:    labels["tailscale_state"],
		Error:    labels["tailscale_error"],
	}
	if tags := splitCommaList(labels["tailscale_tags"]); len(tags) > 0 {
		meta.Tags = tags
	}
	return meta
}

func applyTailscaleMetadataToServer(server *Server, meta TailscaleMetadata) {
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	if meta.Enabled {
		server.Labels["tailscale"] = "true"
	}
	if meta.Hostname != "" {
		server.Labels["tailscale_hostname"] = meta.Hostname
	}
	if meta.FQDN != "" {
		server.Labels["tailscale_fqdn"] = meta.FQDN
	}
	if meta.IPv4 != "" {
		server.Labels["tailscale_ipv4"] = meta.IPv4
	}
	if len(meta.Tags) > 0 {
		server.Labels["tailscale_tags"] = strings.Join(meta.Tags, ",")
	}
	if meta.State != "" {
		server.Labels["tailscale_state"] = meta.State
	}
	if meta.Error != "" {
		server.Labels["tailscale_error"] = meta.Error
	}
}

func (a App) refreshTailscaleMetadata(ctx context.Context, cfg Config, coord *CoordinatorClient, useCoordinator bool, server *Server, target SSHTarget, leaseID string) {
	if server == nil || !serverTailscaleMetadata(*server).Enabled {
		return
	}
	meta, err := readRemoteTailscaleMetadata(ctx, target)
	if err != nil {
		meta = serverTailscaleMetadata(*server)
		meta.State = "failed"
		meta.Error = err.Error()
		fmt.Fprintf(a.Stderr, "warning: tailscale metadata unavailable for %s: %v\n", leaseID, err)
	} else {
		existing := serverTailscaleMetadata(*server)
		meta.Hostname = firstNonEmpty(meta.Hostname, existing.Hostname)
		meta.FQDN = firstNonEmpty(meta.FQDN, existing.FQDN)
		meta.Tags = appendUniqueStrings(existing.Tags, meta.Tags...)
	}
	applyTailscaleMetadataToServer(server, meta)
	if useCoordinator && coord != nil && leaseID != "" {
		if lease, err := coord.UpdateLeaseTailscale(ctx, leaseID, meta); err == nil {
			updated, _, _ := leaseToServerTarget(lease, cfg)
			*server = updated
		} else {
			fmt.Fprintf(a.Stderr, "warning: tailscale metadata update failed for %s: %v\n", leaseID, err)
		}
	}
}

func readRemoteTailscaleMetadata(ctx context.Context, target SSHTarget) (TailscaleMetadata, error) {
	out, err := runSSHOutput(ctx, target, `if [ -f /var/lib/crabbox/tailscale-ipv4 ]; then cat /var/lib/crabbox/tailscale-ipv4; fi
printf '\n'
if [ -f /var/lib/crabbox/tailscale-hostname ]; then cat /var/lib/crabbox/tailscale-hostname; fi
printf '\n'
if [ -f /var/lib/crabbox/tailscale-fqdn ]; then cat /var/lib/crabbox/tailscale-fqdn; fi`)
	if err != nil {
		return TailscaleMetadata{}, err
	}
	lines := strings.Split(out, "\n")
	meta := TailscaleMetadata{Enabled: true, State: "ready"}
	if len(lines) > 0 {
		meta.IPv4 = strings.TrimSpace(lines[0])
	}
	if len(lines) > 1 {
		meta.Hostname = strings.TrimSpace(lines[1])
	}
	if len(lines) > 2 {
		meta.FQDN = strings.TrimSpace(lines[2])
	}
	if meta.IPv4 == "" {
		return TailscaleMetadata{}, fmt.Errorf("remote tailscale metadata missing ipv4")
	}
	return meta, nil
}
