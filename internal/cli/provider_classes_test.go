package cli

import (
	"flag"
	"reflect"
	"strings"
	"testing"
)

type selectorClassProfileProvider struct{}

type selectorOverrideProvider struct {
	selectorClassProfileProvider
	resolve  func(Config) string
	override func(Config) (string, bool)
}

func (p selectorOverrideProvider) ServerTypeForConfig(cfg Config) string { return p.resolve(cfg) }
func (selectorOverrideProvider) ServerTypeForClass(class string) string  { return class }
func (p selectorOverrideProvider) ServerTypeOverrideForConfig(cfg Config) (string, bool) {
	if p.override == nil {
		return "", false
	}
	return p.override(cfg)
}

func testProfile(class, target, windowsMode string, architecture ProviderClassArchitecture, candidates ...string) ProviderClassProfile {
	machines := make([]ProviderClassMachine, 0, len(candidates))
	for _, candidate := range candidates {
		machineArchitecture := architecture
		if architecture == ProviderClassArchitectureMixed {
			machineArchitecture = ProviderClassArchitectureARM64
			if candidate == "mac1.metal" {
				machineArchitecture = ProviderClassArchitectureAMD64
			}
		}
		machines = append(machines, ProviderClassMachine{Type: candidate, Architecture: machineArchitecture})
	}
	return ProviderClassProfileFromMachines(class, target, windowsMode, architecture, machines)
}

func testLinuxProfiles(types []string) []ProviderClassProfile {
	profiles := make([]ProviderClassProfile, 0, len(types))
	for index, class := range CanonicalProviderClasses() {
		profiles = append(profiles, testProfile(class, targetLinux, "", ProviderClassArchitectureAMD64, types[index]))
	}
	return profiles
}

func TestProviderClassSpecsFromProfilesProjectsDefaultSelector(t *testing.T) {
	intPointer := func(value int) *int { return &value }
	profiles := []ProviderClassProfile{
		ProviderClassProfileFromMachines("beast", targetLinux, "", ProviderClassArchitectureAMD64, []ProviderClassMachine{{
			Type: "beast-primary", Architecture: ProviderClassArchitectureAMD64, VCPU: intPointer(32),
			Memory: &ProviderMemory{Value: 64, Unit: ProviderMemoryUnitGB},
		}}),
		ProviderClassProfileFromMachines("standard", targetLinux, "", ProviderClassArchitectureARM64, []ProviderClassMachine{{
			Type: "wrong-architecture", Architecture: ProviderClassArchitectureARM64,
		}}),
		ProviderClassProfileFromMachines("fast", targetLinux, "", ProviderClassArchitectureAMD64, []ProviderClassMachine{
			{Type: "fast-primary", Architecture: ProviderClassArchitectureAMD64, VCPU: intPointer(8), Memory: &ProviderMemory{Value: 15.5, Unit: ProviderMemoryUnitGB}},
			{Type: "fast-fallback", Architecture: ProviderClassArchitectureAMD64, VCPU: intPointer(4), Memory: &ProviderMemory{Value: 8, Unit: ProviderMemoryUnitGB}},
		}),
		ProviderClassProfileFromMachines("large", targetLinux, "", ProviderClassArchitectureAMD64, []ProviderClassMachine{{
			Type: "large-primary", Architecture: ProviderClassArchitectureAMD64, Memory: &ProviderMemory{Value: 32768, Unit: ProviderMemoryUnitMiB},
		}}),
		ProviderClassProfileFromMachines("standard", targetLinux, "", ProviderClassArchitectureAMD64, []ProviderClassMachine{{
			Type: "standard-primary", Architecture: ProviderClassArchitectureAMD64, VCPU: intPointer(4),
			Memory: &ProviderMemory{Value: 8, Unit: ProviderMemoryUnitGiB},
		}}),
	}

	want := []ClassSpec{
		{Class: "standard", Type: "standard-primary", VCPUs: 4, MemoryGB: 8},
		{Class: "fast", Type: "fast-primary", VCPUs: 8},
		{Class: "large", Type: "large-primary"},
		{Class: "beast", Type: "beast-primary", VCPUs: 32, MemoryGB: 64},
	}
	if got := ProviderClassSpecsFromProfiles(profiles); !reflect.DeepEqual(got, want) {
		t.Fatalf("compatibility projection=%#v want %#v", got, want)
	}
}

