package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
)

const (
	azureAddressSpace        = "10.42.0.0/16"
	azureSubnetCIDR          = "10.42.0.0/24"
	azureProviderTag         = "crabbox"
	defaultAzureLinuxImage   = "Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest"
	defaultAzureWindowsImage = "MicrosoftWindowsServer:windowsserver2022:2022-datacenter-smalldisk-g2:latest"
	azureDeleteRetryDelay    = 15 * time.Second
	azureDeleteRetryAttempts = 13
)

type AzureClient struct {
	SubscriptionID string
	Location       string
	ResourceGroup  string
	VNet           string
	Subnet         string
	NSG            string
	SSHCIDRs       []string
	Image          azureImageRef
	SSHPort        string
	FallbackPorts  []string

	cred   azcore.TokenCredential
	rg     *armresources.ResourceGroupsClient
	vnetc  *armnetwork.VirtualNetworksClient
	sgc    *armnetwork.SecurityGroupsClient
	pipc   *armnetwork.PublicIPAddressesClient
	nicc   *armnetwork.InterfacesClient
	vmc    *armcompute.VirtualMachinesClient
	vmextc *armcompute.VirtualMachineExtensionsClient
	diskc  *armcompute.DisksClient
	skuc   *armcompute.ResourceSKUsClient

	ephemeralOSSupport map[string]bool
}

type azureImageRef struct{ Publisher, Offer, SKU, Version string }

func NewAzureClient(ctx context.Context, cfg Config) (*AzureClient, error) {
	_ = ctx
	if cfg.AzureSubscription == "" {
		return nil, exit(3, "AZURE_SUBSCRIPTION_ID is required for direct azure provider")
	}
	if cfg.AzureLocation == "" {
		return nil, exit(3, "azure location is required (set azure.location or CRABBOX_AZURE_LOCATION)")
	}
	cred, err := azureCredentialForConfig(cfg)
	if err != nil {
		return nil, exit(3, "azure credential: %v", err)
	}
	img, err := parseAzureImageRef(azureImageForConfig(cfg))
	if err != nil {
		return nil, err
	}
	rgFactory, err := armresources.NewClientFactory(cfg.AzureSubscription, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("armresources factory: %w", err)
	}
	netFactory, err := armnetwork.NewClientFactory(cfg.AzureSubscription, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("armnetwork factory: %w", err)
	}
	cmpFactory, err := armcompute.NewClientFactory(cfg.AzureSubscription, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("armcompute factory: %w", err)
	}
	cidrs := cfg.AzureSSHCIDRs
	if len(cidrs) == 0 {
		cidrs = []string{"0.0.0.0/0"}
	}
	return &AzureClient{
		SubscriptionID: cfg.AzureSubscription,
		Location:       cfg.AzureLocation,
		ResourceGroup:  cfg.AzureResourceGroup,
		VNet:           cfg.AzureVNet,
		Subnet:         cfg.AzureSubnet,
		NSG:            cfg.AzureNSG,
		SSHCIDRs:       cidrs,
		Image:          img,
		SSHPort:        cfg.SSHPort,
		FallbackPorts:  cfg.SSHFallbackPorts,
		cred:           cred,
		rg:             rgFactory.NewResourceGroupsClient(),
		vnetc:          netFactory.NewVirtualNetworksClient(),
		sgc:            netFactory.NewSecurityGroupsClient(),
		pipc:           netFactory.NewPublicIPAddressesClient(),
		nicc:           netFactory.NewInterfacesClient(),
		vmc:            cmpFactory.NewVirtualMachinesClient(),
		vmextc:         cmpFactory.NewVirtualMachineExtensionsClient(),
		diskc:          cmpFactory.NewDisksClient(),
		skuc:           cmpFactory.NewResourceSKUsClient(),
	}, nil
}

func azureCredentialForConfig(cfg Config) (azcore.TokenCredential, error) {
	if cfg.AzureTenant != "" && cfg.AzureClientID != "" {
		if secret := os.Getenv("AZURE_CLIENT_SECRET"); secret != "" {
			return azidentity.NewClientSecretCredential(cfg.AzureTenant, cfg.AzureClientID, secret, nil)
		}
	}
	return azidentity.NewDefaultAzureCredential(nil)
}

func parseAzureImageRef(s string) (azureImageRef, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 4 {
		return azureImageRef{}, exit(2, "azure image must be Publisher:Offer:SKU:Version, got %q", s)
	}
	return azureImageRef{Publisher: parts[0], Offer: parts[1], SKU: parts[2], Version: parts[3]}, nil
}

