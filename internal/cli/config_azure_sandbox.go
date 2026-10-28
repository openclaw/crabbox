package cli

//go:generate go run ../../scripts/configgen -source config_azure_sandbox.go -output config_azure_sandbox_generated.go -type AzureSandboxConfig -provider azure-sandbox

// AzureSandboxConfig routes to an existing ACA Sandbox group. Group creation,
// worker identities and RBAC remain the infrastructure owner's responsibility.
type AzureSandboxConfig struct {
	Retention          string `json:",omitempty" config:"retention" env:"CRABBOX_AZURE_SANDBOX_RETENTION" flag:"azure-sandbox-retention" sources:"user,env,flag" help:"Opt-in session-disk retention until explicit release"`
	OwnerBinding       string `json:",omitempty" config:"ownerBinding" env:"CRABBOX_AZURE_SANDBOX_OWNER_BINDING" flag:"azure-sandbox-owner-binding" sources:"user,env,flag" help:"Immutable opaque session/environment SHA256 binding"`
	Subscription       string `config:"subscriptionId" env:"CRABBOX_AZURE_SANDBOX_SUBSCRIPTION_ID" flag:"azure-sandbox-subscription-id" sources:"user,env,flag" help:"ACA Sandbox subscription ID" fileIgnoreEmpty:"true" fileStorage:"value"`
	ResourceGroup      string `config:"resourceGroup" env:"CRABBOX_AZURE_SANDBOX_RESOURCE_GROUP" flag:"azure-sandbox-resource-group" sources:"user,env,flag" help:"ACA Sandbox resource group" fileIgnoreEmpty:"true" fileStorage:"value"`
	Group              string `config:"group" env:"CRABBOX_AZURE_SANDBOX_GROUP" flag:"azure-sandbox-group" sources:"user,env,flag" help:"Existing ACA Sandbox group" fileIgnoreEmpty:"true" fileStorage:"value"`
	Region             string `config:"region" env:"CRABBOX_AZURE_SANDBOX_REGION" flag:"azure-sandbox-region" sources:"user,env,flag" help:"ACA Sandbox group region" fileIgnoreEmpty:"true" fileStorage:"value"`
	ClientID           string `config:"clientId" env:"CRABBOX_AZURE_SANDBOX_CLIENT_ID" flag:"azure-sandbox-client-id" sources:"user,env,flag" help:"Allocator managed identity client ID" fileIgnoreEmpty:"true" fileStorage:"value"`
	Disk               string `config:"disk" env:"CRABBOX_AZURE_SANDBOX_DISK" flag:"azure-sandbox-disk" sources:"user,repo,env,flag" help:"Public ACA disk image" default:"ubuntu" fileIgnoreEmpty:"true" fileStorage:"value"`
	DiskID             string `config:"diskId" env:"CRABBOX_AZURE_SANDBOX_DISK_ID" flag:"azure-sandbox-disk-id" sources:"user,env,flag" help:"Prepared private ACA disk image ID in the configured group" fileIgnoreEmpty:"true" fileStorage:"value"`
	SnapshotID         string `json:",omitempty" config:"snapshotId" env:"CRABBOX_AZURE_SANDBOX_SNAPSHOT_ID" flag:"azure-sandbox-snapshot-id" sources:"user,env,flag" help:"Opt-in exact prepared snapshot ID" fileIgnoreEmpty:"true" fileStorage:"value"`
	SnapshotReceipt    string `json:",omitempty" config:"snapshotReceipt" env:"CRABBOX_AZURE_SANDBOX_SNAPSHOT_RECEIPT" flag:"azure-sandbox-snapshot-receipt" sources:"user,env,flag" help:"Trusted prepared snapshot binding receipt file" fileIgnoreEmpty:"true" fileStorage:"value"`
	SnapshotGeneration string `json:",omitempty" config:"snapshotGeneration" env:"CRABBOX_AZURE_SANDBOX_SNAPSHOT_GENERATION" flag:"azure-sandbox-snapshot-generation" sources:"user,env,flag" help:"SHA256 pin of the prepared snapshot binding receipt" fileIgnoreEmpty:"true" fileStorage:"value"`
}
