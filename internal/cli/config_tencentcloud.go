package cli

//go:generate go run ../../scripts/configgen -source config_tencentcloud.go -output config_tencentcloud_generated.go -type TencentCloudConfig -provider tencentcloud

// Runtime fallbacks do not initialize the raw configuration or flag defaults.
const (
	TencentCloudRegionFallback                        = "ap-shanghai"
	TencentCloudZoneFallback                          = "ap-shanghai-2"
	TencentCloudTypeFallback                          = "SA5.MEDIUM2"
	TencentCloudRootGBFallback                  int64 = 50
	TencentCloudInternetChargeTypeFallback            = "TRAFFIC_POSTPAID_BY_HOUR"
	TencentCloudInternetMaxBandwidthOutFallback int64 = 5
	TencentCloudAPIEndpointFallback                   = "https://cvm.tencentcloudapi.com"
)

// TencentCloudConfig contains non-secret CVM settings. Native credentials,
// endpoint families and class selection remain provider policy.
type TencentCloudConfig struct {
	Region                  string   `config:"region" env:"CRABBOX_TENCENTCLOUD_REGION" flag:"tencentcloud-region" sources:"user,repo,env,flag" help:"Tencent Cloud CVM region" fileIgnoreEmpty:"true" reportApplied:"true" fileStorage:"value"`
	Zone                    string   `config:"zone" env:"CRABBOX_TENCENTCLOUD_ZONE" flag:"tencentcloud-zone" sources:"user,repo,env,flag" help:"Tencent Cloud CVM availability zone" fileIgnoreEmpty:"true" reportApplied:"true" fileStorage:"value"`
	Image                   string   `config:"image" env:"CRABBOX_TENCENTCLOUD_IMAGE" flag:"tencentcloud-image" sources:"user,repo,env,flag" help:"Tencent Cloud CVM image ID" fileIgnoreEmpty:"true" reportApplied:"true" fileStorage:"value"`
	Type                    string   `config:"type" env:"CRABBOX_TENCENTCLOUD_TYPE" flag:"tencentcloud-type" sources:"user,repo,env,flag" help:"Tencent Cloud CVM instance type" fileIgnoreEmpty:"true" reportApplied:"true" fileStorage:"value"`
	VPCID                   string   `config:"vpcId" env:"CRABBOX_TENCENTCLOUD_VPC_ID" flag:"tencentcloud-vpc-id" sources:"user,repo,env,flag" help:"Tencent Cloud VPC ID" fileIgnoreEmpty:"true" fileStorage:"value"`
	SubnetID                string   `config:"subnetId" env:"CRABBOX_TENCENTCLOUD_SUBNET_ID" flag:"tencentcloud-subnet-id" sources:"user,repo,env,flag" help:"Tencent Cloud subnet ID" fileIgnoreEmpty:"true" fileStorage:"value"`
	SecurityGroupID         string   `config:"securityGroupId" env:"CRABBOX_TENCENTCLOUD_SECURITY_GROUP_ID" flag:"tencentcloud-security-group-id" sources:"user,repo,env,flag" help:"Tencent Cloud security group ID" fileIgnoreEmpty:"true" fileStorage:"value"`
	SSHCIDRs                []string `config:"sshCIDRs" env:"CRABBOX_TENCENTCLOUD_SSH_CIDRS" flag:"tencentcloud-ssh-cidrs" sources:"user,repo,env,flag" help:"comma-separated Tencent Cloud SSH source CIDRs; reserved for managed security-group support" fileList:"nonempty-raw" flagList:"empty-scalar" fileStorage:"value"`
	RootGB                  int64    `config:"rootGB" env:"CRABBOX_TENCENTCLOUD_ROOT_GB" flag:"tencentcloud-root-gb" sources:"user,repo,env,flag" help:"Tencent Cloud CVM system disk size in GiB" nonnegative:"true" fileInt:"positive" envInt:"fallback" fileStorage:"value"`
	InternetChargeType      string   `config:"internetChargeType" env:"CRABBOX_TENCENTCLOUD_INTERNET_CHARGE_TYPE" flag:"tencentcloud-internet-charge-type" sources:"user,repo,env,flag" help:"Tencent Cloud public bandwidth charge type" fileIgnoreEmpty:"true" fileStorage:"value"`
	InternetMaxBandwidthOut int64    `config:"internetMaxBandwidthOut" env:"CRABBOX_TENCENTCLOUD_INTERNET_MAX_BANDWIDTH_OUT" flag:"tencentcloud-internet-max-bandwidth-out" sources:"user,repo,env,flag" help:"Tencent Cloud public outbound bandwidth in Mbps" nonnegative:"true" fileInt:"positive" envInt:"fallback" fileStorage:"value"`
	APIEndpoint             string   `config:"apiEndpoint" env:"CRABBOX_TENCENTCLOUD_API_ENDPOINT" flag:"tencentcloud-api-endpoint" sources:"user,env,flag" help:"Tencent Cloud CVM API endpoint" fileIgnoreEmpty:"true" fileStorage:"value"`
}

// MarkTencentCloudConfigApplied records accepted source events without
// inferring explicitness from values or changing class precedence.
func MarkTencentCloudConfigApplied(cfg *Config, applied TencentCloudConfigApplied) {
	if applied.Region {
		SetTencentCloudRegionExplicit(cfg)
	}
	if applied.Zone {
		SetTencentCloudZoneExplicit(cfg)
	}
	if applied.Image {
		SetTencentCloudImageExplicit(cfg)
	}
	if applied.Type {
		SetTencentCloudTypeExplicit(cfg)
	}
}
