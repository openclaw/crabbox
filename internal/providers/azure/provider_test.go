package azure

import (
	"flag"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestIsCrabboxAzureLeaseRequiresProviderTag(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{name: "nil labels", labels: nil, want: false},
		{name: "no crabbox tag", labels: map[string]string{"managed_by": "crabbox"}, want: false},
		{name: "different provider", labels: map[string]string{"crabbox": "true", "provider": "aws"}, want: false},
		{name: "tagged azure", labels: map[string]string{"crabbox": "true", "provider": "azure"}, want: true},
		{name: "tagged no provider", labels: map[string]string{"crabbox": "true"}, want: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := core.Server{Labels: tc.labels}
			if got := isCrabboxAzureLease(s); got != tc.want {
				t.Fatalf("labels=%+v got %v want %v", tc.labels, got, tc.want)
			}
		})
	}
}

func TestProviderAppliesAzureOSDiskFlag(t *testing.T) {
	t.Parallel()
	provider := Provider{}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := provider.RegisterFlags(fs, core.Config{})
	if err := fs.Parse([]string{"--azure-os-disk", "managed"}); err != nil {
		t.Fatal(err)
	}
	cfg := core.Config{Provider: "azure"}
	if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.AzureOSDisk != core.AzureOSDiskManaged {
		t.Fatalf("AzureOSDisk=%q want %q", cfg.AzureOSDisk, core.AzureOSDiskManaged)
	}
	if !cfg.AzureOSDiskExplicit {
		t.Fatal("AzureOSDiskExplicit=false, want true")
	}
}

func TestProviderValidatesConfiguredAzureOSDisk(t *testing.T) {
	t.Parallel()
	provider := Provider{}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := provider.RegisterFlags(fs, core.Config{})
	cfg := core.Config{Provider: "azure", AzureOSDisk: "premium"}
	if err := provider.ApplyFlags(&cfg, fs, values); err == nil {
		t.Fatal("expected invalid configured Azure OS disk mode to fail")
	}
}

func TestProviderSupportsDirectWindowsOSDiskCheckpoints(t *testing.T) {
	t.Parallel()
	capability, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{
		Config: core.Config{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
		Server: core.Server{CloudID: "crabbox-source"},
		Target: core.SSHTarget{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
	})
	if !ok {
		t.Fatal("expected Windows Azure checkpoint capability")
	}
	if capability.Kind != core.CheckpointKindAzureOS || !capability.Direct {
		t.Fatalf("capability=%+v, want direct Azure OS disk snapshot", capability)
	}
}

func TestProviderRejectsWindowsImageCheckpoints(t *testing.T) {
	t.Parallel()
	_, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{
		Config:   core.Config{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
		Server:   core.Server{CloudID: "crabbox-source"},
		Target:   core.SSHTarget{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
		Strategy: core.CheckpointStrategyImage,
	})
	if ok {
		t.Fatal("Azure Windows leases must not advertise managed image checkpoints")
	}
}

func TestProviderRejectsDirectWSL2OSDiskCheckpoints(t *testing.T) {
	t.Parallel()
	_, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{
		Config: core.Config{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeWSL2},
		Server: core.Server{CloudID: "crabbox-source"},
		Target: core.SSHTarget{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeWSL2},
	})
	if ok {
		t.Fatal("WSL2 Azure leases must not advertise native Windows snapshot forks")
	}
}

func TestProviderAppliesWindowsSnapshotForkAzureScope(t *testing.T) {
	t.Parallel()
	cfg := core.Config{}
	err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{
		Config: &cfg,
		Record: core.NativeCheckpointForkRecord{
			Kind:     core.CheckpointKindAzureOS,
			Resource: "/subscriptions/sub/resourceGroups/snapshot-rg/providers/Microsoft.Compute/snapshots/checkpoint",
			Region:   "westus2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AzureSnapshot == "" || cfg.AzureLocation != "westus2" || cfg.AzureResourceGroup != "snapshot-rg" || cfg.AzureSubscription != "sub" {
		t.Fatalf("fork config=%+v", cfg)
	}
}

func TestProviderRegistered(t *testing.T) {
	provider, err := core.ProviderFor("azure")
	if err != nil {
		t.Fatalf("expected azure provider to be registered: %v", err)
	}
	if got := provider.Name(); got != "azure" {
		t.Fatalf("provider name = %q, want %q", got, "azure")
	}
}

func TestProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != "azure" {
		t.Fatalf("spec.Name = %q, want azure", spec.Name)
	}
	if spec.Kind != core.ProviderKindSSHLease {
		t.Fatalf("spec.Kind = %q, want %q", spec.Kind, core.ProviderKindSSHLease)
	}
	if spec.Coordinator != core.CoordinatorSupported {
		t.Fatalf("spec.Coordinator = %q, want %q", spec.Coordinator, core.CoordinatorSupported)
	}
	wantTargets := []core.TargetSpec{
		{OS: core.TargetLinux},
		{OS: core.TargetWindows, WindowsMode: "normal"},
		{OS: core.TargetWindows, WindowsMode: "wsl2"},
	}
	if len(spec.Targets) != len(wantTargets) {
		t.Fatalf("spec.Targets = %+v, want %+v", spec.Targets, wantTargets)
	}
	for i, want := range wantTargets {
		if spec.Targets[i] != want {
			t.Fatalf("spec.Targets[%d] = %+v, want %+v", i, spec.Targets[i], want)
		}
	}
	wantFeatures := []core.Feature{
		core.FeatureSSH,
		core.FeatureCrabboxSync,
		core.FeatureCleanup,
		core.FeatureDesktop,
		core.FeatureBrowser,
		core.FeatureCode,
		core.FeatureTailscale,
	}
	if len(spec.Features) != len(wantFeatures) {
		t.Fatalf("spec.Features = %+v, want %+v", spec.Features, wantFeatures)
	}
	for i, f := range wantFeatures {
		if spec.Features[i] != f {
			t.Fatalf("spec.Features[%d] = %q, want %q", i, spec.Features[i], f)
		}
	}
}
