package applecontainer

import (
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection owns the single display section shared with Apple Machine.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.AppleContainer
	return core.ProviderConfigShowSection{
		JSONKey: "appleContainer", TextLabel: "apple_container", Providers: []string{"apple-container", "apple-machine"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "cliPath", JSONValue: c.CLIPath, TextName: "cli", TextValue: c.CLIPath},
			{JSONName: "image", JSONValue: c.Image, TextName: "image", TextValue: c.Image},
			{JSONName: "user", JSONValue: c.User, TextName: "user", TextValue: c.User},
			{JSONName: "workRoot", JSONValue: c.WorkRoot, TextName: "work_root", TextValue: c.WorkRoot},
			{JSONName: "cpus", JSONValue: c.CPUs, TextName: "cpus", TextValue: strconv.Itoa(c.CPUs)},
			{JSONName: "memory", JSONValue: c.Memory, TextName: "memory", TextValue: core.Blank(c.Memory, "-")},
		},
	}
}
