package cli

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
)

func TestParseAzureImageRef(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		input   string
		want    azureImageRef
		wantErr bool
	}{
		{
			name:  "ubuntu jammy gen2",
			input: "Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest",
			want:  azureImageRef{Publisher: "Canonical", Offer: "0001-com-ubuntu-server-jammy", SKU: "22_04-lts-gen2", Version: "latest"},
		},
		{
			name:  "ubuntu resolute server",
			input: "Canonical:ubuntu-26_04-lts:server:latest",
			want:  azureImageRef{Publisher: "Canonical", Offer: "ubuntu-26_04-lts", SKU: "server", Version: "latest"},
		},
		{
			name:  "ubuntu noble server",
			input: "Canonical:ubuntu-24_04-lts:server:latest",
			want:  azureImageRef{Publisher: "Canonical", Offer: "ubuntu-24_04-lts", SKU: "server", Version: "latest"},
		},
		{
			name:    "missing version",
			input:   "Canonical:offer:sku",
			wantErr: true,
		},
		{
			name:    "empty",
			input:   "",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseAzureImageRef(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestValidateAzureCleanupVM(t *testing.T) {
	now := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	expected := Server{
		CloudID:     "crabbox-live",
		ImmutableID: "vmid-live",
		Labels: map[string]string{
			"crabbox":    "true",
			"created_by": "crabbox",
			"provider":   "azure",
			"lease":      "cbx_123456abcdef",
			"slug":       "live",
			"expires_at": leaseLabelTime(now.Add(-time.Hour)),
		},
	}
	if err := validateAzureCleanupVM(expected, expected, now); err != nil {
		t.Fatalf("valid cleanup VM rejected: %v", err)
	}
	replacement := expected
	replacement.ImmutableID = "vmid-replacement"
	if err := validateAzureCleanupVM(expected, replacement, now); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("replacement VM error=%v", err)
	}

	changedSlug := expected
	changedSlug.Labels = maps.Clone(expected.Labels)
	changedSlug.Labels["slug"] = "other"
	if err := validateAzureCleanupVM(expected, changedSlug, now); err == nil || !strings.Contains(err.Error(), "slug") {
		t.Fatalf("changed slug error=%v", err)
	}

	renewed := expected
	renewed.Labels = maps.Clone(expected.Labels)
	renewed.Labels["expires_at"] = leaseLabelTime(now.Add(time.Hour))
	if err := validateAzureCleanupVM(expected, renewed, now); err == nil || !strings.Contains(err.Error(), "no longer cleanup eligible") {
		t.Fatalf("renewed VM error=%v", err)
	}
}

func TestValidateAzureCleanupResourceTags(t *testing.T) {
	labels := map[string]string{
		"crabbox":    "true",
		"created_by": "crabbox",
		"provider":   "azure",
		"lease":      "cbx_123456abcdef",
		"slug":       "live",
	}
	if err := validateAzureCleanupResourceTags("NIC", "crabbox-live-nic", azureLabelsToTags(labels), labels); err != nil {
		t.Fatalf("valid resource tags rejected: %v", err)
	}
	wrongLease := maps.Clone(labels)
	wrongLease["lease"] = "cbx_fedcba654321"
	if err := validateAzureCleanupResourceTags("NIC", "crabbox-live-nic", azureLabelsToTags(wrongLease), labels); err == nil || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("wrong lease error=%v", err)
	}
}

func TestAzureScopedResourceName(t *testing.T) {
	id := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/crabbox-live-nic"
	name, err := azureScopedResourceName(id, "sub", "rg", "Microsoft.Network", "networkInterfaces")
	if err != nil || name != "crabbox-live-nic" {
		t.Fatalf("name=%q err=%v", name, err)
	}
	if _, err := azureScopedResourceName(id, "other", "rg", "Microsoft.Network", "networkInterfaces"); err == nil {
		t.Fatal("cross-subscription resource id accepted")
	}
}

func TestRequireAzureCleanupIdentity(t *testing.T) {
	if err := requireAzureCleanupIdentity("disk", "crabbox-live-osdisk", "disk-guid", "disk-guid"); err != nil {
		t.Fatalf("matching identity rejected: %v", err)
	}
	if err := requireAzureCleanupIdentity("disk", "crabbox-live-osdisk", "", ""); err == nil || !strings.Contains(err.Error(), "no immutable resource identity") {
		t.Fatalf("missing identity error=%v", err)
	}
	if err := requireAzureCleanupIdentity("disk", "crabbox-live-osdisk", "replacement-guid", "disk-guid"); err == nil || !strings.Contains(err.Error(), "does not match validated identity") {
		t.Fatalf("replacement identity error=%v", err)
	}
}

func TestAzureCleanupSharedNSGUsesLiveVMLocation(t *testing.T) {
	if !isAzureCleanupSharedNSG("crabbox-nsg-westus2", "crabbox-nsg", "westus2") {
		t.Fatal("live fallback-region NSG rejected")
	}
	if isAzureCleanupSharedNSG("crabbox-nsg-eastus", "crabbox-nsg", "westus2") {
		t.Fatal("primary-region NSG accepted for fallback-region VM")
	}
}

func TestAzureImageForConfig(t *testing.T) {
	t.Parallel()
	linux := baseConfig()
	linux.TargetOS = targetLinux
	if got := azureImageForConfig(linux); got != defaultAzureLinuxImage {
		t.Fatalf("linux image=%q want %q", got, defaultAzureLinuxImage)
	}
	linuxARM := baseConfig()
	linuxARM.TargetOS = targetLinux
	linuxARM.Architecture = ArchitectureARM64
	linuxARM.architectureExplicit = true
	if got := azureImageForConfig(linuxARM); got != defaultAzureLinuxARM64Image {
		t.Fatalf("linux arm64 image=%q want %q", got, defaultAzureLinuxARM64Image)
	}
	windows := baseConfig()
	windows.TargetOS = targetWindows
	if got := azureImageForConfig(windows); got != defaultAzureWindowsImage {
		t.Fatalf("windows image=%q want %q", got, defaultAzureWindowsImage)
	}
	windows.AzureImage = "Contoso:offer:sku:latest"
	if got := azureImageForConfig(windows); got != windows.AzureImage {
		t.Fatalf("windows explicit image=%q want %q", got, windows.AzureImage)
	}
	windows.AzureImage = legacyAzureJammyImage
	if got := azureImageForConfig(windows); got != defaultAzureWindowsImage {
		t.Fatalf("windows legacy linux default=%q want %q", got, defaultAzureWindowsImage)
	}
	windows.AzureImage = azureNobleLinuxImage
	if got := azureImageForConfig(windows); got != defaultAzureWindowsImage {
		t.Fatalf("windows portable noble linux default=%q want %q", got, defaultAzureWindowsImage)
	}
	windows = baseConfig()
	windows.TargetOS = targetWindows
	windows.OSImage = "ubuntu:24.04"
	applyOSImageProviderDefaults(&windows, false)
	if got := azureImageForConfig(windows); got != defaultAzureWindowsImage {
		t.Fatalf("windows image after portable os=%q want %q", got, defaultAzureWindowsImage)
	}
}

func TestAzureVMSizeCandidatesForClass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		class string
		want  []string
	}{
		{class: "standard", want: []string{"Standard_D32ads_v6", "Standard_D32ds_v6", "Standard_F32s_v2", "Standard_D32ads_v5", "Standard_D32ds_v5", "Standard_D16ads_v6", "Standard_D16ds_v6", "Standard_F16s_v2"}},
		{class: "fast", want: []string{"Standard_D64ads_v6", "Standard_D64ds_v6", "Standard_F64s_v2", "Standard_D64ads_v5", "Standard_D64ds_v5", "Standard_D48ads_v6", "Standard_D48ds_v6", "Standard_F48s_v2", "Standard_D32ads_v6", "Standard_D32ds_v6", "Standard_F32s_v2"}},
		{class: "large", want: []string{"Standard_D96ads_v6", "Standard_D96ds_v6", "Standard_D96ads_v5", "Standard_D96ds_v5", "Standard_D64ads_v6", "Standard_D64ds_v6", "Standard_F64s_v2", "Standard_D48ads_v6", "Standard_D48ds_v6", "Standard_F48s_v2"}},
		{class: "beast", want: []string{"Standard_D192ds_v6", "Standard_D128ds_v6", "Standard_D96ads_v6", "Standard_D96ds_v6", "Standard_D96ads_v5", "Standard_D96ds_v5", "Standard_D64ads_v6", "Standard_D64ds_v6", "Standard_F64s_v2"}},
		{class: "Standard_F2s", want: []string{"Standard_F2s"}},
	}
	for _, tc := range cases {
		got := azureVMSizeCandidatesForClass(tc.class)
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("class=%q: got %v, want %v", tc.class, got, tc.want)
		}
	}
}

