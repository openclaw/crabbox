package boxd

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterBoxdConfigFlags(fs, defaults.Boxd)
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if isProviderName(cfg.Provider) {
		if core.FlagWasSet(fs, "class") {
			return core.Exit(2, "--class is not supported for provider=%s; machine sizing follows the boxd account quota", providerName)
		}
		if core.FlagWasSet(fs, "type") {
			return core.Exit(2, "--type is not supported for provider=%s; machine sizing follows the boxd account quota", providerName)
		}
		if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
			return core.Exit(2, "provider=%s supports target=linux only", providerName)
		}
	}
	v, ok := values.(core.BoxdConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Boxd, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, "boxd")
	if applied.WorkRoot {
		core.MarkBoxdWorkRootExplicit(cfg)
		core.RecordProviderFlagIntents(cfg, true, "boxd")
	}
	if applied.DeleteOnRelease {
		core.MarkDeleteOnReleaseExplicit(cfg, providerName)
		core.RecordProviderFlagIntents(cfg, true, "boxd")
	}
	if err != nil {
		return err
	}
	if isProviderName(cfg.Provider) {
		applyDefaults(cfg)
	}
	return nil
}

func isProviderName(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), providerName)
}
