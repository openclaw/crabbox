package firecracker

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterFirecrackerProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterFirecrackerConfigFlags(fs, defaults.Firecracker)
}

func ApplyFirecrackerProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if isFirecrackerProviderName(cfg.Provider) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --firecracker-cpus, --firecracker-memory-mib, and --firecracker-disk-mib", "use explicit Firecracker kernel, rootfs, and sizing flags"); err != nil {
			return err
		}
	}
	v, ok := values.(core.FirecrackerConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Firecracker, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, "firecracker")
	cfg.Firecracker.ExpandAppliedLocalPaths(applied)
	if applied.User {
		cfg.SSHUser = cfg.Firecracker.User
	}
	if applied.WorkRoot {
		cfg.WorkRoot = cfg.Firecracker.WorkRoot
	}
	if err != nil {
		return err
	}
	if applied.DeleteOnRelease {
		core.MarkDeleteOnReleaseExplicit(cfg, providerName)
		core.RecordProviderFlagIntents(cfg, true, "firecracker")
	}
	if isFirecrackerProviderName(cfg.Provider) {
		applyDefaults(cfg)
	}
	return nil
}

func isFirecrackerProviderName(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), providerName)
}
