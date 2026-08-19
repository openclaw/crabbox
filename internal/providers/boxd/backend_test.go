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
	b.readyPollInterval = time.Millisecond
	b.readyTimeout = 2 * time.Second
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

// TestAcquireCreatesIsolatedMachine pins the sandbox invariant: every machine
// the provider creates is isolated, and the flag is not configurable.
func TestAcquireCreatesIsolatedMachine(t *testing.T) {
	var b *backend
	runner := &scriptedRunner{}
	runner.scripts = []func([]string) (core.LocalCommandResult, error){
		ok(`[]`), // machine list (slug allocation inventory)
		func(args []string) (core.LocalCommandResult, error) { // machine new
			name := args[2]
			return core.LocalCommandResult{Stdout: fmt.Sprintf(`{"name":%q,"id":"vm-1234","url":"%s.boxd.sh","source":"standalone","boot":"1.9s"}`, name, name)}, nil
		},
		func(args []string) (core.LocalCommandResult, error) { // machine get
			name := args[2]
			return core.LocalCommandResult{Stdout: fmt.Sprintf(`{"name":%q,"id":"vm-1234","status":"running","url":"%s.boxd.sh"}`, name, name)}, nil
		},
		func([]string) (core.LocalCommandResult, error) { // machine list (ssh sync)
			name := runner.calls[1].args[2]
			writeTestSSHEntry(t, b.sshConfigPath, name+".boxd.sh", "12345")
			return core.LocalCommandResult{Stdout: fmt.Sprintf(`[{"name":%q,"status":"running","url":"%s.boxd.sh","source":"standalone","sharing":"private"}]`, name, name)}, nil
		},
	}
	b = testBackend(t, runner)
	restore := waitForBoxdSSHReady
	waitForBoxdSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	defer func() { waitForBoxdSSHReady = restore }()

	lease, err := b.Acquire(context.Background(), core.AcquireRequest{})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	newCall := runner.calls[1]
	if newCall.args[0] != "machine" || newCall.args[1] != "new" {
		t.Fatalf("second call=%v want machine new", newCall.args)
	}
	isolated := false
	for _, arg := range newCall.args {
		if arg == "--isolated" {
			isolated = true
		}
	}
	if !isolated {
		t.Fatalf("machine new argv=%v does not pin --isolated", newCall.args)
	}
	name := newCall.args[2]
	if leaseIDFromMachineName(name) != lease.LeaseID {
		t.Fatalf("machine name %q does not encode lease %q", name, lease.LeaseID)
	}
	if lease.SSH.Host != name+".boxd.sh" || lease.SSH.Port != "12345" || lease.SSH.User != "boxd" {
		t.Fatalf("ssh target=%#v", lease.SSH)
	}
	if !lease.SSH.NoControlMaster {
		t.Fatal("SSH multiplexing must be disabled for the boxd proxy")
	}
	if lease.SSH.Key == "" {
		t.Fatal("ssh target missing the CLI-managed identity file")
	}
	claims, err := boxdClaims(b.configForRun())
	if err != nil {
		t.Fatal(err)
	}
	if _, okClaim := claims[lease.LeaseID]; !okClaim {
		t.Fatalf("acquire did not persist the local claim (claims=%v)", claims)
	}
	if lease.Server.Labels["machine"] != name || lease.Server.Labels["state"] != "ready" {
		t.Fatalf("server labels=%v", lease.Server.Labels)
	}
	if lease.Server.Labels["release"] != "delete" {
		t.Fatalf("acquire did not record the release intent: labels=%v", lease.Server.Labels)
	}
}

