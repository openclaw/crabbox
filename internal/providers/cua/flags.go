package cua

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
	return core.RegisterCuaConfigFlags(fs, defaults.Cua)
}

func ApplyProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), providerName) {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=cua; use --cua-vcpus and --cua-memory-mb")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=cua; use --cua-image and --cua-kind")
		}
	}
	v, ok := values.(core.CuaConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.Cua, fs)
	return validateProviderConfig(*cfg)
}

func validateProviderConfig(cfg Config) error {
	if _, err := cuaAPIURL(cfg); err != nil {
		return err
	}
	if _, err := cuaWorkdir(cfg); err != nil {
		return err
	}
	kind := strings.ToLower(strings.TrimSpace(blank(cfg.Cua.Kind, core.CuaConfigDefaultKind)))
	if kind != "container" && kind != "vm" {
		return exit(2, "%s kind must be container or vm", providerName)
	}
	if cfg.Cua.VCPUs < 0 {
		return exit(2, "%s vcpus must be non-negative", providerName)
	}
	if cfg.Cua.MemoryMB < 0 {
		return exit(2, "%s memoryMB must be non-negative", providerName)
	}
	if cfg.Cua.DiskGB < 0 {
		return exit(2, "%s diskGB must be non-negative", providerName)
	}
	if cfg.Cua.StartupTimeoutSecs < 0 {
		return exit(2, "%s startupTimeoutSecs must be non-negative", providerName)
	}
	if cfg.Cua.ExecTimeoutSecs < 0 {
		return exit(2, "%s execTimeoutSecs must be non-negative", providerName)
	}
	if int64(cfg.Cua.ExecTimeoutSecs) > maxBridgeTimeoutSeconds {
		return exit(2, "%s execTimeoutSecs exceeds the maximum safe duration", providerName)
	}
	if strings.TrimSpace(cfg.Cua.BridgeCommand) == "" {
		return exit(2, "%s bridgeCommand must not be empty", providerName)
	}
	if strings.TrimSpace(cfg.Cua.SDKPackage) == "" {
		return exit(2, "%s sdkPackage must not be empty", providerName)
	}
	if strings.TrimSpace(cfg.Cua.SDKImport) == "" {
		return exit(2, "%s sdkImport must not be empty", providerName)
	}
	return nil
}

func cuaWorkdir(cfg Config) (string, error) {
	workdir := strings.TrimSpace(blank(cfg.Cua.Workdir, core.CuaConfigDefaultWorkdir))
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

func cuaAPIURL(cfg Config) (string, error) {
	raw := strings.TrimSpace(cfg.Cua.APIURL)
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return "", exit(2, "%s API URL must be an absolute HTTP(S) URL", providerName)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", exit(2, "%s API URL must not contain userinfo, query parameters, or a fragment", providerName)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return "", exit(2, "%s API URL must use HTTPS except for loopback development endpoints", providerName)
	}
	host := canonicalHostname(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		parsed.Host = "[" + host + "]"
	} else {
		parsed.Host = host
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	// The SDK appends /v1 to CUA_BASE_URL. Accept common versioned inputs, but
	// normalize them to the unversioned base so requests do not target /v1/v1.
	parsed.Path = strings.TrimSuffix(parsed.Path, "/v1")
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func isLoopbackHost(host string) bool {
	return shared.IsLoopbackHost(host)
}

func canonicalHostname(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return strings.TrimSuffix(host, ".")
}
