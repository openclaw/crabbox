package hostinger

import (
	"fmt"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) NormalizeConfigForShow(cfg core.Config) core.Config {
	cfg.Hostinger.WorkRoot = core.EffectiveHostingerWorkRoot(cfg)
	cfg.WorkRoot = cfg.Hostinger.WorkRoot
	cfg.SSHUser = cfg.Hostinger.User
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = nil
	return cfg
}

func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.Hostinger
	c.WorkRoot = core.EffectiveHostingerWorkRoot(cfg)
	return core.ProviderConfigShowSection{JSONKey: "hostinger", TextLabel: "hostinger", Providers: []string{"hostinger"}, Fields: []core.ProviderConfigShowField{
		{JSONName: "apiUrl", JSONValue: core.ConfigShowURL(c.APIURL), TextName: "api_url", TextValue: core.Blank(core.ConfigShowURL(c.APIURL), "-")},
		{JSONName: "itemId", JSONValue: c.ItemID, TextName: "item_id", TextValue: core.Blank(c.ItemID, "-")},
		{JSONName: "paymentMethodId", JSONValue: c.PaymentMethodID, TextName: "payment_method_id", TextValue: core.Blank(c.PaymentMethodID, "-")},
		{JSONName: "templateId", JSONValue: c.TemplateID, TextName: "template_id", TextValue: core.Blank(c.TemplateID, "-")},
		{JSONName: "dataCenterId", JSONValue: c.DataCenterID, TextName: "data_center_id", TextValue: core.Blank(c.DataCenterID, "-")},
		{JSONName: "hostnamePrefix", JSONValue: c.HostnamePrefix, TextName: "hostname_prefix", TextValue: core.Blank(c.HostnamePrefix, "-")},
		{JSONName: "user", JSONValue: c.User, TextName: "user", TextValue: core.Blank(c.User, "-")},
		{JSONName: "workRoot", JSONValue: c.WorkRoot, TextName: "work_root", TextValue: core.Blank(c.WorkRoot, "-")},
		{JSONName: "allowPurchase", JSONValue: c.AllowPurchase, TextName: "allow_purchase", TextValue: fmt.Sprintf("%t", c.AllowPurchase)},
		{JSONName: "releaseAction", JSONValue: c.ReleaseAction, TextName: "release_action", TextValue: core.Blank(c.ReleaseAction, "-")},
		{JSONName: "auth", JSONValue: core.ConfigShowSecretState(c.APIToken), TextName: "auth", TextValue: core.ConfigShowSecretState(c.APIToken)},
	}}
}
