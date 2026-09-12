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
			{JSONName: "project", JSONValue: cfg.GCPProject, TextName: "project", TextValue: core.Blank(cfg.GCPProject, "-")},
			{JSONName: "zone", JSONValue: cfg.GCPZone, TextName: "zone", TextValue: cfg.GCPZone},
			{JSONName: "image", JSONValue: cfg.GCPImage, TextName: "image", TextValue: cfg.GCPImage},
			{JSONName: "network", JSONValue: cfg.GCPNetwork, TextName: "network", TextValue: cfg.GCPNetwork},
			{JSONName: "subnet", JSONValue: cfg.GCPSubnet, TextName: "subnet", TextValue: core.Blank(cfg.GCPSubnet, "-")},
			{JSONName: "tags", JSONValue: cfg.GCPTags},
			{JSONName: "rootGB", JSONValue: cfg.GCPRootGB, TextName: "root_gb", TextValue: strconv.FormatInt(cfg.GCPRootGB, 10)},
			{JSONName: "sshCIDRs", JSONValue: cfg.GCPSSHCIDRs, TextName: "ssh_cidrs", TextValue: core.Blank(strings.Join(cfg.GCPSSHCIDRs, ","), "-")},
			{JSONName: "serviceAccount", JSONValue: cfg.GCPServiceAccount},
		},
	}
}