func TestAzureARM64VMSizeCandidatesForClass(t *testing.T) {
	t.Parallel()
	got := azureARM64VMSizeCandidatesForClass("beast")
	want := []string{"Standard_D96pds_v6", "Standard_D96ps_v6", "Standard_D64pds_v6", "Standard_D64ps_v6"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAzureVMSizeCandidatesForTargetModeClass(t *testing.T) {
	t.Parallel()
	linux := azureVMSizeCandidatesForTargetModeClass(targetLinux, windowsModeNormal, "standard")
	if !reflect.DeepEqual(linux, azureVMSizeCandidatesForClass("standard")) {
		t.Fatalf("linux target got %v want azure linux table", linux)
	}
	windows := azureVMSizeCandidatesForTargetModeClass(targetWindows, windowsModeNormal, "standard")
	if want := azureWindowsVMSizeCandidatesForClass("standard"); !reflect.DeepEqual(windows, want) {
		t.Fatalf("windows target got %v want %v", windows, want)
	}
	wsl2 := azureVMSizeCandidatesForTargetModeClass(targetWindows, windowsModeWSL2, "standard")
	if want := azureWindowsVMSizeCandidatesForClass("standard"); !reflect.DeepEqual(wsl2, want) {
		t.Fatalf("wsl2 target got %v want %v", wsl2, want)
	}
	windowsARM64 := azureVMSizeCandidatesForTargetModeArchitectureClass(targetWindows, windowsModeNormal, ArchitectureARM64, "standard")
	if want := azureARM64VMSizeCandidatesForClass("standard"); !reflect.DeepEqual(windowsARM64, want) {
		t.Fatalf("windows arm64 target got %v want %v", windowsARM64, want)
	}
	wsl2ARM64 := azureVMSizeCandidatesForTargetModeArchitectureClass(targetWindows, windowsModeWSL2, ArchitectureARM64, "standard")
	if want := []string{"standard"}; !reflect.DeepEqual(wsl2ARM64, want) {
		t.Fatalf("wsl2 arm64 target got %v want %v", wsl2ARM64, want)
	}
}

func TestAzureVMSizeCandidatesForConfigHonorsARM64(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.Provider = "azure"
	cfg.TargetOS = targetLinux
	cfg.Architecture = ArchitectureARM64
	cfg.architectureExplicit = true
	if got := azureVMSizeCandidatesForConfig(cfg)[0]; got != "Standard_D96pds_v6" {
		t.Fatalf("first arm64 size=%q", got)
	}
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	if got := azureVMSizeCandidatesForConfig(cfg)[0]; got != "Standard_D96pds_v6" {
		t.Fatalf("first windows arm64 size=%q", got)
	}
	cfg.architectureExplicit = false
	cfg.Architecture = ArchitectureAMD64
	cfg.ServerType = "Standard_D2pds_v6"
	cfg.ServerTypeExplicit = true
	if got := effectiveArchitectureForConfig(cfg); got != ArchitectureARM64 {
		t.Fatalf("windows explicit ARM64 size inferred architecture=%q", got)
	}
}

func TestAzureVMSizeCandidatesForConfigFiltersEphemeralPreview(t *testing.T) {
	t.Parallel()
	arm := baseConfig()
	arm.Provider = "azure"
	arm.TargetOS = targetLinux
	arm.Architecture = ArchitectureARM64
	arm.architectureExplicit = true
	arm.Class = "standard"
	arm.AzureOSDisk = AzureOSDiskEphemeralPreview
	if got := azureVMSizeCandidatesForConfig(arm); !reflect.DeepEqual(got, []string{"Standard_D32pds_v6", "Standard_D16pds_v6"}) {
		t.Fatalf("arm preview candidates=%v", got)
	}
	windows := baseConfig()
	windows.Provider = "azure"
	windows.TargetOS = targetWindows
	windows.WindowsMode = windowsModeNormal
	windows.Class = "standard"
	windows.AzureOSDisk = AzureOSDiskEphemeralPreview
	if got := azureVMSizeCandidatesForConfig(windows); !reflect.DeepEqual(got, []string{"Standard_D8ads_v6", "Standard_D8ds_v6", "Standard_D8ads_v5", "Standard_D8ds_v5", "Standard_D16ads_v6", "Standard_D16ds_v6", "Standard_D16ads_v5", "Standard_D16ds_v5"}) {
		t.Fatalf("windows preview candidates=%v", got)
	}
	windows.Architecture = ArchitectureARM64
	windows.architectureExplicit = true
	if got := azureVMSizeCandidatesForConfig(windows); !reflect.DeepEqual(got, []string{"Standard_D32pds_v6", "Standard_D16pds_v6"}) {
		t.Fatalf("windows arm64 preview candidates=%v", got)
	}
}

func TestAzureProvisioningCandidatesSkipsStaleEphemeralPreviewDefault(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.Provider = "azure"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	cfg.Class = "standard"
	cfg.AzureOSDisk = AzureOSDiskEphemeralPreview
	cfg.ServerType = "Standard_D2ads_v6"
	cfg.ServerTypeExplicit = false
	got := azureProvisioningCandidatesForConfig(cfg)
	if len(got) == 0 {
		t.Fatal("no candidates")
	}
	if got[0] != "Standard_D8ads_v6" {
		t.Fatalf("first candidate=%q, want Standard_D8ads_v6; all=%v", got[0], got)
	}
	for _, candidate := range got {
		if candidate == "Standard_D2ads_v6" {
			t.Fatalf("stale unsupported default was prepended: %v", got)
		}
	}

	cfg.ServerTypeExplicit = true
	got = azureProvisioningCandidatesForConfig(cfg)
	if !reflect.DeepEqual(got, []string{"Standard_D2ads_v6"}) {
		t.Fatalf("explicit candidate=%v, want exact unsupported type preserved", got)
	}

	cfg.ServerTypeExplicit = false
	cfg.AzureSnapshot = "snapshot-id"
	got = azureProvisioningCandidatesForConfig(cfg)
	if got[0] != "Standard_D2ads_v6" {
		t.Fatalf("snapshot-backed first candidate=%q, want stale managed-disk type preserved; all=%v", got[0], got)
	}
}

func TestAzureWindowsSnapshotRehydrateCommandIsBounded(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	cfg.Desktop = true
	cfg.SSHUser = "crabbox"

	publicKey := "ssh-ed25519 " + strings.Repeat("A", 68) + " crabbox@snapshot"
	command, err := azureWindowsSnapshotRehydrateCommand(cfg, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(command) > 8000 {
		t.Fatalf("command length=%d", len(command))
	}
	if !strings.Contains(command, "FromBase64String") || !strings.Contains(command, "ScriptBlock]::Create") {
		t.Fatalf("unexpected command: %s", command)
	}
}

func TestAzureSnapshotOSDiskTypeMatchesTarget(t *testing.T) {
	t.Parallel()
	if got := azureOSDiskType(targetWindows); got != armcompute.OperatingSystemTypesWindows {
		t.Fatalf("Windows disk type=%q", got)
	}
	if got := azureOSDiskType(targetLinux); got != armcompute.OperatingSystemTypesLinux {
		t.Fatalf("Linux disk type=%q", got)
	}
}

func TestAzureSnapshotQuarantineSecurityGroupDeniesAllInbound(t *testing.T) {
	t.Parallel()
	tags := map[string]*string{"lease_id": to.Ptr("cbx_123")}
	group := azureSnapshotQuarantineSecurityGroup("westus2", tags)
	if group.Location == nil || *group.Location != "westus2" || group.Tags["lease_id"] == nil || *group.Tags["lease_id"] != "cbx_123" {
		t.Fatalf("quarantine metadata=%#v", group)
	}
	if group.Properties == nil || len(group.Properties.SecurityRules) != 1 {
		t.Fatalf("quarantine rules=%#v, want one explicit rule", group.Properties)
	}
	rule := group.Properties.SecurityRules[0]
	if rule == nil || rule.Properties == nil {
		t.Fatal("quarantine rule has no properties")
	}
	properties := rule.Properties
	if properties.Protocol == nil || *properties.Protocol != armnetwork.SecurityRuleProtocolAsterisk ||
		properties.Access == nil || *properties.Access != armnetwork.SecurityRuleAccessDeny ||
		properties.Direction == nil || *properties.Direction != armnetwork.SecurityRuleDirectionInbound ||
		properties.Priority == nil || *properties.Priority != 100 ||
		properties.SourceAddressPrefix == nil || *properties.SourceAddressPrefix != "*" ||
		properties.SourcePortRange == nil || *properties.SourcePortRange != "*" ||
		properties.DestinationAddressPrefix == nil || *properties.DestinationAddressPrefix != "*" ||
		properties.DestinationPortRange == nil || *properties.DestinationPortRange != "*" {
		t.Fatalf("quarantine rule=%#v, want deny-all inbound", properties)
	}
}

func TestAzureSnapshotNICReleaseRequestUsesWritableCreatePayload(t *testing.T) {
	t.Parallel()
	quarantineNSGID := "/subscriptions/test/resourceGroups/test/providers/Microsoft.Network/networkSecurityGroups/lease-q-nsg"
	sharedNSGID := "/subscriptions/test/resourceGroups/test/providers/Microsoft.Network/networkSecurityGroups/crabbox-nsg"
	createRequest := armnetwork.Interface{
		Location: to.Ptr("westus2"),
		Tags:     map[string]*string{"lease_id": to.Ptr("cbx_123")},
		Properties: &armnetwork.InterfacePropertiesFormat{
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{
				Name: to.Ptr("ipconfig"),
				Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
					Subnet:                    &armnetwork.Subnet{ID: to.Ptr("/subscriptions/test/subnets/default")},
					PublicIPAddress:           &armnetwork.PublicIPAddress{ID: to.Ptr("/subscriptions/test/publicIPAddresses/lease")},
				},
			}},
			NetworkSecurityGroup: &armnetwork.SecurityGroup{ID: to.Ptr(quarantineNSGID)},
		},
	}

	releaseRequest, err := azureSnapshotNICReleaseRequest(createRequest, sharedNSGID)
	if err != nil {
		t.Fatal(err)
	}
	if got := *createRequest.Properties.NetworkSecurityGroup.ID; got != quarantineNSGID {
		t.Fatalf("create request NSG=%q, want quarantine preserved", got)
	}
	if got := *releaseRequest.Properties.NetworkSecurityGroup.ID; got != sharedNSGID {
		t.Fatalf("release request NSG=%q, want shared NSG", got)
	}
	payload, err := json.Marshal(releaseRequest)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatal(err)
	}
	for _, readOnly := range []string{"etag", "id", "name", "type"} {
		if _, ok := object[readOnly]; ok {
			t.Fatalf("release PUT includes top-level read-only field %q: %s", readOnly, payload)
		}
	}
	properties, ok := object["properties"].(map[string]any)
	if !ok {
		t.Fatalf("release PUT properties=%T", object["properties"])
	}
	for _, readOnly := range []string{"macAddress", "provisioningState", "resourceGuid", "virtualMachine"} {
		if _, ok := properties[readOnly]; ok {
			t.Fatalf("release PUT includes read-only property %q: %s", readOnly, payload)
		}
	}
}

