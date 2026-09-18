package cli

//go:generate go run ../../scripts/configgen -source config_xcpng.go -output config_xcpng_generated.go -type XCPNgConfig -provider xcp-ng

// XCPNgConfig owns mechanical bindings; selector-pair replacement and generic
// connection effects remain explicit policy outside the generated overlays.
type XCPNgConfig struct {
	APIURL       string `config:"apiUrl" env:"CRABBOX_XCP_NG_API_URL" flag:"xcp-ng-api-url" sources:"user,env,flag" help:"XCP-ng pool API URL" fileIgnoreEmpty:"true" fileStorage:"value"`
	Username     string `config:"username" env:"CRABBOX_XCP_NG_USERNAME" flag:"xcp-ng-username" sources:"user,env,flag" help:"XCP-ng API username" fileIgnoreEmpty:"true" fileStorage:"value"`
	Password     string `config:"password" env:"CRABBOX_XCP_NG_PASSWORD" sources:"user,env" fileIgnoreEmpty:"true" fileStorage:"value"`
	Template     string `config:"template" env:"CRABBOX_XCP_NG_TEMPLATE" flag:"xcp-ng-template" sources:"user,repo,env,flag" help:"XCP-ng VM template name" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	TemplateUUID string `config:"templateUuid" env:"CRABBOX_XCP_NG_TEMPLATE_UUID" flag:"xcp-ng-template-uuid" sources:"user,repo,env,flag" help:"XCP-ng VM template UUID" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	SR           string `config:"sr" env:"CRABBOX_XCP_NG_SR" flag:"xcp-ng-sr" sources:"user,repo,env,flag" help:"XCP-ng storage repository name" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	SRUUID       string `config:"srUuid" env:"CRABBOX_XCP_NG_SR_UUID" flag:"xcp-ng-sr-uuid" sources:"user,repo,env,flag" help:"XCP-ng storage repository UUID" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Network      string `config:"network" env:"CRABBOX_XCP_NG_NETWORK" flag:"xcp-ng-network" sources:"user,repo,env,flag" help:"XCP-ng network name" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	NetworkUUID  string `config:"networkUuid" env:"CRABBOX_XCP_NG_NETWORK_UUID" flag:"xcp-ng-network-uuid" sources:"user,repo,env,flag" help:"XCP-ng network UUID" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Host         string `config:"host" env:"CRABBOX_XCP_NG_HOST" flag:"xcp-ng-host" sources:"user,repo,env,flag" help:"XCP-ng host name or UUID" fileIgnoreEmpty:"true" fileStorage:"value"`
	User         string `config:"user" env:"CRABBOX_XCP_NG_USER" flag:"xcp-ng-user" sources:"user,repo,env,flag" help:"cloud-init SSH user for XCP-ng VMs" default:"crabbox" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	WorkRoot     string `config:"workRoot" env:"CRABBOX_XCP_NG_WORK_ROOT" flag:"xcp-ng-work-root" sources:"user,repo,env,flag" help:"remote work root for XCP-ng VMs" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	InsecureTLS  bool   `config:"insecureTLS" env:"CRABBOX_XCP_NG_INSECURE_TLS" flag:"xcp-ng-insecure-tls" sources:"user,env,flag" help:"allow self-signed XCP-ng TLS certificates"`
}

const XCPNgConfigDefaultWorkRoot string = defaultPOSIXWorkRoot

func initialXCPNgConfig() XCPNgConfig {
	cfg := defaultXCPNgConfig()
	cfg.WorkRoot = XCPNgConfigDefaultWorkRoot
	return cfg
}

func applyXCPNgFileConfig(cfg *Config, file *fileXCPNgConfig, trusted bool, source configInputSource) error {
	applied, err := cfg.XCPNg.applyFile(file, trusted)
	cfg.XCPNg.applyLayerPairs(applied)
	recordConfigInput(cfg, "xcp-ng", source, applied.InputAccepted)
	return err
}

func applyXCPNgEnvironmentConfig(cfg *Config) error {
	applied, err := cfg.XCPNg.applyEnv()
	cfg.XCPNg.applyLayerPairs(applied)
	recordConfigInput(cfg, "xcp-ng", configInputEnvironment, applied.InputAccepted)
	return err
}

func (cfg *XCPNgConfig) applyLayerPairs(applied XCPNgConfigApplied) {
	clearXCPNgInheritedCounterpart(&cfg.Template, &cfg.TemplateUUID, applied.Template, applied.TemplateUUID)
	clearXCPNgInheritedCounterpart(&cfg.SR, &cfg.SRUUID, applied.SR, applied.SRUUID)
	clearXCPNgInheritedCounterpart(&cfg.Network, &cfg.NetworkUUID, applied.Network, applied.NetworkUUID)
}

// A nonempty file/env selector replaces its pair; two empty inputs inherit both.
func clearXCPNgInheritedCounterpart(name, uuid *string, nameApplied, uuidApplied bool) {
	if nameApplied && !uuidApplied {
		*uuid = ""
	} else if uuidApplied && !nameApplied {
		*name = ""
	}
}
