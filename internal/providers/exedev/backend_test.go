package exedev

import (
	"bytes"
	"context"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExeDevProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName {
		t.Fatalf("spec.Name = %q, want %q", spec.Name, providerName)
	}
	if spec.Kind != "delegated-run" {
		t.Fatalf("spec.Kind = %q, want delegated-run", spec.Kind)
	}
	aliases := Provider{}.Aliases()
	if len(aliases) != 2 || aliases[0] != "exe" || aliases[1] != "exedev" {
		t.Fatalf("aliases = %#v, want [exe exedev]", aliases)
	}
}

func TestExeDevClientRequiresAPIKey(t *testing.T) {
	cfg := Config{}
	cfg.ExeDev.APIURL = "https://exe.dev"
	if _, err := newExeDevClient(cfg, Runtime{}); err == nil {
		t.Fatal("newExeDevClient accepted empty API key")
	}
}

func TestExeDevClientRejectsBareHTTPURL(t *testing.T) {
	cfg := Config{}
	cfg.ExeDev.APIKey = "test-key"
	cfg.ExeDev.APIURL = "http://exe.dev"
	if _, err := newExeDevClient(cfg, Runtime{}); err == nil {
		t.Fatal("newExeDevClient accepted plaintext http URL")
	}
}

func TestExeDevTokenFlagIsNotRegistered(t *testing.T) {
	cfg := Config{}
	cfg.ExeDev.APIKey = "secret-key"
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterExeDevProviderFlags(fs, cfg)
	if fs.Lookup("exe-dev-key") != nil || fs.Lookup("exe-dev-token") != nil || fs.Lookup("exe-api-key") != nil {
		t.Fatal("exe.dev API key surfaced as a flag")
	}
	if fs.Lookup("exe-dev-url") == nil {
		t.Fatal("exe-dev-url flag missing")
	}
}

func TestExeDevExecSendsBearerAndBody(t *testing.T) {
	var (
		gotAuth   string
		gotMethod string
		gotPath   string
		gotBody   string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		_, _ = io.WriteString(w, "root\n")
	}))
	defer server.Close()

	cfg := Config{}
	cfg.ExeDev.APIKey = "test-key"
	cfg.ExeDev.APIURL = server.URL
	client, err := newExeDevClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code, err := client.Exec(context.Background(), "whoami", &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/exec" {
		t.Fatalf("path = %q, want /exec", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("auth header = %q, want bearer test-key", gotAuth)
	}
	if gotBody != "whoami" {
		t.Fatalf("body = %q, want whoami", gotBody)
	}
	if stdout.String() != "root\n" {
		t.Fatalf("stdout = %q, want root\\n", stdout.String())
	}
}

func TestExeDevExecSurfacesNon2xxAsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized request", http.StatusUnauthorized)
	}))
	defer server.Close()

	cfg := Config{}
	cfg.ExeDev.APIKey = "wrong-key"
	cfg.ExeDev.APIURL = server.URL
	client, err := newExeDevClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	code, err := client.Exec(context.Background(), "whoami", io.Discard, io.Discard)
	if err == nil {
		t.Fatal("Exec accepted 401 response")
	}
	apiErr, ok := err.(*exeDevAPIError)
	if !ok {
		t.Fatalf("err = %T, want *exeDevAPIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Body, "unauthorized request") {
		t.Fatalf("body = %q, want unauthorized request snippet", apiErr.Body)
	}
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestExeDevExecMaps422ToCommandFailure(t *testing.T) {
	// Per https://exe.dev/docs/https-api, HTTP 422 means the command ran but
	// returned a non-zero exit code. The body should be streamed to stdout
	// and the call should return a non-zero exit with exeDevCommandFailedError.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, "missing arguments\n")
	}))
	defer server.Close()

	cfg := Config{}
	cfg.ExeDev.APIKey = "test-key"
	cfg.ExeDev.APIURL = server.URL
	client, err := newExeDevClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	code, err := client.Exec(context.Background(), "new", &stdout, io.Discard)
	if err == nil {
		t.Fatal("Exec accepted 422 response")
	}
	cmdErr, ok := err.(*exeDevCommandFailedError)
	if !ok {
		t.Fatalf("err = %T, want *exeDevCommandFailedError", err)
	}
	if !strings.Contains(cmdErr.Body, "missing arguments") {
		t.Fatalf("body = %q, want missing arguments snippet", cmdErr.Body)
	}
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "missing arguments") {
		t.Fatalf("stdout = %q, want command output body", stdout.String())
	}
}

