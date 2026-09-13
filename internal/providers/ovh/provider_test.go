package ovh

import (
	"flag"
	"fmt"
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
	if got := provider.ServerTypeForConfig(core.Config{Class: "standard"}); got != "b3-8" {
		t.Fatalf("ServerTypeForConfig standard=%q", got)
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

func TestOVHBindingFlagsContract(t *testing.T) {
	for _, selected := range []string{"ovh", "aws"} {
		for _, image := range []string{"", "Ubuntu 24.04", "  "} {
			cfg := core.BaseConfig()
			cfg.Provider = selected
			provider := Provider{}
			fs := flag.NewFlagSet("contract", flag.ContinueOnError)
			values := provider.RegisterFlags(fs, cfg)
			count := 0
			fs.VisitAll(func(*flag.Flag) { count++ })
			if count != 5 {
				t.Fatal("flag count changed")
			}
			for _, tc := range []struct{ name, defaultValue, help string }{{"ovh-endpoint", "https://api.us.ovhcloud.com/1.0", "OVHcloud API endpoint"}, {"ovh-project-id", "", "OVHcloud Public Cloud project ID"}, {"ovh-region", "", "OVHcloud Public Cloud region"}, {"ovh-image", "Ubuntu 24.04", "OVHcloud Public Cloud image name or ID"}, {"ovh-flavor", "b3-8", "OVHcloud Public Cloud flavor name or ID"}} {
				f := fs.Lookup(tc.name)
				if f == nil || f.DefValue != tc.defaultValue || f.Usage != tc.help {
					t.Fatalf("flag %s changed: %#v", tc.name, f)
				}
			}
			cfg.OVH = core.OVHConfig{Endpoint: "layered-endpoint", ProjectID: "layered-project", Region: "layered-region", Image: "layered-image", Flavor: "layered-flavor"}
			want := cfg.OVH
			if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if cfg.OVH != want || core.OVHImageWasExplicit(cfg) {
				t.Fatal("unvisited overwrote config or marked image")
			}
			fs.String("class", "", "")
			fs.String("type", "", "")
			if err := fs.Parse([]string{"--class=standard", "--type=b3-16", "--ovh-endpoint=", "--ovh-project-id=project-flag", "--ovh-region=region-flag", "--ovh-image=" + image, "--ovh-flavor=flavor-flag"}); err != nil {
				t.Fatal(err)
			}
			before := fmt.Sprintf("%#v", cfg)
			if err := provider.ApplyFlags(&cfg, fs, struct{}{}); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%#v", cfg) != before {
				t.Fatal("wrong type changed config")
			}
			if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			want = core.OVHConfig{ProjectID: "project-flag", Region: "region-flag", Image: image, Flavor: "flavor-flag"}
			if cfg.OVH != want || !core.OVHImageWasExplicit(cfg) {
				t.Fatalf("flags selected=%s got=%#v marker=%t", selected, cfg.OVH, core.OVHImageWasExplicit(cfg))
			}
		}
	}
	cfg := core.BaseConfig()
	fs := flag.NewFlagSet("contract", flag.ContinueOnError)
	p := Provider{}
	values := p.RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{"--ovh-region=other-region"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if core.OVHImageWasExplicit(cfg) {
		t.Fatal("non-image visit marked image")
	}
}

func TestOVHBindingEndpointAndClassContract(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{{"", "https://api.us.ovhcloud.com/1.0"}, {"  ", "https://api.us.ovhcloud.com/1.0"}, {" OVH-US ", "https://api.us.ovhcloud.com/1.0"}, {"ovh-ca", "https://ca.api.ovh.com/1.0"}, {"ovh-eu", "https://eu.api.ovh.com/1.0"}, {" https://fixture.ovhcloud.com/1.0 ", "https://fixture.ovhcloud.com/1.0"}} {
		if got := normalizeEndpointAlias(tc.raw); got != tc.want {
			t.Fatalf("alias=%q got=%q want=%q", tc.raw, got, tc.want)
		}
	}
	p := Provider{}
	if p.ServerTypeForConfig(core.Config{Class: "standard"}) != "b3-8" || p.ServerTypeForConfig(core.Config{Class: "unknown-class"}) != "b3-8" {
		t.Fatal("fixed class policy changed")
	}
	for _, tc := range []struct {
		flavor, explicitType, want, override string
		present                              bool
	}{{"configured", "", "configured", "configured", true}, {"configured", "generic-type", "generic-type", "configured", true}, {"  ", "", "  ", "", false}, {" padded ", "", " padded ", "padded", true}} {
		cfg := core.Config{OVH: core.OVHConfig{Flavor: tc.flavor}, ServerType: tc.explicitType, ServerTypeExplicit: tc.explicitType != ""}
		if got := p.ServerTypeForConfig(cfg); got != tc.want {
			t.Fatalf("server type=%q want=%q", got, tc.want)
		}
		got, ok := p.ServerTypeOverrideForConfig(cfg)
		if got != tc.override || ok != tc.present {
			t.Fatalf("override=%q/%t", got, ok)
		}
	}
}
