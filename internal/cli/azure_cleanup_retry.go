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
				return resources, &azureCleanupResourceReadError{err: fmt.Errorf("re-read Azure cleanup public IP %s before retry: %w", resources.publicIP, err)}
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
				return resources, &azureCleanupResourceReadError{err: fmt.Errorf("re-read Azure cleanup disk %s before retry: %w", resources.disk, err)}
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
				return resources, &azureCleanupResourceReadError{err: fmt.Errorf("re-read Azure cleanup quarantine NSG %s before retry: %w", resources.quarantineNSG, err)}
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
	return resources, nil
}
