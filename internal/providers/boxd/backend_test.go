package boxd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type call struct {
	name string
	args []string
}

// scriptedRunner replays canned results and can run a side effect per call
// (e.g. writing the ssh-config file the way `boxd machine list` does).
type scriptedRunner struct {
	calls   []call
	scripts []func(args []string) (core.LocalCommandResult, error)
}

func (r *scriptedRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, call{name: req.Name, args: append([]string(nil), req.Args...)})
	if len(r.scripts) == 0 {
		return core.LocalCommandResult{}, fmt.Errorf("unexpected command: %s %v", req.Name, req.Args)
	}
	script := r.scripts[0]
	r.scripts = r.scripts[1:]
	return script(req.Args)
}

func ok(stdout string) func([]string) (core.LocalCommandResult, error) {
	return func([]string) (core.LocalCommandResult, error) {
		return core.LocalCommandResult{Stdout: stdout}, nil
	}
}

func fail(stderr string) func([]string) (core.LocalCommandResult, error) {
	return func([]string) (core.LocalCommandResult, error) {
		return core.LocalCommandResult{ExitCode: 1, Stderr: stderr}, errors.New("exit status 1")
	}
}

func testBackend(t *testing.T, runner *scriptedRunner) *backend {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
	b.sshConfigPath = filepath.Join(t.TempDir(), "ssh_config")
	b.knownHostsPath = filepath.Join(t.TempDir(), "known_hosts")
	b.readyPollInterval = time.Millisecond
	b.readyTimeout = 2 * time.Second
	b.releaseNotFoundGrace = 5 * time.Millisecond
	return b
}

