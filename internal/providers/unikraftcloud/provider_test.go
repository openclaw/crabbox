package unikraftcloud

import (
	"flag"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestUnikraftCloudProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName {
		t.Fatalf("spec.Name = %q, want %q", spec.Name, providerName)
	}
	if spec.Family != "unikraft-cloud" {
		t.Fatalf("spec.Family = %q, want unikraft-cloud", spec.Family)
	}
	if spec.Kind != core.ProviderKindServiceControl {
		t.Fatalf("spec.Kind = %q, want service-control", spec.Kind)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec.Coordinator = %q, want never", spec.Coordinator)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("spec.Targets = %#v, want linux only", spec.Targets)
	}
	aliases := Provider{}.Aliases()
	if len(aliases) != 2 || aliases[0] != "unikraftcloud" || aliases[1] != "ukc" {
		t.Fatalf("aliases = %#v, want [unikraftcloud ukc]", aliases)
	}
}

func TestUnikraftCloudAPIKeyFlagIsNotRegistered(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	registerUnikraftCloudProviderFlags(fs, Config{})
	for _, name := range []string{"unikraft-cloud-token", "unikraft-cloud-api-key", "unikraft-cloud-key", "ukc-token"} {
		if fs.Lookup(name) != nil {
			t.Fatalf("Unikraft Cloud API key surfaced as a flag --%s", name)
		}
	}
	for _, name := range []string{"unikraft-cloud-url", "unikraft-cloud-metro", "unikraft-cloud-image", "unikraft-cloud-memory"} {
		if fs.Lookup(name) == nil {
			t.Fatalf("--%s flag missing", name)
		}
	}
}

func TestApplyUnikraftCloudProviderFlags(t *testing.T) {
	for _, test := range []struct {
		name    string
		args    []string
		wantErr bool
		check   func(t *testing.T, cfg Config)
	}{
		{
			name: "overrides",
			args: []string{"-unikraft-cloud-metro", "dal", "-unikraft-cloud-image", "unikraft.org/nginx:latest", "-unikraft-cloud-memory", "256"},
			check: func(t *testing.T, cfg Config) {
				if cfg.UnikraftCloud.Metro != "dal" {
					t.Fatalf("metro = %q", cfg.UnikraftCloud.Metro)
				}
				if cfg.UnikraftCloud.Image != "unikraft.org/nginx:latest" {
					t.Fatalf("image = %q", cfg.UnikraftCloud.Image)
				}
				if cfg.UnikraftCloud.MemoryMB != 256 {
					t.Fatalf("memory = %d", cfg.UnikraftCloud.MemoryMB)
				}
			},
		},
		{
			name:    "class rejected",
			args:    []string{"-class", "small"},
			wantErr: true,
		},
		{
			name:    "type rejected",
			args:    []string{"-type", "cx22"},
			wantErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.String("class", "", "")
			fs.String("type", "", "")
			values := registerUnikraftCloudProviderFlags(fs, Config{})
			if err := fs.Parse(test.args); err != nil {
				t.Fatalf("parse: %v", err)
			}
			cfg := Config{Provider: providerName}
			err := applyUnikraftCloudProviderFlags(&cfg, fs, values)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if test.check != nil {
				test.check(t, cfg)
			}
		})
	}
}
