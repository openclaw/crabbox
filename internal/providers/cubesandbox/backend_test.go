package cubesandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
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
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type cubesandboxRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn cubesandboxRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestCubeSandboxClientRedactsReflectedCredentials(t *testing.T) {
	t.Run("API key", func(t *testing.T) {
		const secret = "cubesandbox-api-secret"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"X-API-Key: `+secret+` quota exceeded"}`)
		}))
		defer server.Close()
		client := &cubesandboxClient{apiKey: secret, apiURL: server.URL, httpClient: server.Client()}
		_, err := client.ListSandboxes(context.Background(), nil)
		assertCubeSandboxRedactedError(t, err, secret)
	})

	t.Run("envd access token", func(t *testing.T) {
		const secret = "envd-access-secret"
		httpClient := &http.Client{Transport: cubesandboxRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Status:     "401 Unauthorized",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"message":"Bearer ` + secret + ` quota exceeded"}`)),
				Request:    req,
			}, nil
		})}
		client := &cubesandboxClient{httpClient: httpClient}
		_, err := client.StartProcess(context.Background(), cubesandboxSession{SandboxID: "sbx_1", Domain: "example.test", EnvdAccessToken: secret}, cubesandboxProcessRequest{Command: "true"})
		assertCubeSandboxRedactedError(t, err, secret)
	})
}

func assertCubeSandboxRedactedError(t *testing.T, err error, secret string) {
	t.Helper()
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("error=%v, want redacted useful provider error", err)
	}
}

