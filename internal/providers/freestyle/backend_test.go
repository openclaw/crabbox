package freestyle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestFreestyleProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("kind=%q", spec.Kind)
	}
	for _, feature := range []core.Feature{core.FeatureArchiveSync, core.FeatureRunSession} {
		if !spec.Features.Has(feature) {
			t.Fatalf("missing feature %s in %#v", feature, spec.Features)
		}
	}
}

func TestFreestyleExecCommandPreservesShellString(t *testing.T) {
	got := freestyleExecCommand([]string{"pnpm install && pnpm test"}, true)
	want := "pnpm install && pnpm test"
	if got != want {
		t.Fatalf("command=%q want %q", got, want)
	}
}

func TestFreestyleExecCommandQuotesImplicitShellArgv(t *testing.T) {
	if got := freestyleExecCommand([]string{"go", "test", "./..."}, false); got != "'go' 'test' './...'" {
		t.Fatalf("command=%q", got)
	}
	got := freestyleExecCommand([]string{"FOO=bar", "pnpm", "test"}, false)
	if !strings.Contains(got, "FOO=") || !strings.Contains(got, "'pnpm'") {
		t.Fatalf("command=%q", got)
	}
}

func TestFreestyleExecCommandPreservesSpacedArguments(t *testing.T) {
	got := freestyleExecCommand([]string{"echo", "hello world"}, false)
	want := "'echo' 'hello world'"
	if got != want {
		t.Fatalf("command=%q want %q", got, want)
	}
}

func TestFreestyleExecCommandPreservesSingleShellString(t *testing.T) {
	got := freestyleExecCommand([]string{"echo hello from freestyle"}, false)
	want := "echo hello from freestyle"
	if got != want {
		t.Fatalf("command=%q want %q", got, want)
	}
}

func TestFreestyleEnvExportCommandQuotesValuesOnly(t *testing.T) {
	got := freestyleEnvExportCommand(map[string]string{
		"GREETING":      "hello world",
		"Z_TOKEN":       "abc'123",
		"BAD; id >&2 #": "boom",
	})
	want := "export GREETING='hello world' Z_TOKEN='abc'\\''123'"
	if got != want {
		t.Fatalf("env export=%q want %q", got, want)
	}
	if strings.Contains(got, "'GREETING'") || strings.Contains(got, "BAD;") {
		t.Fatalf("env export contains unsafe name quoting or invalid name: %q", got)
	}
}

func TestFreestyleExecForwardsEnvAfterWorkdir(t *testing.T) {
	client := &fakeFreestyleClient{}
	backend := &freestyleBackend{rt: Runtime{Stderr: io.Discard}}
	code, err := backend.exec(context.Background(), client, "vm123", "/workspace/repo", []string{`echo "$GREETING"`}, false, map[string]string{
		"GREETING": "hello world",
	})
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit code=%d", code)
	}
	if len(client.execCommands) != 1 {
		t.Fatalf("exec commands=%#v", client.execCommands)
	}
	command := client.execCommands[0]
	want := `bash -lc 'cd '\''/workspace/repo'\'' && export GREETING='\''hello world'\'' && echo "$GREETING"'`
	if command != want {
		t.Fatalf("command=%q want %q", command, want)
	}
	if strings.Contains(command, "'GREETING'=") {
		t.Fatalf("command quotes env name: %s", command)
	}
}

func TestFreestyleAPIKeyFlagIsNotRegistered(t *testing.T) {
	cfg := Config{}
	cfg.Freestyle.APIKey = "secret-key"
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterFreestyleProviderFlags(fs, cfg)
	for _, name := range []string{"freestyle-api-key", "freestyle-api-token", "freestyle-key", "freestyle-token"} {
		if fs.Lookup(name) != nil {
			t.Fatalf("freestyle API key surfaced as a flag --%s", name)
		}
	}
	for _, name := range []string{"freestyle-api-url", "freestyle-workdir", "freestyle-vcpus", "freestyle-memory-gb"} {
		if fs.Lookup(name) == nil {
			t.Fatalf("%s flag missing", name)
		}
	}
}

func TestFreestyleWarmupRejectsActionsRunner(t *testing.T) {
	backend := &freestyleBackend{rt: Runtime{Stderr: io.Discard}}
	err := backend.Warmup(context.Background(), WarmupRequest{ActionsRunner: true})
	if err == nil || !strings.Contains(err.Error(), "--actions-runner") {
		t.Fatalf("Warmup err=%v, want actions-runner rejection", err)
	}
}

func TestFreestyleStatusReady(t *testing.T) {
	if !freestyleStatusReady("running") {
		t.Fatal("running should be ready")
	}
	for _, status := range []string{"building", "starting", "suspended", "stopped", "lost"} {
		if freestyleStatusReady(status) {
			t.Fatalf("%q should not be ready", status)
		}
	}
	for _, status := range []string{"stopped", "lost"} {
		if !freestyleStatusTerminal(status) {
			t.Fatalf("%q should be terminal", status)
		}
	}
	for _, status := range []string{"building", "starting", "running", "suspending", "suspended"} {
		if freestyleStatusTerminal(status) {
			t.Fatalf("%q should not be terminal", status)
		}
	}
}

func TestFreestyleStatusWaitFailsOnTerminalState(t *testing.T) {
	client := &fakeFreestyleClient{getVM: freestyleVM{
		ID:    "vm123",
		Name:  "crabbox-repo-abc123",
		State: "stopped",
	}}
	oldClient := newFreestyleClient
	newFreestyleClient = func(Config, Runtime) (freestyleAPI, error) {
		return client, nil
	}
	t.Cleanup(func() { newFreestyleClient = oldClient })

	backend := &freestyleBackend{cfg: Config{}, rt: Runtime{Stderr: io.Discard}}
	_, err := backend.Status(context.Background(), StatusRequest{
		ID:          "fsb_vm123",
		Wait:        true,
		WaitTimeout: time.Minute,
	})
	if err == nil || !strings.Contains(err.Error(), `terminal state "stopped"`) {
		t.Fatalf("Status err=%v, want terminal-state failure", err)
	}
}

func TestResolveFreestyleLeaseIDRejectsUnclaimedRawSandbox(t *testing.T) {
	client := &fakeFreestyleClient{getVM: freestyleVM{
		ID:    "vm123",
		Name:  "personal-vm",
		State: "running",
	}}
	backend := &freestyleBackend{}
	if _, _, err := backend.resolveLeaseID(context.Background(), client, "random-vm-id", "", false); err == nil {
		t.Fatal("expected raw non-Crabbox vm to be rejected")
	}
	if _, _, err := backend.resolveLeaseID(context.Background(), client, "fsb_vm123", "", false); err == nil {
		t.Fatal("expected unclaimed Freestyle vm to be rejected")
	}
}

