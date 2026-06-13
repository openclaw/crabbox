package lambda

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecAndDefaults(t *testing.T) {
	spec := (Provider{}).Spec()
	if spec.Name != providerName || spec.Family != providerName || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureTailscale} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	if got := (Provider{}).ServerTypeForConfig(cfg); got != defaultType {
		t.Fatalf("ServerTypeForConfig=%q want %q", got, defaultType)
	}
}

func TestValidateConfigRejectsAmbiguousImage(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Lambda.Image = "img-123"
	cfg.Lambda.ImageFamily = "lambda-stack-24-04"
	if err := (Provider{}).ValidateConfig(cfg); err == nil {
		t.Fatal("ValidateConfig succeeded for image plus imageFamily")
	}
}
