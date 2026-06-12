package xcpng

import (
	"context"
	"flag"
	"io"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpec(t *testing.T) {
	provider := Provider{}
	if provider.Name() != "xcp-ng" {
		t.Fatalf("Name=%q", provider.Name())
	}
	if aliases := provider.Aliases(); len(aliases) != 0 {
		t.Fatalf("Aliases=%v want none", aliases)
	}
	spec := provider.Spec()
	if spec.Name != "xcp-ng" || spec.Family != "xcp-ng" || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("unexpected spec: %#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("spec missing feature %s: %#v", feature, spec.Features)
		}
	}
}

func TestProviderForResolvesCanonicalOnly(t *testing.T) {
	provider, err := core.ProviderFor("xcp-ng")
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != "xcp-ng" {
		t.Fatalf("provider=%s", provider.Name())
	}
	if _, err := core.ProviderFor("xcpng"); err == nil {
		t.Fatal("xcpng alias must not resolve")
	}
}

func TestProviderServerTypeUsesTemplateIdentity(t *testing.T) {
	provider := Provider{}
	tests := []struct {
		name string
		cfg  core.Config
		want string
	}{
		{name: "default", cfg: core.Config{}, want: "template"},
		{name: "template name", cfg: core.Config{XCPNg: core.XCPNgConfig{Template: "Ubuntu Ready 22.04"}}, want: "template-ubuntu-ready-22-04"},
		{name: "template uuid wins", cfg: core.Config{XCPNg: core.XCPNgConfig{Template: "Ubuntu Ready", TemplateUUID: xcpNgTestVMUUID}}, want: "template-" + xcpNgTestVMUUID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := provider.ServerTypeForConfig(tt.cfg); got != tt.want {
				t.Fatalf("ServerTypeForConfig=%q want %q", got, tt.want)
			}
		})
	}
	if got := provider.ServerTypeForClass("linux-small"); got != "template" {
		t.Fatalf("ServerTypeForClass=%q want template", got)
	}
}

