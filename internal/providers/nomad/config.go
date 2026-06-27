package nomad

import (
	"net/url"
	"path"
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
	if nomadDriverRequiresImage(cfg.Nomad.Driver) && strings.TrimSpace(cfg.Nomad.Image) == "" && strings.TrimSpace(cfg.Nomad.JobSpecTemplate) == "" {
		return exit(2, "nomad image must not be empty")
	}
	if err := validateNomadWorkdir(cfg.Nomad.Workdir); err != nil {
		return err
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

func nomadDriverRequiresImage(driver string) bool {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "", "docker", "podman":
		return true
	default:
		return false
	}
}

func validateNomadWorkdir(workdir string) error {
	workdir = strings.TrimSpace(workdir)
	if workdir == "" || !path.IsAbs(workdir) {
		return exit(2, "nomad workdir must be an absolute path")
	}
	clean := path.Clean(workdir)
	switch clean {
	case "/", "/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/run", "/sbin", "/sys", "/tmp", "/usr", "/var", "/work", "/workspace":
		return exit(2, "nomad workdir %q is too broad; choose a dedicated subdirectory", clean)
	}
	return nil
}

func validateNomadAddress(address string) error {
	address = strings.TrimSpace(address)
	if address == "" {
		return nil
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme == "" {
		return exit(2, "nomad address must be an absolute http, https, or unix URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		if parsed.Host == "" {
			return exit(2, "nomad address must be an absolute http, https, or unix URL")
		}
	case "unix":
		if parsed.User != nil || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return exit(2, "nomad unix address must be unix:///absolute/socket/path")
		}
		if strings.TrimSpace(parsed.Path) == "" || !filepath.IsAbs(parsed.Path) {
			return exit(2, "nomad unix address must use an absolute socket path")
		}
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