func TestAzureSnapshotNICReleaseRequestRequiresProperties(t *testing.T) {
	t.Parallel()
	_, err := azureSnapshotNICReleaseRequest(armnetwork.Interface{}, "shared-nsg")
	if err == nil || err.Error() != "snapshot network interface create request has no properties" {
		t.Fatalf("error=%v", err)
	}
}

func TestAzureSnapshotExposureSequence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		failAt     string
		wantEvents []string
		wantErr    string
	}{
		{name: "success", wantEvents: []string{"rehydrate", "expose", "cleanup"}},
		{name: "rehydrate failure stays quarantined", failAt: "rehydrate", wantEvents: []string{"rehydrate"}, wantErr: "rehydrate snapshot credentials"},
		{name: "exposure failure stays quarantined", failAt: "expose", wantEvents: []string{"rehydrate", "expose"}, wantErr: "expose rehydrated snapshot network"},
		{name: "cleanup failure fails creation", failAt: "cleanup", wantEvents: []string{"rehydrate", "expose", "cleanup"}, wantErr: "delete snapshot quarantine network security group"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var events []string
			step := func(name string) func() error {
				return func() error {
					events = append(events, name)
					if test.failAt == name {
						return errors.New("injected failure")
					}
					return nil
				}
			}
			err := runAzureSnapshotExposureSequence(step("rehydrate"), step("expose"), step("cleanup"))
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error=%v, want containing %q", err, test.wantErr)
			}
			if !reflect.DeepEqual(events, test.wantEvents) {
				t.Fatalf("events=%v, want %v", events, test.wantEvents)
			}
		})
	}
}

