package e2b

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type e2bRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn e2bRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestE2BClientRedactsReflectedCredentials(t *testing.T) {
	t.Run("API key", func(t *testing.T) {
		const secret = "e2b-api-secret"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"X-API-Key: `+secret+` quota exceeded"}`)
		}))
		defer server.Close()
		client := &e2bClient{apiKey: secret, apiURL: server.URL, httpClient: server.Client()}
		_, err := client.ListSandboxes(context.Background(), nil)
		assertE2BRedactedError(t, err, secret)
	})

	t.Run("envd access token", func(t *testing.T) {
		const secret = "envd-access-secret"
		httpClient := &http.Client{Transport: e2bRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Status:     "401 Unauthorized",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"message":"Bearer ` + secret + ` quota exceeded"}`)),
				Request:    req,
			}, nil
		})}
		client := &e2bClient{httpClient: httpClient}
		_, err := client.StartProcess(context.Background(), e2bSession{SandboxID: "sbx_1", Domain: "example.test", EnvdAccessToken: secret}, e2bProcessRequest{Command: "true"})
		assertE2BRedactedError(t, err, secret)
	})
}

func assertE2BRedactedError(t *testing.T, err error, secret string) {
	t.Helper()
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("error=%v, want redacted useful provider error", err)
	}
}

func TestE2BProcessStreamRedactsReflectedCredential(t *testing.T) {
	const secret = "envd-stream-secret"
	t.Run("end stream error", func(t *testing.T) {
		body := e2bTestEnvelope(2, map[string]any{"error": map[string]any{"code": "unauthorized", "message": "Bearer " + secret + " quota exceeded"}})
		_, err := parseE2BProcessStream(bytes.NewReader(body), io.Discard, io.Discard, secret)
		assertE2BRedactedError(t, err, secret)
	})
	t.Run("process end diagnostic", func(t *testing.T) {
		body := e2bTestEnvelope(0, map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": 1, "exited": false, "error": "Bearer " + secret + " quota exceeded"}}})
		var stderr bytes.Buffer
		if _, err := parseE2BProcessStream(bytes.NewReader(body), io.Discard, &stderr, secret); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stderr.String(), secret) || !strings.Contains(stderr.String(), "[redacted]") || !strings.Contains(stderr.String(), "quota exceeded") {
			t.Fatalf("stderr=%q, want redacted useful process diagnostic", stderr.String())
		}
	})
}

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

