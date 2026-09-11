package hostinger

import core "github.com/openclaw/crabbox/internal/cli"

import "flag"

type hostingerFlagValues struct {
	APIURL          *string
	ItemID          *string
	PaymentMethodID *string
	TemplateID      *string
	DataCenterID    *string
	HostnamePrefix  *string
	User            *string
	WorkRoot        *string
	AllowPurchase   *bool
	ReleaseAction   *string
}

func RegisterHostingerProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return hostingerFlagValues{
		APIURL:          fs.String("hostinger-url", defaults.Hostinger.APIURL, "Hostinger API URL"),
		ItemID:          fs.String("hostinger-item-id", defaults.Hostinger.ItemID, "Hostinger priced item ID to purchase, e.g. hostingercom-vps-kvm2-usd-1m"),
		PaymentMethodID: fs.String("hostinger-payment-method-id", defaults.Hostinger.PaymentMethodID, "Hostinger billing payment method ID; defaults to the sole active default method"),
		TemplateID:      fs.String("hostinger-template-id", defaults.Hostinger.TemplateID, "Hostinger VPS template ID"),
		DataCenterID:    fs.String("hostinger-data-center-id", defaults.Hostinger.DataCenterID, "Hostinger VPS data center ID"),
		HostnamePrefix:  fs.String("hostinger-hostname-prefix", defaults.Hostinger.HostnamePrefix, "hostname prefix for Crabbox Hostinger VMs"),
		User:            fs.String("hostinger-user", defaults.Hostinger.User, "SSH user for Hostinger VPS leases"),
		WorkRoot:        fs.String("hostinger-work-root", defaults.Hostinger.WorkRoot, "remote Crabbox work root on Hostinger VPS leases"),
		AllowPurchase:   fs.Bool("hostinger-allow-purchase", defaults.Hostinger.AllowPurchase, "allow Hostinger billable VPS purchase/setup operations"),
		ReleaseAction:   fs.String("hostinger-release-action", defaults.Hostinger.ReleaseAction, "Hostinger release action: stop"),
	}
}

func ApplyHostingerProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(hostingerFlagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "hostinger-url") {
		cfg.Hostinger.APIURL = *v.APIURL
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if core.FlagWasSet(fs, "hostinger-item-id") {
		cfg.Hostinger.ItemID = *v.ItemID
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if core.FlagWasSet(fs, "hostinger-payment-method-id") {
		cfg.Hostinger.PaymentMethodID = *v.PaymentMethodID
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if core.FlagWasSet(fs, "hostinger-template-id") {
		cfg.Hostinger.TemplateID = *v.TemplateID
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if core.FlagWasSet(fs, "hostinger-data-center-id") {
		cfg.Hostinger.DataCenterID = *v.DataCenterID
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if core.FlagWasSet(fs, "hostinger-hostname-prefix") {
		cfg.Hostinger.HostnamePrefix = *v.HostnamePrefix
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if core.FlagWasSet(fs, "hostinger-user") {
		cfg.Hostinger.User = *v.User
		core.RecordProviderFlagInputs(cfg, true, providerName)
		cfg.SSHUser = *v.User
		markHostingerUserExplicit(cfg)
	}
	if core.FlagWasSet(fs, "hostinger-work-root") {
		cfg.Hostinger.WorkRoot = *v.WorkRoot
		core.RecordProviderFlagInputs(cfg, true, providerName)
		markHostingerWorkRootExplicit(cfg)
	}
	if core.FlagWasSet(fs, "hostinger-allow-purchase") {
		cfg.Hostinger.AllowPurchase = *v.AllowPurchase
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if core.FlagWasSet(fs, "hostinger-release-action") {
		cfg.Hostinger.ReleaseAction = *v.ReleaseAction
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if cfg.Provider == providerName {
		applyDefaults(cfg)
	}
	return nil
}
