package cloudflaresandbox

import (
	"flag"
	"net"
	"net/url"
	"path"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterCloudflareSandboxConfigFlags(fs, defaults.CloudflareSandbox)
}

func ApplyProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), providerName) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "", ""); err != nil {
			return err
		}
	}
	v, ok := values.(core.CloudflareSandboxConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.CloudflareSandbox, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if err != nil {
		return err
	}
	return validateProviderConfig(*cfg)
}

func validateProviderConfig(cfg Config) error {
	if _, err := bridgeURL(cfg); err != nil {
		return err
	}
	if _, err := cloudflareSandboxWorkdir(cfg); err != nil {
		return err
	}
	if cfg.CloudflareSandbox.ExecTimeoutSecs < 0 {
		return exit(2, "%s execTimeoutSecs must be non-negative", providerName)
	}
	return nil
}

func cloudflareSandboxWorkdir(cfg Config) (string, error) {
	workdir := strings.TrimSpace(cfg.CloudflareSandbox.Workdir)
	if workdir == "" {
		workdir = core.CloudflareSandboxConfigDefaultWorkdir
	}
	if !path.IsAbs(workdir) {
		return "", exit(2, "%s workdir must be absolute", providerName)
	}
	clean := path.Clean(workdir)
	if clean == "/workspace" {
		return "", exit(2, "%s workdir %q is too broad; choose a dedicated subdirectory", providerName, clean)
	}
	if !strings.HasPrefix(clean, "/workspace/") {
		return "", exit(2, "%s workdir %q must be under /workspace/<dedicated-subdir>", providerName, clean)
	}
	return clean, nil
}

func bridgeURL(cfg Config) (string, error) {
	raw := strings.TrimSpace(cfg.CloudflareSandbox.BridgeURL)
	if raw == "" {
		return "", exit(2, "%s requires cloudflareSandbox.url or CRABBOX_CLOUDFLARE_SANDBOX_URL", providerName)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", exit(2, "%s bridge URL %q is invalid", providerName, bridgeURLForError(raw))
	}
	if parsed.User != nil {
		return "", exit(2, "%s bridge URL must not include userinfo", providerName)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return "", exit(2, "%s bridge URL %q must use https unless it targets localhost", providerName, bridgeURLForError(raw))
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", exit(2, "%s bridge URL %q must not include query or fragment components", providerName, bridgeURLForError(raw))
	}
	parsed.Host = canonicalHostPort(parsed)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return strings.TrimRight(parsed.String(), "/"), nil
}

func bridgeURLForError(raw string) string {
	parsed, err := url.Parse(raw)
	if err == nil {
		if parsed.Opaque != "" || parsed.Host == "" {
			return "<redacted>"
		}
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		return parsed.String()
	}
	return "<redacted>"
}

func isLoopbackHost(host string) bool {
	return shared.IsLoopbackHost(host)
}

func canonicalHostPort(parsed *url.URL) string {
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	if port == "" {
		if strings.Contains(host, ":") {
			return "[" + host + "]"
		}
		return host
	}
	return net.JoinHostPort(host, port)
}
