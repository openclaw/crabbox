package ovh

import (
	"flag"
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

func TestProviderFlagsApplyNonSecretConfig(t *testing.T) {
	cfg := core.Config{}
	provider := Provider{}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := provider.RegisterFlags(fs, core.Config{OVH: core.OVHConfig{
		Endpoint:  "https://api.us.ovhcloud.com/1.0",
		ProjectID: "project-default",
		Region:    "BHS5",
		Image:     "Ubuntu 24.04",
		Flavor:    "b3-8",
	}})
	if err := fs.Parse([]string{
		"--ovh-endpoint", "https://ca.api.ovhcloud.com/1.0",
		"--ovh-project-id", "project-test",
		"--ovh-region", "GRA11",
		"--ovh-image", "image-test",
		"--ovh-flavor", "b3-16",
	}); err != nil {
		t.Fatal(err)
	}
	if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.OVH.Endpoint != "https://ca.api.ovhcloud.com/1.0" || cfg.OVH.ProjectID != "project-test" || cfg.OVH.Region != "GRA11" || cfg.OVH.Image != "image-test" || cfg.OVH.Flavor != "b3-16" {
		t.Fatalf("ovh flags not applied: %#v", cfg.OVH)
	}
	if !core.OVHImageWasExplicit(cfg) {
		t.Fatal("ovh image flag should mark ovh image explicit")
	}
}

func TestProviderServerTypeForConfig(t *testing.T) {
	provider := Provider{}
	if got := provider.ServerTypeForClass("standard"); got != "b3-8" {
		t.Fatalf("ServerTypeForClass standard=%q", got)
	}
	if got := provider.ServerTypeForConfig(core.Config{ServerType: "b3-16", ServerTypeExplicit: true, OVH: core.OVHConfig{Flavor: "b3-8"}}); got != "b3-16" {
		t.Fatalf("explicit ServerTypeForConfig=%q", got)
	}
	if got := provider.ServerTypeForConfig(core.Config{OVH: core.OVHConfig{Flavor: "b3-16"}}); got != "b3-16" {
		t.Fatalf("ovh flavor ServerTypeForConfig=%q", got)
	}
	if got := provider.ServerTypeForConfig(core.Config{Class: "beast"}); got != "b3-8" {
		t.Fatalf("class fallback ServerTypeForConfig=%q", got)
	}
}
