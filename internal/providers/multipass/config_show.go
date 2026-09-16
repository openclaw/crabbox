package multipass

import (
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection preserves the existing loaded-value projection.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.Multipass
	return core.ProviderConfigShowSection{
		JSONKey: "multipass", TextLabel: "multipass", Providers: []string{"multipass"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "cliPath", JSONValue: c.CLIPath, TextName: "cli", TextValue: c.CLIPath},
			{JSONName: "image", JSONValue: c.Image, TextName: "image", TextValue: c.Image},
			{JSONName: "user", JSONValue: c.User, TextName: "user", TextValue: c.User},
			{JSONName: "workRoot", JSONValue: c.WorkRoot, TextName: "work_root", TextValue: c.WorkRoot},
			{JSONName: "cpus", JSONValue: c.CPUs, TextName: "cpus", TextValue: strconv.Itoa(c.CPUs)},
			{JSONName: "memory", JSONValue: c.Memory, TextName: "memory", TextValue: core.Blank(c.Memory, "-")},
			{JSONName: "disk", JSONValue: c.Disk, TextName: "disk", TextValue: core.Blank(c.Disk, "-")},
			{JSONName: "launchTimeout", JSONValue: c.LaunchTimeout.String(), TextName: "launch_timeout", TextValue: c.LaunchTimeout.String()},
		},
	}
}
