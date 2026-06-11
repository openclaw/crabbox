package digitalocean

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName || spec.Family != providerName || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureTailscale} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}
}

func TestProviderServerTypeDefaults(t *testing.T) {
	if got := (Provider{}).ServerTypeForClass("standard"); got != "s-1vcpu-1gb" {
		t.Fatalf("ServerTypeForClass standard=%q", got)
	}
	if got := (Provider{}).ServerTypeForConfig(core.Config{ServerType: "s-2vcpu-2gb", ServerTypeExplicit: true}); got != "s-2vcpu-2gb" {
		t.Fatalf("explicit ServerTypeForConfig=%q", got)
	}
	if got := (Provider{}).ServerTypeForConfig(core.Config{ServerType: "cpx51"}); got != "s-1vcpu-1gb" {
		t.Fatalf("implicit cross-provider ServerTypeForConfig=%q", got)
	}
}
