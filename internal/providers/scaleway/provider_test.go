package scaleway

import (
	"flag"
	"fmt"
	"io"
	"reflect"
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

func TestScalewayBindingFlagContract(t *testing.T) {
	p := Provider{}
	cfg := core.BaseConfig()
	cfg.Scaleway.SSHCIDRs = []string{"203.0.113.0/24"}
	fs := flag.NewFlagSet("contract", flag.ContinueOnError)
	values := p.RegisterFlags(fs, cfg)
	count := 0
	fs.VisitAll(func(*flag.Flag) { count++ })
	if count != 8 {
		t.Fatal("flag count changed")
	}
	listFlag := fs.Lookup("scaleway-ssh-cidrs")
	if listFlag.DefValue != "" || listFlag.Value.String() != "" || listFlag.Value.(flag.Getter).Get() != "" {
		t.Fatal("list registration inherited configured defaults")
	}
	cfg.Scaleway.Region = "layered-region"
	cfg.Scaleway.Zone = "layered-zone"
	cfg.Scaleway.Image = "layered-image"
	cfg.Scaleway.Type = "layered-type"
	expected := cfg.Scaleway
	if err := p.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Scaleway, expected) || core.ScalewayRegionWasExplicit(cfg) || core.ScalewayZoneWasExplicit(cfg) || core.ScalewayImageWasExplicit(cfg) || core.ScalewayTypeWasExplicit(cfg) {
		t.Fatal("unvisited config or markers changed")
	}
	args := []string{"--scaleway-region=nl-ams", "--scaleway-zone=nl-ams-1", "--scaleway-image=ubuntu_jammy", "--scaleway-type=DEV1-M", "--scaleway-project-id=project-fixture", "--scaleway-organization-id=org-fixture", "--scaleway-security-group=group-fixture", "--scaleway-ssh-cidrs=198.51.100.0/24, 2001:db8::/64,198.51.100.0/24"}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	before := fmt.Sprintf("%#v", cfg)
	if err := p.ApplyFlags(&cfg, fs, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%#v", cfg) != before {
		t.Fatal("wrong values type changed config")
	}
	if err := p.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	expected = core.ScalewayConfig{Region: "nl-ams", Zone: "nl-ams-1", Image: "ubuntu_jammy", Type: "DEV1-M", ProjectID: "project-fixture", OrganizationID: "org-fixture", SecurityGroup: "group-fixture", SSHCIDRs: []string{"198.51.100.0/24", "2001:db8::/64", "198.51.100.0/24"}}
	if !reflect.DeepEqual(cfg.Scaleway, expected) || !core.ScalewayRegionWasExplicit(cfg) || !core.ScalewayZoneWasExplicit(cfg) || !core.ScalewayImageWasExplicit(cfg) || !core.ScalewayTypeWasExplicit(cfg) {
		t.Fatal("complete flag values/markers changed")
	}
	for _, name := range []string{"region", "zone", "image", "type"} {
		for _, mode := range []string{"equal", "empty", "whitespace"} {
			cfg := core.BaseConfig()
			fs := flag.NewFlagSet("contract", flag.ContinueOnError)
			values := p.RegisterFlags(fs, cfg)
			value := fs.Lookup("scaleway-" + name).DefValue
			if mode == "empty" {
				value = ""
			}
			if mode == "whitespace" {
				value = "  "
			}
			if err := fs.Parse([]string{"--scaleway-" + name + "=" + value}); err != nil {
				t.Fatal(err)
			}
			if err := p.ApplyFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			for field, marked := range map[string]bool{"region": core.ScalewayRegionWasExplicit(cfg), "zone": core.ScalewayZoneWasExplicit(cfg), "image": core.ScalewayImageWasExplicit(cfg), "type": core.ScalewayTypeWasExplicit(cfg)} {
				if marked != (field == name) {
					t.Fatalf("flag=%s mode=%s marker=%s", name, mode, field)
				}
			}
		}
	}
	for _, tc := range []struct {
		args []string
		want []string
	}{{[]string{"--scaleway-ssh-cidrs="}, nil}, {[]string{"--scaleway-ssh-cidrs= , , "}, nil}, {[]string{"--scaleway-ssh-cidrs=none"}, []string{"none"}}, {[]string{"--scaleway-ssh-cidrs=203.0.113.0/24", "--scaleway-ssh-cidrs= 198.51.100.0/24, ,198.51.100.0/24 "}, []string{"198.51.100.0/24", "198.51.100.0/24"}}} {
		cfg := core.BaseConfig()
		cfg.Scaleway.SSHCIDRs = []string{"prior"}
		fs := flag.NewFlagSet("contract", flag.ContinueOnError)
		values := p.RegisterFlags(fs, cfg)
		if err := fs.Parse(tc.args); err != nil {
			t.Fatal(err)
		}
		if err := p.ApplyFlags(&cfg, fs, values); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(cfg.Scaleway.SSHCIDRs, tc.want) {
			t.Fatalf("flag list=%#v want=%#v", cfg.Scaleway.SSHCIDRs, tc.want)
		}
	}
}
