package lume

import core "github.com/openclaw/crabbox/internal/cli"

// ConfigShowSection preserves raw JSON storage and its existing text fallback.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.Lume
	return core.ProviderConfigShowSection{
		JSONKey: "lume", TextLabel: "lume", Providers: []string{"lume"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "cliPath", JSONValue: c.CLIPath, TextName: "cli", TextValue: c.CLIPath},
			{JSONName: "base", JSONValue: c.Base, TextName: "base", TextValue: c.Base},
			{JSONName: "storage", JSONValue: c.Storage, TextName: "storage", TextValue: core.Blank(c.Storage, "default")},
			{JSONName: "user", JSONValue: c.User, TextName: "user", TextValue: c.User},
			{JSONName: "workRoot", JSONValue: c.WorkRoot, TextName: "work_root", TextValue: c.WorkRoot},
		},
	}
}
