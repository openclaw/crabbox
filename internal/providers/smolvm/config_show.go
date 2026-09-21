package smolvm

import (
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.Smolvm
	return core.ProviderConfigShowSection{
		JSONKey: "smolvm", TextLabel: "smolvm", Providers: []string{"smolvm"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "baseUrl", JSONValue: core.ConfigShowURL(c.BaseURL), TextName: "base_url", TextValue: core.ConfigShowURL(c.BaseURL)},
			{JSONName: "image", JSONValue: c.Image, TextName: "image", TextValue: c.Image},
			{JSONName: "workdir", JSONValue: c.Workdir, TextName: "workdir", TextValue: c.Workdir},
			{JSONName: "cpus", JSONValue: c.CPUs, TextName: "cpus", TextValue: strconv.Itoa(c.CPUs)},
			{JSONName: "memoryMB", JSONValue: c.MemoryMB, TextName: "memory_mb", TextValue: strconv.Itoa(c.MemoryMB)},
			{JSONName: "network", JSONValue: c.Network, TextName: "network", TextValue: c.Network},
			{JSONName: "keep", JSONValue: c.Keep, TextName: "keep", TextValue: strconv.FormatBool(c.Keep)},
			{JSONName: "auth", JSONValue: core.ConfigShowSecretState(c.APIKey), TextName: "auth", TextValue: core.ConfigShowSecretState(c.APIKey)},
		},
	}
}
