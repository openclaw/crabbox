package dockersandbox

import (
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection projects only the legacy display allowlist without normalization.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.DockerSandbox
	return core.ProviderConfigShowSection{
		JSONKey: "dockerSandbox", TextLabel: "docker_sandbox", Providers: []string{"docker-sandbox"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "cliPath", JSONValue: c.CLIPath, TextName: "cli", TextValue: c.CLIPath},
			{JSONName: "agent", JSONValue: c.Agent, TextName: "agent", TextValue: c.Agent},
			{JSONName: "template", JSONValue: c.Template, TextName: "template", TextValue: core.Blank(c.Template, "-")},
			{JSONName: "cpus", JSONValue: c.CPUs, TextName: "cpus", TextValue: strconv.FormatFloat(c.CPUs, 'g', -1, 64)},
			{JSONName: "memory", JSONValue: c.Memory, TextName: "memory", TextValue: core.Blank(c.Memory, "-")},
			{JSONName: "clone", JSONValue: c.Clone, TextName: "clone", TextValue: strconv.FormatBool(c.Clone)},
			{JSONName: "workdir", JSONValue: c.Workdir, TextName: "workdir", TextValue: core.Blank(c.Workdir, "-")},
			{JSONName: "extraWorkspaces", JSONValue: c.ExtraWorkspaces, TextName: "extra_workspaces", TextValue: core.Blank(strings.Join(c.ExtraWorkspaces, ","), "-")},
			{JSONName: "mcp", JSONValue: c.MCP, TextName: "mcp", TextValue: core.Blank(strings.Join(c.MCP, ","), "-")},
			{JSONName: "kit", JSONValue: c.Kit, TextName: "kit", TextValue: core.Blank(strings.Join(c.Kit, ","), "-")},
		},
	}
}
