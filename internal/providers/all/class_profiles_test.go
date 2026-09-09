package all

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestProductionProviderClassCatalogCompleteness(t *testing.T) {
	mappedProviders := map[string]struct{}{
		"aws": {}, "azure": {}, "cloudflare": {}, "daytona": {}, "digitalocean": {}, "gcp": {}, "hetzner": {}, "linode": {},
		"machine0": {}, "namespace-devbox": {}, "namespace-instance": {}, "ovh": {}, "phala": {}, "scaleway": {}, "tencentcloud": {}, "vultr": {},
	}
	counts := map[core.ProviderClassDisposition]int{}
	compatibilityProviders := map[string]struct{}{
		"aws": {}, "azure": {}, "gcp": {}, "hetzner": {}, "namespace-instance": {},
	}
	for _, name := range core.RegisteredProviderNames() {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", name, err)
		}
		spec := provider.Spec()
		catalog := core.ProviderClassCatalogFor(provider)
		counts[spec.ClassDisposition]++
		source, hasProfiles := provider.(core.ProviderClassProfileProvider)
		_, hasCompatibilitySummary := provider.(core.ProviderClassSpecProvider)
		_, wantsCompatibilitySummary := compatibilityProviders[name]
		if hasCompatibilitySummary != wantsCompatibilitySummary {
			t.Errorf("provider=%s compatibility summary=%t want %t", name, hasCompatibilitySummary, wantsCompatibilitySummary)
		}
		_, shouldBeMapped := mappedProviders[name]
		if shouldBeMapped {
			if spec.ClassDisposition != core.ProviderClassDispositionMapped {
				t.Errorf("provider=%s disposition=%q want mapped", name, spec.ClassDisposition)
			}
			if !hasProfiles || len(source.ClassProfiles()) == 0 {
				t.Errorf("provider=%s disposition=mapped requires profiles", name)
			}
			if _, ok := provider.(core.ProviderServerTypeProvider); !ok {
				t.Errorf("provider=%s disposition=mapped requires provider-owned type resolution", name)
			}
			validateProviderClassProfiles(t, provider)
		} else {
			if spec.ClassDisposition != core.ProviderClassDispositionUnmapped {
				t.Errorf("provider=%s disposition=%q want unmapped", name, spec.ClassDisposition)
			}
			if hasProfiles && len(source.ClassProfiles()) > 0 {
				t.Errorf("provider=%s disposition=unmapped must not expose profiles", name)
			}
			if len(catalog.Profiles) != 0 {
				t.Errorf("provider=%s disposition=unmapped catalog profiles=%#v", name, catalog.Profiles)
			}
		}
		if catalog.Profiles == nil {
			t.Errorf("provider=%s catalog profiles is nil", name)
		}
		for _, alias := range provider.Aliases() {
			resolved, err := core.ProviderFor(alias)
			if err != nil {
				t.Errorf("provider=%s alias=%s: %v", name, alias, err)
				continue
			}
			if aliasCatalog := core.ProviderClassCatalogFor(resolved); !reflect.DeepEqual(aliasCatalog, catalog) {
				t.Errorf("provider=%s alias=%s catalog differs: canonical=%#v alias=%#v", name, alias, catalog, aliasCatalog)
			}
		}
	}
	if counts[core.ProviderClassDispositionMapped] != 16 || counts[core.ProviderClassDispositionUnmapped] != 65 || len(counts) != 2 {
		t.Fatalf("class disposition counts=%v want mapped=16 unmapped=65", counts)
	}
}

func TestAWSAndAzureClassProfileVariantCoverage(t *testing.T) {
	want := map[string][]string{
		"aws": {
			"linux//amd64", "linux//arm64", "macos//mixed", "windows/normal/amd64", "windows/wsl2/amd64",
		},
		"azure": {
			"linux//amd64", "linux//arm64", "windows/normal/amd64", "windows/normal/arm64", "windows/wsl2/amd64",
		},
	}
	for name, wantKeys := range want {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatal(err)
		}
		source := provider.(core.ProviderClassProfileProvider)
		keys := map[string]struct{}{}
		for _, profile := range source.ClassProfiles() {
			keys[variantKey(profile)] = struct{}{}
		}
		got := make([]string, 0, len(keys))
		for key := range keys {
			got = append(got, key)
		}
		sort.Strings(got)
		sort.Strings(wantKeys)
		if !reflect.DeepEqual(got, wantKeys) {
			t.Errorf("provider=%s variant keys=%v want %v", name, got, wantKeys)
		}
	}
}

