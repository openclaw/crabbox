package cli

import (
	"fmt"
	"os"
	"strings"
)

const (
	AzureBackendVM              = "vm"
	AzureBackendDynamicSessions = "dynamic-sessions"
)

func NormalizeAzureBackend(backend string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "", "vm", "vms", "virtual-machine", "virtual-machines":
		return AzureBackendVM, nil
	case "dynamic-sessions", "dynamic-session", "sessions", "azds":
		return AzureBackendDynamicSessions, nil
	default:
		return "", fmt.Errorf("azure backend must be vm or dynamic-sessions")
	}
}

// AzureConfig owns Azure inputs shared by VM and dynamic-session routing.
// Source admission and explicit-input markers stay beside their bindings.
type AzureConfig struct {
	Subscription   string
	Tenant         string
	ClientID       string
	Location       string
	Backend        string
	ResourceGroup  string
	Image          string
	imageExplicit  bool
	Snapshot       string
	SnapshotSKU    string
	OSDisk         string
	OSDiskExplicit bool
	OSDiskSKU      string
	VNet           string
	Subnet         string
	NSG            string
	SSHCIDRs       []string
	Network        string

	UserAssignedIdentityResourceID string
}

func initialAzureConfig(image string) AzureConfig {
	return AzureConfig{
		Backend:       "vm",
		Location:      "eastus",
		ResourceGroup: "crabbox-leases",
		Image:         image,
		OSDisk:        AzureOSDiskManaged,
		VNet:          "crabbox-vnet",
		Subnet:        "crabbox-subnet",
		NSG:           "crabbox-nsg",
	}
}

func (cfg *AzureConfig) applyOSImageDefault(image, baseImage string, force, wasOSDefault bool) {
	if force || cfg.Image == "" || (!cfg.imageExplicit && (cfg.Image == baseImage || wasOSDefault)) {
		cfg.Image = image
	}
}

func addCoordinatorAzureFields(req map[string]any, cfg Config) {
	// These two legacy wire fields are present even when another provider is selected.
	req["azureLocation"] = cfg.Azure.Location
	req["azureSnapshot"] = cfg.Azure.Snapshot
	if cfg.Azure.imageExplicit {
		req["azureImage"] = cfg.Azure.Image
	}
	if cfg.Azure.OSDiskExplicit {
		req["azureOSDisk"] = cfg.Azure.OSDisk
	}
}

type fileAzureConfig struct {
	SubscriptionID string   `yaml:"subscriptionId,omitempty"`
	TenantID       string   `yaml:"tenantId,omitempty"`
	ClientID       string   `yaml:"clientId,omitempty"`
	Backend        string   `yaml:"backend,omitempty"`
	Location       string   `yaml:"location,omitempty"`
	ResourceGroup  string   `yaml:"resourceGroup,omitempty"`
	Image          string   `yaml:"image,omitempty"`
	OSDisk         string   `yaml:"osDisk,omitempty"`
	SnapshotSKU    string   `yaml:"snapshotSKU,omitempty"`
	OSDiskSKU      string   `yaml:"osDiskSKU,omitempty"`
	VNet           string   `yaml:"vnet,omitempty"`
	Subnet         string   `yaml:"subnet,omitempty"`
	NSG            string   `yaml:"nsg,omitempty"`
	SSHCIDRs       []string `yaml:"sshCIDRs,omitempty"`
	Network        string   `yaml:"network,omitempty"`

	UserAssignedIdentityResourceID string `yaml:"userAssignedIdentityResourceId,omitempty"`
}

func (cfg *Config) applyAzureFileConfig(file *fileAzureConfig, inputSource configInputSource) {
	if file != nil {
		configInputFileString(cfg, "azure", inputSource, &cfg.Azure.Backend, file.Backend)
		if file.SubscriptionID != "" {
			cfg.Azure.Subscription = file.SubscriptionID
			recordConfigInput(cfg, "azure", inputSource, true)
			recordConfigInput(cfg, "azure-dynamic-sessions", inputSource, true)
		}
		if file.TenantID != "" {
			cfg.Azure.Tenant = file.TenantID
			recordConfigInput(cfg, "azure", inputSource, true)
			recordConfigInput(cfg, "azure-dynamic-sessions", inputSource, true)
		}
		configInputFileString(cfg, "azure", inputSource, &cfg.Azure.ClientID, file.ClientID)
		configInputFileString(cfg, "azure", inputSource, &cfg.Azure.UserAssignedIdentityResourceID, file.UserAssignedIdentityResourceID)
		configInputFileString(cfg, "azure", inputSource, &cfg.Azure.Location, file.Location)
		configInputFileString(cfg, "azure", inputSource, &cfg.Azure.ResourceGroup, file.ResourceGroup)
		if file.Image != "" {
			cfg.Azure.Image = file.Image
			recordConfigInput(cfg, "azure", inputSource, true)
			cfg.Azure.imageExplicit = true
		}
		if file.OSDisk != "" {
			cfg.Azure.OSDisk = file.OSDisk
			recordConfigInput(cfg, "azure", inputSource, true)
			cfg.Azure.OSDiskExplicit = true
		}
		configInputFileString(cfg, "azure", inputSource, &cfg.Azure.SnapshotSKU, file.SnapshotSKU)
		configInputFileString(cfg, "azure", inputSource, &cfg.Azure.OSDiskSKU, file.OSDiskSKU)
		configInputFileString(cfg, "azure", inputSource, &cfg.Azure.VNet, file.VNet)
		configInputFileString(cfg, "azure", inputSource, &cfg.Azure.Subnet, file.Subnet)
		configInputFileString(cfg, "azure", inputSource, &cfg.Azure.NSG, file.NSG)
		if len(file.SSHCIDRs) > 0 {
			cfg.Azure.SSHCIDRs = file.SSHCIDRs
			recordConfigInput(cfg, "azure", inputSource, true)
		}
		configInputFileString(cfg, "azure", inputSource, &cfg.Azure.Network, file.Network)
	}
}