func TestAzureSnapshotQuarantineCleanupRetries(t *testing.T) {
	t.Parallel()
	t.Run("retryable error then success", func(t *testing.T) {
		attempts := 0
		err := retryAzureSnapshotQuarantineCleanup(context.Background(), 3, 0, func() error {
			attempts++
			if attempts < 3 {
				return errors.New("NetworkSecurityGroupCannotBeDeleted because in use")
			}
			return nil
		})
		if err != nil || attempts != 3 {
			t.Fatalf("error=%v attempts=%d", err, attempts)
		}
	})
	t.Run("non-retryable error", func(t *testing.T) {
		attempts := 0
		err := retryAzureSnapshotQuarantineCleanup(context.Background(), 3, 0, func() error {
			attempts++
			return errors.New("permission denied")
		})
		if err == nil || err.Error() != "permission denied" || attempts != 1 {
			t.Fatalf("error=%v attempts=%d", err, attempts)
		}
	})
	t.Run("retry exhaustion", func(t *testing.T) {
		attempts := 0
		err := retryAzureSnapshotQuarantineCleanup(context.Background(), 2, 0, func() error {
			attempts++
			return errors.New("NetworkSecurityGroupCannotBeDeleted because in use")
		})
		if err == nil || attempts != 2 {
			t.Fatalf("error=%v attempts=%d", err, attempts)
		}
	})
	t.Run("context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		attempts := 0
		err := retryAzureSnapshotQuarantineCleanup(ctx, 3, time.Hour, func() error {
			attempts++
			return errors.New("NetworkSecurityGroupCannotBeDeleted because in use")
		})
		if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) || attempts != 1 {
			t.Fatalf("error=%v attempts=%d", err, attempts)
		}
	})
}

func TestAzureWindowsSnapshotRehydrateRotatesServiceVNCCredentials(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	cfg.Desktop = true
	cfg.SSHUser = "crabbox"

	script := azureWindowsSnapshotRehydratePowerShell(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"ConvertTo-CrabboxVNCPassword",
		"0xE8, 0x4A, 0xD6, 0x60, 0xC4, 0x72, 0x1A, 0xE0",
		`HKLM:\Software\TightVNC\Server`,
		"-Name Password -PropertyType Binary",
		"-Name ControlPassword -PropertyType Binary",
		"Stop-Service -Name tvnserver",
		"Start-Service -Name tvnserver",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("snapshot rehydrate script missing %q", want)
		}
	}
	if strings.Index(script, "Stop-Service -Name tvnserver") > strings.Index(script, "-Name Password -PropertyType Binary") {
		t.Fatal("snapshot rehydrate must stop TightVNC before replacing its service password")
	}
}

func TestAzureWindowsSnapshotRehydrateDefinesVNCPathWithoutDesktop(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeWSL2
	cfg.Desktop = false
	cfg.SSHUser = "crabbox"

	script := azureWindowsSnapshotRehydratePowerShell(cfg, "ssh-ed25519 test")
	definition := `$vncPasswordPath = "C:\ProgramData\crabbox\vnc.password"`
	reset := `Remove-Item -LiteralPath $vncPasswordPath, $windowsUsernamePath, $windowsPasswordPath`
	definitionIndex := strings.Index(script, definition)
	resetIndex := strings.Index(script, reset)
	if definitionIndex < 0 || resetIndex < 0 || definitionIndex > resetIndex {
		t.Fatalf("snapshot credential reset must define the VNC password path before use")
	}
}

func TestAzureWindowsVMSizeCandidatesForClass(t *testing.T) {
	t.Parallel()
	got := azureWindowsVMSizeCandidatesForClass("beast")
	want := []string{"Standard_D16ads_v6", "Standard_D16ds_v6", "Standard_D16ads_v5", "Standard_D16ds_v5", "Standard_D8ads_v6"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAzureRegionCandidates(t *testing.T) {
	t.Parallel()
	cfg := Config{AzureLocation: "eastus"}
	cfg.Capacity.Regions = []string{"westeurope", "eastus"}
	got := azureRegionCandidates(cfg, "eastus")
	want := []string{"eastus", "westeurope"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	cfg.AzureLocation = "westeurope"
	got = azureRegionCandidates(cfg, "eastus")
	want = []string{"westeurope", "eastus"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("override got %v, want %v", got, want)
	}
	if got := azureRegionalName("crabbox-vnet", "West Europe"); got != "crabbox-vnet-west-europe" {
		t.Fatalf("regional name=%q", got)
	}
	if got := azureRegionalName("crabbox-vnet-westeurope", "westeurope"); got != "crabbox-vnet-westeurope" {
		t.Fatalf("regional name duplicated suffix: %q", got)
	}
}

func TestAzureSameLocation(t *testing.T) {
	t.Parallel()
	if !azureSameLocation(to.Ptr("eastus"), "eastus") {
		t.Fatal("same compact location should match")
	}
	if !azureSameLocation(to.Ptr("West Europe"), "west europe") {
		t.Fatal("same display-style location should match")
	}
	if azureSameLocation(to.Ptr("eastus"), "westus3") {
		t.Fatal("different locations should not match")
	}
}

func TestApplyAzureSpotCapacity(t *testing.T) {
	t.Parallel()
	props := &armcompute.VirtualMachineProperties{}
	applyAzureSpotCapacity(props)
	if props.Priority == nil || *props.Priority != armcompute.VirtualMachinePriorityTypesSpot {
		t.Fatalf("Priority=%v want Spot", props.Priority)
	}
	if props.EvictionPolicy == nil || *props.EvictionPolicy != armcompute.VirtualMachineEvictionPolicyTypesDelete {
		t.Fatalf("EvictionPolicy=%v want Delete", props.EvictionPolicy)
	}
	if props.BillingProfile == nil || props.BillingProfile.MaxPrice == nil || *props.BillingProfile.MaxPrice != -1 {
		t.Fatalf("BillingProfile.MaxPrice=%v want -1", props.BillingProfile)
	}
}

func TestServerTypeForProviderClassAzure(t *testing.T) {
	t.Parallel()
	got := serverTypeForProviderClass("azure", "beast")
	if got != "Standard_D192ds_v6" {
		t.Fatalf("got %q, want Standard_D192ds_v6", got)
	}
}

func TestAzureSupportsEphemeralOS(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"Standard_D2as_v5":  false,
		"Standard_D8s_v5":   false,
		"Standard_D2ads_v5": true,
		"Standard_D2ads_v6": true,
		"Standard_F2s_v2":   true,
		"Standard_E4ds_v5":  true,
		"Standard_D2as_v6":  false,
		"Standard_D2s_v6":   false,
		"Standard_B2s":      false,
		"Standard_A2_v2":    false,
		"":                  false,
	}
	for size, want := range cases {
		if got := azureSupportsEphemeralOS(size); got != want {
			t.Fatalf("size=%q got %v want %v", size, got, want)
		}
	}
}

func TestAzureSupportsEphemeralFullCaching(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"Standard_D2ads_v6":  false,
		"Standard_D4ads_v6":  false,
		"Standard_D8ads_v6":  true,
		"Standard_D32ads_v6": true,
		"Standard_F32s_v2":   true,
		"Standard_D32pds_v6": true,
		"Standard_D32ps_v6":  false,
		"Standard_D96pds_v6": true,
		"Standard_D96ps_v6":  false,
		"Standard_D32as_v6":  false,
		"custom-size":        false,
	}
	for size, want := range cases {
		if got := azureSupportsEphemeralFullCaching(size); got != want {
			t.Fatalf("size=%q got %v want %v", size, got, want)
		}
	}
}

func TestNormalizeAzureOSDiskMode(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                  AzureOSDiskManaged,
		"auto":              AzureOSDiskManaged,
		"MANAGED":           AzureOSDiskManaged,
		"ephemeral":         AzureOSDiskEphemeral,
		"ephemeral-preview": AzureOSDiskEphemeralPreview,
		" managed ":         AzureOSDiskManaged,
	}
	for input, want := range cases {
		got, err := NormalizeAzureOSDiskMode(input)
		if err != nil {
			t.Fatalf("NormalizeAzureOSDiskMode(%q) err=%v", input, err)
		}
		if got != want {
			t.Fatalf("NormalizeAzureOSDiskMode(%q)=%q want %q", input, got, want)
		}
	}
	if _, err := NormalizeAzureOSDiskMode("premium"); err == nil {
		t.Fatal("expected invalid Azure OS disk mode to fail")
	}
}

