package freestyle

import (
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection describes loaded values without resolving provider state.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.Freestyle
	apiURL := core.ConfigShowURL(c.APIURL)
	auth := core.ConfigShowSecretState(c.APIKey)
	return core.ProviderConfigShowSection{
		JSONKey: "freestyle", TextLabel: "freestyle", Providers: []string{"freestyle"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "apiUrl", JSONValue: apiURL, TextName: "api_url", TextValue: core.Blank(apiURL, "-")},
			{JSONName: "workdir", JSONValue: c.Workdir, TextName: "workdir", TextValue: core.Blank(c.Workdir, "-")},
			{JSONName: "vcpus", JSONValue: c.VCPUs, TextName: "vcpus", TextValue: strconv.Itoa(c.VCPUs)},
			{JSONName: "memoryGB", JSONValue: c.MemoryGB, TextName: "memory_gb", TextValue: strconv.Itoa(c.MemoryGB)},
			{JSONName: "auth", JSONValue: auth, TextName: "auth", TextValue: auth},
		},
	}
}
