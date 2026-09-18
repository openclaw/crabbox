package cua

import (
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection reports configured values without provider discovery.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.Cua
	apiURL := core.ConfigShowURL(c.APIURL)
	return core.ProviderConfigShowSection{
		JSONKey: "cua", TextLabel: "cua", Providers: []string{"cua"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "apiUrl", JSONValue: apiURL, TextName: "api_url", TextValue: core.Blank(apiURL, "-")},
			{JSONName: "image", JSONValue: c.Image, TextName: "image", TextValue: c.Image},
			{JSONName: "kind", JSONValue: c.Kind, TextName: "kind", TextValue: c.Kind},
			{JSONName: "region", JSONValue: c.Region, TextName: "region", TextValue: c.Region},
			{JSONName: "workdir", JSONValue: c.Workdir, TextName: "workdir", TextValue: c.Workdir},
			{JSONName: "vcpus", JSONValue: c.VCPUs, TextName: "vcpus", TextValue: strconv.Itoa(c.VCPUs)},
			{JSONName: "memoryMB", JSONValue: c.MemoryMB, TextName: "memory_mb", TextValue: strconv.Itoa(c.MemoryMB)},
			{JSONName: "diskGB", JSONValue: c.DiskGB, TextName: "disk_gb", TextValue: strconv.Itoa(c.DiskGB)},
			{JSONName: "startupTimeoutSecs", JSONValue: c.StartupTimeoutSecs, TextName: "startup_timeout_secs", TextValue: strconv.Itoa(c.StartupTimeoutSecs)},
			{JSONName: "execTimeoutSecs", JSONValue: c.ExecTimeoutSecs, TextName: "exec_timeout_secs", TextValue: strconv.Itoa(c.ExecTimeoutSecs)},
			{JSONName: "bridgeCommand", JSONValue: c.BridgeCommand, TextName: "bridge_command", TextValue: c.BridgeCommand},
			{JSONName: "sdkPackage", JSONValue: c.SDKPackage, TextName: "sdk_package", TextValue: c.SDKPackage},
			{JSONName: "sdkImport", JSONValue: c.SDKImport, TextName: "sdk_import", TextValue: c.SDKImport},
			{JSONName: "sdkFallbackImport", JSONValue: c.SDKFallbackImport, TextName: "sdk_fallback_import", TextValue: c.SDKFallbackImport},
		},
	}
}
