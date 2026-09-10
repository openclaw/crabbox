package namespace

import core "github.com/openclaw/crabbox/internal/cli"

import (
	"flag"
	"path"
	"strings"
)

func RegisterNamespaceProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterNamespaceConfigFlags(fs, defaults.Namespace)
}

func ApplyNamespaceProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.NamespaceConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Namespace, fs)
	if applied.Size {
		cfg.Namespace.Size = strings.ToUpper(strings.TrimSpace(cfg.Namespace.Size))
		cfg.ServerType = cfg.Namespace.Size
		cfg.ServerTypeExplicit = true
	}
	// Size's partial effects precede a duration error; later flags do not apply.
	if err != nil {
		return err
	}
	if applied.WorkRoot {
		cfg.WorkRoot = cfg.Namespace.WorkRoot
	}
	if applied.DeleteOnRelease {
		markDeleteOnReleaseExplicit(cfg)
	}
	return validateNamespaceConfig(*cfg)
}

func validateNamespaceConfig(cfg Config) error {
	if strings.TrimSpace(cfg.Namespace.Size) != "" {
		switch strings.ToUpper(strings.TrimSpace(cfg.Namespace.Size)) {
		case "S", "M", "L", "XL":
		default:
			return exit(2, "namespace devbox size must be S, M, L, or XL")
		}
	}
	size := namespaceSize(cfg)
	switch size {
	case "S", "M", "L", "XL":
	default:
		return exit(2, "namespace devbox size must be S, M, L, or XL")
	}
	if cfg.Namespace.VolumeSizeGB < 0 {
		return exit(2, "namespace volume size must be non-negative")
	}
	if err := cleanNamespaceWorkRoot(namespaceWorkRoot(cfg)); err != nil {
		return err
	}
	return nil
}

func cleanNamespaceWorkRoot(workRoot string) error {
	clean := path.Clean(strings.TrimSpace(workRoot))
	if clean == "" || !strings.HasPrefix(clean, "/") {
		return exit(2, "namespace.workRoot %q must resolve to an absolute path", workRoot)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspaces":
		return exit(2, "namespace.workRoot %q is too broad; choose a dedicated subdirectory", clean)
	}
	return nil
}