func writeTestSSHEntry(t *testing.T, path, host, port string) {
	t.Helper()
	entry := "# BEGIN boxd (managed by boxd; do not edit)\n" +
		"Host " + strings.TrimSuffix(host, ".boxd.sh") + ".boxd " + host + "\n" +
		"    HostName " + host + "\n"
	if port != "" {
		entry += "    Port " + port + "\n"
	}
	entry += "    User boxd\n" +
		"    IdentityFile \"/home/user/.config/boxd/id_ed25519_dev\"\n" +
		"# END boxd\n"
	if err := os.WriteFile(path, []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeTestHostKeyPin mimics the boxd CLI pre-trusting the proxy host key.
func writeTestHostKeyPin(t *testing.T, path, host, port string) {
	t.Helper()
	line := "[" + host + "]:" + port + " ssh-ed25519 AAAATESTPIN boxd-hosts\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProviderContract(t *testing.T) {
	provider := Provider{}
	if provider.Name() != providerName || provider.Aliases() != nil {
		t.Fatalf("provider identity=%q aliases=%v", provider.Name(), provider.Aliases())
	}
	spec := provider.Spec()
	if spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever ||
		!spec.Features.Has(core.FeatureSSH) || !spec.Features.Has(core.FeatureCrabboxSync) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	resolved, err := core.ProviderFor(providerName)
	if err != nil || resolved.Name() != providerName {
		t.Fatalf("ProviderFor(%q)=%v err=%v", providerName, resolved, err)
	}
}

func TestMachineNameRoundTrip(t *testing.T) {
	leaseID := "cbx_a1b2c3d4e5f6"
	name := machineNameForLease(leaseID)
	if name != "crabbox-cbx-a1b2c3d4e5f6" {
		t.Fatalf("machineNameForLease=%q", name)
	}
	if got := leaseIDFromMachineName(name); got != leaseID {
		t.Fatalf("leaseIDFromMachineName=%q want %q", got, leaseID)
	}
	for _, bad := range []string{"", "crabbox-", "myapp", "crabbox-notalease", "crabboxcbx-a1b2c3d4e5f6"} {
		if got := leaseIDFromMachineName(bad); got != "" {
			t.Fatalf("leaseIDFromMachineName(%q)=%q want empty", bad, got)
		}
	}
}

func TestBoxdStateMapping(t *testing.T) {
	for status, want := range map[string]string{
		"running":    "ready",
		"standby":    "ready",
		"suspended":  "ready",
		"hibernated": "ready",
		"starting":   "provisioning",
		"pending":    "provisioning",
		"stopped":    "stopped",
		"failed":     "failed",
	} {
		if got := boxdState(status); got != want {
			t.Fatalf("boxdState(%q)=%q want %q", status, got, want)
		}
	}
	if !machineTerminal("failed") || machineTerminal("running") || machineTerminal("stopped") {
		t.Fatal("machineTerminal misclassifies")
	}
}

func TestMissingMachineResponseAnchorsName(t *testing.T) {
	if !missingMachineResponse("crabbox-cbx-1", "", "Error: VM 'crabbox-cbx-1' not found") {
		t.Fatal("definitive not-found not recognized")
	}
	if missingMachineResponse("crabbox-cbx-1", "", "Error: config file not found") {
		t.Fatal("unrelated not-found treated as machine gone")
	}
	if missingMachineResponse("crabbox-cbx-1", "", "Error: VM 'other-machine' not found") {
		t.Fatal("another machine's not-found treated as this machine gone")
	}
}

func TestZoneFromHost(t *testing.T) {
	if zone := zoneFromHost("crabbox-cbx-1.boxd.sh", "crabbox-cbx-1"); zone != "boxd.sh" {
		t.Fatalf("zone=%q", zone)
	}
	if zone := zoneFromHost("https://crabbox-cbx-1.boxd.azin.run/", "crabbox-cbx-1"); zone != "boxd.azin.run" {
		t.Fatalf("zone=%q", zone)
	}
	if zone := zoneFromHost("unrelated.example.com", "crabbox-cbx-1"); zone != "" {
		t.Fatalf("zone=%q want empty", zone)
	}
}

// The next three tests pin the machine-id binding: boxd retains destroyed
// machine names for reuse, so a stale claim plus a name match must never
// authorize touching a REPLACEMENT machine with a different immutable id.

func TestFlagsApplyOnlyWhenSet(t *testing.T) {
	provider := Provider{}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := core.BaseConfig()
	values := provider.RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{"-boxd-org", "acme", "-boxd-delete-on-release=false"}); err != nil {
		t.Fatal(err)
	}
	if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Boxd.Org != "acme" || cfg.Boxd.DeleteOnRelease {
		t.Fatalf("cfg.Boxd=%#v", cfg.Boxd)
	}
	if cfg.Boxd.CLIPath != "boxd" || cfg.Boxd.WorkRoot != defaultBoxdWorkRoot {
		t.Fatalf("defaults clobbered: %#v", cfg.Boxd)
	}
}

func TestFlagsRejectClassAndTypeForBoxd(t *testing.T) {
	provider := Provider{}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs.String("class", "", "")
	fs.String("type", "", "")
	values := provider.RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{"-class", "small"}); err != nil {
		t.Fatal(err)
	}
	err := provider.ApplyFlags(&cfg, fs, values)
	if err == nil || !strings.Contains(err.Error(), "--class is not supported") {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateConfigRejectsUnsafeWorkRoot(t *testing.T) {
	provider := Provider{}
	for workRoot, want := range map[string]string{
		"../../etc": "canonical absolute Linux path",
		"/tmp":      "too broad",
		"":          "canonical absolute Linux path",
	} {
		cfg := core.BaseConfig()
		cfg.Boxd.WorkRoot = workRoot
		err := provider.ValidateConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("workRoot=%q err=%v want %q", workRoot, err, want)
		}
	}
}

func TestClaimScopeFencesEndpointAndOrg(t *testing.T) {
	provider := Provider{}
	cfg := core.BaseConfig()
	if scope := provider.ClaimScope(cfg); scope != "" {
		t.Fatalf("default scope=%q want empty", scope)
	}
	cfg.Boxd.APIURL = "https://boxd-stg.sh"
	cfg.Boxd.Org = "acme"
	if scope := provider.ClaimScope(cfg); scope != "endpoint:https://boxd-stg.sh|org:acme" {
		t.Fatalf("scope=%q", scope)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