func TestValidateE2BAPIURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "https", raw: "HTTPS://API.E2B.APP:443/v1/", want: "https://api.e2b.app/v1"},
		{name: "loopback", raw: "http://127.0.0.1:8080/api/", want: "http://127.0.0.1:8080/api"},
		{name: "localhost", raw: "http://localhost:8080", want: "http://localhost:8080"},
		{name: "IPv6 loopback", raw: "http://[::1]:8080/", want: "http://[::1]:8080"},
		{name: "remote HTTP", raw: "http://api.e2b.app", wantErr: "must use HTTPS"},
		{name: "relative", raw: "/api", wantErr: "absolute HTTPS URL"},
		{name: "userinfo", raw: "https://user:pass@api.e2b.app", wantErr: "must not contain userinfo"},
		{name: "query", raw: "https://api.e2b.app?token=secret", wantErr: "must not contain userinfo"},
		{name: "fragment", raw: "https://api.e2b.app/#secret", wantErr: "must not contain userinfo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateE2BAPIURL(tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validateE2BAPIURL(%q) error = %v, want %q", tt.raw, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("validateE2BAPIURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
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

func TestE2BProcessUser(t *testing.T) {
	tests := []struct {
		name    string
		user    string
		want    string
		wantErr string
	}{
		{name: "empty keeps default process user", user: "", want: ""},
		{name: "trims user", user: " ubuntu ", want: "ubuntu"},
		{name: "root allowed", user: "root", want: "root"},
		{name: "rejects slash", user: "../tmp", wantErr: "not a path"},
		{name: "rejects backslash", user: `team\dev`, wantErr: "not a path"},
		{name: "rejects dot", user: ".", wantErr: "not a path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := e2bProcessUser(tt.user)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("user=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestE2BWarmupRejectsUnsafeUserBeforeClient(t *testing.T) {
	backend := &e2bBackend{
		cfg: Config{E2B: E2BConfig{User: "../tmp"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	err := backend.Warmup(context.Background(), WarmupRequest{})
	if err == nil || !strings.Contains(err.Error(), "invalid e2b.user") {
		t.Fatalf("err=%v, want invalid e2b.user", err)
	}
	if strings.Contains(err.Error(), "E2B_API_KEY") {
		t.Fatalf("validated user after client setup: %v", err)
	}
}

func TestProviderSpecAdvertisesRunSession(t *testing.T) {
	if !(Provider{}).Spec().Features.Has(core.FeatureRunSession) {
		t.Fatalf("features=%#v want run session", Provider{}.Spec().Features)
	}
}

func TestCleanE2BWorkspacePath(t *testing.T) {
	tests := []struct {
		name      string
		workspace string
		want      string
		wantErr   string
	}{
		{name: "cleans absolute path", workspace: " /home/user/repo/ ", want: "/home/user/repo"},
		{name: "rejects empty path", workspace: " ", wantErr: "empty"},
		{name: "rejects relative path", workspace: "repo", wantErr: "absolute"},
		{name: "rejects root", workspace: "/", wantErr: "too broad"},
		{name: "rejects home root", workspace: "/home", wantErr: "too broad"},
		{name: "rejects tmp root", workspace: "/tmp", wantErr: "too broad"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cleanE2BWorkspacePath(tt.workspace)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("workspace=%q, want %q", got, tt.want)
			}
		})
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

func TestE2BAPIClientRefusesCrossOriginRedirectBeforeReplay(t *testing.T) {
	var targetRequests int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests++
		t.Errorf("redirect target received %s %s key=%q", r.Method, r.URL.Path, r.Header.Get("X-API-Key"))
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer target.Close()

	trusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer trusted.Close()

	api, err := newE2BClient(Config{E2B: E2BConfig{APIKey: "e2b_test", APIURL: trusted.URL}}, Runtime{HTTP: trusted.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.ListSandboxes(t.Context(), nil)
	if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("ListSandboxes error = %v, want cross-origin refusal", err)
	}
	if targetRequests != 0 {
		t.Fatalf("redirect target received %d requests, want 0", targetRequests)
	}
}

func TestE2BEnvdClientRefusesCrossOriginRedirectBeforeReplay(t *testing.T) {
	var targetRequests int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests++
		t.Errorf("redirect target received %s %s token=%q", r.Method, r.URL.Path, r.Header.Get("X-Access-Token"))
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer target.Close()

	trusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer trusted.Close()
	trustedURL, err := url.Parse(trusted.URL)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, trusted.URL+"/envd", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Access-Token", "envd-token")
	resp, err := secureE2BHTTPClient(trusted.Client(), trustedURL).Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("envd request error = %v, want cross-origin refusal", err)
	}
	if targetRequests != 0 {
		t.Fatalf("redirect target received %d requests, want 0", targetRequests)
	}
}

func TestE2BClientFollowsSameOriginRedirect(t *testing.T) {
	var redirectedKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/sandboxes":
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
		case "/redirected":
			redirectedKey = r.Header.Get("X-API-Key")
			_ = json.NewEncoder(w).Encode([]e2bSandbox{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	api, err := newE2BClient(Config{E2B: E2BConfig{APIKey: "e2b_test", APIURL: server.URL}}, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.ListSandboxes(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if redirectedKey != "e2b_test" {
		t.Fatalf("redirected key = %q", redirectedKey)
	}
}

func TestE2BClientPreservesCallerRedirectPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/redirected", http.StatusFound)
	}))
	defer server.Close()

	callerErr := errors.New("caller refused redirect")
	callerChecks := 0
	httpClient := server.Client()
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		callerChecks++
		return callerErr
	}
	api, err := newE2BClient(Config{E2B: E2BConfig{APIKey: "e2b_test", APIURL: server.URL}}, Runtime{HTTP: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.ListSandboxes(t.Context(), nil)
	if !errors.Is(err, callerErr) || callerChecks != 1 {
		t.Fatalf("ListSandboxes error = %v, caller checks = %d", err, callerChecks)
	}
}

func TestE2BUploadFileRejectsMalformedDomainBeforeProducer(t *testing.T) {
	client := &e2bClient{apiKey: "e2b_test", domain: "%zz", httpClient: http.DefaultClient}
	err := client.UploadFile(context.Background(), e2bSession{SandboxID: "sbx_1"}, "/tmp/archive.tgz", strings.NewReader("archive"))
	if err == nil {
		t.Fatal("UploadFile err=nil, want malformed URL error")
	}
	runtime.Gosched()
	time.Sleep(10 * time.Millisecond)
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	if bytes.Contains(buf[:n], []byte("github.com/openclaw/crabbox/internal/providers/e2b.(*e2bClient).UploadFile.func1")) {
		t.Fatalf("multipart producer goroutine still running after malformed URL:\n%s", buf[:n])
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

func TestE2BSyncWorkspaceCleansRemoteArchiveWhenExtractFails(t *testing.T) {
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
	client := &fakeE2BSyncClient{processCodes: []int{0, 7, 0}}
	backend := &e2bBackend{
		cfg: Config{E2B: E2BConfig{User: "ubuntu", Workdir: "repo"}},
		rt:  Runtime{Stderr: io.Discard},
	}
	workspace := e2bWorkspacePath(backend.cfg)
	_, _, err := backend.syncWorkspace(context.Background(), client, e2bSession{SandboxID: "sbx_1"}, RunRequest{
		Repo: Repo{Root: root, Name: "repo"},
	}, workspace)
	if err == nil {
		t.Fatalf("expected extract failure")
	}
	if len(client.commands) != 3 {
		t.Fatalf("commands=%#v, want prepare, extract, cleanup", client.commands)
	}
	cleanup := client.commands[2]
	if !strings.Contains(cleanup, "rm -f '/tmp/crabbox-") {
		t.Fatalf("cleanup command missing remote archive removal: %q", cleanup)
	}
}

func TestE2BPrepareWorkspaceRejectsUnsafePath(t *testing.T) {
	client := &fakeE2BSyncClient{}
	cfg := Config{}
	cfg.Sync.Delete = true
	backend := &e2bBackend{
		cfg: cfg,
		rt:  Runtime{Stderr: io.Discard},
	}
	err := backend.prepareWorkspace(context.Background(), client, e2bSession{SandboxID: "sbx_1"}, "/")
	if err == nil || !strings.Contains(err.Error(), "too broad") {
		t.Fatalf("err=%v, want unsafe workspace error", err)
	}
	if len(client.commands) != 0 {
		t.Fatalf("commands=%#v, want none", client.commands)
	}
}

func TestE2BCreateSandboxRejectsUnsafeWorkdirBeforeAPI(t *testing.T) {
	client := &fakeE2BSyncClient{}
	backend := &e2bBackend{
		cfg: Config{E2B: E2BConfig{Workdir: "/"}},
		rt:  Runtime{Stderr: io.Discard},
	}
	_, _, _, err := backend.createSandbox(context.Background(), client, Repo{}, false, false, "")
	if err == nil || !strings.Contains(err.Error(), "too broad") {
		t.Fatalf("err=%v, want unsafe workspace error", err)
	}
	if client.createCalls != 0 {
		t.Fatalf("createCalls=%d, want 0", client.createCalls)
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

func TestE2BTimeoutCapsAtOneHour(t *testing.T) {
	if got := e2bTimeoutSeconds(90 * time.Minute); got != 3600 {
		t.Fatalf("timeout=%d want 3600", got)
	}
	if got := e2bTimeoutSeconds(0); got != 300 {
		t.Fatalf("default timeout=%d want 300", got)
	}
	if got := e2bTimeoutSeconds(42 * time.Minute); got != 2520 {
		t.Fatalf("custom timeout=%d want 2520", got)
	}
}

func TestE2BCreateSandboxCapsDefaultTTL(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeE2BSyncClient{}
	backend := &e2bBackend{
		cfg: Config{
			TTL:         90 * time.Minute,
			IdleTimeout: 30 * time.Minute,
			E2B:         E2BConfig{Template: "base"},
		},
		rt: Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	_, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if client.createReq.TimeoutSeconds != 3600 {
		t.Fatalf("timeout=%d want 3600", client.createReq.TimeoutSeconds)
	}
	if client.createReq.Metadata["ttl_secs"] != "3600" {
		t.Fatalf("metadata=%#v want capped ttl", client.createReq.Metadata)
	}
}

func TestE2BCreateSandboxReportsCleanupFailureAfterClaimFailure(t *testing.T) {
	origClaim := claimLeaseTargetForRepoConfig
	claimLeaseTargetForRepoConfig = func(_, _ string, _ Config, _ Server, _ SSHTarget, _ string, _ time.Duration, _ bool) error {
		return errors.New("claim write failed")
	}
	t.Cleanup(func() { claimLeaseTargetForRepoConfig = origClaim })

	client := &fakeE2BSyncClient{deleteErr: errors.New("delete failed")}
	var stderr bytes.Buffer
	backend := &e2bBackend{
		cfg: Config{E2B: E2BConfig{Template: "base"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: &stderr},
	}
	_, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, false, "")
	if err == nil {
		t.Fatal("expected claim failure")
	}
	for _, want := range []string{
		"claim write failed",
		"cleanup e2b sandbox sbx_1",
		"delete failed",
		"crabbox stop --provider e2b --id sbx_1 --reclaim",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%v, want %q", err, want)
		}
	}
	if len(client.deleteIDs) != 1 || client.deleteIDs[0] != "sbx_1" {
		t.Fatalf("deleteIDs=%#v, want sbx_1", client.deleteIDs)
	}
	if !client.deleteDeadlineSet {
		t.Fatal("delete cleanup did not use a bounded context")
	}
	if !strings.Contains(stderr.String(), "warning: cleanup e2b sandbox sbx_1") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestE2BRunReturnsSessionHandleForKeptSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeE2BSyncClient{}
	restore := swapNewE2BClient(client)
	defer restore()
	backend := &e2bBackend{
		cfg: Config{
			IdleTimeout: 30 * time.Minute,
			TTL:         2 * time.Minute,
			E2B:         E2BConfig{APIKey: "test", Workdir: "repo"},
		},
		rt: Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "repo", Root: t.TempDir()},
		Command: []string{"true"},
		Keep:    true,
		NoSync:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil {
		t.Fatal("missing session handle")
	}
	got := result.Session
	if got.Provider != e2bProvider || got.LeaseID == "" || got.Slug == "" || got.Reused || !got.Kept {
		t.Fatalf("session=%#v", got)
	}
	if got.CleanupCommand != "crabbox stop --provider e2b --id "+shellQuote(got.LeaseID) {
		t.Fatalf("cleanup command=%q", got.CleanupCommand)
	}
	if len(client.deleteIDs) != 0 {
		t.Fatalf("deleteIDs=%#v, want kept sandbox", client.deleteIDs)
	}
}

func TestE2BRunReturnsSessionHandleWhenKeepOnFailureRetainsSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeE2BSyncClient{processCodes: []int{0, 7}}
	restore := swapNewE2BClient(client)
	defer restore()
	var stderr bytes.Buffer
	backend := &e2bBackend{
		cfg: Config{
			IdleTimeout: 30 * time.Minute,
			TTL:         2 * time.Minute,
			E2B:         E2BConfig{APIKey: "test", Workdir: "repo"},
		},
		rt: Runtime{Stdout: io.Discard, Stderr: &stderr},
	}

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:          Repo{Name: "repo", Root: t.TempDir()},
		Command:       []string{"false"},
		KeepOnFailure: true,
		NoSync:        true,
		TimingJSON:    true,
	})
	var ee ExitError
	if !errors.As(err, &ee) || ee.Code != 7 {
		t.Fatalf("err=%v want ExitError code 7", err)
	}
	if result.Session == nil || !result.Session.Kept || result.Session.CleanupCommand == "" {
		t.Fatalf("session=%#v", result.Session)
	}
	if len(client.deleteIDs) != 0 {
		t.Fatalf("deleteIDs=%#v, want kept sandbox", client.deleteIDs)
	}
	var report map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		var candidate map[string]any
		if err := json.Unmarshal([]byte(line), &candidate); err == nil {
			report = candidate
		}
	}
	if report == nil {
		t.Fatalf("stderr does not contain timing JSON: %q", stderr.String())
	}
	if report["runStatus"] != "failed" || report["errorKind"] != "command-exit" {
		t.Fatalf("timing outcome status=%v kind=%v", report["runStatus"], report["errorKind"])
	}
}

func TestE2BRunCleanupUsesExactClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeE2BSyncClient{}
	restore := swapNewE2BClient(client)
	defer restore()
	backend := &e2bBackend{
		cfg: Config{
			IdleTimeout: 30 * time.Minute,
			TTL:         2 * time.Minute,
			E2B:         E2BConfig{APIKey: "test", Workdir: "repo"},
		},
		rt: Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "repo", Root: t.TempDir()},
		Command: []string{"true"},
		NoSync:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil || result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	if len(client.deleteIDs) != 1 || client.deleteIDs[0] != "sbx_1" {
		t.Fatalf("deleteIDs=%#v", client.deleteIDs)
	}
	if !client.deleteDeadlineSet {
		t.Fatal("run cleanup did not use a bounded context")
	}
	if _, exists, err := readLeaseClaimWithPresence(result.Session.LeaseID); err != nil || exists {
		t.Fatalf("cleanup claim exists=%t err=%v", exists, err)
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

func TestE2BCreateBindsExactSandboxAndEndpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeE2BSyncClient{}
	backend := &e2bBackend{
		cfg: Config{
			IdleTimeout: 30 * time.Minute,
			E2B: E2BConfig{
				APIURL:   "https://api.example.test/root/",
				Template: "base",
			},
		},
		rt: Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	leaseID, sandbox, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir()}, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, exact, err := resolveLeaseClaimForProviderScopeWithExact(leaseID, "endpoint:https://api.example.test/root")
	if err != nil || !ok || !exact {
		t.Fatalf("claim=%#v ok=%t exact=%t err=%v", claim, ok, exact, err)
	}
	if claim.CloudID != sandbox.SandboxID || claim.ProviderScope != "endpoint:https://api.example.test/root" {
		t.Fatalf("claim=%#v sandbox=%#v", claim, sandbox)
	}
}

func TestE2BStopRejectsLabelledButUnclaimedSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeE2BSyncClient{sandbox: e2bSandbox{
		SandboxID: "sbx_unclaimed",
		Metadata: map[string]string{
			"provider": e2bProvider,
			"crabbox":  "true",
			"lease":    "cbx_123456789abc",
			"slug":     "unclaimed",
		},
	}}
	restore := swapNewE2BClient(client)
	defer restore()
	backend := &e2bBackend{cfg: e2bClaimConfig(Config{}), rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	err := backend.Stop(context.Background(), StopRequest{ID: "sbx_unclaimed"})
	if err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("Stop err=%v, want exact-claim rejection", err)
	}
	if len(client.getIDs) != 0 || len(client.deleteIDs) != 0 {
		t.Fatalf("unclaimed Stop touched provider: gets=%#v deletes=%#v", client.getIDs, client.deleteIDs)
	}
}

func TestE2BStopChecksLiveOwnershipAndRemovesClaimAtomically(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeE2BSyncClient{}
	restore := swapNewE2BClient(client)
	defer restore()
	backend := &e2bBackend{
		cfg: Config{E2B: E2BConfig{APIURL: "https://api.example.test", Template: "base"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	leaseID, sandbox, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir()}, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	client.sandbox.Metadata["lease"] = "cbx_ffffffffffff"
	err = backend.Stop(context.Background(), StopRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "no longer has canonical ownership metadata") {
		t.Fatalf("mismatched Stop err=%v", err)
	}
	if len(client.deleteIDs) != 0 {
		t.Fatalf("mismatched Stop deleted %#v", client.deleteIDs)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || !exists {
		t.Fatalf("mismatched Stop lost recovery claim: exists=%t err=%v", exists, err)
	}

	client.sandbox.Metadata["lease"] = leaseID
	if err := backend.Stop(context.Background(), StopRequest{ID: leaseID}); err != nil {
		t.Fatalf("Stop exact claim: %v", err)
	}
	if len(client.deleteIDs) != 1 || client.deleteIDs[0] != sandbox.SandboxID {
		t.Fatalf("deleteIDs=%#v", client.deleteIDs)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("successful Stop claim exists=%t err=%v", exists, err)
	}
}

func TestE2BStopRetainsClaimWhenDeleteFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeE2BSyncClient{deleteErr: errors.New("delete failed")}
	restore := swapNewE2BClient(client)
	defer restore()
	backend := &e2bBackend{
		cfg: Config{E2B: E2BConfig{APIURL: "https://api.example.test", Template: "base"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	leaseID, sandbox, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir()}, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	err = backend.Stop(context.Background(), StopRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("Stop err=%v, want delete failure", err)
	}
	if len(client.deleteIDs) != 1 || client.deleteIDs[0] != sandbox.SandboxID {
		t.Fatalf("deleteIDs=%#v", client.deleteIDs)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || !exists {
		t.Fatalf("failed Stop lost recovery claim: exists=%t err=%v", exists, err)
	}
}

func TestE2BStopRejectsDifferentAPIEndpointBeforeRemoteRead(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeE2BSyncClient{}
	creator := &e2bBackend{
		cfg: Config{E2B: E2BConfig{APIURL: "https://api-a.example.test", Template: "base"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	leaseID, _, _, err := creator.createSandbox(context.Background(), client, Repo{Root: t.TempDir()}, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	client.getIDs = nil
	restore := swapNewE2BClient(client)
	defer restore()
	other := &e2bBackend{
		cfg: Config{E2B: E2BConfig{APIURL: "https://api-b.example.test"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	err = other.Stop(context.Background(), StopRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "different API endpoint") {
		t.Fatalf("Stop err=%v, want endpoint rejection", err)
	}
	if len(client.getIDs) != 0 || len(client.deleteIDs) != 0 {
		t.Fatalf("endpoint mismatch touched provider: gets=%#v deletes=%#v", client.getIDs, client.deleteIDs)
	}
}

func TestE2BReclaimAndStopAdoptsExactSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeE2BSyncClient{sandbox: e2bSandbox{
		SandboxID: "sbx_reclaim",
		Metadata: map[string]string{
			"provider": e2bProvider,
			"crabbox":  "true",
			"lease":    "cbx_123456789abc",
			"slug":     "reclaim-me",
		},
	}}
	restore := swapNewE2BClient(client)
	defer restore()
	backend := &e2bBackend{cfg: e2bClaimConfig(Config{}), rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	if err := backend.ReclaimAndStop(context.Background(), StopRequest{ID: "sbx_reclaim"}); err != nil {
		t.Fatalf("ReclaimAndStop: %v", err)
	}
	if len(client.deleteIDs) != 1 || client.deleteIDs[0] != "sbx_reclaim" {
		t.Fatalf("deleteIDs=%#v", client.deleteIDs)
	}
	if _, exists, err := readLeaseClaimWithPresence("cbx_123456789abc"); err != nil || exists {
		t.Fatalf("successful reclaim claim exists=%t err=%v", exists, err)
	}
}

type fakeE2BSyncClient struct {
	commands          []string
	users             []string
	sandbox           e2bSandbox
	createReq         e2bCreateSandboxRequest
	createCalls       int
	getIDs            []string
	getErr            error
	deleteIDs         []string
	deleteErr         error
	deleteDeadlineSet bool
	uploadPath        string
	uploaded          bytes.Buffer
	processCodes      []int
}

func swapNewE2BClient(fake e2bAPI) func() {
	prev := newE2BClient
	newE2BClient = func(Config, Runtime) (e2bAPI, error) { return fake, nil }
	return func() { newE2BClient = prev }
}

func (f *fakeE2BSyncClient) CreateSandbox(_ context.Context, req e2bCreateSandboxRequest) (e2bSandbox, error) {
	f.createReq = req
	f.createCalls++
	if f.sandbox.SandboxID != "" {
		return f.sandbox, nil
	}
	f.sandbox = e2bSandbox{SandboxID: "sbx_1", Metadata: req.Metadata, State: "running"}
	return f.sandbox, nil
}

func (f *fakeE2BSyncClient) ConnectSandbox(context.Context, string, int) (e2bSession, error) {
	return e2bSession{}, nil
}

func (f *fakeE2BSyncClient) GetSandbox(_ context.Context, sandboxID string) (e2bSandbox, error) {
	f.getIDs = append(f.getIDs, sandboxID)
	if f.getErr != nil {
		return e2bSandbox{}, f.getErr
	}
	return f.sandbox, nil
}

func (f *fakeE2BSyncClient) ListSandboxes(context.Context, map[string]string) ([]e2bSandbox, error) {
	return nil, nil
}

func (f *fakeE2BSyncClient) DeleteSandbox(ctx context.Context, sandboxID string) error {
	f.deleteIDs = append(f.deleteIDs, sandboxID)
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		f.deleteDeadlineSet = remaining > 0 && remaining <= e2bCleanupTimeout
	}
	return f.deleteErr
}

func (f *fakeE2BSyncClient) UploadFile(_ context.Context, _ e2bSession, targetPath string, r io.Reader) error {
	f.uploadPath = targetPath
	_, err := io.Copy(&f.uploaded, r)
	return err
}

func (f *fakeE2BSyncClient) StartProcess(_ context.Context, _ e2bSession, req e2bProcessRequest) (int, error) {
	f.commands = append(f.commands, req.Command)
	f.users = append(f.users, req.User)
	if len(f.processCodes) > 0 {
		code := f.processCodes[0]
		f.processCodes = f.processCodes[1:]
		return code, nil
	}
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
