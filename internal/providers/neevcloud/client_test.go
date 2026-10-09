package neevcloud

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	testKey     = "sk-nc-test-key"
	testSandbox = "01a1"
	sandboxPath = "/agent/api/v1beta1/orgs/org-1/projects/prj-1/sandboxes"
)

func testConfig(baseURL string) core.Config {
	var cfg core.Config
	cfg.Neevcloud = core.NeevcloudConfig{BaseURL: baseURL, OrgID: "org-1", ProjectID: "prj-1"}
	return cfg
}

func newTestClient(t *testing.T, server *httptest.Server) *client {
	t.Helper()
	c, err := newClientWithKey(testConfig(server.URL), core.Runtime{HTTP: server.Client()}, testKey)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestClientLifecycleUsesBearerAndProjectPath(t *testing.T) {
	var mu sync.Mutex
	var created map[string]any
	var listQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
			t.Errorf("%s %s Authorization=%q", r.Method, r.URL.Path, got)
		}
		if r.Header.Get("X-Api-Key") != "" {
			t.Errorf("lifecycle call sent X-Api-Key")
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == sandboxPath:
			_ = json.NewDecoder(r.Body).Decode(&created)
			writeJSON(w, http.StatusCreated, map[string]any{"id": testSandbox, "name": created["name"], "phase": "Creating"})
		case r.Method == http.MethodGet && r.URL.Path == sandboxPath:
			listQuery = r.URL.RawQuery
			writeJSON(w, http.StatusOK, map[string]any{"total": 1, "items": []map[string]any{
				{"id": testSandbox, "name": "crabbox-x", "phase": "Ready", "labels": map[string]string{"crabbox": "true", "provider": providerName}},
			}})
		case r.Method == http.MethodDelete && r.URL.Path == sandboxPath+"/"+testSandbox:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := newTestClient(t, server)
	ctx := context.Background()

	sb, err := c.CreateSandbox(ctx, shared.EnvdSandboxCreateRequest{
		TemplateID: "sb-ubuntu-26-04-dev", TimeoutSeconds: 600, AllowInternetAccess: true,
		Metadata: map[string]string{"crabbox": "true", "lease": "cbx_0123456789abcdef", "slug": "fast-coral"},
	})
	if err != nil || sb.SandboxID != testSandbox {
		t.Fatalf("CreateSandbox sb=%+v err=%v", sb, err)
	}
	if created["name"] != "crabbox-fast-coral-0123456789ab" || created["sandbox_template_id"] != "sb-ubuntu-26-04-dev" {
		t.Fatalf("create body=%v", created)
	}
	if egress := created["egress"].(map[string]any); egress["mode"] != "allow_list" || egress["allow_internet"] != true {
		t.Fatalf("egress=%v", egress)
	}
	if lc := created["lifecycle"].(map[string]any); lc["max_lifetime_seconds"].(float64) != 600 {
		t.Fatalf("lifecycle=%v", lc)
	}

	list, err := c.ListSandboxes(ctx, map[string]string{"provider": providerName, "crabbox": "true"})
	if err != nil || len(list) != 1 || list[0].State != "running" {
		t.Fatalf("ListSandboxes=%+v err=%v", list, err)
	}
	if listQuery != "label=crabbox%3Dtrue&label=provider%3Dneevcloud&limit=100&page=1" {
		t.Fatalf("list query=%s", listQuery)
	}
	if err := c.DeleteSandbox(ctx, testSandbox); err != nil {
		t.Fatalf("DeleteSandbox err=%v", err)
	}
}

func TestClientCreateConflictAdoptsOnlyOwnedSandbox(t *testing.T) {
	tests := []struct {
		name    string
		owner   string
		wantErr string
	}{
		{name: "success: same lease adopts", owner: "cbx_0123456789abcdef"},
		{name: "failure: other lease is refused", owner: "cbx_ffffffffffffffff", wantErr: "not owned by lease"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					writeJSON(w, http.StatusConflict, map[string]string{"error": "exists"})
					return
				}
				items := []map[string]any{}
				if r.URL.Query().Get("label") != "" && strings.Contains(r.URL.RawQuery, tt.owner) {
					items = append(items, map[string]any{"id": testSandbox, "name": "crabbox-fast-coral-0123456789ab", "phase": "Ready",
						"labels": map[string]string{"lease": tt.owner, "provider": providerName}})
				}
				writeJSON(w, http.StatusOK, map[string]any{"total": len(items), "items": items})
			}))
			defer server.Close()
			sb, err := newTestClient(t, server).CreateSandbox(context.Background(), shared.EnvdSandboxCreateRequest{
				Metadata: map[string]string{"lease": "cbx_0123456789abcdef", "slug": "fast-coral"},
			})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || sb.SandboxID != testSandbox {
				t.Fatalf("sb=%+v err=%v", sb, err)
			}
		})
	}
}

