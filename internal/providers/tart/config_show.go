package tart

import (
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection reports only the established public fields, without defaults.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.Tart
	return core.ProviderConfigShowSection{
		JSONKey: "tart", TextLabel: "tart", Providers: []string{"tart"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "image", JSONValue: c.Image, TextName: "image", TextValue: c.Image},
			{JSONName: "user", JSONValue: c.User, TextName: "user", TextValue: c.User},
			{JSONName: "workRoot", JSONValue: c.WorkRoot, TextName: "work_root", TextValue: c.WorkRoot},
			{JSONName: "cpus", JSONValue: c.CPUs, TextName: "cpus", TextValue: strconv.Itoa(c.CPUs)},
			{JSONName: "memory", JSONValue: c.Memory, TextName: "memory", TextValue: strconv.Itoa(c.Memory)},
			{JSONName: "disk", JSONValue: c.Disk, TextName: "disk", TextValue: strconv.Itoa(c.Disk)},
		},
	}
}
