package ssh

import core "github.com/openclaw/crabbox/internal/cli"

// ConfigShowSection describes loaded values without resolving an SSH target.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.Static
	return core.ProviderConfigShowSection{
		JSONKey: "static", TextLabel: "static", Providers: []string{"ssh"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "id", JSONValue: c.ID, TextName: "id", TextValue: core.Blank(c.ID, "-")},
			{JSONName: "name", JSONValue: c.Name, TextName: "name", TextValue: core.Blank(c.Name, "-")},
			{JSONName: "host", JSONValue: c.Host, TextName: "host", TextValue: core.Blank(c.Host, "-")},
			{JSONName: "user", JSONValue: c.User, TextName: "user", TextValue: core.Blank(c.User, "-")},
			{JSONName: "port", JSONValue: c.Port, TextName: "port", TextValue: core.Blank(c.Port, "-")},
			{JSONName: "workRoot", JSONValue: c.WorkRoot, TextName: "work_root", TextValue: core.Blank(c.WorkRoot, "-")},
		},
	}
}