func TestResolveFreestyleLeaseIDAcceptsCrabboxSandbox(t *testing.T) {
	client := &fakeFreestyleClient{getVM: freestyleVM{
		ID:    "vm123",
		Name:  "crabbox-repo-abc123",
		State: "running",
	}}
	backend := &freestyleBackend{}
	leaseID, name, err := backend.resolveLeaseID(context.Background(), client, "fsb_vm123", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "fsb_vm123" || name != "vm123" {
		t.Fatalf("lease=%q name=%q", leaseID, name)
	}
}

func TestResolveFreestyleLeaseIDRejectsMalformedCrabboxNames(t *testing.T) {
	for _, name := range []string{
		"crabbox repo abc123",
		"crabbox/repo/abc123",
		"crabbox?repo=abc123",
		"Crabbox-repo-abc123",
	} {
		t.Run(name, func(t *testing.T) {
			client := &fakeFreestyleClient{getVM: freestyleVM{ID: "vm123", Name: name, State: "running"}}
			backend := &freestyleBackend{}
			if _, _, err := backend.resolveLeaseID(context.Background(), client, "fsb_vm123", "", false); err == nil {
				t.Fatalf("expected malformed Freestyle VM name %q to be rejected", name)
			}
		})
	}
}

func TestFreestyleStopRejectsMalformedCrabboxNameWithoutDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeFreestyleClient{getVM: freestyleVM{
		ID:    "vm123",
		Name:  "crabbox repo abc123",
		State: "running",
	}}
	oldClient := newFreestyleClient
	newFreestyleClient = func(Config, Runtime) (freestyleAPI, error) { return client, nil }
	t.Cleanup(func() { newFreestyleClient = oldClient })
	backend := &freestyleBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}

	err := backend.Stop(context.Background(), StopRequest{ID: "fsb_vm123"})
	if err == nil || !strings.Contains(err.Error(), "not claimed by Crabbox") {
		t.Fatalf("Stop error = %v, want ownership refusal", err)
	}
	if len(client.deleteIDs) != 0 {
		t.Fatalf("DeleteVM called for malformed name: %#v", client.deleteIDs)
	}
}

func TestFreestyleStopRejectsCanonicalSandboxWithoutExactClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeFreestyleClient{getVM: freestyleVM{
		ID:    "vm123",
		Name:  "crabbox-repo-abc123",
		State: "running",
	}}
	oldClient := newFreestyleClient
	newFreestyleClient = func(Config, Runtime) (freestyleAPI, error) { return client, nil }
	t.Cleanup(func() { newFreestyleClient = oldClient })
	backend := &freestyleBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}

	err := backend.Stop(context.Background(), StopRequest{ID: "fsb_vm123"})
	if err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("Stop error = %v, want exact-claim refusal", err)
	}
	if len(client.deleteIDs) != 0 {
		t.Fatalf("DeleteVM called without an exact claim: %#v", client.deleteIDs)
	}
}

func TestFreestyleStopDeletesExactlyClaimedSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	if err := claimLeaseForRepoProviderPond("fsb_vm123", "web", freestyleProvider, "", root, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	client := &fakeFreestyleClient{}
	oldClient := newFreestyleClient
	newFreestyleClient = func(Config, Runtime) (freestyleAPI, error) { return client, nil }
	t.Cleanup(func() { newFreestyleClient = oldClient })
	backend := &freestyleBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}

	if err := backend.Stop(context.Background(), StopRequest{ID: "web"}); err != nil {
		t.Fatal(err)
	}
	if len(client.deleteIDs) != 1 || client.deleteIDs[0] != "vm123" {
		t.Fatalf("DeleteVM calls = %#v, want vm123", client.deleteIDs)
	}
}

func TestResolveFreestyleLeaseIDClaimsRawSandboxForRepo(t *testing.T) {
	client := &fakeFreestyleClient{getVM: freestyleVM{
		ID:    "vm123",
		Name:  "crabbox-repo-abc123",
		State: "running",
	}}
	oldClaim := claimLeaseForRepoProviderPond
	var gotLeaseID, gotSlug, gotProvider, gotPond, gotRepo string
	var gotIdle time.Duration
	var gotReclaim bool
	claimLeaseForRepoProviderPond = func(leaseID, slug, provider, pond, repo string, idle time.Duration, reclaim bool) error {
		gotLeaseID, gotSlug, gotProvider, gotPond, gotRepo = leaseID, slug, provider, pond, repo
		gotIdle, gotReclaim = idle, reclaim
		return nil
	}
	t.Cleanup(func() { claimLeaseForRepoProviderPond = oldClaim })

	repoRoot := t.TempDir()
	backend := &freestyleBackend{cfg: Config{Pond: "demo", IdleTimeout: 7 * time.Minute}}
	if _, _, err := backend.resolveLeaseID(context.Background(), client, "fsb_vm123", repoRoot, true); err != nil {
		t.Fatal(err)
	}
	if gotLeaseID != "fsb_vm123" || gotSlug == "" || gotProvider != freestyleProvider || gotPond != "demo" || gotRepo != repoRoot || gotIdle != 7*time.Minute || !gotReclaim {
		t.Fatalf("claim args lease=%q slug=%q provider=%q pond=%q repo=%q idle=%s reclaim=%v", gotLeaseID, gotSlug, gotProvider, gotPond, gotRepo, gotIdle, gotReclaim)
	}
}

func TestResolveFreestyleLeaseIDRequiresExplicitReclaimForUnclaimedReuse(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeFreestyleClient{getVM: freestyleVM{
		ID:    "vm123",
		Name:  "crabbox-repo-abc123",
		State: "running",
	}}
	backend := &freestyleBackend{}

	if _, _, err := backend.resolveLeaseID(context.Background(), client, "fsb_vm123", t.TempDir(), false); err == nil || !strings.Contains(err.Error(), "--reclaim") {
		t.Fatalf("resolve error = %v, want explicit reclaim refusal", err)
	}
	if _, ok, err := resolveLeaseClaim("fsb_vm123"); err != nil || ok {
		t.Fatalf("claim created without reclaim: ok=%t err=%v", ok, err)
	}
}

func TestResolveFreestyleLeaseIDAcceptsRawIdentifierWithExactClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	if err := claimLeaseForRepoProviderPond("fsb_vm123", "web", freestyleProvider, "demo", root, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	client := &fakeFreestyleClient{listVMs: []freestyleVM{{
		ID:    "vm123",
		Name:  "crabbox-repo-abc123",
		State: "running",
	}}}
	backend := &freestyleBackend{}

	leaseID, vmID, err := backend.resolveLeaseID(context.Background(), client, "vm123", root, false)
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "fsb_vm123" || vmID != "vm123" {
		t.Fatalf("lease=%q vm=%q", leaseID, vmID)
	}
}