func (testAWSProvider) ClassProfiles() []ProviderClassProfile {
	primaryAMD64 := map[string]string{"standard": "c7a.8xlarge", "fast": "c7a.16xlarge", "large": "c7a.24xlarge", "beast": "c7a.48xlarge"}
	primaryARM64 := map[string]string{"standard": "c7g.8xlarge", "fast": "c7g.16xlarge", "large": "c7g.16xlarge", "beast": "c7g.16xlarge"}
	primaryWindows := map[string]string{"standard": "m7i.large", "fast": "m7i.xlarge", "large": "m7i.2xlarge", "beast": "m7i.4xlarge"}
	primaryWSL2 := map[string]string{"standard": "m8i.large", "fast": "m8i.xlarge", "large": "m8i.2xlarge", "beast": "m8i.4xlarge"}
	mac := []string{"mac2.metal", "mac2-m2.metal", "mac2-m2pro.metal", "mac-m4.metal", "mac-m4pro.metal", "mac-m4max.metal", "mac2-m1ultra.metal", "mac-m3ultra.metal", "mac1.metal"}
	profiles := make([]ProviderClassProfile, 0, 20)
	for _, class := range CanonicalProviderClasses() {
		profiles = append(profiles,
			testProfile(class, targetLinux, "", ProviderClassArchitectureAMD64, primaryAMD64[class], "t3.small"),
			testProfile(class, targetLinux, "", ProviderClassArchitectureARM64, primaryARM64[class], "t4g.small"),
			testProfile(class, targetWindows, windowsModeNormal, ProviderClassArchitectureAMD64, primaryWindows[class], "t3.large"),
			testProfile(class, targetWindows, windowsModeWSL2, ProviderClassArchitectureAMD64, primaryWSL2[class], "m8i.large"),
			testProfile(class, targetMacOS, "", ProviderClassArchitectureMixed, mac...),
		)
	}
	return profiles
}

func (testAzureProvider) ClassProfiles() []ProviderClassProfile {
	linuxAMD64 := map[string][]string{
		"standard": {"Standard_D32ads_v6"}, "fast": {"Standard_D64ads_v6"}, "large": {"Standard_D96ads_v6"}, "beast": {"Standard_D192ds_v6"},
	}
	arm64 := map[string][]string{
		"standard": {"Standard_D32pds_v6", "Standard_D32ps_v6", "Standard_D16pds_v6", "Standard_D16ps_v6"},
		"fast":     {"Standard_D64pds_v6"}, "large": {"Standard_D96pds_v6"}, "beast": {"Standard_D96pds_v6"},
	}
	windows := map[string][]string{
		"standard": {"Standard_D2ads_v6", "Standard_D2ds_v6", "Standard_D2ads_v5", "Standard_D2ds_v5", "Standard_D2as_v6"},
		"fast":     {"Standard_D4ads_v6"},
		"large":    {"Standard_D8ads_v6", "Standard_D8ds_v6", "Standard_D8ads_v5", "Standard_D8ds_v5", "Standard_D8as_v6"},
		"beast":    {"Standard_D16ads_v6", "Standard_D16ds_v6", "Standard_D16ads_v5", "Standard_D16ds_v5", "Standard_D8ads_v6"},
	}
	profiles := make([]ProviderClassProfile, 0, 20)
	for _, class := range CanonicalProviderClasses() {
		profiles = append(profiles,
			testProfile(class, targetLinux, "", ProviderClassArchitectureAMD64, linuxAMD64[class]...),
			testProfile(class, targetLinux, "", ProviderClassArchitectureARM64, arm64[class]...),
			testProfile(class, targetWindows, windowsModeNormal, ProviderClassArchitectureAMD64, windows[class]...),
			testProfile(class, targetWindows, windowsModeNormal, ProviderClassArchitectureARM64, arm64[class]...),
			testProfile(class, targetWindows, windowsModeWSL2, ProviderClassArchitectureAMD64, windows[class]...),
		)
	}
	return profiles
}

func (testGCPProvider) ClassProfiles() []ProviderClassProfile {
	return testLinuxProfiles([]string{"c4-standard-32", "c4-standard-64", "c4-standard-96", "c4-standard-192"})
}

func (testHetznerProvider) ClassProfiles() []ProviderClassProfile {
	return testLinuxProfiles([]string{"ccx33", "ccx43", "ccx53", "ccx63"})
}

func (testNamespaceProvider) ClassProfiles() []ProviderClassProfile {
	return testLinuxProfiles([]string{"S", "M", "L", "XL"})
}

