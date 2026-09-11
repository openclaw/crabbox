package aws

import (
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection preserves loaded values and the existing format-only fields.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	return core.ProviderConfigShowSection{
		JSONKey: "aws", TextLabel: "aws", Providers: []string{"aws"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "region", JSONValue: cfg.AWSRegion, TextName: "region", TextValue: cfg.AWSRegion},
			{JSONName: "ami", JSONValue: cfg.AWSAMI},
			{JSONName: "securityGroupId", JSONValue: cfg.AWSSGID},
			{JSONName: "subnetId", JSONValue: cfg.AWSSubnetID},
			{JSONName: "instanceProfile", JSONValue: cfg.AWSProfile},
			{JSONName: "rootGB", JSONValue: cfg.AWSRootGB, TextName: "root_gb", TextValue: strconv.FormatInt(int64(cfg.AWSRootGB), 10)},
			{JSONName: "sshCIDRs", JSONValue: cfg.AWSSSHCIDRs, TextName: "ssh_cidrs", TextValue: core.Blank(strings.Join(cfg.AWSSSHCIDRs, ","), "-")},
		},
	}
}