func TestTinyAndSmallProfilePrimariesForMappedProviders(t *testing.T) {
	want := map[string]map[string]string{
		"aws":                {"tiny": "m7a.large", "small": "c7a.2xlarge"},
		"azure":              {"tiny": "Standard_D2ads_v6", "small": "Standard_D8ads_v6"},
		"cloudflare":         {"tiny": "standard-4", "small": "standard-4"},
		"daytona":            {"tiny": "daytona-small", "small": "daytona-small"},
		"digitalocean":       {"tiny": "s-1vcpu-1gb", "small": "s-1vcpu-1gb"},
		"gcp":                {"tiny": "c4-standard-4", "small": "c4-standard-8"},
		"hetzner":            {"tiny": "ccx13", "small": "ccx23"},
		"linode":             {"tiny": "g6-standard-1", "small": "g6-standard-1"},
		"machine0":           {"tiny": "large", "small": "xl"},
		"namespace-devbox":   {"tiny": "S", "small": "S"},
		"namespace-instance": {"tiny": "1x2", "small": "2x4"},
		"ovh":                {"tiny": "b3-8", "small": "b3-8"},
		"phala":              {"tiny": "tdx.small", "small": "tdx.small"},
		"scaleway":           {"tiny": "DEV1-S", "small": "DEV1-S"},
		"tencentcloud":       {"tiny": "SA5.MEDIUM2", "small": "SA5.MEDIUM2"},
		"vultr":              {"tiny": "vc2-1c-1gb", "small": "vc2-1c-1gb"},
	}
	for providerName, classes := range want {
		provider, err := core.ProviderFor(providerName)
		if err != nil {
			t.Fatal(err)
		}
		profiles := provider.(core.ProviderClassProfileProvider).ClassProfiles()
		for class, wantType := range classes {
			found := false
			for _, profile := range profiles {
				if profile.Class == class && profile.Target == core.TargetLinux && profile.Architecture == core.ProviderClassArchitectureAMD64 {
					found = true
					if profile.Primary.Type != wantType {
						t.Errorf("provider=%s class=%s primary=%q want %q", providerName, class, profile.Primary.Type, wantType)
					}
				}
			}
			if !found {
				t.Errorf("provider=%s class=%s missing linux/amd64 profile", providerName, class)
			}
		}
	}
}

func TestClassProfileCandidatesMatchRuntimeLoops(t *testing.T) {
	for _, name := range []string{"aws", "azure", "gcp", "hetzner"} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, profile := range provider.(core.ProviderClassProfileProvider).ClassProfiles() {
			cfg := core.BaseConfig()
			cfg.Provider = name
			cfg.TargetOS = profile.Target
			cfg.WindowsMode = profile.WindowsMode
			cfg.Architecture = string(profile.Architecture)
			if profile.Architecture == core.ProviderClassArchitectureMixed {
				cfg.Architecture = core.ArchitectureAMD64
			}
			cfg.Class = profile.Class
			cfg.ServerType = ""
			cfg.AzureOSDisk = core.AzureOSDiskManaged
			want := profileCandidateTypes(profile)
			var got []string
			switch name {
			case "aws":
				got = core.AWSLaunchCandidates(cfg)
			case "azure":
				got = core.AzureVMSizeCandidatesForConfig(cfg)
			case "gcp":
				got = core.GCPMachineTypeCandidatesForConfig(cfg)
			case "hetzner":
				got = core.HetznerServerTypeCandidatesForConfig(cfg)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("provider=%s selector=%s/%s/%s/%s runtime=%v profiles=%v", name, profile.Class, profile.Target, profile.WindowsMode, profile.Architecture, got, want)
			}
		}
	}
}