func (testCloudflareProvider) ClassProfiles() []ProviderClassProfile {
	return UniformLinuxAMD64ClassProfiles(ProviderClassMachine{Type: "standard-4"})
}

func (selectorClassProfileProvider) Name() string      { return "selector-class-profile" }
func (selectorClassProfileProvider) Aliases() []string { return []string{"selector-class-alias"} }
func (selectorClassProfileProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name: "selector-class-profile", Kind: ProviderKindSSHLease,
		Targets:          []TargetSpec{{OS: targetLinux}, {OS: targetWindows, WindowsMode: windowsModeNormal}, {OS: targetWindows, WindowsMode: windowsModeWSL2}},
		ClassDisposition: ProviderClassDispositionMapped,
	}
}
func (selectorClassProfileProvider) RegisterFlags(*flag.FlagSet, Config) any {
	return noProviderFlags{}
}
func (selectorClassProfileProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (selectorClassProfileProvider) Configure(Config, Runtime) (Backend, error) { return nil, nil }
func (selectorClassProfileProvider) ClassProfiles() []ProviderClassProfile {
	return []ProviderClassProfile{
		testProfile("standard", targetLinux, "", ProviderClassArchitectureMixed, "mixed-primary", "mixed-fallback"),
		testProfile("standard", targetLinux, "", ProviderClassArchitectureAMD64, "amd64-primary", "amd64-fallback"),
		testProfile("standard", targetWindows, windowsModeNormal, ProviderClassArchitectureAMD64, "windows-normal"),
		testProfile("standard", targetWindows, windowsModeWSL2, ProviderClassArchitectureAMD64, "windows-wsl2"),
	}
}

func TestProviderClassProfileSelectionIsExact(t *testing.T) {
	provider := selectorClassProfileProvider{}

	tests := []struct {
		name string
		cfg  Config
		want []string
		ok   bool
	}{
		{name: "exact architecture beats mixed", cfg: Config{Provider: provider.Name(), TargetOS: targetLinux, Architecture: ArchitectureAMD64, Class: "standard"}, want: []string{"amd64-primary", "amd64-fallback"}, ok: true},
		{name: "mixed is explicit fallback", cfg: Config{Provider: provider.Name(), TargetOS: targetLinux, Architecture: ArchitectureARM64, Class: "standard"}, want: []string{"mixed-primary", "mixed-fallback"}, ok: true},
		{name: "empty Windows mode normalizes", cfg: Config{Provider: provider.Name(), TargetOS: targetWindows, Architecture: ArchitectureAMD64, Class: "standard"}, want: []string{"windows-normal"}, ok: true},
		{name: "exact Windows mode", cfg: Config{Provider: provider.Name(), TargetOS: targetWindows, WindowsMode: windowsModeWSL2, Architecture: ArchitectureAMD64, Class: "standard"}, want: []string{"windows-wsl2"}, ok: true},
		{name: "no architecture fallback", cfg: Config{Provider: provider.Name(), TargetOS: targetWindows, WindowsMode: windowsModeNormal, Architecture: ArchitectureARM64, Class: "standard"}},
		{name: "no Windows mode fallback", cfg: Config{Provider: provider.Name(), TargetOS: targetWindows, WindowsMode: "future", Architecture: ArchitectureAMD64, Class: "standard"}},
		{name: "custom class not synthesized", cfg: Config{Provider: provider.Name(), TargetOS: targetLinux, Architecture: ArchitectureAMD64, Class: "custom"}},
		{name: "uppercase class not normalized", cfg: Config{Provider: provider.Name(), TargetOS: targetLinux, Architecture: ArchitectureAMD64, Class: "STANDARD"}},
		{name: "padded class not trimmed", cfg: Config{Provider: provider.Name(), TargetOS: targetLinux, Architecture: ArchitectureAMD64, Class: " standard "}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := ProviderClassCandidatesForProfiles(provider.ClassProfiles(), test.cfg)
			if ok != test.ok {
				t.Fatalf("profile found=%t want %t", ok, test.ok)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("candidates=%v want %v", got, test.want)
			}
		})
	}
}