func azureImageForConfig(cfg Config) string {
	if cfg.TargetOS == targetWindows && (cfg.AzureImage == "" || cfg.AzureImage == defaultAzureLinuxImage) {
		return defaultAzureWindowsImage
	}
	if cfg.AzureImage == "" {
		return defaultAzureLinuxImage
	}
	return cfg.AzureImage
}

func azureVMSizeCandidatesForConfig(cfg Config) []string {
	return azureVMSizeCandidatesForTargetModeClass(cfg.TargetOS, cfg.WindowsMode, cfg.Class)
}

func azureVMSizeCandidatesForTargetModeClass(target, windowsMode, class string) []string {
	switch target {
	case targetLinux:
		return azureVMSizeCandidatesForClass(class)
	case targetWindows:
		if windowsMode == windowsModeNormal {
			return azureWindowsVMSizeCandidatesForClass(class)
		}
		return []string{class}
	default:
		return []string{class}
	}
}

func azureVMSizeCandidatesForClass(class string) []string {
	switch class {
	case "standard":
		return []string{"Standard_D32ads_v6", "Standard_D32ds_v6", "Standard_F32s_v2", "Standard_D32ads_v5", "Standard_D32ds_v5", "Standard_D16ads_v6", "Standard_D16ds_v6", "Standard_F16s_v2"}
	case "fast":
		return []string{"Standard_D64ads_v6", "Standard_D64ds_v6", "Standard_F64s_v2", "Standard_D64ads_v5", "Standard_D64ds_v5", "Standard_D48ads_v6", "Standard_D48ds_v6", "Standard_F48s_v2", "Standard_D32ads_v6", "Standard_D32ds_v6", "Standard_F32s_v2"}
	case "large":
		return []string{"Standard_D96ads_v6", "Standard_D96ds_v6", "Standard_D96ads_v5", "Standard_D96ds_v5", "Standard_D64ads_v6", "Standard_D64ds_v6", "Standard_F64s_v2", "Standard_D48ads_v6", "Standard_D48ds_v6", "Standard_F48s_v2"}
	case "beast":
		return []string{"Standard_D192ds_v6", "Standard_D128ds_v6", "Standard_D96ads_v6", "Standard_D96ds_v6", "Standard_D96ads_v5", "Standard_D96ds_v5", "Standard_D64ads_v6", "Standard_D64ds_v6", "Standard_F64s_v2"}
	default:
		return []string{class}
	}
}

func azureWindowsVMSizeCandidatesForClass(class string) []string {
	switch class {
	case "standard":
		return []string{"Standard_D2ads_v6", "Standard_D2ds_v6", "Standard_D2ads_v5", "Standard_D2ds_v5", "Standard_D2as_v6"}
	case "fast":
		return []string{"Standard_D4ads_v6", "Standard_D4ds_v6", "Standard_D4ads_v5", "Standard_D4ds_v5", "Standard_D4as_v6"}
	case "large":
		return []string{"Standard_D8ads_v6", "Standard_D8ds_v6", "Standard_D8ads_v5", "Standard_D8ds_v5", "Standard_D8as_v6"}
	case "beast":
		return []string{"Standard_D16ads_v6", "Standard_D16ds_v6", "Standard_D16ads_v5", "Standard_D16ds_v5", "Standard_D8ads_v6"}
	default:
		return []string{class}
	}
}

func azureSupportsEphemeralOS(vmSize string) bool {
	normalized := strings.ToLower(vmSize)
	if strings.HasPrefix(normalized, "standard_f") && strings.HasSuffix(normalized, "s_v2") {
		return true
	}
	if (strings.HasPrefix(normalized, "standard_d") || strings.HasPrefix(normalized, "standard_e")) &&
		(strings.Contains(normalized, "ds_v5") || strings.Contains(normalized, "ds_v6")) {
		return true
	}
	return false
}

func (c *AzureClient) supportsEphemeralOS(ctx context.Context, vmSize string) bool {
	if c.skuc == nil {
		return azureSupportsEphemeralOS(vmSize)
	}
	if c.ephemeralOSSupport == nil {
		if err := c.loadEphemeralOSSupport(ctx); err != nil {
			return azureSupportsEphemeralOS(vmSize)
		}
	}
	supported, ok := c.ephemeralOSSupport[vmSize]
	if !ok {
		return azureSupportsEphemeralOS(vmSize)
	}
	return supported
}

