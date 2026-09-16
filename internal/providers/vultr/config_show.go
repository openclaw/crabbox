package vultr

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection keeps independent boot selectors and raw list shapes visible.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.Vultr
	return core.ProviderConfigShowSection{
		JSONKey: "vultr", TextLabel: "vultr", Providers: []string{"vultr"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "region", JSONValue: c.Region, TextName: "region", TextValue: c.Region},
			{JSONName: "os", JSONValue: c.OS, TextName: "os", TextValue: core.Blank(c.OS, "-")},
			{JSONName: "image", JSONValue: c.Image, TextName: "image", TextValue: core.Blank(c.Image, "-")},
			{JSONName: "snapshot", JSONValue: c.Snapshot, TextName: "snapshot", TextValue: core.Blank(c.Snapshot, "-")},
			{JSONName: "firewallGroup", JSONValue: c.FirewallGroup, TextName: "firewall_group", TextValue: core.Blank(c.FirewallGroup, "-")},
			{JSONName: "vpcIds", JSONValue: c.VPCIDs, TextName: "vpc_ids", TextValue: core.Blank(strings.Join(c.VPCIDs, ","), "-")},
			{JSONName: "sshCIDRs", JSONValue: c.SSHCIDRs, TextName: "ssh_cidrs", TextValue: core.Blank(strings.Join(c.SSHCIDRs, ","), "-")},
			{JSONName: "userScheme", JSONValue: c.UserScheme, TextName: "user_scheme", TextValue: core.Blank(c.UserScheme, "-")},
		},
	}
}