func TestNormalizeAzureSnapshotStorageSKUs(t *testing.T) {
	t.Parallel()
	snapshot, err := NormalizeAzureSnapshotSKU(" premium_lrs ")
	if err != nil || snapshot != "Premium_LRS" {
		t.Fatalf("snapshot SKU=%q err=%v", snapshot, err)
	}
	disk, err := NormalizeAzureDiskSKU("standardssd_lrs")
	if err != nil || disk != "StandardSSD_LRS" {
		t.Fatalf("disk SKU=%q err=%v", disk, err)
	}
	if _, err := NormalizeAzureSnapshotSKU("UltraSSD_LRS"); err == nil {
		t.Fatal("expected unsupported snapshot SKU to fail")
	}
	if _, err := NormalizeAzureDiskSKU("not-a-sku"); err == nil {
		t.Fatal("expected unsupported disk SKU to fail")
	}
}

func TestAzureSnapshotPrerequisitesRunConcurrently(t *testing.T) {
	t.Parallel()
	started := make(chan string, 2)
	release := make(chan struct{})
	result := make(chan struct {
		network azureLeaseNetwork
		disk    string
		err     error
	}, 1)
	go func() {
		network, disk, err := runAzureSnapshotPrerequisites(
			context.Background(),
			func(context.Context) (azureLeaseNetwork, error) {
				started <- "network"
				<-release
				return azureLeaseNetwork{
					id: "nic-id",
					nicCreateRequest: armnetwork.Interface{
						Location: to.Ptr("westus2"),
					},
				}, nil
			},
			func(context.Context) (string, error) {
				started <- "disk"
				<-release
				return "disk-id", nil
			},
		)
		result <- struct {
			network azureLeaseNetwork
			disk    string
			err     error
		}{network: network, disk: disk, err: err}
	}()

	seen := map[string]bool{}
	for range 2 {
		select {
		case operation := <-started:
			seen[operation] = true
		case <-time.After(time.Second):
			t.Fatal("snapshot prerequisites did not start concurrently")
		}
	}
	close(release)
	completed := <-result
	if completed.err != nil || completed.network.id != "nic-id" || completed.disk != "disk-id" {
		t.Fatalf("result=%+v", completed)
	}
	if completed.network.nicCreateRequest.Location == nil || *completed.network.nicCreateRequest.Location != "westus2" {
		t.Fatalf("NIC create request lost across prerequisite result: %#v", completed.network.nicCreateRequest)
	}
	if !seen["network"] || !seen["disk"] {
		t.Fatalf("started=%v", seen)
	}
}

func TestAzureUseEphemeralOSDiskModes(t *testing.T) {
	t.Parallel()
	client := &AzureClient{}
	ctx := t.Context()
	cases := []struct {
		name    string
		cfg     Config
		want    bool
		wantErr bool
	}{
		{
			name: "auto uses managed disk",
			cfg:  Config{AzureOSDisk: AzureOSDiskAuto, ServerType: "Standard_D2ads_v6"},
			want: false,
		},
		{
			name: "managed forces managed disk",
			cfg:  Config{AzureOSDisk: AzureOSDiskManaged, ServerType: "Standard_D2ads_v6"},
			want: false,
		},
		{
			name: "ephemeral allows supported sku",
			cfg:  Config{AzureOSDisk: AzureOSDiskEphemeral, ServerType: "Standard_D2ads_v6"},
			want: true,
		},
		{
			name: "ephemeral preview allows supported full caching sku",
			cfg:  Config{AzureOSDisk: AzureOSDiskEphemeralPreview, ServerType: "Standard_D8ads_v6"},
			want: true,
		},
		{
			name:    "ephemeral preview rejects two core sku",
			cfg:     Config{AzureOSDisk: AzureOSDiskEphemeralPreview, ServerType: "Standard_D2ads_v6"},
			wantErr: true,
		},
		{
			name:    "ephemeral preview rejects arm sku without local disk",
			cfg:     Config{AzureOSDisk: AzureOSDiskEphemeralPreview, ServerType: "Standard_D32ps_v6"},
			wantErr: true,
		},
		{
			name:    "ephemeral rejects unsupported sku",
			cfg:     Config{AzureOSDisk: AzureOSDiskEphemeral, ServerType: "Standard_D2as_v6"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := client.useEphemeralOSDisk(ctx, tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("useEphemeralOSDisk err=%v", err)
			}
			if got != tc.want {
				t.Fatalf("useEphemeralOSDisk=%t want %t", got, tc.want)
			}
		})
	}
}

func TestAzureCreateServerWithFallbackRejectsEphemeralPreviewBeforeSharedInfra(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.Provider = "azure"
	cfg.TargetOS = targetLinux
	cfg.AzureLocation = "eastus"
	cfg.AzureOSDisk = AzureOSDiskEphemeralPreview
	cfg.ServerType = "Standard_D32ps_v6"
	cfg.ServerTypeExplicit = true
	client := &AzureClient{Location: "eastus"}
	_, resolved, err := client.createServerWithFallbackInLocation(t.Context(), cfg, "ssh-ed25519 test", "cbx_123456789abc", "bad-preview", false, nil)
	if err == nil {
		t.Fatal("expected unsupported ephemeral-preview SKU to fail")
	}
	if !strings.Contains(err.Error(), "azure.osDisk=ephemeral-preview requires") {
		t.Fatalf("error=%v, want ephemeral-preview validation", err)
	}
	if resolved.ServerType != "Standard_D32ps_v6" {
		t.Fatalf("resolved server type=%q", resolved.ServerType)
	}
}

func TestAzureSnapshotOSDiskID(t *testing.T) {
	t.Parallel()
	managedDiskID := "/subscriptions/test/resourceGroups/test/providers/Microsoft.Compute/disks/source"
	futureOption := armcompute.DiffDiskOptions("FutureSchemaValue")
	tests := []struct {
		name    string
		osDisk  *armcompute.OSDisk
		want    string
		wantErr string
	}{
		{
			name: "ephemeral disk with phantom managed id",
			osDisk: &armcompute.OSDisk{
				DiffDiskSettings: &armcompute.DiffDiskSettings{Option: to.Ptr(armcompute.DiffDiskOptionsLocal)},
				ManagedDisk:      &armcompute.ManagedDiskParameters{ID: to.Ptr(managedDiskID)},
			},
			wantErr: `azure differential OS disk option "Local" on vm source-vm cannot be snapshotted; use --mode archive or relaunch the lease with a managed Azure OS disk`,
		},
		{
			name: "unknown future option",
			osDisk: &armcompute.OSDisk{
				DiffDiskSettings: &armcompute.DiffDiskSettings{Option: &futureOption},
				ManagedDisk:      &armcompute.ManagedDiskParameters{ID: to.Ptr(managedDiskID)},
			},
			wantErr: `azure differential OS disk option "FutureSchemaValue" on vm source-vm cannot be snapshotted; use --mode archive or relaunch the lease with a managed Azure OS disk`,
		},
		{
			name:   "managed disk",
			osDisk: &armcompute.OSDisk{ManagedDisk: &armcompute.ManagedDiskParameters{ID: to.Ptr(managedDiskID)}},
			want:   managedDiskID,
		},
		{name: "missing disk", wantErr: "snapshot source VM has no managed OS disk"},
		{name: "missing managed id", osDisk: &armcompute.OSDisk{}, wantErr: "snapshot source VM has no managed OS disk"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := azureSnapshotOSDiskID("source-vm", test.osDisk)
			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("error=%v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("disk id=%q, want %q", got, test.want)
			}
		})
	}
}

