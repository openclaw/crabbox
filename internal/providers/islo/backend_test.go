package islo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"

	gosdk "github.com/islo-labs/go-sdk"
)

func TestParseIsloSSE(t *testing.T) {
	body := strings.Join([]string{
		"event: stdout",
		"data: hello",
		"",
		"event: stderr",
		"data: warn",
		"",
		"event: exit",
		"data: 7",
		"",
	}, "\n")
	var stdout, stderr bytes.Buffer
	code, err := parseIsloSSE(strings.NewReader(body), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 || stdout.String() != "hello" || stderr.String() != "warn" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestParseIsloSSERequiresExitEvent(t *testing.T) {
	body := strings.Join([]string{
		"event: stdout",
		"data: partial",
		"",
	}, "\n")
	var stdout, stderr bytes.Buffer
	code, err := parseIsloSSE(strings.NewReader(body), &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "without exit event") {
		t.Fatalf("code=%d err=%v, want missing exit event error", code, err)
	}
	if stdout.String() != "partial" {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestParseIsloSSERejectsInvalidExitEvent(t *testing.T) {
	body := strings.Join([]string{
		"event: exit",
		"data: nope",
		"",
	}, "\n")
	if _, err := parseIsloSSE(strings.NewReader(body), &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "invalid exit event") {
		t.Fatalf("err=%v, want invalid exit event error", err)
	}
}

func TestIsloExecCommandPreservesShellString(t *testing.T) {
	got, err := isloExecCommand([]string{"pnpm install && pnpm test"}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bash", "-lc", "pnpm install && pnpm test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command=%#v want %#v", got, want)
	}
}

func TestIsloExecCommandQuotesImplicitShellArgv(t *testing.T) {
	got, err := isloExecCommand([]string{"FOO=bar", "pnpm", "test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "bash" || got[1] != "-lc" || !strings.Contains(got[2], "FOO=") || !strings.Contains(got[2], "'pnpm'") {
		t.Fatalf("command=%#v", got)
	}
}

func TestLeadingEnvAssignmentUsesShell(t *testing.T) {
	if !leadingEnvAssignment([]string{"FOO=bar", "pnpm", "test"}) {
		t.Fatal("expected leading env assignment to require shell")
	}
	if leadingEnvAssignment([]string{"pnpm", "test"}) {
		t.Fatal("plain argv should not require shell")
	}
}

func TestIsloStatusReady(t *testing.T) {
	for _, status := range []string{"ready", "running", "started", "active"} {
		if !isloStatusReady(status) {
			t.Fatalf("expected %q ready", status)
		}
	}
	if isloStatusReady("stopped") {
		t.Fatal("stopped should not be ready")
	}
}

func TestResolveIsloLeaseIDRejectsUnclaimedRawSandbox(t *testing.T) {
	if _, _, err := resolveIsloLeaseID("production", "", false); err == nil {
		t.Fatal("expected raw non-Crabbox sandbox to be rejected")
	}
	leaseID, name, err := resolveIsloLeaseID("crabbox-repo-abcdef", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "isb_crabbox-repo-abcdef" || name != "crabbox-repo-abcdef" {
		t.Fatalf("lease=%q name=%q", leaseID, name)
	}
}

func TestIsloWorkspacePathDefaultsUnderWorkspace(t *testing.T) {
	if got, err := isloWorkspacePath(Config{}); err != nil || got != "/workspace/crabbox" {
		t.Fatalf("workspace=%q err=%v", got, err)
	}
	if got, err := isloWorkspacePath(Config{Islo: IsloConfig{Workdir: "repo"}}); err != nil || got != "/workspace/repo" {
		t.Fatalf("workspace=%q err=%v", got, err)
	}
	if got, err := isloWorkspacePath(Config{Islo: IsloConfig{Workdir: "team/repo"}}); err != nil || got != "/workspace/team/repo" {
		t.Fatalf("workspace=%q err=%v", got, err)
	}
}

func TestIsloWorkspacePathRejectsEscapes(t *testing.T) {
	for _, workdir := range []string{"/work/repo", "/etc", "../etc", "repo/../../../etc", ".", "./.."} {
		t.Run(workdir, func(t *testing.T) {
			if got, err := isloWorkspacePath(Config{Islo: IsloConfig{Workdir: workdir}}); err == nil {
				t.Fatalf("workspace=%q, want error for workdir %q", got, workdir)
			}
		})
	}
}

func TestIsloRunRejectsUnsafeWorkdirBeforeProviderClient(t *testing.T) {
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{Workdir: "../etc"}},
		rt:  Runtime{Stderr: io.Discard},
	}
	_, err := backend.Run(context.Background(), RunRequest{NoSync: true})
	if err == nil || !strings.Contains(err.Error(), "escapes /workspace") {
		t.Fatalf("Run err=%v, want workdir containment error", err)
	}
}

func TestIsloCreateSandboxRejectsUnsafeWorkdirBeforeAPI(t *testing.T) {
	client := &fakeIsloSyncClient{}
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{Workdir: "../etc"}},
		rt:  Runtime{Stderr: io.Discard},
	}
	_, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false)
	if err == nil || !strings.Contains(err.Error(), "escapes /workspace") {
		t.Fatalf("createSandbox err=%v, want workdir containment error", err)
	}
	if client.createRequest != nil {
		t.Fatalf("CreateSandbox was called with %#v", client.createRequest)
	}
}

func TestIsloCreateSandboxPassesRelativeWorkdirToProvider(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{createName: "crabbox-repo-abcdef"}
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{Workdir: "team/repo"}},
		rt:  Runtime{Stderr: io.Discard},
	}
	_, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if client.createRequest == nil || client.createRequest.Workdir == nil || *client.createRequest.Workdir != "team/repo" {
		t.Fatalf("create workdir=%v", client.createRequest)
	}
}

