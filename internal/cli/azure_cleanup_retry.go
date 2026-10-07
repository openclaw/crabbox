package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (c *AzureClient) deleteAzureCleanupResourcesWithRetry(ctx context.Context, expected Server, resources azureVMDeleteResources, now time.Time) error {
	return c.deleteAzureValidatedResourcesWithRetry(ctx, expected, resources, func(expected, live Server) error {
		return validateAzureCleanupVM(expected, live, now)
	})
}

func (c *AzureClient) deleteAzureValidatedResourcesWithRetry(ctx context.Context, expected Server, resources azureVMDeleteResources, validateVM func(Server, Server) error) error {
	name := strings.TrimSpace(expected.CloudID)
	var err error
	resources, err = c.revalidateAzureDeleteResourcesWithRetry(ctx, expected, resources, validateVM)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		if resources.vm {
			vmOnly := azureVMDeleteResources{vm: true}
			errs, retry := c.deleteVMResourcesOnce(ctx, name, vmOnly)
			if len(errs) != 0 {
				if !retry || attempt >= azureDeleteRetryAttempts-1 {
					return joinErrors(errs)
				}
				if err := waitForAzureCleanupRetry(ctx, errs); err != nil {
					return err
				}
				var err error
				resources, err = c.revalidateAzureDeleteResourcesWithRetry(ctx, expected, resources, validateVM)
				if err != nil {
					return err
				}
				continue
			}
			resources.vm = false
			var err error
			resources, err = c.revalidateAzureDeleteResourcesWithRetry(ctx, expected, resources, validateVM)
			if err != nil {
				return err
			}
			if azureCleanupResourcesEmpty(resources) {
				return nil
			}
		}

		errs, retry := c.deleteVMResourcesOnce(ctx, name, resources)
		if len(errs) == 0 {
			return nil
		}
		if !retry || attempt >= azureDeleteRetryAttempts-1 {
			return joinErrors(errs)
		}
		if err := waitForAzureCleanupRetry(ctx, errs); err != nil {
			return err
		}
		var err error
		resources, err = c.revalidateAzureDeleteResourcesWithRetry(ctx, expected, resources, validateVM)
		if err != nil {
			return err
		}
		if azureCleanupResourcesEmpty(resources) {
			return nil
		}
	}
}

func (c *AzureClient) revalidateAzureDeleteResourcesWithRetry(ctx context.Context, expected Server, resources azureVMDeleteResources, validateVM func(Server, Server) error) (azureVMDeleteResources, error) {
	return retryAzureCleanupResourceReads(ctx, resources, func(current azureVMDeleteResources) (azureVMDeleteResources, error) {
		return c.revalidateAzureDeleteResources(ctx, expected, current, validateVM)
	})
}

func retryAzureCleanupResourceReads(ctx context.Context, resources azureVMDeleteResources, revalidate func(azureVMDeleteResources) (azureVMDeleteResources, error)) (azureVMDeleteResources, error) {
	return retryAzureCleanupResourceReadsWithWait(ctx, resources, revalidate, waitForAzureCleanupRetry)
}

func retryAzureCleanupResourceReadsWithWait(ctx context.Context, resources azureVMDeleteResources, revalidate func(azureVMDeleteResources) (azureVMDeleteResources, error), wait func(context.Context, []error) error) (azureVMDeleteResources, error) {
	for attempt := 0; ; attempt++ {
		next, err := revalidate(resources)
		if err == nil {
			return next, nil
		}
		var readErr *azureCleanupResourceReadError
		if !errors.As(err, &readErr) || attempt >= azureDeleteRetryAttempts-1 {
			return resources, err
		}
		if err := wait(ctx, []error{err}); err != nil {
			return resources, err
		}
	}
}

func waitForAzureCleanupRetry(ctx context.Context, errs []error) error {
	select {
	case <-ctx.Done():
		return joinErrors(append(errs, ctx.Err()))
	case <-time.After(azureDeleteRetryDelay):
		return nil
	}
}

func azureCleanupResourcesEmpty(resources azureVMDeleteResources) bool {
	return !resources.vm && resources.nic == "" && resources.publicIP == "" && resources.disk == "" && resources.quarantineNSG == ""
}

