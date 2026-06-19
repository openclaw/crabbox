package scaleway

import (
	"flag"
	"io"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecAndServerType(t *testing.T) {
	p := Provider{}
	if p.Name() != providerName || p.Aliases() != nil {
		t.Fatalf("provider name/aliases=%q/%v", p.Name(), p.Aliases())
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindSSHLease || spec.Family != providerName || spec.Coordinator != core.CoordinatorNever {
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

	cfg := core.Config{Scaleway: core.ScalewayConfig{Type: "DEV1-M"}}
	if got := p.ServerTypeForConfig(cfg); got != "DEV1-M" {
		t.Fatalf("server type=%q", got)
	}
	cfg.ServerType = "custom-type"
	cfg.ServerTypeExplicit = true
	if got := p.ServerTypeForConfig(cfg); got != "custom-type" {
		t.Fatalf("explicit server type=%q", got)
	}
}

func TestProviderApplyFlags(t *testing.T) {
	p := Provider{}
	cfg := core.Config{}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	values := p.RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--scaleway-region", "nl-ams",
		"--scaleway-zone", "nl-ams-1",
		"--scaleway-image", "ubuntu_jammy",
		"--scaleway-type", "DEV1-M",
		"--scaleway-project-id", "project-1",
		"--scaleway-organization-id", "org-1",
		"--scaleway-security-group", "sg-1",
		"--scaleway-ssh-cidrs", "203.0.113.0/24, 2001:db8::/64",
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Scaleway.Region != "nl-ams" || cfg.Scaleway.Zone != "nl-ams-1" || cfg.Scaleway.Image != "ubuntu_jammy" || cfg.Scaleway.Type != "DEV1-M" || cfg.Scaleway.ProjectID != "project-1" || cfg.Scaleway.OrganizationID != "org-1" || cfg.Scaleway.SecurityGroup != "sg-1" {
		t.Fatalf("flags not applied: %#v", cfg.Scaleway)
	}
	if !core.ScalewayRegionWasExplicit(cfg) || !core.ScalewayZoneWasExplicit(cfg) || !core.ScalewayImageWasExplicit(cfg) || !core.ScalewayTypeWasExplicit(cfg) {
		t.Fatal("scaleway location/image/type flags should mark explicit provider values")
	}
	if strings.Join(cfg.Scaleway.SSHCIDRs, ",") != "203.0.113.0/24,2001:db8::/64" {
		t.Fatalf("ssh cidrs=%v", cfg.Scaleway.SSHCIDRs)
	}
}

func TestValidateFoundationConfigDefersUnsupportedPortableOS(t *testing.T) {
	cfg := core.Config{OSImage: "ubuntu:26.04"}
	core.SetOSImageExplicit(&cfg)
	if err := (Provider{}).ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "provider=scaleway does not support os") {
		t.Fatalf("ValidateConfig err=%v", err)
	}
	cfg.Scaleway.Image = "custom-image"
	if err := (Provider{}).ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig with explicit image: %v", err)
	}
}
