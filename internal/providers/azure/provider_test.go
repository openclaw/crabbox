package azure

import (
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