func TestCanonicalProviderClassRequiresMatchingSelector(t *testing.T) {
	provider := selectorClassProfileProvider{}
	cfg := Config{Provider: provider.Name(), TargetOS: targetWindows, WindowsMode: windowsModeNormal, Architecture: ArchitectureARM64, Class: "standard"}
	err := validateProviderClassSelector(provider, cfg)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(err.Error(), "no class profile") {
		t.Fatalf("err=%v want exit 2 class-profile error", err)
	}

	for _, class := range []string{"STANDARD", " standard ", "custom"} {
		cfg.Class = class
		if err := validateProviderClassSelector(provider, cfg); err != nil {
			t.Errorf("class=%q err=%v", class, err)
		}
	}

	cfg.Class = "standard"
	cfg.ServerType = "exact-arm-type"
	cfg.ServerTypeExplicit = true
	if err := validateProviderClassSelector(provider, cfg); err != nil {
		t.Fatalf("explicit type err=%v", err)
	}
	cfg.ServerType = ""
	if err := validateProviderClassSelector(provider, cfg); err == nil {
		t.Fatal("empty explicit type unexpectedly bypassed selector validation")
	}
}

func TestMappedProvidersRejectUnsupportedCanonicalSelectors(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "hetzner linux arm64", cfg: Config{Provider: "hetzner", TargetOS: targetLinux, Architecture: ArchitectureARM64, architectureExplicit: true, Class: "fast"}},
		{name: "aws windows arm64", cfg: Config{Provider: "aws", TargetOS: targetWindows, WindowsMode: windowsModeNormal, Architecture: ArchitectureARM64, architectureExplicit: true, Class: "standard"}},
		{name: "azure wsl2 arm64", cfg: Config{Provider: "azure", TargetOS: targetWindows, WindowsMode: windowsModeWSL2, Architecture: ArchitectureARM64, architectureExplicit: true, Class: "standard"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateProviderConfig(test.cfg)
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(err.Error(), "no class profile") {
				t.Fatalf("err=%v want exit 2 class-profile error", err)
			}
			if got := serverTypeForConfig(test.cfg); got != "" {
				t.Fatalf("serverType=%q want empty", got)
			}
		})
	}
}

func TestProviderNativeTypesBypassMissingClassProfile(t *testing.T) {
	base := Config{Provider: "selector-class-profile", TargetOS: targetWindows, WindowsMode: windowsModeNormal, Architecture: ArchitectureARM64, Class: "fast"}
	coreClassFirst := func(cfg *Config) {
		MarkClassExplicit(cfg)
	}
	tests := []struct {
		name    string
		prepare func(*Config)
		resolve func(Config) string
	}{
		{name: "Namespace size", prepare: func(cfg *Config) { cfg.Namespace.Size = "XL" }, resolve: func(cfg Config) string { return cfg.Namespace.Size }},
		{name: "Linode type", prepare: func(cfg *Config) { cfg.Linode.Type = "g6-standard-2" }, resolve: func(cfg Config) string { return cfg.Linode.Type }},
		{name: "OVH flavor", prepare: func(cfg *Config) { cfg.OVH.Flavor = "b3-16" }, resolve: func(cfg Config) string { return cfg.OVH.Flavor }},
		{name: "Scaleway type", prepare: func(cfg *Config) { cfg.Scaleway.Type = "DEV1-M" }, resolve: func(cfg Config) string { return cfg.Scaleway.Type }},
		{name: "stored exact type", prepare: func(cfg *Config) { cfg.ServerType = "stored-arm-type" }, resolve: func(cfg Config) string { return cfg.ServerType }},
		{name: "Phala native type", prepare: func(cfg *Config) {
			coreClassFirst(cfg)
			cfg.Phala.InstanceType = "tdx.large"
			MarkPhalaInstanceTypeExplicit(cfg)
		}, resolve: func(cfg Config) string {
			if PhalaInstanceTypeWasExplicit(cfg) && PhalaInstanceTypeOverridesClass(cfg) {
				return cfg.Phala.InstanceType
			}
			return cfg.Class
		}},
		{name: "Tencent native type", prepare: func(cfg *Config) {
			coreClassFirst(cfg)
			cfg.TencentCloud.Type = "S5.SMALL2"
			SetTencentCloudTypeExplicit(cfg)
		}, resolve: func(cfg Config) string {
			if TencentCloudTypeWasExplicit(cfg) {
				return cfg.TencentCloud.Type
			}
			return cfg.Class
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			test.prepare(&cfg)
			provider := selectorOverrideProvider{
				resolve: test.resolve,
				override: func(cfg Config) (string, bool) {
					resolvedType := test.resolve(cfg)
					return resolvedType, strings.TrimSpace(resolvedType) != "" && resolvedType != cfg.Class
				},
			}
			if err := validateProviderClassSelector(provider, cfg); err != nil {
				t.Fatalf("err=%v", err)
			}
		})
	}

	literal := selectorOverrideProvider{resolve: func(cfg Config) string { return cfg.Class }}
	if err := validateProviderClassSelector(literal, base); err == nil {
		t.Fatal("canonical class literal unexpectedly bypassed validation")
	}
	providerDefault := selectorOverrideProvider{resolve: func(Config) string { return "provider-default" }}
	if err := validateProviderClassSelector(providerDefault, base); err == nil {
		t.Fatal("inherited provider default unexpectedly bypassed validation")
	}
}

