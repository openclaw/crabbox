package namespace

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamespaceSSHTargetParsesPrepareResult(t *testing.T) {
	target, err := namespaceSSHTarget(namespacePrepareResult{
		SSHEndpoint: "crabbox@ssh.namespace.example:2222",
		SSHKeyPath:  "/tmp/ns-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if target.User != "crabbox" || target.Host != "ssh.namespace.example" || target.Port != "2222" || target.Key != "/tmp/ns-key" {
		t.Fatalf("target=%#v", target)
	}
}

func TestParseNamespaceListAcceptsArrayAndWrappedObjects(t *testing.T) {
	for name, input := range map[string]string{
		"array":   `[{"name":"crabbox-blue-lobster-deadbeef","status":"running","size":"L","repository":"github.com/openclaw/crabbox","created_at":"2026-05-09T12:00:00Z"}]`,
		"wrapped": `{"devboxes":[{"display_name":"crabbox-blue-lobster-deadbeef","state":"stopped","machine_size":"M","repo":"github.com/openclaw/crabbox","createdAt":"2026-05-09T12:00:00Z"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			items, err := parseNamespaceList(input)
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 {
				t.Fatalf("items=%#v", items)
			}
			if items[0].Name != "crabbox-blue-lobster-deadbeef" || items[0].Repository != "github.com/openclaw/crabbox" {
				t.Fatalf("item=%#v", items[0])
			}
		})
	}
}

func TestParseNamespaceListAcceptsEmptyCLIText(t *testing.T) {
	items, err := parseNamespaceList("No devbox available yet. Try running `devbox create`.\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("items=%#v", items)
	}
}

func TestNamespaceSSHTargetFromConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".namespace", "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "crabbox-live.devbox.namespace.ssh")
	if err := os.WriteFile(path, []byte(`
Host crabbox-live.devbox.namespace
  IdentityFile ~/.namespace/ssh/crabbox-live.devbox.namespace.key
  ProxyCommand ~/.namespace/ssh/devbox-ssh-proxy ssh-proxy crabbox-live
  User devbox
`), 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := namespaceSSHTargetFromConfig("crabbox-live")
	if err != nil {
		t.Fatal(err)
	}
	if target.User != "devbox" || target.Host != "crabbox-live.devbox.namespace" || target.Port != "22" || !target.SSHConfigProxy {
		t.Fatalf("target=%#v", target)
	}
	if !strings.HasPrefix(target.Key, home) {
		t.Fatalf("key=%q", target.Key)
	}
}

func TestNamespaceItemToServerMapsCrabboxNames(t *testing.T) {
	server := namespaceItemToServer(namespaceListItem{
		Name:   "crabbox-blue-lobster-deadbeef",
		Status: "running",
		Size:   "XL",
	}, Config{})
	if server.Provider != namespaceProvider || server.Name != "crabbox-blue-lobster-deadbeef" || server.Status != "running" {
		t.Fatalf("server=%#v", server)
	}
	if server.Labels["slug"] != "blue-lobster" || server.ServerType.Name != "XL" {
		t.Fatalf("labels=%#v type=%#v", server.Labels, server.ServerType)
	}
}

func TestResolveNamespaceDevboxNameKeepsClaimedExternalName(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := claimLeaseForRepoProvider("nsd_existing-devbox", "existing-devbox", namespaceProvider, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	name, leaseID, slug, err := resolveNamespaceDevboxName("existing-devbox", false)
	if err != nil {
		t.Fatal(err)
	}
	if name != "existing-devbox" || leaseID != "nsd_existing-devbox" || slug != "existing-devbox" {
		t.Fatalf("name=%q leaseID=%q slug=%q", name, leaseID, slug)
	}
}

func TestResolveReleaseOnlySkipsNamespacePrepare(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()
	if err := claimLeaseForRepoProvider("nsd_crabbox-blue-lobster-deadbeef", "blue-lobster", namespaceProvider, repoRoot, 0, true); err != nil {
		t.Fatal(err)
	}
	runner := &namespaceRecordingRunner{}
	backend := &namespaceLeaseBackend{
		cfg: Config{Namespace: NamespaceConfig{WorkRoot: "/workspaces/crabbox"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
	}
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "blue-lobster", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("release-only resolve should not call devbox: %#v", runner.calls)
	}
	if lease.LeaseID != "nsd_crabbox-blue-lobster-deadbeef" || lease.Server.Name != "blue-lobster" {
		t.Fatalf("lease=%#v", lease)
	}
	if lease.SSH.Host != "" {
		t.Fatalf("release-only lease should not prepare SSH: %#v", lease.SSH)
	}
}

func TestCleanupNamespaceSSHFilesRemovesOnlyCrabboxNamespaceFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".namespace", "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "personal.devbox.namespace.ssh")
	for _, path := range []string{
		filepath.Join(dir, "crabbox-blue-lobster-deadbeef.devbox.namespace.ssh"),
		filepath.Join(dir, "crabbox-blue-lobster-deadbeef.devbox.namespace.key"),
		keep,
		filepath.Join(dir, "crabbox-blue-lobster-deadbeef.devbox.namespace.pub"),
	} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := cleanupNamespaceSSHFiles("", false, &out); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(dir, "crabbox-blue-lobster-deadbeef.devbox.namespace.ssh"),
		filepath.Join(dir, "crabbox-blue-lobster-deadbeef.devbox.namespace.key"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed, err=%v", path, err)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("non-crabbox namespace file should remain: %v", err)
	}
	if !strings.Contains(out.String(), "namespace ssh cleanup delete") {
		t.Fatalf("cleanup output=%q", out.String())
	}
}

func TestReleaseLeaseCleansNamespaceSSHFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".namespace", "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{".ssh", ".key"} {
		if err := os.WriteFile(filepath.Join(dir, "crabbox-blue-lobster-deadbeef.devbox.namespace"+ext), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner := &namespaceRecordingRunner{}
	var out bytes.Buffer
	backend := &namespaceLeaseBackend{
		cfg: Config{Namespace: NamespaceConfig{DeleteOnRelease: true}},
		rt:  Runtime{Stdout: &out, Stderr: io.Discard, Exec: runner},
	}
	lease := LeaseTarget{LeaseID: "cbx_deadbeef0000", Server: Server{Name: "crabbox-blue-lobster-deadbeef"}}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0] != "devbox delete crabbox-blue-lobster-deadbeef --force" {
		t.Fatalf("calls=%#v", runner.calls)
	}
	for _, ext := range []string{".ssh", ".key"} {
		path := filepath.Join(dir, "crabbox-blue-lobster-deadbeef.devbox.namespace"+ext)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed, err=%v", path, err)
		}
	}
}

func TestNamespaceRejectsUnsafeWorkRoot(t *testing.T) {
	for _, workRoot := range []string{"/", "/workspaces", "/tmp", "relative"} {
		cfg := Config{Namespace: NamespaceConfig{WorkRoot: workRoot}}
		if err := validateNamespaceConfig(cfg); err == nil {
			t.Fatalf("expected %q to be rejected", workRoot)
		}
	}
	if err := validateNamespaceConfig(Config{Namespace: NamespaceConfig{WorkRoot: "/workspaces/crabbox"}}); err != nil {
		t.Fatalf("valid work root rejected: %v", err)
	}
}

func TestNamespaceLifecycleCommandFallbacks(t *testing.T) {
	runner := &namespaceRecordingRunner{failFirst: true}
	backend := &namespaceLeaseBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}

	if err := backend.shutdownDevbox(context.Background(), "crabbox-blue-lobster-deadbeef"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || runner.calls[0] != "devbox shutdown crabbox-blue-lobster-deadbeef --force" || runner.calls[1] != "devbox stop crabbox-blue-lobster-deadbeef --force" {
		t.Fatalf("shutdown calls=%#v", runner.calls)
	}

	runner.calls = nil
	runner.failFirst = true
	if err := backend.deleteDevbox(context.Background(), "crabbox-blue-lobster-deadbeef"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || runner.calls[0] != "devbox delete crabbox-blue-lobster-deadbeef --force" || runner.calls[1] != "devbox destroy crabbox-blue-lobster-deadbeef --force" {
		t.Fatalf("delete calls=%#v", runner.calls)
	}
}

func TestNamespacePrepareReportsPrepareFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := &namespaceRecordingRunner{failAll: true}
	backend := &namespaceLeaseBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}

	_, err := backend.prepareDevbox(context.Background(), "crabbox-blue-lobster-deadbeef")
	if err == nil || !strings.Contains(err.Error(), "namespace devbox failed") {
		t.Fatalf("err=%v", err)
	}
	if len(runner.calls) != 2 || runner.calls[0] != "devbox configure-ssh" || runner.calls[1] != "devbox prepare crabbox-blue-lobster-deadbeef" {
		t.Fatalf("prepare calls=%#v", runner.calls)
	}
}

type namespaceRecordingRunner struct {
	calls     []string
	failAll   bool
	failFirst bool
}

func (r *namespaceRecordingRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.calls = append(r.calls, req.Name+" "+strings.Join(req.Args, " "))
	if r.failAll {
		return LocalCommandResult{ExitCode: 2}, errors.New("unsupported")
	}
	if r.failFirst {
		r.failFirst = false
		return LocalCommandResult{ExitCode: 2}, errors.New("unsupported")
	}
	return LocalCommandResult{}, nil
}
