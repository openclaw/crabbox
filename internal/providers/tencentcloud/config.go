package tencentcloud

import (
	"net/url"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	defaultRegion                  = "ap-shanghai"
	defaultZone                    = "ap-shanghai-2"
	defaultType                    = "SA5.MEDIUM2"
	defaultRootGB                  = int64(50)
	defaultInternetChargeType      = "TRAFFIC_POSTPAID_BY_HOUR"
	defaultInternetMaxBandwidthOut = int64(5)
	defaultCVMEndpoint             = "https://cvm.tencentcloudapi.com"
	defaultTagEndpoint             = "https://tag.tencentcloudapi.com"
	defaultSTSEndpoint             = "https://sts.tencentcloudapi.com"
)

func cfgForRun(cfg core.Config) core.Config {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = core.TargetLinux
	}
	if cfg.TencentCloud.Region == "" {
		cfg.TencentCloud.Region = defaultRegion
	}
	if cfg.TencentCloud.Zone == "" {
		cfg.TencentCloud.Zone = defaultZone
	}
	resolvedType := serverTypeForConfig(cfg)
	cfg.TencentCloud.Type = resolvedType
	if cfg.TencentCloud.RootGB == 0 {
		cfg.TencentCloud.RootGB = defaultRootGB
	}
	if cfg.TencentCloud.InternetChargeType == "" {
		cfg.TencentCloud.InternetChargeType = defaultInternetChargeType
	}
	if cfg.TencentCloud.InternetMaxBandwidthOut == 0 {
		cfg.TencentCloud.InternetMaxBandwidthOut = defaultInternetMaxBandwidthOut
	}
	if !core.IsSSHUserExplicit(&cfg) && (cfg.SSHUser == "" || cfg.SSHUser == core.BaseConfig().SSHUser) {
		cfg.SSHUser = "ubuntu"
	}
	if !core.IsSSHPortExplicit(&cfg) && (cfg.SSHPort == "" || cfg.SSHPort == core.BaseConfig().SSHPort) {
		cfg.SSHPort = "22"
	}
	cfg.SSHFallbackPorts = nil
	cfg.ServerType = resolvedType
	return cfg
}

func regionForConfig(cfg core.Config) string {
	if value := strings.TrimSpace(cfg.TencentCloud.Region); value != "" {
		return value
	}
	return defaultRegion
}

func zoneForConfig(cfg core.Config) string {
	if value := strings.TrimSpace(cfg.TencentCloud.Zone); value != "" {
		return value
	}
	return defaultZone
}

func imageForConfig(cfg core.Config) string {
	return strings.TrimSpace(cfg.TencentCloud.Image)
}

func serverTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" {
		return strings.TrimSpace(cfg.ServerType)
	}
	if value := strings.TrimSpace(cfg.TencentCloud.Type); core.TencentCloudTypeWasExplicit(cfg) && value != "" {
		return value
	}
	if core.ClassWasExplicit(cfg) {
		if candidates, matched := core.ProviderClassCandidatesForProfiles(classProfiles, cfg); matched {
			return candidates[0]
		}
		if core.IsCanonicalProviderClass(cfg.Class) {
			return ""
		}
		return serverTypeForClass(cfg.Class)
	}
	if value := strings.TrimSpace(cfg.TencentCloud.Type); value != "" {
		return value
	}
	return defaultType
}

func serverTypeForClass(class string) string {
	if serverType, ok := providerClassType(class); ok {
		return serverType
	}
	normalized := strings.ToLower(strings.TrimSpace(class))
	if normalized != class {
		if serverType, ok := providerClassType(normalized); ok {
			return serverType
		}
	}
	return defaultType
}

func providerClassType(class string) (string, bool) {
	candidates, ok := core.ProviderClassCandidatesForProfiles(classProfiles, core.Config{
		Provider: providerName, TargetOS: core.TargetLinux, Architecture: core.ArchitectureAMD64, Class: class,
	})
	if !ok {
		return "", false
	}
	return candidates[0], true
}

func validateAcquireConfig(cfg core.Config) error {
	if _, err := tencentCloudChargeType(cfg); err != nil {
		return err
	}
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return core.Exit(2, "provider=tencentcloud only supports target=linux")
	}
	if strings.TrimSpace(regionForConfig(cfg)) == "" {
		return core.Exit(2, "tencentcloud region is required")
	}
	if strings.TrimSpace(zoneForConfig(cfg)) == "" {
		return core.Exit(2, "tencentcloud zone is required")
	}
	if strings.TrimSpace(imageForConfig(cfg)) == "" {
		return core.Exit(2, "tencentcloud image is required; set tencentcloud.image, --tencentcloud-image, or CRABBOX_TENCENTCLOUD_IMAGE to a CVM image ID")
	}
	if strings.TrimSpace(serverTypeForConfig(cfg)) == "" {
		return core.Exit(2, "tencentcloud type is required")
	}
	if cfg.TencentCloud.RootGB < 0 {
		return core.Exit(2, "tencentcloud rootGB must be non-negative")
	}
	if cfg.TencentCloud.InternetMaxBandwidthOut < 0 {
		return core.Exit(2, "tencentcloud internetMaxBandwidthOut must be non-negative")
	}
	if len(cfg.TencentCloud.SSHCIDRs) > 0 {
		return core.Exit(2, "provider=tencentcloud does not yet manage security-group SSH CIDRs; attach a preconfigured tencentcloud.securityGroupId or remove tencentcloud.sshCIDRs")
	}
	return nil
}

func cvmEndpointForConfig(cfg core.Config) string {
	if value := strings.TrimSpace(cfg.TencentCloud.APIEndpoint); value != "" {
		return normalizeEndpoint(value)
	}
	return defaultCVMEndpoint
}

func tagEndpointForConfig(cfg core.Config) string {
	cvm := cvmEndpointForConfig(cfg)
	if strings.Contains(cvm, ".intl.tencentcloudapi.com") {
		return "https://tag.intl.tencentcloudapi.com"
	}
	return defaultTagEndpoint
}

func stsEndpointForConfig(cfg core.Config) string {
	cvm := cvmEndpointForConfig(cfg)
	if strings.Contains(cvm, ".intl.tencentcloudapi.com") {
		return "https://sts.intl.tencentcloudapi.com"
	}
	return defaultSTSEndpoint
}

func normalizeEndpoint(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return value
	}
	return "https://" + strings.TrimPrefix(value, "//")
}
