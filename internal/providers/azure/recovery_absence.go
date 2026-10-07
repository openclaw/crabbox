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

// VerifyFailedLeaseHoldAbsent only observes Azure. The backend owns publishing
// the terminal hold receipt under its durable claim fence; no DELETE is issued.
func (c *nativeAzureClient) VerifyFailedLeaseHoldAbsent(ctx context.Context, leaseID, slug string) (core.LeaseRecoveryHold, error) {
	expected := core.Server{CloudID: core.LeaseProviderName(leaseID, slug), Labels: map[string]string{
		"crabbox": "true", "created_by": "crabbox", "provider": "azure",
		"lease": leaseID, "slug": slug, "provider_key": core.ProviderKeyForLease(leaseID),
	}}
	if slug != core.NormalizeLeaseSlug(slug) {
		return core.LeaseRecoveryHold{}, errors.New("Azure finalization requires the original exact slug")
	}
	if err := c.validateRecoveryIdentity(expected, false); err != nil {
		return core.LeaseRecoveryHold{}, err
	}
	name := expected.CloudID
	receipt := core.LeaseRecoveryHold{Schema: "crabbox.lease-hold.v1", Provider: "azure", LeaseID: leaseID, Status: "finalized", UnacceptedChanges: "unknown"}
	checks := []struct {
		kind, namespace, resourceType, name string
		get                                 func() error
	}{
		{"vm", "Microsoft.Compute", "virtualMachines", name, func() error { _, err := c.VirtualMachines.Get(ctx, c.ResourceGroup, name, nil); return err }},
		{"nic", "Microsoft.Network", "networkInterfaces", name + "-nic", func() error { _, err := c.Interfaces.Get(ctx, c.ResourceGroup, name+"-nic", nil); return err }},
		{"public-ip", "Microsoft.Network", "publicIPAddresses", name + "-pip", func() error { _, err := c.PublicIPs.Get(ctx, c.ResourceGroup, name+"-pip", nil); return err }},
		{"disk", "Microsoft.Compute", "disks", name + "-osdisk", func() error { _, err := c.Disks.Get(ctx, c.ResourceGroup, name+"-osdisk", nil); return err }},
		{"nsg", "Microsoft.Network", "networkSecurityGroups", name + azureSnapshotQuarantineNSGSuffix, func() error {
			_, err := c.SecurityGroups.Get(ctx, c.ResourceGroup, name+azureSnapshotQuarantineNSGSuffix, nil)
			return err
		}},
	}
	for _, check := range checks {
		if err := check.get(); !azureResourceAbsent(err) {
			if err == nil {
				return core.LeaseRecoveryHold{}, fmt.Errorf("Azure finalization refused: %s remains; retain the hold until operator cleanup completes", check.kind)
			}
			return core.LeaseRecoveryHold{}, fmt.Errorf("verify Azure %s absence: %w", check.kind, err)
		}
		receipt.Resources = append(receipt.Resources, core.LeaseHeldResource{Kind: check.kind, ID: c.resourceID(check.namespace, check.resourceType, check.name), State: "absent"})
	}
	pager := c.VirtualMachines.NewListPager(c.ResourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			var response *azcore.ResponseError
			// Deleting the entire original group is a supported operator cleanup.
			if errors.As(err, &response) && response.StatusCode == http.StatusNotFound && response.ErrorCode == "ResourceGroupNotFound" {
				break
			}
			return core.LeaseRecoveryHold{}, fmt.Errorf("verify Azure VM inventory before finalization: %w", err)
		}
		for _, vm := range page.Value {
			if vm == nil || vm.Name == nil || vm.ID == nil || strings.TrimSpace(*vm.Name) == "" ||
				!strings.EqualFold(*vm.ID, c.resourceID("Microsoft.Compute", "virtualMachines", *vm.Name)) {
				return core.LeaseRecoveryHold{}, errors.New("Azure finalization VM inventory is incomplete")
			}
			if strings.EqualFold(*vm.Name, name) || stringValue(vm.Tags["lease"]) == leaseID || stringValue(vm.Tags["provider_key"]) == expected.Labels["provider_key"] {
				return core.LeaseRecoveryHold{}, errors.New("Azure finalization found a live VM for the held lease")
			}
		}
	}
	if _, err := c.VirtualMachines.Get(ctx, c.ResourceGroup, name, nil); !azureResourceAbsent(err) {
		if err == nil {
			err = errors.New("VM reappeared")
		}
		return core.LeaseRecoveryHold{}, fmt.Errorf("recheck Azure VM absence before finalization: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return core.LeaseRecoveryHold{}, err
	}
	return receipt, nil
}
