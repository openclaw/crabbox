package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	core "github.com/openclaw/crabbox/internal/cli"
)

// New recovery policy lives here; the inherited client remains native transport.
type nativeAzureClient struct {
	*core.AzureClient
	core.AzureResourceClients
}

func newNativeAzureClient(client *core.AzureClient) *nativeAzureClient {
	return &nativeAzureClient{AzureClient: client, AzureResourceClients: client.ResourceClients()}
}

func (c *nativeAzureClient) CreateFixedServer(ctx context.Context, cfg core.Config, publicKey, leaseID, slug string, labels map[string]string, record func(core.AzureFixedCompanions) error) (core.Server, error) {
	server, err := c.AzureClient.CreateFixedServer(ctx, cfg, publicKey, leaseID, slug, labels, record)
	return server, classifyAzureFixedCreateError(err)
}

type azureFixedVMShortage struct {
	Code string
	Err  error
}

func (e *azureFixedVMShortage) Error() string { return e.Err.Error() }
func (e *azureFixedVMShortage) Unwrap() error { return e.Err }

func classifyAzureFixedCreateError(err error) error {
	var operation *core.AzureVMCreateError
	var response *azcore.ResponseError
	if errors.As(err, &operation) && operation.Terminal && errors.As(operation.Err, &response) &&
		(response.ErrorCode == "AllocationFailed" || response.ErrorCode == "ZonalAllocationFailed") {
		return &azureFixedVMShortage{Code: response.ErrorCode, Err: err}
	}
	return err
}

const (
	azureSnapshotQuarantineNSGSuffix  = "-q-nsg"
	azureCleanupNICIdentityLabel      = "_crabbox_azure_cleanup_nic_id"
	azureCleanupPublicIPIdentityLabel = "_crabbox_azure_cleanup_public_ip_id"
	azureCleanupDiskIdentityLabel     = "_crabbox_azure_cleanup_disk_id"
)

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func azureResourceAbsent(err error) bool {
	var response *azcore.ResponseError
	return errors.As(err, &response) && response.StatusCode == http.StatusNotFound &&
		(response.ErrorCode == "ResourceNotFound" || response.ErrorCode == "ResourceGroupNotFound")
}

func requireAzureResourceID(kind, name, actual, expected string) error {
	if strings.TrimSpace(actual) == "" || !strings.EqualFold(strings.TrimSpace(actual), strings.TrimSpace(expected)) {
		return fmt.Errorf("Azure %s %s has a conflicting resource ID", kind, name)
	}
	return nil
}

func validateAzureCleanupResourceTags(kind, name string, tags map[string]*string, expected map[string]string) error {
	if stringValue(tags["crabbox"]) != "true" || stringValue(tags["created_by"]) != "crabbox" || stringValue(tags["provider"]) != "azure" {
		return fmt.Errorf("Azure %s %s lacks canonical ownership tags", kind, name)
	}
	leaseID, slug := strings.TrimSpace(stringValue(tags["lease"])), strings.TrimSpace(stringValue(tags["slug"]))
	if !core.IsCanonicalLeaseID(leaseID) || leaseID != strings.TrimSpace(expected["lease"]) || slug == "" || slug != strings.TrimSpace(expected["slug"]) {
		return fmt.Errorf("Azure %s %s ownership does not match the lease", kind, name)
	}
	return nil
}

func (c *nativeAzureClient) resourceID(namespace, kind, name string) string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/%s/%s/%s", c.SubscriptionID, c.ResourceGroup, namespace, kind, name)
}

func (c *nativeAzureClient) validateRecoveryIdentity(expected core.Server, fixed bool) error {
	labels := expected.Labels
	if strings.TrimSpace(c.SubscriptionID) == "" || strings.TrimSpace(c.ResourceGroup) == "" ||
		labels["crabbox"] != "true" || labels["created_by"] != "crabbox" || labels["provider"] != "azure" ||
		!core.IsCanonicalLeaseID(labels["lease"]) || labels["slug"] == "" ||
		expected.CloudID != core.LeaseProviderName(labels["lease"], labels["slug"]) ||
		labels["provider_key"] != core.ProviderKeyForLease(labels["lease"]) ||
		(fixed && (labels["fixed_attempt"] == "" || !core.FixedSHA256(labels["fixed_intent_sha256"]))) {
		return errors.New("Azure recovery requires an exact lease identity and account scope")
	}
	return nil
}
