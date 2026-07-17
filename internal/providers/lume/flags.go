package lume

import (
	"flag"
	"path"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	CLIPath  *string
	Base     *string
	Storage  *string
	User     *string
	WorkRoot *string
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		CLIPath:  fs.String("lume-cli", defaults.Lume.CLIPath, "path to the Lume CLI"),
		Base:     fs.String("lume-base", defaults.Lume.Base, "stopped Lume VM to clone for each lease"),
		Storage:  fs.String("lume-storage", defaults.Lume.Storage, "optional Lume storage location"),
		User:     fs.String("lume-user", defaults.Lume.User, "guest account prepared for SSH"),
		WorkRoot: fs.String("lume-work-root", defaults.Lume.WorkRoot, "guest work root"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "lume-cli") {
		cfg.Lume.CLIPath = *v.CLIPath
	}
	if flagWasSet(fs, "lume-base") {
		cfg.Lume.Base = *v.Base
	}
	if flagWasSet(fs, "lume-storage") {
		cfg.Lume.Storage = *v.Storage
	}
	if flagWasSet(fs, "lume-user") {
		cfg.Lume.User = *v.User
	}
	if flagWasSet(fs, "lume-work-root") {
		cfg.Lume.WorkRoot = *v.WorkRoot
	}
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