func TestStoredExactTypesPrecedeMissingClassProfiles(t *testing.T) {
	storedType := "stored-arm-type"
	tests := []struct {
		name string
		got  []string
	}{
		{name: "AWS", got: awsLaunchCandidates(Config{Provider: "aws", TargetOS: targetWindows, WindowsMode: windowsModeNormal, Architecture: ArchitectureARM64, Class: "standard", ServerType: storedType})},
		{name: "Azure", got: azureVMSizeCandidatesForConfig(Config{Provider: "azure", TargetOS: targetWindows, WindowsMode: windowsModeWSL2, Architecture: ArchitectureARM64, Class: "standard", ServerType: storedType})},
		{name: "GCP", got: gcpMachineTypeCandidatesForConfig(Config{Provider: "gcp", TargetOS: targetLinux, Architecture: ArchitectureARM64, Class: "standard", ServerType: storedType})},
		{name: "Hetzner", got: hetznerServerTypeCandidatesForConfig(Config{Provider: "hetzner", TargetOS: targetLinux, Architecture: ArchitectureARM64, Class: "standard", ServerType: storedType})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !reflect.DeepEqual(test.got, []string{storedType}) {
				t.Fatalf("candidates=%v want [%s]", test.got, storedType)
			}
		})
	}

	literalTests := []struct {
		name string
		got  []string
	}{
		{name: "AWS", got: awsLaunchCandidates(Config{Provider: "aws", TargetOS: targetWindows, WindowsMode: windowsModeNormal, Architecture: ArchitectureARM64, Class: "standard", ServerType: "standard"})},
		{name: "Azure", got: azureVMSizeCandidatesForConfig(Config{Provider: "azure", TargetOS: targetWindows, WindowsMode: windowsModeWSL2, Architecture: ArchitectureARM64, Class: "standard", ServerType: "standard"})},
		{name: "GCP", got: gcpMachineTypeCandidatesForConfig(Config{Provider: "gcp", TargetOS: targetLinux, Architecture: ArchitectureARM64, Class: "standard", ServerType: "standard"})},
		{name: "Hetzner", got: hetznerServerTypeCandidatesForConfig(Config{Provider: "hetzner", TargetOS: targetLinux, Architecture: ArchitectureARM64, Class: "standard", ServerType: "standard"})},
	}
	for _, test := range literalTests {
		t.Run(test.name+" canonical literal", func(t *testing.T) {
			if len(test.got) != 0 {
				t.Fatalf("candidates=%v want none", test.got)
			}
		})
	}

	matchedTests := []struct {
		name       string
		stored     []string
		classValue []string
	}{
		{name: "AWS", stored: awsLaunchCandidates(Config{Provider: "aws", TargetOS: targetLinux, Architecture: ArchitectureAMD64, architectureExplicit: true, Class: "standard", ServerType: storedType}), classValue: awsLaunchCandidates(Config{Provider: "aws", TargetOS: targetLinux, Architecture: ArchitectureAMD64, architectureExplicit: true, Class: "standard", ServerType: "standard"})},
		{name: "Azure", stored: azureProvisioningCandidatesForConfig(Config{Provider: "azure", TargetOS: targetLinux, Architecture: ArchitectureAMD64, architectureExplicit: true, Class: "standard", ServerType: storedType}), classValue: azureProvisioningCandidatesForConfig(Config{Provider: "azure", TargetOS: targetLinux, Architecture: ArchitectureAMD64, architectureExplicit: true, Class: "standard", ServerType: "standard"})},
		{name: "GCP", stored: gcpMachineTypeCandidatesForConfig(Config{Provider: "gcp", TargetOS: targetLinux, Architecture: ArchitectureAMD64, architectureExplicit: true, Class: "standard", ServerType: storedType}), classValue: gcpMachineTypeCandidatesForConfig(Config{Provider: "gcp", TargetOS: targetLinux, Architecture: ArchitectureAMD64, architectureExplicit: true, Class: "standard", ServerType: "standard"})},
		{name: "Hetzner", stored: hetznerServerTypeCandidatesForConfig(Config{Provider: "hetzner", TargetOS: targetLinux, Architecture: ArchitectureAMD64, architectureExplicit: true, Class: "standard", ServerType: storedType}), classValue: hetznerServerTypeCandidatesForConfig(Config{Provider: "hetzner", TargetOS: targetLinux, Architecture: ArchitectureAMD64, architectureExplicit: true, Class: "standard", ServerType: "standard"})},
	}
	for _, test := range matchedTests {
		t.Run(test.name+" matched selector", func(t *testing.T) {
			if len(test.stored) == 0 || test.stored[0] != storedType {
				t.Fatalf("stored candidates=%v want %s first", test.stored, storedType)
			}
			for _, candidate := range test.classValue {
				if candidate == "standard" {
					t.Fatalf("canonical literal leaked into candidates=%v", test.classValue)
				}
			}
		})
	}

	padded := Config{Provider: "aws", TargetOS: targetLinux, Architecture: ArchitectureAMD64, architectureExplicit: true, Class: " fast ", ServerType: " fast "}
	if storedType := concreteStoredServerType(padded); storedType != "" {
		t.Fatalf("padded class placeholder treated as stored type %q", storedType)
	}
	if got := awsLaunchCandidates(padded); !reflect.DeepEqual(got, []string{" fast ", "t3.small"}) {
		t.Fatalf("padded custom class candidates=%v want literal plus policy fallback", got)
	}
}