func TestClientListPagesUntilTotal(t *testing.T) {
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		n := listPageLimit
		if page == "2" {
			n = 20
		}
		items := make([]map[string]any, n)
		for i := range items {
			items[i] = map[string]any{"id": page + "-" + strconv.Itoa(i), "labels": map[string]string{"provider": providerName}}
		}
		writeJSON(w, http.StatusOK, map[string]any{"total": listPageLimit + 20, "items": items})
	}))
	defer server.Close()
	list, err := newTestClient(t, server).ListSandboxes(context.Background(), map[string]string{"provider": providerName})
	if err != nil || len(list) != listPageLimit+20 || strings.Join(pages, ",") != "1,2" {
		t.Fatalf("len=%d pages=%v err=%v", len(list), pages, err)
	}
}

func TestClientConnectWaitsForReady(t *testing.T) {
	old := readyPollInterval
	readyPollInterval = time.Millisecond
	t.Cleanup(func() { readyPollInterval = old })
	tests := []struct {
		name    string
		phases  []string
		wantErr string
	}{
		{name: "success: Creating then Ready", phases: []string{"Creating", "Ready"}},
		{name: "failure: RestoreFailed stops the wait", phases: []string{"RestoreFailed"}, wantErr: "RestoreFailed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				phase := tt.phases[min(calls, len(tt.phases)-1)]
				calls++
				body := map[string]any{"id": testSandbox, "phase": phase}
				if phase == phaseReady {
					body["connect_url"] = server.URL
				}
				writeJSON(w, http.StatusOK, body)
			}))
			defer server.Close()
			session, err := newTestClient(t, server).ConnectSandbox(context.Background(), testSandbox, 0)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || session.Domain != server.URL {
				t.Fatalf("session=%+v err=%v", session, err)
			}
		})
	}
}

func TestClientUploadUsesTusChunks(t *testing.T) {
	content := bytes.Repeat([]byte("x"), 2*uploadChunkSize+5)
	var got bytes.Buffer
	var offsets []string
	var meta string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != testKey || r.Header.Get("Authorization") != "" {
			t.Errorf("runtime auth headers wrong on %s %s", r.Method, r.URL.Path)
		}
		switch r.Method {
		case http.MethodPost:
			meta = r.Header.Get("Upload-Metadata")
			if r.Header.Get("Upload-Length") != strconv.Itoa(len(content)) {
				t.Errorf("Upload-Length=%s", r.Header.Get("Upload-Length"))
			}
			w.Header().Set("Location", "/v1/files/uploads/abc")
			w.WriteHeader(http.StatusCreated)
		case http.MethodPatch:
			offsets = append(offsets, r.Header.Get("Upload-Offset"))
			n, _ := io.Copy(&got, r.Body)
			if n > uploadChunkSize {
				t.Errorf("chunk of %d bytes", n)
			}
			w.Header().Set("Upload-Offset", strconv.Itoa(got.Len()))
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	session := shared.EnvdSandboxSession{SandboxID: testSandbox, Domain: server.URL}
	if err := newTestClient(t, server).UploadFile(context.Background(), session, "/workspace/.crabbox-sync-1.tgz", bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), content) || strings.Join(offsets, ",") != "0,1048576,2097152" {
		t.Fatalf("bytes=%d offsets=%v", got.Len(), offsets)
	}
	if want := "path " + base64.StdEncoding.EncodeToString([]byte(".crabbox-sync-1.tgz")); meta != want {
		t.Fatalf("Upload-Metadata=%q want %q", meta, want)
	}
}

