package opencomputer

import (
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection reports configured values without provider discovery.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.OpenComputer
	apiURL := core.ConfigShowURL(c.APIURL)
	return core.ProviderConfigShowSection{
		JSONKey: "openComputer", TextLabel: "opencomputer", Providers: []string{"opencomputer"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "apiUrl", JSONValue: apiURL, TextName: "api_url", TextValue: core.Blank(apiURL, "-")},
			{JSONName: "workdir", JSONValue: c.Workdir, TextName: "workdir", TextValue: c.Workdir},
			{JSONName: "cpu", JSONValue: c.CPU, TextName: "cpu", TextValue: strconv.Itoa(c.CPU)},
			{JSONName: "memoryMB", JSONValue: c.MemoryMB, TextName: "memory_mb", TextValue: strconv.Itoa(c.MemoryMB)},
			{JSONName: "timeoutSecs", JSONValue: c.TimeoutSecs, TextName: "timeout_secs", TextValue: strconv.Itoa(c.TimeoutSecs)},
			{JSONName: "execTimeoutSecs", JSONValue: c.ExecTimeoutSecs, TextName: "exec_timeout_secs", TextValue: strconv.Itoa(c.ExecTimeoutSecs)},
			{JSONName: "burst", JSONValue: c.Burst, TextName: "burst", TextValue: strconv.FormatBool(c.Burst)},
			{JSONName: "forgetMissing", JSONValue: c.ForgetMissing, TextName: "forget_missing", TextValue: strconv.FormatBool(c.ForgetMissing)},
		},
	}
}