func TestProviderCandidateStoredTypeFallbackParity(t *testing.T) {
	const storedType = "stored-native-type"
	const explicitType = " exact-native-type "
	tests := []struct {
		name          string
		candidates    func(Config) []string
		base          Config
		missing       Config
		customWant    []string
		uppercaseWant []string
		paddedWant    []string
		standardType  string
		fastType      string
	}{
		{
			name: "AWS", candidates: awsLaunchCandidates,
			base:       Config{Provider: "aws", TargetOS: targetLinux, WindowsMode: windowsModeNormal, Architecture: ArchitectureAMD64, architectureExplicit: true},
			missing:    Config{Provider: "aws", TargetOS: targetWindows, WindowsMode: windowsModeNormal, Architecture: ArchitectureARM64, architectureExplicit: true},
			customWant: []string{storedType, "custom-shape", "t3.small"}, uppercaseWant: []string{storedType, "FAST", "t3.small"}, paddedWant: []string{storedType, " fast ", "t3.small"},
			standardType: "c7a.8xlarge", fastType: "c7a.16xlarge",
		},
		{
			name: "Azure", candidates: azureProvisioningCandidatesForConfig,
			base:       Config{Provider: "azure", TargetOS: targetLinux, WindowsMode: windowsModeNormal, Architecture: ArchitectureAMD64, architectureExplicit: true, AzureOSDisk: AzureOSDiskManaged},
			missing:    Config{Provider: "azure", TargetOS: targetWindows, WindowsMode: windowsModeWSL2, Architecture: ArchitectureARM64, architectureExplicit: true, AzureOSDisk: AzureOSDiskManaged},
			customWant: []string{storedType, "custom-shape"}, uppercaseWant: []string{storedType, "FAST"}, paddedWant: []string{storedType, " fast "},
			standardType: "Standard_D32ads_v6", fastType: "Standard_D64ads_v6",
		},
		{
			name: "GCP", candidates: gcpMachineTypeCandidatesForConfig,
			base:       Config{Provider: "gcp", TargetOS: targetLinux, Architecture: ArchitectureAMD64, architectureExplicit: true},
			missing:    Config{Provider: "gcp", TargetOS: targetLinux, Architecture: ArchitectureARM64, architectureExplicit: true},
			customWant: []string{storedType, "custom-shape"}, uppercaseWant: []string{storedType, "FAST"}, paddedWant: []string{storedType, " fast "},
			standardType: "c4-standard-32", fastType: "c4-standard-64",
		},
		{
			name: "Hetzner", candidates: hetznerServerTypeCandidatesForConfig,
			base:       Config{Provider: "hetzner", TargetOS: targetLinux, Architecture: ArchitectureAMD64, architectureExplicit: true},
			missing:    Config{Provider: "hetzner", TargetOS: targetLinux, Architecture: ArchitectureARM64, architectureExplicit: true},
			customWant: []string{storedType, "custom-shape"}, uppercaseWant: []string{storedType, "FAST"}, paddedWant: []string{storedType, " fast "},
			standardType: "ccx33", fastType: "ccx43",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, classTest := range []struct {
				name  string
				class string
				want  []string
			}{
				{name: "custom", class: "custom-shape", want: test.customWant},
				{name: "uppercase", class: "FAST", want: test.uppercaseWant},
				{name: "padded", class: " fast ", want: test.paddedWant},
			} {
				t.Run(classTest.name, func(t *testing.T) {
					cfg := test.base
					cfg.Class = classTest.class
					cfg.ServerType = storedType
					if got := test.candidates(cfg); !reflect.DeepEqual(got, classTest.want) {
						t.Fatalf("candidates=%v want %v", got, classTest.want)
					}
				})
			}

			for _, profileTest := range []struct {
				name        string
				class       string
				placeholder string
				wantFirst   string
			}{
				{name: "old beast placeholder does not override fast", class: "fast", placeholder: "beast", wantFirst: test.fastType},
				{name: "old fast placeholder does not override standard", class: "standard", placeholder: "fast", wantFirst: test.standardType},
			} {
				t.Run(profileTest.name, func(t *testing.T) {
					cfg := test.base
					cfg.Class = profileTest.class
					cfg.ServerType = profileTest.placeholder
					got := test.candidates(cfg)
					if len(got) == 0 || got[0] != profileTest.wantFirst {
						t.Fatalf("candidates=%v want %q first", got, profileTest.wantFirst)
					}
				})
			}

			for _, rawStoredType := range []string{"FAST", " fast ", "custom-native-type"} {
				cfg := test.base
				cfg.Class = "standard"
				cfg.ServerType = rawStoredType
				if got := test.candidates(cfg); len(got) == 0 || got[0] != rawStoredType {
					t.Fatalf("raw stored=%q candidates=%v want raw value first", rawStoredType, got)
				}
			}

			missing := test.missing
			missing.Class = "standard"
			missing.ServerType = storedType
			if got := test.candidates(missing); !reflect.DeepEqual(got, []string{storedType}) {
				t.Fatalf("canonical stored candidates=%v want [%s]", got, storedType)
			}
			missing.ServerType = ""
			if got := test.candidates(missing); len(got) != 0 {
				t.Fatalf("canonical missing candidates=%v want none", got)
			}
			missing.ServerType = explicitType
			missing.ServerTypeExplicit = true
			if got := test.candidates(missing); !reflect.DeepEqual(got, []string{explicitType}) {
				t.Fatalf("explicit candidates=%v want [%s]", got, explicitType)
			}
			missing.ServerType = "fast"
			if got := test.candidates(missing); !reflect.DeepEqual(got, []string{"fast"}) {
				t.Fatalf("explicit canonical-word candidates=%v want [fast]", got)
			}

			missing.ServerTypeExplicit = false
			missing.Class = "fast"
			missing.ServerType = "beast"
			if got := test.candidates(missing); len(got) != 0 {
				t.Fatalf("missing selector with canonical placeholder candidates=%v want none", got)
			}
			provider, err := ProviderFor(missing.Provider)
			if err != nil {
				t.Fatal(err)
			}
			var exitErr ExitError
			if err := validateProviderClassSelector(provider, missing); !AsExitError(err, &exitErr) || exitErr.Code != 2 {
				t.Fatalf("missing selector validation err=%v want exit 2", err)
			}

			rawStored := test.base
			rawStored.Class = "custom-shape"
			rawStored.ServerType = " stored-native-type "
			if got := test.candidates(rawStored); len(got) == 0 || got[0] != rawStored.ServerType {
				t.Fatalf("raw stored candidates=%v want %q first", got, rawStored.ServerType)
			}
		})
	}

	macConfig := Config{
		Provider: "aws", TargetOS: targetMacOS, Architecture: ArchitectureAMD64, architectureExplicit: true,
		Class: " custom-mac-type ", ServerType: storedType,
	}
	macWant := append([]string{storedType, macConfig.Class}, awsMacOSInstanceTypeCandidates()...)
	if got := awsLaunchCandidates(macConfig); !reflect.DeepEqual(got, macWant) {
		t.Fatalf("AWS custom macOS candidates=%v want %v", got, macWant)
	}
}

