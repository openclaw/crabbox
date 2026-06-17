package cloudflaresandbox

import (
	"flag"
	"net"
	"net/url"
	"path"
	"strings"
)

type flagValues struct {
	URL             *string
	Workdir         *string
	ExecTimeoutSecs *int
	ForgetMissing   *bool
}

func RegisterProviderFlags(fs *flag.FlagSet, defaults Config) any {
	cfg := defaults.CloudflareSandbox
	return flagValues{
		URL:             fs.String("cloudflare-sandbox-url", cfg.BridgeURL, "Cloudflare Sandbox bridge URL"),
		Workdir:         fs.String("cloudflare-sandbox-workdir", cfg.Workdir, "Absolute working directory inside the sandbox"),
		ExecTimeoutSecs: fs.Int("cloudflare-sandbox-exec-timeout-secs", cfg.ExecTimeoutSecs, "command timeout in seconds (0 = bridge default)"),
		ForgetMissing:   fs.Bool("cloudflare-sandbox-forget-missing", cfg.ForgetMissing, "remove the local claim when stop gets 404 (explicit stale-claim cleanup)"),
	}
}

func ApplyProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), providerName) {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s", providerName)
		}
	}
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "cloudflare-sandbox-url") {
		cfg.CloudflareSandbox.BridgeURL = *v.URL
	}
	if flagWasSet(fs, "cloudflare-sandbox-workdir") {
		cfg.CloudflareSandbox.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "cloudflare-sandbox-exec-timeout-secs") {
		cfg.CloudflareSandbox.ExecTimeoutSecs = *v.ExecTimeoutSecs
	}
	if flagWasSet(fs, "cloudflare-sandbox-forget-missing") {
		cfg.CloudflareSandbox.ForgetMissing = *v.ForgetMissing
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
		workdir = defaultWorkdir
	}
	if !path.IsAbs(workdir) {
		return "", exit(2, "%s workdir must be absolute", providerName)
	}
	clean := path.Clean(workdir)
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace":
		return "", exit(2, "%s workdir %q is too broad; choose a dedicated subdirectory", providerName, clean)
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
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