func TestFlagsApplyNonSecretConfigOnly(t *testing.T) {
	defaults := core.Config{}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	values := Provider{}.RegisterFlags(fs, defaults)
	if fs.Lookup("xcp-ng-password") != nil {
		t.Fatal("xcp-ng password must not be registered as an argv flag")
	}
	err := fs.Parse([]string{
		"--xcp-ng-api-url", "https://xcp-ng.example.test",
		"--xcp-ng-username", "root",
		"--xcp-ng-template", "Ubuntu Ready",
		"--xcp-ng-template-uuid", "tpl-0001",
		"--xcp-ng-sr", "default-sr",
		"--xcp-ng-sr-uuid", "sr-0001",
		"--xcp-ng-network", "pool-network",
		"--xcp-ng-network-uuid", "net-0001",
		"--xcp-ng-host", "host-0001",
		"--xcp-ng-user", "runner",
		"--xcp-ng-work-root", "/work/xcp-ng",
		"--xcp-ng-insecure-tls",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := core.Config{}
	if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.XCPNg.APIURL != "https://xcp-ng.example.test" || cfg.XCPNg.Username != "root" || cfg.XCPNg.Template != "" || cfg.XCPNg.TemplateUUID != "tpl-0001" || cfg.XCPNg.SR != "" || cfg.XCPNg.SRUUID != "sr-0001" || cfg.XCPNg.Network != "" || cfg.XCPNg.NetworkUUID != "net-0001" || cfg.XCPNg.Host != "host-0001" || cfg.XCPNg.User != "runner" || cfg.SSHUser != "runner" || cfg.WorkRoot != "/work/xcp-ng" || !cfg.XCPNg.InsecureTLS {
		t.Fatalf("flags not applied: %#v", cfg.XCPNg)
	}
	if cfg.XCPNg.Password != "" {
		t.Fatalf("password unexpectedly set from flags: %q", cfg.XCPNg.Password)
	}
	if cfg.ServerType != "template-tpl-0001" {
		t.Fatalf("server type=%q", cfg.ServerType)
	}
}

func TestFlagsClearStaleNameUUIDCounterparts(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want core.XCPNgConfig
	}{
		{
			name: "name flags clear stale uuids",
			args: []string{
				"--xcp-ng-template", "Ubuntu Ready",
				"--xcp-ng-sr", "default-sr",
				"--xcp-ng-network", "pool-network",
			},
			want: core.XCPNgConfig{
				Template: "Ubuntu Ready",
				SR:       "default-sr",
				Network:  "pool-network",
			},
		},
		{
			name: "uuid flags clear stale names",
			args: []string{
				"--xcp-ng-template-uuid", xcpNgTestVMUUID,
				"--xcp-ng-sr-uuid", "sr-0002",
				"--xcp-ng-network-uuid", "net-0002",
			},
			want: core.XCPNgConfig{
				TemplateUUID: xcpNgTestVMUUID,
				SRUUID:       "sr-0002",
				NetworkUUID:  "net-0002",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := core.Config{XCPNg: core.XCPNgConfig{
				Template:     "Ubuntu Ready",
				TemplateUUID: "11111111-1111-1111-1111-111111111111",
				SR:           "default-sr",
				SRUUID:       "sr-0001",
				Network:      "pool-network",
				NetworkUUID:  "net-0001",
			}}
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			values := Provider{}.RegisterFlags(fs, cfg)
			if err := fs.Parse(tt.args); err != nil {
				t.Fatal(err)
			}
			if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if cfg.XCPNg.Template != tt.want.Template || cfg.XCPNg.TemplateUUID != tt.want.TemplateUUID || cfg.XCPNg.SR != tt.want.SR || cfg.XCPNg.SRUUID != tt.want.SRUUID || cfg.XCPNg.Network != tt.want.Network || cfg.XCPNg.NetworkUUID != tt.want.NetworkUUID {
				t.Fatalf("placement flags did not clear stale counterpart: %#v", cfg.XCPNg)
			}
			if got := xcpNgServerTypeForConfig(cfg); got != cfg.ServerType {
				t.Fatalf("server type=%q want %q", cfg.ServerType, got)
			}
		})
	}
}

func TestConfigureDoctorReturnsNonMutatingBackend(t *testing.T) {
	cfg := core.Config{}
	cfg.XCPNg.APIURL = "https://xcp-ng.example.test"
	cfg.XCPNg.Username = "root"
	cfg.XCPNg.Password = "secret"
	cfg.XCPNg.Template = "ubuntu-template"
	cfg.XCPNg.SRUUID = "sr-uuid"
	fake := &fakeLifecycleClient{}
	old := newLifecycleClient
	newLifecycleClient = func(context.Context, core.Config) (lifecycleClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newLifecycleClient = old })
	doctor, err := Provider{}.ConfigureDoctor(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	result, err := doctor.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != "xcp-ng" || !strings.Contains(result.Message, "auth=ready") || !strings.Contains(result.Message, "mutation=false") {
		t.Fatalf("result=%#v", result)
	}
	if strings.Contains(result.Message, cfg.XCPNg.Password) {
		t.Fatal("doctor result leaked password")
	}
	if _, ok := doctor.(core.SSHLeaseBackend); !ok {
		t.Fatalf("doctor backend=%T, want SSHLeaseBackend scaffolding", doctor)
	}
}

func TestDoctorReportsIncompleteConfigWithoutSecretValues(t *testing.T) {
	cfg := core.Config{}
	cfg.XCPNg.Password = "secret"
	doctor, err := Provider{}.ConfigureDoctor(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	result, err := doctor.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Message, "auth=configuration-incomplete") {
		t.Fatalf("result=%#v", result)
	}
	if strings.Contains(result.Message, cfg.XCPNg.Password) {
		t.Fatal("doctor result leaked password")
	}
}