func TestAzureEphemeralFullCachingVMPayload(t *testing.T) {
	t.Parallel()
	vm := armcompute.VirtualMachine{
		Location: to.Ptr("eastus"),
		Properties: &armcompute.VirtualMachineProperties{
			StorageProfile: &armcompute.StorageProfile{
				OSDisk: &armcompute.OSDisk{
					CreateOption: to.Ptr(armcompute.DiskCreateOptionTypesFromImage),
					Caching:      to.Ptr(armcompute.CachingTypesReadOnly),
					DiffDiskSettings: &armcompute.DiffDiskSettings{
						Option: to.Ptr(armcompute.DiffDiskOptionsLocal),
					},
				},
			},
		},
	}
	data, err := azureEphemeralFullCachingVMPayload(vm)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	properties := payload["properties"].(map[string]any)
	storageProfile := properties["storageProfile"].(map[string]any)
	osDisk := storageProfile["osDisk"].(map[string]any)
	diffDiskSettings := osDisk["diffDiskSettings"].(map[string]any)
	if diffDiskSettings["enableFullCaching"] != true {
		t.Fatalf("enableFullCaching=%v", diffDiskSettings["enableFullCaching"])
	}
	if diffDiskSettings["option"] != "Local" {
		t.Fatalf("option=%v", diffDiskSettings["option"])
	}
	if osDisk["caching"] != "ReadOnly" {
		t.Fatalf("caching=%v", osDisk["caching"])
	}
	managedDisk := osDisk["managedDisk"].(map[string]any)
	if managedDisk["storageAccountType"] != "StandardSSD_LRS" {
		t.Fatalf("managedDisk=%v", managedDisk)
	}
}

func TestAzureComputerNameWindowsLimit(t *testing.T) {
	t.Parallel()
	got := azureComputerName("crabbox-coral-lobster-c9adbbb9", "cbx_8556d7bc1580", targetWindows)
	if len(got) > 15 {
		t.Fatalf("computer name %q length=%d", got, len(got))
	}
	if got != "cbxcbx8556d7bc1" {
		t.Fatalf("got %q", got)
	}
	if linux := azureComputerName("crabbox-coral-lobster-c9adbbb9", "cbx_8556d7bc1580", targetLinux); linux != "crabbox-coral-lobster-c9adbbb9" {
		t.Fatalf("linux computer name changed to %q", linux)
	}
}

func TestAzureWindowsBootstrapPowerShell(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.Provider = "azure"
	cfg.TargetOS = targetWindows
	cfg.WorkRoot = defaultWindowsWorkRoot
	defaultWorkRootCfg := cfg
	defaultWorkRootCfg.WorkRoot = ""
	if got := azureWindowsBootstrapPowerShell(defaultWorkRootCfg, "ssh-rsa test"); !strings.Contains(got, `$workRoot = 'C:\crabbox'`) {
		t.Fatalf("azure bootstrap should default work root")
	}
	got := azureWindowsBootstrapPowerShell(cfg, "ssh-rsa test")
	for _, want := range []string{
		"OpenSSH-Win64.zip",
		"Git-2.52.0-64-bit.exe",
		"administrators_authorized_keys",
		"Match Group administrators",
		"$sshPorts = @('2222', '22')",
		"PasswordAuthentication no",
		"Restart-Service sshd -Force",
		"Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("bootstrap missing %q", want)
		}
	}
	if strings.Contains(got, "Restart-Computer") {
		t.Fatalf("azure extension bootstrap must not restart inside Custom Script Extension")
	}
	setupIndex := strings.Index(got, "Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath")
	restartIndex := strings.Index(got, "Restart-Service sshd -Force")
	if setupIndex < 0 || restartIndex < 0 {
		t.Fatalf("azure bootstrap missing setup/restart markers")
	}
	if setupIndex > restartIndex {
		t.Fatalf("azure bootstrap must mark setup complete before restarting sshd")
	}
}

func TestAzureTagsMapReservedWindowsPrefix(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		"crabbox":      "true",
		"windows_mode": "normal",
	}
	tags := azureTagsFromLabels(labels)
	if tags["windows_mode"] != "" {
		t.Fatalf("reserved windows tag key was not remapped: %#v", tags)
	}
	if tags["crabbox_windows_mode"] != "normal" {
		t.Fatalf("missing remapped windows mode tag: %#v", tags)
	}
	server := azureVMToServer(armcompute.VirtualMachine{
		Tags: stringMapToPtrMap(tags),
	}, "", "")
	if server.Labels["windows_mode"] != "normal" {
		t.Fatalf("windows_mode label not restored: %#v", server.Labels)
	}
}

func TestAzureSKUCapabilityTrue(t *testing.T) {
	t.Parallel()
	caps := []*armcompute.ResourceSKUCapabilities{
		{Name: to.Ptr("EphemeralOSDiskSupported"), Value: to.Ptr("True")},
	}
	if !azureSKUCapabilityTrue(caps, "EphemeralOSDiskSupported") {
		t.Fatal("capability should be true")
	}
	caps[0].Value = to.Ptr("False")
	if azureSKUCapabilityTrue(caps, "EphemeralOSDiskSupported") {
		t.Fatal("capability should be false")
	}
}

func TestStringMapToPtrMap(t *testing.T) {
	t.Parallel()
	in := map[string]string{"a": "1", "b": "2"}
	out := stringMapToPtrMap(in)
	if len(out) != 2 {
		t.Fatalf("len=%d, want 2", len(out))
	}
	if *out["a"] != "1" || *out["b"] != "2" {
		t.Fatalf("values = %v, %v", *out["a"], *out["b"])
	}
}

func TestIsAzureRetryableProvisioningError(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"":                 false,
		"some other error": false,
		"compute.VMs: SkuNotAvailable in this region":      true,
		"QuotaExceeded for cores":                          true,
		"AllocationFailed: out of capacity":                true,
		"OverconstrainedAllocationRequest: zone exhausted": true,
		"NotAvailableForSubscription":                      true,
	}
	for msg, want := range cases {
		var err error
		if msg != "" {
			err = errSentinel(msg)
		}
		if got := isAzureRetryableProvisioningError(err); got != want {
			t.Fatalf("msg=%q got %v want %v", msg, got, want)
		}
	}
}

func TestIsAzureNotFoundError(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"":          false,
		"transient": false,
		"ResponseError: ResourceNotFound: vm missing": true,
		"NotFound: pip already deleted":               true,
	}
	for msg, want := range cases {
		var err error
		if msg != "" {
			err = errSentinel(msg)
		}
		if got := isAzureNotFoundError(err); got != want {
			t.Fatalf("msg=%q got %v want %v", msg, got, want)
		}
	}
}

