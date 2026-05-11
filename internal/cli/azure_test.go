package cli

import (
	"reflect"
	"strings"
	"testing"

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

func TestAzureImageForConfig(t *testing.T) {
	t.Parallel()
	linux := baseConfig()
	linux.TargetOS = targetLinux
	if got := azureImageForConfig(linux); got != defaultAzureLinuxImage {
		t.Fatalf("linux image=%q want %q", got, defaultAzureLinuxImage)
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
	if !reflect.DeepEqual(wsl2, []string{"standard"}) {
		t.Fatalf("wsl2 target got %v want explicit fallback", wsl2)
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
		"NicReservedForAnotherVm retry after 180 seconds": true,
		"PublicIPAddressCannotBeDeleted because in use":   true,
		"AnotherOperationInProgress":                      true,
		"OperationNotAllowed retry after 180 seconds":     true,
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
	server := azureVMToServer(armcompute.VirtualMachine{}, "1.2.3.4", "10.0.0.5")
	if server.PublicNet.IPv4.IP != "1.2.3.4" {
		t.Fatalf("public IP: got %q", server.PublicNet.IPv4.IP)
	}
	if server.PrivateNet.IPv4.IP != "10.0.0.5" {
		t.Fatalf("private IP: got %q", server.PrivateNet.IPv4.IP)
	}
}
