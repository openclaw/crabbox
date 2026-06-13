package vercelsandbox

import (
	"flag"
	"fmt"
	"net"
	"path"
	"strconv"
	"strings"
)

type vercelSandboxFlagValues struct {
	Runtime         *string
	Workdir         *string
	ProjectID       *string
	TeamID          *string
	Scope           *string
	VCPUs           *float64
	TimeoutSecs     *int
	ExecTimeoutSecs *int
	Persistent      *bool
	Snapshot        *string
	SnapshotMode    *string
	NetworkPolicy   *string
	NetworkAllow    *string
	NetworkDeny     *string
	Ports           *string
	ForgetMissing   *bool
}

func RegisterVercelSandboxProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return vercelSandboxFlagValues{
		Runtime:         fs.String("vercel-sandbox-runtime", defaults.VercelSandbox.Runtime, "Vercel Sandbox runtime (node26, node24, node22, python3.13)"),
		Workdir:         fs.String("vercel-sandbox-workdir", defaults.VercelSandbox.Workdir, "Absolute working directory inside the sandbox"),
		ProjectID:       fs.String("vercel-sandbox-project-id", defaults.VercelSandbox.ProjectID, "Vercel project ID used for sandbox scoping"),
		TeamID:          fs.String("vercel-sandbox-team-id", defaults.VercelSandbox.TeamID, "Vercel team ID used for sandbox scoping"),
		Scope:           fs.String("vercel-sandbox-scope", defaults.VercelSandbox.Scope, "Vercel account or team slug used for sandbox scoping"),
		VCPUs:           fs.Float64("vercel-sandbox-vcpus", defaults.VercelSandbox.VCPUs, "requested Vercel Sandbox vCPU count (0 = service default)"),
		TimeoutSecs:     fs.Int("vercel-sandbox-timeout-secs", defaults.VercelSandbox.TimeoutSecs, "sandbox lifetime cap in seconds (0 = service default)"),
		ExecTimeoutSecs: fs.Int("vercel-sandbox-exec-timeout-secs", defaults.VercelSandbox.ExecTimeoutSecs, "command timeout in seconds (0 = service default)"),
		Persistent:      fs.Bool("vercel-sandbox-persistent", defaults.VercelSandbox.Persistent, "request a persistent sandbox when lifecycle support lands"),
		Snapshot:        fs.String("vercel-sandbox-snapshot", defaults.VercelSandbox.Snapshot, "snapshot/checkpoint name or ID for future lifecycle use"),
		SnapshotMode:    fs.String("vercel-sandbox-snapshot-mode", defaults.VercelSandbox.SnapshotMode, "snapshot/checkpoint mode for future lifecycle use"),
		NetworkPolicy:   fs.String("vercel-sandbox-network-policy", defaults.VercelSandbox.NetworkPolicy, "sandbox network policy: default, public, private, restricted, or none"),
		NetworkAllow:    fs.String("vercel-sandbox-network-allow", strings.Join(defaults.VercelSandbox.NetworkAllow, ","), "comma-separated outbound CIDR/domain allow list"),
		NetworkDeny:     fs.String("vercel-sandbox-network-deny", strings.Join(defaults.VercelSandbox.NetworkDeny, ","), "comma-separated outbound IP/CIDR deny list"),
		Ports:           fs.String("vercel-sandbox-ports", strings.Join(defaults.VercelSandbox.Ports, ","), "comma-separated ports or ranges to expose later"),
		ForgetMissing:   fs.Bool("vercel-sandbox-forget-missing", defaults.VercelSandbox.ForgetMissing, "remove the local claim when stop gets 404 (explicit stale-claim cleanup)"),
	}
}

func ApplyVercelSandboxProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), providerName) {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=vercel-sandbox; use --vercel-sandbox-vcpus")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=vercel-sandbox; use --vercel-sandbox-runtime or --vercel-sandbox-vcpus")
		}
	}
	v, ok := values.(vercelSandboxFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "vercel-sandbox-runtime") {
		cfg.VercelSandbox.Runtime = *v.Runtime
	}
	if flagWasSet(fs, "vercel-sandbox-workdir") {
		cfg.VercelSandbox.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "vercel-sandbox-project-id") {
		cfg.VercelSandbox.ProjectID = *v.ProjectID
	}
	if flagWasSet(fs, "vercel-sandbox-team-id") {
		cfg.VercelSandbox.TeamID = *v.TeamID
	}
	if flagWasSet(fs, "vercel-sandbox-scope") {
		cfg.VercelSandbox.Scope = *v.Scope
	}
	if flagWasSet(fs, "vercel-sandbox-vcpus") {
		cfg.VercelSandbox.VCPUs = *v.VCPUs
	}
	if flagWasSet(fs, "vercel-sandbox-timeout-secs") {
		cfg.VercelSandbox.TimeoutSecs = *v.TimeoutSecs
	}
	if flagWasSet(fs, "vercel-sandbox-exec-timeout-secs") {
		cfg.VercelSandbox.ExecTimeoutSecs = *v.ExecTimeoutSecs
	}
	if flagWasSet(fs, "vercel-sandbox-persistent") {
		cfg.VercelSandbox.Persistent = *v.Persistent
	}
	if flagWasSet(fs, "vercel-sandbox-snapshot") {
		cfg.VercelSandbox.Snapshot = *v.Snapshot
	}
	if flagWasSet(fs, "vercel-sandbox-snapshot-mode") {
		cfg.VercelSandbox.SnapshotMode = *v.SnapshotMode
	}
	if flagWasSet(fs, "vercel-sandbox-network-policy") {
		cfg.VercelSandbox.NetworkPolicy = *v.NetworkPolicy
	}
	if flagWasSet(fs, "vercel-sandbox-network-allow") {
		cfg.VercelSandbox.NetworkAllow = splitList(*v.NetworkAllow)
	}
	if flagWasSet(fs, "vercel-sandbox-network-deny") {
		cfg.VercelSandbox.NetworkDeny = splitList(*v.NetworkDeny)
	}
	if flagWasSet(fs, "vercel-sandbox-ports") {
		cfg.VercelSandbox.Ports = splitList(*v.Ports)
	}
	if flagWasSet(fs, "vercel-sandbox-forget-missing") {
		cfg.VercelSandbox.ForgetMissing = *v.ForgetMissing
	}
	return validateVercelSandboxConfig(*cfg)
}

