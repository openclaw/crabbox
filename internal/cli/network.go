package cli

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net"
	"strconv"
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
	DeferExitNode          bool
	ScrubCloudInitSecrets  bool
	ResetIdentityBeforeUp  bool
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

type TailscaleMetadataPaths struct {
	Directory              string
	IPv4                   string
	Hostname               string
	FQDN                   string
	ExitNode               string
	ExitNodeAllowLANAccess string
	Version                string
	DeviceID               string
}

func TailscaleGuestMetadataPaths(targetOS, windowsMode string) TailscaleMetadataPaths {
	if targetOS == targetWindows && windowsMode != WindowsModeWSL2 {
		const dir = `C:\ProgramData\crabbox\tailscale`
		return TailscaleMetadataPaths{
			Directory:              dir,
			IPv4:                   dir + `\ipv4`,
			Hostname:               dir + `\hostname`,
			FQDN:                   dir + `\fqdn`,
			ExitNode:               dir + `\exit-node`,
			ExitNodeAllowLANAccess: dir + `\exit-node-allow-lan-access`,
			Version:                dir + `\version`,
			DeviceID:               dir + `\device-id`,
		}
	}
	const dir = "/var/lib/crabbox"
	return TailscaleMetadataPaths{
		Directory:              dir,
		IPv4:                   dir + "/tailscale-ipv4",
		Hostname:               dir + "/tailscale-hostname",
		FQDN:                   dir + "/tailscale-fqdn",
		ExitNode:               dir + "/tailscale-exit-node",
		ExitNodeAllowLANAccess: dir + "/tailscale-exit-node-allow-lan-access",
		Version:                dir + "/tailscale-version",
		DeviceID:               dir + "/tailscale-device-id",
	}
}

func escapePowerShellSingleQuoted(value string) string {
	return strings.ReplaceAll(value, "'", "''")
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
		recordConfigInput(cfg, configInputGeneric, configInputFlag, true)
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
		recordConfigInput(cfg, configInputGeneric, configInputFlag, true)
	}
	if flagWasSet(fs, "tailscale") {
		cfg.Tailscale.Enabled = *values.Tailscale
		recordConfigInput(cfg, configInputGeneric, configInputFlag, true)
	}
	if flagWasSet(fs, "tailscale-tags") {
		cfg.Tailscale.Tags = normalizeTailscaleTags(splitCommaList(*values.TailscaleTags))
		recordConfigInput(cfg, configInputGeneric, configInputFlag, true)
	}
	if flagWasSet(fs, "tailscale-hostname-template") {
		cfg.Tailscale.HostnameTemplate = strings.TrimSpace(*values.TailscaleHost)
		recordConfigInput(cfg, configInputGeneric, configInputFlag, true)
	}
	if flagWasSet(fs, "tailscale-auth-key-env") {
		cfg.Tailscale.AuthKeyEnv = strings.TrimSpace(*values.TailscaleKeyEnv)
		recordConfigInput(cfg, configInputGeneric, configInputFlag, true)
		cfg.Tailscale.AuthKey = configInputEnvString(cfg, configInputGeneric, "", cfg.Tailscale.AuthKeyEnv)
	}
	if flagWasSet(fs, "tailscale-exit-node") {
		cfg.Tailscale.ExitNode = strings.TrimSpace(*values.TailscaleExitNode)
		recordConfigInput(cfg, configInputGeneric, configInputFlag, true)
	}
	if flagWasSet(fs, "tailscale-exit-node-allow-lan-access") {
		cfg.Tailscale.ExitNodeAllowLANAccess = *values.TailscaleExitNodeAllowLAN
		recordConfigInput(cfg, configInputGeneric, configInputFlag, true)
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
		return "", Exit(2, "network must be auto, tailscale, or public")
	}
}