func (c *AzureClient) loadEphemeralOSSupport(ctx context.Context) error {
	support := map[string]bool{}
	filter := fmt.Sprintf("location eq '%s'", c.Location)
	pager := c.skuc.NewListPager(&armcompute.ResourceSKUsClientListOptions{Filter: to.Ptr(filter)})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, sku := range page.Value {
			if sku == nil || sku.Name == nil || sku.ResourceType == nil || *sku.ResourceType != "virtualMachines" {
				continue
			}
			support[*sku.Name] = azureSKUCapabilityTrue(sku.Capabilities, "EphemeralOSDiskSupported")
		}
	}
	c.ephemeralOSSupport = support
	return nil
}

func azureSKUCapabilityTrue(capabilities []*armcompute.ResourceSKUCapabilities, name string) bool {
	for _, capability := range capabilities {
		if capability == nil || capability.Name == nil || capability.Value == nil {
			continue
		}
		if *capability.Name == name && strings.EqualFold(*capability.Value, "true") {
			return true
		}
	}
	return false
}

func (c *AzureClient) EnsureSharedInfra(ctx context.Context) error {
	if err := c.ensureResourceGroup(ctx); err != nil {
		return err
	}
	if err := c.ensureVNet(ctx); err != nil {
		return err
	}
	return c.ensureNSG(ctx)
}

func azureSharedTags() map[string]*string {
	return map[string]*string{
		azureProviderTag: to.Ptr("true"),
		"managed_by":     to.Ptr("crabbox"),
	}
}

func azureManagedByCrabbox(tags map[string]*string) bool {
	if tags == nil {
		return false
	}
	v := tags["managed_by"]
	if v == nil {
		return false
	}
	return *v == "crabbox"
}

func azureAdoptError(kind, name string) error {
	return fmt.Errorf("azure %s %q exists but is not Crabbox-managed; either delete it, set tag managed_by=crabbox to adopt it, or use a different name", kind, name)
}

func preserveNonCrabboxRules(rules []*armnetwork.SecurityRule) []*armnetwork.SecurityRule {
	out := make([]*armnetwork.SecurityRule, 0, len(rules))
	for _, rule := range rules {
		if rule == nil || rule.Name == nil {
			continue
		}
		if strings.HasPrefix(*rule.Name, "crabbox-ssh-") {
			continue
		}
		out = append(out, rule)
	}
	return out
}

func (c *AzureClient) ensureResourceGroup(ctx context.Context) error {
	existing, err := c.rg.Get(ctx, c.ResourceGroup, nil)
	if err == nil {
		if !azureManagedByCrabbox(existing.Tags) {
			return azureAdoptError("resource group", c.ResourceGroup)
		}
		return nil
	}
	if !isAzureNotFoundError(err) {
		return fmt.Errorf("get resource group: %w", err)
	}
	if _, err := c.rg.CreateOrUpdate(ctx, c.ResourceGroup, armresources.ResourceGroup{
		Location: to.Ptr(c.Location),
		Tags:     azureSharedTags(),
	}, nil); err != nil {
		return fmt.Errorf("create resource group: %w", err)
	}
	return nil
}

