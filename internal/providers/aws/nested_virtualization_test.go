package aws

import (
	"flag"
	"io"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestAWSNestedVirtualizationFlag(t *testing.T) {
	fs := flag.NewFlagSet("warmup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	values := (Provider{}).RegisterFlags(fs, core.Config{})
	if err := fs.Parse([]string{"--aws-nested-virtualization"}); err != nil {
		t.Fatal(err)
	}
	cfg := core.Config{Provider: "aws", TargetOS: core.TargetLinux, Class: "beast", ServerType: "c7a.48xlarge"}
	if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if !cfg.AWSNestedVirtualization {
		t.Fatal("--aws-nested-virtualization did not enable aws.nestedVirtualization")
	}
	if cfg.ServerType != "" {
		t.Fatalf("serverType=%q, want class selection to re-resolve", cfg.ServerType)
	}
	if got := (Provider{}).ServerTypeForConfig(cfg); got != "c8i.48xlarge" {
		t.Fatalf("ServerTypeForConfig=%q want c8i.48xlarge", got)
	}

	exact := core.Config{Provider: "aws", TargetOS: core.TargetLinux, Class: "beast", ServerType: "c8i.2xlarge", ServerTypeExplicit: true}
	if err := (Provider{}).ApplyFlags(&exact, fs, values); err != nil {
		t.Fatal(err)
	}
	if exact.ServerType != "c8i.2xlarge" {
		t.Fatalf("explicit serverType changed to %q", exact.ServerType)
	}
}

func TestAWSNestedVirtualizationFlagUnsetKeepsConfig(t *testing.T) {
	fs := flag.NewFlagSet("warmup", flag.ContinueOnError)
	values := (Provider{}).RegisterFlags(fs, core.Config{})
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	cfg := core.Config{Provider: "aws", TargetOS: core.TargetLinux, Class: "small", ServerType: "c8i.2xlarge", AWSNestedVirtualization: true}
	if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if !cfg.AWSNestedVirtualization || cfg.ServerType != "c8i.2xlarge" {
		t.Fatalf("cfg changed without the flag: nested=%v serverType=%q", cfg.AWSNestedVirtualization, cfg.ServerType)
	}
	if got := (Provider{}).ServerTypeForConfig(core.Config{Provider: "aws", TargetOS: core.TargetLinux, Class: "small"}); got != "c7a.2xlarge" {
		t.Fatalf("default ServerTypeForConfig=%q want c7a.2xlarge", got)
	}
}