func TestIsloSyncWorkspaceUploadsRepoArchive(t *testing.T) {
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
	client := &fakeIsloSyncClient{}
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{Workdir: "repo"}},
		rt:  Runtime{Stderr: io.Discard},
	}
	_, _, err := backend.syncWorkspace(context.Background(), client, "crabbox-test", RunRequest{
		Repo: Repo{Root: root, Name: "repo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.uploadPath != "/workspace/repo" {
		t.Fatalf("upload path=%q", client.uploadPath)
	}
	if len(client.prepareCommands) != 1 || !strings.Contains(client.prepareCommands[0], "mkdir -p '/workspace/repo'") {
		t.Fatalf("prepare commands=%#v", client.prepareCommands)
	}
	if !tarGzipContains(t, client.uploaded.Bytes(), "go.mod") {
		t.Fatal("uploaded archive missing go.mod")
	}
}

func TestIsloSyncWorkspaceFallsBackToExecUpload(t *testing.T) {
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
	client := &fakeIsloSyncClient{uploadErr: errors.New("api upload failed"), closeUploadReader: true}
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{Workdir: "repo"}},
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

func TestRejectIsloSyncOptionsAllowsForceSyncLarge(t *testing.T) {
	if err := rejectIsloSyncOptions(RunRequest{ForceSyncLarge: true}); err != nil {
		t.Fatalf("force sync large should be honored by Islo archive sync: %v", err)
	}
	if err := rejectIsloSyncOptions(RunRequest{SyncOnly: true}); err == nil || !strings.Contains(err.Error(), "--sync-only") {
		t.Fatalf("sync-only err=%v", err)
	}
	if err := rejectIsloSyncOptions(RunRequest{ChecksumSync: true}); err == nil || !strings.Contains(err.Error(), "--checksum") {
		t.Fatalf("checksum err=%v", err)
	}
}

func TestNewIsloSandboxNameUsesCrabboxPrefix(t *testing.T) {
	name := newIsloSandboxName(Repo{Name: "repo"})
	if !strings.HasPrefix(name, "crabbox-repo-") {
		t.Fatalf("name=%q", name)
	}
	if !isCrabboxIsloSandboxName(name) {
		t.Fatalf("expected %q to be recognized as Crabbox-owned", name)
	}
}

func TestIsloSDKClientListUsesInjectedHTTPAndPaginates(t *testing.T) {
	authHits := 0
	listHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/token":
			authHits++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"session_token":  "jwt-from-test",
				"cookie_max_age": 3600,
			})
		case "/sandboxes/":
			listHits++
			if got := r.Header.Get("Authorization"); got != "Bearer jwt-from-test" {
				t.Fatalf("Authorization=%q", got)
			}
			offset := r.URL.Query().Get("offset")
			offsetValue, _ := strconv.Atoi(offset)
			items := []map[string]any{}
			if offset == "0" {
				for i := 0; i < 100; i++ {
					items = append(items, map[string]any{"id": "id", "name": "crabbox-a", "status": "running", "image": "ubuntu"})
				}
			} else if offset == "100" {
				items = append(items, map[string]any{"id": "id", "name": "crabbox-b", "status": "running", "image": "ubuntu"})
			} else {
				t.Fatalf("unexpected offset=%q", offset)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items":  items,
				"total":  101,
				"limit":  100,
				"offset": offsetValue,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	api, err := newIsloClient(Config{Islo: IsloConfig{APIKey: "ak_test", BaseURL: srv.URL}}, Runtime{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	items, err := api.ListSandboxes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 101 {
		t.Fatalf("items=%d", len(items))
	}
	if authHits != 1 || listHits != 2 {
		t.Fatalf("authHits=%d listHits=%d", authHits, listHits)
	}
}

func TestIsloSDKClientUploadArchiveStreamsRawTarball(t *testing.T) {
	authHits := 0
	uploadHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/token":
			authHits++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"session_token":  "jwt-from-test",
				"cookie_max_age": 3600,
			})
		case "/sandboxes/crabbox-test/files-archive":
			uploadHits++
			if got := r.Header.Get("Authorization"); got != "Bearer jwt-from-test" {
				t.Fatalf("Authorization=%q", got)
			}
			if got := r.Header.Get("Content-Type"); got != "application/gzip" {
				t.Fatalf("Content-Type=%q", got)
			}
			if got := r.URL.Query().Get("path"); got != "/workspace/repo" {
				t.Fatalf("path=%q", got)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != "archive" {
				t.Fatalf("body=%q", string(body))
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	api, err := newIsloClient(Config{Islo: IsloConfig{APIKey: "ak_test", BaseURL: srv.URL}}, Runtime{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.UploadArchive(t.Context(), "crabbox-test", "/workspace/repo", strings.NewReader("archive")); err != nil {
		t.Fatal(err)
	}
	if authHits != 1 || uploadHits != 1 {
		t.Fatalf("authHits=%d uploadHits=%d", authHits, uploadHits)
	}
}

type fakeIsloSyncClient struct {
	prepareCommands   []string
	uploadPath        string
	uploaded          bytes.Buffer
	uploadErr         error
	closeUploadReader bool
	createRequest     *gosdk.SandboxCreate
	createName        string
}

func (f *fakeIsloSyncClient) CreateSandbox(_ context.Context, req *gosdk.SandboxCreate) (*gosdk.SandboxResponse, error) {
	f.createRequest = req
	name := f.createName
	if name == "" {
		name = "crabbox-test-abcdef"
	}
	return &gosdk.SandboxResponse{Name: name}, nil
}

func (f *fakeIsloSyncClient) GetSandbox(context.Context, string) (*gosdk.SandboxResponse, error) {
	return nil, nil
}

func (f *fakeIsloSyncClient) ListSandboxes(context.Context) ([]*gosdk.SandboxResponse, error) {
	return nil, nil
}

func (f *fakeIsloSyncClient) DeleteSandbox(context.Context, string) error {
	return nil
}

func (f *fakeIsloSyncClient) UploadArchive(_ context.Context, _ string, targetPath string, archive io.Reader) error {
	f.uploadPath = targetPath
	_, err := io.Copy(&f.uploaded, archive)
	if f.closeUploadReader {
		if closer, ok := archive.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	if f.uploadErr != nil {
		return f.uploadErr
	}
	return err
}

func (f *fakeIsloSyncClient) ExecStream(_ context.Context, _ string, req *gosdk.ExecRequest, _, _ io.Writer) (int, error) {
	f.prepareCommands = append(f.prepareCommands, strings.Join(req.GetCommand(), " "))
	return 0, nil
}

func (f *fakeIsloSyncClient) commandContains(value string) bool {
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
