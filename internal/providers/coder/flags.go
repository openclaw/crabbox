package coder

import core "github.com/openclaw/crabbox/internal/cli"

import (
	"flag"
	"path"
	"strings"

	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterCoderProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterCoderConfigFlags(fs, defaults.Coder)
}

func ApplyCoderProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == coderProvider {
		if err := shared.RejectExplicitMachineSizingFlags(fs, coderProvider, "choose size through the Coder template or --coder-preset", "choose a Coder template with --coder-template"); err != nil {
			return err
		}
		if cfg.TargetOS != "" && cfg.TargetOS != targetLinux {
			return exit(2, "provider=coder supports target=linux only")
		}
	}
	v, ok := values.(core.CoderConfigFlagValues)
	if !ok {
		return nil
	}
	applied := v.Apply(&cfg.Coder, fs)
	if applied.WorkRoot {
		cfg.WorkRoot = cfg.Coder.WorkRoot
	}
	if cfg.Provider == coderProvider {
		return validateCoderConfig(*cfg)
	}
	return nil
}

func validateCoderConfig(cfg Config) error {
	if strings.TrimSpace(cfg.Coder.CLIPath) == "" {
		return exit(2, "coder.cliPath must not be empty")
	}
	if err := validateCoderWait(cfg.Coder.Wait); err != nil {
		return err
	}
	if _, err := cleanCoderWorkRoot(coderWorkRoot(cfg)); err != nil {
		return err
	}
	if _, err := cleanCoderWorkspacePrefix(cfg.Coder.WorkspacePrefix); err != nil {
		return err
	}
	for _, param := range cfg.Coder.Parameters {
		if strings.TrimSpace(param) == "" {
			return exit(2, "coder.parameters entries must not be empty")
		}
		if !strings.Contains(param, "=") {
			return exit(2, "coder parameter %q must use name=value", param)
		}
	}
	return nil
}

func validateCoderWait(value string) error {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "yes", "no", "auto":
		return nil
	default:
		return exit(2, "coder.wait must be yes, no, or auto")
	}
}

func coderWorkRoot(cfg Config) string {
	if strings.TrimSpace(cfg.Coder.WorkRoot) != "" {
		return strings.TrimSpace(cfg.Coder.WorkRoot)
	}
	return core.CoderConfigDefaultWorkRoot
}

func cleanCoderWorkRoot(workRoot string) (string, error) {
	clean := path.Clean(strings.TrimSpace(workRoot))
	if clean == "" || !strings.HasPrefix(clean, "/") {
		return "", exit(2, "coder.workRoot %q must resolve to an absolute path", workRoot)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspaces":
		return "", exit(2, "coder.workRoot %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}

func cleanCoderWorkspacePrefix(prefix string) (string, error) {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		prefix = core.CoderConfigDefaultWorkspacePrefix
	}
	prefix = strings.Trim(prefix, "-")
	if prefix == "" {
		return "", exit(2, "coder.workspacePrefix must include at least one letter or number")
	}
	for _, r := range prefix {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return "", exit(2, "coder.workspacePrefix must contain only letters, numbers, and hyphens")
	}
	return prefix + "-", nil
}