func TestResolveFreestyleLeaseIDAcceptsListedIdentifiers(t *testing.T) {
	vm := freestyleVM{
		ID:    "vm123",
		Name:  "crabbox-repo-abc123",
		State: "running",
	}
	client := &fakeFreestyleClient{listVMs: []freestyleVM{vm}}
	backend := &freestyleBackend{}
	leaseID := freestyleLeasePrefix + vm.ID
	for _, id := range []string{vm.ID, vm.Name, newLeaseSlug(leaseID)} {
		t.Run(id, func(t *testing.T) {
			gotLeaseID, gotVMID, err := backend.resolveLeaseID(context.Background(), client, id, "", false)
			if err != nil {
				t.Fatal(err)
			}
			if gotLeaseID != leaseID || gotVMID != vm.ID {
				t.Fatalf("resolve %q lease=%q vm=%q", id, gotLeaseID, gotVMID)
			}
		})
	}
}

func TestFreestyleWorkspacePathDefaultsUnderWorkspace(t *testing.T) {
	cfg := Config{Freestyle: FreestyleConfig{}}
	if got, err := freestyleWorkspacePath(cfg); err != nil || got != "/workspace/crabbox" {
		t.Fatalf("workspace=%q err=%v", got, err)
	}
	cfg = Config{Freestyle: FreestyleConfig{Workdir: "repo"}}
	if got, err := freestyleWorkspacePath(cfg); err != nil || got != "/workspace/repo" {
		t.Fatalf("workspace=%q err=%v", got, err)
	}
	cfg = Config{Freestyle: FreestyleConfig{Workdir: "team/repo"}}
	if got, err := freestyleWorkspacePath(cfg); err != nil || got != "/workspace/team/repo" {
		t.Fatalf("workspace=%q err=%v", got, err)
	}
}

func TestFreestyleWorkspacePathRejectsEscapes(t *testing.T) {
	for _, workdir := range []string{"/work/repo", "/etc", "../etc", "repo/../../../etc", ".", "./.."} {
		t.Run(workdir, func(t *testing.T) {
			if got, err := freestyleWorkspacePath(Config{Freestyle: FreestyleConfig{Workdir: workdir}}); err == nil {
				t.Fatalf("workspace=%q, want error for workdir %q", got, workdir)
			}
		})
	}
}

func TestFreestyleRunRejectsUnsafeWorkdirBeforeProviderClient(t *testing.T) {
	backend := &freestyleBackend{
		spec: Provider{}.Spec(),
		cfg:  Config{Freestyle: FreestyleConfig{Workdir: "../etc"}},
		rt:   Runtime{Stderr: io.Discard},
	}
	_, err := backend.Run(context.Background(), RunRequest{NoSync: true})
	if err == nil || !strings.Contains(err.Error(), "escapes /workspace") {
		t.Fatalf("Run err=%v, want workdir containment error", err)
	}
}

func TestFreestyleRunRejectsMissingCommand(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeFreestyleClient{createID: "vm123"}
	oldClient := newFreestyleClient
	newFreestyleClient = func(cfg Config, rt Runtime) (freestyleAPI, error) {
		return client, nil
	}
	defer func() { newFreestyleClient = oldClient }()
	backend := &freestyleBackend{
		spec: Provider{}.Spec(),
		cfg:  Config{Freestyle: FreestyleConfig{}},
		rt:   Runtime{Stderr: io.Discard},
	}
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: t.TempDir(), Name: "repo"},
		NoSync:  true,
		Command: nil,
	})
	if err == nil || !strings.Contains(err.Error(), "missing command") {
		t.Fatalf("Run err=%v, want missing command", err)
	}
	if client.createReq != nil || len(client.deleteIDs) != 0 {
		t.Fatalf("missing command created or deleted a VM: create=%#v delete=%#v", client.createReq, client.deleteIDs)
	}
}

func TestFreestyleRunCleansNewSandboxAfterPrepareFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeFreestyleClient{createID: "vm123", execErrAt: 1}
	oldClient := newFreestyleClient
	newFreestyleClient = func(Config, Runtime) (freestyleAPI, error) {
		return client, nil
	}
	t.Cleanup(func() { newFreestyleClient = oldClient })

	backend := &freestyleBackend{
		spec: Provider{}.Spec(),
		cfg:  Config{Freestyle: FreestyleConfig{}},
		rt:   Runtime{Stderr: io.Discard},
	}
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: t.TempDir(), Name: "repo"},
		NoSync:  true,
		Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "exec failed") {
		t.Fatalf("Run err=%v, want prepare failure", err)
	}
	if len(client.deleteIDs) != 1 || client.deleteIDs[0] != "vm123" {
		t.Fatalf("deleteIDs=%#v want vm123", client.deleteIDs)
	}
	if result.Session == nil {
		t.Fatal("session=nil")
	}
	if result.Session.Provider != freestyleProvider || result.Session.LeaseID != "fsb_vm123" || result.Session.Reused || result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	if !strings.Contains(result.Session.CleanupCommand, "crabbox stop --provider freestyle --id") {
		t.Fatalf("cleanup command=%q", result.Session.CleanupCommand)
	}
}

func TestFreestyleRunKeepsNewSandboxAfterPrepareFailureWhenRequested(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeFreestyleClient{createID: "vm123", execErrAt: 1}
	oldClient := newFreestyleClient
	newFreestyleClient = func(Config, Runtime) (freestyleAPI, error) {
		return client, nil
	}
	t.Cleanup(func() { newFreestyleClient = oldClient })

	var stderr bytes.Buffer
	backend := &freestyleBackend{
		spec: Provider{}.Spec(),
		cfg: Config{
			IdleTimeout: 5 * time.Minute,
			TTL:         time.Hour,
			Freestyle:   FreestyleConfig{},
		},
		rt: Runtime{Stderr: &stderr},
	}
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:          Repo{Root: t.TempDir(), Name: "repo"},
		NoSync:        true,
		KeepOnFailure: true,
		Command:       []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "exec failed") {
		t.Fatalf("Run err=%v, want prepare failure", err)
	}
	if len(client.deleteIDs) != 0 {
		t.Fatalf("deleteIDs=%#v want kept VM", client.deleteIDs)
	}
	if !strings.Contains(stderr.String(), "keep-on-failure: kept lease=fsb_vm123") {
		t.Fatalf("stderr=%q, want keep-on-failure hint", stderr.String())
	}
	if result.Session == nil {
		t.Fatal("session=nil")
	}
	if result.Session.Provider != freestyleProvider || result.Session.LeaseID != "fsb_vm123" || result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	if result.Session.CleanupCommand == "" {
		t.Fatal("cleanup command is empty")
	}
}

