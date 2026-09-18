package nvidiabrev

import core "github.com/openclaw/crabbox/internal/cli"

func (Provider) NormalizeConfigForShow(cfg core.Config) core.Config {
	cfg.NvidiaBrev.WorkRoot = core.EffectiveNvidiaBrevWorkRoot(cfg)
	cfg.WorkRoot = cfg.NvidiaBrev.WorkRoot
	return cfg
}

func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.NvidiaBrev
	c.WorkRoot = core.EffectiveNvidiaBrevWorkRoot(cfg)
	return core.ProviderConfigShowSection{JSONKey: "nvidiaBrev", TextLabel: "nvidia_brev", Providers: []string{"nvidia-brev"}, Fields: []core.ProviderConfigShowField{
		{JSONName: "cli", JSONValue: c.CLI, TextName: "cli", TextValue: core.Blank(c.CLI, "-")},
		{JSONName: "org", JSONValue: c.Org, TextName: "org", TextValue: core.Blank(c.Org, "-")},
		{JSONName: "type", JSONValue: c.Type, TextName: "type", TextValue: core.Blank(c.Type, "-")},
		{JSONName: "gpuName", JSONValue: c.GPUName, TextName: "gpu_name", TextValue: core.Blank(c.GPUName, "-")},
		{JSONName: "provider", JSONValue: c.Provider, TextName: "provider", TextValue: core.Blank(c.Provider, "-")},
		{JSONName: "mode", JSONValue: c.Mode, TextName: "mode", TextValue: core.Blank(c.Mode, "-")},
		{JSONName: "launchable", JSONValue: c.Launchable, TextName: "launchable", TextValue: core.Blank(c.Launchable, "-")},
		{JSONName: "startupScript", JSONValue: c.StartupScript, TextName: "startup_script", TextValue: core.Blank(c.StartupScript, "-")},
		{JSONName: "releaseAction", JSONValue: c.ReleaseAction, TextName: "release_action", TextValue: core.Blank(c.ReleaseAction, "-")},
		{JSONName: "target", JSONValue: c.Target, TextName: "target", TextValue: core.Blank(c.Target, "-")},
		{JSONName: "user", JSONValue: c.User, TextName: "user", TextValue: core.Blank(c.User, "-")},
		{JSONName: "workRoot", JSONValue: c.WorkRoot, TextName: "work_root", TextValue: core.Blank(c.WorkRoot, "-")},
		{JSONName: "auth", JSONValue: "cli"},
		{TextName: "auth_mode", TextValue: "cli"},
		{TextName: "auth_status", TextValue: "unchecked"},
		{TextName: "readiness", TextValue: "unchecked"},
	}}
}
