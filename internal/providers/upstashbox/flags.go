package upstashbox

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterUpstashBoxProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterUpstashBoxConfigFlags(fs, defaults.UpstashBox)
}

func ApplyUpstashBoxProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatchesExact(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --upstash-box-size", "use --upstash-box-runtime"); err != nil {
			return err
		}
	}
	v, ok := values.(core.UpstashBoxConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.UpstashBox, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if err != nil {
		return err
	}
	return validateConfig(*cfg)
}

func validateConfig(cfg Config) error {
	if runtime := strings.TrimSpace(cfg.UpstashBox.Runtime); runtime != "" {
		switch runtime {
		case "node", "python", "golang", "ruby", "rust", "node-alpine", "python-alpine", "golang-alpine", "ruby-alpine", "rust-alpine":
		default:
			return exit(2, "invalid upstash-box runtime %q", runtime)
		}
	}
	if size := strings.TrimSpace(cfg.UpstashBox.Size); size != "" {
		switch size {
		case "small", "medium", "large":
		default:
			return exit(2, "invalid upstash-box size %q", size)
		}
	}
	_, err := cleanWorkdir(workdir(cfg))
	return err
}