func TestMappedProviderProfilePrimaryMatchesProviderResolver(t *testing.T) {
	for _, name := range core.RegisteredProviderNames() {
		provider, err := core.ProviderFor(name)
		if err != nil || provider.Spec().ClassDisposition != core.ProviderClassDispositionMapped {
			continue
		}
		resolver, ok := provider.(core.ProviderServerTypeProvider)
		if !ok {
			continue
		}
		for _, profile := range provider.(core.ProviderClassProfileProvider).ClassProfiles() {
			cfg := core.BaseConfig()
			cfg.Provider = name
			cfg.TargetOS = profile.Target
			cfg.WindowsMode = profile.WindowsMode
			cfg.Architecture = string(profile.Architecture)
			if profile.Architecture == core.ProviderClassArchitectureMixed {
				cfg.Architecture = core.ArchitectureAMD64
			}
			cfg.Class = profile.Class
			cfg.ServerType = ""
			cfg.ServerTypeExplicit = false
			core.MarkClassExplicit(&cfg)
			if got := resolver.ServerTypeForConfig(cfg); got != profile.Primary.Type {
				t.Errorf("provider=%s selector=%s/%s/%s/%s resolver=%q profile=%q", name, profile.Class, profile.Target, profile.WindowsMode, profile.Architecture, got, profile.Primary.Type)
			}
		}
	}
}

func validateProviderClassProfiles(t *testing.T, provider core.Provider) {
	t.Helper()
	profiles := provider.(core.ProviderClassProfileProvider).ClassProfiles()
	selectors := map[string]struct{}{}
	classesByVariant := map[string]map[string]struct{}{}
	for _, profile := range profiles {
		selector := fmt.Sprintf("%s/%s/%s/%s", profile.Class, profile.Target, profile.WindowsMode, profile.Architecture)
		if _, exists := selectors[selector]; exists {
			t.Errorf("duplicate selector %s", selector)
		}
		selectors[selector] = struct{}{}
		if !core.IsCanonicalProviderClass(profile.Class) {
			t.Errorf("selector %s has noncanonical class", selector)
		}
		if !specSupportsTarget(provider.Spec(), profile.Target, profile.WindowsMode) {
			t.Errorf("selector %s is incompatible with provider targets %#v", selector, provider.Spec().Targets)
		}
		if profile.Target != core.TargetWindows && profile.WindowsMode != "" {
			t.Errorf("selector %s has windows mode on non-Windows target", selector)
		}
		if profile.Target == core.TargetWindows && profile.WindowsMode != core.WindowsModeNormal && profile.WindowsMode != core.WindowsModeWSL2 {
			t.Errorf("selector %s has invalid Windows mode", selector)
		}
		if !validProfileArchitecture(profile.Architecture) {
			t.Errorf("selector %s has invalid profile architecture", selector)
		}
		validateClassMachine(t, selector+" primary", profile.Primary, false)
		if profile.Fallbacks == nil {
			t.Errorf("selector %s has nil fallbacks", selector)
		}
		seenTypes := map[string]struct{}{profile.Primary.Type: {}}
		for index, fallback := range profile.Fallbacks {
			validateClassMachine(t, fmt.Sprintf("%s fallback[%d]", selector, index), fallback, false)
			if _, exists := seenTypes[fallback.Type]; exists {
				t.Errorf("selector %s has duplicate machine type %q", selector, fallback.Type)
			}
			seenTypes[fallback.Type] = struct{}{}
		}
		key := variantKey(profile)
		if classesByVariant[key] == nil {
			classesByVariant[key] = map[string]struct{}{}
		}
		classesByVariant[key][profile.Class] = struct{}{}
	}
	for key, classes := range classesByVariant {
		if len(classes) != len(core.CanonicalProviderClasses()) {
			t.Errorf("variant=%s has %d classes, want %d", key, len(classes), len(core.CanonicalProviderClasses()))
		}
		for _, class := range core.CanonicalProviderClasses() {
			if _, ok := classes[class]; !ok {
				t.Errorf("variant=%s missing class=%s", key, class)
			}
		}
	}
}

