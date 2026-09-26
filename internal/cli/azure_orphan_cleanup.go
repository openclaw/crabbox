package cli

import (
	"context"
	"fmt"
	"strings"
)

// azureOrphanDeleteResources attests current orphan resources, never an inferred
// historical VM binding. Its distinct durable dialect requires VM absence and
// complete fixed-attempt ownership on every read, including deletion retries.
func (c *AzureClient) azureOrphanDeleteResources(ctx context.Context, expected Server) (azureVMDeleteResources, error) {
	resources := azureVMDeleteResources{orphan: true, vmID: expected.ImmutableID}
	labels, name := expected.Labels, expected.CloudID
	if err := ValidateAzureOwnedVM(expected, expected); err != nil {
		return resources, err
	}
	if name != LeaseProviderName(labels["lease"], labels["slug"]) ||
		labels["provider_key"] != ProviderKeyForLease(labels["lease"]) ||
		labels["fixed_attempt"] == "" || !FixedSHA256(labels["fixed_intent_sha256"]) {
		return resources, fmt.Errorf("Azure orphan recovery requires an exact fixed lease identity")
	}
	prefix := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/", c.SubscriptionID, c.ResourceGroup)
	nicID := prefix + "Microsoft.Network/networkInterfaces/" + name + "-nic"
	pipID := prefix + "Microsoft.Network/publicIPAddresses/" + name + "-pip"
	validate := func(kind, id string, tags map[string]*string, identity, bindingKey string) error {
		if err := validateAzureCleanupResourceTags(kind, name, tags, labels); err != nil {
			return err
		}
		for _, key := range []string{"provider_key", "fixed_attempt", "fixed_intent_sha256"} {
			if stringValue(tags[azureLabelToTagKey(key)]) != labels[key] {
				return fmt.Errorf("Azure orphan %s %s does not match fixed lease %s", kind, id, key)
			}
		}
		if strings.TrimSpace(labels[AzureCleanupBindingLabel]) == azureOrphanCleanupBindingVersion && labels[bindingKey] == "" {
			return fmt.Errorf("Azure orphan %s %s appeared after recovery binding", kind, id)
		}
		return requireAzureCleanupIdentity(kind, id, identity, labels[bindingKey])
	}

	nic, err := c.nicc.Get(ctx, c.ResourceGroup, name+"-nic", nil)
	if err != nil && !isAzureNotFoundError(err) {
		return resources, err
	}
	if err == nil {
		if err := requireAzureResourceID("NIC", name, stringValue(nic.ID), nicID); err != nil {
			return resources, err
		}
		p := nic.Properties
		if p == nil || p.VirtualMachine != nil || p.PrivateEndpoint != nil || p.PrivateLinkService != nil || len(p.HostedWorkloads) != 0 {
			return resources, fmt.Errorf("Azure orphan NIC %s is attached or has unknown attachment state", name)
		}
		if err := validate("NIC", nicID, nic.Tags, stringValue(p.ResourceGUID), azureCleanupNICIdentityLabel); err != nil {
			return resources, err
		}
		for _, config := range p.IPConfigurations {
			if config == nil || config.Properties == nil {
				return resources, fmt.Errorf("Azure orphan NIC %s has unknown IP configuration", name)
			}
			if ip := config.Properties.PublicIPAddress; ip != nil {
				if err := requireAzureResourceID("public IP", name, stringValue(ip.ID), pipID); err != nil {
					return resources, err
				}
			}
		}
		resources.nic, resources.nicID = name+"-nic", stringValue(p.ResourceGUID)
	}

	ip, err := c.pipc.Get(ctx, c.ResourceGroup, name+"-pip", nil)
	if err != nil && !isAzureNotFoundError(err) {
		return resources, err
	}
	if err == nil {
		if err := requireAzureResourceID("public IP", name, stringValue(ip.ID), pipID); err != nil {
			return resources, err
		}
		p := ip.Properties
		if p == nil || p.NatGateway != nil || p.LinkedPublicIPAddress != nil || p.ServicePublicIPAddress != nil {
			return resources, fmt.Errorf("Azure orphan public IP %s is attached or has unknown attachment state", name)
		}
		if p.IPConfiguration != nil {
			id := strings.ToLower(stringValue(p.IPConfiguration.ID))
			parent := strings.ToLower(nicID) + "/ipconfigurations/"
			if resources.nic == "" || !strings.HasPrefix(id, parent) || strings.TrimPrefix(id, parent) == "" || strings.Contains(strings.TrimPrefix(id, parent), "/") {
				return resources, fmt.Errorf("Azure orphan public IP %s is attached to another resource", name)
			}
		}
		if err := validate("public IP", pipID, ip.Tags, stringValue(p.ResourceGUID), azureCleanupPublicIPIdentityLabel); err != nil {
			return resources, err
		}
		resources.publicIP, resources.publicIPID = name+"-pip", stringValue(p.ResourceGUID)
	}

	disk, err := c.diskc.Get(ctx, c.ResourceGroup, name+"-osdisk", nil)
	if err != nil && !isAzureNotFoundError(err) {
		return resources, err
	}
	if err == nil {
		id := prefix + "Microsoft.Compute/disks/" + name + "-osdisk"
		if err := requireAzureResourceID("disk", name, stringValue(disk.ID), id); err != nil {
			return resources, err
		}
		p := disk.Properties
		if p == nil || p.DiskState == nil || string(*p.DiskState) != "Unattached" || stringValue(disk.ManagedBy) != "" || len(disk.ManagedByExtended) != 0 {
			return resources, fmt.Errorf("Azure orphan disk %s is attached or has unknown attachment state", name)
		}
		if err := validate("disk", id, disk.Tags, stringValue(p.UniqueID), azureCleanupDiskIdentityLabel); err != nil {
			return resources, err
		}
		resources.disk, resources.diskID = name+"-osdisk", stringValue(p.UniqueID)
	}

	nsg, err := c.sgc.Get(ctx, c.ResourceGroup, name+azureSnapshotQuarantineNSGSuffix, nil)
	if err != nil && !isAzureNotFoundError(err) {
		return resources, err
	}
	if err == nil {
		id := prefix + "Microsoft.Network/networkSecurityGroups/" + name + azureSnapshotQuarantineNSGSuffix
		if err := requireAzureResourceID("quarantine NSG", name, stringValue(nsg.ID), id); err != nil {
			return resources, err
		}
		p := nsg.Properties
		if p == nil || len(p.Subnets) != 0 {
			return resources, fmt.Errorf("Azure orphan quarantine NSG %s is attached or has unknown attachment state", name)
		}
		for _, nic := range p.NetworkInterfaces {
			if nic == nil || resources.nic == "" || !strings.EqualFold(stringValue(nic.ID), nicID) {
				return resources, fmt.Errorf("Azure orphan quarantine NSG %s is attached to another resource", name)
			}
		}
		if err := validate("quarantine NSG", id, nsg.Tags, stringValue(p.ResourceGUID), azureCleanupNSGIdentityLabel); err != nil {
			return resources, err
		}
		resources.quarantineNSG, resources.quarantineID = name+azureSnapshotQuarantineNSGSuffix, stringValue(p.ResourceGUID)
	}

	// A replacement or reappearing VM must never enter the ordinary VM deletion
	// path, even if its tags match. Read last, immediately before mutation.
	if _, err := c.vmc.Get(ctx, c.ResourceGroup, name, nil); !isAzureNotFoundError(err) {
		if err == nil {
			err = fmt.Errorf("Azure orphan recovery refused: VM %s exists", name)
		}
		return resources, err
	}
	return resources, nil
}
