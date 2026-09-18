package opensandbox

import (
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection reports configured values without provider discovery.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.OpenSandbox
	apiURL := core.ConfigShowURL(c.APIURL)
	return core.ProviderConfigShowSection{
		JSONKey: "openSandbox", TextLabel: "opensandbox", Providers: []string{"opensandbox"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "apiUrl", JSONValue: apiURL, TextName: "api_url", TextValue: core.Blank(apiURL, "-")},
			{JSONName: "image", JSONValue: c.Image, TextName: "image", TextValue: c.Image},
			{JSONName: "workdir", JSONValue: c.Workdir, TextName: "workdir", TextValue: c.Workdir},
			{JSONName: "cpu", JSONValue: c.CPU, TextName: "cpu", TextValue: c.CPU},
			{JSONName: "memory", JSONValue: c.Memory, TextName: "memory", TextValue: c.Memory},
			{JSONName: "timeoutSecs", JSONValue: c.TimeoutSecs, TextName: "timeout_secs", TextValue: strconv.Itoa(c.TimeoutSecs)},
			{JSONName: "execTimeoutSecs", JSONValue: c.ExecTimeoutSecs, TextName: "exec_timeout_secs", TextValue: strconv.Itoa(c.ExecTimeoutSecs)},
			{JSONName: "platformOS", JSONValue: c.PlatformOS, TextName: "platform_os", TextValue: c.PlatformOS},
			{JSONName: "platformArch", JSONValue: c.PlatformArch, TextName: "platform_arch", TextValue: c.PlatformArch},
			{JSONName: "secureAccess", JSONValue: c.SecureAccess, TextName: "secure_access", TextValue: strconv.FormatBool(c.SecureAccess)},
			{JSONName: "useServerProxy", JSONValue: c.UseServerProxy, TextName: "use_server_proxy", TextValue: strconv.FormatBool(c.UseServerProxy)},
			{JSONName: "forgetMissing", JSONValue: c.ForgetMissing, TextName: "forget_missing", TextValue: strconv.FormatBool(c.ForgetMissing)},
		},
	}
}