func TestServerTypeForConfigRecomputesNonExplicitStoredType(t *testing.T) {
	provider := selectorOverrideProvider{
		resolve: func(Config) string { return "provider-default" },
		override: func(cfg Config) (string, bool) {
			serverType := strings.TrimSpace(cfg.Namespace.Size)
			return serverType, serverType != ""
		},
	}
	providerRegistry[provider.Name()] = provider
	t.Cleanup(func() { delete(providerRegistry, provider.Name()) })

	cfg := Config{Provider: provider.Name(), TargetOS: targetWindows, Architecture: ArchitectureARM64, Class: "standard", ServerType: "stored-arm-type"}
	if got := serverTypeForConfig(cfg); got != "provider-default" {
		t.Fatalf("server type=%q want provider-default", got)
	}
	cfg.Namespace.Size = "provider-native-type"
	if got := serverTypeForConfig(cfg); got != "provider-native-type" {
		t.Fatalf("native override=%q want provider-native-type", got)
	}
	cfg.ServerTypeExplicit = true
	if got := serverTypeForConfig(cfg); got != "stored-arm-type" {
		t.Fatalf("explicit generic type=%q want stored-arm-type", got)
	}
	cfg.ServerType = cfg.Class
	if got := serverTypeForConfig(cfg); got != cfg.Class {
		t.Fatalf("explicit type matching class=%q want %q", got, cfg.Class)
	}
	cfg.ServerTypeExplicit = false
	cfg.Namespace.Size = ""
	if got := serverTypeForConfig(cfg); got != "provider-default" {
		t.Fatalf("canonical literal resolved=%q want provider-default", got)
	}
}

