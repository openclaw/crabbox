package hyperv

import (
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.HyperV
	return core.ProviderConfigShowSection{
		JSONKey: "hyperv", TextLabel: "hyperv", Providers: []string{"hyperv"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "image", JSONValue: c.Image, TextName: "image", TextValue: c.Image},
			{JSONName: "user", JSONValue: c.User, TextName: "user", TextValue: c.User},
			{JSONName: "workRoot", JSONValue: c.WorkRoot, TextName: "work_root", TextValue: c.WorkRoot},
			{JSONName: "cpus", JSONValue: c.CPUs, TextName: "cpus", TextValue: strconv.Itoa(c.CPUs)},
			{JSONName: "memory", JSONValue: c.Memory, TextName: "memory", TextValue: strconv.Itoa(c.Memory)},
			{JSONName: "switch", JSONValue: c.Switch, TextName: "switch", TextValue: c.Switch},
			{JSONName: "initPassword", JSONValue: c.InitPassword, TextName: "init_password", TextValue: strconv.FormatBool(c.InitPassword)},
		},
	}
}
