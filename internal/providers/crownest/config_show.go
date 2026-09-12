package crownest

import (
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection describes loaded values without discovering credentials.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.Crownest
	apiURL := core.ConfigShowURL(c.APIURL)
	return core.ProviderConfigShowSection{
		JSONKey: "crownest", TextLabel: "crownest", Providers: []string{"crownest"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "apiUrl", JSONValue: apiURL, TextName: "api_url", TextValue: core.Blank(apiURL, "-")},
			{JSONName: "projectId", JSONValue: c.ProjectID, TextName: "project_id", TextValue: core.Blank(c.ProjectID, "-")},
			{JSONName: "template", JSONValue: c.Template, TextName: "template", TextValue: core.Blank(c.Template, "-")},
			{JSONName: "timeoutSecs", JSONValue: c.TimeoutSecs, TextName: "timeout_secs", TextValue: strconv.Itoa(c.TimeoutSecs)},
			{JSONName: "forgetMissing", JSONValue: c.ForgetMissing, TextName: "forget_missing", TextValue: strconv.FormatBool(c.ForgetMissing)},
		},
	}
}
