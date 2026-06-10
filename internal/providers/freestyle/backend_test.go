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
)

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
	for _, status := range []string{"ready", "running", "started", "active"} {
		if !freestyleStatusReady(status) {
			t.Fatalf("expected %q ready", status)
		}
	}
	if freestyleStatusReady("stopped") {
		t.Fatal("stopped should not be ready")
	}
}

func TestResolveFreestyleLeaseIDRejectsUnclaimedRawSandbox(t *testing.T) {
	client := &fakeFreestyleClient{getVM: freestyleVM{
		ID:    "vm123",
		Name:  "personal-vm",
		State: "running",
	}}
	if _, _, err := resolveFreestyleLeaseID(context.Background(), client, "random-vm-id", "", false); err == nil {
		t.Fatal("expected raw non-Crabbox vm to be rejected")
	}
	if _, _, err := resolveFreestyleLeaseID(context.Background(), client, "fsb_vm123", "", false); err == nil {
		t.Fatal("expected unclaimed Freestyle vm to be rejected")
	}
}

func TestResolveFreestyleLeaseIDAcceptsCrabboxSandbox(t *testing.T) {
	client := &fakeFreestyleClient{getVM: freestyleVM{
		ID:    "vm123",
		Name:  "crabbox-repo-abc123",
		State: "running",
	}}
	leaseID, name, err := resolveFreestyleLeaseID(context.Background(), client, "fsb_vm123", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "fsb_vm123" || name != "vm123" {
		t.Fatalf("lease=%q name=%q", leaseID, name)
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
	_, err := backend.Run(context.Background(), RunRequest{
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
	if client.createReq.VcpuCount != 0 || client.createReq.MemSizeGb != 0 {
		t.Fatalf("create sizing=%d/%d want omitted", client.createReq.VcpuCount, client.createReq.MemSizeGb)
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
	if client.createReq.VcpuCount != 4 || client.createReq.MemSizeGb != 8 {
		t.Fatalf("create sizing=%d/%d", client.createReq.VcpuCount, client.createReq.MemSizeGb)
	}
}

func TestFreestyleCreateSandboxStoresClaimForList(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeFreestyleClient{createID: "vm789"}
	backend := &freestyleBackend{
		cfg: Config{Freestyle: FreestyleConfig{}},
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
}

func TestFreestyleListAndStatusUseStoredClaimSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeFreestyleClient{createID: "vm789"}
	backend := &freestyleBackend{
		cfg: Config{Freestyle: FreestyleConfig{}},
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
	if server.Labels["slug"] != "blue-lobster" {
		t.Fatalf("list labels=%v", server.Labels)
	}
	status := freestyleStatusView(leaseID, vm)
	if status.Slug != "blue-lobster" || status.Labels["slug"] != "blue-lobster" {
		t.Fatalf("status=%#v labels=%v", status, status.Labels)
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

type fakeFreestyleClient struct {
	createID          string
	createReq         *freestyleCreateVMRequest
	getVM             freestyleVM
	getVMErr          error
	prepareCommands   []string
	writeFilePath     string
	writeFileContent  string
	writeFileEncoding string
	writeFileErr      error
	execCommands      []string
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
	return nil, nil
}

func (f *fakeFreestyleClient) DeleteVM(_ context.Context, _ string) error {
	return nil
}

func (f *fakeFreestyleClient) Exec(_ context.Context, _ string, command string, _, _ io.Writer) (int, error) {
	f.execCommands = append(f.execCommands, command)
	f.prepareCommands = append(f.prepareCommands, command)
	return 0, nil
}

func (f *fakeFreestyleClient) WriteFile(_ context.Context, _ string, path, content, encoding string) error {
	f.writeFilePath = path
	f.writeFileContent = content
	f.writeFileEncoding = encoding
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