func TestClientUploadRefusesBadTargets(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("cross-origin Location must not be followed")
	}))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", other.URL+"/v1/files/uploads/abc")
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	c := newTestClient(t, server)
	session := shared.EnvdSandboxSession{Domain: server.URL}
	tests := []struct {
		name    string
		target  string
		wantErr string
	}{
		{name: "failure: Location on another origin", target: "/workspace/a.tgz", wantErr: "leaves the sandbox origin"},
		{name: "failure: path outside workspace", target: "/etc/passwd", wantErr: "must be under /workspace"},
		{name: "failure: escape via dot-dot", target: "/workspace/../etc/passwd", wantErr: "must be under /workspace"},
		{name: "failure: workspace root itself", target: "/workspace", wantErr: "must name a file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := c.UploadFile(context.Background(), session, tt.target, strings.NewReader("data"))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err=%v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestClientStartProcessRequest(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/exec" || r.Header.Get("X-Protocol-Version") != protocolVersion {
			t.Errorf("path=%s protocol=%q", r.URL.Path, r.Header.Get("X-Protocol-Version"))
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{"type":"stdout","data":"aGk="}`+"\n"+`{"type":"exit","exit_code":3}`+"\n")
	}))
	defer server.Close()
	var stdout bytes.Buffer
	code, err := newTestClient(t, server).StartProcess(context.Background(), shared.EnvdSandboxSession{Domain: server.URL}, shared.EnvdSandboxProcessRequest{
		Command: "make test", CWD: "/workspace/crabbox", Timeout: 3 * time.Hour, Stdout: &stdout,
		Env: map[string]string{"B": "2", "A": "1", "NEEV_API_KEY": testKey, "CRABBOX_NEEVCLOUD_API_KEY": testKey},
	})
	if err != nil || code != 3 || stdout.String() != "hi" {
		t.Fatalf("code=%d stdout=%q err=%v", code, stdout.String(), err)
	}
	if body["command"] != "/bin/sh" || body["cwd"] != "crabbox" || body["timeout_ms"].(float64) != float64(execCeiling.Milliseconds()) {
		t.Fatalf("exec body=%v", body)
	}
	if args, _ := json.Marshal(body["args"]); string(args) != `["-c","make test"]` {
		t.Fatalf("args=%s", args)
	}
	if env, _ := json.Marshal(body["env"]); string(env) != `["A=1","B=2"]` {
		t.Fatalf("env=%s (api key must not be forwarded)", env)
	}
}

func TestClientStartProcessRejectsOtherUser(t *testing.T) {
	c := &client{}
	_, err := c.StartProcess(context.Background(), shared.EnvdSandboxSession{}, shared.EnvdSandboxProcessRequest{User: "ubuntu"})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("err=%v", err)
	}
}

func TestParseExecStream(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	tests := []struct {
		name       string
		stream     string
		wantCode   int
		wantStdout string
		wantStderr string
		wantErr    string
	}{
		{name: "success: interleaved output and exit 0",
			stream:     `{"type":"stdout","data":"` + b64("a") + `"}` + "\n\n" + `{"type":"stderr","data":"` + b64("b") + `"}` + "\n" + `{"type":"exit","exit_code":0}`,
			wantStdout: "a", wantStderr: "b"},
		{name: "success: non-zero exit is not an error", stream: `{"type":"exit","exit_code":42}`, wantCode: 42},
		{name: "failure: error event redacts the key", stream: `{"type":"error","reason_code":"timeout","message":"bad ` + testKey + `"}`, wantErr: "timeout"},
		{name: "failure: stream ends without exit", stream: `{"type":"stdout","data":"` + b64("a") + `"}`, wantStdout: "a", wantErr: "without an exit event"},
		{name: "failure: exit without code", stream: `{"type":"exit"}`, wantErr: "no exit_code"},
		{name: "failure: bad base64", stream: `{"type":"stdout","data":"%%%"}`, wantErr: "decode exec stdout"},
		{name: "failure: not json", stream: `nope`, wantErr: "decode exec event"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, err := parseExecStream(strings.NewReader(tt.stream), &stdout, &stderr, testKey)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || strings.Contains(err.Error(), testKey) {
					t.Fatalf("err=%v, want %q without key", err, tt.wantErr)
				}
			} else if err != nil || code != tt.wantCode {
				t.Fatalf("code=%d err=%v", code, err)
			}
			if stdout.String() != tt.wantStdout || stderr.String() != tt.wantStderr {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestClientErrorsRedactKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bad key " + testKey})
	}))
	defer server.Close()
	_, err := newTestClient(t, server).GetSandbox(context.Background(), testSandbox)
	if err == nil || strings.Contains(err.Error(), testKey) {
		t.Fatalf("err=%v leaked or missing", err)
	}
}

func TestClientRefusesCrossOriginRedirect(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("redirect target must not be reached")
	}))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer server.Close()
	_, err := newTestClient(t, server).GetSandbox(context.Background(), testSandbox)
	if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("err=%v", err)
	}
}