func TestIsAzureRetryableDeleteError(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"":                  false,
		"validation failed": false,
		"NicReservedForAnotherVm retry after 180 seconds":    true,
		"PublicIPAddressCannotBeDeleted because in use":      true,
		"NetworkSecurityGroupCannotBeDeleted because in use": true,
		"DiskIsAttachedToVM: disk is still attached":         true,
		"DiskInUse: managed disk has active lease":           true,
		"AnotherOperationInProgress":                         true,
		"OperationNotAllowed retry after 180 seconds":        true,
	}
	for msg, want := range cases {
		var err error
		if msg != "" {
			err = errSentinel(msg)
		}
		if got := isAzureRetryableDeleteError(err); got != want {
			t.Fatalf("msg=%q got %v want %v", msg, got, want)
		}
	}
}

func TestWaitForAzureExtensionUsesResourceSuccess(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pollStarted := make(chan struct{})
	err := waitForAzureExtension(ctx, time.Millisecond, func(pollCtx context.Context) error {
		close(pollStarted)
		<-pollCtx.Done()
		return context.Cause(pollCtx)
	}, func(context.Context) (string, error) {
		return "Succeeded", nil
	})
	if err != nil {
		t.Fatalf("waitForAzureExtension returned %v, want nil", err)
	}
	select {
	case <-pollStarted:
	default:
		t.Fatal("SDK poller was not started")
	}
}

func TestWaitForAzureExtensionReturnsTerminalResourceFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := waitForAzureExtension(ctx, time.Millisecond, func(pollCtx context.Context) error {
		<-pollCtx.Done()
		return context.Cause(pollCtx)
	}, func(context.Context) (string, error) {
		return "Failed", nil
	})
	if err == nil || !strings.Contains(err.Error(), "reached Failed") {
		t.Fatalf("waitForAzureExtension returned %v, want terminal resource failure", err)
	}
}

func TestWaitForAzureExtensionReturnsPollerFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("poller failed")
	err := waitForAzureExtension(context.Background(), time.Hour, func(context.Context) error {
		return want
	}, func(context.Context) (string, error) {
		return "", nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("waitForAzureExtension returned %v, want wrapped %v", err, want)
	}
}

func TestWaitForAzureExtensionHonorsContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitForAzureExtension(ctx, time.Hour, func(pollCtx context.Context) error {
		<-pollCtx.Done()
		return context.Cause(pollCtx)
	}, func(context.Context) (string, error) {
		return "", nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForAzureExtension returned %v, want context cancellation", err)
	}
}

func TestPreserveNonCrabboxRules(t *testing.T) {
	t.Parallel()
	in := []*armnetwork.SecurityRule{
		{Name: to.Ptr("crabbox-ssh-2222-0")},
		{Name: to.Ptr("operator-https")},
		nil,
		{},
	}
	got := preserveNonCrabboxRules(in)
	if len(got) != 1 || got[0] == nil || got[0].Name == nil || *got[0].Name != "operator-https" {
		t.Fatalf("got %+v, want a single operator-https rule", got)
	}
}

func TestNextAzureNSGPrioritySkipsPreservedRules(t *testing.T) {
	t.Parallel()
	used := azureNSGUsedPriorities([]*armnetwork.SecurityRule{{
		Name: to.Ptr("operator-ssh"),
		Properties: &armnetwork.SecurityRulePropertiesFormat{
			Priority: to.Ptr[int32](100),
		},
	}})
	got, err := nextAzureNSGPriority(used)
	if err != nil {
		t.Fatal(err)
	}
	if got != 101 {
		t.Fatalf("got %d want 101", got)
	}
}

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

