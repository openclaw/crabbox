package parallels

import (
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection describes loaded values without resolving hosts or templates.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.Parallels
	return core.ProviderConfigShowSection{
		JSONKey: "parallels", TextLabel: "parallels", Providers: []string{"parallels"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "template", JSONValue: c.Template, TextName: "template", TextValue: core.Blank(c.Template, "-")},
			{JSONName: "source", JSONValue: c.Source, TextName: "source", TextValue: core.Blank(c.Source, "-")},
			{JSONName: "sourceId", JSONValue: c.SourceID, TextName: "source_id", TextValue: core.Blank(c.SourceID, "-")},
			{JSONName: "sourceSnapshot", JSONValue: c.SourceSnapshot, TextName: "snapshot", TextValue: core.Blank(c.SourceSnapshot, "-")},
			{JSONName: "sourceSnapshotId", JSONValue: c.SourceSnapshotID, TextName: "snapshot_id", TextValue: core.Blank(c.SourceSnapshotID, "-")},
			{JSONName: "cloneMode", JSONValue: c.CloneMode, TextName: "clone_mode", TextValue: c.CloneMode},
			{JSONName: "host", JSONValue: c.Host, TextName: "host", TextValue: core.Blank(c.Host, "local")},
			{JSONName: "hostUser", JSONValue: c.HostUser},
			{JSONName: "hostKey", JSONValue: core.ConfigShowSecretState(c.HostKey)},
			{JSONName: "vmRoot", JSONValue: c.VMRoot},
			{JSONName: "user", JSONValue: c.User, TextName: "user", TextValue: c.User},
			{JSONName: "workRoot", JSONValue: c.WorkRoot, TextName: "work_root", TextValue: c.WorkRoot},
			{JSONName: "startupTimeout", JSONValue: c.StartupTimeout.String(), TextName: "startup_timeout", TextValue: c.StartupTimeout.String()},
			{JSONName: "templates", JSONValue: redactedParallelsTemplateConfigs(c.Templates), TextName: "templates", TextValue: strconv.Itoa(len(c.Templates))},
			{JSONName: "hosts", JSONValue: redactedParallelsHostConfigs(c.Hosts), TextName: "hosts", TextValue: strconv.Itoa(len(c.Hosts))},
		},
	}
}

func redactedParallelsTemplateConfigs(templates map[string]core.ParallelsTemplateConfig) map[string]core.ParallelsTemplateConfig {
	if templates == nil {
		return nil
	}
	redacted := make(map[string]core.ParallelsTemplateConfig, len(templates))
	for name, template := range templates {
		template.HostKey = core.ConfigShowSecretState(template.HostKey)
		redacted[name] = template
	}
	return redacted
}

func redactedParallelsHostConfigs(hosts []core.ParallelsHostConfig) []core.ParallelsHostConfig {
	if hosts == nil {
		return nil
	}
	redacted := append(make([]core.ParallelsHostConfig, 0, len(hosts)), hosts...)
	for i := range redacted {
		redacted[i].Key = core.ConfigShowSecretState(redacted[i].Key)
	}
	return redacted
}
