package cua

import (
	"flag"
	"net"
	"net/url"
	"path"
	"strings"
)

type flagValues struct {
	APIURL             *string
	Image              *string
	Kind               *string
	Region             *string
	Workdir            *string
	VCPUs              *int
	MemoryMB           *int
	DiskGB             *int
	StartupTimeoutSecs *int
	ExecTimeoutSecs    *int
	BridgeCommand      *string
	SDKPackage         *string
	SDKImport          *string
	SDKFallbackImport  *string
}

func RegisterProviderFlags(fs *flag.FlagSet, defaults Config) any {
	cfg := defaults.Cua
	return flagValues{
		APIURL:             fs.String("cua-api-url", cfg.APIURL, "Trusted CUA API base URL; not accepted from repository config"),
		Image:              fs.String("cua-image", cfg.Image, "CUA Linux sandbox image"),
		Kind:               fs.String("cua-kind", cfg.Kind, "CUA sandbox kind: container or vm"),
		Region:             fs.String("cua-region", cfg.Region, "CUA deployment region (empty = service default/policy)"),
		Workdir:            fs.String("cua-workdir", cfg.Workdir, "Absolute working directory inside the sandbox"),
		VCPUs:              fs.Int("cua-vcpus", cfg.VCPUs, "CUA sandbox vCPU count (0 = service default)"),
		MemoryMB:           fs.Int("cua-memory-mb", cfg.MemoryMB, "CUA sandbox memory in MB (0 = service default)"),
		DiskGB:             fs.Int("cua-disk-gb", cfg.DiskGB, "CUA sandbox disk in GB (0 = service default)"),
		StartupTimeoutSecs: fs.Int("cua-startup-timeout-secs", cfg.StartupTimeoutSecs, "CUA sandbox startup timeout in seconds (0 = Crabbox default)"),
		ExecTimeoutSecs:    fs.Int("cua-exec-timeout-secs", cfg.ExecTimeoutSecs, "CUA command timeout in seconds (0 = Crabbox default 600)"),
		BridgeCommand:      fs.String("cua-bridge-command", cfg.BridgeCommand, "trusted local Python command for the future CUA SDK bridge"),
		SDKPackage:         fs.String("cua-sdk-package", cfg.SDKPackage, "trusted local Python package name for CUA SDK diagnostics"),
		SDKImport:          fs.String("cua-sdk-import", cfg.SDKImport, "trusted local Python import path for CUA SDK diagnostics"),
		SDKFallbackImport:  fs.String("cua-sdk-fallback-import", cfg.SDKFallbackImport, "trusted local fallback import path for CUA SDK diagnostics"),
	}
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
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "cua-api-url") {
		cfg.Cua.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "cua-image") {
		cfg.Cua.Image = *v.Image
	}
	if flagWasSet(fs, "cua-kind") {
		cfg.Cua.Kind = *v.Kind
	}
	if flagWasSet(fs, "cua-region") {
		cfg.Cua.Region = *v.Region
	}
	if flagWasSet(fs, "cua-workdir") {
		cfg.Cua.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "cua-vcpus") {
		cfg.Cua.VCPUs = *v.VCPUs
	}
	if flagWasSet(fs, "cua-memory-mb") {
		cfg.Cua.MemoryMB = *v.MemoryMB
	}
	if flagWasSet(fs, "cua-disk-gb") {
		cfg.Cua.DiskGB = *v.DiskGB
	}
	if flagWasSet(fs, "cua-startup-timeout-secs") {
		cfg.Cua.StartupTimeoutSecs = *v.StartupTimeoutSecs
	}
	if flagWasSet(fs, "cua-exec-timeout-secs") {
		cfg.Cua.ExecTimeoutSecs = *v.ExecTimeoutSecs
	}
	if flagWasSet(fs, "cua-bridge-command") {
		cfg.Cua.BridgeCommand = *v.BridgeCommand
	}
	if flagWasSet(fs, "cua-sdk-package") {
		cfg.Cua.SDKPackage = *v.SDKPackage
	}
	if flagWasSet(fs, "cua-sdk-import") {
		cfg.Cua.SDKImport = *v.SDKImport
	}
	if flagWasSet(fs, "cua-sdk-fallback-import") {
		cfg.Cua.SDKFallbackImport = *v.SDKFallbackImport
	}
	return validateProviderConfig(*cfg)
}

func validateProviderConfig(cfg Config) error {
	if _, err := cuaAPIURL(cfg); err != nil {
		return err
	}
	if _, err := cuaWorkdir(cfg); err != nil {
		return err
	}
	kind := strings.ToLower(strings.TrimSpace(blank(cfg.Cua.Kind, defaultKind)))
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
	workdir := strings.TrimSpace(blank(cfg.Cua.Workdir, defaultWorkdir))
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
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func canonicalHostname(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return strings.TrimSuffix(host, ".")
}
