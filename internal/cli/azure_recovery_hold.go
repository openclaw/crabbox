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
	if err := ValidateAzureOwnedVM(expected, expected); err != nil {
		return LeaseRecoveryHold{}, err
	}
	return c.inspectAzureFailedLeaseHold(ctx, expected, false)
}

// Claimless inspection records current resources at the original fixed name.
// The observed immutable IDs are custody facts, never original delete grants.
func (c *AzureClient) InspectClaimlessFailedLeaseHold(ctx context.Context, leaseID, slug string) (LeaseRecoveryHold, error) {
	if !IsCanonicalLeaseID(leaseID) || slug == "" || slug != NormalizeLeaseSlug(slug) {
		return LeaseRecoveryHold{}, errors.New("claimless Azure hold requires an exact lease id and original slug")
	}
	if strings.TrimSpace(c.SubscriptionID) == "" || strings.TrimSpace(c.ResourceGroup) == "" {
		return LeaseRecoveryHold{}, errors.New("claimless Azure hold requires an exact subscription and resource group")
	}
	expected := Server{CloudID: LeaseProviderName(leaseID, slug), Labels: map[string]string{
		"crabbox": "true", "created_by": "crabbox", "provider": "azure",
		"lease": leaseID, "slug": slug, "provider_key": ProviderKeyForLease(leaseID),
	}}
	return c.inspectAzureFailedLeaseHold(ctx, expected, true)
}

// An unbound failed attempt can be retained without fabricating a VM identity
// or a disk cleanup binding. Require the original attempt tags where present.
func (c *AzureClient) InspectUnboundFailedLeaseHold(ctx context.Context, expected Server, original AzureFixedCompanions) (LeaseRecoveryHold, error) {
	labels := expected.Labels
	if expected.ImmutableID != "" || labels["crabbox"] != "true" || labels["created_by"] != "crabbox" || labels["provider"] != "azure" ||
		!IsCanonicalLeaseID(labels["lease"]) || labels["slug"] == "" || labels["provider_key"] != ProviderKeyForLease(labels["lease"]) ||
		labels["fixed_attempt"] == "" || !FixedSHA256(labels["fixed_intent_sha256"]) ||
		strings.TrimSpace(c.SubscriptionID) == "" || strings.TrimSpace(c.ResourceGroup) == "" {
		return LeaseRecoveryHold{}, errors.New("Azure unbound hold requires the original fixed attempt and account scope")
	}
	// Observation mode also inventories VMs and permits an untagged disk to be
	// recorded without claiming that its identity was bound during allocation.
	receipt, err := c.inspectAzureFailedLeaseHold(ctx, expected, true)
	if err != nil {
		return receipt, err
	}
	for _, resource := range receipt.Resources {
		if resource.State != "retained" {
			continue
		}
		identity := ""
		switch resource.Kind {
		case "nic":
			identity = original.NICGUID
		case "public-ip":
			identity = original.PublicIPGUID
		}
		if identity != "" && resource.ImmutableID != identity {
			return LeaseRecoveryHold{}, errors.New("Azure unbound hold companion differs from its original pre-VM identity")
		}
	}
	return receipt, nil
}

