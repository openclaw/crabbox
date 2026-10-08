package hyperv

import (
	"flag"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestApplyFlagsPreservesExplicitLinuxWorkRootSentinel(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = targetLinux
	cfg.WorkRoot = "/srv/global"

	fs := flag.NewFlagSet("hyperv", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Set("hyperv-work-root", "/work/crabbox"); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.HyperV.WorkRoot != "/work/crabbox" || cfg.WorkRoot != "/work/crabbox" {
		t.Fatalf("hyperv.workRoot=%q workRoot=%q", cfg.HyperV.WorkRoot, cfg.WorkRoot)
	}
}