// TestReleaseIntentPersistsAcrossProcesses pins the morph/coder convention: the
// release action recorded on the lease at acquire time decides destroy-vs-stop
// for a later process that passed no explicit flag.
func TestReleaseIntentPersistsAcrossProcesses(t *testing.T) {
	cfg := core.BaseConfig() // DeleteOnRelease defaults true, not explicit
	stopLease := core.LeaseTarget{Server: core.Server{Labels: map[string]string{"release": "stop"}}}
	if deleteOnRelease(stopLease, cfg) {
		t.Fatal("recorded release=stop must beat the destroy default")
	}
	deleteLease := core.LeaseTarget{Server: core.Server{Labels: map[string]string{"release": "delete"}}}
	if !deleteOnRelease(deleteLease, cfg) {
		t.Fatal("recorded release=delete must destroy")
	}
	cfg.Boxd.DeleteOnRelease = false
	if releaseAction(cfg) != "stop" {
		t.Fatalf("releaseAction=%q want stop", releaseAction(cfg))
	}
	core.MarkDeleteOnReleaseExplicit(&cfg, providerName)
	if deleteOnRelease(deleteLease, cfg) {
		t.Fatal("an explicit flag must beat the recorded label")
	}
}

// TestAcquireRollsBackOnTerminalMachine pins that a failed boot destroys the
// machine it created rather than leaking it.
func TestAcquireRollsBackOnTerminalMachine(t *testing.T) {
	runner := &scriptedRunner{}
	runner.scripts = []func([]string) (core.LocalCommandResult, error){
		ok(`[]`),
		func(args []string) (core.LocalCommandResult, error) {
			name := args[2]
			return core.LocalCommandResult{Stdout: fmt.Sprintf(`{"name":%q,"id":"vm-1","url":"%s.boxd.sh"}`, name, name)}, nil
		},
		func(args []string) (core.LocalCommandResult, error) {
			name := args[2]
			return core.LocalCommandResult{Stdout: fmt.Sprintf(`{"name":%q,"id":"vm-1","status":"failed","url":"%s.boxd.sh"}`, name, name)}, nil
		},
		ok(`{"removed":true}`), // machine remove (rollback)
	}
	b := testBackend(t, runner)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "terminal state") {
		t.Fatalf("err=%v", err)
	}
	last := runner.calls[len(runner.calls)-1]
	if last.args[0] != "machine" || last.args[1] != "remove" || !contains(last.args, "--confirm") {
		t.Fatalf("rollback did not destroy the machine: %v", last.args)
	}
}

