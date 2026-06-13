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
	Enabled                bool
	Tags                   []string
	HostnameTemplate       string
	Hostname               string
	AuthKeyEnv             string
	AuthKey                string
	ExitNode               string
	ExitNodeAllowLANAccess bool
}

type TailscaleMetadata struct {
	Enabled                bool     `json:"enabled"`
	Hostname               string   `json:"hostname,omitempty"`
	FQDN                   string   `json:"fqdn,omitempty"`
	IPv4                   string   `json:"ipv4,omitempty"`
	Tags                   []string `json:"tags,omitempty"`
	State                  string   `json:"state,omitempty"`
	Error                  string   `json:"error,omitempty"`
	Version                string   `json:"version,omitempty"`
	DeviceID               string   `json:"deviceID,omitempty"`
	ExitNode               string   `json:"exitNode,omitempty"`
	ExitNodeAllowLANAccess bool     `json:"exitNodeAllowLanAccess,omitempty"`
}

type networkFlagValues struct {
	Network                   *string
	Tailscale                 *bool
	TailscaleTags             *string
	TailscaleHost             *string
	TailscaleKeyEnv           *string
	TailscaleExitNode         *string
	TailscaleExitNodeAllowLAN *bool
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
		Network:                   fs.String("network", string(defaults.Network), "network mode: auto, tailscale, or public"),
		Tailscale:                 fs.Bool("tailscale", defaults.Tailscale.Enabled, "join new managed leases to the configured tailnet"),
		TailscaleTags:             fs.String("tailscale-tags", strings.Join(defaults.Tailscale.Tags, ","), "comma-separated Tailscale tags for new managed leases"),
		TailscaleHost:             fs.String("tailscale-hostname-template", defaults.Tailscale.HostnameTemplate, "Tailscale hostname template for new managed leases"),
		TailscaleKeyEnv:           fs.String("tailscale-auth-key-env", defaults.Tailscale.AuthKeyEnv, "environment variable containing a direct-provider Tailscale auth key"),
		TailscaleExitNode:         fs.String("tailscale-exit-node", defaults.Tailscale.ExitNode, "Tailscale exit node name or 100.x address for new managed leases"),
		TailscaleExitNodeAllowLAN: fs.Bool("tailscale-exit-node-allow-lan-access", defaults.Tailscale.ExitNodeAllowLANAccess, "allow LAN access while using the Tailscale exit node"),
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
	if flagWasSet(fs, "tailscale-exit-node") {
		cfg.Tailscale.ExitNode = strings.TrimSpace(*values.TailscaleExitNode)
	}
	if flagWasSet(fs, "tailscale-exit-node-allow-lan-access") {
		cfg.Tailscale.ExitNodeAllowLANAccess = *values.TailscaleExitNodeAllowLAN
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
		if cfg.Tailscale.ExitNodeAllowLANAccess && strings.TrimSpace(cfg.Tailscale.ExitNode) == "" {
			return exit(2, "tailscale.exitNodeAllowLanAccess requires tailscale.exitNode")
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

func bootstrapNetworkTarget(cfg Config, server Server, target SSHTarget) SSHTarget {
	if !preferTailscaleBootstrap(cfg, server) {
		return target
	}
	host := tailscaleTargetHost(serverTailscaleMetadata(server))
	if host == "" {
		return target
	}
	next := target
	next.Host = host
	next.NetworkKind = NetworkTailscale
	return next
}

func preferTailscaleBootstrap(cfg Config, server Server) bool {
	if cfg.Network == NetworkPublic {
		return false
	}
	if cfg.Network == NetworkTailscale {
		return true
	}
	meta := serverTailscaleMetadata(server)
	return meta.Enabled && meta.ExitNode != ""
}

func tailscaleTargetHost(meta TailscaleMetadata) string {
	return firstNonEmpty(meta.FQDN, meta.IPv4, meta.Hostname)
}

func serverTailscaleMetadata(server Server) TailscaleMetadata {
	labels := server.Labels
	meta := TailscaleMetadata{
		Enabled:                labelBool(labels["tailscale"]),
		Hostname:               labels["tailscale_hostname"],
		FQDN:                   labels["tailscale_fqdn"],
		IPv4:                   labels["tailscale_ipv4"],
		State:                  labels["tailscale_state"],
		Error:                  labels["tailscale_error"],
		Version:                labels["tailscale_version"],
		DeviceID:               labels["tailscale_device_id"],
		ExitNode:               labels["tailscale_exit_node"],
		ExitNodeAllowLANAccess: labelBool(labels["tailscale_exit_node_allow_lan_access"]),
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
	if meta.Version != "" {
		server.Labels["tailscale_version"] = meta.Version
	}
	if meta.DeviceID != "" {
		server.Labels["tailscale_device_id"] = meta.DeviceID
	}
	if meta.ExitNode != "" {
		server.Labels["tailscale_exit_node"] = meta.ExitNode
	}
	if meta.ExitNodeAllowLANAccess {
		server.Labels["tailscale_exit_node_allow_lan_access"] = "true"
	}
}

func (a App) refreshTailscaleMetadata(ctx context.Context, cfg Config, backend Backend, coord *CoordinatorClient, useCoordinator bool, server *Server, target SSHTarget, leaseID string) {
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
		meta.ExitNode = firstNonEmpty(meta.ExitNode, existing.ExitNode)
		meta.ExitNodeAllowLANAccess = meta.ExitNodeAllowLANAccess || existing.ExitNodeAllowLANAccess
	}
	applyTailscaleMetadataToServer(server, meta)
	if useCoordinator && coord != nil && leaseID != "" {
		if lease, err := coord.UpdateLeaseTailscale(ctx, leaseID, meta); err == nil {
			updated, _, _ := leaseToServerTarget(lease, cfg)
			*server = updated
		} else {
			fmt.Fprintf(a.Stderr, "warning: tailscale metadata update failed for %s: %v\n", leaseID, err)
		}
	} else if direct, ok := backend.(TailscaleMetadataBackend); ok && leaseID != "" {
		updated, err := direct.UpdateTailscaleMetadata(ctx, LeaseTarget{Server: *server, SSH: target, LeaseID: leaseID}, meta)
		if err == nil {
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
if [ -f /var/lib/crabbox/tailscale-fqdn ]; then cat /var/lib/crabbox/tailscale-fqdn; fi
printf '\n'
if [ -f /var/lib/crabbox/tailscale-exit-node ]; then cat /var/lib/crabbox/tailscale-exit-node; fi
printf '\n'
if [ -f /var/lib/crabbox/tailscale-exit-node-allow-lan-access ]; then cat /var/lib/crabbox/tailscale-exit-node-allow-lan-access; fi
printf '\n'
if [ -f /var/lib/crabbox/tailscale-version ]; then cat /var/lib/crabbox/tailscale-version; fi
printf '\n'
if [ -f /var/lib/crabbox/tailscale-device-id ]; then cat /var/lib/crabbox/tailscale-device-id; fi`)
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
	if len(lines) > 3 {
		meta.ExitNode = strings.TrimSpace(lines[3])
	}
	if len(lines) > 4 {
		meta.ExitNodeAllowLANAccess = labelBool(strings.TrimSpace(lines[4]))
	}
	if len(lines) > 5 {
		meta.Version = strings.TrimSpace(lines[5])
	}
	if len(lines) > 6 {
		meta.DeviceID = strings.TrimSpace(lines[6])
	}
	if meta.IPv4 == "" {
		return TailscaleMetadata{}, fmt.Errorf("remote tailscale metadata missing ipv4")
	}
	return meta, nil
}

func (a App) logoutRemoteTailscaleBestEffort(ctx context.Context, lease LeaseTarget) {
	if lease.SSH.Host == "" || !serverTailscaleMetadata(lease.Server).Enabled {
		return
	}
	logoutCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if out, err := runSSHCombinedOutput(logoutCtx, lease.SSH, `if command -v tailscale >/dev/null 2>&1; then tailscale logout >/dev/null 2>&1 || exit $?; fi`); err != nil {
		detail := strings.TrimSpace(out)
		if detail == "" {
			detail = err.Error()
		}
		fmt.Fprintf(a.Stderr, "warning: tailscale logout failed for %s: %s\n", lease.LeaseID, detail)
		return
	}
	fmt.Fprintf(a.Stderr, "tailscale logout attempted for %s\n", lease.LeaseID)
}

func validateTailscaleExitNodeEgress(ctx context.Context, server Server, target SSHTarget) error {
	meta := serverTailscaleMetadata(server)
	if strings.TrimSpace(meta.ExitNode) == "" {
		return nil
	}
	command := tailscaleExitNodeEgressCheckScript()
	if out, err := runSSHCombinedOutput(ctx, target, command); err != nil {
		detail := strings.TrimSpace(out)
		if detail == "" {
			detail = err.Error()
		}
		return exit(5, "tailscale exit node %s joined but remote internet egress failed; verify the exit node is approved and forwarding internet traffic: %s", meta.ExitNode, detail)
	}
	return nil
}

func tailscaleExitNodeEgressCheckScript() string {
	return `set -eu
if ! command -v tailscale >/dev/null 2>&1; then
  printf '%s\n' "tailscale is not installed for exit-node egress check" >&2
  exit 87
fi
prefs="$(tailscale debug prefs 2>/dev/null)" || {
  printf '%s\n' "tailscale prefs unavailable for exit-node egress check" >&2
  exit 88
}
case "$prefs" in
  *'"ExitNodeID": ""'*|*'"ExitNodeID":""'*)
    printf '%s\n' "exit node is not selected in tailscale prefs" >&2
    exit 86
    ;;
  *'"ExitNodeID":'*)
    ;;
  *)
    printf '%s\n' "tailscale prefs did not include ExitNodeID" >&2
    exit 89
    ;;
esac
if command -v curl >/dev/null 2>&1; then
  timeout 12 sh -c 'curl -4fsS --connect-timeout 5 https://ifconfig.me/ip || curl -4fsS --connect-timeout 5 https://icanhazip.com' >/tmp/crabbox-exit-node-ip
else
  printf '%s\n' "curl is not installed for exit-node egress check" >&2
  exit 87
fi
test -s /tmp/crabbox-exit-node-ip
`
}
