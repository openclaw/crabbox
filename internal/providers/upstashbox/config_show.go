package upstashbox

import (
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.UpstashBox
	return core.ProviderConfigShowSection{
		JSONKey: "upstashBox", TextLabel: "upstash_box", Providers: []string{"upstash-box"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "baseUrl", JSONValue: core.ConfigShowURL(c.BaseURL), TextName: "base_url", TextValue: core.ConfigShowURL(c.BaseURL)},
			{JSONName: "runtime", JSONValue: c.Runtime, TextName: "runtime", TextValue: c.Runtime},
			{JSONName: "size", JSONValue: c.Size, TextName: "size", TextValue: c.Size},
			{JSONName: "workdir", JSONValue: c.Workdir, TextName: "workdir", TextValue: c.Workdir},
			{JSONName: "keepAlive", JSONValue: c.KeepAlive, TextName: "keep_alive", TextValue: strconv.FormatBool(c.KeepAlive)},
			{JSONName: "auth", JSONValue: core.ConfigShowSecretState(c.APIKey), TextName: "auth", TextValue: core.ConfigShowSecretState(c.APIKey)},
		},
	}
}
