package phala

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	CLIPath      *string
	InstanceType *string
	NodeID       *string
	WorkRoot     *string
	Compose      *string
	Attest       *bool
	SkipAttest   *bool
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		CLIPath:      fs.String("phala-cli", defaults.Phala.CLIPath, "Phala CLI path"),
		InstanceType: fs.String("phala-instance-type", defaults.Phala.InstanceType, "Phala confidential TDX instance type, for example tdx.small"),
		NodeID:       fs.String("phala-node-id", defaults.Phala.NodeID, "Phala node id to pin deployments to"),
		WorkRoot:     fs.String("phala-work-root", defaults.Phala.WorkRoot, "remote Crabbox work root"),
		Compose:      fs.String("phala-compose", defaults.Phala.Compose, "optional Docker Compose file deployed alongside the dev OS"),
		Attest:       fs.Bool("phala-attest", attestEnabled(defaults), "verify the leased CVM's Intel TDX remote attestation before trusting it (default true)"),
		SkipAttest:   fs.Bool("phala-skip-attestation", false, "skip TDX remote attestation verification of the leased CVM (insecure; for diagnostics only)"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	// A command-line class overrides a provider-specific value inherited from
	// config or environment. An explicit provider flag below remains more
	// specific and wins when both flags are present.
	if core.FlagWasSet(fs, "class") {
		core.ClearPhalaInstanceTypeExplicit(cfg)
	}
	if core.FlagWasSet(fs, "phala-cli") {
		cfg.Phala.CLIPath = *v.CLIPath
	}
	if core.FlagWasSet(fs, "phala-instance-type") {
		cfg.Phala.InstanceType = *v.InstanceType
		core.MarkPhalaInstanceTypeExplicit(cfg)
	}
	if core.FlagWasSet(fs, "phala-node-id") {
		cfg.Phala.NodeID = *v.NodeID
	}
	if core.FlagWasSet(fs, "phala-work-root") {
		cfg.Phala.WorkRoot = *v.WorkRoot
	}
	if core.FlagWasSet(fs, "phala-compose") {
		cfg.Phala.Compose = *v.Compose
	}
	// --phala-skip-attestation is the explicit opt-out and wins over --phala-attest
	// when both are set. Either flag, when present, pins cfg.Phala.Attest so the
	// backend gate reads an explicit value rather than the nil "default on".
	if core.FlagWasSet(fs, "phala-skip-attestation") && *v.SkipAttest {
		disabled := false
		cfg.Phala.Attest = &disabled
	} else if core.FlagWasSet(fs, "phala-attest") {
		value := *v.Attest
		cfg.Phala.Attest = &value
	}
	if isProviderName(cfg.Provider) {
		applyDefaults(cfg)
	}
	return nil
}

// attestEnabled reports the effective TDX attestation gate setting. The gate is
// ON by default (nil config => true); only an explicit false value disables it.
func attestEnabled(cfg core.Config) bool {
	return cfg.Phala.Attest == nil || *cfg.Phala.Attest
}

func isProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "phala-cloud", "dstack":
		return true
	default:
		return false
	}
}