func TestExeDevExecSurfaces504AsAPIError(t *testing.T) {
	// Per https://exe.dev/docs/https-api, HTTP 504 means the command exceeded
	// the 30s timeout. It should surface as a transport-level API error rather
	// than a clean exit, so callers can distinguish it from command failures.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "timeout", http.StatusGatewayTimeout)
	}))
	defer server.Close()

	cfg := Config{}
	cfg.ExeDev.APIKey = "test-key"
	cfg.ExeDev.APIURL = server.URL
	client, err := newExeDevClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Exec(context.Background(), "sleep 60", io.Discard, io.Discard)
	if err == nil {
		t.Fatal("Exec accepted 504 response")
	}
	apiErr, ok := err.(*exeDevAPIError)
	if !ok {
		t.Fatalf("err = %T, want *exeDevAPIError", err)
	}
	if apiErr.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", apiErr.StatusCode)
	}
}

func TestExeDevRunRequiresNoSync(t *testing.T) {
	backend := &exeDevBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	_, err := backend.Run(context.Background(), RunRequest{Command: []string{"whoami"}})
	if err == nil {
		t.Fatal("Run accepted request without --no-sync")
	}
	if !strings.Contains(err.Error(), "--no-sync") {
		t.Fatalf("err = %v, want --no-sync hint", err)
	}
}

func TestExeDevRunRejectsLeaseFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  RunRequest
		want string
	}{
		{name: "id", req: RunRequest{ID: "foo", NoSync: true, Command: []string{"whoami"}}, want: "--id"},
		{name: "keep", req: RunRequest{Keep: true, NoSync: true, Command: []string{"whoami"}}, want: "--keep"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &exeDevBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
			_, err := backend.Run(context.Background(), tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %s rejection", err, tc.want)
			}
		})
	}
}

func TestExeDevRunHappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "root\n")
	}))
	defer server.Close()

	cfg := Config{Provider: providerName}
	cfg.ExeDev.APIKey = "test-key"
	cfg.ExeDev.APIURL = server.URL
	rt := Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard}
	backend := NewExeDevBackend(Provider{}.Spec(), cfg, rt).(*exeDevBackend)
	result, err := backend.Run(context.Background(), RunRequest{Command: []string{"whoami"}, NoSync: true})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
}

func TestExeDevWarmupRejected(t *testing.T) {
	backend := &exeDevBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	err := backend.Warmup(context.Background(), WarmupRequest{})
	if err == nil || !strings.Contains(err.Error(), "warmup") {
		t.Fatalf("Warmup err = %v, want warmup rejection", err)
	}
}

func TestExeDevStopRejected(t *testing.T) {
	backend := &exeDevBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	err := backend.Stop(context.Background(), StopRequest{})
	if err == nil || !strings.Contains(err.Error(), "stop") {
		t.Fatalf("Stop err = %v, want stop rejection", err)
	}
}

func TestExeDevStatusRejected(t *testing.T) {
	backend := &exeDevBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	if _, err := backend.Status(context.Background(), StatusRequest{}); err == nil {
		t.Fatal("Status should reject without sandboxes")
	}
}

func TestExeDevListEmpty(t *testing.T) {
	backend := &exeDevBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	servers, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatalf("List err = %v", err)
	}
	if len(servers) != 0 {
		t.Fatalf("List len = %d, want 0", len(servers))
	}
}

func TestExeDevFlagsApply(t *testing.T) {
	cfg := Config{Provider: providerName}
	cfg.ExeDev.APIURL = "https://exe.dev"
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterExeDevProviderFlags(fs, cfg)
	if err := fs.Parse([]string{"--exe-dev-url", "https://exe.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyExeDevProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.ExeDev.APIURL != "https://exe.example.com" {
		t.Fatalf("APIURL = %q, want https://exe.example.com", cfg.ExeDev.APIURL)
	}
}
