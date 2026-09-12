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

func RegisterProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterCuaConfigFlags(fs, defaults.Cua)
}

func ApplyProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), providerName) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --cua-vcpus and --cua-memory-mb", "use --cua-image and --cua-kind"); err != nil {
			return err
		}
	}
	v, ok := values.(core.CuaConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Cua, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if err != nil {
		return err
	}
	return validateProviderConfig(*cfg)
}

func validateProviderConfig(cfg core.Config) error {
	if _, err := cuaAPIURL(cfg); err != nil {
		return err
	}
	if _, err := cuaWorkdir(cfg); err != nil {
		return err
	}
	kind := strings.ToLower(strings.TrimSpace(core.Blank(cfg.Cua.Kind, core.CuaConfigDefaultKind)))
	if kind != "container" && kind != "vm" {
		return core.Exit(2, "%s kind must be container or vm", providerName)
	}
	if cfg.Cua.VCPUs < 0 {
		return core.Exit(2, "%s vcpus must be non-negative", providerName)
	}
	if cfg.Cua.MemoryMB < 0 {
		return core.Exit(2, "%s memoryMB must be non-negative", providerName)
	}
	if cfg.Cua.DiskGB < 0 {
		return core.Exit(2, "%s diskGB must be non-negative", providerName)
	}
	if cfg.Cua.StartupTimeoutSecs < 0 {
		return core.Exit(2, "%s startupTimeoutSecs must be non-negative", providerName)
	}
	if cfg.Cua.ExecTimeoutSecs < 0 {
		return core.Exit(2, "%s execTimeoutSecs must be non-negative", providerName)
	}
	if int64(cfg.Cua.ExecTimeoutSecs) > maxBridgeTimeoutSeconds {
		return core.Exit(2, "%s execTimeoutSecs exceeds the maximum safe duration", providerName)
	}
	if strings.TrimSpace(cfg.Cua.BridgeCommand) == "" {
		return core.Exit(2, "%s bridgeCommand must not be empty", providerName)
	}
	if strings.TrimSpace(cfg.Cua.SDKPackage) == "" {
		return core.Exit(2, "%s sdkPackage must not be empty", providerName)
	}
	if strings.TrimSpace(cfg.Cua.SDKImport) == "" {
		return core.Exit(2, "%s sdkImport must not be empty", providerName)
	}
	return nil
}

func cuaWorkdir(cfg core.Config) (string, error) {
	workdir := strings.TrimSpace(core.Blank(cfg.Cua.Workdir, core.CuaConfigDefaultWorkdir))
	if !path.IsAbs(workdir) {
		return "", core.Exit(2, "%s workdir must be absolute", providerName)
	}
	clean := path.Clean(workdir)
	if clean == "/workspace" {
		return "", core.Exit(2, "%s workdir %q is too broad; choose a dedicated subdirectory", providerName, clean)
	}
	if !strings.HasPrefix(clean, "/workspace/") {
		return "", core.Exit(2, "%s workdir %q must be under /workspace/<dedicated-subdir>", providerName, clean)
	}
	return clean, nil
}

func cuaAPIURL(cfg core.Config) (string, error) {
	raw := strings.TrimSpace(cfg.Cua.APIURL)
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return "", core.Exit(2, "%s API URL must be an absolute HTTP(S) URL", providerName)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", core.Exit(2, "%s API URL must not contain userinfo, query parameters, or a fragment", providerName)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return "", core.Exit(2, "%s API URL must use HTTPS except for loopback development endpoints", providerName)
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