func (c *AzureClient) inspectAzureFailedLeaseHold(ctx context.Context, expected Server, claimless bool) (LeaseRecoveryHold, error) {
	receipt := LeaseRecoveryHold{Schema: "crabbox.lease-hold.v1", Provider: "azure", LeaseID: expected.Labels["lease"], Status: "held", UnacceptedChanges: "unknown"}
	name := expected.CloudID
	if name != LeaseProviderName(receipt.LeaseID, expected.Labels["slug"]) ||
		(!claimless && (expected.Labels["fixed_attempt"] == "" || !FixedSHA256(expected.Labels["fixed_intent_sha256"]))) {
		return receipt, errors.New("Azure recovery hold requires an exact fixed lease identity")
	}
	absent := func(err error) bool {
		var response *azcore.ResponseError
		return errors.As(err, &response) && response.StatusCode == http.StatusNotFound &&
			(response.ErrorCode == "ResourceNotFound" || response.ErrorCode == "ResourceGroupNotFound")
	}
	checkVMAbsent := func() error {
		if _, err := c.vmc.Get(ctx, c.ResourceGroup, name, nil); !absent(err) {
			if err == nil {
				err = errors.New("VM still exists")
			}
			return fmt.Errorf("Azure failed-lease hold requires proven VM absence: %w", err)
		}
		if claimless {
			pager := c.vmc.NewListPager(c.ResourceGroup, nil)
			for pager.More() {
				page, err := pager.NextPage(ctx)
				if err != nil {
					return fmt.Errorf("list Azure VMs before claimless hold: %w", err)
				}
				for _, vm := range page.Value {
					if vm == nil || vm.Name == nil || vm.ID == nil {
						return errors.New("Azure claimless hold VM inventory is incomplete")
					}
					if strings.EqualFold(*vm.Name, name) ||
						stringValue(vm.Tags[azureLabelToTagKey("lease")]) == receipt.LeaseID ||
						stringValue(vm.Tags[azureLabelToTagKey("provider_key")]) == expected.Labels["provider_key"] {
						return errors.New("Azure claimless hold found a live VM for the original lease")
					}
				}
			}
		}
		return nil
	}
	if err := checkVMAbsent(); err != nil {
		return receipt, err
	}
	resourceID := func(namespace, kind, resource string) string {
		return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/%s/%s/%s", c.SubscriptionID, c.ResourceGroup, namespace, kind, resource)
	}
	receipt.Resources = append(receipt.Resources, LeaseHeldResource{Kind: "vm", ID: resourceID("Microsoft.Compute", "virtualMachines", name), ImmutableID: expected.ImmutableID, State: "absent"})
	var observedAttempt, observedFingerprint string
	retain := func(kind, resource, id, immutable string, tags map[string]*string, err error) error {
		if absent(err) {
			receipt.Resources = append(receipt.Resources, LeaseHeldResource{Kind: kind, ID: id, State: "absent"})
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect held Azure %s: %w", kind, err)
		}
		if kind != "disk" || len(tags) != 0 {
			if err := validateAzureCleanupResourceTags(kind, resource, tags, expected.Labels); err != nil {
				return err
			}
			if stringValue(tags[azureLabelToTagKey("provider_key")]) != expected.Labels["provider_key"] {
				return fmt.Errorf("Azure held %s provider key changed", kind)
			}
			if claimless && expected.Labels["fixed_attempt"] == "" {
				attempt := stringValue(tags[azureLabelToTagKey("fixed_attempt")])
				fingerprint := stringValue(tags[azureLabelToTagKey("fixed_intent_sha256")])
				if attempt == "" || !FixedSHA256(fingerprint) ||
					(observedAttempt != "" && (observedAttempt != attempt || observedFingerprint != fingerprint)) {
					return fmt.Errorf("Azure held %s has conflicting fixed identity", kind)
				}
				observedAttempt, observedFingerprint = attempt, fingerprint
			} else if stringValue(tags[azureLabelToTagKey("fixed_attempt")]) != expected.Labels["fixed_attempt"] ||
				stringValue(tags[azureLabelToTagKey("fixed_intent_sha256")]) != expected.Labels["fixed_intent_sha256"] {
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
		if !claimless && len(disk.Tags) == 0 {
			// Image-created OS disks do not inherit VM tags. The original
			// durable binding must prove this is the retained disk instead.
			binding, err := azureDeleteResourcesFromLabels(expected)
			if err != nil {
				return receipt, fmt.Errorf("Azure held untagged disk requires its original cleanup binding: %w", err)
			}
			if binding.disk != diskName || binding.diskID == "" || binding.diskID != diskGUID {
				return receipt, errors.New("Azure held untagged disk identity does not match its original cleanup binding")
			}
		}
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
	if err := checkVMAbsent(); err != nil {
		return receipt, err
	}
	if claimless {
		retained := false
		for _, resource := range receipt.Resources[1:] {
			retained = retained || resource.State == "retained"
		}
		if !retained {
			return receipt, errors.New("Azure claimless hold has no remaining resource to retain")
		}
	}
	return receipt, nil
}
