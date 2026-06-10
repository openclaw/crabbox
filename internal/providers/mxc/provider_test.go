package mxc

import (
	"flag"
	"io"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestExperimentalContainmentRequiresOptIn(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetWindows
	cfg.WindowsMode = core.WindowsModeNormal
	cfg.MXC.Containment = "windows_sandbox"
	_, err := (Provider{}).Configure(cfg, core.Runtime{})
	if err == nil || !strings.Contains(err.Error(), "--mxc-experimental") {
		t.Fatalf("err=%v", err)
	}
}

func TestConfigureNormalizesContainment(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetWindows
	cfg.WindowsMode = core.WindowsModeNormal
	cfg.MXC.Containment = "ProcessContainer"
	configured, err := (Provider{}).Configure(cfg, core.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if got := configured.(*backend).cfg.MXC.Containment; got != "processcontainer" {
		t.Fatalf("containment=%q", got)
	}
}

func TestConfigureAcceptsStableProcessIntent(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetWindows
	cfg.WindowsMode = core.WindowsModeNormal
	cfg.MXC.Containment = "Process"

	configured, err := (Provider{}).Configure(cfg, core.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if got := configured.(*backend).cfg.MXC.Containment; got != "process" {
		t.Fatalf("containment=%q", got)
	}
}

func TestFlagsApplyPolicy(t *testing.T) {
	cfg := core.BaseConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--mxc-network", "allow", "--mxc-readwrite-paths", `C:\src,C:\cache`, "--mxc-allow-dacl-mutation", "--mxc-allow-windows-ui", "--mxc-experimental"}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.MXC.Network != "allow" || len(cfg.MXC.ReadWritePaths) != 2 || !cfg.MXC.AllowDACLMutation || !cfg.MXC.AllowWindowsUI || !cfg.MXC.Experimental {
		t.Fatalf("mxc=%+v", cfg.MXC)
	}
}

func TestParseWindowsBuild(t *testing.T) {
	build, err := parseWindowsBuild("CurrentBuildNumber    REG_SZ    26100\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if build != 26100 {
		t.Fatalf("build = %d, want 26100", build)
	}
}
