package freestyle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "synced /workspace/crabbox") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	for _, command := range client.execCommands {
		if strings.Contains(command, "bash -lc") && !strings.Contains(command, "mkdir") && !strings.Contains(command, "tar") && !strings.Contains(command, "base64") && !strings.Contains(command, "printf") && !strings.Contains(command, "rm -f") {
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
		"crabbox stop --provider freestyle --id fsb_vm-leaked",
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
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.writeFilePath != "/tmp/crabbox-" {
		if !strings.HasPrefix(client.writeFilePath, "/tmp/crabbox-") || !strings.HasSuffix(client.writeFilePath, ".tgz") {
			t.Fatalf("write file path=%q", client.writeFilePath)
		}
	}
	if client.writeFileEncoding != "base64" {
		t.Fatalf("write file encoding=%q", client.writeFileEncoding)
	}
	if len(client.prepareCommands) < 1 || !strings.Contains(client.prepareCommands[0], "mkdir") || !strings.Contains(client.prepareCommands[0], "/workspace/repo") {
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
	if err := os.Chmod(trackedPath, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(trackedPath, 0o644) })
	client := &fakeFreestyleClient{}
	backend := &freestyleBackend{
		cfg: Config{
			Freestyle: FreestyleConfig{Workdir: "repo"},
			Sync:      SyncConfig{Delete: true},
		},
		rt: Runtime{Stderr: io.Discard},
	}

	if _, _, err := backend.syncWorkspace(context.Background(), client, "crabbox-test", RunRequest{
		Repo: Repo{Root: root, Name: "repo"},
	}); err == nil {
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
	})
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
	})
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
	}); err != nil {
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
		execErrAt:    4,
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
	}); err == nil || !strings.Contains(err.Error(), "exec failed") {
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
	client := &fakeFreestyleClient{writeFileBlock: true}
	backend := &freestyleBackend{
		cfg: Config{
			Freestyle: FreestyleConfig{Workdir: "repo"},
			Sync:      SyncConfig{Timeout: 100 * time.Millisecond},
		},
		rt: Runtime{Stderr: io.Discard},
	}
	started := time.Now()
	if _, _, err := backend.syncWorkspace(context.Background(), client, "vm123", RunRequest{
		Repo: Repo{Root: root, Name: "repo"},
	}); err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("syncWorkspace err=%v, want timeout", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("syncWorkspace took %s, timeout should bound transfer", elapsed)
	}
}

func TestFreestyleFallbackUploadCleansPartialArchiveAfterChunkFailure(t *testing.T) {
	client := &fakeFreestyleClient{execErrAt: 2}
	backend := &freestyleBackend{rt: Runtime{Stderr: io.Discard}}
	payload := []byte("secret payload")
	err := backend.uploadArchiveViaExec(context.Background(), client, "vm123", "/workspace/repo", payload)
	if err == nil || !strings.Contains(err.Error(), "exec failed") {
		t.Fatalf("err=%v, want chunk upload failure", err)
	}
	if strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(payload)) || strings.Contains(err.Error(), "printf") {
		t.Fatalf("err=%v contains archive payload command", err)
	}
	if len(client.execCommands) != 3 {
		t.Fatalf("commands=%#v, want initial cleanup, failed chunk, rollback cleanup", client.execCommands)
	}
	if client.execCommands[0] != client.execCommands[2] || !strings.Contains(client.execCommands[2], "rm -f") {
		t.Fatalf("commands=%#v, want matching cleanup commands", client.execCommands)
	}
}

func TestFreestyleDirectUploadCleansArchiveAfterExecFailure(t *testing.T) {
	client := &fakeFreestyleClient{execErrAt: 1}
	backend := &freestyleBackend{rt: Runtime{Stderr: io.Discard}}
	err := backend.extractFreestyleArchive(context.Background(), client, "vm123", "/tmp/crabbox-test.tgz", "/workspace/repo")
	if err == nil || !strings.Contains(err.Error(), "exec failed") {
		t.Fatalf("err=%v, want extraction transport failure", err)
	}
	if len(client.execCommands) != 2 {
		t.Fatalf("commands=%#v, want failed extract and cleanup", client.execCommands)
	}
	if !strings.Contains(client.execCommands[0], "tar -xzf") || !strings.Contains(client.execCommands[1], "rm -f") {
		t.Fatalf("commands=%#v, want extract followed by cleanup", client.execCommands)
	}
}

func TestFreestyleFallbackExtractCommandCleansUploadsOnFailure(t *testing.T) {
	cmd := freestyleFallbackExtractCommand("/tmp/crabbox-test.tgz.b64", "/tmp/crabbox-test.tgz", "/workspace/repo")
	for _, want := range []string{
		"base64 -d '/tmp/crabbox-test.tgz.b64' > '/tmp/crabbox-test.tgz'",
		"tar -xzf '/tmp/crabbox-test.tgz' -C '/workspace/repo'",
		"; status=$?; rm -f '/tmp/crabbox-test.tgz.b64' '/tmp/crabbox-test.tgz'; exit $status",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("command missing %q: %s", want, cmd)
		}
	}
	if strings.Index(cmd, "rm -f '/tmp/crabbox-test.tgz.b64'") < strings.Index(cmd, "tar -xzf") {
		t.Fatalf("cleanup should run after extract attempt: %s", cmd)
	}
}

func TestFreestyleDirectExtractCommandCleansArchiveOnFailure(t *testing.T) {
	cmd := freestyleDirectExtractCommand("/tmp/crabbox-test.tgz", "/workspace/repo")
	for _, want := range []string{
		"tar -xzf '/tmp/crabbox-test.tgz' -C '/workspace/repo'",
		"; status=$?; rm -f '/tmp/crabbox-test.tgz'; exit $status",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("command missing %q: %s", want, cmd)
		}
	}
	if strings.Index(cmd, "rm -f '/tmp/crabbox-test.tgz'") < strings.Index(cmd, "tar -xzf") {
		t.Fatalf("cleanup should run after extract attempt: %s", cmd)
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
	getVM             freestyleVM
	getVMErr          error
	listVMs           []freestyleVM
	listVMsErr        error
	prepareCommands   []string
	writeFilePath     string
	writeFileContent  string
	writeFileEncoding string
	writeFileErr      error
	writeFileBlock    bool
	execCommands      []string
	deleteIDs         []string
	deleteErr         error
	deleteDeadlineSet bool
	execCalls         int
	execErrAt         int
}

func (f *fakeFreestyleClient) CreateVM(_ context.Context, req freestyleCreateVMRequest) (freestyleVM, error) {
	f.createReq = &req
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
	if f.writeFileBlock {
		<-ctx.Done()
		return ctx.Err()
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
