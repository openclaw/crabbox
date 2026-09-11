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
	applied := v.Apply(&cfg.AgentSandbox, fs)
	cfg.AgentSandbox.ExpandAppliedLocalPaths(applied)
	if applied.DeleteOnRelease {
		core.MarkDeleteOnReleaseExplicit(cfg, providerName)
	}
	return validateConfig(*cfg)
}
