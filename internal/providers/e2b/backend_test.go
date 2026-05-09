package e2b

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestParseE2BProcessStream(t *testing.T) {
	body := bytes.Join([][]byte{
		e2bTestEnvelope(0, map[string]any{"event": map[string]any{"start": map[string]any{"pid": 42}}}),
		e2bTestEnvelope(0, map[string]any{"event": map[string]any{"data": map[string]any{"stdout": base64.StdEncoding.EncodeToString([]byte("hello"))}}}),
		e2bTestEnvelope(0, map[string]any{"event": map[string]any{"data": map[string]any{"stderr": base64.StdEncoding.EncodeToString([]byte("warn"))}}}),
		e2bTestEnvelope(0, map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": 7, "exited": true}}}),
		e2bTestEnvelope(2, map[string]any{}),
	}, nil)
	var stdout, stderr bytes.Buffer
	code, err := parseE2BProcessStream(bytes.NewReader(body), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 || stdout.String() != "hello" || stderr.String() != "warn" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestParseE2BProcessStreamRequiresEndEvent(t *testing.T) {
	body := bytes.Join([][]byte{
		e2bTestEnvelope(0, map[string]any{"event": map[string]any{"data": map[string]any{"stdout": base64.StdEncoding.EncodeToString([]byte("partial"))}}}),
		e2bTestEnvelope(2, map[string]any{}),
	}, nil)
	var stdout bytes.Buffer
	code, err := parseE2BProcessStream(bytes.NewReader(body), &stdout, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "without end event") {
		t.Fatalf("code=%d err=%v, want missing end event error", code, err)
	}
	if stdout.String() != "partial" {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestE2BCommandString(t *testing.T) {
	if got := e2bCommandString([]string{"go", "test", "./..."}, false); got != "'go' 'test' './...'" {
		t.Fatalf("plain command=%q", got)
	}
	if got := e2bCommandString([]string{"FOO=bar", "go", "test"}, false); !strings.Contains(got, "FOO=") || !strings.Contains(got, "'go'") {
		t.Fatalf("env command=%q", got)
	}
	if got := e2bCommandString([]string{"pnpm install && pnpm test"}, true); got != "pnpm install && pnpm test" {
		t.Fatalf("shell command=%q", got)
	}
}

func TestE2BWorkspacePath(t *testing.T) {
	if got := e2bWorkspacePath(Config{}); got != "/home/user/crabbox" {
		t.Fatalf("workspace=%q", got)
	}
	if got := e2bWorkspacePath(Config{E2B: E2BConfig{Workdir: "repo"}}); got != "/home/user/repo" {
		t.Fatalf("workspace=%q", got)
	}
	if got := e2bWorkspacePath(Config{E2B: E2BConfig{User: "ubuntu", Workdir: "repo"}}); got != "/home/ubuntu/repo" {
		t.Fatalf("workspace=%q", got)
	}
	if got := e2bWorkspacePath(Config{E2B: E2BConfig{User: "root", Workdir: "repo"}}); got != "/root/repo" {
		t.Fatalf("workspace=%q", got)
	}
	if got := e2bWorkspacePath(Config{E2B: E2BConfig{Workdir: "/work/repo"}}); got != "/work/repo" {
		t.Fatalf("workspace=%q", got)
	}
}

func TestE2BClientCreateConnectListAndDeleteUseOfficialRESTShape(t *testing.T) {
	var createBody map[string]any
	listHits := 0
	deleteHit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-API-Key"); got != "e2b_test" {
			t.Fatalf("X-API-Key=%q", got)
		}
		switch r.URL.Path {
		case "/sandboxes":
			if r.Method != http.MethodPost {
				t.Fatalf("method=%s", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"templateID":      "base",
				"sandboxID":       "sbx_1",
				"envdVersion":     "0.5.7",
				"envdAccessToken": "envd-token",
			})
		case "/sandboxes/sbx_1/connect":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["timeout"].(float64) != 120 {
				t.Fatalf("connect body=%v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"templateID":      "base",
				"sandboxID":       "sbx_1",
				"envdVersion":     "0.5.7",
				"envdAccessToken": "envd-token",
			})
		case "/v2/sandboxes":
			listHits++
			if got := r.URL.Query().Get("metadata"); !strings.Contains(got, "provider=e2b") || !strings.Contains(got, "crabbox=true") {
				t.Fatalf("metadata query=%q", got)
			}
			if listHits == 1 {
				w.Header().Set("x-next-token", "next")
				_ = json.NewEncoder(w).Encode([]map[string]any{{"templateID": "base", "sandboxID": "sbx_1", "state": "running", "metadata": map[string]string{"provider": "e2b", "crabbox": "true"}}})
				return
			}
			if got := r.URL.Query().Get("nextToken"); got != "next" {
				t.Fatalf("nextToken=%q", got)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"templateID": "base", "sandboxID": "sbx_2", "state": "running", "metadata": map[string]string{"provider": "e2b", "crabbox": "true"}}})
		case "/sandboxes/sbx_1":
			if r.Method != http.MethodDelete {
				t.Fatalf("method=%s", r.Method)
			}
			deleteHit = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	api, err := newE2BClient(Config{E2B: E2BConfig{APIKey: "e2b_test", APIURL: srv.URL}}, Runtime{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := api.CreateSandbox(t.Context(), e2bCreateSandboxRequest{
		TemplateID:          "base",
		TimeoutSeconds:      60,
		AllowInternetAccess: true,
		Metadata:            map[string]string{"provider": "e2b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.SandboxID != "sbx_1" {
		t.Fatalf("sandbox=%#v", sandbox)
	}
	if createBody["templateID"] != "base" || createBody["timeout"].(float64) != 60 || createBody["secure"] != true || createBody["allow_internet_access"] != true {
		t.Fatalf("create body=%v", createBody)
	}
	session, err := api.ConnectSandbox(t.Context(), "sbx_1", 120)
	if err != nil {
		t.Fatal(err)
	}
	if session.SandboxID != "sbx_1" || session.EnvdAccessToken != "envd-token" {
		t.Fatalf("session=%#v", session)
	}
	items, err := api.ListSandboxes(t.Context(), map[string]string{"provider": "e2b", "crabbox": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || listHits != 2 {
		t.Fatalf("items=%d listHits=%d", len(items), listHits)
	}
	if err := api.DeleteSandbox(t.Context(), "sbx_1"); err != nil {
		t.Fatal(err)
	}
	if !deleteHit {
		t.Fatal("delete endpoint was not called")
	}
}

func TestE2BSyncWorkspaceUploadsRepoArchive(t *testing.T) {
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
	client := &fakeE2BSyncClient{}
	backend := &e2bBackend{
		cfg: Config{E2B: E2BConfig{User: "ubuntu", Workdir: "repo"}},
		rt:  Runtime{Stderr: io.Discard},
	}
	workspace := e2bWorkspacePath(backend.cfg)
	_, _, err := backend.syncWorkspace(context.Background(), client, e2bSession{SandboxID: "sbx_1"}, RunRequest{
		Repo: Repo{Root: root, Name: "repo"},
	}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if client.uploadPath == "" || !strings.HasPrefix(client.uploadPath, "/tmp/crabbox-") {
		t.Fatalf("upload path=%q", client.uploadPath)
	}
	if !tarGzipContains(t, client.uploaded.Bytes(), "go.mod") {
		t.Fatal("uploaded archive missing go.mod")
	}
	if !client.commandContains("mkdir -p '/home/ubuntu/repo'") || !client.commandContains("tar -xzf") {
		t.Fatalf("commands=%#v", client.commands)
	}
	if !client.userContains("ubuntu") {
		t.Fatalf("users=%#v", client.users)
	}
}

func TestE2BStatusReady(t *testing.T) {
	for _, status := range []string{"", "running"} {
		if !e2bStatusReady(status) {
			t.Fatalf("expected %q ready", status)
		}
	}
	if e2bStatusReady("paused") {
		t.Fatal("paused should not be ready")
	}
}

func TestE2BSandboxToServerUsesMetadata(t *testing.T) {
	server := e2bSandboxToServer(e2bSandbox{
		SandboxID:  "sbx_1",
		TemplateID: "base",
		State:      "running",
		Metadata: map[string]string{
			"provider": "e2b",
			"crabbox":  "true",
			"lease":    "cbx_123",
			"slug":     "blue-lobster",
		},
	})
	if server.Provider != "e2b" || server.CloudID != "sbx_1" || server.Labels["lease"] != "cbx_123" || server.Labels["slug"] != "blue-lobster" {
		t.Fatalf("server=%#v", server)
	}
	if server.ServerType.Name != "base" {
		t.Fatalf("type=%q", server.ServerType.Name)
	}
}

func TestE2BResolveSyntheticIDRequiresCrabboxMetadata(t *testing.T) {
	backend := &e2bBackend{}
	client := &fakeE2BSyncClient{
		sandbox: e2bSandbox{
			SandboxID: "sbx_1",
			Metadata:  map[string]string{"provider": "other"},
		},
	}
	_, _, _, err := backend.resolveSandboxID(context.Background(), client, "e2b_sbx_1", "", false)
	if err == nil || !strings.Contains(err.Error(), "not claimed by Crabbox") {
		t.Fatalf("err=%v, want ownership error", err)
	}

	client.sandbox.Metadata = map[string]string{
		"provider": "e2b",
		"crabbox":  "true",
		"lease":    "cbx_123",
		"slug":     "blue-lobster",
	}
	leaseID, sandboxID, slug, err := backend.resolveSandboxID(context.Background(), client, "e2b_sbx_1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "cbx_123" || sandboxID != "sbx_1" || slug != "blue-lobster" {
		t.Fatalf("lease=%q sandbox=%q slug=%q", leaseID, sandboxID, slug)
	}
}

type fakeE2BSyncClient struct {
	commands   []string
	users      []string
	sandbox    e2bSandbox
	getErr     error
	uploadPath string
	uploaded   bytes.Buffer
}

func (f *fakeE2BSyncClient) CreateSandbox(context.Context, e2bCreateSandboxRequest) (e2bSandbox, error) {
	return e2bSandbox{}, nil
}

func (f *fakeE2BSyncClient) ConnectSandbox(context.Context, string, int) (e2bSession, error) {
	return e2bSession{}, nil
}

func (f *fakeE2BSyncClient) GetSandbox(context.Context, string) (e2bSandbox, error) {
	if f.getErr != nil {
		return e2bSandbox{}, f.getErr
	}
	return f.sandbox, nil
}

func (f *fakeE2BSyncClient) ListSandboxes(context.Context, map[string]string) ([]e2bSandbox, error) {
	return nil, nil
}

func (f *fakeE2BSyncClient) DeleteSandbox(context.Context, string) error {
	return nil
}

func (f *fakeE2BSyncClient) UploadFile(_ context.Context, _ e2bSession, targetPath string, r io.Reader) error {
	f.uploadPath = targetPath
	_, err := io.Copy(&f.uploaded, r)
	return err
}

func (f *fakeE2BSyncClient) StartProcess(_ context.Context, _ e2bSession, req e2bProcessRequest) (int, error) {
	f.commands = append(f.commands, req.Command)
	f.users = append(f.users, req.User)
	return 0, nil
}

func (f *fakeE2BSyncClient) commandContains(value string) bool {
	for _, command := range f.commands {
		if strings.Contains(command, value) {
			return true
		}
	}
	return false
}

func (f *fakeE2BSyncClient) userContains(value string) bool {
	for _, user := range f.users {
		if user == value {
			return true
		}
	}
	return false
}

func e2bTestEnvelope(flags byte, v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out bytes.Buffer
	out.WriteByte(flags)
	out.Write([]byte{byte(len(data) >> 24), byte(len(data) >> 16), byte(len(data) >> 8), byte(len(data))})
	out.Write(data)
	return out.Bytes()
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