func TestReleaseDestroysAndRemovesClaim(t *testing.T) {
	name := "crabbox-cbx-aaaabbbbcccc"
	leaseID := "cbx_aaaabbbbcccc"
	runner := &scriptedRunner{scripts: []func([]string) (core.LocalCommandResult, error){
		ok(fmt.Sprintf(`{"name":%q,"id":"vm-9","status":"running","url":"%s.boxd.sh"}`, name, name)),
		ok(`{"removed":true}`),
	}}
	b := testBackend(t, runner)
	cfg := b.configForRun()
	server := core.Server{CloudID: "vm-9", Provider: providerName, Name: name, Labels: map[string]string{"machine": name, "lease": leaseID}}
	if err := core.ClaimLeaseTargetForConfig(leaseID, "", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if claims, _ := boxdClaims(cfg); len(claims) != 0 {
		t.Fatalf("claim not removed: %v", claims)
	}
	last := runner.calls[len(runner.calls)-1]
	if last.args[1] != "remove" || last.args[2] != name {
		t.Fatalf("release argv=%v", last.args)
	}
}

func TestReleaseTreatsPersistentNotFoundAsGone(t *testing.T) {
	name := "crabbox-cbx-aaaabbbbcccc"
	leaseID := "cbx_aaaabbbbcccc"
	notFound := fail("Error: VM '" + name + "' not found")
	runner := &scriptedRunner{scripts: []func([]string) (core.LocalCommandResult, error){notFound, notFound, notFound}}
	b := testBackend(t, runner)
	cfg := b.configForRun()
	server := core.Server{Provider: providerName, Name: name, Labels: map[string]string{"machine": name, "lease": leaseID}}
	if err := core.ClaimLeaseTargetForConfig(leaseID, "", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatalf("release of a gone machine must succeed: %v", err)
	}
	if claims, _ := boxdClaims(cfg); len(claims) != 0 {
		t.Fatalf("claim not removed: %v", claims)
	}
	for _, c := range runner.calls {
		if c.args[1] == "remove" {
			t.Fatalf("remove must not run against a machine that is provably gone: %v", runner.calls)
		}
	}
}

func TestReleaseStopModeStopsAndRetainsClaim(t *testing.T) {
	name := "crabbox-cbx-aaaabbbbcccc"
	leaseID := "cbx_aaaabbbbcccc"
	runner := &scriptedRunner{scripts: []func([]string) (core.LocalCommandResult, error){
		ok(fmt.Sprintf(`{"name":%q,"id":"vm-9","status":"running","url":"%s.boxd.sh"}`, name, name)),
		ok(""), // machine stop
	}}
	b := testBackend(t, runner)
	b.cfg.Boxd.DeleteOnRelease = false
	core.MarkDeleteOnReleaseExplicit(&b.cfg, providerName)
	cfg := b.configForRun()
	server := core.Server{Provider: providerName, Name: name, Labels: map[string]string{"machine": name, "lease": leaseID}}
	if err := core.ClaimLeaseTargetForConfig(leaseID, "", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	lease := core.LeaseTarget{LeaseID: leaseID, Server: server}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if !b.RetainLeaseClaimAfterRelease(lease) {
		t.Fatal("stop-mode release must retain the claim")
	}
	last := runner.calls[len(runner.calls)-1]
	if last.args[1] != "stop" || last.args[2] != name {
		t.Fatalf("release argv=%v", last.args)
	}
	claims, _ := boxdClaims(cfg)
	claim, okClaim := claims[leaseID]
	if !okClaim || claim.Labels["state"] != "stopped" {
		t.Fatalf("claim after stop-release=%v", claims)
	}
}

// TestReleaseRefusesUnclaimedMachine pins the destructive ownership fence: a
// canonical crabbox-cbx-* machine name or lease id with no matching local
// claim must never reach `machine stop`/`machine remove` — a same-account
// machine managed by a different crabbox install is not ours to touch.
func TestReleaseRefusesUnclaimedMachine(t *testing.T) {
	foreign := "crabbox-cbx-ffffeeeedddd"
	runner := &scriptedRunner{} // any CLI invocation would fail the test
	b := testBackend(t, runner)

	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{Server: core.Server{Name: foreign, Labels: map[string]string{"machine": foreign}}},
	})
	if err == nil || !strings.Contains(err.Error(), "no local claim") {
		t.Fatalf("unclaimed machine name must be refused, err=%v", err)
	}
	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{LeaseID: "cbx_ffffeeeedddd"},
	})
	if err == nil || !strings.Contains(err.Error(), "no local claim") {
		t.Fatalf("unclaimed lease id must be refused, err=%v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("release of unclaimed identities must not invoke the boxd CLI: %v", runner.calls)
	}
}