func validateClassMachine(t *testing.T, label string, machine core.ProviderClassMachine, allowMixed bool) {
	t.Helper()
	if strings.TrimSpace(machine.Type) == "" {
		t.Errorf("%s has blank type", label)
	}
	if machine.Architecture != core.ProviderClassArchitectureAMD64 && machine.Architecture != core.ProviderClassArchitectureARM64 && !(allowMixed && machine.Architecture == core.ProviderClassArchitectureMixed) {
		t.Errorf("%s has invalid concrete architecture %q", label, machine.Architecture)
	}
	if machine.VCPU != nil && *machine.VCPU <= 0 {
		t.Errorf("%s has invalid vcpu=%d", label, *machine.VCPU)
	}
	if machine.Memory != nil {
		if machine.Memory.Value <= 0 {
			t.Errorf("%s has invalid memory=%g", label, machine.Memory.Value)
		}
		switch machine.Memory.Unit {
		case core.ProviderMemoryUnitMB, core.ProviderMemoryUnitMiB, core.ProviderMemoryUnitGB, core.ProviderMemoryUnitGiB:
		default:
			t.Errorf("%s has invalid memory unit=%q", label, machine.Memory.Unit)
		}
	}
}

func variantKey(profile core.ProviderClassProfile) string {
	return fmt.Sprintf("%s/%s/%s", profile.Target, profile.WindowsMode, profile.Architecture)
}

func profileCandidateTypes(profile core.ProviderClassProfile) []string {
	types := []string{profile.Primary.Type}
	for _, fallback := range profile.Fallbacks {
		types = append(types, fallback.Type)
	}
	return types
}

func validProfileArchitecture(architecture core.ProviderClassArchitecture) bool {
	return architecture == core.ProviderClassArchitectureAMD64 || architecture == core.ProviderClassArchitectureARM64 || architecture == core.ProviderClassArchitectureMixed
}

func specSupportsTarget(spec core.ProviderSpec, target, windowsMode string) bool {
	for _, candidate := range spec.Targets {
		if candidate.OS != target {
			continue
		}
		if target != core.TargetWindows || candidate.WindowsMode == windowsMode {
			return true
		}
	}
	return false
}

