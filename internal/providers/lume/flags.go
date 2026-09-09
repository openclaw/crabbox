package lume

import (
	"flag"
	"path"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterLumeConfigFlags(fs, defaults.Lume)
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.LumeConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.Lume, fs)
	if isLumeProviderName(cfg.Provider) {
		applyDefaults(cfg)
		return validateConfig(cfg)
	}
	return nil
}

func validateConfig(cfg *core.Config) error {
	if strings.TrimSpace(cfg.Lume.CLIPath) == "" {
		return exit(2, "lume CLI path must not be empty")
	}
	if strings.TrimSpace(cfg.Lume.Base) == "" {
		return exit(2, "lume base VM must not be empty")
	}
	if strings.TrimSpace(cfg.Lume.Storage) == "ephemeral" {
		return exit(2, "lume storage \"ephemeral\" is not supported because Lume excludes ephemeral VMs from inventory")
	}
	if !validPOSIXUser.MatchString(cfg.Lume.User) {
		return exit(2, "lume user %q is not a valid POSIX account name", cfg.Lume.User)
	}
	cfg.Lume.WorkRoot = path.Clean(strings.TrimSpace(cfg.Lume.WorkRoot))
	cfg.WorkRoot = cfg.Lume.WorkRoot
	userHome := "/Users/" + cfg.Lume.User
	if !path.IsAbs(cfg.Lume.WorkRoot) || !strings.HasPrefix(cfg.Lume.WorkRoot, userHome+"/") {
		return exit(2, "lume work root must be beneath /Users/%s", cfg.Lume.User)
	}
	return nil
}

func isLumeProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "local-lume", "lume-macos":
		return true
	default:
		return false
	}
}
