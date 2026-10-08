package hyperv

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestLinuxTargetIgnoresCompiledWindowsWorkRootDefault(t *testing.T) {
	cfg := core.Config{TargetOS: core.TargetLinux}
	cfg.HyperV.WorkRoot = `C:\crabbox`
	applyDefaults(&cfg)
	if cfg.HyperV.WorkRoot != "/work/crabbox" {
		t.Fatalf("Linux guest work root=%q, want /work/crabbox", cfg.HyperV.WorkRoot)
	}

	custom := core.Config{TargetOS: core.TargetLinux}
	custom.HyperV.WorkRoot = "/srv/work"
	applyDefaults(&custom)
	if custom.HyperV.WorkRoot != "/srv/work" {
		t.Fatalf("explicit Linux work root replaced: %q", custom.HyperV.WorkRoot)
	}

	win := core.Config{TargetOS: core.TargetWindows}
	win.HyperV.WorkRoot = `C:\crabbox`
	applyDefaults(&win)
	if win.HyperV.WorkRoot != `C:\crabbox` {
		t.Fatalf("Windows work root changed: %q", win.HyperV.WorkRoot)
	}
}
