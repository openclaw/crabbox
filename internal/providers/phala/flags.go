package phala

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	Config     core.PhalaConfigFlagValues
	SkipAttest *bool
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	registrationDefaults := defaults.Phala
	effective := attestEnabled(defaults)
	registrationDefaults.Attest = &effective
	return flagValues{
		Config:     core.RegisterPhalaConfigFlags(fs, registrationDefaults),
		SkipAttest: fs.Bool("phala-skip-attestation", false, "skip TDX remote attestation verification of the leased CVM (insecure; for diagnostics only)"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	visited := core.PhalaConfigFlagPresence(fs)
	if visited.CLIPath {
		cfg.Phala.CLIPath = *v.Config.CLIPath
		core.RecordProviderFlagInputs(cfg, true, "phala")
	}
	if visited.InstanceType {
		cfg.Phala.InstanceType = *v.Config.InstanceType
		core.RecordProviderFlagInputs(cfg, true, "phala")
		core.MarkPhalaInstanceTypeExplicit(cfg)
	}
	if visited.NodeID {
		cfg.Phala.NodeID = *v.Config.NodeID
		core.RecordProviderFlagInputs(cfg, true, "phala")
	}
	if visited.WorkRoot {
		cfg.Phala.WorkRoot = *v.Config.WorkRoot
		core.RecordProviderFlagInputs(cfg, true, "phala")
	}
	if visited.Compose {
		cfg.Phala.Compose = *v.Config.Compose
		core.RecordProviderFlagInputs(cfg, true, "phala")
	}
	// --phala-skip-attestation is the explicit opt-out and wins over --phala-attest
	// when both are set. Either flag, when present, pins cfg.Phala.Attest so the
	// backend gate reads an explicit value rather than the nil "default on".
	if core.FlagWasSet(fs, "phala-skip-attestation") && *v.SkipAttest {
		disabled := false
		cfg.Phala.Attest = &disabled
		core.RecordProviderFlagInputs(cfg, true, "phala")
	} else if visited.Attest {
		value := *v.Config.Attest
		cfg.Phala.Attest = &value
		core.RecordProviderFlagInputs(cfg, true, "phala")
	}
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		applyDefaults(cfg)
	}
	return nil
}

// attestEnabled reports the effective TDX attestation gate setting. The gate is
// ON by default (nil config => true); only an explicit false value disables it.

func attestEnabled(cfg core.Config) bool {
	return cfg.Phala.Attest == nil || *cfg.Phala.Attest
}