func (c *AzureClient) ensureVNet(ctx context.Context) error {
	existing, err := c.vnetc.Get(ctx, c.ResourceGroup, c.VNet, nil)
	if err == nil {
		if !azureManagedByCrabbox(existing.Tags) {
			return azureAdoptError("virtual network", c.VNet)
		}
		return nil
	}
	if !isAzureNotFoundError(err) {
		return fmt.Errorf("get vnet: %w", err)
	}
	poller, err := c.vnetc.BeginCreateOrUpdate(ctx, c.ResourceGroup, c.VNet, armnetwork.VirtualNetwork{
		Location: to.Ptr(c.Location),
		Tags:     azureSharedTags(),
		Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace: &armnetwork.AddressSpace{
				AddressPrefixes: []*string{to.Ptr(azureAddressSpace)},
			},
			Subnets: []*armnetwork.Subnet{{
				Name: to.Ptr(c.Subnet),
				Properties: &armnetwork.SubnetPropertiesFormat{
					AddressPrefix: to.Ptr(azureSubnetCIDR),
				},
			}},
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("begin vnet create: %w", err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("vnet create: %w", err)
	}
	return nil
}

func (c *AzureClient) ensureNSG(ctx context.Context) error {
	existing, err := c.sgc.Get(ctx, c.ResourceGroup, c.NSG, nil)
	existingRules := []*armnetwork.SecurityRule{}
	if err == nil {
		if !azureManagedByCrabbox(existing.Tags) {
			return azureAdoptError("network security group", c.NSG)
		}
		if existing.Properties != nil {
			existingRules = existing.Properties.SecurityRules
		}
	} else if !isAzureNotFoundError(err) {
		return fmt.Errorf("get nsg: %w", err)
	}
	rules := preserveNonCrabboxRules(existingRules)
	usedPriorities := azureNSGUsedPriorities(rules)
	for _, port := range sshPortCandidates(c.SSHPort, c.FallbackPorts) {
		for j, cidr := range c.SSHCIDRs {
			priority, err := nextAzureNSGPriority(usedPriorities)
			if err != nil {
				return err
			}
			rules = append(rules, &armnetwork.SecurityRule{
				Name: to.Ptr(fmt.Sprintf("crabbox-ssh-%s-%d", port, j)),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                 to.Ptr(armnetwork.SecurityRuleProtocolTCP),
					Access:                   to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					Priority:                 to.Ptr(priority),
					SourceAddressPrefix:      to.Ptr(cidr),
					SourcePortRange:          to.Ptr("*"),
					DestinationAddressPrefix: to.Ptr("*"),
					DestinationPortRange:     to.Ptr(port),
				},
			})
		}
	}
	poller, err := c.sgc.BeginCreateOrUpdate(ctx, c.ResourceGroup, c.NSG, armnetwork.SecurityGroup{
		Location: to.Ptr(c.Location),
		Tags:     azureSharedTags(),
		Properties: &armnetwork.SecurityGroupPropertiesFormat{
			SecurityRules: rules,
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("begin nsg create: %w", err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("nsg create: %w", err)
	}
	return nil
}

func azureNSGUsedPriorities(rules []*armnetwork.SecurityRule) map[int32]bool {
	used := map[int32]bool{}
	for _, rule := range rules {
		if rule == nil || rule.Properties == nil || rule.Properties.Priority == nil {
			continue
		}
		used[*rule.Properties.Priority] = true
	}
	return used
}

func nextAzureNSGPriority(used map[int32]bool) (int32, error) {
	for priority := int32(100); priority <= 4096; priority++ {
		if !used[priority] {
			used[priority] = true
			return priority, nil
		}
	}
	return 0, errors.New("azure nsg: no available security rule priorities")
}

func (c *AzureClient) CreateServerWithFallback(ctx context.Context, cfg Config, publicKey, leaseID, slug string, keep bool, logf func(string, ...any)) (Server, Config, error) {
	if err := c.EnsureSharedInfra(ctx); err != nil {
		return Server{}, cfg, err
	}
	var candidates []string
	if cfg.ServerTypeExplicit && cfg.ServerType != "" {
		candidates = []string{cfg.ServerType}
	} else {
		candidates = azureVMSizeCandidatesForConfig(cfg)
		if cfg.ServerType != "" && cfg.ServerType != candidates[0] {
			candidates = append([]string{cfg.ServerType}, candidates...)
		}
	}
	var errs []error
	for i, vmSize := range candidates {
		next := cfg
		next.ServerType = vmSize
		if i > 0 && logf != nil {
			logf("fallback provisioning type=%s after quota/capacity rejection\n", vmSize)
		}
		server, err := c.createServer(ctx, next, publicKey, leaseID, slug, keep)
		if err == nil {
			return server, next, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", vmSize, err))
		if !isAzureRetryableProvisioningError(err) {
			return Server{}, next, joinErrors(errs)
		}
	}
	if strings.EqualFold(cfg.Capacity.Market, "spot") && strings.HasPrefix(cfg.Capacity.Fallback, "on-demand") {
		for _, vmSize := range candidates {
			next := cfg
			next.ServerType = vmSize
			next.Capacity.Market = "on-demand"
			if logf != nil {
				logf("fallback provisioning type=%s market=on-demand after spot rejection\n", vmSize)
			}
			server, err := c.createServer(ctx, next, publicKey, leaseID, slug, keep)
			if err == nil {
				return server, next, nil
			}
			errs = append(errs, fmt.Errorf("on-demand %s: %w", vmSize, err))
			if !isAzureRetryableProvisioningError(err) {
				return Server{}, next, joinErrors(errs)
			}
		}
	}
	return Server{}, cfg, joinErrors(errs)
}

func (c *AzureClient) createServer(ctx context.Context, cfg Config, publicKey, leaseID, slug string, keep bool) (server Server, err error) {
	name := leaseProviderName(leaseID, slug)
	defer func() {
		if err == nil {
			return
		}
		_ = c.deleteVMResources(context.Background(), name)
	}()
	return c.createServerSteps(ctx, cfg, publicKey, leaseID, slug, keep, name)
}

func (c *AzureClient) createServerSteps(ctx context.Context, cfg Config, publicKey, leaseID, slug string, keep bool, name string) (Server, error) {
	pipName := name + "-pip"
	nicName := name + "-nic"
	diskName := name + "-osdisk"

	if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
		cfg.Tailscale.Hostname = renderTailscaleHostname(cfg.Tailscale.HostnameTemplate, leaseID, slug, cfg.Provider)
	}
	now := time.Now().UTC()
	labels := directLeaseLabels(cfg, leaseID, slug, "azure", mapMarket(strings.EqualFold(cfg.Capacity.Market, "spot")), keep, now)
	tags := azureLabelsToTags(labels)

	pipPoller, err := c.pipc.BeginCreateOrUpdate(ctx, c.ResourceGroup, pipName, armnetwork.PublicIPAddress{
		Location: to.Ptr(c.Location),
		Tags:     tags,
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
		},
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
		},
	}, nil)
	if err != nil {
		return Server{}, fmt.Errorf("begin public ip: %w", err)
	}
	pipResp, err := pipPoller.PollUntilDone(ctx, nil)
	if err != nil {
		return Server{}, fmt.Errorf("public ip: %w", err)
	}
	pipID := *pipResp.ID

	subnetID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/virtualNetworks/%s/subnets/%s",
		c.SubscriptionID, c.ResourceGroup, c.VNet, c.Subnet)
	nsgID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/networkSecurityGroups/%s",
		c.SubscriptionID, c.ResourceGroup, c.NSG)
	nicPoller, err := c.nicc.BeginCreateOrUpdate(ctx, c.ResourceGroup, nicName, armnetwork.Interface{
		Location: to.Ptr(c.Location),
		Tags:     tags,
		Properties: &armnetwork.InterfacePropertiesFormat{
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{
				Name: to.Ptr("ipconfig"),
				Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
					Subnet:                    &armnetwork.Subnet{ID: to.Ptr(subnetID)},
					PublicIPAddress:           &armnetwork.PublicIPAddress{ID: to.Ptr(pipID)},
				},
			}},
			NetworkSecurityGroup: &armnetwork.SecurityGroup{ID: to.Ptr(nsgID)},
		},
	}, nil)
	if err != nil {
		return Server{}, fmt.Errorf("begin nic: %w", err)
	}
	nicResp, err := nicPoller.PollUntilDone(ctx, nil)
	if err != nil {
		return Server{}, fmt.Errorf("nic: %w", err)
	}
	nicID := *nicResp.ID

	osProfile, err := c.azureOSProfile(cfg, publicKey, name, leaseID)
	if err != nil {
		return Server{}, err
	}
	osDisk := &armcompute.OSDisk{
		Name:         to.Ptr(diskName),
		CreateOption: to.Ptr(armcompute.DiskCreateOptionTypesFromImage),
	}
	if c.supportsEphemeralOS(ctx, cfg.ServerType) {
		osDisk.Caching = to.Ptr(armcompute.CachingTypesReadOnly)
		osDisk.DiffDiskSettings = &armcompute.DiffDiskSettings{
			Option: to.Ptr(armcompute.DiffDiskOptionsLocal),
		}
	} else {
		osDisk.Caching = to.Ptr(armcompute.CachingTypesReadWrite)
		osDisk.ManagedDisk = &armcompute.ManagedDiskParameters{
			StorageAccountType: to.Ptr(armcompute.StorageAccountTypesStandardSSDLRS),
		}
	}
	vmProperties := &armcompute.VirtualMachineProperties{
		HardwareProfile: &armcompute.HardwareProfile{
			VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(cfg.ServerType)),
		},
		StorageProfile: &armcompute.StorageProfile{
			ImageReference: &armcompute.ImageReference{
				Publisher: to.Ptr(c.Image.Publisher),
				Offer:     to.Ptr(c.Image.Offer),
				SKU:       to.Ptr(c.Image.SKU),
				Version:   to.Ptr(c.Image.Version),
			},
			OSDisk: osDisk,
		},
		OSProfile: osProfile,
		NetworkProfile: &armcompute.NetworkProfile{
			NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{
				ID: to.Ptr(nicID),
			}},
		},
	}
	if strings.EqualFold(cfg.Capacity.Market, "spot") {
		vmProperties.Priority = to.Ptr(armcompute.VirtualMachinePriorityTypesSpot)
		vmProperties.EvictionPolicy = to.Ptr(armcompute.VirtualMachineEvictionPolicyTypesDelete)
	}
	vmPoller, err := c.vmc.BeginCreateOrUpdate(ctx, c.ResourceGroup, name, armcompute.VirtualMachine{
		Location:   to.Ptr(c.Location),
		Tags:       tags,
		Properties: vmProperties,
	}, nil)
	if err != nil {
		return Server{}, fmt.Errorf("begin vm: %w", err)
	}
	vmResp, err := vmPoller.PollUntilDone(ctx, nil)
	if err != nil {
		return Server{}, fmt.Errorf("vm: %w", err)
	}
	if cfg.TargetOS == targetWindows {
		if err := c.installWindowsBootstrapExtension(ctx, name, tags); err != nil {
			return Server{}, err
		}
	}
	return azureVMToServer(vmResp.VirtualMachine, ""), nil
}

