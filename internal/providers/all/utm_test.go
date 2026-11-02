package all

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestUTMDiscoveryConfigAndTargetAdmission(t *testing.T) {
	testutil.IsolateUserDirs(t)
	t.Chdir(t.TempDir())
	config := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(config, []byte("provider: utm\nutm:\n  template: 11111111-1111-4111-8111-111111111111\n  user: prepared\n  sshKey: /tmp/prepared-key\n  storageRoot: /tmp/utm-bundles\n  startupTimeout: 3m\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", config)
	t.Setenv("CRABBOX_UTM_USER", "from-environment")
	p, err := core.ProviderFor("utm")
	if err != nil {
		t.Fatal(err)
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("UTM must remain a direct SSH lease provider: %#v", spec)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureCheckpoint, core.FeatureRestore, core.FeatureClaimExec} {
		if !spec.Features.Has(feature) {
			t.Errorf("UTM is missing accepted capability %s: %#v", feature, spec)
		}
	}
	for _, feature := range []core.Feature{core.FeatureDesktop, core.FeatureBrowser, core.FeatureCode, core.FeatureFork} {
		if spec.Features.Has(feature) {
			t.Errorf("UTM advertises unsupported capability %s: %#v", feature, spec)
		}
	}
	targets := map[string]bool{}
	for _, target := range spec.Targets {
		if target.WindowsMode != "" || targets[target.OS] {
			t.Fatalf("unexpected or duplicate UTM target: %#v", spec.Targets)
		}
		targets[target.OS] = true
	}
	if len(targets) != 2 || !targets[core.TargetLinux] || !targets[core.TargetMacOS] {
		t.Fatalf("UTM must advertise prepared Linux and IPSW macOS targets: %#v", spec.Targets)
	}
	if _, ok := p.(core.NativeCheckpointLifecycleProvider); !ok {
		t.Fatal("UTM checkpoint capability lacks native capture/verify/delete lifecycle")
	}
	if _, ok := p.(core.NativeCheckpointRestoreProvider); !ok {
		t.Fatal("UTM restore capability lacks native in-place restoration")
	}
	checkpoint, ok := p.(core.NativeCheckpointProvider)
	if !ok {
		t.Fatal("UTM checkpoint capability lacks native admission policy")
	}
	if _, supported := checkpoint.NativeCheckpointCapability(core.NativeCheckpointRequest{Strategy: core.CheckpointStrategyDiskSnapshot}); !supported {
		t.Fatal("UTM must admit native disk snapshots")
	}
	if _, supported := checkpoint.NativeCheckpointCapability(core.NativeCheckpointRequest{Strategy: core.CheckpointStrategyImage}); supported {
		t.Fatal("UTM must not admit native image/fork strategy")
	}
	cfg, err := core.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UTM.User != "from-environment" || cfg.UTM.StartupTimeout.String() != "3m0s" || cfg.Architecture != "arm64" || cfg.UTM.StorageRoot != "/tmp/utm-bundles" {
		t.Fatalf("typed UTM configuration not loaded: %#v arch=%s", cfg.UTM, cfg.Architecture)
	}
	configured, err := p.Configure(cfg, core.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := configured.(core.SSHLeaseBackend); !ok {
		t.Fatal("UTM must route run/sync through the core SSH lease backend")
	}
	if _, ok := configured.(core.ExecLeaseClaimResolver); !ok {
		t.Fatal("UTM claim-exec capability lacks a claim-fenced SSH resolver")
	}
	if _, ok := configured.(core.RunLeaseClaimResolver); !ok {
		t.Fatal("UTM reuse lacks claim-fenced run admission")
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := p.RegisterFlags(fs, cfg)
	if err = fs.Parse([]string{"--utm-user=override", "--utm-template=22222222-2222-4222-8222-222222222222"}); err != nil {
		t.Fatal(err)
	}
	if err = p.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.UTM.User != "override" || cfg.UTM.SSHKey != "/tmp/prepared-key" {
		t.Fatal("flag application clobbered absent fields")
	}
	for _, args := range [][]string{{"providers", "describe", "utm", "--json"}, {"config", "show", "--json"}} {
		var out bytes.Buffer
		if err = (core.App{Stdout: &out, Stderr: io.Discard}).Run(t.Context(), args); err != nil {
			t.Fatal(err)
		}
		if !json.Valid(out.Bytes()) || !strings.Contains(out.String(), "utm") {
			t.Fatalf("UTM absent from discovery/config: %s", out.String())
		}
	}
	// Template acquisition remains Linux-only; macOS is admitted separately for
	// IPSW acquisition. Configuration checks must not call UTM or start a VM.
	for _, scenario := range []struct {
		name, acquisition, target string
	}{
		{"template-macos", "template", core.TargetMacOS},
		{"template-windows", "template", core.TargetWindows},
		{"ipsw-linux", "ipsw", core.TargetLinux},
		{"ipsw-windows", "ipsw", core.TargetWindows},
		{"unknown-acquisition", "unsupported", core.TargetLinux},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			candidate := cfg
			candidate.UTM.Acquisition = scenario.acquisition
			candidate.TargetOS = scenario.target
			if _, err := p.Configure(candidate, core.Runtime{}); err == nil {
				t.Fatalf("unsupported UTM acquisition=%s target=%s accepted", scenario.acquisition, scenario.target)
			}
		})
	}
	architecture, ok := p.(core.ProviderArchitectureCapability)
	if !ok {
		t.Fatal("UTM must supply explicit target/architecture admission")
	}
	for _, target := range []string{core.TargetLinux, core.TargetMacOS} {
		candidate := cfg
		candidate.TargetOS = target
		candidate.UTM.Acquisition = "template"
		if target == core.TargetMacOS {
			candidate.UTM.Acquisition = "ipsw"
			candidate.UTM.Template = ""
		}
		if !architecture.SupportsArchitecture(candidate, "arm64") {
			t.Errorf("UTM target=%s must admit ARM64", target)
		}
		for _, arch := range []string{"amd64", "unsupported"} {
			if architecture.SupportsArchitecture(candidate, arch) {
				t.Errorf("unsupported UTM target=%s architecture=%s accepted", target, arch)
			}
		}
	}
	windows := cfg
	windows.TargetOS = core.TargetWindows
	if architecture.SupportsArchitecture(windows, "arm64") {
		t.Fatal("UTM must not admit Windows even with ARM64")
	}

	macOS := cfg
	macOS.TargetOS = core.TargetMacOS
	macOS.UTM.Template = ""
	macOS.UTM.SSHKey = ""
	macOS.SSHKey = ""
	macFlags := flag.NewFlagSet("ipsw", flag.ContinueOnError)
	macValues := p.RegisterFlags(macFlags, macOS)
	image := filepath.Join(t.TempDir(), "restore.ipsw")
	if err := macFlags.Parse([]string{"--utm-acquisition=ipsw", "--utm-ipsw=" + image, "--utm-install-timeout=30m"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyFlags(&macOS, macFlags, macValues); err != nil {
		t.Fatal(err)
	}
	if macOS.UTM.Acquisition != "ipsw" || macOS.UTM.IPSW != image || macOS.UTM.InstallTimeout.String() != "30m0s" {
		t.Fatalf("typed macOS IPSW flags not applied: %#v", macOS.UTM)
	}
	macBackend, err := p.Configure(macOS, core.Runtime{})
	if err != nil {
		t.Fatalf("supported macOS IPSW configuration refused: %v", err)
	}
	if _, ok := macBackend.(core.SSHLeaseBackend); !ok {
		t.Fatal("macOS IPSW must route continuing run/sync through SSH leases")
	}
	if _, ok := macBackend.(core.ExecLeaseClaimResolver); !ok {
		t.Fatal("macOS IPSW must supply continuing claim-fenced SSH execution")
	}
}
