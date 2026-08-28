package vercelsandbox

import (
	"flag"
	"fmt"
	"net"
	"path"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func RegisterVercelSandboxProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterVercelSandboxConfigFlags(fs, defaults.VercelSandbox)
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
	v, ok := values.(core.VercelSandboxConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.VercelSandbox, fs)
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
	if strings.EqualFold(strings.TrimSpace(cfg.VercelSandbox.NetworkPolicy), "none") &&
		(len(cfg.VercelSandbox.NetworkAllow) > 0 || len(cfg.VercelSandbox.NetworkDeny) > 0) {
		return exit(2, "vercel-sandbox networkPolicy none cannot be combined with networkAllow or networkDeny")
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
	exposedPorts := map[int]struct{}{}
	for _, port := range cfg.VercelSandbox.Ports {
		if err := validatePortSpec(port); err != nil {
			return exit(2, "vercel-sandbox port %q is invalid: %v", port, err)
		}
		value := strings.TrimSpace(port)
		if value == "" {
			continue
		}
		parts := strings.Split(value, "-")
		start, _ := parsePort(parts[0])
		end := start
		if len(parts) == 2 {
			end, _ = parsePort(parts[1])
		}
		for current := start; current <= end; current++ {
			exposedPorts[current] = struct{}{}
			if len(exposedPorts) > 15 {
				return exit(2, "vercel-sandbox supports at most 15 unique exposed ports")
			}
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