func (cfg *Config) applyAzureEnvironment() {
	if value, accepted := firstNonEmptyEnv("CRABBOX_AZURE_SUBSCRIPTION_ID", "AZURE_SUBSCRIPTION_ID"); accepted {
		cfg.Azure.Subscription = value
		recordConfigInput(cfg, "azure", configInputEnvironment, true)
		recordConfigInput(cfg, "azure-dynamic-sessions", configInputEnvironment, true)
	}
	if value, accepted := firstNonEmptyEnv("CRABBOX_AZURE_TENANT_ID", "AZURE_TENANT_ID"); accepted {
		cfg.Azure.Tenant = value
		recordConfigInput(cfg, "azure", configInputEnvironment, true)
		recordConfigInput(cfg, "azure-dynamic-sessions", configInputEnvironment, true)
	}
	cfg.Azure.ClientID = configInputEnvString(cfg, "azure", cfg.Azure.ClientID, "CRABBOX_AZURE_CLIENT_ID", "AZURE_CLIENT_ID")
	cfg.Azure.UserAssignedIdentityResourceID = configInputEnvString(cfg, "azure", cfg.Azure.UserAssignedIdentityResourceID, "CRABBOX_AZURE_USER_ASSIGNED_IDENTITY_RESOURCE_ID")
	cfg.Azure.Backend = configInputEnvString(cfg, "azure", cfg.Azure.Backend, "CRABBOX_AZURE_BACKEND")
	cfg.Azure.Location = configInputEnvString(cfg, "azure", cfg.Azure.Location, "CRABBOX_AZURE_LOCATION")
	cfg.Azure.ResourceGroup = configInputEnvString(cfg, "azure", cfg.Azure.ResourceGroup, "CRABBOX_AZURE_RESOURCE_GROUP")
	if image := os.Getenv("CRABBOX_AZURE_IMAGE"); image != "" {
		cfg.Azure.Image = image
		recordConfigInput(cfg, "azure", configInputEnvironment, true)
		cfg.Azure.imageExplicit = true
	}
	if value := os.Getenv("CRABBOX_AZURE_OS_DISK"); value != "" {
		cfg.Azure.OSDisk = value
		recordConfigInput(cfg, "azure", configInputEnvironment, true)
		cfg.Azure.OSDiskExplicit = true
	}
	cfg.Azure.SnapshotSKU = configInputEnvString(cfg, "azure", cfg.Azure.SnapshotSKU, "CRABBOX_AZURE_SNAPSHOT_SKU")
	cfg.Azure.OSDiskSKU = configInputEnvString(cfg, "azure", cfg.Azure.OSDiskSKU, "CRABBOX_AZURE_OS_DISK_SKU")
	cfg.Azure.VNet = configInputEnvString(cfg, "azure", cfg.Azure.VNet, "CRABBOX_AZURE_VNET")
	cfg.Azure.Subnet = configInputEnvString(cfg, "azure", cfg.Azure.Subnet, "CRABBOX_AZURE_SUBNET")
	cfg.Azure.NSG = configInputEnvString(cfg, "azure", cfg.Azure.NSG, "CRABBOX_AZURE_NSG")
	if cidrs := os.Getenv("CRABBOX_AZURE_SSH_CIDRS"); cidrs != "" {
		cfg.Azure.SSHCIDRs = splitCommaList(cidrs)
		recordConfigInput(cfg, "azure", configInputEnvironment, true)
	}
	cfg.Azure.Network = configInputEnvString(cfg, "azure", cfg.Azure.Network, "CRABBOX_AZURE_NETWORK")
}
