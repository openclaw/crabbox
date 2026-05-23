package freestyle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestFreestyleExecCommandPreservesShellString(t *testing.T) {
	got, err := freestyleExecCommand([]string{"pnpm install && pnpm test"}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := "pnpm install && pnpm test"
	if got != want {
		t.Fatalf("command=%q want %q", got, want)
	}
}

func TestFreestyleExecCommandQuotesImplicitShellArgv(t *testing.T) {
	got, err := freestyleExecCommand([]string{"FOO=bar", "pnpm", "test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "FOO=") || !strings.Contains(got, "pnpm") || !strings.Contains(got, "test") {
		t.Fatalf("command=%q", got)
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
	if _, _, err := resolveFreestyleLeaseID("some-random-id", "", false); err == nil {
		t.Fatal("expected non-fsb_ id to be rejected")
	}
	leaseID, name, err := resolveFreestyleLeaseID("fsb_some-vm-id", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "fsb_some-vm-id" || name != "some-vm-id" {
		t.Fatalf("lease=%q name=%q", leaseID, name)
	}
}

func TestFreestyleWorkspacePathDefaultsUnderWorkspace(t *testing.T) {
	if got, err := freestyleWorkspacePath(Config{}); err != nil || got != "/workspace/crabbox" {
		t.Fatalf("workspace=%q err=%v", got, err)
	}
	if got, err := freestyleWorkspacePath(Config{Freestyle: FreestyleConfig{Workdir: "repo"}}); err != nil || got != "/workspace/repo" {
		t.Fatalf("workspace=%q err=%v", got, err)
	}
	if got, err := freestyleWorkspacePath(Config{Freestyle: FreestyleConfig{Workdir: "team/repo"}}); err != nil || got != "/workspace/team/repo" {
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
		cfg: Config{Freestyle: FreestyleConfig{Workdir: "../etc"}},
		rt:  Runtime{Stderr: io.Discard},
	}
	_, err := backend.Run(context.Background(), RunRequest{NoSync: true})
	if err == nil || !strings.Contains(err.Error(), "escapes /workspace") {
		t.Fatalf("Run err=%v, want workdir containment error", err)
	}
}

func TestFreestyleCreateSandboxWorksWithoutWorkdir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeFreestyleClient{createName: "crabbox-repo-abcdef", createID: "test-vm-id"}
	backend := &freestyleBackend{
		cfg: Config{},
		rt:  Runtime{Stderr: io.Discard},
	}
	_, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if client.createRequest == nil {
		t.Fatal("CreateVM was not called")
	}
	if client.createRequest.Workdir != "" {
		t.Fatalf("create workdir should be empty, got=%q", client.createRequest.Workdir)
	}
	if client.createRequest.Name == "" {
		t.Fatal("create name should be set")
	}
}

func TestFreestyleCreateSandboxPassesNameWithoutWorkdir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeFreestyleClient{createName: "crabbox-repo-abcdef", createID: "test-vm-id"}
	backend := &freestyleBackend{
		cfg: Config{Freestyle: FreestyleConfig{Workdir: "team/repo"}},
		rt:  Runtime{Stderr: io.Discard},
	}
	_, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if client.createRequest == nil {
		t.Fatal("CreateVM was not called")
	}
	if !strings.HasPrefix(client.createRequest.Name, "crabbox-repo-") {
		t.Fatalf("create name=%q", client.createRequest.Name)
	}
	if client.createRequest.Workdir != "" {
		t.Fatalf("create workdir should be empty, got=%q", client.createRequest.Workdir)
	}
}

func TestFreestyleCreateSandboxStoresPondClaimForList(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeFreestyleClient{createName: "crabbox-repo-abcdef", createID: "test-vm-id"}
	backend := &freestyleBackend{
		cfg: Config{
			Pond:      "Alpha Pond",
			Freestyle: FreestyleConfig{Workdir: "team/repo"},
		},
		rt: Runtime{Stderr: io.Discard},
	}
	leaseID, _, slug, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "web")
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveLeaseClaim(leaseID)
	if err != nil || !ok {
		t.Fatalf("resolve claim ok=%t err=%v", ok, err)
	}
	if claim.Pond != "alpha-pond" {
		t.Fatalf("claim pond=%q want alpha-pond", claim.Pond)
	}
	server := freestyleVMToServer(freestyleVM{ID: client.createID, Name: client.createName, State: "running"})
	if server.Labels["pond"] != "alpha-pond" {
		t.Fatalf("server pond label=%q labels=%#v", server.Labels["pond"], server.Labels)
	}
	if server.Labels["slug"] != normalizeLeaseSlug(slug) {
		t.Fatalf("server slug=%q want %q", server.Labels["slug"], normalizeLeaseSlug(slug))
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
	if !client.commandContains("mkdir") || !client.commandContains("workspace/repo") {
		t.Fatalf("prepare commands missing mkdir: %#v", client.prepareCommands)
	}
	if !client.writtenFile {
		t.Fatal("expected WriteFile to be called")
	}
	if client.writePath == "" {
		t.Fatal("expected write path to be set")
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
	client := &fakeFreestyleClient{writeFileErr: io.ErrUnexpectedEOF}
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
	if !client.commandContains("base64 -d") && !client.commandContains("base64 --decode") {
		t.Fatalf("fallback commands missing base64 decode: %#v", client.prepareCommands)
	}
	if !client.commandContains("tar -xzf") {
		t.Fatalf("fallback commands missing tar extract: %#v", client.prepareCommands)
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

func TestRejectFreestyleSyncOptionsAllowsForceSyncLarge(t *testing.T) {
	if err := rejectFreestyleSyncOptions(RunRequest{ForceSyncLarge: true}); err != nil {
		t.Fatalf("force sync large should be honored by Freestyle archive sync: %v", err)
	}
	if err := rejectFreestyleSyncOptions(RunRequest{SyncOnly: true}); err == nil || !strings.Contains(err.Error(), "--sync-only") {
		t.Fatalf("sync-only err=%v", err)
	}
	if err := rejectFreestyleSyncOptions(RunRequest{ChecksumSync: true}); err == nil || !strings.Contains(err.Error(), "--checksum") {
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
	prepareCommands []string
	execRequests    []freestyleExecRequest
	writePath       string
	writeData       []byte
	writeFileErr    error
	writtenFile     bool
	createRequest   *freestyleCreateVMRequest
	createName      string
	createID        string
}

func (f *fakeFreestyleClient) CreateVM(_ context.Context, req freestyleCreateVMRequest) (freestyleVM, error) {
	f.createRequest = &req
	name := f.createName
	if name == "" {
		name = "crabbox-test-abcdef"
	}
	id := f.createID
	if id == "" {
		id = "test-vm-id-abcdef"
	}
	return freestyleVM{ID: id, Name: name}, nil
}

func (f *fakeFreestyleClient) GetVM(_ context.Context, _ string) (freestyleVM, error) {
	return freestyleVM{}, nil
}

func (f *fakeFreestyleClient) ListVMs(_ context.Context) ([]freestyleVM, error) {
	return nil, nil
}

func (f *fakeFreestyleClient) DeleteVM(_ context.Context, _ string) error {
	return nil
}

func (f *fakeFreestyleClient) Exec(_ context.Context, _ string, command string, _ io.Writer, _ io.Writer) (int, error) {
	f.prepareCommands = append(f.prepareCommands, command)
	return 0, nil
}

func (f *fakeFreestyleClient) WriteFile(_ context.Context, vmID, path string, content []byte) error {
	f.writtenFile = true
	f.writePath = path
	f.writeData = content
	return f.writeFileErr
}

func (f *fakeFreestyleClient) ReadFile(_ context.Context, vmID, path string) ([]byte, error) {
	return nil, nil
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
