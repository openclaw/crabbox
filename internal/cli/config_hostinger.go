package cli

//go:generate go run ../../scripts/configgen -source config_hostinger.go -output config_hostinger_generated.go -type HostingerConfig -provider hostinger

// HostingerConfig owns mechanical bindings; conditional file admission and
// generic connection effects remain in policy wrappers.
type HostingerConfig struct {
	APIToken        string `config:"apiToken" env:"CRABBOX_HOSTINGER_API_TOKEN" envAlias:"HOSTINGER_API_TOKEN" sources:"user,env" fileIgnoreEmpty:"true" fileStorage:"value"`
	APIURL          string `config:"apiUrl" env:"CRABBOX_HOSTINGER_API_URL" envAlias:"HOSTINGER_API_URL" flag:"hostinger-url" sources:"user,env,flag" help:"Hostinger API URL" default:"https://developers.hostinger.com" fileIgnoreEmpty:"true" fileStorage:"value"`
	ItemID          string `config:"itemId" env:"CRABBOX_HOSTINGER_ITEM_ID" flag:"hostinger-item-id" sources:"user,env,flag" help:"Hostinger priced item ID to purchase, e.g. hostingercom-vps-kvm2-usd-1m" fileIgnoreEmpty:"true" fileStorage:"value"`
	PaymentMethodID string `config:"paymentMethodId" env:"CRABBOX_HOSTINGER_PAYMENT_METHOD_ID" flag:"hostinger-payment-method-id" sources:"user,env,flag" help:"Hostinger billing payment method ID; defaults to the sole active default method" fileIgnoreEmpty:"true" fileStorage:"value"`
	TemplateID      string `config:"templateId" env:"CRABBOX_HOSTINGER_TEMPLATE_ID" flag:"hostinger-template-id" sources:"user,env,flag" help:"Hostinger VPS template ID" fileIgnoreEmpty:"true" fileStorage:"value"`
	DataCenterID    string `config:"dataCenterId" env:"CRABBOX_HOSTINGER_DATA_CENTER_ID" flag:"hostinger-data-center-id" sources:"user,env,flag" help:"Hostinger VPS data center ID" fileIgnoreEmpty:"true" fileStorage:"value"`
	HostnamePrefix  string `config:"hostnamePrefix" env:"CRABBOX_HOSTINGER_HOSTNAME_PREFIX" flag:"hostinger-hostname-prefix" sources:"user,repo,env,flag" help:"hostname prefix for Crabbox Hostinger VMs" default:"crabbox" fileIgnoreEmpty:"true" fileStorage:"value"`
	User            string `config:"user" env:"CRABBOX_HOSTINGER_USER" flag:"hostinger-user" sources:"user,repo,env,flag" help:"SSH user for Hostinger VPS leases" default:"root" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	WorkRoot        string `config:"workRoot" env:"CRABBOX_HOSTINGER_WORK_ROOT" flag:"hostinger-work-root" sources:"user,repo,env,flag" help:"remote Crabbox work root on Hostinger VPS leases" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	AllowPurchase   bool   `config:"allowPurchase" env:"CRABBOX_HOSTINGER_ALLOW_PURCHASE" flag:"hostinger-allow-purchase" sources:"user,repo,env,flag" help:"allow Hostinger billable VPS purchase/setup operations"`
	ReleaseAction   string `config:"releaseAction" env:"CRABBOX_HOSTINGER_RELEASE_ACTION" flag:"hostinger-release-action" sources:"user,repo,env,flag" help:"Hostinger release action: stop" default:"stop" fileIgnoreEmpty:"true" fileStorage:"value"`
}

func applyHostingerFileConfig(cfg *Config, file *fileHostingerConfig, trusted bool, source configInputSource) error {
	if file == nil {
		return nil
	}
	snapshot := *file
	if !trusted && snapshot.AllowPurchase != nil && *snapshot.AllowPurchase {
		snapshot.AllowPurchase = nil
	}
	applied, err := cfg.Hostinger.applyFile(&snapshot, trusted)
	recordConfigInput(cfg, "hostinger", source, applied.InputAccepted)
	if applied.User {
		MarkHostingerUserExplicit(cfg)
	}
	if applied.WorkRoot {
		MarkHostingerWorkRootExplicit(cfg)
	}
	return err
}

func applyHostingerEnvironmentConfig(cfg *Config) error {
	applied, err := cfg.Hostinger.applyEnv()
	recordConfigInput(cfg, "hostinger", configInputEnvironment, applied.InputAccepted)
	if applied.User {
		MarkHostingerUserExplicit(cfg)
	}
	if applied.WorkRoot {
		MarkHostingerWorkRootExplicit(cfg)
	}
	return err
}
