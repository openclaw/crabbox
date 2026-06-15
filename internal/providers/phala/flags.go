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
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		CLIPath:      fs.String("phala-cli", defaults.Phala.CLIPath, "Phala CLI path"),
		InstanceType: fs.String("phala-instance-type", defaults.Phala.InstanceType, "Phala confidential TDX instance type, for example tdx.small"),
		NodeID:       fs.String("phala-node-id", defaults.Phala.NodeID, "Phala node id to pin deployments to"),
		WorkRoot:     fs.String("phala-work-root", defaults.Phala.WorkRoot, "remote Crabbox work root"),
		Compose:      fs.String("phala-compose", defaults.Phala.Compose, "optional Docker Compose file deployed alongside the dev OS"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "phala-cli") {
		cfg.Phala.CLIPath = *v.CLIPath
	}
	if core.FlagWasSet(fs, "phala-instance-type") {
		cfg.Phala.InstanceType = *v.InstanceType
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
	if isProviderName(cfg.Provider) {
		applyDefaults(cfg)
	}
	return nil
}

func isProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "phala-cloud", "dstack":
		return true
	default:
		return false
	}
}
