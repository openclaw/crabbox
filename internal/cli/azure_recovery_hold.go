package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

// Inspection never attaches, snapshots, retags, or deletes a retained disk.
func (c *AzureClient) InspectFailedLeaseHold(ctx context.Context, expected Server) (LeaseRecoveryHold, error) {
	receipt := LeaseRecoveryHold{Schema: "crabbox.lease-hold.v1", Provider: "azure", LeaseID: expected.Labels["lease"], Status: "held", UnacceptedChanges: "unknown"}
	if err := ValidateAzureOwnedVM(expected, expected); err != nil {
		return receipt, err
	}
	name := expected.CloudID
	if name != LeaseProviderName(receipt.LeaseID, expected.Labels["slug"]) || expected.Labels["fixed_attempt"] == "" || !FixedSHA256(expected.Labels["fixed_intent_sha256"]) {
		return receipt, errors.New("Azure recovery hold requires an exact fixed lease identity")
	}
	absent := func(err error) bool {
		var response *azcore.ResponseError
		return errors.As(err, &response) && response.StatusCode == http.StatusNotFound &&
			(response.ErrorCode == "ResourceNotFound" || response.ErrorCode == "ResourceGroupNotFound")
	}
	if _, err := c.vmc.Get(ctx, c.ResourceGroup, name, nil); !absent(err) {
		if err == nil {
			err = errors.New("VM still exists")
		}
		return receipt, fmt.Errorf("Azure failed-lease hold requires proven VM absence: %w", err)
	}
	resourceID := func(namespace, kind, resource string) string {
		return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/%s/%s/%s", c.SubscriptionID, c.ResourceGroup, namespace, kind, resource)
	}
	receipt.Resources = append(receipt.Resources, LeaseHeldResource{Kind: "vm", ID: resourceID("Microsoft.Compute", "virtualMachines", name), ImmutableID: expected.ImmutableID, State: "absent"})
	retain := func(kind, resource, id, immutable string, tags map[string]*string, err error) error {
		if absent(err) {
			receipt.Resources = append(receipt.Resources, LeaseHeldResource{Kind: kind, ID: id, State: "absent"})
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect held Azure %s: %w", kind, err)
		}
		if err := validateAzureCleanupResourceTags(kind, resource, tags, expected.Labels); err != nil {
			return err
		}
		for _, key := range []string{"fixed_attempt", "fixed_intent_sha256", "provider_key"} {
			if stringValue(tags[azureLabelToTagKey(key)]) != expected.Labels[key] {
				return fmt.Errorf("Azure held %s fixed identity changed", kind)
			}
		}
		if strings.TrimSpace(immutable) == "" {
			return fmt.Errorf("Azure held %s has no immutable identity", kind)
		}
		receipt.Resources = append(receipt.Resources, LeaseHeldResource{Kind: kind, ID: id, ImmutableID: immutable, State: "retained"})
		return nil
	}
	nicName, nicID := name+"-nic", resourceID("Microsoft.Network", "networkInterfaces", name+"-nic")
	nic, nicErr := c.nicc.Get(ctx, c.ResourceGroup, nicName, nil)
	nicGUID := ""
	if nicErr == nil {
		if nic.Properties == nil || nic.Properties.VirtualMachine != nil {
			return receipt, errors.New("Azure held NIC is attached or has no properties")
		}
		if err := requireAzureResourceID("NIC", nicName, stringValue(nic.ID), nicID); err != nil {
			return receipt, err
		}
		nicGUID = stringValue(nic.Properties.ResourceGUID)
	}
	if err := retain("nic", nicName, nicID, nicGUID, nic.Tags, nicErr); err != nil {
		return receipt, err
	}
	pipName, pipID := name+"-pip", resourceID("Microsoft.Network", "publicIPAddresses", name+"-pip")
	pip, pipErr := c.pipc.Get(ctx, c.ResourceGroup, pipName, nil)
	pipGUID := ""
	if pipErr == nil {
		if pip.Properties == nil {
			return receipt, errors.New("Azure held public IP has no properties")
		}
		if pip.Properties.NatGateway != nil {
			return receipt, errors.New("Azure held public IP is attached to a NAT gateway")
		}
		if pip.Properties.IPConfiguration != nil && !strings.HasPrefix(strings.ToLower(stringValue(pip.Properties.IPConfiguration.ID)), strings.ToLower(nicID)+"/ipconfigurations/") {
			return receipt, errors.New("Azure held public IP is attached to another NIC")
		}
		if err := requireAzureResourceID("public IP", pipName, stringValue(pip.ID), pipID); err != nil {
			return receipt, err
		}
		pipGUID = stringValue(pip.Properties.ResourceGUID)
	}
	if err := retain("public-ip", pipName, pipID, pipGUID, pip.Tags, pipErr); err != nil {
		return receipt, err
	}
	diskName, diskID := name+"-osdisk", resourceID("Microsoft.Compute", "disks", name+"-osdisk")
	disk, diskErr := c.diskc.Get(ctx, c.ResourceGroup, diskName, nil)
	diskGUID := ""
	if diskErr == nil {
		if disk.ManagedBy != nil || len(disk.ManagedByExtended) != 0 || disk.Properties == nil || disk.Properties.DiskState == nil || string(*disk.Properties.DiskState) != "Unattached" {
			return receipt, errors.New("Azure held disk is attached or its attachment state is unproven")
		}
		if err := requireAzureResourceID("disk", diskName, stringValue(disk.ID), diskID); err != nil {
			return receipt, err
		}
		diskGUID = stringValue(disk.Properties.UniqueID)
	}
	if err := retain("disk", diskName, diskID, diskGUID, disk.Tags, diskErr); err != nil {
		return receipt, err
	}
	nsgName, nsgID := name+azureSnapshotQuarantineNSGSuffix, resourceID("Microsoft.Network", "networkSecurityGroups", name+azureSnapshotQuarantineNSGSuffix)
	nsg, nsgErr := c.sgc.Get(ctx, c.ResourceGroup, nsgName, nil)
	nsgGUID := ""
	if nsgErr == nil {
		if nsg.Properties == nil || len(nsg.Properties.Subnets) != 0 {
			return receipt, errors.New("Azure held NSG has an unproven or shared attachment")
		}
		for _, attached := range nsg.Properties.NetworkInterfaces {
			if attached == nil || !strings.EqualFold(stringValue(attached.ID), nicID) {
				return receipt, errors.New("Azure held NSG is attached to another NIC")
			}
		}
		if err := requireAzureResourceID("NSG", nsgName, stringValue(nsg.ID), nsgID); err != nil {
			return receipt, err
		}
		nsgGUID = stringValue(nsg.Properties.ResourceGUID)
	}
	if err := retain("nsg", nsgName, nsgID, nsgGUID, nsg.Tags, nsgErr); err != nil {
		return receipt, err
	}
	// A resource can reappear during the reads; a second VM observation never deletes it.
	if _, err := c.vmc.Get(ctx, c.ResourceGroup, name, nil); !absent(err) {
		if err == nil {
			err = errors.New("VM reappeared")
		}
		return receipt, fmt.Errorf("Azure failed-lease hold lost VM absence: %w", err)
	}
	return receipt, nil
}