// The selector and guidance table pins adapter-owned contracts independently
// of the shared formatter and of each provider's registry aliases.
var sizingFlagContracts = []struct {
	name, classGuidance, typeGuidance string
	normalized, assertionFirst        bool
	aliases                           []string
}{
	{"azure-dynamic-sessions", "choose pool sizing in Azure", "choose pool sizing in Azure", false, false, nil},
	{"blaxel", "use --blaxel-memory-mb", "use --blaxel-image", true, false, nil},
	{"cloudflare-dynamic-workers", "", "", false, false, []string{"cf-dynamic", "cfdw"}},
	{"cloudflare-sandbox", "", "", true, false, nil},
	{"cloud-run-sandbox", "sandboxes share Cloud Run service CPU/memory", "sandboxes share Cloud Run service CPU/memory", true, false, []string{"gcrun-sandbox", "google-cloud-run-sandbox", "cloudrun-sandbox"}},
	{"coder", "choose size through the Coder template or --coder-preset", "choose a Coder template with --coder-template", false, false, nil},
	{"codesandbox", "use --codesandbox-vm-tier", "use --codesandbox-vm-tier", true, true, []string{"csb", "code-sandbox"}},
	{"crownest", "use --crownest-template", "use --crownest-template", true, false, nil},
	{"cua", "use --cua-vcpus and --cua-memory-mb", "use --cua-image and --cua-kind", true, false, nil},
	{"cubesandbox", "", "", false, false, nil},
	{"docker-sandbox", "use --docker-sandbox-cpus or --docker-sandbox-memory", "use --docker-sandbox-template", false, false, nil},
	{"e2b", "", "", false, false, nil},
	{"exe-dev", "use --exe-dev-cpus, --exe-dev-memory, and --exe-dev-disk", "use --exe-dev-image", false, false, []string{"exe", "exedev"}},
	{"fastapi-cloud", "", "", true, false, []string{"fastapicloud", "fastapi"}},
	{"firecracker", "use --firecracker-cpus, --firecracker-memory-mib, and --firecracker-disk-mib", "use explicit Firecracker kernel, rootfs, and sizing flags", true, false, nil},
	{"modal", "", "", false, false, nil},
	{"morph", "", "use --morph-snapshot", true, false, nil},
	{"nvidia-brev", "use --nvidia-brev-gpu-name", "use --nvidia-brev-type", true, false, []string{"brev", "nvidia"}},
	{"opencomputer", "use --opencomputer-cpu and --opencomputer-memory-mb", "use --opencomputer-cpu and --opencomputer-memory-mb", true, false, []string{"oc", "open-computer"}},
	{"opensandbox", "use --opensandbox-cpu and --opensandbox-memory", "use --opensandbox-cpu and --opensandbox-memory", true, false, nil},
	{"orgo", "", "", true, false, []string{"orgo-ai"}},
	{"railway", "", "", true, false, []string{"rail", "railwayapp"}},
	{"runpod", "use --runpod-instance-id", "use --runpod-image", true, false, []string{"run-pod", "runpodio"}},
	{"smolvm", "use --smolvm-cpus/--smolvm-memory-mb", "use --smolvm-image", false, false, []string{"smol", "smolmachines", "smolfleet"}},
	{"superserve", "use --superserve-template or --superserve-snapshot", "use --superserve-template or --superserve-snapshot", true, false, nil},
	{"unikraft-cloud", "", "", true, false, []string{"unikraftcloud", "ukc"}},
	{"upstash-box", "use --upstash-box-size", "use --upstash-box-runtime", false, false, []string{"upstash", "box", "upstashbox"}},
	{"vast", "use --vast-gpu-name or --vast-gpu-count", "use --vast-image", true, false, []string{"vast-ai", "vastai"}},
	{"vercel-sandbox", "use --vercel-sandbox-vcpus", "use --vercel-sandbox-runtime or --vercel-sandbox-vcpus", true, false, nil},
	{"wandb", "", "", true, false, []string{"weights-and-biases"}},
	{"windows-sandbox", "Windows Sandbox sizing is controlled by the host", "Windows Sandbox sizing is controlled by the host", false, true, []string{"wsb", "windows-sandbox-provider"}},
}

func sizingContractFlags(t *testing.T, provider core.Provider, cfg core.Config, args []string) (*flag.FlagSet, any) {
	t.Helper()
	fs := flag.NewFlagSet("contract", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("class", "", "")
	fs.String("type", "", "")
	fs.String("target", "", "")
	fs.String("windows-mode", "", "")
	fs.String("expose", "", "")
	values := provider.RegisterFlags(fs, cfg)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return fs, values
}

func assertSizingContractError(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	var exitErr core.ExitError
	if err == nil || err.Error() != want || !errors.As(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v exit=%#v want exit2 %q", err, exitErr, want)
	}
}

func TestProviderSizingGuardContracts(t *testing.T) {
	testutil.IsolateUserDirs(t)
	if len(sizingFlagContracts) != 31 {
		t.Fatal("expected all 31 eligible adapters")
	}
	for _, tc := range sizingFlagContracts {
		t.Run(tc.name, func(t *testing.T) {
			provider, err := core.ProviderFor(tc.name)
			if err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"--class=large"}, {"--type=machine"}, {"--class=large", "--type=machine"}, {"--type=machine", "--class=large"}, {"--class="}, {"--type="}} {
				for _, wrong := range []bool{false, true} {
					cfg := core.BaseConfig()
					cfg.Provider = tc.name
					cfg.TargetOS = "linux"
					cfg.WindowsMode = "prior-mode"
					before := fmt.Sprintf("%#v", cfg)
					fs, values := sizingContractFlags(t, provider, cfg, args)
					// A visited provider value exposes any copy accidentally moved before rejection.
					copiedFlag := ""
					fs.VisitAll(func(f *flag.Flag) {
						if copiedFlag != "" || f.Name == "class" || f.Name == "type" || f.Name == "target" || f.Name == "windows-mode" || f.Name == "expose" {
							return
						}
						if getter, ok := f.Value.(flag.Getter); ok {
							if _, ok := getter.Get().(string); ok {
								copiedFlag = f.Name
							}
						}
					})
					if copiedFlag == "" {
						t.Fatal("provider has no string flag for copy-order fixture")
					}
					if err := fs.Set(copiedFlag, fs.Lookup(copiedFlag).Value.String()+"-fixture"); err != nil {
						t.Fatal(err)
					}
					if wrong {
						values = struct{}{}
					}
					flagName, guide := "class", tc.classGuidance
					if len(args) == 1 && strings.HasPrefix(args[0], "--type") {
						flagName = "type"
						guide = tc.typeGuidance
					}
					want := "--" + flagName + " is not supported for provider=" + tc.name
					if guide != "" {
						want += "; " + guide
					}
					if wrong && tc.assertionFirst {
						want = ""
					}
					assertSizingContractError(t, provider.ApplyFlags(&cfg, fs, values), want)
					if after := fmt.Sprintf("%#v", cfg); after != before {
						t.Fatalf("rejection/wrong-type mutated config for args=%v", args)
					}
				}
			}
		})
	}
}

