package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
)

const (
	azureAddressSpace             = "10.42.0.0/16"
	azureSubnetCIDR               = "10.42.0.0/24"
	azureProviderTag              = "crabbox"
	AzureOSDiskAuto               = "auto"
	AzureOSDiskEphemeral          = "ephemeral"
	AzureOSDiskEphemeralPreview   = "ephemeral-preview"
	AzureOSDiskManaged            = "managed"
	azureComputePreviewAPIVersion = "2025-04-01"
	defaultAzureLinuxImage        = "Canonical:ubuntu-26_04-lts:server:latest"
	defaultAzureLinuxARM64Image   = "Canonical:ubuntu-26_04-lts:server-arm64:latest"
	azureNobleLinuxImage          = "Canonical:ubuntu-24_04-lts:server:latest"
	azureNobleLinuxARM64Image     = "Canonical:ubuntu-24_04-lts:server-arm64:latest"
	legacyAzureJammyImage         = "Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest"
	legacyAzureNobleGen2Image     = "Canonical:0001-com-ubuntu-server-noble:24_04-lts-gen2:latest"
	defaultAzureWindowsImage      = "MicrosoftWindowsServer:windowsserver2022:2022-datacenter-smalldisk-g2:latest"
	azureDeleteRetryDelay         = 15 * time.Second
	azureDeleteRetryAttempts      = 13
)

type AzureClient struct {
	SubscriptionID string
	Location       string
	ResourceGroup  string
	VNet           string
	Subnet         string
	NSG            string
	SSHCIDRs       []string
	Network        string
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
	snapc  *armcompute.SnapshotsClient
	skuc   *armcompute.ResourceSKUsClient

	ephemeralOSSupport map[string]bool
}

type azureImageRef struct{ Publisher, Offer, SKU, Version string }

