package superserve

import (
	"flag"
	"net"
	"net/url"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type superserveFlagValues struct {
	BaseURL         *string
	Template        *string
	Snapshot        *string
	Workdir         *string
	TimeoutSecs     *int
	ExecTimeoutSecs *int
	NetworkAllowOut *string
	NetworkDenyOut  *string
	ForgetMissing   *bool
}

const maxSuperserveSandboxTimeoutSecs = 7 * 24 * 60 * 60

func RegisterSuperserveProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return superserveFlagValues{
		BaseURL:         fs.String("superserve-base-url", defaults.Superserve.BaseURL, "Trusted Superserve API base URL"),
		Template:        fs.String("superserve-template", defaults.Superserve.Template, "Superserve sandbox template"),
		Snapshot:        fs.String("superserve-snapshot", defaults.Superserve.Snapshot, "Superserve snapshot ID or name"),
		Workdir:         fs.String("superserve-workdir", defaults.Superserve.Workdir, "Absolute working directory inside the sandbox"),
		TimeoutSecs:     fs.Int("superserve-timeout-secs", defaults.Superserve.TimeoutSecs, "Superserve sandbox lifetime cap in seconds (0 = Crabbox TTL)"),
		ExecTimeoutSecs: fs.Int("superserve-exec-timeout-secs", defaults.Superserve.ExecTimeoutSecs, "Superserve command timeout in seconds (0 = service default)"),
		NetworkAllowOut: fs.String("superserve-network-allow-out", strings.Join(defaults.Superserve.NetworkAllowOut, ","), "comma-separated outbound network allow list"),
		NetworkDenyOut:  fs.String("superserve-network-deny-out", strings.Join(defaults.Superserve.NetworkDenyOut, ","), "comma-separated outbound network deny list"),
		ForgetMissing:   fs.Bool("superserve-forget-missing", defaults.Superserve.ForgetMissing, "remove the local claim when stop gets 404 (explicit stale-claim cleanup)"),
	}
}

func ApplySuperserveProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), providerName) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --superserve-template or --superserve-snapshot", "use --superserve-template or --superserve-snapshot"); err != nil {
			return err
		}
	}
	v, ok := values.(superserveFlagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "superserve-base-url") {
		cfg.Superserve.BaseURL = *v.BaseURL
		core.RecordProviderFlagInputs(cfg, true, "superserve")
	}
	if core.FlagWasSet(fs, "superserve-template") {
		cfg.Superserve.Template = *v.Template
		core.RecordProviderFlagInputs(cfg, true, "superserve")
	}
	if core.FlagWasSet(fs, "superserve-snapshot") {
		cfg.Superserve.Snapshot = *v.Snapshot
		core.RecordProviderFlagInputs(cfg, true, "superserve")
	}
	if core.FlagWasSet(fs, "superserve-workdir") {
		cfg.Superserve.Workdir = *v.Workdir
		core.RecordProviderFlagInputs(cfg, true, "superserve")
	}
	if core.FlagWasSet(fs, "superserve-timeout-secs") {
		cfg.Superserve.TimeoutSecs = *v.TimeoutSecs
		core.RecordProviderFlagInputs(cfg, true, "superserve")
	}
	if core.FlagWasSet(fs, "superserve-exec-timeout-secs") {
		cfg.Superserve.ExecTimeoutSecs = *v.ExecTimeoutSecs
		core.RecordProviderFlagInputs(cfg, true, "superserve")
	}
	if core.FlagWasSet(fs, "superserve-network-allow-out") {
		cfg.Superserve.NetworkAllowOut = splitSuperserveList(*v.NetworkAllowOut)
		core.RecordProviderFlagInputs(cfg, true, "superserve")
	}
	if core.FlagWasSet(fs, "superserve-network-deny-out") {
		cfg.Superserve.NetworkDenyOut = splitSuperserveList(*v.NetworkDenyOut)
		core.RecordProviderFlagInputs(cfg, true, "superserve")
	}
	if core.FlagWasSet(fs, "superserve-forget-missing") {
		cfg.Superserve.ForgetMissing = *v.ForgetMissing
		core.RecordProviderFlagInputs(cfg, true, "superserve")
	}
	return validateSuperserveConfig(*cfg)
}

func validateSuperserveConfig(cfg core.Config) error {
	if _, err := validateSuperserveBaseURL(cfg.Superserve.BaseURL); err != nil {
		return err
	}
	if _, err := superserveWorkdir(cfg); err != nil {
		return err
	}
	if cfg.Superserve.TimeoutSecs < 0 {
		return core.Exit(2, "superserve timeoutSecs must be non-negative")
	}
	if cfg.Superserve.ExecTimeoutSecs < 0 {
		return core.Exit(2, "superserve execTimeoutSecs must be non-negative")
	}
	if _, err := superserveSandboxTimeoutSecs(cfg); err != nil {
		return err
	}
	for _, deny := range cfg.Superserve.NetworkDenyOut {
		if _, _, err := net.ParseCIDR(deny); err != nil {
			return core.Exit(2, "superserve networkDenyOut entry %q must be a CIDR", deny)
		}
	}
	return nil
}

func superserveSandboxTimeoutSecs(cfg core.Config) (int, error) {
	timeout := cfg.Superserve.TimeoutSecs
	if timeout == 0 {
		lifetime := cfg.TTL
		if lifetime <= 0 {
			lifetime = 90 * time.Minute
		}
		timeout = int((lifetime + time.Second - 1) / time.Second)
	}
	if timeout > maxSuperserveSandboxTimeoutSecs {
		return 0, core.Exit(2, "superserve sandbox lifetime must not exceed %d seconds (7 days)", maxSuperserveSandboxTimeoutSecs)
	}
	// Sandbox lifetime is an independent hard resource cap. It may intentionally
	// be shorter than the command timeout to bound billing and remote lifetime.
	return timeout, nil
}

func validateSuperserveBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = defaultBaseURL
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", core.Exit(2, "provider=superserve base URL must be an absolute URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", core.Exit(2, "provider=superserve base URL must not contain userinfo, query parameters, or a fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return "", core.Exit(2, "provider=superserve base URL must use HTTPS except for loopback development endpoints")
	}
	parsed.Host = shared.CanonicalHostPort(parsed)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

func superserveWorkdir(cfg core.Config) (string, error) {
	workdir := strings.TrimSpace(cfg.Superserve.Workdir)
	if workdir == "" {
		workdir = defaultWorkdir
	}
	if !path.IsAbs(workdir) {
		return "", core.Exit(2, "superserve workdir must be absolute")
	}
	clean := path.Clean(workdir)
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace":
		return "", core.Exit(2, "superserve workdir %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}

func isLoopbackHost(host string) bool {
	return shared.IsLoopbackHost(host)
}

func splitSuperserveList(value string) []string {
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