func TestAzureManagedByCrabbox(t *testing.T) {
	t.Parallel()
	val := "crabbox"
	other := "platform-team"
	cases := []struct {
		name string
		tags map[string]*string
		want bool
	}{
		{name: "nil tags", tags: nil, want: false},
		{name: "missing key", tags: map[string]*string{"crabbox": &val}, want: false},
		{name: "wrong value", tags: map[string]*string{"managed_by": &other}, want: false},
		{name: "match", tags: map[string]*string{"managed_by": &val}, want: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := azureManagedByCrabbox(tc.tags); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestAzureCredentialForConfigPrefersClientSecret(t *testing.T) {
	t.Setenv("AZURE_CLIENT_SECRET", "shh")
	cfg := Config{
		AzureTenant:   "00000000-0000-0000-0000-000000000001",
		AzureClientID: "00000000-0000-0000-0000-000000000002",
	}
	cred, err := azureCredentialForConfig(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := cred.(*azidentity.ClientSecretCredential); !ok {
		t.Fatalf("got %T, want *azidentity.ClientSecretCredential", cred)
	}
}

func TestAzureCredentialForConfigFallsBackToDefault(t *testing.T) {
	// Make sure env vars don't accidentally yield ClientSecretCredential.
	t.Setenv("AZURE_CLIENT_SECRET", "")
	cfg := Config{AzureTenant: "tenant", AzureClientID: "client"}
	cred, err := azureCredentialForConfig(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := cred.(*azidentity.ClientSecretCredential); ok {
		t.Fatalf("got ClientSecretCredential, want DefaultAzureCredential")
	}
	if _, ok := cred.(*azidentity.DefaultAzureCredential); !ok {
		t.Fatalf("got %T, want *azidentity.DefaultAzureCredential", cred)
	}
}

func TestNewAzureClientAutoResolvesSubscription(t *testing.T) {
	// When az CLI is not available and no subscription is set, NewAzureClient
	// should return an error mentioning both AZURE_SUBSCRIPTION_ID and az login.
	t.Setenv("AZURE_SUBSCRIPTION_ID", "")
	t.Setenv("CRABBOX_AZURE_SUBSCRIPTION_ID", "")
	t.Setenv("PATH", "")

	cfg := defaultConfig()
	cfg.Provider = "azure"
	cfg.AzureSubscription = ""
	cfg.AzureLocation = "eastus"

	_, err := NewAzureClient(t.Context(), cfg)
	if err == nil {
		t.Fatal("expected error when no subscription and no az CLI")
	}
	if !strings.Contains(err.Error(), "az login") {
		t.Fatalf("error should mention 'az login': %v", err)
	}
}

func TestNewAzureClientDoesNotResolveDefaultSSHCIDRs(t *testing.T) {
	prev := detectOutboundIPv4CIDRFunc
	detectOutboundIPv4CIDRFunc = func(context.Context) (string, error) {
		t.Fatal("NewAzureClient should not resolve SSH CIDRs before provisioning")
		return "", nil
	}
	t.Cleanup(func() { detectOutboundIPv4CIDRFunc = prev })

	cfg := defaultConfig()
	cfg.Provider = "azure"
	cfg.AzureSubscription = "sub"
	cfg.AzureLocation = "eastus"

	client, err := NewAzureClient(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(client.SSHCIDRs) != 0 {
		t.Fatalf("SSHCIDRs=%v, want unresolved until ensureNSG", client.SSHCIDRs)
	}
}

func TestAzureSSHCIDRsForConfigUsesExplicitCIDRs(t *testing.T) {
	prev := detectOutboundIPv4CIDRFunc
	detectOutboundIPv4CIDRFunc = func(context.Context) (string, error) {
		t.Fatal("explicit Azure SSH CIDRs should not call outbound detection")
		return "", nil
	}
	t.Cleanup(func() { detectOutboundIPv4CIDRFunc = prev })

	got, err := azureSSHCIDRsForConfig(t.Context(), Config{AzureSSHCIDRs: []string{"0.0.0.0/0"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"0.0.0.0/0"}) {
		t.Fatalf("cidrs=%v, want explicit world-open CIDR", got)
	}
}

func TestAzureSSHCIDRsForConfigDetectsOperatorIPv4(t *testing.T) {
	prev := detectOutboundIPv4CIDRFunc
	detectOutboundIPv4CIDRFunc = func(context.Context) (string, error) {
		return "198.51.100.7/32", nil
	}
	t.Cleanup(func() { detectOutboundIPv4CIDRFunc = prev })

	got, err := azureSSHCIDRsForConfig(t.Context(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"198.51.100.7/32"}) {
		t.Fatalf("cidrs=%v, want detected /32", got)
	}
}

func TestAzureSSHCIDRsForConfigRequiresExplicitCIDRsWhenDetectionFails(t *testing.T) {
	prev := detectOutboundIPv4CIDRFunc
	detectOutboundIPv4CIDRFunc = func(context.Context) (string, error) {
		return "", errors.New("offline")
	}
	t.Cleanup(func() { detectOutboundIPv4CIDRFunc = prev })

	_, err := azureSSHCIDRsForConfig(t.Context(), Config{})
	if err == nil || !strings.Contains(err.Error(), "CRABBOX_AZURE_SSH_CIDRS") || !strings.Contains(err.Error(), "0.0.0.0/0 only if") {
		t.Fatalf("err=%v, want explicit CIDR guidance", err)
	}
}

func TestAzureSSHCIDRsForRulesKeepsMatchingExistingManagedCIDRs(t *testing.T) {
	prev := detectOutboundIPv4CIDRFunc
	detectOutboundIPv4CIDRFunc = func(context.Context) (string, error) {
		return "203.0.113.9/32", nil
	}
	t.Cleanup(func() { detectOutboundIPv4CIDRFunc = prev })

	rules := []*armnetwork.SecurityRule{
		{
			Name: to.Ptr("crabbox-ssh-2222-0"),
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				SourceAddressPrefix: to.Ptr("203.0.113.9/32"),
			},
		},
		{
			Name: to.Ptr("crabbox-ssh-22-0"),
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				SourceAddressPrefix: to.Ptr("0.0.0.0/0"),
			},
		},
		{
			Name: to.Ptr("user-owned"),
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				SourceAddressPrefix: to.Ptr("203.0.113.10/32"),
			},
		},
	}

	got, err := azureSSHCIDRsForRules(t.Context(), Config{}, rules)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"203.0.113.9/32"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cidrs=%v, want %v", got, want)
	}
}

func TestAzureSSHCIDRsForRulesRejectsDifferentDetectedCIDRForSharedNSG(t *testing.T) {
	prev := detectOutboundIPv4CIDRFunc
	detectOutboundIPv4CIDRFunc = func(context.Context) (string, error) {
		return "198.51.100.7/32", nil
	}
	t.Cleanup(func() { detectOutboundIPv4CIDRFunc = prev })

	rules := []*armnetwork.SecurityRule{{
		Name: to.Ptr("crabbox-ssh-2222-0"),
		Properties: &armnetwork.SecurityRulePropertiesFormat{
			SourceAddressPrefix: to.Ptr("203.0.113.9/32"),
		},
	}}

	_, err := azureSSHCIDRsForRules(t.Context(), Config{}, rules)
	if err == nil || !strings.Contains(err.Error(), "already has managed SSH CIDRs") || !strings.Contains(err.Error(), "CRABBOX_AZURE_SSH_CIDRS") {
		t.Fatalf("err=%v, want explicit CIDR guidance for shared NSG", err)
	}
}

func TestAzureSSHCIDRsForRulesRequiresExplicitPrivateNetworkCIDRs(t *testing.T) {
	prev := detectOutboundIPv4CIDRFunc
	detectOutboundIPv4CIDRFunc = func(context.Context) (string, error) {
		t.Fatal("private Azure network should not use public outbound detection")
		return "", nil
	}
	t.Cleanup(func() { detectOutboundIPv4CIDRFunc = prev })

	_, err := azureSSHCIDRsForRules(t.Context(), Config{AzureNetwork: "private"}, nil)
	if err == nil || !strings.Contains(err.Error(), "VPN/VNet source CIDR") {
		t.Fatalf("err=%v, want explicit private-network CIDR guidance", err)
	}
}

func TestAzureSSHCIDRsForRulesUsesExplicitCIDRsWithoutMerging(t *testing.T) {
	prev := detectOutboundIPv4CIDRFunc
	detectOutboundIPv4CIDRFunc = func(context.Context) (string, error) {
		t.Fatal("explicit Azure SSH CIDRs should not call outbound detection")
		return "", nil
	}
	t.Cleanup(func() { detectOutboundIPv4CIDRFunc = prev })

	rules := []*armnetwork.SecurityRule{{
		Name: to.Ptr("crabbox-ssh-2222-0"),
		Properties: &armnetwork.SecurityRulePropertiesFormat{
			SourceAddressPrefix: to.Ptr("203.0.113.9/32"),
		},
	}}
	got, err := azureSSHCIDRsForRules(t.Context(), Config{AzureSSHCIDRs: []string{"198.51.100.8/32"}}, rules)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"198.51.100.8/32"}) {
		t.Fatalf("cidrs=%v, want explicit only", got)
	}
}

func TestAzureServerHostSelectsPrivateIP(t *testing.T) {
	t.Parallel()
	server := Server{}
	server.PublicNet.IPv4.IP = "20.168.181.119"
	server.PrivateNet.IPv4.IP = "10.42.0.4"

	if got := AzureServerHost(server, "public"); got != "20.168.181.119" {
		t.Fatalf("public network: got %q, want public IP", got)
	}
	if got := AzureServerHost(server, "private"); got != "10.42.0.4" {
		t.Fatalf("private network: got %q, want private IP", got)
	}
	if got := AzureServerHost(server, ""); got != "20.168.181.119" {
		t.Fatalf("empty network: got %q, want public IP", got)
	}
	if got := AzureServerHost(server, "PRIVATE"); got != "10.42.0.4" {
		t.Fatalf("case-insensitive: got %q, want private IP", got)
	}
}

func TestAzureServerHostFallsBackToPublicWhenNoPrivateIP(t *testing.T) {
	t.Parallel()
	server := Server{}
	server.PublicNet.IPv4.IP = "20.168.181.119"

	if got := AzureServerHost(server, "private"); got != "20.168.181.119" {
		t.Fatalf("private with no private IP: got %q, want public IP fallback", got)
	}
}

func TestAzureVMToServerSetsPrivateIP(t *testing.T) {
	t.Parallel()
	server := azureVMToServer(armcompute.VirtualMachine{
		Properties: &armcompute.VirtualMachineProperties{VMID: to.Ptr("vmid-live")},
	}, "1.2.3.4", "10.0.0.5")
	if server.PublicNet.IPv4.IP != "1.2.3.4" {
		t.Fatalf("public IP: got %q", server.PublicNet.IPv4.IP)
	}
	if server.PrivateNet.IPv4.IP != "10.0.0.5" {
		t.Fatalf("private IP: got %q", server.PrivateNet.IPv4.IP)
	}
	if server.ImmutableID != "vmid-live" {
		t.Fatalf("immutable ID: got %q, want vmid-live", server.ImmutableID)
	}
}