func (c *AzureClient) revalidateAzureDeleteResources(ctx context.Context, expected Server, resources azureVMDeleteResources, validateVM func(Server, Server) error) (azureVMDeleteResources, error) {
	name := strings.TrimSpace(expected.CloudID)
	labels := expected.Labels
	vmID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/virtualMachines/%s", c.SubscriptionID, c.ResourceGroup, name)
	nicID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/networkInterfaces/%s-nic", c.SubscriptionID, c.ResourceGroup, name)
	pipID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s-pip", c.SubscriptionID, c.ResourceGroup, name)
	type vmAttachment struct{ kind, name, id string }
	var attachments []vmAttachment
	if resources.nic != "" {
		response, err := c.nicc.Get(ctx, c.ResourceGroup, resources.nic, nil)
		if err != nil {
			if isAzureNotFoundError(err) {
				resources.nic = ""
			} else {
				return resources, &azureCleanupResourceReadError{err: fmt.Errorf("re-read Azure cleanup NIC %s before retry: %w", resources.nic, err)}
			}
		} else {
			if err := validateAzureCleanupResourceTags("NIC", resources.nic, response.Tags, labels); err != nil {
				return resources, &azureCleanupSkipError{err: err}
			}
			if expected.ImmutableID == "" && !azureFixedAttemptTagsMatch(response.Tags, labels) {
				return resources, &azureCleanupSkipError{err: errors.New("rejected Azure NIC changed fixed ownership")}
			}
			if response.Properties == nil {
				return resources, &azureCleanupSkipError{err: fmt.Errorf("Azure cleanup NIC %s has no properties", resources.nic)}
			}
			if response.Properties.VirtualMachine != nil {
				attachments = append(attachments, vmAttachment{"NIC", resources.nic, stringValue(response.Properties.VirtualMachine.ID)})
			}
			for _, config := range response.Properties.IPConfigurations {
				if config != nil && config.Properties != nil && config.Properties.PublicIPAddress != nil &&
					!strings.EqualFold(strings.TrimSpace(stringValue(config.Properties.PublicIPAddress.ID)), pipID) {
					return resources, &azureCleanupSkipError{err: fmt.Errorf("Azure cleanup NIC %s references another public IP", resources.nic)}
				}
			}
			if err := requireAzureCleanupIdentity("NIC", resources.nic, stringValue(response.Properties.ResourceGUID), resources.nicID); err != nil {
				return resources, &azureCleanupSkipError{err: err}
			}
		}
	}

	if resources.publicIP != "" {
		response, err := c.pipc.Get(ctx, c.ResourceGroup, resources.publicIP, nil)
		if err != nil {
			if isAzureNotFoundError(err) {
				resources.publicIP = ""
			} else {
				return resources, &azureCleanupResourceReadError{err: fmt.Errorf("re-read Azure cleanup public IP %s before retry: %w", resources.publicIP, err)}
			}
		} else {
			if err := validateAzureCleanupResourceTags("public IP", resources.publicIP, response.Tags, labels); err != nil {
				return resources, &azureCleanupSkipError{err: err}
			}
			if expected.ImmutableID == "" && !azureFixedAttemptTagsMatch(response.Tags, labels) {
				return resources, &azureCleanupSkipError{err: errors.New("rejected Azure public IP changed fixed ownership")}
			}
			if response.Properties == nil {
				return resources, &azureCleanupSkipError{err: fmt.Errorf("Azure cleanup public IP %s has no properties", resources.publicIP)}
			}
			if response.Properties.NatGateway != nil {
				return resources, &azureCleanupSkipError{err: fmt.Errorf("Azure cleanup public IP %s is attached to a NAT gateway", resources.publicIP)}
			}
			if config := response.Properties.IPConfiguration; config != nil {
				prefix := strings.ToLower(nicID) + "/ipconfigurations/"
				id := strings.ToLower(strings.TrimSpace(stringValue(config.ID)))
				if resources.nic == "" || !strings.HasPrefix(id, prefix) || strings.TrimPrefix(id, prefix) == "" || strings.Contains(strings.TrimPrefix(id, prefix), "/") {
					return resources, &azureCleanupSkipError{err: fmt.Errorf("Azure cleanup public IP %s has an unproven or foreign NIC association", resources.publicIP)}
				}
			}
			if err := requireAzureCleanupIdentity("public IP", resources.publicIP, stringValue(response.Properties.ResourceGUID), resources.publicIPID); err != nil {
				return resources, &azureCleanupSkipError{err: err}
			}
		}
	}

	if resources.disk != "" {
		response, err := c.diskc.Get(ctx, c.ResourceGroup, resources.disk, nil)
		if err != nil {
			if isAzureNotFoundError(err) {
				resources.disk = ""
			} else {
				return resources, &azureCleanupResourceReadError{err: fmt.Errorf("re-read Azure cleanup disk %s before retry: %w", resources.disk, err)}
			}
		} else {
			if response.Properties == nil {
				return resources, &azureCleanupSkipError{err: fmt.Errorf("Azure cleanup disk %s has no properties", resources.disk)}
			}
			if response.ManagedBy != nil {
				attachments = append(attachments, vmAttachment{"disk", resources.disk, stringValue(response.ManagedBy)})
			}
			for _, id := range response.ManagedByExtended {
				attachments = append(attachments, vmAttachment{"disk", resources.disk, stringValue(id)})
			}
			if response.Properties.DiskState == nil ||
				(string(*response.Properties.DiskState) != "Unattached" && response.ManagedBy == nil && len(response.ManagedByExtended) == 0) {
				return resources, &azureCleanupSkipError{err: fmt.Errorf("Azure cleanup disk %s attachment state is unproven", resources.disk)}
			}
			if err := requireAzureCleanupIdentity("disk", resources.disk, stringValue(response.Properties.UniqueID), resources.diskID); err != nil {
				return resources, &azureCleanupSkipError{err: err}
			}
		}
	}

	if resources.quarantineNSG != "" {
		response, err := c.sgc.Get(ctx, c.ResourceGroup, resources.quarantineNSG, nil)
		if err != nil {
			if isAzureNotFoundError(err) {
				resources.quarantineNSG = ""
			} else {
				return resources, &azureCleanupResourceReadError{err: fmt.Errorf("re-read Azure cleanup quarantine NSG %s before retry: %w", resources.quarantineNSG, err)}
			}
		} else {
			if err := validateAzureCleanupResourceTags("quarantine NSG", resources.quarantineNSG, response.Tags, labels); err != nil {
				return resources, &azureCleanupSkipError{err: err}
			}
			if response.Properties == nil {
				return resources, &azureCleanupSkipError{err: fmt.Errorf("Azure cleanup quarantine NSG %s has no properties", resources.quarantineNSG)}
			}
			if len(response.Properties.Subnets) != 0 {
				return resources, &azureCleanupSkipError{err: fmt.Errorf("Azure cleanup quarantine NSG %s is attached to a subnet", resources.quarantineNSG)}
			}
			for _, nic := range response.Properties.NetworkInterfaces {
				if nic == nil || resources.nic == "" || !strings.EqualFold(strings.TrimSpace(stringValue(nic.ID)), nicID) {
					return resources, &azureCleanupSkipError{err: fmt.Errorf("Azure cleanup quarantine NSG %s has an unproven or foreign NIC association", resources.quarantineNSG)}
				}
			}
			if err := requireAzureCleanupIdentity("quarantine NSG", resources.quarantineNSG, stringValue(response.Properties.ResourceGUID), resources.quarantineID); err != nil {
				return resources, &azureCleanupSkipError{err: err}
			}
		}
	}

	// Read the VM last so lease renewal and immutable identity are checked
	// after every companion read and immediately before the delete boundary.
	if resources.vm {
		response, err := c.vmc.Get(ctx, c.ResourceGroup, name, nil)
		if err != nil {
			if isAzureNotFoundError(err) {
				resources.vm = false
			} else {
				return resources, &azureCleanupResourceReadError{err: fmt.Errorf("re-read Azure cleanup VM %s before retry: %w", name, err)}
			}
		} else {
			live := azureVMToServer(response.VirtualMachine, "", "")
			if err := validateVM(expected, live); err != nil {
				return resources, &azureCleanupSkipError{err: err}
			}
			if response.VirtualMachine.Properties == nil {
				return resources, &azureCleanupSkipError{err: fmt.Errorf("live Azure VM %s has no properties", name)}
			}
			if err := requireAzureCleanupIdentity("VM", name, stringValue(response.VirtualMachine.Properties.VMID), resources.vmID); err != nil {
				return resources, &azureCleanupSkipError{err: err}
			}
		}
	}
	// Immutable IDs identify the original objects, not their current users.
	// Validate all VM attachments after the final VM read, before any DELETE.
	for _, attachment := range attachments {
		if !resources.vm || !strings.EqualFold(strings.TrimSpace(attachment.id), vmID) {
			return resources, &azureCleanupSkipError{err: fmt.Errorf("Azure cleanup %s %s is attached to an absent or different VM", attachment.kind, attachment.name)}
		}
	}
	return resources, nil
}

func azureFixedAttemptTagsMatch(tags map[string]*string, labels map[string]string) bool {
	for _, key := range []string{"fixed_attempt", "fixed_intent_sha256", "provider_key"} {
		if labels[key] == "" || stringValue(tags[azureLabelToTagKey(key)]) != labels[key] {
			return false
		}
	}
	return true
}
