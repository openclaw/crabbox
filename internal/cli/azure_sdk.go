package cli

import (
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v8"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
)

// AzureResourceClients exposes the authenticated native transport already owned
// by AzureClient. These handles do not confer cleanup authorization: adapters
// must validate the claim, identity, and current relationships before mutation.
type AzureResourceClients struct {
	VirtualMachines *armcompute.VirtualMachinesClient
	Interfaces      *armnetwork.InterfacesClient
	PublicIPs       *armnetwork.PublicIPAddressesClient
	Disks           *armcompute.DisksClient
	SecurityGroups  *armnetwork.SecurityGroupsClient
}

func (c *AzureClient) ResourceClients() AzureResourceClients {
	return AzureResourceClients{
		VirtualMachines: c.vmc, Interfaces: c.nicc, PublicIPs: c.pipc,
		Disks: c.diskc, SecurityGroups: c.sgc,
	}
}

// AzureVMCreateError is an observation from the allocating VM operation. The
// adapter, not the transport, decides whether its native code settles a lease.
type AzureVMCreateError struct {
	Terminal bool
	Err      error
}

func (e *AzureVMCreateError) Error() string { return e.Err.Error() }
func (e *AzureVMCreateError) Unwrap() error { return e.Err }