func TestFreestyleRunSyncOnlySkipsUserExec(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(root+"/go.mod", []byte("module example.test/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	client := &fakeFreestyleClient{createID: "vm-sync"}
	oldClient := newFreestyleClient
	newFreestyleClient = func(cfg Config, rt Runtime) (freestyleAPI, error) {
		return client, nil
	}
	defer func() { newFreestyleClient = oldClient }()
	var stdout bytes.Buffer
	backend := &freestyleBackend{
		spec: Provider{}.Spec(),
		cfg:  Config{Freestyle: FreestyleConfig{}},
		rt:   Runtime{Stdout: &stdout, Stderr: io.Discard},
	}
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:     Repo{Root: root, Name: "repo"},
		SyncOnly: true,
		Command:  []string{"printf", "unexpected-user-command"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "synced /workspace/crabbox") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	for _, command := range client.execCommands {
		if strings.Contains(command, "unexpected-user-command") {
			t.Fatalf("unexpected user exec: %q", command)
		}
	}
}

func TestFreestyleRunNoSyncDoesNotDeleteExistingWorkspace(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeFreestyleClient{getVM: freestyleVM{
		ID:    "vm123",
		Name:  "crabbox-repo-abc123",
		State: "running",
	}}
	oldClient := newFreestyleClient
	newFreestyleClient = func(cfg Config, rt Runtime) (freestyleAPI, error) {
		return client, nil
	}
	defer func() { newFreestyleClient = oldClient }()
	backend := &freestyleBackend{
		spec: Provider{}.Spec(),
		cfg: Config{
			Freestyle: FreestyleConfig{},
			Sync:      SyncConfig{Delete: true},
		},
		rt: Runtime{Stderr: io.Discard},
	}
	result, err := backend.Run(context.Background(), RunRequest{
		ID:      "fsb_vm123",
		Repo:    Repo{Root: t.TempDir(), Name: "repo"},
		Reclaim: true,
		NoSync:  true,
		Command: []string{"test", "-f", "kept.txt"},
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if len(client.execCommands) != 2 {
		t.Fatalf("exec commands=%#v want prepare and user command", client.execCommands)
	}
	prepare := client.execCommands[0]
	if strings.Contains(prepare, "rm -rf") {
		t.Fatalf("--no-sync prepare deleted workspace: %q", prepare)
	}
	if !strings.Contains(prepare, "mkdir -p") {
		t.Fatalf("--no-sync prepare should ensure workspace: %q", prepare)
	}
	if result.Session == nil {
		t.Fatal("session=nil")
	}
	if result.Session.Provider != freestyleProvider || result.Session.LeaseID != "fsb_vm123" || !result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	if result.Session.CleanupCommand == "" {
		t.Fatal("cleanup command is empty")
	}
}

func TestFreestyleRunCleanupFailureReportsRetainedSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeFreestyleClient{
		createID:  "vm123",
		deleteErr: errors.New("delete failed"),
	}
	oldClient := newFreestyleClient
	newFreestyleClient = func(Config, Runtime) (freestyleAPI, error) {
		return client, nil
	}
	t.Cleanup(func() { newFreestyleClient = oldClient })

	var stderr bytes.Buffer
	backend := &freestyleBackend{
		spec: Provider{}.Spec(),
		cfg:  Config{Freestyle: FreestyleConfig{}},
		rt:   Runtime{Stderr: &stderr},
	}
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: t.TempDir(), Name: "repo"},
		NoSync:  true,
		Command: []string{"true"},
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if len(client.deleteIDs) != 1 || client.deleteIDs[0] != "vm123" {
		t.Fatalf("deleteIDs=%#v want vm123", client.deleteIDs)
	}
	if !strings.Contains(stderr.String(), "warning: freestyle stop failed for vm123") {
		t.Fatalf("stderr=%q, want cleanup warning", stderr.String())
	}
	if result.Session == nil {
		t.Fatal("session=nil")
	}
	if result.Session.Provider != freestyleProvider || result.Session.LeaseID != "fsb_vm123" || result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	if result.Session.CleanupCommand != "crabbox stop --provider freestyle --id 'fsb_vm123'" {
		t.Fatalf("cleanup command=%q", result.Session.CleanupCommand)
	}
}

func TestFreestyleRunPrintsRedactedEnvSummary(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeFreestyleClient{getVM: freestyleVM{
		ID:    "vm123",
		Name:  "crabbox-repo-abc123",
		State: "running",
	}}
	oldClient := newFreestyleClient
	newFreestyleClient = func(cfg Config, rt Runtime) (freestyleAPI, error) {
		return client, nil
	}
	defer func() { newFreestyleClient = oldClient }()
	var stderr bytes.Buffer
	backend := &freestyleBackend{
		spec: Provider{}.Spec(),
		cfg:  Config{Freestyle: FreestyleConfig{}},
		rt:   Runtime{Stdout: io.Discard, Stderr: &stderr},
	}
	_, err := backend.Run(context.Background(), RunRequest{
		ID:         "fsb_vm123",
		Repo:       Repo{Root: t.TempDir(), Name: "repo"},
		Reclaim:    true,
		NoSync:     true,
		Command:    []string{"printenv", "SECRET_TOKEN"},
		Env:        map[string]string{"SECRET_TOKEN": "super-secret"},
		EnvSummary: true,
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if strings.Contains(stderr.String(), "super-secret") {
		t.Fatalf("secret leaked in stderr: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "SECRET_TOKEN=set len=12 secret=true") {
		t.Fatalf("missing redacted env summary: %s", stderr.String())
	}
}

func TestFreestyleCreateSandboxWorksWithoutWorkdir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeFreestyleClient{createID: "vm123"}
	backend := &freestyleBackend{
		cfg: Config{Freestyle: FreestyleConfig{}},
		rt:  Runtime{Stderr: io.Discard},
	}
	leaseID, id, slug, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "fsb_vm123" {
		t.Fatalf("leaseID=%q", leaseID)
	}
	if id != "vm123" {
		t.Fatalf("id=%q", id)
	}
	if slug == "" {
		t.Fatal("slug is empty")
	}
	if client.createReq == nil {
		t.Fatal("create request was nil")
	}
	if client.createReq.Template != nil {
		t.Fatalf("create template=%#v want omitted", client.createReq.Template)
	}
	if client.createReq.Ports == nil || len(client.createReq.Ports) != 0 {
		t.Fatalf("create ports=%#v want explicit empty array", client.createReq.Ports)
	}
}

func TestFreestyleCreateSandboxPassesNameWithoutWorkdir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeFreestyleClient{createID: "vm456"}
	backend := &freestyleBackend{
		cfg: Config{Freestyle: FreestyleConfig{VCPUs: 4, MemoryGB: 8}},
		rt:  Runtime{Stderr: io.Discard},
	}
	_, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if client.createReq == nil {
		t.Fatal("create request was nil")
	}
	if !strings.HasPrefix(client.createReq.Name, "crabbox-repo-") {
		t.Fatalf("name=%q", client.createReq.Name)
	}
	if client.createReq.Template == nil || client.createReq.Template.VcpuCount != 4 || client.createReq.Template.MemSizeGb != 8 {
		t.Fatalf("create template=%#v", client.createReq.Template)
	}
	if client.createReq.Ports == nil || len(client.createReq.Ports) != 0 {
		t.Fatalf("create ports=%#v want explicit empty array", client.createReq.Ports)
	}
}

func TestFreestyleCreateSandboxStoresClaimForList(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeFreestyleClient{createID: "vm789"}
	backend := &freestyleBackend{
		cfg: Config{Pond: "Alpha Pond", Freestyle: FreestyleConfig{}},
		rt:  Runtime{Stderr: io.Discard},
	}
	_, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveLeaseClaim("fsb_vm789")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("claim not found for fsb_vm789")
	}
	if claim.Provider != "freestyle" {
		t.Fatalf("claim provider=%q", claim.Provider)
	}
	if claim.Pond != "alpha-pond" {
		t.Fatalf("claim pond=%q want alpha-pond", claim.Pond)
	}
}

func TestFreestyleListAndStatusUseStoredClaimSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeFreestyleClient{createID: "vm789"}
	backend := &freestyleBackend{
		cfg: Config{Pond: "demo", Freestyle: FreestyleConfig{}},
		rt:  Runtime{Stderr: io.Discard},
	}
	leaseID, id, slug, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "blue-lobster")
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "fsb_vm789" || id != "vm789" || slug != "blue-lobster" {
		t.Fatalf("lease=%q id=%q slug=%q", leaseID, id, slug)
	}
	vm := freestyleVM{ID: "vm789", Name: "crabbox-repo-abc123", State: "running"}
	server := freestyleVMToServer(vm)
	if server.Labels["slug"] != "blue-lobster" || server.Labels["pond"] != "demo" {
		t.Fatalf("list labels=%v", server.Labels)
	}
	status := freestyleStatusView(leaseID, vm)
	if status.Slug != "blue-lobster" || status.Labels["slug"] != "blue-lobster" || status.Labels["pond"] != "demo" {
		t.Fatalf("status=%#v labels=%v", status, status.Labels)
	}
}