func TestHetznerExplicitTypeMatchingClassIsExact(t *testing.T) {
	matched := Config{Provider: "hetzner", TargetOS: targetLinux, Architecture: ArchitectureAMD64, architectureExplicit: true, Class: "fast", ServerType: "fast", ServerTypeExplicit: true}
	if got := hetznerServerTypeCandidatesForConfig(matched); !reflect.DeepEqual(got, []string{"fast"}) {
		t.Fatalf("matched candidates=%v want [fast]", got)
	}

	missing := matched
	missing.Architecture = ArchitectureARM64
	if got := hetznerServerTypeCandidatesForConfig(missing); !reflect.DeepEqual(got, []string{"fast"}) {
		t.Fatalf("missing-selector candidates=%v want [fast]", got)
	}
}

func TestProviderClassCatalogSortsProfilesWithoutSortingFallbacks(t *testing.T) {
	provider := selectorClassProfileProvider{}
	catalog := providerClassCatalogFor(provider)
	if catalog.Profiles == nil {
		t.Fatal("profiles is nil")
	}
	for _, profile := range catalog.Profiles {
		if profile.Primary.Type == "mixed-primary" && !reflect.DeepEqual(profileCandidateTypesForTest(profile), []string{"mixed-primary", "mixed-fallback"}) {
			t.Fatalf("fallback order changed: %#v", profile)
		}
	}
}

func TestProviderClassConstructorsKeepCanonicalDataImmutable(t *testing.T) {
	classes := CanonicalProviderClasses()
	classes[0] = "changed"
	if got := CanonicalProviderClasses()[0]; got != "standard" {
		t.Fatalf("canonical classes mutated: %v", CanonicalProviderClasses())
	}

	profile := ProviderClassProfileFromMachines(
		"standard", targetLinux, "", ProviderClassArchitectureAMD64,
		[]ProviderClassMachine{{Type: "primary"}},
	)
	if profile.Primary.Type != "primary" || profile.Fallbacks == nil || len(profile.Fallbacks) != 0 {
		t.Fatalf("profile=%#v", profile)
	}

	uniform := UniformLinuxAMD64ClassProfiles(ProviderClassMachine{Type: "uniform"})
	if len(uniform) != len(CanonicalProviderClasses()) {
		t.Fatalf("uniform profiles=%d", len(uniform))
	}
	for _, item := range uniform {
		if item.Primary.Type != "uniform" || item.Primary.Architecture != ProviderClassArchitectureAMD64 || item.Fallbacks == nil {
			t.Fatalf("uniform profile=%#v", item)
		}
	}
}

func profileCandidateTypesForTest(profile ProviderClassProfile) []string {
	values := []string{profile.Primary.Type}
	for _, fallback := range profile.Fallbacks {
		values = append(values, fallback.Type)
	}
	return values
}
