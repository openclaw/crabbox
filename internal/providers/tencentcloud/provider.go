package tencentcloud

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "tencentcloud"

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return providerName }
func (Provider) Aliases() []string {
	return []string{"tencent", "tencent-cvm", "cvm"}
}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      providerName,
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureTailscale},
		Coordinator: core.CoordinatorNever,
	}
}

type flagValues struct {
	Region                  *string
	Zone                    *string
	Image                   *string
	Type                    *string
	VPCID                   *string
	SubnetID                *string
	SecurityGroupID         *string
	SSHCIDRs                *string
	RootGB                  *int64
	InternetChargeType      *string
	InternetMaxBandwidthOut *int64
	APIEndpoint             *string
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		Region:                  fs.String("tencentcloud-region", defaults.TencentCloud.Region, "Tencent Cloud CVM region"),
		Zone:                    fs.String("tencentcloud-zone", defaults.TencentCloud.Zone, "Tencent Cloud CVM availability zone"),
		Image:                   fs.String("tencentcloud-image", defaults.TencentCloud.Image, "Tencent Cloud CVM image ID"),
		Type:                    fs.String("tencentcloud-type", defaults.TencentCloud.Type, "Tencent Cloud CVM instance type"),
		VPCID:                   fs.String("tencentcloud-vpc-id", defaults.TencentCloud.VPCID, "Tencent Cloud VPC ID"),
		SubnetID:                fs.String("tencentcloud-subnet-id", defaults.TencentCloud.SubnetID, "Tencent Cloud subnet ID"),
		SecurityGroupID:         fs.String("tencentcloud-security-group-id", defaults.TencentCloud.SecurityGroupID, "Tencent Cloud security group ID"),
		SSHCIDRs:                fs.String("tencentcloud-ssh-cidrs", "", "comma-separated Tencent Cloud SSH source CIDRs; reserved for managed security-group support"),
		RootGB:                  fs.Int64("tencentcloud-root-gb", defaults.TencentCloud.RootGB, "Tencent Cloud CVM system disk size in GiB"),
		InternetChargeType:      fs.String("tencentcloud-internet-charge-type", defaults.TencentCloud.InternetChargeType, "Tencent Cloud public bandwidth charge type"),
		InternetMaxBandwidthOut: fs.Int64("tencentcloud-internet-max-bandwidth-out", defaults.TencentCloud.InternetMaxBandwidthOut, "Tencent Cloud public outbound bandwidth in Mbps"),
		APIEndpoint:             fs.String("tencentcloud-api-endpoint", defaults.TencentCloud.APIEndpoint, "Tencent Cloud CVM API endpoint"),
	}
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "tencentcloud-region") {
		cfg.TencentCloud.Region = *v.Region
		core.SetTencentCloudRegionExplicit(cfg)
	}
	if core.FlagWasSet(fs, "tencentcloud-zone") {
		cfg.TencentCloud.Zone = *v.Zone
		core.SetTencentCloudZoneExplicit(cfg)
	}
	if core.FlagWasSet(fs, "tencentcloud-image") {
		cfg.TencentCloud.Image = *v.Image
		core.SetTencentCloudImageExplicit(cfg)
	}
	if core.FlagWasSet(fs, "tencentcloud-type") {
		cfg.TencentCloud.Type = *v.Type
		core.SetTencentCloudTypeExplicit(cfg)
	}
	if core.FlagWasSet(fs, "tencentcloud-vpc-id") {
		cfg.TencentCloud.VPCID = *v.VPCID
	}
	if core.FlagWasSet(fs, "tencentcloud-subnet-id") {
		cfg.TencentCloud.SubnetID = *v.SubnetID
	}
	if core.FlagWasSet(fs, "tencentcloud-security-group-id") {
		cfg.TencentCloud.SecurityGroupID = *v.SecurityGroupID
	}
	if core.FlagWasSet(fs, "tencentcloud-ssh-cidrs") {
		cfg.TencentCloud.SSHCIDRs = splitCommaList(*v.SSHCIDRs)
	}
	if core.FlagWasSet(fs, "tencentcloud-root-gb") {
		cfg.TencentCloud.RootGB = *v.RootGB
	}
	if core.FlagWasSet(fs, "tencentcloud-internet-charge-type") {
		cfg.TencentCloud.InternetChargeType = *v.InternetChargeType
	}
	if core.FlagWasSet(fs, "tencentcloud-internet-max-bandwidth-out") {
		cfg.TencentCloud.InternetMaxBandwidthOut = *v.InternetMaxBandwidthOut
	}
	if core.FlagWasSet(fs, "tencentcloud-api-endpoint") {
		cfg.TencentCloud.APIEndpoint = *v.APIEndpoint
	}
	return nil
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && cfg.ServerType != "" {
		return cfg.ServerType
	}
	if cfg.TencentCloud.Type != "" {
		return cfg.TencentCloud.Type
	}
	return serverTypeForClass(cfg.Class)
}

func (Provider) ServerTypeForClass(class string) string {
	return serverTypeForClass(class)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend := NewBackend(p.Spec(), cfg, rt)
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "tencentcloud doctor backend unavailable")
	}
	return doctor, nil
}

func splitCommaList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