func NewAzureClient(ctx context.Context, cfg Config) (*AzureClient, error) {
	if cfg.AzureSubscription == "" {
		info, err := azAccountShow(ctx, "")
		if err != nil {
			return nil, exit(3, "AZURE_SUBSCRIPTION_ID is required for direct azure provider (or run 'az login' and 'crabbox azure login'): %v", err)
		}
		cfg.AzureSubscription = info.ID
		if cfg.AzureTenant == "" {
			cfg.AzureTenant = info.TenantID
		}
		fmt.Fprintf(os.Stderr, "using azure subscription from az cli: %s (%s)\n", info.Name, info.ID)
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
	return &AzureClient{
		SubscriptionID: cfg.AzureSubscription,
		Location:       cfg.AzureLocation,
		ResourceGroup:  cfg.AzureResourceGroup,
		VNet:           cfg.AzureVNet,
		Subnet:         cfg.AzureSubnet,
		NSG:            cfg.AzureNSG,
		SSHCIDRs:       cfg.AzureSSHCIDRs,
		Network:        cfg.AzureNetwork,
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
		snapc:          cmpFactory.NewSnapshotsClient(),
		skuc:           cmpFactory.NewResourceSKUsClient(),
	}, nil
}

func azureSSHCIDRsForConfig(ctx context.Context, cfg Config) ([]string, error) {
	if len(cfg.AzureSSHCIDRs) > 0 {
		return cfg.AzureSSHCIDRs, nil
	}
	cidr, err := detectOutboundIPv4CIDRFunc(ctx)
	if err == nil && cidr != "" {
		return []string{cidr}, nil
	}
	if err != nil {
		return nil, exit(3, "azure ssh CIDRs are not configured and outbound IPv4 detection failed: %v; set CRABBOX_AZURE_SSH_CIDRS explicitly (use 0.0.0.0/0 only if world-open SSH is intentional)", err)
	}
	return nil, exit(3, "azure ssh CIDRs are not configured and outbound IPv4 detection returned no IPv4 address; set CRABBOX_AZURE_SSH_CIDRS explicitly (use 0.0.0.0/0 only if world-open SSH is intentional)")
}

func azureSSHCIDRsForRules(ctx context.Context, cfg Config, existingRules []*armnetwork.SecurityRule) ([]string, error) {
	if len(cfg.AzureSSHCIDRs) > 0 {
		return cfg.AzureSSHCIDRs, nil
	}
	if strings.EqualFold(strings.TrimSpace(cfg.AzureNetwork), "private") {
		return nil, exit(3, "azure private network SSH CIDRs are not configured; set CRABBOX_AZURE_SSH_CIDRS to the VPN/VNet source CIDR explicitly")
	}
	detected, err := azureSSHCIDRsForConfig(ctx, Config{})
	if err != nil {
		return nil, err
	}
	existing := azureExistingCrabboxSSHCIDRs(existingRules)
	if len(existing) == 0 {
		return detected, nil
	}
	for _, cidr := range detected {
		if slices.Contains(existing, cidr) {
			return existing, nil
		}
	}
	return nil, exit(3, "azure ssh CIDRs are not configured and this shared NSG already has managed SSH CIDRs %s; set CRABBOX_AZURE_SSH_CIDRS explicitly to replace or extend them", strings.Join(existing, ","))
}

func azureExistingCrabboxSSHCIDRs(rules []*armnetwork.SecurityRule) []string {
	out := []string{}
	for _, rule := range rules {
		if rule == nil || rule.Name == nil || !strings.HasPrefix(*rule.Name, "crabbox-ssh-") || rule.Properties == nil || rule.Properties.SourceAddressPrefix == nil {
			continue
		}
		cidr := strings.TrimSpace(*rule.Properties.SourceAddressPrefix)
		if cidr == "" || cidr == "0.0.0.0/0" || cidr == "::/0" {
			continue
		}
		out = appendUniqueStrings(out, cidr)
	}
	return out
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
	if cfg.TargetOS == targetWindows && (cfg.AzureImage == "" || isAzureDefaultLinuxImage(cfg.AzureImage)) {
		return defaultAzureWindowsImage
	}
	if cfg.TargetOS == targetLinux && effectiveArchitectureForConfig(cfg) == ArchitectureARM64 && (cfg.AzureImage == "" || isAzureDefaultLinuxImage(cfg.AzureImage)) {
		if cfg.OSImage == "ubuntu:24.04" {
			return azureNobleLinuxARM64Image
		}
		return defaultAzureLinuxARM64Image
	}
	if cfg.AzureImage == "" {
		return defaultAzureLinuxImage
	}
	return cfg.AzureImage
}

func isAzureDefaultLinuxImage(image string) bool {
	switch strings.TrimSpace(image) {
	case defaultAzureLinuxImage, defaultAzureLinuxARM64Image, azureNobleLinuxImage, azureNobleLinuxARM64Image, legacyAzureJammyImage, legacyAzureNobleGen2Image:
		return true
	default:
		return false
	}
}

func azureWindowsARM64HasExplicitImage(cfg Config) bool {
	image := strings.TrimSpace(cfg.AzureImage)
	return image != "" && image != defaultAzureWindowsImage && !isAzureDefaultLinuxImage(image)
}

func azureVMSizeCandidatesForTargetModeClass(target, windowsMode, class string) []string {
	switch target {
	case targetLinux:
		return azureVMSizeCandidatesForArchitectureClass(ArchitectureAMD64, class)
	case targetWindows:
		if windowsMode == windowsModeNormal || windowsMode == windowsModeWSL2 {
			return azureWindowsVMSizeCandidatesForClass(class)
		}
		return []string{class}
	default:
		return []string{class}
	}
}

func azureVMSizeCandidatesForClass(class string) []string {
	return azureVMSizeCandidatesForArchitectureClass(ArchitectureAMD64, class)
}

func azureVMSizeCandidatesForConfig(cfg Config) []string {
	candidates := azureVMSizeCandidatesForTargetModeArchitectureClass(cfg.TargetOS, cfg.WindowsMode, effectiveArchitectureForConfig(cfg), cfg.Class)
	mode, err := NormalizeAzureOSDiskMode(cfg.AzureOSDisk)
	if cfg.AzureSnapshot != "" {
		mode = AzureOSDiskManaged
	}
	if err != nil || !azureOSDiskUsesFullCaching(mode) {
		return candidates
	}
	return azureEphemeralFullCachingCandidates(cfg, candidates)
}

func azureVMSizeCandidatesForTargetModeArchitectureClass(target, windowsMode, architecture, class string) []string {
	switch target {
	case targetLinux:
		return azureVMSizeCandidatesForArchitectureClass(architecture, class)
	case targetWindows:
		if windowsMode == windowsModeNormal || windowsMode == windowsModeWSL2 {
			if architecture == ArchitectureARM64 {
				if windowsMode == windowsModeWSL2 {
					return []string{class}
				}
				return azureARM64VMSizeCandidatesForClass(class)
			}
			return azureWindowsVMSizeCandidatesForClass(class)
		}
		return []string{class}
	default:
		return []string{class}
	}
}

func azureVMSizeCandidatesForArchitectureClass(architecture, class string) []string {
	if architecture == ArchitectureARM64 {
		return azureARM64VMSizeCandidatesForClass(class)
	}
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

func azureARM64VMSizeCandidatesForClass(class string) []string {
	switch class {
	case "standard":
		return []string{"Standard_D32pds_v6", "Standard_D32ps_v6", "Standard_D16pds_v6", "Standard_D16ps_v6"}
	case "fast":
		return []string{"Standard_D64pds_v6", "Standard_D64ps_v6", "Standard_D48pds_v6", "Standard_D48ps_v6", "Standard_D32pds_v6", "Standard_D32ps_v6"}
	case "large":
		return []string{"Standard_D96pds_v6", "Standard_D96ps_v6", "Standard_D64pds_v6", "Standard_D64ps_v6", "Standard_D48pds_v6", "Standard_D48ps_v6"}
	case "beast":
		return []string{"Standard_D96pds_v6", "Standard_D96ps_v6", "Standard_D64pds_v6", "Standard_D64ps_v6"}
	default:
		return []string{class}
	}
}

func azureVMSizeIsARM64(vmSize string) bool {
	normalized := strings.ToLower(vmSize)
	return strings.Contains(normalized, "ps_v6") || strings.Contains(normalized, "pds_v6") || strings.Contains(normalized, "pls_v6") || strings.Contains(normalized, "plds_v6")
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

func azureEphemeralFullCachingCandidates(cfg Config, candidates []string) []string {
	filtered := filterAzureEphemeralFullCachingCandidates(candidates)
	if len(filtered) > 0 {
		return filtered
	}
	if cfg.TargetOS == targetWindows {
		return filterAzureEphemeralFullCachingCandidates(appendUniqueStrings(
			azureVMSizeCandidatesForTargetModeArchitectureClass(targetWindows, cfg.WindowsMode, effectiveArchitectureForConfig(cfg), "large"),
			azureVMSizeCandidatesForTargetModeArchitectureClass(targetWindows, cfg.WindowsMode, effectiveArchitectureForConfig(cfg), "beast")...,
		))
	}
	return candidates
}

func filterAzureEphemeralFullCachingCandidates(candidates []string) []string {
	var filtered []string
	for _, candidate := range candidates {
		if azureSupportsEphemeralFullCaching(candidate) {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func azureSupportsEphemeralOS(vmSize string) bool {
	normalized := strings.ToLower(vmSize)
	if strings.HasPrefix(normalized, "standard_f") && strings.HasSuffix(normalized, "s_v2") {
		return true
	}
	if strings.Contains(normalized, "pds_v6") || strings.Contains(normalized, "plds_v6") {
		return true
	}
	if (strings.HasPrefix(normalized, "standard_d") || strings.HasPrefix(normalized, "standard_e")) &&
		(strings.Contains(normalized, "ds_v5") || strings.Contains(normalized, "ds_v6")) {
		return true
	}
	return false
}

func azureSupportsEphemeralFullCaching(vmSize string) bool {
	if !azureSupportsEphemeralOS(vmSize) {
		return false
	}
	cores, ok := azureVMSizeVCPUCount(vmSize)
	if !ok {
		return false
	}
	return cores > 4
}

func azureVMSizeVCPUCount(vmSize string) (int, bool) {
	normalized := strings.ToLower(strings.TrimSpace(vmSize))
	if !strings.HasPrefix(normalized, "standard_") {
		return 0, false
	}
	for i := len("standard_"); i < len(normalized); i++ {
		if normalized[i] < '0' || normalized[i] > '9' {
			continue
		}
		j := i + 1
		for j < len(normalized) && normalized[j] >= '0' && normalized[j] <= '9' {
			j++
		}
		cores, err := strconv.Atoi(normalized[i:j])
		return cores, err == nil
	}
	return 0, false
}

func NormalizeAzureOSDiskMode(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return AzureOSDiskManaged, nil
	case AzureOSDiskAuto:
		return AzureOSDiskManaged, nil
	case AzureOSDiskEphemeral:
		return AzureOSDiskEphemeral, nil
	case AzureOSDiskEphemeralPreview:
		return AzureOSDiskEphemeralPreview, nil
	case AzureOSDiskManaged:
		return AzureOSDiskManaged, nil
	default:
		return "", exit(2, "azure.osDisk must be auto, managed, ephemeral, or ephemeral-preview")
	}
}

func azureOSDiskIsEphemeral(mode string) bool {
	return mode == AzureOSDiskEphemeral || mode == AzureOSDiskEphemeralPreview
}

func azureOSDiskUsesFullCaching(mode string) bool {
	return mode == AzureOSDiskEphemeralPreview
}

func (c *AzureClient) useEphemeralOSDisk(ctx context.Context, cfg Config) (bool, error) {
	mode, err := c.validatedAzureOSDiskMode(ctx, cfg)
	if err != nil {
		return false, err
	}
	return azureOSDiskIsEphemeral(mode), nil
}

func (c *AzureClient) validatedAzureOSDiskMode(ctx context.Context, cfg Config) (string, error) {
	mode, err := NormalizeAzureOSDiskMode(cfg.AzureOSDisk)
	if err != nil {
		return "", err
	}
	if !azureOSDiskIsEphemeral(mode) {
		return mode, nil
	}
	supported := c.supportsEphemeralOS(ctx, cfg.ServerType)
	if !supported {
		return "", exit(2, "azure.osDisk=%s requires an Azure VM size with ephemeral OS disk support; %s is not supported", mode, cfg.ServerType)
	}
	if azureOSDiskUsesFullCaching(mode) && !azureSupportsEphemeralFullCaching(cfg.ServerType) {
		return "", exit(2, "azure.osDisk=ephemeral-preview requires a full-caching preview Azure VM size; %s is not supported because preview full caching requires more than 4 vCPUs and local storage larger than 2x the OS disk plus 1 GiB", cfg.ServerType)
	}
	return mode, nil
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
	if err := c.regionalizeSharedInfraNamesForLocation(ctx); err != nil {
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

func azureLocationKey(location string) string {
	return strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return '-'
	}, strings.TrimSpace(location)), "-")
}

func azureSameLocation(existing *string, desired string) bool {
	if existing == nil || strings.TrimSpace(desired) == "" {
		return true
	}
	return azureLocationKey(*existing) == azureLocationKey(desired)
}

func azureSharedLocationError(kind, name string, existing *string, desired string) error {
	location := ""
	if existing != nil {
		location = *existing
	}
	return fmt.Errorf("azure %s %q exists in location %q, not %q; use a region-scoped name such as %q", kind, name, location, desired, azureRegionalName(name, desired))
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

func (c *AzureClient) regionalizeSharedInfraNamesForLocation(ctx context.Context) error {
	mismatch := false
	existingVNet, err := c.vnetc.Get(ctx, c.ResourceGroup, c.VNet, nil)
	if err == nil {
		if !azureManagedByCrabbox(existingVNet.Tags) {
			return azureAdoptError("virtual network", c.VNet)
		}
		mismatch = mismatch || !azureSameLocation(existingVNet.Location, c.Location)
	} else if !isAzureNotFoundError(err) {
		return fmt.Errorf("get vnet: %w", err)
	}
	existingNSG, err := c.sgc.Get(ctx, c.ResourceGroup, c.NSG, nil)
	if err == nil {
		if !azureManagedByCrabbox(existingNSG.Tags) {
			return azureAdoptError("network security group", c.NSG)
		}
		mismatch = mismatch || !azureSameLocation(existingNSG.Location, c.Location)
	} else if !isAzureNotFoundError(err) {
		return fmt.Errorf("get nsg: %w", err)
	}
	if mismatch {
		c.VNet = azureRegionalName(c.VNet, c.Location)
		c.NSG = azureRegionalName(c.NSG, c.Location)
	}
	return nil
}

func (c *AzureClient) ensureVNet(ctx context.Context) error {
	existing, err := c.vnetc.Get(ctx, c.ResourceGroup, c.VNet, nil)
	if err == nil {
		if !azureManagedByCrabbox(existing.Tags) {
			return azureAdoptError("virtual network", c.VNet)
		}
		if !azureSameLocation(existing.Location, c.Location) {
			return azureSharedLocationError("virtual network", c.VNet, existing.Location, c.Location)
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
		if !azureSameLocation(existing.Location, c.Location) {
			return azureSharedLocationError("network security group", c.NSG, existing.Location, c.Location)
		}
		if existing.Properties != nil {
			existingRules = existing.Properties.SecurityRules
		}
	} else if !isAzureNotFoundError(err) {
		return fmt.Errorf("get nsg: %w", err)
	}
	rules := preserveNonCrabboxRules(existingRules)
	usedPriorities := azureNSGUsedPriorities(rules)
	cidrs, err := azureSSHCIDRsForRules(ctx, Config{AzureSSHCIDRs: c.SSHCIDRs, AzureNetwork: c.Network}, existingRules)
	if err != nil {
		return err
	}
	for _, port := range sshPortCandidates(c.SSHPort, c.FallbackPorts) {
		for j, cidr := range cidrs {
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
	regions := azureRegionCandidates(cfg, c.Location)
	var errs []error
	for _, region := range regions {
		nextCfg := azureConfigForLocation(cfg, region, len(regions) > 1)
		client := c
		if region != c.Location || len(regions) > 1 {
			var err error
			client, err = NewAzureClient(ctx, nextCfg)
			if err != nil {
				return Server{}, nextCfg, err
			}
		}
		if logf != nil && region != c.Location {
			logf("fallback provisioning region=%s after Azure capacity rejection\n", region)
		}
		server, resolved, err := client.createServerWithFallbackInLocation(ctx, nextCfg, publicKey, leaseID, slug, keep, logf)
		if err == nil {
			return server, resolved, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", region, err))
		if !isAzureRetryableProvisioningError(err) {
			return Server{}, nextCfg, joinErrors(errs)
		}
	}
	return Server{}, cfg, joinErrors(errs)
}

func (c *AzureClient) createServerWithFallbackInLocation(ctx context.Context, cfg Config, publicKey, leaseID, slug string, keep bool, logf func(string, ...any)) (Server, Config, error) {
	candidates := azureProvisioningCandidatesForConfig(cfg)
	var errs []error
	sharedInfraReady := false
	for i, vmSize := range candidates {
		next := cfg
		next.ServerType = vmSize
		if i > 0 && logf != nil {
			logf("fallback provisioning type=%s after quota/capacity rejection\n", vmSize)
		}
		if next.AzureSnapshot == "" {
			if _, err := c.validatedAzureOSDiskMode(ctx, next); err != nil {
				return Server{}, next, err
			}
		}
		if !sharedInfraReady {
			if err := c.EnsureSharedInfra(ctx); err != nil {
				return Server{}, next, err
			}
			sharedInfraReady = true
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
			if next.AzureSnapshot == "" {
				if _, err := c.validatedAzureOSDiskMode(ctx, next); err != nil {
					return Server{}, next, err
				}
			}
			if !sharedInfraReady {
				if err := c.EnsureSharedInfra(ctx); err != nil {
					return Server{}, next, err
				}
				sharedInfraReady = true
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

func azureProvisioningCandidatesForConfig(cfg Config) []string {
	if cfg.ServerTypeExplicit && cfg.ServerType != "" {
		return []string{cfg.ServerType}
	}
	candidates := azureVMSizeCandidatesForConfig(cfg)
	if cfg.ServerType == "" || len(candidates) == 0 || cfg.ServerType == candidates[0] {
		return candidates
	}
	if !azureCanPrependNonExplicitServerType(cfg) {
		return candidates
	}
	return append([]string{cfg.ServerType}, candidates...)
}

func azureCanPrependNonExplicitServerType(cfg Config) bool {
	if cfg.AzureSnapshot != "" {
		return true
	}
	mode, err := NormalizeAzureOSDiskMode(cfg.AzureOSDisk)
	if err != nil {
		return true
	}
	if azureOSDiskUsesFullCaching(mode) {
		return azureSupportsEphemeralFullCaching(cfg.ServerType)
	}
	return true
}

func azureRegionCandidates(cfg Config, preferredLocation string) []string {
	return appendUniqueStrings([]string{cfg.AzureLocation, preferredLocation}, cfg.Capacity.Regions...)
}

func azureConfigForLocation(cfg Config, location string, multiRegion bool) Config {
	cfg.AzureLocation = location
	if multiRegion {
		cfg.AzureVNet = azureRegionalName(cfg.AzureVNet, location)
		cfg.AzureNSG = azureRegionalName(cfg.AzureNSG, location)
	}
	return cfg
}

func azureRegionalName(base, location string) string {
	if base == "" {
		return base
	}
	suffix := strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return '-'
	}, strings.TrimSpace(location)), "-")
	if suffix == "" || strings.HasSuffix(strings.ToLower(base), "-"+suffix) {
		return base
	}
	return base + "-" + suffix
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
	quarantineNSGName := name + "-q-nsg"

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
	sharedNSGID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/networkSecurityGroups/%s",
		c.SubscriptionID, c.ResourceGroup, c.NSG)
	nsgID := sharedNSGID
	quarantinedSnapshot := cfg.AzureSnapshot != "" && cfg.TargetOS == targetWindows
	if quarantinedSnapshot {
		nsgID, err = c.createSnapshotQuarantineNSG(ctx, quarantineNSGName, tags)
		if err != nil {
			return Server{}, err
		}
	}
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

	var osProfile *armcompute.OSProfile
	var osDiskMode string
	var imageReference *armcompute.ImageReference
	osDisk := &armcompute.OSDisk{Name: to.Ptr(diskName)}
	if cfg.AzureSnapshot != "" {
		diskID, err := c.createManagedDiskFromSnapshot(ctx, diskName, cfg.AzureSnapshot, tags)
		if err != nil {
			return Server{}, err
		}
		osDisk.CreateOption = to.Ptr(armcompute.DiskCreateOptionTypesAttach)
		osDisk.OSType = to.Ptr(azureOSDiskType(cfg.TargetOS))
		osDisk.Caching = to.Ptr(armcompute.CachingTypesReadWrite)
		osDisk.ManagedDisk = &armcompute.ManagedDiskParameters{ID: to.Ptr(diskID)}
	} else {
		osProfile, err = c.azureOSProfile(cfg, publicKey, name, leaseID)
		if err != nil {
			return Server{}, err
		}
		osDisk.CreateOption = to.Ptr(armcompute.DiskCreateOptionTypesFromImage)
		osDiskMode, err = c.validatedAzureOSDiskMode(ctx, cfg)
		if err != nil {
			return Server{}, err
		}
		if azureOSDiskIsEphemeral(osDiskMode) {
			osDisk.Caching = to.Ptr(armcompute.CachingTypesReadOnly)
			osDisk.DiffDiskSettings = &armcompute.DiffDiskSettings{
				Option: to.Ptr(armcompute.DiffDiskOptionsLocal),
			}
			if azureOSDiskUsesFullCaching(osDiskMode) {
				osDisk.ManagedDisk = &armcompute.ManagedDiskParameters{
					StorageAccountType: to.Ptr(armcompute.StorageAccountTypesStandardSSDLRS),
				}
			}
		} else {
			osDisk.Caching = to.Ptr(armcompute.CachingTypesReadWrite)
			osDisk.ManagedDisk = &armcompute.ManagedDiskParameters{
				StorageAccountType: to.Ptr(armcompute.StorageAccountTypesStandardSSDLRS),
			}
		}
		imageReference = &armcompute.ImageReference{
			Publisher: to.Ptr(c.Image.Publisher),
			Offer:     to.Ptr(c.Image.Offer),
			SKU:       to.Ptr(c.Image.SKU),
			Version:   to.Ptr(c.Image.Version),
		}
	}
	vmProperties := &armcompute.VirtualMachineProperties{
		HardwareProfile: &armcompute.HardwareProfile{
			VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(cfg.ServerType)),
		},
		StorageProfile: &armcompute.StorageProfile{
			ImageReference: imageReference,
			OSDisk:         osDisk,
		},
		OSProfile: osProfile,
		NetworkProfile: &armcompute.NetworkProfile{
			NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{
				ID: to.Ptr(nicID),
			}},
		},
	}
	if strings.EqualFold(cfg.Capacity.Market, "spot") {
		applyAzureSpotCapacity(vmProperties)
	}
	vm := armcompute.VirtualMachine{
		Location:   to.Ptr(c.Location),
		Tags:       tags,
		Properties: vmProperties,
	}
	var createdVM armcompute.VirtualMachine
	if azureOSDiskUsesFullCaching(osDiskMode) {
		createdVM, err = c.createVMWithEphemeralFullCaching(ctx, name, vm)
		if err != nil {
			return Server{}, err
		}
	} else {
		vmPoller, err := c.vmc.BeginCreateOrUpdate(ctx, c.ResourceGroup, name, vm, nil)
		if err != nil {
			return Server{}, fmt.Errorf("begin vm: %w", err)
		}
		vmResp, err := vmPoller.PollUntilDone(ctx, nil)
		if err != nil {
			return Server{}, fmt.Errorf("vm: %w", err)
		}
		createdVM = vmResp.VirtualMachine
	}
	if cfg.TargetOS == targetWindows {
		command := azureWindowsBootstrapCommand()
		if cfg.AzureSnapshot != "" {
			command, err = azureWindowsSnapshotRehydrateCommand(cfg, publicKey)
			if err != nil {
				return Server{}, err
			}
		}
		if err := c.installWindowsBootstrapExtension(ctx, name, tags, command); err != nil {
			return Server{}, err
		}
	}
	if quarantinedSnapshot {
		if err := c.releaseSnapshotNIC(ctx, nicName, sharedNSGID); err != nil {
			return Server{}, err
		}
		// The fork is safe and usable once released; lease deletion retries quarantine cleanup.
		_ = c.deleteSnapshotQuarantineNSG(ctx, quarantineNSGName)
	}
	return azureVMToServer(createdVM, "", ""), nil
}

func azureOSDiskType(targetOS string) armcompute.OperatingSystemTypes {
	if targetOS == targetWindows {
		return armcompute.OperatingSystemTypesWindows
	}
	return armcompute.OperatingSystemTypesLinux
}

func applyAzureSpotCapacity(vmProperties *armcompute.VirtualMachineProperties) {
	vmProperties.Priority = to.Ptr(armcompute.VirtualMachinePriorityTypesSpot)
	vmProperties.EvictionPolicy = to.Ptr(armcompute.VirtualMachineEvictionPolicyTypesDelete)
	vmProperties.BillingProfile = &armcompute.BillingProfile{MaxPrice: to.Ptr(float64(-1))}
}

func (c *AzureClient) createVMWithEphemeralFullCaching(ctx context.Context, name string, vm armcompute.VirtualMachine) (armcompute.VirtualMachine, error) {
	payload, err := azureEphemeralFullCachingVMPayload(vm)
	if err != nil {
		return armcompute.VirtualMachine{}, err
	}
	path := azureResourcePath(
		"subscriptions", c.SubscriptionID,
		"resourceGroups", c.ResourceGroup,
		"providers", "Microsoft.Compute",
		"virtualMachines", name,
	)
	respBody, headers, status, err := c.azureARM(ctx, http.MethodPut, path, azureComputePreviewAPIVersion, payload)
	if err != nil {
		return armcompute.VirtualMachine{}, fmt.Errorf("begin vm: %w", err)
	}
	if pollURL := azurePollURL(headers); pollURL != "" {
		if err := c.pollAzureARMOperation(ctx, pollURL); err != nil {
			return armcompute.VirtualMachine{}, fmt.Errorf("vm: %w", err)
		}
	}
	if len(respBody) == 0 || status == http.StatusAccepted {
		respBody, _, _, err = c.azureARM(ctx, http.MethodGet, path, azureComputePreviewAPIVersion, nil)
		if err != nil {
			return armcompute.VirtualMachine{}, fmt.Errorf("get vm: %w", err)
		}
	}
	var created armcompute.VirtualMachine
	if err := json.Unmarshal(respBody, &created); err != nil {
		return armcompute.VirtualMachine{}, fmt.Errorf("decode vm: %w", err)
	}
	return created, nil
}

func azureEphemeralFullCachingVMPayload(vm armcompute.VirtualMachine) ([]byte, error) {
	data, err := json.Marshal(vm)
	if err != nil {
		return nil, fmt.Errorf("encode vm: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode vm payload: %w", err)
	}
	properties, ok := payload["properties"].(map[string]any)
	if !ok {
		return nil, errors.New("azure vm payload missing properties")
	}
	storageProfile, ok := properties["storageProfile"].(map[string]any)
	if !ok {
		return nil, errors.New("azure vm payload missing storageProfile")
	}
	osDisk, ok := storageProfile["osDisk"].(map[string]any)
	if !ok {
		return nil, errors.New("azure vm payload missing osDisk")
	}
	diffDiskSettings, ok := osDisk["diffDiskSettings"].(map[string]any)
	if !ok {
		diffDiskSettings = map[string]any{}
		osDisk["diffDiskSettings"] = diffDiskSettings
	}
	diffDiskSettings["option"] = "Local"
	diffDiskSettings["enableFullCaching"] = true
	osDisk["caching"] = "ReadOnly"
	osDisk["managedDisk"] = map[string]any{"storageAccountType": "StandardSSD_LRS"}
	return json.Marshal(payload)
}

func (c *AzureClient) azureARM(ctx context.Context, method, path, apiVersion string, body []byte) ([]byte, http.Header, int, error) {
	token, err := c.cred.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{"https://management.azure.com/.default"},
	})
	if err != nil {
		return nil, nil, 0, err
	}
	endpoint := "https://management.azure.com" + path
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, nil, 0, err
	}
	q := u.Query()
	q.Set("api-version", apiVersion)
	u.RawQuery = q.Encode()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if readErr != nil {
		return nil, resp.Header, resp.StatusCode, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.Header, resp.StatusCode, fmt.Errorf("azure %s %s: http %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return respBody, resp.Header, resp.StatusCode, nil
}

func (c *AzureClient) pollAzureARMOperation(ctx context.Context, pollURL string) error {
	for {
		status, retryAfter, err := c.azureARMOperationStatus(ctx, pollURL)
		if err != nil {
			return err
		}
		switch strings.ToLower(status) {
		case "succeeded":
			return nil
		case "failed", "canceled", "cancelled":
			return fmt.Errorf("operation %s", status)
		}
		delay := azureDeleteRetryDelay
		if retryAfter > 0 {
			delay = retryAfter
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *AzureClient) azureARMOperationStatus(ctx context.Context, pollURL string) (string, time.Duration, error) {
	token, err := c.cred.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{"https://management.azure.com/.default"},
	})
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if readErr != nil {
		return "", 0, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("azure poll %s: http %d: %s", pollURL, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var operation struct {
		Status string `json:"status"`
		Error  any    `json:"error"`
	}
	if err := json.Unmarshal(respBody, &operation); err != nil {
		return "", 0, fmt.Errorf("decode operation: %w", err)
	}
	if operation.Status == "" {
		return "succeeded", 0, nil
	}
	if strings.EqualFold(operation.Status, "failed") && operation.Error != nil {
		data, _ := json.Marshal(operation.Error)
		return operation.Status, 0, fmt.Errorf("operation failed: %s", data)
	}
	return operation.Status, retryAfterDuration(resp.Header.Get("Retry-After")), nil
}

func azurePollURL(headers http.Header) string {
	if value := strings.TrimSpace(headers.Get("Azure-AsyncOperation")); value != "" {
		return value
	}
	return strings.TrimSpace(headers.Get("Location"))
}

func retryAfterDuration(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func azureResourcePath(parts ...string) string {
	escaped := make([]string, 0, len(parts))
	for _, part := range parts {
		escaped = append(escaped, url.PathEscape(part))
	}
	return "/" + strings.Join(escaped, "/")
}

func azureResourceName(resourceID string) string {
	parts := strings.Split(strings.Trim(resourceID, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
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

func (c *AzureClient) createManagedDiskFromSnapshot(ctx context.Context, diskName, snapshotID string, tags map[string]*string) (string, error) {
	poller, err := c.diskc.BeginCreateOrUpdate(ctx, c.ResourceGroup, diskName, armcompute.Disk{
		Location: to.Ptr(c.Location),
		Tags:     tags,
		Properties: &armcompute.DiskProperties{
			CreationData: &armcompute.CreationData{
				CreateOption:     to.Ptr(armcompute.DiskCreateOptionCopy),
				SourceResourceID: to.Ptr(snapshotID),
			},
		},
	}, nil)
	if err != nil {
		return "", fmt.Errorf("begin snapshot disk: %w", err)
	}
	response, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("snapshot disk: %w", err)
	}
	if response.ID == nil || *response.ID == "" {
		return "", errors.New("snapshot disk has no resource id")
	}
	return *response.ID, nil
}

func (c *AzureClient) createSnapshotQuarantineNSG(ctx context.Context, name string, tags map[string]*string) (string, error) {
	poller, err := c.sgc.BeginCreateOrUpdate(ctx, c.ResourceGroup, name, azureSnapshotQuarantineSecurityGroup(c.Location, tags), nil)
	if err != nil {
		return "", fmt.Errorf("begin snapshot quarantine nsg: %w", err)
	}
	response, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("snapshot quarantine nsg: %w", err)
	}
	if response.ID == nil || *response.ID == "" {
		return "", errors.New("snapshot quarantine nsg has no resource id")
	}
	return *response.ID, nil
}

func azureSnapshotQuarantineSecurityGroup(location string, tags map[string]*string) armnetwork.SecurityGroup {
	return armnetwork.SecurityGroup{
		Location: to.Ptr(location),
		Tags:     tags,
		Properties: &armnetwork.SecurityGroupPropertiesFormat{
			SecurityRules: []*armnetwork.SecurityRule{{
				Name: to.Ptr("deny-inbound-until-rehydrated"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                 to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
					Access:                   to.Ptr(armnetwork.SecurityRuleAccessDeny),
					Direction:                to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					Priority:                 to.Ptr(int32(100)),
					SourceAddressPrefix:      to.Ptr("*"),
					SourcePortRange:          to.Ptr("*"),
					DestinationAddressPrefix: to.Ptr("*"),
					DestinationPortRange:     to.Ptr("*"),
				},
			}},
		},
	}
}

func (c *AzureClient) releaseSnapshotNIC(ctx context.Context, nicName, sharedNSGID string) error {
	response, err := c.nicc.Get(ctx, c.ResourceGroup, nicName, nil)
	if err != nil {
		return fmt.Errorf("get quarantined snapshot nic: %w", err)
	}
	if response.Properties == nil {
		return errors.New("quarantined snapshot nic has no properties")
	}
	response.Properties.NetworkSecurityGroup = &armnetwork.SecurityGroup{ID: to.Ptr(sharedNSGID)}
	poller, err := c.nicc.BeginCreateOrUpdate(ctx, c.ResourceGroup, nicName, response.Interface, nil)
	if err != nil {
		return fmt.Errorf("begin release snapshot nic: %w", err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("release snapshot nic: %w", err)
	}
	return nil
}

func (c *AzureClient) deleteSnapshotQuarantineNSG(ctx context.Context, name string) error {
	poller, err := c.sgc.BeginDelete(ctx, c.ResourceGroup, name, nil)
	if err != nil {
		if isAzureNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("begin delete snapshot quarantine nsg: %w", err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil && !isAzureNotFoundError(err) {
		return fmt.Errorf("delete snapshot quarantine nsg %s: %w", name, err)
	}
	return nil
}

func (c *AzureClient) installWindowsBootstrapExtension(ctx context.Context, vmName string, tags map[string]*string, command string) error {
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
				"commandToExecute": command,
			},
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("begin windows bootstrap extension: %w", err)
	}
	// Azure occasionally leaves the long-running-operation poller pending
	// after the extension resource itself has reached Succeeded. Keep the SDK
	// poller as the primary signal, but also read the resource state so a
	// completed Windows bootstrap does not strand the lease indefinitely.
	return waitForAzureExtension(ctx, 10*time.Second, func(pollCtx context.Context) error {
		_, pollErr := poller.PollUntilDone(pollCtx, nil)
		return pollErr
	}, func(stateCtx context.Context) (string, error) {
		extension, getErr := c.vmextc.Get(stateCtx, c.ResourceGroup, vmName, "crabbox-bootstrap", nil)
		if getErr != nil || extension.Properties == nil || extension.Properties.ProvisioningState == nil {
			return "", getErr
		}
		return *extension.Properties.ProvisioningState, nil
	})
}

// waitForAzureExtension waits for the SDK long-running operation while also
// consulting the extension resource state. Azure can leave the poller pending
// after the resource has already reached a terminal state, so callers must not
// rely on the poller alone.
func waitForAzureExtension(
	ctx context.Context,
	stateInterval time.Duration,
	poll func(context.Context) error,
	state func(context.Context) (string, error),
) error {
	pollCtx, cancelPoll := context.WithCancel(ctx)
	defer cancelPoll()
	pollResult := make(chan error, 1)
	go func() {
		pollResult <- poll(pollCtx)
	}()
	ticker := time.NewTicker(stateInterval)
	defer ticker.Stop()
	for {
		select {
		case pollErr := <-pollResult:
			if pollErr != nil {
				return fmt.Errorf("windows bootstrap extension: %w", pollErr)
			}
			return nil
		case <-ticker.C:
			provisioningState, stateErr := state(ctx)
			if stateErr != nil {
				continue
			}
			switch strings.ToLower(provisioningState) {
			case "succeeded":
				return nil
			case "failed", "canceled":
				return fmt.Errorf("windows bootstrap extension reached %s", provisioningState)
			}
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

func azureWindowsBootstrapCommand() string {
	return `powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "$p=Join-Path $env:SystemDrive 'AzureData\CustomData.bin'; $d=Join-Path $env:SystemDrive 'AzureData\crabbox-bootstrap.ps1'; Copy-Item -Force $p $d; & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $d"`
}

func azureWindowsSnapshotRehydrateCommand(cfg Config, publicKey string) (string, error) {
	script := azureWindowsSnapshotRehydratePowerShell(cfg, publicKey)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(script)); err != nil {
		return "", fmt.Errorf("compress windows snapshot rehydrate script: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close windows snapshot rehydrate script: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(compressed.Bytes())
	command := "powershell.exe -NoProfile -ExecutionPolicy Bypass -Command \"$b=[Convert]::FromBase64String('" + encoded + "');$m=[IO.MemoryStream]::new($b);$g=[IO.Compression.GZipStream]::new($m,[IO.Compression.CompressionMode]::Decompress);$r=[IO.StreamReader]::new($g);$s=$r.ReadToEnd();& ([ScriptBlock]::Create($s))\""
	if len(command) > 8000 {
		return "", fmt.Errorf("windows snapshot rehydrate command is too large: %d bytes", len(command))
	}
	return command, nil
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
			return azureVMToServer(vm.VirtualMachine, *pip.Properties.IPAddress, c.nicPrivateIP(ctx, name)), nil
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
	return azureVMToServer(vm.VirtualMachine, ip, c.nicPrivateIP(ctx, name)), nil
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
			privateIP := ""
			if vm.Name != nil {
				privateIP = c.nicPrivateIP(ctx, *vm.Name)
			}
			servers = append(servers, azureVMToServer(*vm, ip, privateIP))
		}
	}
	return servers, nil
}

func (c *AzureClient) DeleteServer(ctx context.Context, name string) error {
	return c.deleteVMResources(ctx, name)
}

func (c *AzureClient) CreateOSDiskSnapshot(ctx context.Context, vmName, snapshotName string) (image NativeCheckpointImage, err error) {
	vm, err := c.vmc.Get(ctx, c.ResourceGroup, vmName, nil)
	if err != nil {
		return NativeCheckpointImage{}, fmt.Errorf("get snapshot source vm: %w", err)
	}
	if vm.Properties == nil || vm.Properties.StorageProfile == nil {
		return NativeCheckpointImage{}, errors.New("snapshot source VM has no managed OS disk")
	}
	diskID, err := azureSnapshotOSDiskID(vmName, vm.Properties.StorageProfile.OSDisk)
	if err != nil {
		return NativeCheckpointImage{}, err
	}

	deallocate, err := c.vmc.BeginDeallocate(ctx, c.ResourceGroup, vmName, nil)
	if err != nil {
		return NativeCheckpointImage{}, fmt.Errorf("begin deallocate snapshot source vm: %w", err)
	}
	defer func() {
		restartCtx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		start, startErr := c.vmc.BeginStart(restartCtx, c.ResourceGroup, vmName, nil)
		if startErr == nil {
			_, startErr = start.PollUntilDone(restartCtx, nil)
		}
		if startErr != nil {
			err = errors.Join(err, fmt.Errorf("restart snapshot source vm: %w", startErr))
		}
	}()
	if _, err := deallocate.PollUntilDone(ctx, nil); err != nil {
		return NativeCheckpointImage{}, fmt.Errorf("deallocate snapshot source vm: %w", err)
	}
	poller, err := c.snapc.BeginCreateOrUpdate(ctx, c.ResourceGroup, snapshotName, armcompute.Snapshot{
		Location: to.Ptr(c.Location),
		Tags:     azureSharedTags(),
		Properties: &armcompute.SnapshotProperties{
			CreationData: &armcompute.CreationData{
				CreateOption:     to.Ptr(armcompute.DiskCreateOptionCopy),
				SourceResourceID: to.Ptr(diskID),
			},
		},
	}, nil)
	if err != nil {
		return NativeCheckpointImage{}, fmt.Errorf("begin OS disk snapshot: %w", err)
	}
	response, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return NativeCheckpointImage{}, fmt.Errorf("OS disk snapshot: %w", err)
	}
	if response.ID == nil || *response.ID == "" {
		return NativeCheckpointImage{}, errors.New("OS disk snapshot has no resource id")
	}
	image = NativeCheckpointImage{
		ID:         *response.ID,
		Name:       snapshotName,
		State:      "available",
		Provider:   "azure",
		Kind:       CheckpointKindAzureOS,
		Region:     c.Location,
		ResourceID: *response.ID,
		Direct:     true,
	}
	return image, nil
}

func azureSnapshotOSDiskID(vmName string, osDisk *armcompute.OSDisk) (string, error) {
	if osDisk == nil {
		return "", errors.New("snapshot source VM has no managed OS disk")
	}
	if osDisk.DiffDiskSettings != nil && osDisk.DiffDiskSettings.Option != nil &&
		*osDisk.DiffDiskSettings.Option == armcompute.DiffDiskOptionsLocal {
		return "", fmt.Errorf(
			"azure ephemeral OS disk on vm %s cannot be snapshotted; use --mode archive or relaunch the lease with a managed Azure OS disk",
			vmName,
		)
	}
	if osDisk.ManagedDisk == nil || osDisk.ManagedDisk.ID == nil {
		return "", errors.New("snapshot source VM has no managed OS disk")
	}
	return *osDisk.ManagedDisk.ID, nil
}

func (c *AzureClient) GetOSDiskSnapshot(ctx context.Context, snapshotID string) (NativeCheckpointImage, error) {
	name := azureResourceName(snapshotID)
	response, err := c.snapc.Get(ctx, c.ResourceGroup, name, nil)
	if err != nil {
		return NativeCheckpointImage{}, err
	}
	state := "available"
	if response.Properties != nil && response.Properties.ProvisioningState != nil {
		state = string(*response.Properties.ProvisioningState)
	}
	id := firstNonBlank(stringValue(response.ID), snapshotID)
	return NativeCheckpointImage{
		ID:         id,
		Name:       firstNonBlank(stringValue(response.Name), name),
		State:      state,
		Provider:   "azure",
		Kind:       CheckpointKindAzureOS,
		Region:     c.Location,
		ResourceID: id,
		Direct:     true,
	}, nil
}

func (c *AzureClient) DeleteOSDiskSnapshot(ctx context.Context, snapshotID string) error {
	poller, err := c.snapc.BeginDelete(ctx, c.ResourceGroup, azureResourceName(snapshotID), nil)
	if err != nil {
		if isAzureNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("begin delete OS disk snapshot: %w", err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil && !isAzureNotFoundError(err) {
		return fmt.Errorf("delete OS disk snapshot: %w", err)
	}
	return nil
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
	if err := c.deleteSnapshotQuarantineNSG(ctx, name+"-q-nsg"); err != nil {
		errs = append(errs, err)
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

// nicPrivateIP reads the private IP from the NIC associated with a VM.
func (c *AzureClient) nicPrivateIP(ctx context.Context, vmName string) string {
	nicName := vmName + "-nic"
	nic, err := c.nicc.Get(ctx, c.ResourceGroup, nicName, nil)
	if err != nil {
		return ""
	}
	if nic.Properties == nil {
		return ""
	}
	for _, ipCfg := range nic.Properties.IPConfigurations {
		if ipCfg.Properties != nil && ipCfg.Properties.PrivateIPAddress != nil {
			return *ipCfg.Properties.PrivateIPAddress
		}
	}
	return ""
}

func azureVMToServer(vm armcompute.VirtualMachine, ip, privateIP string) Server {
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
	s.PrivateNet.IPv4.IP = privateIP
	for k, v := range vm.Tags {
		if v != nil {
			s.Labels[azureTagToLabelKey(k)] = *v
		}
	}
	normalizeAzureWindowsModeLabel(s.Labels)
	return s
}

// AzureServerHost returns the SSH host for an Azure server based on the
// configured network preference. When network is "private" and a private IP
// is available, it returns the private IP; otherwise it returns the public IP.
func AzureServerHost(server Server, network string) string {
	if strings.EqualFold(network, "private") && server.PrivateNet.IPv4.IP != "" {
		return server.PrivateNet.IPv4.IP
	}
	return server.PublicNet.IPv4.IP
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
		strings.Contains(s, "OverconstrainedAllocationRequest") ||
		strings.Contains(s, "NotAvailableForSubscription")
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
		strings.Contains(s, "DiskInUse") ||
		strings.Contains(s, "DiskIsAttachedToVM") ||
		strings.Contains(s, "DiskAttached") ||
		strings.Contains(s, "CannotDeleteDisk") ||
		strings.Contains(s, "InUse") ||
		strings.Contains(s, "AnotherOperationInProgress") ||
		(strings.Contains(s, "OperationNotAllowed") && strings.Contains(s, "retry after"))
}