func (c *AzureClient) azureOSProfile(cfg Config, publicKey, name, leaseID string) (*armcompute.OSProfile, error) {
	if cfg.TargetOS != targetWindows {
		sshPath := fmt.Sprintf("/home/%s/.ssh/authorized_keys", cfg.SSHUser)
		return &armcompute.OSProfile{
			ComputerName:  to.Ptr(name),
			AdminUsername: to.Ptr(cfg.SSHUser),
			CustomData:    to.Ptr(base64.StdEncoding.EncodeToString([]byte(cloudInit(cfg, publicKey)))),
			LinuxConfiguration: &armcompute.LinuxConfiguration{
				DisablePasswordAuthentication: to.Ptr(true),
				SSH: &armcompute.SSHConfiguration{
					PublicKeys: []*armcompute.SSHPublicKey{{
						Path:    to.Ptr(sshPath),
						KeyData: to.Ptr(publicKey),
					}},
				},
			},
		}, nil
	}
	password, err := azureRandomAdminPassword()
	if err != nil {
		return nil, err
	}
	return &armcompute.OSProfile{
		ComputerName:             to.Ptr(azureComputerName(name, leaseID, cfg.TargetOS)),
		AdminUsername:            to.Ptr("crabadmin"),
		AdminPassword:            to.Ptr(password),
		AllowExtensionOperations: to.Ptr(true),
		CustomData:               to.Ptr(base64.StdEncoding.EncodeToString([]byte(azureWindowsBootstrapPowerShell(cfg, publicKey)))),
		WindowsConfiguration: &armcompute.WindowsConfiguration{
			EnableAutomaticUpdates: to.Ptr(false),
			ProvisionVMAgent:       to.Ptr(true),
		},
	}, nil
}

