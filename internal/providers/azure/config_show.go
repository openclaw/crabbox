package azure

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) NormalizeConfigForShow(cfg core.Config) core.Config {
	if mode, err := core.NormalizeAzureOSDiskMode(cfg.Azure.OSDisk); err == nil {
		cfg.Azure.OSDisk = mode
	}
	return cfg
}

// ConfigShowSection reports the established fields without cloud discovery.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	return core.ProviderConfigShowSection{
		JSONKey: "azure", TextLabel: "azure", Providers: []string{"azure"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "location", JSONValue: cfg.Azure.Location, TextName: "location", TextValue: cfg.Azure.Location},
			{JSONName: "resourceGroup", JSONValue: cfg.Azure.ResourceGroup, TextName: "resource_group", TextValue: cfg.Azure.ResourceGroup},
			{JSONName: "image", JSONValue: cfg.Azure.Image},
			{JSONName: "osDisk", JSONValue: cfg.Azure.OSDisk, TextName: "os_disk", TextValue: cfg.Azure.OSDisk},
			{JSONName: "snapshotSKU", JSONValue: cfg.Azure.SnapshotSKU, TextName: "snapshot_sku", TextValue: core.Blank(cfg.Azure.SnapshotSKU, "-")},
			{JSONName: "osDiskSKU", JSONValue: cfg.Azure.OSDiskSKU, TextName: "os_disk_sku", TextValue: core.Blank(cfg.Azure.OSDiskSKU, "-")},
			{JSONName: "network", JSONValue: cfg.Azure.Network, TextName: "network", TextValue: core.Blank(cfg.Azure.Network, "-")},
			{JSONName: "sshCIDRs", JSONValue: cfg.Azure.SSHCIDRs, TextName: "ssh_cidrs", TextValue: core.Blank(strings.Join(cfg.Azure.SSHCIDRs, ","), "-")},
			{JSONName: "userAssignedIdentityResourceId", JSONValue: cfg.Azure.UserAssignedIdentityResourceID, TextName: "user_assigned_identity_resource_id", TextValue: core.Blank(cfg.Azure.UserAssignedIdentityResourceID, "-")},
		},
	}
}
