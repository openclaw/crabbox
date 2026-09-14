package azure

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection reports the established fields without cloud discovery.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	return core.ProviderConfigShowSection{
		JSONKey: "azure", TextLabel: "azure", Providers: []string{"azure"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "location", JSONValue: cfg.AzureLocation, TextName: "location", TextValue: cfg.AzureLocation},
			{JSONName: "resourceGroup", JSONValue: cfg.AzureResourceGroup, TextName: "resource_group", TextValue: cfg.AzureResourceGroup},
			{JSONName: "image", JSONValue: cfg.AzureImage},
			{JSONName: "osDisk", JSONValue: cfg.AzureOSDisk, TextName: "os_disk", TextValue: cfg.AzureOSDisk},
			{JSONName: "snapshotSKU", JSONValue: cfg.AzureSnapshotSKU, TextName: "snapshot_sku", TextValue: core.Blank(cfg.AzureSnapshotSKU, "-")},
			{JSONName: "osDiskSKU", JSONValue: cfg.AzureOSDiskSKU, TextName: "os_disk_sku", TextValue: core.Blank(cfg.AzureOSDiskSKU, "-")},
			{JSONName: "network", JSONValue: cfg.AzureNetwork, TextName: "network", TextValue: core.Blank(cfg.AzureNetwork, "-")},
			{JSONName: "sshCIDRs", JSONValue: cfg.AzureSSHCIDRs, TextName: "ssh_cidrs", TextValue: core.Blank(strings.Join(cfg.AzureSSHCIDRs, ","), "-")},
		},
	}
}
