package mxc

import (
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection preserves list values in JSON and their legacy text counts.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.MXC
	return core.ProviderConfigShowSection{
		JSONKey: "mxc", TextLabel: "mxc", Providers: []string{"mxc"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "cliPath", JSONValue: c.CLIPath, TextName: "cli", TextValue: c.CLIPath},
			{JSONName: "version", JSONValue: c.Version, TextName: "version", TextValue: c.Version},
			{JSONName: "containment", JSONValue: c.Containment, TextName: "containment", TextValue: c.Containment},
			{JSONName: "network", JSONValue: c.Network, TextName: "network", TextValue: c.Network},
			{JSONName: "readOnlyPaths", JSONValue: c.ReadOnlyPaths, TextName: "readonly_paths", TextValue: strconv.Itoa(len(c.ReadOnlyPaths))},
			{JSONName: "readWritePaths", JSONValue: c.ReadWritePaths, TextName: "readwrite_paths", TextValue: strconv.Itoa(len(c.ReadWritePaths))},
			{JSONName: "allowedHosts", JSONValue: c.AllowedHosts, TextName: "allowed_hosts", TextValue: strconv.Itoa(len(c.AllowedHosts))},
			{JSONName: "blockedHosts", JSONValue: c.BlockedHosts, TextName: "blocked_hosts", TextValue: strconv.Itoa(len(c.BlockedHosts))},
			{JSONName: "allowDaclMutation", JSONValue: c.AllowDACLMutation, TextName: "allow_dacl_mutation", TextValue: strconv.FormatBool(c.AllowDACLMutation)},
			{JSONName: "allowWindowsUI", JSONValue: c.AllowWindowsUI, TextName: "allow_windows_ui", TextValue: strconv.FormatBool(c.AllowWindowsUI)},
			{JSONName: "experimental", JSONValue: c.Experimental, TextName: "experimental", TextValue: strconv.FormatBool(c.Experimental)},
		},
	}
}
