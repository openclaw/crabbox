package vast

import (
	"fmt"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) NormalizeConfigForShow(cfg core.Config) core.Config {
	cfg.Vast.WorkRoot = core.EffectiveVastWorkRoot(cfg)
	cfg.WorkRoot = cfg.Vast.WorkRoot
	if !core.IsSSHUserExplicit(&cfg) {
		cfg.SSHUser = cfg.Vast.User
	}
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = nil
	return cfg
}

func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.Vast
	c.WorkRoot = core.EffectiveVastWorkRoot(cfg)
	return core.ProviderConfigShowSection{JSONKey: "vast", TextLabel: "vast", Providers: []string{"vast"}, Fields: []core.ProviderConfigShowField{
		{JSONName: "apiUrl", JSONValue: core.ConfigShowURL(c.APIURL), TextName: "api_url", TextValue: core.Blank(core.ConfigShowURL(c.APIURL), "-")},
		{JSONName: "instanceType", JSONValue: c.InstanceType, TextName: "instance_type", TextValue: core.Blank(c.InstanceType, "-")},
		{JSONName: "gpuName", JSONValue: c.GPUName, TextName: "gpu_name", TextValue: core.Blank(c.GPUName, "-")},
		{JSONName: "gpuCount", JSONValue: c.GPUCount, TextName: "gpu_count", TextValue: fmt.Sprintf("%d", c.GPUCount)},
		{JSONName: "image", JSONValue: c.Image, TextName: "image", TextValue: core.Blank(c.Image, "-")},
		{JSONName: "templateId", JSONValue: c.TemplateID, TextName: "template_id", TextValue: core.Blank(c.TemplateID, "-")},
		{JSONName: "runtype", JSONValue: c.Runtype, TextName: "runtype", TextValue: core.Blank(c.Runtype, "-")},
		{JSONName: "diskGB", JSONValue: c.DiskGB, TextName: "disk_gb", TextValue: fmt.Sprintf("%d", c.DiskGB)},
		{JSONName: "maxDphTotal", JSONValue: c.MaxDphTotal, TextName: "max_dph_total", TextValue: fmt.Sprintf("%.4g", c.MaxDphTotal)},
		{JSONName: "minReliability", JSONValue: c.MinReliability, TextName: "min_reliability", TextValue: fmt.Sprintf("%.4g", c.MinReliability)},
		{JSONName: "order", JSONValue: c.Order, TextName: "order", TextValue: core.Blank(c.Order, "-")},
		{JSONName: "user", JSONValue: c.User, TextName: "user", TextValue: core.Blank(c.User, "-")},
		{JSONName: "workRoot", JSONValue: c.WorkRoot, TextName: "work_root", TextValue: core.Blank(c.WorkRoot, "-")},
		{JSONName: "releaseAction", JSONValue: c.ReleaseAction, TextName: "release_action", TextValue: core.Blank(c.ReleaseAction, "-")},
		{JSONName: "auth", JSONValue: core.ConfigShowSecretState(c.APIKey), TextName: "auth", TextValue: core.ConfigShowSecretState(c.APIKey)},
	}}
}
