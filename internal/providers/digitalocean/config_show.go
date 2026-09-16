package digitalocean

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection preserves the loaded provider values without applying defaults.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.DigitalOcean
	return core.ProviderConfigShowSection{
		JSONKey: "digitalocean", TextLabel: "digitalocean", Providers: []string{"digitalocean"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "region", JSONValue: c.Region, TextName: "region", TextValue: c.Region},
			{JSONName: "image", JSONValue: c.Image, TextName: "image", TextValue: c.Image},
			{JSONName: "vpc", JSONValue: c.VPCUUID, TextName: "vpc", TextValue: core.Blank(c.VPCUUID, "-")},
			{JSONName: "sshCIDRs", JSONValue: c.SSHCIDRs, TextName: "ssh_cidrs", TextValue: core.Blank(strings.Join(c.SSHCIDRs, ","), "-")},
		},
	}
}