func TestFreestyleCreateSandboxReportsBoundedCleanupFailure(t *testing.T) {
	oldClaim := claimLeaseForRepoProviderPond
	claimLeaseForRepoProviderPond = func(_, _, _, _, _ string, _ time.Duration, _ bool) error {
		return errors.New("claim write failed")
	}
	t.Cleanup(func() { claimLeaseForRepoProviderPond = oldClaim })

	client := &fakeFreestyleClient{
		createID:  "vm-leaked",
		deleteErr: errors.New("delete failed"),
	}
	var stderr bytes.Buffer
	backend := &freestyleBackend{
		cfg: Config{Freestyle: FreestyleConfig{}},
		rt:  Runtime{Stderr: &stderr},
	}
	_, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "")
	if err == nil {
		t.Fatal("expected claim failure")
	}
	for _, want := range []string{
		"claim write failed",
		"cleanup freestyle vm vm-leaked",
		"delete failed",
		"crabbox run --provider freestyle --id 'fsb_vm-leaked' --reclaim --no-sync -- true",
		"crabbox stop --provider freestyle --id 'fsb_vm-leaked'",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%v, want %q", err, want)
		}
	}
	if len(client.deleteIDs) != 1 || client.deleteIDs[0] != "vm-leaked" {
		t.Fatalf("deleteIDs=%#v want vm-leaked", client.deleteIDs)
	}
	if !client.deleteDeadlineSet {
		t.Fatal("cleanup delete did not use a bounded context")
	}
	if !strings.Contains(stderr.String(), "warning: cleanup freestyle vm vm-leaked") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestFreestyleSyncWorkspaceUploadsRepoArchive(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	root := t.TempDir()
	if err := os.WriteFile(root+"/go.mod", []byte("module example.test/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	client := &fakeFreestyleClient{}
	backend := &freestyleBackend{
		cfg: Config{Freestyle: FreestyleConfig{Workdir: "repo"}},
		rt:  Runtime{Stderr: io.Discard},
	}
	_, _, err := backend.syncWorkspace(context.Background(), client, "crabbox-test", RunRequest{
		Repo: Repo{Root: root, Name: "repo"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if client.writeFilePath != "/tmp/crabbox-" {
		if !strings.HasPrefix(client.writeFilePath, "/tmp/crabbox-") || !strings.HasSuffix(client.writeFilePath, ".tgz.api") {
			t.Fatalf("write file path=%q", client.writeFilePath)
		}
	}
	if client.writeFileEncoding != "base64" {
		t.Fatalf("write file encoding=%q", client.writeFileEncoding)
	}
	if !client.commandContains("mkdir") || !client.commandContains("/workspace/repo") {
		t.Fatalf("prepare commands=%#v", client.prepareCommands)
	}
}

func TestFreestyleSyncWorkspaceValidatesArchiveBeforeDeletingWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "missing.txt"), []byte("tracked"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cmd := exec.Command("git", "init")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "add", "missing.txt")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	trackedPath := filepath.Join(root, "missing.txt")
	calls := 0
	client := &fakeFreestyleClient{}
	backend := &freestyleBackend{
		cfg: Config{
			Freestyle: FreestyleConfig{Workdir: "repo"},
			Sync:      SyncConfig{Delete: true},
		},
		rt: Runtime{Stderr: io.Discard, Clock: freestyleArchiveClock(func() time.Time {
			calls++
			if calls == 5 {
				if err := os.Remove(trackedPath); err != nil {
					t.Fatal(err)
				}
			}
			return time.Unix(0, int64(calls)*int64(time.Millisecond))
		})},
	}

	if _, _, err := backend.syncWorkspace(context.Background(), client, "crabbox-test", RunRequest{
		Repo: Repo{Root: root, Name: "repo"},
	}, nil); err == nil {
		t.Fatal("syncWorkspace err=nil, want local archive failure")
	}
	if len(client.prepareCommands) != 0 {
		t.Fatalf("remote commands=%#v, want no workspace mutation before archive validation", client.prepareCommands)
	}
}

func TestFreestyleSyncWorkspaceHonorsIncludes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "skip"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep", "file.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "skip", "file.txt"), []byte("skip"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "add", ".")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	client := &fakeFreestyleClient{}
	backend := &freestyleBackend{
		cfg: Config{
			Freestyle: FreestyleConfig{Workdir: "repo"},
			Sync:      SyncConfig{Includes: []string{"keep/**"}},
		},
		rt: Runtime{Stderr: io.Discard},
	}
	_, _, err := backend.syncWorkspace(context.Background(), client, "crabbox-test", RunRequest{
		Repo: Repo{Root: root, Name: "repo"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := base64.StdEncoding.DecodeString(client.writeFileContent)
	if err != nil {
		t.Fatal(err)
	}
	if !tarGzipContains(t, archive, "keep/file.txt") {
		t.Fatal("archive missing included file")
	}
	if tarGzipContains(t, archive, "skip/file.txt") {
		t.Fatal("archive contains file outside sync.include")
	}
}

func TestFreestyleSyncWorkspaceFallsBackToExecUpload(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	root := t.TempDir()
	if err := os.WriteFile(root+"/go.mod", []byte("module example.test/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	client := &fakeFreestyleClient{writeFileErr: errors.New("file api upload failed")}
	backend := &freestyleBackend{
		cfg: Config{Freestyle: FreestyleConfig{Workdir: "repo"}},
		rt:  Runtime{Stderr: io.Discard},
	}
	_, _, err := backend.syncWorkspace(context.Background(), client, "crabbox-test", RunRequest{
		Repo: Repo{Root: root, Name: "repo"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !client.commandContains("base64 -d") || !client.commandContains("tar -xzf") {
		t.Fatalf("fallback commands=%#v", client.prepareCommands)
	}
}

func TestFreestyleSyncDeleteStagesBeforeReplacingWorkspace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	client := &fakeFreestyleClient{}
	backend := &freestyleBackend{
		cfg: Config{
			Freestyle: FreestyleConfig{Workdir: "repo"},
			Sync:      SyncConfig{Delete: true},
		},
		rt: Runtime{Stderr: io.Discard},
	}
	if _, _, err := backend.syncWorkspace(context.Background(), client, "vm123", RunRequest{
		Repo: Repo{Root: root, Name: "repo"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	extractIndex, replaceIndex := -1, -1
	for i, command := range client.execCommands {
		if strings.Contains(command, "rm -rf '/workspace/repo'") {
			t.Fatalf("sync deleted live workspace directly: %q", command)
		}
		if strings.Contains(command, "tar -xzf") && strings.Contains(command, ".repo.crabbox-sync-") {
			extractIndex = i
		}
		if strings.Contains(command, "if mv ") && strings.Contains(command, "'/workspace/repo'") {
			replaceIndex = i
		}
	}
	if extractIndex < 0 || replaceIndex <= extractIndex {
		t.Fatalf("commands=%#v, want staged extract before replacement", client.execCommands)
	}
}

func TestFreestyleSyncDeletePreservesWorkspaceWhenFallbackUploadFails(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	client := &fakeFreestyleClient{
		writeFileErr: errors.New("file api upload failed"),
		exec: func(_ context.Context, command string) (int, error) {
			if strings.Contains(command, "printf %s") {
				return 1, errors.New("exec failed")
			}
			return 0, nil
		},
	}
	backend := &freestyleBackend{
		cfg: Config{
			Freestyle: FreestyleConfig{Workdir: "repo"},
			Sync:      SyncConfig{Delete: true},
		},
		rt: Runtime{Stderr: io.Discard},
	}
	if _, _, err := backend.syncWorkspace(context.Background(), client, "vm123", RunRequest{
		Repo: Repo{Root: root, Name: "repo"},
	}, nil); err == nil || !strings.Contains(err.Error(), "exec failed") {
		t.Fatalf("syncWorkspace err=%v, want fallback upload failure", err)
	}
	for _, command := range client.execCommands {
		if strings.Contains(command, "if mv ") || strings.Contains(command, "rm -rf '/workspace/repo'") {
			t.Fatalf("failed sync touched live workspace: %q", command)
		}
	}
}

func TestFreestyleSyncHonorsConfiguredTimeout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	backend := &freestyleBackend{
		cfg: Config{
			Freestyle: FreestyleConfig{Workdir: "repo"},
			Sync:      SyncConfig{Timeout: 100 * time.Millisecond},
		},
		rt: Runtime{Stderr: io.Discard},
	}
	// Local Git manifest queries and archive I/O finish before the fake transfer
	// blocks. Measure cancellation in virtual time, independent of their wall time.
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		transferred := false
		client := &fakeFreestyleClient{writeFile: func(ctx context.Context) error {
			transferred = true
			deadline, ok := ctx.Deadline()
			if !ok || deadline.Sub(started) != backend.cfg.Sync.Timeout {
				t.Fatalf("transfer deadline=%v set=%t, want start + %s", deadline, ok, backend.cfg.Sync.Timeout)
			}
			if err := ctx.Err(); err != nil {
				t.Fatalf("transfer context already canceled: %v", err)
			}
			time.Sleep(time.Until(deadline) - time.Nanosecond)
			synctest.Wait()
			select {
			case <-ctx.Done():
				t.Fatal("transfer canceled before configured timeout")
			default:
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			select {
			case <-ctx.Done():
			default:
				t.Fatal("transfer not canceled at configured timeout")
			}
			if err := ctx.Err(); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("transfer context error=%v, want deadline exceeded", err)
			}
			return ctx.Err()
		}}
		if _, _, err := backend.syncWorkspace(context.Background(), client, "vm123", RunRequest{
			Repo: Repo{Root: root, Name: "repo"},
		}, nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("syncWorkspace err=%v, want timeout", err)
		}
		if !transferred {
			t.Fatal("sync did not reach the transfer timeout contract")
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("syncWorkspace took %s, timeout should bound transfer", elapsed)
		}
	})
}

func TestFreestyleUploadCleansAttemptFilesAfterFailure(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%t", fallback), func(t *testing.T) {
			client := &fakeFreestyleClient{execErrAt: 1}
			if fallback {
				client.writeFileErr = errors.New("file api upload failed")
				client.execErrAt = 2
			}
			backend := &freestyleBackend{rt: Runtime{Stderr: io.Discard}}
			payload := []byte("synthetic payload")
			err := backend.uploadArchive(t.Context(), client, "vm123", "/tmp/archive.tgz", bytes.NewReader(payload))
			if err == nil || !strings.Contains(err.Error(), "exec failed") {
				t.Fatalf("err=%v", err)
			}
			if strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(payload)) || strings.Contains(err.Error(), "printf") {
				t.Fatalf("payload in error=%v", err)
			}
			last := client.execCommands[len(client.execCommands)-1]
			for _, path := range []string{"/tmp/archive.tgz.api", "/tmp/archive.tgz.exec.b64", "/tmp/archive.tgz.exec"} {
				if !strings.Contains(last, path) {
					t.Fatalf("cleanup missing %s: %s", path, last)
				}
			}
			if !strings.Contains(last, "rm -f") || client.commandContains("tar -xzf") {
				t.Fatalf("upload commands=%v", client.execCommands)
			}
		})
	}
}

func TestReadFreestyleArchiveForUploadRejectsOversize(t *testing.T) {
	data := bytes.Repeat([]byte("x"), maxFreestyleArchiveUploadBytes+1)
	if _, err := readFreestyleArchiveForUpload(bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err=%v, want archive size rejection", err)
	}
}

func TestRejectFreestyleSyncOptionsAllowsForceSyncLarge(t *testing.T) {
	spec := Provider{}.Spec()
	if err := delegatedSyncOptionsError(spec, RunRequest{ForceSyncLarge: true}); err != nil {
		t.Fatalf("force sync large should be honored by Freestyle archive sync: %v", err)
	}
	if err := delegatedSyncOptionsError(spec, RunRequest{SyncOnly: true}); err != nil {
		t.Fatalf("sync-only should be supported: %v", err)
	}
	if err := delegatedSyncOptionsError(spec, RunRequest{ChecksumSync: true}); err == nil || !strings.Contains(err.Error(), "--checksum") {
		t.Fatalf("checksum err=%v", err)
	}
}

func TestNewFreestyleSandboxNameUsesCrabboxPrefix(t *testing.T) {
	name := newFreestyleSandboxName(Repo{Name: "repo"})
	if !strings.HasPrefix(name, "crabbox-repo-") {
		t.Fatalf("name=%q", name)
	}
	if !isCrabboxFreestyleSandboxName(name) {
		t.Fatalf("expected %q to be recognized as Crabbox-owned", name)
	}
}

func TestFreestyleOwnershipRequiresCanonicalGeneratedName(t *testing.T) {
	for _, name := range []string{"crabbox repo abc123", "crabbox/repo/abc123", "crabbox?repo=abc123", "Crabbox-repo-abc123"} {
		if isCrabboxFreestyleSandboxName(name) {
			t.Fatalf("malformed provider name %q recognized as Crabbox-owned", name)
		}
	}
}

type fakeFreestyleClient struct {
	createID          string
	createReq         *freestyleCreateVMRequest
	create            func() error
	getVM             freestyleVM
	getVMErr          error
	listVMs           []freestyleVM
	listVMsErr        error
	prepareCommands   []string
	writeFilePath     string
	writeFileContent  string
	writeFileEncoding string
	writeFileErr      error
	writeFile         func(context.Context) error
	execCommands      []string
	deleteIDs         []string
	deleteErr         error
	deleteDeadlineSet bool
	execCalls         int
	execErrAt         int
	exec              func(context.Context, string) (int, error)
}

func (f *fakeFreestyleClient) CreateVM(_ context.Context, req freestyleCreateVMRequest) (freestyleVM, error) {
	f.createReq = &req
	if f.create != nil {
		if err := f.create(); err != nil {
			return freestyleVM{}, err
		}
	}
	id := f.createID
	if id == "" {
		id = "vm-test-abcdef"
	}
	return freestyleVM{ID: id, State: "running"}, nil
}

func (f *fakeFreestyleClient) GetVM(_ context.Context, id string) (freestyleVM, error) {
	if f.getVMErr != nil {
		return freestyleVM{}, f.getVMErr
	}
	if f.getVM.ID != "" || f.getVM.Name != "" || f.getVM.State != "" {
		return f.getVM, nil
	}
	return freestyleVM{ID: id, State: "running"}, nil
}

func (f *fakeFreestyleClient) ListVMs(_ context.Context) ([]freestyleVM, error) {
	return f.listVMs, f.listVMsErr
}

func (f *fakeFreestyleClient) DeleteVM(ctx context.Context, id string) error {
	f.deleteIDs = append(f.deleteIDs, id)
	_, f.deleteDeadlineSet = ctx.Deadline()
	return f.deleteErr
}

func (f *fakeFreestyleClient) Exec(ctx context.Context, _ string, command string, _, _ io.Writer) (int, error) {
	f.execCalls++
	f.execCommands = append(f.execCommands, command)
	f.prepareCommands = append(f.prepareCommands, command)
	if f.exec != nil {
		return f.exec(ctx, command)
	}
	if err := ctx.Err(); err != nil {
		return 1, err
	}
	if f.execCalls == f.execErrAt {
		return 1, errors.New("exec failed")
	}
	return 0, nil
}

func (f *fakeFreestyleClient) WriteFile(ctx context.Context, _ string, path, content, encoding string) error {
	f.writeFilePath = path
	f.writeFileContent = content
	f.writeFileEncoding = encoding
	if f.writeFile != nil {
		return f.writeFile(ctx)
	}
	if f.writeFileErr != nil {
		return f.writeFileErr
	}
	return nil
}

func (f *fakeFreestyleClient) ReadFile(_ context.Context, _, _ string) (string, error) {
	return "", nil
}

func (f *fakeFreestyleClient) commandContains(value string) bool {
	for _, command := range f.prepareCommands {
		if strings.Contains(command, value) {
			return true
		}
	}
	return false
}

func tarGzipContains(t *testing.T, data []byte, name string) bool {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == name {
			return true
		}
	}
}

func freestyleArchiveRepo(t *testing.T) Repo {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"tracked.txt", "other.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=Synthetic Proof", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return Repo{Root: root, Name: "repo"}
}

func TestFreestyleRunBoundsFullArchiveBeforeMutation(t *testing.T) {
	for _, reused := range []bool{false, true} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("reused=%t/force=%t", reused, force), func(t *testing.T) {
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				temp := t.TempDir()
				t.Setenv("TMPDIR", temp)
				t.Setenv("TMP", temp)
				t.Setenv("TEMP", temp)
				repo := freestyleArchiveRepo(t)
				if err := os.WriteFile(filepath.Join(repo.Root, "tracked.txt"), []byte("dirty"), 0600); err != nil {
					t.Fatal(err)
				}
				client := &fakeFreestyleClient{getVM: freestyleVM{ID: "vm123", Name: "crabbox-repo-abc123", State: "running"}}
				old := newFreestyleClient
				newFreestyleClient = func(Config, Runtime) (freestyleAPI, error) { return client, nil }
				t.Cleanup(func() { newFreestyleClient = old })
				b := &freestyleBackend{spec: Provider{}.Spec(), cfg: Config{Sync: SyncConfig{FailFiles: 2, Delete: true}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
				req := RunRequest{Repo: repo, SyncOnly: true, ForceSyncLarge: force}
				if reused {
					req.ID = "fsb_vm123"
					req.Reclaim = true
				}
				_, err := b.Run(context.Background(), req)
				if force {
					if err != nil || client.writeFileContent == "" {
						t.Fatalf("forced archive not uploaded: %v", err)
					}
				} else {
					if err == nil || !strings.Contains(err.Error(), "sync candidate too large") {
						t.Fatalf("full archive admitted or wrong failure: %v", err)
					}
					if client.createReq != nil || len(client.execCommands) != 0 || client.writeFileContent != "" {
						t.Fatalf("mutation before guardrail: create=%t exec=%d upload=%t", client.createReq != nil, len(client.execCommands), client.writeFileContent != "")
					}
				}
				paths, err := filepath.Glob(filepath.Join(temp, "crabbox-freestyle-sync-*.tgz"))
				if err != nil || len(paths) != 0 {
					t.Fatalf("local archive residue=%v err=%v", paths, err)
				}
			})
		}
	}
}

func TestFreestyleRunCompressedCapPrecedesCreateEvenWhenForced(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	t.Setenv("TMP", temp)
	t.Setenv("TEMP", temp)
	repo := freestyleArchiveRepo(t)
	file, err := os.Create(filepath.Join(repo.Root, "incompressible.bin"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.CopyN(file, rand.Reader, int64(maxFreestyleArchiveUploadBytes)+128*1024)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("fixture write=%v close=%v", err, closeErr)
	}
	client := &fakeFreestyleClient{}
	old := newFreestyleClient
	newFreestyleClient = func(Config, Runtime) (freestyleAPI, error) { return client, nil }
	t.Cleanup(func() { newFreestyleClient = old })
	b := &freestyleBackend{spec: Provider{}.Spec(), rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	_, err = b.Run(t.Context(), RunRequest{Repo: repo, SyncOnly: true, ForceSyncLarge: true})
	if err == nil || !strings.Contains(err.Error(), "after compression") {
		t.Fatalf("compressed cap failure=%v", err)
	}
	if client.createReq != nil || len(client.execCommands) != 0 || client.writeFileContent != "" {
		t.Fatalf("provider mutation before compressed cap: create=%t exec=%d", client.createReq != nil, len(client.execCommands))
	}
	paths, err := filepath.Glob(filepath.Join(temp, "crabbox-freestyle-sync-*.tgz"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("archive residue=%v err=%v", paths, err)
	}
}

type freestyleArchiveClock func() time.Time

func (now freestyleArchiveClock) Now() time.Time { return now() }

func TestFreestyleRunPreparesSnapshotBeforeCreate(t *testing.T) {
	for _, createFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("createFails=%t", createFails), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			temp := t.TempDir()
			t.Setenv("TMPDIR", temp)
			t.Setenv("TMP", temp)
			t.Setenv("TEMP", temp)
			repo := freestyleArchiveRepo(t)
			var preparedPath string
			client := &fakeFreestyleClient{create: func() error {
				paths, err := filepath.Glob(filepath.Join(temp, "crabbox-freestyle-sync-*.tgz"))
				if err != nil || len(paths) != 1 {
					t.Fatalf("archive not prepared before create: paths=%v err=%v", paths, err)
				}
				preparedPath = paths[0]
				if createFails {
					return errors.New("synthetic create failure")
				}
				return os.WriteFile(filepath.Join(repo.Root, "tracked.txt"), []byte("changed during create"), 0600)
			}}
			old := newFreestyleClient
			newFreestyleClient = func(Config, Runtime) (freestyleAPI, error) { return client, nil }
			t.Cleanup(func() { newFreestyleClient = old })
			b := &freestyleBackend{spec: Provider{}.Spec(), rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
			_, err := b.Run(t.Context(), RunRequest{Repo: repo, SyncOnly: true})
			if createFails {
				if err == nil || !strings.Contains(err.Error(), "synthetic create failure") || client.writeFileContent != "" {
					t.Fatalf("create failure err=%v uploaded=%t", err, client.writeFileContent != "")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				data, err := base64.StdEncoding.DecodeString(client.writeFileContent)
				if err != nil {
					t.Fatal(err)
				}
				gz, err := gzip.NewReader(bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				defer gz.Close()
				tr := tar.NewReader(gz)
				for {
					header, err := tr.Next()
					if err != nil {
						t.Fatalf("prepared file missing: %v", err)
					}
					if header.Name != "tracked.txt" {
						continue
					}
					data, err := io.ReadAll(tr)
					if err != nil || string(data) != "original" {
						t.Fatalf("snapshot=%q err=%v", data, err)
					}
					break
				}
			}
			if _, err := os.Stat(preparedPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("prepared archive retained after Run: %v", err)
			}
		})
	}
}

func TestFreestyleUploadNativeAttemptIsolation(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("native POSIX shell fixture")
	}
	for _, decodeFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("decodeFails=%t", decodeFails), func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "archive.tgz")
			faults := filepath.Join(dir, "faults.sh")
			faultSource := ""
			if decodeFails {
				faultSource = "base64() { printf partial; return 51; }\n"
			}
			if err := os.WriteFile(faults, []byte(faultSource), 0600); err != nil {
				t.Fatal(err)
			}
			payload := bytes.Repeat([]byte("synthetic archive bytes"), 5000)
			client := &fakeFreestyleClient{}
			client.writeFile = func(context.Context) error {
				if err := os.WriteFile(client.writeFilePath, []byte("partial"), 0600); err != nil {
					return err
				}
				return errors.New("synthetic file API failure")
			}
			chunks := 0
			client.exec = func(ctx context.Context, source string) (int, error) {
				cmd := exec.CommandContext(ctx, "/bin/sh", "-c", source)
				cmd.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin", "BASH_ENV=" + faults}
				out, err := cmd.CombinedOutput()
				if strings.Contains(source, "printf %s") {
					chunks++
					if chunks == 1 {
						if err := os.WriteFile(client.writeFilePath, []byte("late API write"), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					return exitErr.ExitCode(), nil
				}
				if err != nil {
					return 1, fmt.Errorf("native fixture: %w: %s", err, out)
				}
				return 0, nil
			}
			b := &freestyleBackend{rt: Runtime{Stderr: io.Discard}}
			err := b.uploadArchive(t.Context(), client, "fixture", archive, bytes.NewReader(payload))
			if decodeFails {
				var exitErr core.ExitError
				if !errors.As(err, &exitErr) || exitErr.Code != 51 {
					t.Fatalf("decode failure=%v", err)
				}
				if _, err := os.Stat(archive); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("partial decode published: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(archive)
				if err != nil || !bytes.Equal(data, payload) {
					t.Fatalf("fallback bytes changed: %v", err)
				}
			}
			if chunks != 3 {
				t.Fatalf("chunk count=%d", chunks)
			}
			for _, suffix := range []string{".api", ".exec.b64", ".exec"} {
				if _, err := os.Stat(archive + suffix); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("attempt residue %s: %v", suffix, err)
				}
			}
		})
	}
}
