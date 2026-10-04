package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

// SettleRejectedFixedCompanions uses the existing Azure cleanup validation and
// the caller's durable fixed-claim lock. A failed read or mismatched physical
// identity leaves the claim intact for same-lease recovery.
func (c *AzureClient) SettleRejectedFixedCompanions(ctx context.Context, expected Server, binding AzureFixedCompanions) error {
	if expected.CloudID == "" || expected.Labels["fixed_attempt"] == "" || !FixedSHA256(expected.Labels["fixed_intent_sha256"]) ||
		binding.NICGUID == "" || binding.PublicIPGUID == "" {
		return errors.New("Azure fixed rejection lacks exact attempt or companion custody")
	}
	if err := c.verifyRejectedFixedVMAndOtherResourcesAbsent(ctx, expected.CloudID); err != nil {
		return err
	}
	resources := azureVMDeleteResources{
		nic: expected.CloudID + "-nic", nicID: binding.NICGUID,
		publicIP: expected.CloudID + "-pip", publicIPID: binding.PublicIPGUID,
	}
	var err error
	resources, err = c.revalidateAzureDeleteResources(ctx, expected, resources, nil)
	if err != nil {
		return err
	}
	if resources.nic != "" {
		if err := c.deleteNIC(ctx, resources.nic); err != nil {
			return err
		}
	}
	if err := c.verifyRejectedFixedVMAndOtherResourcesAbsent(ctx, expected.CloudID); err != nil {
		return err
	}
	resources, err = c.revalidateAzureDeleteResources(ctx, expected, resources, nil)
	if err != nil {
		return err
	}
	if resources.publicIP != "" {
		if err := c.deletePublicIP(ctx, resources.publicIP); err != nil {
			return err
		}
	}
	return c.verifyAzureFixedResourcesAbsent(ctx, expected)
}

func (c *AzureClient) verifyRejectedFixedVMAndOtherResourcesAbsent(ctx context.Context, name string) error {
	checks := []struct {
		kind string
		get  func() error
	}{
		{"VM", func() error { _, err := c.vmc.Get(ctx, c.ResourceGroup, name, nil); return err }},
		{"OS disk", func() error { _, err := c.diskc.Get(ctx, c.ResourceGroup, name+"-osdisk", nil); return err }},
		{"quarantine NSG", func() error {
			_, err := c.sgc.Get(ctx, c.ResourceGroup, name+azureSnapshotQuarantineNSGSuffix, nil)
			return err
		}},
	}
	for _, check := range checks {
		err := check.get()
		if err == nil {
			return fmt.Errorf("Azure fixed rejection cannot settle: %s still exists", check.kind)
		}
		var response *azcore.ResponseError
		if !errors.As(err, &response) || response.StatusCode != http.StatusNotFound ||
			(response.ErrorCode != "ResourceNotFound" && response.ErrorCode != "ResourceGroupNotFound") {
			return fmt.Errorf("verify Azure rejected %s absence: %w", check.kind, err)
		}
	}
	return nil
}
