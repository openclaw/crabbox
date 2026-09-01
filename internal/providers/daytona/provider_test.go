package daytona

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSupportsCoordinator(t *testing.T) {
	spec := (Provider{}).Spec()
	if spec.Name != "daytona" || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorSupported {
		t.Fatalf("spec=%#v", spec)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureArchiveSync, core.FeatureRunScript, core.FeatureCheckpoint, core.FeatureFork, core.FeatureSnapshot} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}
	for _, unsupported := range []core.Feature{
		core.FeatureDesktop,
		core.FeatureBrowser,
		core.FeatureCode,
		core.FeatureTailscale,
		core.FeatureRestore,
	} {
		if spec.Features.Has(unsupported) {
			t.Fatalf("features=%v unexpectedly includes %s", spec.Features, unsupported)
		}
	}
}