func TestProviderSizingSelectorContracts(t *testing.T) {
	testutil.IsolateUserDirs(t)
	for _, tc := range sizingFlagContracts {
		t.Run(tc.name, func(t *testing.T) {
			provider, err := core.ProviderFor(tc.name)
			if err != nil {
				t.Fatal(err)
			}
			names := append([]string{tc.name, " " + strings.ToUpper(tc.name) + " ", "unselected"}, tc.aliases...)
			names = append(names, provider.Aliases()...)
			for _, selector := range names {
				selected := selector == tc.name
				for _, alias := range tc.aliases {
					selected = selected || selector == alias
				}
				if tc.normalized {
					normalized := strings.ToLower(strings.TrimSpace(selector))
					selected = normalized == tc.name
					for _, alias := range tc.aliases {
						selected = selected || normalized == alias
					}
				}
				cfg := core.BaseConfig()
				cfg.Provider = selector
				cfg.Class = "large"
				cfg.ServerType = "inherited-machine"
				fs, values := sizingContractFlags(t, provider, cfg, []string{"--class="})
				want := ""
				if selected {
					want = "--class is not supported for provider=" + tc.name
					if tc.classGuidance != "" {
						want += "; " + tc.classGuidance
					}
				} else if tc.name == "cloudflare-sandbox" {
					want = "cloudflare-sandbox requires cloudflareSandbox.url or CRABBOX_CLOUDFLARE_SANDBOX_URL"
				}
				assertSizingContractError(t, provider.ApplyFlags(&cfg, fs, values), want)
			}
			cfg := core.BaseConfig()
			cfg.Provider = tc.name
			cfg.Class = "large"
			cfg.ServerType = "inherited-machine"
			fs, values := sizingContractFlags(t, provider, cfg, nil)
			want := ""
			if tc.name == "cloudflare-sandbox" {
				want = "cloudflare-sandbox requires cloudflareSandbox.url or CRABBOX_CLOUDFLARE_SANDBOX_URL"
			}
			assertSizingContractError(t, provider.ApplyFlags(&cfg, fs, values), want)
		})
	}
}

