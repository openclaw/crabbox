package nomad

import (
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

var tokenEnvPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.TargetOS) != "" && cfg.TargetOS != core.TargetLinux {
		return exit(2, "nomad target must be linux")
	}
	if err := validateNomadAddress(cfg.Nomad.Address); err != nil {
		return err
	}
	if !tokenEnvPattern.MatchString(nomadTokenEnv(cfg)) {
		return exit(2, "nomad tokenEnv must be a valid environment variable name")
	}
	if strings.TrimSpace(cfg.Nomad.Task) == "" {
		return exit(2, "nomad task must not be empty")
	}
	if strings.TrimSpace(cfg.Nomad.Driver) == "" {
		return exit(2, "nomad driver must not be empty")
	}
	if strings.TrimSpace(cfg.Nomad.Image) == "" {
		return exit(2, "nomad image must not be empty")
	}
	if strings.TrimSpace(cfg.Nomad.Workdir) == "" || !filepath.IsAbs(cfg.Nomad.Workdir) {
		return exit(2, "nomad workdir must be an absolute path")
	}
	if cfg.Nomad.CPU < 0 {
		return exit(2, "nomad cpu must be non-negative")
	}
	if cfg.Nomad.MemoryMB < 0 {
		return exit(2, "nomad memoryMB must be non-negative")
	}
	if cfg.Nomad.DiskMB < 0 {
		return exit(2, "nomad diskMB must be non-negative")
	}
	if cfg.Nomad.AllocReadyTimeout < 0 {
		return exit(2, "nomad allocReadyTimeout must be non-negative")
	}
	if cfg.Nomad.EvalTimeout < 0 {
		return exit(2, "nomad evalTimeout must be non-negative")
	}
	if cfg.Nomad.ExecTimeoutSecs < 0 {
		return exit(2, "nomad execTimeoutSecs must be non-negative")
	}
	return nil
}

func validateNomadAddress(address string) error {
	address = strings.TrimSpace(address)
	if address == "" {
		return nil
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return exit(2, "nomad address must be an absolute http, https, or unix URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "unix":
	default:
		return exit(2, "nomad address scheme must be http, https, or unix")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return exit(2, "nomad address must not include credentials, query, or fragment")
	}
	return nil
}

func nomadTokenEnv(cfg Config) string {
	envName := strings.TrimSpace(cfg.Nomad.TokenEnv)
	if envName == "" {
		return "NOMAD_TOKEN"
	}
	return envName
}

func nomadToken(cfg Config, lookup func(string) string) (string, string) {
	envName := nomadTokenEnv(cfg)
	return strings.TrimSpace(lookup(envName)), envName
}
