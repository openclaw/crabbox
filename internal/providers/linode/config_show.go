package linode

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection reports the existing fields without image or SSH resolution.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.Linode
	return core.ProviderConfigShowSection{
		JSONKey: "linode", TextLabel: "linode", Providers: []string{"linode"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "region", JSONValue: c.Region, TextName: "region", TextValue: c.Region},
			{JSONName: "image", JSONValue: c.Image, TextName: "image", TextValue: c.Image},
			{JSONName: "type", JSONValue: c.Type, TextName: "type", TextValue: c.Type},
			{JSONName: "firewall", JSONValue: c.FirewallID, TextName: "firewall", TextValue: core.Blank(c.FirewallID, "-")},
			{JSONName: "sshCIDRs", JSONValue: c.SSHCIDRs, TextName: "ssh_cidrs", TextValue: core.Blank(strings.Join(c.SSHCIDRs, ","), "-")},
		},
	}
}
