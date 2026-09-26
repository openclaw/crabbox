package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

// verifyAzureOrphanResourcesAbsent allows the existing claim lifecycle to
// finish after external cleanup. Missing historical bindings never authorize
// a name-addressed DELETE: even matching tags cannot guard against replacement
// between an ownership read and Azure's unconditional companion DELETE.
func (c *AzureClient) verifyAzureOrphanResourcesAbsent(ctx context.Context, expected Server) error {
	if err := ValidateAzureOwnedVM(expected, expected); err != nil {
		return err
	}
	labels, name := expected.Labels, expected.CloudID
	if name != LeaseProviderName(labels["lease"], labels["slug"]) ||
		labels["provider_key"] != ProviderKeyForLease(labels["lease"]) ||
		labels["fixed_attempt"] == "" || !FixedSHA256(labels["fixed_intent_sha256"]) {
		return errors.New("Azure orphan recovery requires an exact fixed lease identity")
	}
	checks := []struct {
		kind string
		get  func() error
	}{
		{"NIC", func() error { _, err := c.nicc.Get(ctx, c.ResourceGroup, name+"-nic", nil); return err }},
		{"public IP", func() error { _, err := c.pipc.Get(ctx, c.ResourceGroup, name+"-pip", nil); return err }},
		{"OS disk", func() error { _, err := c.diskc.Get(ctx, c.ResourceGroup, name+"-osdisk", nil); return err }},
		{"quarantine NSG", func() error {
			_, err := c.sgc.Get(ctx, c.ResourceGroup, name+azureSnapshotQuarantineNSGSuffix, nil)
			return err
		}},
		{"VM", func() error { _, err := c.vmc.Get(ctx, c.ResourceGroup, name, nil); return err }},
	}
	for _, check := range checks {
		err := check.get()
		if err == nil {
			return fmt.Errorf("Azure orphan recovery refused: %s for %s still exists; inspect and clean up remaining resources in Azure, then retry stop", check.kind, name)
		}
		var response *azcore.ResponseError
		if !errors.As(err, &response) || response.StatusCode != http.StatusNotFound ||
			(response.ErrorCode != "ResourceNotFound" && response.ErrorCode != "ResourceGroupNotFound") {
			return fmt.Errorf("verify Azure orphan %s absence: %w", check.kind, err)
		}
	}
	return nil
}
