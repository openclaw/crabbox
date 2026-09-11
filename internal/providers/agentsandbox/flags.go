package agentsandbox

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterAgentSandboxConfigFlags(fs, defaults.AgentSandbox)
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.AgentSandboxConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.AgentSandbox, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	cfg.AgentSandbox.ExpandAppliedLocalPaths(applied)
	if applied.DeleteOnRelease {
		core.MarkDeleteOnReleaseExplicit(cfg, providerName)
	}
	if err != nil {
		return err
	}
	return validateConfig(*cfg)
}