func validateNetworkConfig(cfg Config) error {
	if _, err := parseNetworkMode(string(cfg.Network)); err != nil {
		return err
	}
	if cfg.Tailscale.Enabled {
		if len(cfg.Tailscale.Tags) == 0 {
			return Exit(2, "tailscale.tags must include at least one tag")
		}
		for _, tag := range cfg.Tailscale.Tags {
			if !validTailscaleTag(tag) {
				return Exit(2, "invalid Tailscale tag %q; tags must look like tag:crabbox", tag)
			}
		}
		if strings.TrimSpace(cfg.Tailscale.HostnameTemplate) == "" {
			return Exit(2, "tailscale.hostnameTemplate must not be empty")
		}
		if cfg.Tailscale.ExitNodeAllowLANAccess && strings.TrimSpace(cfg.Tailscale.ExitNode) == "" {
			return Exit(2, "tailscale.exitNodeAllowLanAccess requires tailscale.exitNode")
		}
		if cfg.TargetOS != targetLinux {
			provider, err := ProviderFor(cfg.Provider)
			if err != nil {
				return err
			}
			features := provider.Spec().TargetFeatures[providerTargetKey(cfg.TargetOS, cfg.WindowsMode)]
			if !features.Has(FeatureTailscale) {
				return Exit(2, "--tailscale managed provisioning currently supports target=linux only")
			}
		}
		if isBlacksmithProvider(cfg.Provider) {
			return Exit(2, "--tailscale is not supported for provider=%s; Blacksmith owns machine connectivity", cfg.Provider)
		}
		if isStaticProvider(cfg.Provider) {
			return Exit(2, "--tailscale only provisions managed leases; set static.host to a MagicDNS name or 100.x address and use --network tailscale")
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

func RenderTailscaleHostname(template, leaseID, slug, provider string) string {
	value := strings.TrimSpace(template)
	if value == "" {
		value = "crabbox-{slug}"
	}
	replacements := map[string]string{
		"{id}":       strings.ReplaceAll(leaseID, "_", "-"),
		"{slug}":     NormalizeLeaseSlug(slug),
		"{provider}": NormalizeLeaseSlug(provider),
	}
	for key, replacement := range replacements {
		value = strings.ReplaceAll(value, key, replacement)
	}
	value = NormalizeLeaseSlug(value)
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
					return resolvedNetworkTarget{}, Exit(5, "network=tailscale requested for static host %s but SSH is not reachable; is this client joined to the tailnet?", target.Host)
				}
				return resolvedNetworkTarget{Target: target, Network: NetworkTailscale}, nil
			}
			return resolvedNetworkTarget{}, Exit(5, "network=tailscale requested but lease %s has no tailnet address", blank(server.Labels["lease"], server.Name))
		}
		next := target
		next.Host = host
		if !probeSSHTransport(ctx, &next, 6*time.Second) {
			return resolvedNetworkTarget{}, Exit(5, "network=tailscale requested but %s is not reachable over SSH; is this client joined to the tailnet?", host)
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
	state := strings.TrimSpace(meta.State)
	if !meta.Enabled || (state != "" && !strings.EqualFold(state, "ready")) {
		return ""
	}
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

func ApplyTailscaleMetadataToServer(server *Server, meta TailscaleMetadata) {
	applyTailscaleMetadataToServer(server, meta)
}

func (a App) refreshTailscaleMetadata(ctx context.Context, cfg Config, backend Backend, coord *CoordinatorClient, useCoordinator bool, server *Server, target SSHTarget, leaseID string) {
	if server == nil || !serverTailscaleMetadata(*server).Enabled {
		return
	}
	original := *server
	original.Labels = cloneStringMap(server.Labels)
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
		if lease, err := coord.UpdateLeaseTailscaleForProvider(ctx, leaseID, cfg.Provider, meta); err == nil {
			updated, identityErr := coordinatorTailscaleResponseServer(cfg, original, lease)
			if identityErr != nil {
				*server = original
				fmt.Fprintf(a.Stderr, "warning: tailscale metadata update returned mismatched provider for %s: %v\n", leaseID, identityErr)
				return
			}
			*server = updated
		} else {
			*server = original
			fmt.Fprintf(a.Stderr, "warning: tailscale metadata update failed for %s: %v\n", leaseID, err)
		}
	} else if direct, ok := backend.(TailscaleMetadataBackend); ok && leaseID != "" {
		updated, err := direct.UpdateTailscaleMetadata(ctx, LeaseTarget{Server: original, SSH: target, LeaseID: leaseID}, meta)
		if err == nil {
			*server = updated
		} else {
			*server = original
			fmt.Fprintf(a.Stderr, "warning: tailscale metadata update failed for %s: %v\n", leaseID, err)
		}
	}
}

func coordinatorTailscaleResponseServer(cfg Config, previous Server, lease CoordinatorLease) (Server, error) {
	if err := validateCoordinatorProviderIdentity(cfg.Provider, lease.ID, lease.Provider, true); err != nil {
		return previous, err
	}
	updated, _, _ := leaseToServerTarget(lease, cfg)
	return updated, nil
}

func readRemoteTailscaleMetadata(ctx context.Context, target SSHTarget) (TailscaleMetadata, error) {
	out, err := runSSHOutput(ctx, target, tailscaleMetadataReadScript(target))
	if err != nil {
		return TailscaleMetadata{}, err
	}
	return parseTailscaleMetadataOutput(out)
}

func tailscaleMetadataReadScript(target SSHTarget) string {
	paths := TailscaleGuestMetadataPaths(target.TargetOS, target.WindowsMode)
	if isWindowsNativeTarget(target) {
		return fmt.Sprintf(`$paths = @('%s','%s','%s','%s','%s','%s','%s')
foreach ($path in $paths) {
  if (Test-Path -LiteralPath $path) {
    [Console]::Out.Write(([System.IO.File]::ReadAllText($path)).Trim())
  }
  [Console]::Out.Write([Environment]::NewLine)
}`, escapePowerShellSingleQuoted(paths.IPv4), escapePowerShellSingleQuoted(paths.Hostname),
			escapePowerShellSingleQuoted(paths.FQDN), escapePowerShellSingleQuoted(paths.ExitNode),
			escapePowerShellSingleQuoted(paths.ExitNodeAllowLANAccess), escapePowerShellSingleQuoted(paths.Version),
			escapePowerShellSingleQuoted(paths.DeviceID))
	}
	return fmt.Sprintf(`if [ -f %s ]; then head -n1 %s | tr -d '\r\n'; fi
printf '\n'
if [ -f %s ]; then head -n1 %s | tr -d '\r\n'; fi
printf '\n'
if [ -f %s ]; then head -n1 %s | tr -d '\r\n'; fi
printf '\n'
if [ -f %s ]; then head -n1 %s | tr -d '\r\n'; fi
printf '\n'
if [ -f %s ]; then head -n1 %s | tr -d '\r\n'; fi
printf '\n'
if [ -f %s ]; then head -n1 %s | tr -d '\r\n'; fi
printf '\n'
if [ -f %s ]; then head -n1 %s | tr -d '\r\n'; fi`,
		shellQuote(paths.IPv4), shellQuote(paths.IPv4),
		shellQuote(paths.Hostname), shellQuote(paths.Hostname),
		shellQuote(paths.FQDN), shellQuote(paths.FQDN),
		shellQuote(paths.ExitNode), shellQuote(paths.ExitNode),
		shellQuote(paths.ExitNodeAllowLANAccess), shellQuote(paths.ExitNodeAllowLANAccess),
		shellQuote(paths.Version), shellQuote(paths.Version),
		shellQuote(paths.DeviceID), shellQuote(paths.DeviceID))
}

func parseTailscaleMetadataOutput(out string) (TailscaleMetadata, error) {
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
	if parsed := net.ParseIP(meta.IPv4); parsed == nil || parsed.To4() == nil {
		return TailscaleMetadata{}, fmt.Errorf("remote tailscale metadata has invalid ipv4 %q", meta.IPv4)
	}
	return meta, nil
}

func (a App) logoutRemoteTailscaleBestEffort(ctx context.Context, lease LeaseTarget) {
	if lease.SSH.Host == "" || !serverTailscaleMetadata(lease.Server).Enabled {
		return
	}
	logoutCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if out, err := LogoutTailscaleTarget(logoutCtx, lease.SSH); err != nil {
		detail := strings.TrimSpace(out)
		if detail == "" {
			detail = err.Error()
		}
		fmt.Fprintf(a.Stderr, "warning: tailscale logout failed for %s: %s\n", lease.LeaseID, detail)
		return
	}
	fmt.Fprintf(a.Stderr, "tailscale logout attempted for %s\n", lease.LeaseID)
}

func LogoutTailscaleTarget(ctx context.Context, target SSHTarget) (string, error) {
	return runSSHCombinedOutput(ctx, target, tailscaleLogoutScript(target))
}

func BootstrapTailscaleTarget(ctx context.Context, cfg Config, target SSHTarget) (string, error) {
	if !cfg.Tailscale.Enabled {
		return "", nil
	}
	remote, input, err := tailscaleSSHBootstrapPayload(cfg, target)
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	if err := runSSHInput(ctx, target, remote, strings.NewReader(input), &stdout, &stderr); err != nil {
		return strings.TrimSpace(stdout.String()), fmt.Errorf("bootstrap Tailscale over SSH: %w", err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func tailscaleSSHBootstrapPayload(cfg Config, target SSHTarget) (string, string, error) {
	if !cfg.Tailscale.Enabled {
		return "", "", nil
	}
	targetOS := strings.TrimSpace(target.TargetOS)
	if targetOS == "" {
		targetOS = strings.TrimSpace(cfg.TargetOS)
	}
	if targetOS != targetLinux {
		return "", "", fmt.Errorf("direct Tailscale SSH bootstrap requires target=linux")
	}
	authKey := strings.TrimSpace(cfg.Tailscale.AuthKey)
	if authKey == "" {
		return "", "", fmt.Errorf("Tailscale auth key is required")
	}
	if strings.ContainsAny(authKey, "\r\n") {
		return "", "", fmt.Errorf("Tailscale auth key must not contain newlines")
	}

	const authKeyMarker = "crabbox-tailscale-auth-key-from-stdin"
	scriptCfg := cfg
	scriptCfg.Tailscale.AuthKey = authKeyMarker
	scriptCfg.Tailscale.ScrubCloudInitSecrets = false
	bootstrap := cloudInitTailscaleBootstrap(scriptCfg)
	assignment := "TS_AUTHKEY=" + shellQuote(authKeyMarker)
	if !strings.Contains(bootstrap, assignment) {
		return "", "", fmt.Errorf("build direct Tailscale bootstrap payload")
	}
	bootstrap = strings.Replace(bootstrap, assignment, `: "${TS_AUTHKEY:?}"`, 1)
	if strings.Contains(bootstrap, authKeyMarker) {
		return "", "", fmt.Errorf("direct Tailscale bootstrap payload retained its auth marker")
	}
	script := `set -euo pipefail
retry() {
  n=1
  until "$@"; do
    if [ "$n" -ge 8 ]; then
      return 1
    fi
    sleep $((n * 5))
    n=$((n + 1))
  done
}
` + bootstrap + "\n"
	remote := `sudo -n sh -c 'IFS= read -r TS_AUTHKEY; export TS_AUTHKEY; exec bash -s'`
	return remote, authKey + "\n" + script, nil
}

func tailscaleLogoutScript(target SSHTarget) string {
	return TailscaleLogoutScript(target.TargetOS, target.WindowsMode)
}

func TailscaleLogoutScript(targetOS, windowsMode string) string {
	if targetOS == targetWindows && windowsMode != WindowsModeWSL2 {
		return `$tailscalePath = $null
$tailscale = Get-Command tailscale.exe -ErrorAction SilentlyContinue
if ($tailscale) { $tailscalePath = $tailscale.Source }
if (-not $tailscalePath) {
  $candidate = Join-Path $env:ProgramFiles 'Tailscale\tailscale.exe'
  if (Test-Path -LiteralPath $candidate) { $tailscalePath = $candidate }
}
if ($tailscalePath) {
  & $tailscalePath logout *> $null
  if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}`
	}
	return `if command -v tailscale >/dev/null 2>&1; then tailscale logout >/dev/null 2>&1 || exit $?; fi`
}

func TailscaleIdentityResetScript(targetOS, windowsMode string) string {
	paths := TailscaleGuestMetadataPaths(targetOS, windowsMode)
	if targetOS == targetWindows && windowsMode != WindowsModeWSL2 {
		return fmt.Sprintf(`$tailscaleService=Get-Service -Name Tailscale -ErrorAction SilentlyContinue; `+
			`if ($tailscaleService -and $tailscaleService.Status -ne 'Stopped') { Stop-Service -Name Tailscale -Force -ErrorAction Stop; $tailscaleService.WaitForStatus('Stopped',[TimeSpan]::FromSeconds(30)) }; `+
			`foreach ($statePath in @('C:\ProgramData\Tailscale\tailscaled.state','C:\ProgramData\Tailscale\server-state.conf','C:\Windows\System32\config\systemprofile\AppData\Local\Tailscale\tailscaled.state')) { `+
			`if (Test-Path -LiteralPath $statePath) { Remove-Item -LiteralPath $statePath -Force -ErrorAction Stop }; `+
			`if (Test-Path -LiteralPath $statePath) { throw "Tailscale identity state remains at $statePath" } }; `+
			`$metadataPath='%s'; if (Test-Path -LiteralPath $metadataPath) { Remove-Item -LiteralPath $metadataPath -Recurse -Force -ErrorAction Stop }; `+
			`if (Test-Path -LiteralPath $metadataPath) { throw "Tailscale metadata remains at $metadataPath" }; `,
			escapePowerShellSingleQuoted(paths.Directory))
	}
	return fmt.Sprintf(`if command -v systemctl >/dev/null 2>&1 && systemctl cat tailscaled.service >/dev/null 2>&1; then
  systemctl stop tailscaled
elif command -v service >/dev/null 2>&1 && service tailscaled status >/dev/null 2>&1; then
  service tailscaled stop
fi
rm -f /var/lib/tailscale/tailscaled.state /var/lib/tailscale/server-state.conf
test ! -e /var/lib/tailscale/tailscaled.state
test ! -e /var/lib/tailscale/server-state.conf
rm -f %s/tailscale-*
if find %s -maxdepth 1 -type f -name 'tailscale-*' -print -quit | grep -q .; then
  echo "Tailscale metadata remains after identity reset" >&2
  exit 1
fi`,
		shellQuote(paths.Directory),
		shellQuote(paths.Directory))
}

func validateTailscaleExitNodeEgress(ctx context.Context, server Server, target SSHTarget) error {
	meta := serverTailscaleMetadata(server)
	if strings.TrimSpace(meta.ExitNode) == "" {
		return nil
	}
	if labelBool(server.Labels["tailscale_exit_node_deferred"]) {
		if err := configureTailscaleExitNode(ctx, target, meta); err != nil {
			return err
		}
	}
	command := tailscaleExitNodeEgressCheckScriptForTarget(target)
	if out, err := runSSHCombinedOutput(ctx, target, command); err != nil {
		detail := strings.TrimSpace(out)
		if detail == "" {
			detail = err.Error()
		}
		return Exit(5, "tailscale exit node %s joined but remote internet egress failed; verify the exit node is approved and forwarding internet traffic: %s", meta.ExitNode, detail)
	}
	return nil
}

func configureTailscaleExitNode(ctx context.Context, target SSHTarget, meta TailscaleMetadata) error {
	if !meta.ExitNodeAllowLANAccess && target.NetworkKind != NetworkTailscale {
		return Exit(5, "tailscale exit node %s requires the tailnet SSH endpoint before LAN access can be disabled", meta.ExitNode)
	}
	command := tailscaleExitNodeConfigureScript(target, meta)
	if out, err := runSSHCombinedOutput(ctx, target, command); err != nil {
		detail := strings.TrimSpace(out)
		if detail == "" {
			detail = err.Error()
		}
		return Exit(5, "configure tailscale exit node %s: %s", meta.ExitNode, detail)
	}
	return nil
}

func tailscaleExitNodeConfigureScript(target SSHTarget, meta TailscaleMetadata) string {
	allowLAN := strings.ToLower(strconv.FormatBool(meta.ExitNodeAllowLANAccess))
	if isWindowsNativeTarget(target) {
		return fmt.Sprintf(`$tailscalePath = $null
$tailscale = Get-Command tailscale.exe -ErrorAction SilentlyContinue
if ($tailscale) { $tailscalePath = $tailscale.Source }
if (-not $tailscalePath) {
  $candidate = Join-Path $env:ProgramFiles 'Tailscale\tailscale.exe'
  if (Test-Path -LiteralPath $candidate) { $tailscalePath = $candidate }
}
if (-not $tailscalePath) { throw 'tailscale is not installed for exit-node configuration' }
& $tailscalePath set '%s' '%s'
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }`,
			escapePowerShellSingleQuoted("--exit-node="+meta.ExitNode),
			escapePowerShellSingleQuoted("--exit-node-allow-lan-access="+allowLAN))
	}
	return fmt.Sprintf(`if command -v sudo >/dev/null 2>&1; then
  sudo -n tailscale set --exit-node=%s --exit-node-allow-lan-access=%s
else
  tailscale set --exit-node=%s --exit-node-allow-lan-access=%s
fi`,
		shellQuote(meta.ExitNode), shellQuote(allowLAN),
		shellQuote(meta.ExitNode), shellQuote(allowLAN))
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

func tailscaleExitNodeEgressCheckScriptForTarget(target SSHTarget) string {
	if !isWindowsNativeTarget(target) {
		return tailscaleExitNodeEgressCheckScript()
	}
	return `$tailscalePath = $null
$tailscale = Get-Command tailscale.exe -ErrorAction SilentlyContinue
if ($tailscale) { $tailscalePath = $tailscale.Source }
if (-not $tailscalePath) {
  $candidate = Join-Path $env:ProgramFiles 'Tailscale\tailscale.exe'
  if (Test-Path -LiteralPath $candidate) { $tailscalePath = $candidate }
}
if (-not $tailscalePath) { throw 'tailscale is not installed for exit-node egress check' }
$prefs = & $tailscalePath debug prefs 2>&1
if ($LASTEXITCODE -ne 0) { throw 'tailscale prefs unavailable for exit-node egress check' }
$prefsText = [string]::Join([Environment]::NewLine, [string[]]$prefs)
if ($prefsText -notmatch '"ExitNodeID"\s*:') { throw 'tailscale prefs did not include ExitNodeID' }
if ($prefsText -match '"ExitNodeID"\s*:\s*""') { throw 'exit node is not selected in tailscale prefs' }
$response = Invoke-WebRequest -UseBasicParsing -TimeoutSec 12 -Uri 'https://ifconfig.me/ip'
if ([string]::IsNullOrWhiteSpace($response.Content)) { throw 'exit-node egress response was empty' }`
}
