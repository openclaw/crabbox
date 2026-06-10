package mxc

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	CLIPath      *string
	Version      *string
	Containment  *string
	Network      *string
	ReadOnly     *string
	ReadWrite    *string
	AllowedHosts *string
	BlockedHosts *string
	Experimental *bool
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		CLIPath:      fs.String("mxc-cli", defaults.MXC.CLIPath, "path to the MXC executor"),
		Version:      fs.String("mxc-version", defaults.MXC.Version, "MXC configuration schema version"),
		Containment:  fs.String("mxc-containment", defaults.MXC.Containment, "MXC containment backend"),
		Network:      fs.String("mxc-network", defaults.MXC.Network, "MXC network default: block or allow"),
		ReadOnly:     fs.String("mxc-readonly-paths", strings.Join(defaults.MXC.ReadOnlyPaths, ","), "comma-separated additional read-only paths"),
		ReadWrite:    fs.String("mxc-readwrite-paths", strings.Join(defaults.MXC.ReadWritePaths, ","), "comma-separated additional read-write paths"),
		AllowedHosts: fs.String("mxc-allowed-hosts", strings.Join(defaults.MXC.AllowedHosts, ","), "comma-separated allowed outbound hosts"),
		BlockedHosts: fs.String("mxc-blocked-hosts", strings.Join(defaults.MXC.BlockedHosts, ","), "comma-separated blocked outbound hosts"),
		Experimental: fs.Bool("mxc-experimental", defaults.MXC.Experimental, "enable experimental MXC containment backends"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "mxc-cli") {
		cfg.MXC.CLIPath = *v.CLIPath
	}
	if core.FlagWasSet(fs, "mxc-version") {
		cfg.MXC.Version = *v.Version
	}
	if core.FlagWasSet(fs, "mxc-containment") {
		cfg.MXC.Containment = *v.Containment
	}
	if core.FlagWasSet(fs, "mxc-network") {
		cfg.MXC.Network = *v.Network
	}
	if core.FlagWasSet(fs, "mxc-readonly-paths") {
		cfg.MXC.ReadOnlyPaths = splitCSV(*v.ReadOnly)
	}
	if core.FlagWasSet(fs, "mxc-readwrite-paths") {
		cfg.MXC.ReadWritePaths = splitCSV(*v.ReadWrite)
	}
	if core.FlagWasSet(fs, "mxc-allowed-hosts") {
		cfg.MXC.AllowedHosts = splitCSV(*v.AllowedHosts)
	}
	if core.FlagWasSet(fs, "mxc-blocked-hosts") {
		cfg.MXC.BlockedHosts = splitCSV(*v.BlockedHosts)
	}
	if core.FlagWasSet(fs, "mxc-experimental") {
		cfg.MXC.Experimental = *v.Experimental
	}
	return nil
}

func splitCSV(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
