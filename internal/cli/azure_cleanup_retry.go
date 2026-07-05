package cli

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (c *AzureClient) deleteAzureCleanupResourcesWithRetry(ctx context.Context, expected Server, resources azureVMDeleteResources, now time.Time) error {
	name := strings.TrimSpace(expected.CloudID)
	for attempt := 0; ; attempt++ {
		errs, retry := c.deleteVMResourcesOnce(ctx, name, resources)
		if len(errs) == 0 {
			return nil
		}
		if !retry || attempt >= azureDeleteRetryAttempts-1 {
			return joinErrors(errs)
		}
		select {
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
			return joinErrors(errs)
		case <-time.After(azureDeleteRetryDelay):
		}
		var err error
		resources, err = c.revalidateAzureCleanupDeleteResources(ctx, expected, resources, now)
		if err != nil {
			return err
		}
		if !resources.vm && resources.nic == "" && resources.publicIP == "" && resources.disk == "" && resources.quarantineNSG == "" {
			return nil
		}
	}
}

func (c *AzureClient) revalidateAzureCleanupDeleteResources(ctx context.Context, expected Server, resources azureVMDeleteResources, now time.Time) (azureVMDeleteResources, error) {
	name := strings.TrimSpace(expected.CloudID)
	labels := expected.Labels
	if resources.vm {
		response, err := c.vmc.Get(ctx, c.ResourceGroup, name, nil)
		if err != nil {
			if isAzureNotFoundError(err) {
				resources.vm = false
			} else {
				return resources, fmt.Errorf("re-read Azure cleanup VM %s before retry: %w", name, err)
			}
		} else {
			live := azureVMToServer(response.VirtualMachine, "", "")
			if err := validateAzureCleanupVM(expected, live, now); err != nil {
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

	if resources.nic != "" {
		response, err := c.nicc.Get(ctx, c.ResourceGroup, resources.nic, nil)
		if err != nil {
			if isAzureNotFoundError(err) {
				resources.nic = ""
			} else {
				return resources, fmt.Errorf("re-read Azure cleanup NIC %s before retry: %w", resources.nic, err)
			}
		} else {
			if err := validateAzureCleanupResourceTags("NIC", resources.nic, response.Tags, labels); err != nil {
				return resources, &azureCleanupSkipError{err: err}
			}
			if response.Properties == nil {
				return resources, &azureCleanupSkipError{err: fmt.Errorf("Azure cleanup NIC %s has no properties", resources.nic)}
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
				return resources, fmt.Errorf("re-read Azure cleanup public IP %s before retry: %w", resources.publicIP, err)
			}
		} else {
			if err := validateAzureCleanupResourceTags("public IP", resources.publicIP, response.Tags, labels); err != nil {
				return resources, &azureCleanupSkipError{err: err}
			}
			if response.Properties == nil {
				return resources, &azureCleanupSkipError{err: fmt.Errorf("Azure cleanup public IP %s has no properties", resources.publicIP)}
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
				return resources, fmt.Errorf("re-read Azure cleanup disk %s before retry: %w", resources.disk, err)
			}
		} else {
			if response.Properties == nil {
				return resources, &azureCleanupSkipError{err: fmt.Errorf("Azure cleanup disk %s has no properties", resources.disk)}
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
				return resources, fmt.Errorf("re-read Azure cleanup quarantine NSG %s before retry: %w", resources.quarantineNSG, err)
			}
		} else {
			if err := validateAzureCleanupResourceTags("quarantine NSG", resources.quarantineNSG, response.Tags, labels); err != nil {
				return resources, &azureCleanupSkipError{err: err}
			}
			if response.Properties == nil {
				return resources, &azureCleanupSkipError{err: fmt.Errorf("Azure cleanup quarantine NSG %s has no properties", resources.quarantineNSG)}
			}
			if err := requireAzureCleanupIdentity("quarantine NSG", resources.quarantineNSG, stringValue(response.Properties.ResourceGUID), resources.quarantineID); err != nil {
				return resources, &azureCleanupSkipError{err: err}
			}
		}
	}
	return resources, nil
}
