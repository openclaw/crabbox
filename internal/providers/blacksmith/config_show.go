package blacksmith

import (
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection describes loaded values without resolving workflow references.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.Blacksmith
	return core.ProviderConfigShowSection{
		JSONKey: "blacksmith", TextLabel: "blacksmith", Providers: []string{"blacksmith-testbox"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "org", JSONValue: c.Org, TextName: "org", TextValue: core.Blank(c.Org, "-")},
			{JSONName: "workflow", JSONValue: c.Workflow, TextName: "workflow", TextValue: core.Blank(c.Workflow, "-")},
			{JSONName: "job", JSONValue: c.Job, TextName: "job", TextValue: core.Blank(c.Job, "-")},
			{JSONName: "ref", JSONValue: c.Ref, TextName: "ref", TextValue: core.Blank(c.Ref, "-")},
			{JSONName: "idleTimeout", JSONValue: c.IdleTimeout.String(), TextName: "idle_timeout", TextValue: c.IdleTimeout.String()},
			{JSONName: "debug", JSONValue: c.Debug, TextName: "debug", TextValue: strconv.FormatBool(c.Debug)},
		},
	}
}
