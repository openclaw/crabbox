package superserve

import (
	"flag"
	"net"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const maxSuperserveSandboxTimeoutSecs = 7 * 24 * 60 * 60

func RegisterSuperserveProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterSuperserveConfigFlags(fs, defaults.Superserve)
}

func ApplySuperserveProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), providerName) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --superserve-template or --superserve-snapshot", "use --superserve-template or --superserve-snapshot"); err != nil {
			return err
		}
	}
	if ok, err := core.ApplyProviderConfigFlags[core.SuperserveConfigFlagValues](cfg, fs, values, &cfg.Superserve, "superserve"); !ok || err != nil {
		return err
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
	return shared.NormalizeHTTPSBaseURL(raw, shared.EndpointURLErrors{
		Invalid:    core.Exit(2, "provider=superserve base URL must be an absolute URL"),
		Components: core.Exit(2, "provider=superserve base URL must not contain userinfo, query parameters, or a fragment"),
		Insecure:   core.Exit(2, "provider=superserve base URL must use HTTPS except for loopback development endpoints"),
	})
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