func (c *AzureClient) installWindowsBootstrapExtension(ctx context.Context, vmName string, tags map[string]*string) error {
	poller, err := c.vmextc.BeginCreateOrUpdate(ctx, c.ResourceGroup, vmName, "crabbox-bootstrap", armcompute.VirtualMachineExtension{
		Location: to.Ptr(c.Location),
		Tags:     tags,
		Properties: &armcompute.VirtualMachineExtensionProperties{
			Publisher:               to.Ptr("Microsoft.Compute"),
			Type:                    to.Ptr("CustomScriptExtension"),
			TypeHandlerVersion:      to.Ptr("1.10"),
			AutoUpgradeMinorVersion: to.Ptr(true),
			Settings:                map[string]any{"timestamp": time.Now().Unix()},
			ProtectedSettings: map[string]any{
				"commandToExecute": azureWindowsBootstrapCommand(),
			},
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("begin windows bootstrap extension: %w", err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("windows bootstrap extension: %w", err)
	}
	return nil
}

func azureWindowsBootstrapCommand() string {
	return `powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "$p=Join-Path $env:SystemDrive 'AzureData\CustomData.bin'; $d=Join-Path $env:SystemDrive 'AzureData\crabbox-bootstrap.ps1'; Copy-Item -Force $p $d; & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $d"`
}

func azureRandomAdminPassword() (string, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate azure admin password: %w", err)
	}
	return "Cb1!" + base64.StdEncoding.EncodeToString(b[:])[:18], nil
}

func azureComputerName(vmName, leaseID, target string) string {
	if target != targetWindows {
		return vmName
	}
	source := leaseID
	if source == "" {
		source = vmName
	}
	var b strings.Builder
	for _, r := range strings.ToLower(source) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	suffix := b.String()
	if suffix == "" {
		suffix = "windows"
	}
	if len(suffix) > 12 {
		suffix = suffix[:12]
	}
	return "cbx" + suffix
}

func (c *AzureClient) WaitForServerIP(ctx context.Context, name string) (Server, error) {
	pipName := name + "-pip"
	deadline := time.Now().Add(2 * time.Minute)
	for {
		pip, err := c.pipc.Get(ctx, c.ResourceGroup, pipName, nil)
		if err != nil {
			return Server{}, err
		}
		if pip.Properties != nil && pip.Properties.IPAddress != nil && *pip.Properties.IPAddress != "" {
			vm, err := c.vmc.Get(ctx, c.ResourceGroup, name, nil)
			if err != nil {
				return Server{}, err
			}
			return azureVMToServer(vm.VirtualMachine, *pip.Properties.IPAddress), nil
		}
		if time.Now().After(deadline) {
			return Server{}, fmt.Errorf("timeout waiting for public ip on %s", name)
		}
		select {
		case <-ctx.Done():
			return Server{}, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (c *AzureClient) GetServer(ctx context.Context, name string) (Server, error) {
	vm, err := c.vmc.Get(ctx, c.ResourceGroup, name, nil)
	if err != nil {
		return Server{}, err
	}
	pipName := name + "-pip"
	ip := ""
	if pip, err := c.pipc.Get(ctx, c.ResourceGroup, pipName, nil); err == nil && pip.Properties != nil && pip.Properties.IPAddress != nil {
		ip = *pip.Properties.IPAddress
	}
	return azureVMToServer(vm.VirtualMachine, ip), nil
}

func (c *AzureClient) ListCrabboxServers(ctx context.Context) ([]Server, error) {
	pager := c.vmc.NewListPager(c.ResourceGroup, nil)
	var servers []Server
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			if isAzureNotFoundError(err) {
				return servers, nil
			}
			return nil, err
		}
		for _, vm := range page.Value {
			if vm == nil {
				continue
			}
			if vm.Tags == nil || vm.Tags[azureProviderTag] == nil || *vm.Tags[azureProviderTag] != "true" {
				continue
			}
			ip := ""
			if vm.Name != nil {
				pipName := *vm.Name + "-pip"
				if pip, err := c.pipc.Get(ctx, c.ResourceGroup, pipName, nil); err == nil && pip.Properties != nil && pip.Properties.IPAddress != nil {
					ip = *pip.Properties.IPAddress
				}
			}
			servers = append(servers, azureVMToServer(*vm, ip))
		}
	}
	return servers, nil
}

func (c *AzureClient) DeleteServer(ctx context.Context, name string) error {
	return c.deleteVMResources(ctx, name)
}

func (c *AzureClient) deleteVMResources(ctx context.Context, name string) error {
	for attempt := 0; ; attempt++ {
		errs, retry := c.deleteVMResourcesOnce(ctx, name)
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
	}
}

func (c *AzureClient) deleteVMResourcesOnce(ctx context.Context, name string) ([]error, bool) {
	var errs []error
	retry := false
	if poller, err := c.vmc.BeginDelete(ctx, c.ResourceGroup, name, nil); err == nil {
		if _, err := poller.PollUntilDone(ctx, nil); err != nil && !isAzureNotFoundError(err) {
			errs = append(errs, fmt.Errorf("delete vm %s: %w", name, err))
			retry = retry || isAzureRetryableDeleteError(err)
		}
	} else if !isAzureNotFoundError(err) {
		errs = append(errs, fmt.Errorf("begin delete vm: %w", err))
		retry = retry || isAzureRetryableDeleteError(err)
	}
	if poller, err := c.nicc.BeginDelete(ctx, c.ResourceGroup, name+"-nic", nil); err == nil {
		if _, err := poller.PollUntilDone(ctx, nil); err != nil && !isAzureNotFoundError(err) {
			errs = append(errs, fmt.Errorf("delete nic %s-nic: %w", name, err))
			retry = retry || isAzureRetryableDeleteError(err)
		}
	} else if !isAzureNotFoundError(err) {
		errs = append(errs, fmt.Errorf("begin delete nic: %w", err))
		retry = retry || isAzureRetryableDeleteError(err)
	}
	if err := c.deletePublicIP(ctx, name+"-pip"); err != nil {
		errs = append(errs, err)
		retry = retry || isAzureRetryableDeleteError(err)
	}
	if poller, err := c.diskc.BeginDelete(ctx, c.ResourceGroup, name+"-osdisk", nil); err == nil {
		if _, err := poller.PollUntilDone(ctx, nil); err != nil && !isAzureNotFoundError(err) {
			errs = append(errs, fmt.Errorf("delete disk %s-osdisk: %w", name, err))
			retry = retry || isAzureRetryableDeleteError(err)
		}
	} else if !isAzureNotFoundError(err) {
		errs = append(errs, fmt.Errorf("begin delete disk: %w", err))
		retry = retry || isAzureRetryableDeleteError(err)
	}
	return errs, retry
}

func (c *AzureClient) deletePublicIP(ctx context.Context, pipName string) error {
	poller, err := c.pipc.BeginDelete(ctx, c.ResourceGroup, pipName, nil)
	if err != nil {
		if isAzureNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("begin delete pip: %w", err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil && !isAzureNotFoundError(err) {
		return fmt.Errorf("delete pip %s: %w", pipName, err)
	}
	return nil
}

func (c *AzureClient) SetTags(ctx context.Context, name string, labels map[string]string) error {
	poller, err := c.vmc.BeginUpdate(ctx, c.ResourceGroup, name, armcompute.VirtualMachineUpdate{
		Tags: azureLabelsToTags(labels),
	}, nil)
	if err != nil {
		return err
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return err
	}
	return nil
}

func azureVMToServer(vm armcompute.VirtualMachine, ip string) Server {
	s := Server{
		Provider: "azure",
		Labels:   map[string]string{},
	}
	if vm.Name != nil {
		s.CloudID = *vm.Name
		s.Name = *vm.Name
	}
	if vm.Properties != nil && vm.Properties.ProvisioningState != nil {
		s.Status = *vm.Properties.ProvisioningState
	}
	if vm.Properties != nil && vm.Properties.HardwareProfile != nil && vm.Properties.HardwareProfile.VMSize != nil {
		s.ServerType.Name = string(*vm.Properties.HardwareProfile.VMSize)
	}
	s.PublicNet.IPv4.IP = ip
	for k, v := range vm.Tags {
		if v != nil {
			s.Labels[azureTagToLabelKey(k)] = *v
		}
	}
	normalizeAzureWindowsModeLabel(s.Labels)
	return s
}

func azureLabelsToTags(labels map[string]string) map[string]*string {
	return stringMapToPtrMap(azureTagsFromLabels(labels))
}

func azureTagsFromLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[azureLabelToTagKey(k)] = v
	}
	return out
}

func azureLabelToTagKey(key string) string {
	if strings.HasPrefix(strings.ToLower(key), "windows") {
		return "crabbox_" + key
	}
	return key
}

func azureTagToLabelKey(key string) string {
	if strings.HasPrefix(key, "crabbox_windows") {
		return strings.TrimPrefix(key, "crabbox_")
	}
	return key
}

func normalizeAzureWindowsModeLabel(labels map[string]string) {
	if labels == nil {
		return
	}
	if labels["windows_mode"] == "" && labels["crabbox_windows_mode"] != "" {
		labels["windows_mode"] = labels["crabbox_windows_mode"]
	}
}

func stringMapToPtrMap(m map[string]string) map[string]*string {
	out := make(map[string]*string, len(m))
	for k, v := range m {
		out[k] = to.Ptr(v)
	}
	return out
}

func isAzureRetryableProvisioningError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "SkuNotAvailable") ||
		strings.Contains(s, "QuotaExceeded") ||
		strings.Contains(s, "OperationNotAllowed") ||
		strings.Contains(s, "AllocationFailed") ||
		strings.Contains(s, "ZonalAllocationFailed") ||
		strings.Contains(s, "OverconstrainedAllocationRequest")
}

func isAzureNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) && respErr.StatusCode == 404 {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "ResourceNotFound") || strings.Contains(s, "NotFound")
}

func isAzureRetryableDeleteError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "NicReservedForAnotherVm") ||
		strings.Contains(s, "PublicIPAddressCannotBeDeleted") ||
		strings.Contains(s, "InUse") ||
		strings.Contains(s, "AnotherOperationInProgress") ||
		(strings.Contains(s, "OperationNotAllowed") && strings.Contains(s, "retry after"))
}

func deleteAzureServer(ctx context.Context, cfg Config, server Server) error {
	client, err := NewAzureClient(ctx, cfg)
	if err != nil {
		return err
	}
	name := server.CloudID
	if name == "" {
		name = server.Name
	}
	if name == "" {
		return errors.New("azure delete: server has no name")
	}
	return client.DeleteServer(ctx, name)
}
