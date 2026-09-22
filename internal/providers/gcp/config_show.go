package gcp

import (
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection preserves raw references, numeric width and list shapes.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	return core.ProviderConfigShowSection{
		JSONKey: "gcp", TextLabel: "gcp", Providers: []string{"gcp"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "project", JSONValue: cfg.GCP.Project, TextName: "project", TextValue: core.Blank(cfg.GCP.Project, "-")},
			{JSONName: "zone", JSONValue: cfg.GCP.Zone, TextName: "zone", TextValue: cfg.GCP.Zone},
			{JSONName: "image", JSONValue: cfg.GCP.Image, TextName: "image", TextValue: cfg.GCP.Image},
			{JSONName: "network", JSONValue: cfg.GCP.Network, TextName: "network", TextValue: cfg.GCP.Network},
			{JSONName: "subnet", JSONValue: cfg.GCP.Subnet, TextName: "subnet", TextValue: core.Blank(cfg.GCP.Subnet, "-")},
			{JSONName: "tags", JSONValue: cfg.GCP.Tags},
			{JSONName: "rootGB", JSONValue: cfg.GCP.RootGB, TextName: "root_gb", TextValue: strconv.FormatInt(cfg.GCP.RootGB, 10)},
			{JSONName: "sshCIDRs", JSONValue: cfg.GCP.SSHCIDRs, TextName: "ssh_cidrs", TextValue: core.Blank(strings.Join(cfg.GCP.SSHCIDRs, ","), "-")},
			{JSONName: "serviceAccount", JSONValue: cfg.GCP.ServiceAccount},
		},
	}
}
