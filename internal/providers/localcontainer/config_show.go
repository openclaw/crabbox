package localcontainer

import (
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection preserves the established loaded-value display without defaults.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.LocalContainer
	return core.ProviderConfigShowSection{
		JSONKey: "localContainer", TextLabel: "local_container", Providers: []string{"local-container"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "runtime", JSONValue: c.Runtime, TextName: "runtime", TextValue: c.Runtime},
			{JSONName: "image", JSONValue: c.Image, TextName: "image", TextValue: c.Image},
			{JSONName: "user", JSONValue: c.User, TextName: "user", TextValue: c.User},
			{JSONName: "workRoot", JSONValue: c.WorkRoot, TextName: "work_root", TextValue: core.Blank(c.WorkRoot, "-")},
			{JSONName: "cpus", JSONValue: c.CPUs, TextName: "cpus", TextValue: strconv.Itoa(c.CPUs)},
			{JSONName: "memory", JSONValue: c.Memory, TextName: "memory", TextValue: core.Blank(c.Memory, "-")},
			{JSONName: "network", JSONValue: c.Network, TextName: "network", TextValue: c.Network},
			{JSONName: "dockerSocket", JSONValue: c.DockerSocket, TextName: "docker_socket", TextValue: strconv.FormatBool(c.DockerSocket)},
		},
	}
}