// TestReleaseRefusesClaimMachineMismatch pins that a claim authorizes exactly
// the machine it recorded, not any machine sharing the lease id.
func TestReleaseRefusesClaimMachineMismatch(t *testing.T) {
	leaseID := "cbx_aaaabbbbcccc"
	claimedName := "crabbox-cbx-aaaabbbbcccc"
	runner := &scriptedRunner{}
	b := testBackend(t, runner)
	cfg := b.configForRun()
	server := core.Server{Provider: providerName, Name: claimedName, Labels: map[string]string{"machine": claimedName, "lease": leaseID}}
	if err := core.ClaimLeaseTargetForConfig(leaseID, "", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{LeaseID: leaseID, Server: core.Server{Labels: map[string]string{"machine": "crabbox-cbx-other0000000"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "names machine") {
		t.Fatalf("claim/machine mismatch must be refused, err=%v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("mismatched release must not invoke the boxd CLI: %v", runner.calls)
	}
}

// TestListRequiresLocalClaim pins the ownership rule: boxd has no server-side
// labels, so a crabbox-named machine with no matching local claim is foreign
// and must not be surfaced as a lease.
func TestListRequiresLocalClaim(t *testing.T) {
	claimedName := "crabbox-cbx-aaaabbbbcccc"
	foreignName := "crabbox-cbx-ffffeeeedddd"
	runner := &scriptedRunner{scripts: []func([]string) (core.LocalCommandResult, error){
		ok(fmt.Sprintf(`[
			{"name":%q,"status":"running","url":"%s.boxd.sh","source":"standalone","sharing":"private"},
			{"name":%q,"status":"running","url":"%s.boxd.sh","source":"standalone","sharing":"private"},
			{"name":"unrelated-vm","status":"running","url":"unrelated-vm.boxd.sh","source":"standalone","sharing":"private"}
		]`, claimedName, claimedName, foreignName, foreignName)),
	}}
	b := testBackend(t, runner)
	cfg := b.configForRun()
	server := core.Server{Provider: providerName, Name: claimedName, Labels: map[string]string{"machine": claimedName, "lease": "cbx_aaaabbbbcccc"}}
	if err := core.ClaimLeaseTargetForConfig("cbx_aaaabbbbcccc", "", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	views, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Labels["machine"] != claimedName {
		t.Fatalf("views=%#v", views)
	}
}

func TestCleanupDryRunNeverDestroys(t *testing.T) {
	name := "crabbox-cbx-aaaabbbbcccc"
	leaseID := "cbx_aaaabbbbcccc"
	runner := &scriptedRunner{scripts: []func([]string) (core.LocalCommandResult, error){
		ok(fmt.Sprintf(`[{"name":%q,"status":"running","url":"%s.boxd.sh","source":"standalone","sharing":"private"}]`, name, name)),
	}}
	b := testBackend(t, runner)
	var stdout strings.Builder
	b.rt.Stdout = &stdout
	cfg := b.configForRun()
	server := core.Server{Provider: providerName, Name: name, Labels: map[string]string{"machine": name, "lease": leaseID, "recovery": "rollback-cleanup"}}
	if err := core.ClaimLeaseTargetForConfig(leaseID, "", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "would destroy machine="+name) {
		t.Fatalf("dry-run output=%q", stdout.String())
	}
	for _, c := range runner.calls {
		if len(c.args) > 1 && c.args[1] == "remove" {
			t.Fatalf("dry-run must not destroy: %v", runner.calls)
		}
	}
}

func TestCleanupSkipsUnclaimedMachines(t *testing.T) {
	foreign := "crabbox-cbx-ffffeeeedddd"
	runner := &scriptedRunner{scripts: []func([]string) (core.LocalCommandResult, error){
		ok(fmt.Sprintf(`[{"name":%q,"status":"running","url":"%s.boxd.sh","source":"standalone","sharing":"private"}]`, foreign, foreign)),
	}}
	b := testBackend(t, runner)
	var stderr strings.Builder
	b.rt.Stderr = &stderr
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "no local claim") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	for _, c := range runner.calls {
		if len(c.args) > 1 && c.args[1] == "remove" {
			t.Fatalf("cleanup destroyed an unclaimed machine: %v", runner.calls)
		}
	}
}

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

func TestResolveMachineIdentityShapes(t *testing.T) {
	b := testBackend(t, &scriptedRunner{})
	cfg := b.configForRun()
	name, leaseID, _, claimed, err := b.resolveMachineIdentity("cbx_a1b2c3d4e5f6", cfg)
	if err != nil || claimed || name != "crabbox-cbx-a1b2c3d4e5f6" || leaseID != "cbx_a1b2c3d4e5f6" {
		t.Fatalf("lease-id resolve name=%q lease=%q claimed=%v err=%v", name, leaseID, claimed, err)
	}
	name, leaseID, _, _, err = b.resolveMachineIdentity("crabbox-cbx-a1b2c3d4e5f6", cfg)
	if err != nil || name != "crabbox-cbx-a1b2c3d4e5f6" || leaseID != "cbx_a1b2c3d4e5f6" {
		t.Fatalf("name resolve name=%q lease=%q err=%v", name, leaseID, err)
	}
	if _, _, _, _, err = b.resolveMachineIdentity("no-such-thing", cfg); err == nil {
		t.Fatal("unknown identifier must not resolve")
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