func validateVercelSandboxConfig(cfg Config) error {
	if _, err := vercelSandboxWorkdir(cfg); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.VercelSandbox.ProjectID) != "" &&
		strings.TrimSpace(cfg.VercelSandbox.TeamID) == "" &&
		strings.TrimSpace(cfg.VercelSandbox.Scope) == "" {
		return exit(2, "vercel-sandbox projectId requires teamId or scope")
	}
	switch strings.ToLower(strings.TrimSpace(cfg.VercelSandbox.Runtime)) {
	case "", "node26", "node24", "node22", "python3.13":
	default:
		return exit(2, "vercel-sandbox runtime must be one of node26, node24, node22, python3.13")
	}
	if cfg.VercelSandbox.TimeoutSecs < 0 {
		return exit(2, "vercel-sandbox timeoutSecs must be non-negative")
	}
	if cfg.VercelSandbox.ExecTimeoutSecs < 0 {
		return exit(2, "vercel-sandbox execTimeoutSecs must be non-negative")
	}
	if cfg.VercelSandbox.VCPUs < 0 {
		return exit(2, "vercel-sandbox vcpus must be positive when set")
	}
	if cfg.VercelSandbox.VCPUs > 0 && cfg.VercelSandbox.VCPUs < 0.25 {
		return exit(2, "vercel-sandbox vcpus must be at least 0.25 when set")
	}
	switch strings.ToLower(strings.TrimSpace(cfg.VercelSandbox.NetworkPolicy)) {
	case "", "default", "public", "private", "restricted", "none":
	default:
		return exit(2, "vercel-sandbox networkPolicy must be default, public, private, restricted, or none")
	}
	for _, entry := range append([]string{}, cfg.VercelSandbox.NetworkAllow...) {
		if err := validateNetworkEntry(entry); err != nil {
			return exit(2, "vercel-sandbox networkAllow entry %q is invalid: %v", entry, err)
		}
	}
	for _, entry := range append([]string{}, cfg.VercelSandbox.NetworkDeny...) {
		if err := validateNetworkEntry(entry); err != nil {
			return exit(2, "vercel-sandbox networkDeny entry %q is invalid: %v", entry, err)
		}
		value := strings.TrimSpace(entry)
		if value == "" {
			continue
		}
		if net.ParseIP(value) == nil {
			if _, _, err := net.ParseCIDR(value); err != nil {
				return exit(2, "vercel-sandbox networkDeny entry %q must be an IP address or CIDR; Vercel does not support domain deny rules", entry)
			}
		}
	}
	for _, port := range cfg.VercelSandbox.Ports {
		if err := validatePortSpec(port); err != nil {
			return exit(2, "vercel-sandbox port %q is invalid: %v", port, err)
		}
	}
	return nil
}

func vercelSandboxWorkdir(cfg Config) (string, error) {
	workdir := strings.TrimSpace(cfg.VercelSandbox.Workdir)
	if workdir == "" {
		workdir = defaultWorkdir
	}
	if !path.IsAbs(workdir) {
		return "", exit(2, "vercel-sandbox workdir must be absolute")
	}
	clean := path.Clean(workdir)
	switch clean {
	case "/", "/tmp", "/usr", "/var", "/home", "/vercel", "/vercel/sandbox":
		return "", exit(2, "vercel-sandbox workdir %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}

func validateNetworkEntry(entry string) error {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return nil
	}
	if _, _, err := net.ParseCIDR(entry); err == nil {
		return nil
	}
	if ip := net.ParseIP(entry); ip != nil {
		return nil
	}
	labels := strings.Split(entry, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("domain labels must be 1-63 characters")
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return fmt.Errorf("domain contains invalid character %q", r)
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("domain labels must not start or end with '-'")
		}
	}
	return nil
}

func validatePortSpec(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parts := strings.Split(value, "-")
	if len(parts) > 2 {
		return fmt.Errorf("expected port or start-end range")
	}
	start, err := parsePort(parts[0])
	if err != nil {
		return err
	}
	if len(parts) == 1 {
		return nil
	}
	end, err := parsePort(parts[1])
	if err != nil {
		return err
	}
	if end < start {
		return fmt.Errorf("range end must be >= start")
	}
	return nil
}

func parsePort(value string) (int, error) {
	port, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("must be numeric")
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("must be between 1 and 65535")
	}
	return port, nil
}

func splitList(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