func TestProviderSizingLocalOrderContracts(t *testing.T) {
	testutil.IsolateUserDirs(t)
	for _, name := range []string{"coder", "morph"} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"--class="}, {"--type="}, nil} {
			cfg := core.BaseConfig()
			cfg.Provider = name
			cfg.TargetOS = "windows"
			fs, values := sizingContractFlags(t, provider, cfg, args)
			want := "provider=" + name + " supports target=linux only"
			if len(args) > 0 {
				for _, tc := range sizingFlagContracts {
					if tc.name == name {
						flagName, guide := "class", tc.classGuidance
						if strings.HasPrefix(args[0], "--type") {
							flagName = "type"
							guide = tc.typeGuidance
						}
						want = "--" + flagName + " is not supported for provider=" + name
						if guide != "" {
							want += "; " + guide
						}
					}
				}
			}
			before := fmt.Sprintf("%#v", cfg)
			assertSizingContractError(t, provider.ApplyFlags(&cfg, fs, values), want)
			if fmt.Sprintf("%#v", cfg) != before {
				t.Fatal("target/sizing rejection mutated config")
			}
		}
	}
	provider, err := core.ProviderFor("cloudflare-dynamic-workers")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		flag string
	}{{[]string{"--expose=8080", "--type=machine", "--class=large"}, "class"}, {[]string{"--expose=8080", "--type="}, "type"}, {[]string{"--expose="}, "expose"}} {
		cfg := core.BaseConfig()
		cfg.Provider = provider.Name()
		fs, values := sizingContractFlags(t, provider, cfg, tc.args)
		before := fmt.Sprintf("%#v", cfg)
		assertSizingContractError(t, provider.ApplyFlags(&cfg, fs, values), "--"+tc.flag+" is not supported for provider=cloudflare-dynamic-workers")
		if fmt.Sprintf("%#v", cfg) != before {
			t.Fatal("dynamic rejection mutated config")
		}
	}
	provider, err = core.ProviderFor("windows-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	cfg := core.BaseConfig()
	cfg.Provider = provider.Name()
	cfg.TargetOS = "linux"
	cfg.WindowsMode = "prior-mode"
	fs, values := sizingContractFlags(t, provider, cfg, nil)
	assertSizingContractError(t, provider.ApplyFlags(&cfg, fs, values), "")
	if cfg.TargetOS != "windows" || cfg.WindowsMode != "normal" {
		t.Fatalf("Windows defaults=%s/%s", cfg.TargetOS, cfg.WindowsMode)
	}
}

func TestProviderSizingNonAdopterControls(t *testing.T) {
	testutil.IsolateUserDirs(t)
	for _, name := range []string{"semaphore", "tensorlake", "srt"} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatal(err)
		}
		cfg := core.BaseConfig()
		cfg.Provider = name
		fs, values := sizingContractFlags(t, provider, cfg, []string{"--class=large", "--type=machine"})
		assertSizingContractError(t, provider.ApplyFlags(&cfg, fs, values), "")
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{{"daytona", []string{"--class=large"}, ""}, {"daytona", []string{"--type="}, "--type is not supported for provider=daytona; choose CPU, memory, and disk in the Daytona snapshot"}, {"github-codespaces", []string{"--class="}, "--class is not supported for provider=github-codespaces; use --type or --github-codespaces-machine for a Codespaces machine slug"}, {"github-codespaces", []string{"--type= fixture-machine "}, ""}} {
		provider, err := core.ProviderFor(tc.name)
		if err != nil {
			t.Fatal(err)
		}
		cfg := core.BaseConfig()
		cfg.Provider = tc.name
		fs, values := sizingContractFlags(t, provider, cfg, tc.args)
		assertSizingContractError(t, provider.ApplyFlags(&cfg, fs, values), tc.want)
		if tc.name == "github-codespaces" && tc.want == "" && cfg.GitHubCodespaces.Machine != "fixture-machine" {
			t.Fatal("Codespaces type mapping changed")
		}
	}
	provider, err := core.ProviderFor("cloudflare")
	if err != nil {
		t.Fatal(err)
	}
	cfg := core.BaseConfig()
	cfg.Provider = "cloudflare"
	cfg.Class = "tiny"
	cfg.ServerType = ""
	fs, values := sizingContractFlags(t, provider, cfg, []string{"--class=tiny"})
	assertSizingContractError(t, provider.ApplyFlags(&cfg, fs, values), "")
	if cfg.ServerType != "standard-4" {
		t.Fatalf("Cloudflare mapped type=%q", cfg.ServerType)
	}
}