func TestCubeSandboxProcessStreamRedactsReflectedCredential(t *testing.T) {
	const secret = "envd-stream-secret"
	t.Run("end stream error", func(t *testing.T) {
		body := cubesandboxTestEnvelope(2, map[string]any{"error": map[string]any{"code": "unauthorized", "message": "Bearer " + secret + " quota exceeded"}})
		_, err := parseCubeSandboxProcessStream(bytes.NewReader(body), io.Discard, io.Discard, secret)
		assertCubeSandboxRedactedError(t, err, secret)
	})
	t.Run("process end diagnostic", func(t *testing.T) {
		body := cubesandboxTestEnvelope(0, map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": 1, "exited": false, "error": "Bearer " + secret + " quota exceeded"}}})
		var stderr bytes.Buffer
		code, err := parseCubeSandboxProcessStream(bytes.NewReader(body), io.Discard, &stderr, secret)
		if code != 1 {
			t.Fatalf("code=%d, want 1", code)
		}
		assertCubeSandboxRedactedError(t, err, secret)
		if strings.Contains(stderr.String(), secret) || !strings.Contains(stderr.String(), "[redacted]") || !strings.Contains(stderr.String(), "quota exceeded") {
			t.Fatalf("stderr=%q, want redacted useful process diagnostic", stderr.String())
		}
	})
	t.Run("process end without diagnostic", func(t *testing.T) {
		body := cubesandboxTestEnvelope(0, map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": 0, "exited": false}}})
		code, err := parseCubeSandboxProcessStream(bytes.NewReader(body), io.Discard, io.Discard)
		if code != 1 || err == nil || !strings.Contains(err.Error(), "did not exit normally") {
			t.Fatalf("code=%d err=%v, want abnormal termination failure", code, err)
		}
	})
}

func TestParseCubeSandboxProcessStream(t *testing.T) {
	body := bytes.Join([][]byte{
		cubesandboxTestEnvelope(0, map[string]any{"event": map[string]any{"start": map[string]any{"pid": 42}}}),
		cubesandboxTestEnvelope(0, map[string]any{"event": map[string]any{"data": map[string]any{"stdout": base64.StdEncoding.EncodeToString([]byte("hello"))}}}),
		cubesandboxTestEnvelope(0, map[string]any{"event": map[string]any{"data": map[string]any{"stderr": base64.StdEncoding.EncodeToString([]byte("warn"))}}}),
		cubesandboxTestEnvelope(0, map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": 7, "exited": true}}}),
		cubesandboxTestEnvelope(2, map[string]any{}),
	}, nil)
	var stdout, stderr bytes.Buffer
	code, err := parseCubeSandboxProcessStream(bytes.NewReader(body), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 || stdout.String() != "hello" || stderr.String() != "warn" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestValidateCubeSandboxAPIURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "https", raw: "HTTPS://API.CubeSandbox.APP:443/v1/", want: "https://api.cubesandbox.app/v1"},
		{name: "loopback", raw: "http://127.0.0.1:8080/api/", want: "http://127.0.0.1:8080/api"},
		{name: "localhost", raw: "http://localhost:8080", want: "http://localhost:8080"},
		{name: "IPv6 loopback", raw: "http://[::1]:8080/", want: "http://[::1]:8080"},
		{name: "remote HTTP", raw: "http://api.cubesandbox.app", want: "http://api.cubesandbox.app"},
		{name: "relative", raw: "/api", wantErr: "absolute"},
		{name: "userinfo", raw: "https://user:pass@api.cubesandbox.app", wantErr: "must not contain userinfo"},
		{name: "query", raw: "https://api.cubesandbox.app?token=secret", wantErr: "must not contain userinfo"},
		{name: "fragment", raw: "https://api.cubesandbox.app/#secret", wantErr: "must not contain userinfo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateCubeSandboxAPIURL(tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validateCubeSandboxAPIURL(%q) error = %v, want %q", tt.raw, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("validateCubeSandboxAPIURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestCubeSandboxClientRejectsInvalidProxyScheme(t *testing.T) {
	_, err := newCubeSandboxClient(Config{CubeSandbox: CubeSandboxConfig{
		APIURL:      "http://127.0.0.1:3000",
		ProxyScheme: "htps",
	}}, Runtime{})
	if err == nil || !strings.Contains(err.Error(), "must be http or https") {
		t.Fatalf("err=%v, want invalid proxy scheme", err)
	}
}

func TestCubeSandboxClientNormalizesUploadUser(t *testing.T) {
	api, err := newCubeSandboxClient(Config{CubeSandbox: CubeSandboxConfig{
		APIURL: "http://127.0.0.1:3000",
		User:   " ubuntu ",
	}}, Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	client := api.(*cubesandboxClient)
	if client.user != "ubuntu" {
		t.Fatalf("user=%q, want normalized upload user", client.user)
	}
}

func TestParseCubeSandboxProcessStreamRequiresEndEvent(t *testing.T) {
	body := bytes.Join([][]byte{
		cubesandboxTestEnvelope(0, map[string]any{"event": map[string]any{"data": map[string]any{"stdout": base64.StdEncoding.EncodeToString([]byte("partial"))}}}),
		cubesandboxTestEnvelope(2, map[string]any{}),
	}, nil)
	var stdout bytes.Buffer
	code, err := parseCubeSandboxProcessStream(bytes.NewReader(body), &stdout, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "without end event") {
		t.Fatalf("code=%d err=%v, want missing end event error", code, err)
	}
	if stdout.String() != "partial" {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestCubeSandboxCommandString(t *testing.T) {
	if got := cubesandboxCommandString([]string{"go", "test", "./..."}, false); got != "'go' 'test' './...'" {
		t.Fatalf("plain command=%q", got)
	}
	if got := cubesandboxCommandString([]string{"FOO=bar", "go", "test"}, false); !strings.Contains(got, "FOO=") || !strings.Contains(got, "'go'") {
		t.Fatalf("env command=%q", got)
	}
	if got := cubesandboxCommandString([]string{"pnpm install && pnpm test"}, true); got != "pnpm install && pnpm test" {
		t.Fatalf("shell command=%q", got)
	}
}

func TestCubeSandboxWorkspacePath(t *testing.T) {
	if got := cubesandboxWorkspacePath(Config{}); got != "/root/crabbox" {
		t.Fatalf("workspace=%q", got)
	}
	if got := cubesandboxWorkspacePath(Config{CubeSandbox: CubeSandboxConfig{Workdir: "repo"}}); got != "/root/repo" {
		t.Fatalf("workspace=%q", got)
	}
	if got := cubesandboxWorkspacePath(Config{CubeSandbox: CubeSandboxConfig{User: "ubuntu", Workdir: "repo"}}); got != "/home/ubuntu/repo" {
		t.Fatalf("workspace=%q", got)
	}
	if got := cubesandboxWorkspacePath(Config{CubeSandbox: CubeSandboxConfig{User: "root", Workdir: "repo"}}); got != "/root/repo" {
		t.Fatalf("workspace=%q", got)
	}
	if got := cubesandboxWorkspacePath(Config{CubeSandbox: CubeSandboxConfig{Workdir: "/work/repo"}}); got != "/work/repo" {
		t.Fatalf("workspace=%q", got)
	}
}

func TestCubeSandboxProcessUser(t *testing.T) {
	tests := []struct {
		name    string
		user    string
		want    string
		wantErr string
	}{
		{name: "empty defaults to root", user: "", want: "root"},
		{name: "trims user", user: " ubuntu ", want: "ubuntu"},
		{name: "root allowed", user: "root", want: "root"},
		{name: "rejects slash", user: "../tmp", wantErr: "not a path"},
		{name: "rejects backslash", user: `team\dev`, wantErr: "not a path"},
		{name: "rejects dot", user: ".", wantErr: "not a path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cubesandboxProcessUser(tt.user)
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

func TestCubeSandboxWarmupRejectsUnsafeUserBeforeClient(t *testing.T) {
	backend := &cubesandboxBackend{
		cfg: Config{CubeSandbox: CubeSandboxConfig{User: "../tmp"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	err := backend.Warmup(context.Background(), WarmupRequest{})
	if err == nil || !strings.Contains(err.Error(), "invalid cubesandbox.user") {
		t.Fatalf("err=%v, want invalid cubesandbox.user", err)
	}
	if strings.Contains(err.Error(), "template") {
		t.Fatalf("validated user after client setup: %v", err)
	}
}

func TestProviderSpecAdvertisesRunSession(t *testing.T) {
	if !(Provider{}).Spec().Features.Has(core.FeatureRunSession) {
		t.Fatalf("features=%#v want run session", Provider{}.Spec().Features)
	}
}

func TestCleanCubeSandboxWorkspacePath(t *testing.T) {
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
			got, err := cleanCubeSandboxWorkspacePath(tt.workspace)
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

func TestCubeSandboxClientCreateConnectListAndDeleteUseOfficialRESTShape(t *testing.T) {
	var createBody map[string]any
	listHits := 0
	deleteHit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer cubesandbox_test" {
			t.Fatalf("Authorization=%q", got)
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
			if got := r.URL.Query().Get("metadata"); !strings.Contains(got, "provider=cubesandbox") || !strings.Contains(got, "crabbox=true") {
				t.Fatalf("metadata query=%q", got)
			}
			if listHits == 1 {
				w.Header().Set("x-next-token", "next")
				_ = json.NewEncoder(w).Encode([]map[string]any{{"templateID": "base", "sandboxID": "sbx_1", "state": "running", "metadata": map[string]string{"provider": "cubesandbox", "crabbox": "true"}}})
				return
			}
			if got := r.URL.Query().Get("nextToken"); got != "next" {
				t.Fatalf("nextToken=%q", got)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"templateID": "base", "sandboxID": "sbx_2", "state": "running", "metadata": map[string]string{"provider": "cubesandbox", "crabbox": "true"}}})
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

	api, err := newCubeSandboxClient(Config{CubeSandbox: CubeSandboxConfig{APIKey: "cubesandbox_test", APIURL: srv.URL}}, Runtime{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := api.CreateSandbox(t.Context(), cubesandboxCreateSandboxRequest{
		TemplateID:          "base",
		TimeoutSeconds:      60,
		AllowInternetAccess: true,
		Metadata:            map[string]string{"provider": "cubesandbox"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.SandboxID != "sbx_1" {
		t.Fatalf("sandbox=%#v", sandbox)
	}
	if createBody["templateID"] != "base" || createBody["timeout"].(float64) != 60 || createBody["secure"] != nil || createBody["allow_internet_access"] != nil {
		t.Fatalf("create body=%v", createBody)
	}
	session, err := api.ConnectSandbox(t.Context(), "sbx_1", 120)
	if err != nil {
		t.Fatal(err)
	}
	if session.SandboxID != "sbx_1" || session.EnvdAccessToken != "envd-token" {
		t.Fatalf("session=%#v", session)
	}
	items, err := api.ListSandboxes(t.Context(), map[string]string{"provider": "cubesandbox", "crabbox": "true"})
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

func TestCubeSandboxAPIClientRefusesCrossOriginRedirectBeforeReplay(t *testing.T) {
	var targetRequests int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests++
		t.Errorf("redirect target received %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer target.Close()

	trusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer trusted.Close()

	api, err := newCubeSandboxClient(Config{CubeSandbox: CubeSandboxConfig{APIKey: "cubesandbox_test", APIURL: trusted.URL}}, Runtime{HTTP: trusted.Client()})
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

func TestCubeSandboxEnvdClientRefusesCrossOriginRedirectBeforeReplay(t *testing.T) {
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
	resp, err := secureCubeSandboxHTTPClient(trusted.Client(), trustedURL).Do(req)
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

func TestCubeSandboxClientRoutesEnvdToPort49983(t *testing.T) {
	client := &cubesandboxClient{
		domain:      "cube.test",
		proxyScheme: "http",
	}
	session := cubesandboxSession{SandboxID: "sbx_1", Domain: "cube.test"}
	endpoint := client.envdURL(session, "/process.Process/Start")
	if got, want := endpoint, "http://49983-sbx_1.cube.test/process.Process/Start"; got != want {
		t.Fatalf("endpoint=%q, want %q", got, want)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	client.setEnvdHeaders(req, session)
	if got, want := req.Header.Get("E2b-Sandbox-Port"), "49983"; got != want {
		t.Fatalf("E2b-Sandbox-Port=%q, want %q", got, want)
	}
}

func TestCubeSandboxHTTPSProxyPreservesVirtualHostForSNI(t *testing.T) {
	var gotHost, gotServerName string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(serverURL.Port())
	if err != nil {
		t.Fatal(err)
	}
	source := server.Client()
	transport := source.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.InsecureSkipVerify = true // test verifies SNI explicitly below
	transport.TLSClientConfig.VerifyConnection = func(state tls.ConnectionState) error {
		gotServerName = state.ServerName
		return nil
	}
	source.Transport = transport
	envdClient, err := cubeSandboxDataPlaneHTTPClient(source, serverURL.Hostname(), port)
	if err != nil {
		t.Fatal(err)
	}
	client := &cubesandboxClient{
		domain:      "cube.test",
		proxyScheme: "https",
		httpClient:  source,
		envdClient:  envdClient,
	}
	session := cubesandboxSession{SandboxID: "sbx_1", Domain: "cube.test", EnvdAccessToken: "test-token"}
	if err := client.UploadFile(t.Context(), session, "/tmp/proof", strings.NewReader("proof")); err != nil {
		t.Fatal(err)
	}
	want := "49983-sbx_1.cube.test"
	if gotServerName != want || gotHost != want {
		t.Fatalf("SNI=%q Host=%q, want virtual host %q", gotServerName, gotHost, want)
	}
}

func TestCubeSandboxClientFollowsSameOriginRedirect(t *testing.T) {
	var redirectedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/sandboxes":
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
		case "/redirected":
			redirectedAuth = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode([]cubesandboxSandbox{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	api, err := newCubeSandboxClient(Config{CubeSandbox: CubeSandboxConfig{APIKey: "cubesandbox_test", APIURL: server.URL}}, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.ListSandboxes(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if redirectedAuth != "Bearer cubesandbox_test" {
		t.Fatalf("redirected auth = %q", redirectedAuth)
	}
}

func TestCubeSandboxClientPreservesCallerRedirectPolicy(t *testing.T) {
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
	api, err := newCubeSandboxClient(Config{CubeSandbox: CubeSandboxConfig{APIKey: "cubesandbox_test", APIURL: server.URL}}, Runtime{HTTP: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.ListSandboxes(t.Context(), nil)
	if !errors.Is(err, callerErr) || callerChecks != 1 {
		t.Fatalf("ListSandboxes error = %v, caller checks = %d", err, callerChecks)
	}
}

func TestCubeSandboxUploadFileRejectsMalformedDomainBeforeProducer(t *testing.T) {
	client := &cubesandboxClient{apiKey: "cubesandbox_test", domain: "%zz", httpClient: http.DefaultClient}
	err := client.UploadFile(context.Background(), cubesandboxSession{SandboxID: "sbx_1"}, "/tmp/archive.tgz", strings.NewReader("archive"))
	if err == nil {
		t.Fatal("UploadFile err=nil, want malformed URL error")
	}
	runtime.Gosched()
	time.Sleep(10 * time.Millisecond)
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	if bytes.Contains(buf[:n], []byte("github.com/openclaw/crabbox/internal/providers/cubesandbox.(*cubesandboxClient).UploadFile.func1")) {
		t.Fatalf("multipart producer goroutine still running after malformed URL:\n%s", buf[:n])
	}
}

func TestCubeSandboxSyncWorkspaceUploadsRepoArchive(t *testing.T) {
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
	client := &fakeCubeSandboxSyncClient{}
	cfg := Config{CubeSandbox: CubeSandboxConfig{User: "ubuntu", Workdir: "repo"}}
	cfg.Sync.Delete = true
	backend := &cubesandboxBackend{
		cfg: cfg,
		rt:  Runtime{Stderr: io.Discard},
	}
	workspace := cubesandboxWorkspacePath(backend.cfg)
	_, _, err := backend.syncWorkspace(context.Background(), client, cubesandboxSession{SandboxID: "sbx_1"}, RunRequest{
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
	if !client.commandContains("mkdir -p '/home/ubuntu/.repo.crabbox-sync-") || !client.commandContains("tar -xzf") {
		t.Fatalf("commands=%#v", client.commands)
	}
	extractIndex, replaceIndex := -1, -1
	for i, command := range client.commands {
		if strings.Contains(command, "tar -xzf") {
			extractIndex = i
		}
		if strings.Contains(command, "mv '/home/ubuntu/repo'") {
			replaceIndex = i
		}
		if strings.Contains(command, "rm -rf '/home/ubuntu/repo'") {
			t.Fatalf("workspace deleted before replacement: %q", command)
		}
	}
	if extractIndex < 0 || replaceIndex <= extractIndex {
		t.Fatalf("commands=%#v, want staged extract before replacement", client.commands)
	}
	if !client.userContains("ubuntu") {
		t.Fatalf("users=%#v", client.users)
	}
}

func TestCubeSandboxSyncWorkspaceCleansRemoteArchiveWhenExtractFails(t *testing.T) {
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
	client := &fakeCubeSandboxSyncClient{processCodes: []int{0, 7, 0}}
	cfg := Config{CubeSandbox: CubeSandboxConfig{User: "ubuntu", Workdir: "repo"}}
	cfg.Sync.Delete = true
	backend := &cubesandboxBackend{
		cfg: cfg,
		rt:  Runtime{Stderr: io.Discard},
	}
	workspace := cubesandboxWorkspacePath(backend.cfg)
	_, _, err := backend.syncWorkspace(context.Background(), client, cubesandboxSession{SandboxID: "sbx_1"}, RunRequest{
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
	for _, command := range client.commands {
		if strings.Contains(command, "rm -rf '/home/ubuntu/repo'") || strings.Contains(command, "mv '/home/ubuntu/repo'") {
			t.Fatalf("failed staged extraction modified existing workspace: %q", command)
		}
	}
}

func TestCubeSandboxPrepareWorkspaceRejectsUnsafePath(t *testing.T) {
	client := &fakeCubeSandboxSyncClient{}
	cfg := Config{}
	cfg.Sync.Delete = true
	backend := &cubesandboxBackend{
		cfg: cfg,
		rt:  Runtime{Stderr: io.Discard},
	}
	err := backend.prepareWorkspace(context.Background(), client, cubesandboxSession{SandboxID: "sbx_1"}, "/")
	if err == nil || !strings.Contains(err.Error(), "too broad") {
		t.Fatalf("err=%v, want unsafe workspace error", err)
	}
	if len(client.commands) != 0 {
		t.Fatalf("commands=%#v, want none", client.commands)
	}
}

func TestCubeSandboxPrepareWorkspacePreservesExistingWithoutSync(t *testing.T) {
	client := &fakeCubeSandboxSyncClient{}
	backend := &cubesandboxBackend{rt: Runtime{Stderr: io.Discard}}
	if err := backend.prepareWorkspace(context.Background(), client, cubesandboxSession{SandboxID: "sbx_1"}, "/root/repo"); err != nil {
		t.Fatal(err)
	}
	if len(client.commands) != 1 || client.commands[0] != "mkdir -p '/root/repo'" {
		t.Fatalf("commands=%#v, want non-destructive mkdir", client.commands)
	}
}

func TestCubeSandboxCreateSandboxRejectsUnsafeWorkdirBeforeAPI(t *testing.T) {
	client := &fakeCubeSandboxSyncClient{}
	backend := &cubesandboxBackend{
		cfg: Config{CubeSandbox: CubeSandboxConfig{Workdir: "/"}},
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

func TestCubeSandboxStatusReady(t *testing.T) {
	for _, status := range []string{"", "running"} {
		if !cubesandboxStatusReady(status) {
			t.Fatalf("expected %q ready", status)
		}
	}
	if cubesandboxStatusReady("paused") {
		t.Fatal("paused should not be ready")
	}
}

func TestCubeSandboxTimeoutPreservesRequestedTTL(t *testing.T) {
	if got := cubesandboxTimeoutSeconds(90 * time.Minute); got != 5400 {
		t.Fatalf("timeout=%d want 5400", got)
	}
	if got := cubesandboxTimeoutSeconds(0); got != 300 {
		t.Fatalf("default timeout=%d want 300", got)
	}
	if got := cubesandboxTimeoutSeconds(42 * time.Minute); got != 2520 {
		t.Fatalf("custom timeout=%d want 2520", got)
	}
}

func TestCubeSandboxCreateSandboxPreservesDefaultTTL(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // keep=true writes a lease claim; keep it out of the real state dir
	client := &fakeCubeSandboxSyncClient{}
	backend := &cubesandboxBackend{
		cfg: Config{
			TTL:         90 * time.Minute,
			IdleTimeout: 30 * time.Minute,
			CubeSandbox: CubeSandboxConfig{Template: "base"},
		},
		rt: Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	_, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if client.createReq.TimeoutSeconds != 5400 {
		t.Fatalf("timeout=%d want 5400", client.createReq.TimeoutSeconds)
	}
	if client.createReq.Metadata["ttl_secs"] != "5400" {
		t.Fatalf("metadata=%#v want requested ttl", client.createReq.Metadata)
	}
}

func TestCubeSandboxCreateSandboxReportsCleanupFailureAfterClaimFailure(t *testing.T) {
	origClaim := claimLeaseTargetForRepoConfig
	claimLeaseTargetForRepoConfig = func(_, _ string, _ Config, _ Server, _ SSHTarget, _ string, _ time.Duration, _ bool) error {
		return errors.New("claim write failed")
	}
	t.Cleanup(func() { claimLeaseTargetForRepoConfig = origClaim })

	client := &fakeCubeSandboxSyncClient{deleteErr: errors.New("delete failed")}
	var stderr bytes.Buffer
	backend := &cubesandboxBackend{
		cfg: Config{CubeSandbox: CubeSandboxConfig{Template: "base"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: &stderr},
	}
	_, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, false, "")
	if err == nil {
		t.Fatal("expected claim failure")
	}
	for _, want := range []string{
		"claim write failed",
		"cleanup cubesandbox sandbox sbx_1",
		"delete failed",
		"crabbox stop --provider cubesandbox --id sbx_1 --reclaim",
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
	if !strings.Contains(stderr.String(), "warning: cleanup cubesandbox sandbox sbx_1") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestCubeSandboxRunBoundsAutomaticCleanup(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeCubeSandboxSyncClient{}
	restore := swapNewCubeSandboxClient(client)
	defer restore()
	backend := &cubesandboxBackend{
		cfg: Config{CubeSandbox: CubeSandboxConfig{Template: "base"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: t.TempDir()},
		Command: []string{"true"},
		NoSync:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !client.deleteDeadlineSet {
		t.Fatal("automatic cleanup did not use a bounded context")
	}
}

func TestCubeSandboxRunStripsProviderAuthenticationEnv(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeCubeSandboxSyncClient{}
	restore := swapNewCubeSandboxClient(client)
	defer restore()
	var stderr bytes.Buffer
	backend := &cubesandboxBackend{
		cfg: Config{CubeSandbox: CubeSandboxConfig{Template: "base"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: &stderr},
	}
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: t.TempDir()},
		Command: []string{"true"},
		NoSync:  true,
		Env: map[string]string{
			"CRABBOX_CUBESANDBOX_API_KEY": "crabbox-secret",
			"CUBE_API_KEY":                "cube-secret",
			"E2B_API_KEY":                 "fallback-secret",
			"SAFE_VALUE":                  "forwarded",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.processEnvs) < 2 {
		t.Fatalf("process envs=%#v, want workspace and command calls", client.processEnvs)
	}
	commandEnv := client.processEnvs[len(client.processEnvs)-1]
	if len(commandEnv) != 1 || commandEnv["SAFE_VALUE"] != "forwarded" {
		t.Fatalf("command env=%#v, want only safe value", commandEnv)
	}
	for _, name := range []string{"CRABBOX_CUBESANDBOX_API_KEY", "CUBE_API_KEY", "E2B_API_KEY"} {
		if !strings.Contains(stderr.String(), name) {
			t.Fatalf("stderr=%q, want stripped variable %s", stderr.String(), name)
		}
	}
	for _, secret := range []string{"crabbox-secret", "cube-secret", "fallback-secret"} {
		if strings.Contains(stderr.String(), secret) {
			t.Fatalf("stderr leaked secret %q: %q", secret, stderr.String())
		}
	}
}

func TestCubeSandboxRunReturnsSessionHandleForKeptSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeCubeSandboxSyncClient{}
	restore := swapNewCubeSandboxClient(client)
	defer restore()
	backend := &cubesandboxBackend{
		cfg: Config{
			IdleTimeout: 30 * time.Minute,
			TTL:         2 * time.Minute,
			CubeSandbox: CubeSandboxConfig{APIKey: "test", Template: "base", Workdir: "repo"},
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
	if got.Provider != providerName || got.LeaseID == "" || got.Slug == "" || got.Reused || !got.Kept {
		t.Fatalf("session=%#v", got)
	}
	if got.CleanupCommand != "crabbox stop --provider cubesandbox --id "+shellQuote(got.LeaseID) {
		t.Fatalf("cleanup command=%q", got.CleanupCommand)
	}
	if len(client.deleteIDs) != 0 {
		t.Fatalf("deleteIDs=%#v, want kept sandbox", client.deleteIDs)
	}
}

func TestCubeSandboxRunReturnsSessionHandleWhenKeepOnFailureRetainsSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeCubeSandboxSyncClient{processCodes: []int{0, 7}}
	restore := swapNewCubeSandboxClient(client)
	defer restore()
	var stderr bytes.Buffer
	backend := &cubesandboxBackend{
		cfg: Config{
			IdleTimeout: 30 * time.Minute,
			TTL:         2 * time.Minute,
			CubeSandbox: CubeSandboxConfig{APIKey: "test", Template: "base", Workdir: "repo"},
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

func TestCubeSandboxRunPreservesAbnormalProcessExitCode(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeCubeSandboxSyncClient{
		processCodes: []int{0, 137},
		processErrs:  []error{nil, errors.New("process did not exit normally")},
	}
	restore := swapNewCubeSandboxClient(client)
	defer restore()
	backend := &cubesandboxBackend{
		cfg: Config{TTL: time.Minute, CubeSandbox: CubeSandboxConfig{APIKey: "test", Template: "base", Workdir: "repo"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "repo", Root: t.TempDir()},
		Command: []string{"false"},
		NoSync:  true,
	})
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 137 {
		t.Fatalf("err=%v, want ExitError code 137", err)
	}
}

func TestCubeSandboxSandboxToServerUsesMetadata(t *testing.T) {
	server := cubesandboxSandboxToServer(cubesandboxSandbox{
		SandboxID:  "sbx_1",
		TemplateID: "base",
		State:      "running",
		Metadata: map[string]string{
			"provider": "cubesandbox",
			"crabbox":  "true",
			"lease":    "cbx_123",
			"slug":     "blue-lobster",
		},
	})
	if server.Provider != "cubesandbox" || server.CloudID != "sbx_1" || server.Labels["lease"] != "cbx_123" || server.Labels["slug"] != "blue-lobster" {
		t.Fatalf("server=%#v", server)
	}
	if server.ServerType.Name != "base" {
		t.Fatalf("type=%q", server.ServerType.Name)
	}
}

func TestCubeSandboxResolveSyntheticIDRequiresCrabboxMetadata(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	backend := &cubesandboxBackend{}
	client := &fakeCubeSandboxSyncClient{
		sandbox: cubesandboxSandbox{
			SandboxID: "sbx_1",
			Metadata:  map[string]string{"provider": "other"},
		},
	}
	_, _, _, err := backend.resolveSandboxID(context.Background(), client, "cubesandbox_sbx_1", "", false)
	if err == nil || !strings.Contains(err.Error(), "not claimed by Crabbox") {
		t.Fatalf("err=%v, want ownership error", err)
	}

	client.sandbox.Metadata = map[string]string{
		"provider": "cubesandbox",
		"crabbox":  "true",
		"lease":    "cbx_123456789abc",
		"slug":     "blue-lobster",
	}
	_, _, _, err = backend.resolveSandboxID(context.Background(), client, "cubesandbox_sbx_1", t.TempDir(), false)
	if err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("err=%v, want exact-claim rejection", err)
	}

	repoRoot := t.TempDir()
	leaseID, sandboxID, slug, err := backend.resolveSandboxID(context.Background(), client, "cubesandbox_sbx_1", repoRoot, true)
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "cbx_123456789abc" || sandboxID != "sbx_1" || slug != "blue-lobster" {
		t.Fatalf("lease=%q sandbox=%q slug=%q", leaseID, sandboxID, slug)
	}
	claim, ok, exact, err := resolveLeaseClaimForProviderScopeWithExact(leaseID, "endpoint:http://127.0.0.1:3000")
	if err != nil || !ok || !exact || claim.CloudID != "sbx_1" || claim.ProviderScope != "endpoint:http://127.0.0.1:3000" {
		t.Fatalf("claim=%#v ok=%t exact=%t err=%v", claim, ok, exact, err)
	}
	if _, _, _, err := backend.resolveSandboxID(context.Background(), client, leaseID, repoRoot, false); err != nil {
		t.Fatalf("resolve exact claim: %v", err)
	}
}

func TestCubeSandboxCreateBindsExactSandboxAndEndpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeCubeSandboxSyncClient{}
	backend := &cubesandboxBackend{
		cfg: Config{
			IdleTimeout: 30 * time.Minute,
			CubeSandbox: CubeSandboxConfig{
				APIURL:   "https://cube.example.test/root/",
				Template: "base",
			},
		},
		rt: Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	leaseID, sandbox, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir()}, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, exact, err := resolveLeaseClaimForProviderScopeWithExact(leaseID, "endpoint:https://cube.example.test/root")
	if err != nil || !ok || !exact {
		t.Fatalf("claim=%#v ok=%t exact=%t err=%v", claim, ok, exact, err)
	}
	if claim.CloudID != sandbox.SandboxID || claim.ProviderScope != "endpoint:https://cube.example.test/root" {
		t.Fatalf("claim=%#v sandbox=%#v", claim, sandbox)
	}
}

func TestCubeSandboxStopRejectsLabelledButUnclaimedSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeCubeSandboxSyncClient{sandbox: cubesandboxSandbox{
		SandboxID: "sbx_unclaimed",
		Metadata: map[string]string{
			"provider": providerName,
			"crabbox":  "true",
			"lease":    "cbx_123456789abc",
		},
	}}
	restore := swapNewCubeSandboxClient(client)
	defer restore()
	backend := &cubesandboxBackend{cfg: cubesandboxClaimConfig(Config{}), rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	err := backend.Stop(context.Background(), StopRequest{ID: "sbx_unclaimed"})
	if err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("Stop err=%v, want exact-claim rejection", err)
	}
	if len(client.deleteIDs) != 0 {
		t.Fatalf("unclaimed Stop deleted %#v", client.deleteIDs)
	}
}

func TestCubeSandboxStopChecksLiveOwnershipAndRemovesClaimAtomically(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeCubeSandboxSyncClient{}
	restore := swapNewCubeSandboxClient(client)
	defer restore()
	backend := &cubesandboxBackend{
		cfg: Config{CubeSandbox: CubeSandboxConfig{APIURL: "https://cube.example.test", Template: "base"}},
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

func TestCubeSandboxStopRemovesStaleClaimWhenSandboxIsAlreadyGone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeCubeSandboxSyncClient{}
	restore := swapNewCubeSandboxClient(client)
	defer restore()
	backend := &cubesandboxBackend{
		cfg: Config{CubeSandbox: CubeSandboxConfig{APIURL: "https://cube.example.test", Template: "base"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	leaseID, sandbox, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir()}, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	client.getErr = &cubesandboxAPIError{StatusCode: http.StatusNotFound, Status: "404 Not Found"}
	if err := backend.Stop(context.Background(), StopRequest{ID: leaseID}); err != nil {
		t.Fatalf("Stop stale claim: %v", err)
	}
	if len(client.deleteIDs) != 0 {
		t.Fatalf("already absent sandbox deleted: %#v", client.deleteIDs)
	}
	if client.getIDs[len(client.getIDs)-1] != sandbox.SandboxID {
		t.Fatalf("getIDs=%#v, want %q", client.getIDs, sandbox.SandboxID)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("stale claim exists=%t err=%v", exists, err)
	}
}

func TestCubeSandboxStopRemovesClaimWhenDeleteRacesWithExpiry(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeCubeSandboxSyncClient{}
	restore := swapNewCubeSandboxClient(client)
	defer restore()
	backend := &cubesandboxBackend{
		cfg: Config{CubeSandbox: CubeSandboxConfig{APIURL: "https://cube.example.test", Template: "base"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	leaseID, sandbox, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir()}, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	client.deleteErr = &cubesandboxAPIError{StatusCode: http.StatusNotFound, Status: "404 Not Found"}
	if err := backend.Stop(context.Background(), StopRequest{ID: leaseID}); err != nil {
		t.Fatalf("Stop raced expiry: %v", err)
	}
	if len(client.deleteIDs) != 1 || client.deleteIDs[0] != sandbox.SandboxID {
		t.Fatalf("deleteIDs=%#v", client.deleteIDs)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("raced expiry claim exists=%t err=%v", exists, err)
	}
}

func TestCubeSandboxExactSandboxIDResolvesExistingClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeCubeSandboxSyncClient{}
	backend := &cubesandboxBackend{
		cfg: Config{CubeSandbox: CubeSandboxConfig{APIURL: "https://cube.example.test", Template: "base"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	leaseID, sandbox, slug, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir()}, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{sandbox.SandboxID, "cubesandbox_" + sandbox.SandboxID} {
		gotLease, gotSandbox, gotSlug, err := backend.resolveSandboxID(context.Background(), client, id, "", false)
		if err != nil {
			t.Fatalf("resolve %q: %v", id, err)
		}
		if gotLease != leaseID || gotSandbox != sandbox.SandboxID || gotSlug != slug {
			t.Fatalf("resolve %q = %q %q %q, want %q %q %q", id, gotLease, gotSandbox, gotSlug, leaseID, sandbox.SandboxID, slug)
		}
	}
}

func TestCubeSandboxStopRejectsDifferentAPIEndpointBeforeRemoteRead(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeCubeSandboxSyncClient{}
	creator := &cubesandboxBackend{
		cfg: Config{CubeSandbox: CubeSandboxConfig{APIURL: "https://cube-a.example.test", Template: "base"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	leaseID, _, _, err := creator.createSandbox(context.Background(), client, Repo{Root: t.TempDir()}, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	client.getIDs = nil
	restore := swapNewCubeSandboxClient(client)
	defer restore()
	other := &cubesandboxBackend{
		cfg: Config{CubeSandbox: CubeSandboxConfig{APIURL: "https://cube-b.example.test"}},
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

func TestCubeSandboxReclaimAndStopAdoptsExactSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeCubeSandboxSyncClient{sandbox: cubesandboxSandbox{
		SandboxID: "sbx_reclaim",
		Metadata: map[string]string{
			"provider": providerName,
			"crabbox":  "true",
			"lease":    "cbx_123456789abc",
			"slug":     "reclaim-me",
		},
	}}
	restore := swapNewCubeSandboxClient(client)
	defer restore()
	backend := &cubesandboxBackend{cfg: cubesandboxClaimConfig(Config{}), rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
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

func TestCubeSandboxReclaimAndStopPreservesRepoBoundClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_123456789abc"
	firstCfg := Config{Provider: providerName, CubeSandbox: CubeSandboxConfig{APIURL: "https://cube-a.example.test"}}
	sandbox := cubesandboxSandbox{
		SandboxID: "sbx_reclaim",
		Metadata: map[string]string{
			"provider": providerName,
			"crabbox":  "true",
			"lease":    leaseID,
			"slug":     "reclaim-me",
		},
	}
	if err := claimLeaseTargetForRepoConfig(leaseID, "reclaim-me", firstCfg, cubesandboxSandboxToServer(sandbox), SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	client := &fakeCubeSandboxSyncClient{sandbox: sandbox}
	restore := swapNewCubeSandboxClient(client)
	defer restore()
	backend := &cubesandboxBackend{
		cfg: Config{Provider: providerName, CubeSandbox: CubeSandboxConfig{APIURL: "https://cube-b.example.test"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	if err := backend.ReclaimAndStop(context.Background(), StopRequest{ID: sandbox.SandboxID}); err != nil {
		t.Fatalf("ReclaimAndStop: %v", err)
	}
	if len(client.deleteIDs) != 1 || client.deleteIDs[0] != sandbox.SandboxID {
		t.Fatalf("deleteIDs=%#v", client.deleteIDs)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("successful reclaim claim exists=%t err=%v", exists, err)
	}
}

func TestCubeSandboxReclaimRejectsCrossProviderLeaseCollision(t *testing.T) {
	for _, mode := range []string{"run", "stop"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const leaseID = "cbx_123456789abc"
			otherCfg := Config{Provider: "e2b"}
			otherServer := Server{Provider: "e2b", CloudID: "e2b-owned"}
			if _, err := claimLeaseTargetForConfigIfUnchanged(leaseID, "other", otherCfg, otherServer, SSHTarget{}, 0, LeaseClaim{}, false); err != nil {
				t.Fatal(err)
			}

			client := &fakeCubeSandboxSyncClient{sandbox: cubesandboxSandbox{
				SandboxID: "sbx_reclaim",
				Metadata: map[string]string{
					"provider": providerName,
					"crabbox":  "true",
					"lease":    leaseID,
				},
			}}
			restore := swapNewCubeSandboxClient(client)
			defer restore()
			backend := &cubesandboxBackend{cfg: cubesandboxClaimConfig(Config{}), rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
			var err error
			if mode == "stop" {
				err = backend.ReclaimAndStop(context.Background(), StopRequest{ID: "sbx_reclaim"})
			} else {
				_, _, _, err = backend.resolveSandboxID(context.Background(), client, "sbx_reclaim", t.TempDir(), true)
			}
			if err == nil || !strings.Contains(err.Error(), "already claimed by provider \"e2b\"") {
				t.Fatalf("err=%v, want cross-provider collision", err)
			}
			if len(client.deleteIDs) != 0 {
				t.Fatalf("deleteIDs=%#v, want none", client.deleteIDs)
			}
			claim, exists, readErr := readLeaseClaimWithPresence(leaseID)
			if readErr != nil || !exists || claim.Provider != "e2b" || claim.CloudID != "e2b-owned" {
				t.Fatalf("claim=%#v exists=%t err=%v", claim, exists, readErr)
			}
		})
	}
}

func TestCubeSandboxReclaimScopesCloudIDCollisionToEndpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	firstCfg := Config{Provider: providerName, CubeSandbox: CubeSandboxConfig{APIURL: "https://cube-a.example.test"}}
	firstServer := Server{Provider: providerName, CloudID: "same-sandbox-id"}
	if _, err := claimLeaseTargetForConfigIfUnchanged("cbx_111111111111", "first", firstCfg, firstServer, SSHTarget{}, 0, LeaseClaim{}, false); err != nil {
		t.Fatal(err)
	}

	client := &fakeCubeSandboxSyncClient{sandbox: cubesandboxSandbox{
		SandboxID: "same-sandbox-id",
		Metadata: map[string]string{
			"provider": providerName,
			"crabbox":  "true",
			"lease":    "cbx_222222222222",
			"slug":     "second",
		},
	}}
	secondCfg := Config{Provider: providerName, CubeSandbox: CubeSandboxConfig{APIURL: "https://cube-b.example.test"}}
	backend := &cubesandboxBackend{cfg: secondCfg, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	leaseID, sandboxID, _, err := backend.resolveSandboxID(context.Background(), client, "same-sandbox-id", t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "cbx_222222222222" || sandboxID != "same-sandbox-id" {
		t.Fatalf("resolved lease=%q sandbox=%q", leaseID, sandboxID)
	}
	for _, tc := range []struct {
		cfg     Config
		leaseID string
	}{
		{cfg: firstCfg, leaseID: "cbx_111111111111"},
		{cfg: secondCfg, leaseID: "cbx_222222222222"},
	} {
		claim, ok, err := resolveLeaseClaimForProviderCloudIDScope("same-sandbox-id", providerClaimScope(tc.cfg))
		if err != nil || !ok || claim.LeaseID != tc.leaseID {
			t.Fatalf("scope=%q claim=%#v ok=%t err=%v", providerClaimScope(tc.cfg), claim, ok, err)
		}
	}
}

type fakeCubeSandboxSyncClient struct {
	commands          []string
	users             []string
	sandbox           cubesandboxSandbox
	createReq         cubesandboxCreateSandboxRequest
	createCalls       int
	getIDs            []string
	getErr            error
	deleteIDs         []string
	deleteErr         error
	deleteDeadlineSet bool
	uploadPath        string
	uploaded          bytes.Buffer
	processCodes      []int
	processErrs       []error
	processEnvs       []map[string]string
}

func swapNewCubeSandboxClient(fake cubesandboxAPI) func() {
	prev := newCubeSandboxClient
	newCubeSandboxClient = func(Config, Runtime) (cubesandboxAPI, error) { return fake, nil }
	return func() { newCubeSandboxClient = prev }
}

func (f *fakeCubeSandboxSyncClient) CreateSandbox(_ context.Context, req cubesandboxCreateSandboxRequest) (cubesandboxSandbox, error) {
	f.createReq = req
	f.createCalls++
	if f.sandbox.SandboxID != "" {
		return f.sandbox, nil
	}
	f.sandbox = cubesandboxSandbox{SandboxID: "sbx_1", Metadata: req.Metadata, State: "running"}
	return f.sandbox, nil
}

func (f *fakeCubeSandboxSyncClient) ConnectSandbox(context.Context, string, int) (cubesandboxSession, error) {
	return cubesandboxSession{}, nil
}

func (f *fakeCubeSandboxSyncClient) GetSandbox(_ context.Context, sandboxID string) (cubesandboxSandbox, error) {
	f.getIDs = append(f.getIDs, sandboxID)
	if f.getErr != nil {
		return cubesandboxSandbox{}, f.getErr
	}
	return f.sandbox, nil
}

func (f *fakeCubeSandboxSyncClient) ListSandboxes(context.Context, map[string]string) ([]cubesandboxSandbox, error) {
	return nil, nil
}

func (f *fakeCubeSandboxSyncClient) DeleteSandbox(ctx context.Context, sandboxID string) error {
	f.deleteIDs = append(f.deleteIDs, sandboxID)
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		f.deleteDeadlineSet = remaining > 0 && remaining <= cubesandboxCleanupTimeout
	}
	return f.deleteErr
}

func (f *fakeCubeSandboxSyncClient) UploadFile(_ context.Context, _ cubesandboxSession, targetPath string, r io.Reader) error {
	f.uploadPath = targetPath
	_, err := io.Copy(&f.uploaded, r)
	return err
}

func (f *fakeCubeSandboxSyncClient) StartProcess(_ context.Context, _ cubesandboxSession, req cubesandboxProcessRequest) (int, error) {
	f.commands = append(f.commands, req.Command)
	f.users = append(f.users, req.User)
	f.processEnvs = append(f.processEnvs, req.Env)
	code := 0
	if len(f.processCodes) > 0 {
		code = f.processCodes[0]
		f.processCodes = f.processCodes[1:]
	}
	var err error
	if len(f.processErrs) > 0 {
		err = f.processErrs[0]
		f.processErrs = f.processErrs[1:]
	}
	return code, err
}

func (f *fakeCubeSandboxSyncClient) commandContains(value string) bool {
	for _, command := range f.commands {
		if strings.Contains(command, value) {
			return true
		}
	}
	return false
}

func (f *fakeCubeSandboxSyncClient) userContains(value string) bool {
	for _, user := range f.users {
		if user == value {
			return true
		}
	}
	return false
}

func cubesandboxTestEnvelope(flags byte, v any) []byte {
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
