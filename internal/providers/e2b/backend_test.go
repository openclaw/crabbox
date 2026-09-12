package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"github.com/openclaw/crabbox/internal/testutil"
)

type e2bRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn e2bRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type e2bRewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (transport e2bRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	endpoint := *req.URL
	endpoint.Scheme = transport.target.Scheme
	endpoint.Host = transport.target.Host
	clone.URL = &endpoint
	clone.Host = transport.target.Host
	return transport.base.RoundTrip(clone)
}

type e2bDelayedReader struct {
	data    []byte
	split   int
	delay   time.Duration
	offset  int
	delayed bool
}

func (reader *e2bDelayedReader) Read(p []byte) (int, error) {
	if reader.offset >= len(reader.data) {
		return 0, io.EOF
	}
	if reader.offset >= reader.split && !reader.delayed {
		time.Sleep(reader.delay)
		reader.delayed = true
	}
	limit := len(reader.data)
	if !reader.delayed && limit > reader.split {
		limit = reader.split
	}
	n := copy(p, reader.data[reader.offset:limit])
	reader.offset += n
	return n, nil
}

func e2bLoopbackClient(t *testing.T, server *httptest.Server, timeout time.Duration) *http.Client {
	t.Helper()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Transport: e2bRewriteTransport{target: target, base: server.Client().Transport},
		Timeout:   timeout,
	}
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
		testutil.RequireRedactedProviderError(t, err, secret)
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
		client := &e2bClient{envdClient: httpClient}
		_, err := client.StartProcess(context.Background(), e2bSession{SandboxID: "sbx_1", Domain: "example.test", EnvdAccessToken: secret}, e2bProcessRequest{Command: "true"})
		testutil.RequireRedactedProviderError(t, err, secret)
	})
}

func TestE2BProcessStreamRedactsReflectedCredential(t *testing.T) {
	const secret = "envd-stream-secret"

	t.Run("process end diagnostic", func(t *testing.T) {
		body := testutil.GRPCWebEnvelope(0, map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": 1, "exited": false, "error": "Bearer " + secret + " quota exceeded"}}})
		var stderr bytes.Buffer
		if _, err := parseE2BProcessStream(bytes.NewReader(body), io.Discard, &stderr, secret); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stderr.String(), secret) || !strings.Contains(stderr.String(), "[redacted]") || !strings.Contains(stderr.String(), "quota exceeded") {
			t.Fatalf("stderr=%q, want redacted useful process diagnostic", stderr.String())
		}
	})
}

func TestE2BDefaultHTTPClientsSeparateControlAndDataPlanes(t *testing.T) {
	api, err := newE2BClient(Config{E2B: E2BConfig{APIKey: "e2b_test"}}, Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	client, ok := api.(*e2bClient)
	if !ok {
		t.Fatalf("api=%T, want *e2bClient", api)
	}
	if client.httpClient.Timeout != e2bControlTimeout {
		t.Fatalf("control Timeout=%s, want %s", client.httpClient.Timeout, e2bControlTimeout)
	}
	if client.envdClient.Timeout != 0 {
		t.Fatalf("envd Timeout=%s, want caller-owned lifetime", client.envdClient.Timeout)
	}
	if client.httpClient == client.envdClient {
		t.Fatal("control and envd fallbacks share one client")
	}
	if client.httpClient.Transport != nil || client.envdClient.Transport != nil {
		t.Fatalf("fallback transports=control:%T envd:%T, want process default transport", client.httpClient.Transport, client.envdClient.Transport)
	}
}

func TestE2BInjectedHTTPClientIsPreservedForBothPlanes(t *testing.T) {
	injected := &http.Client{
		Transport: e2bRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("test transport")
		}),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       17 * time.Second,
	}
	api, err := newE2BClient(Config{E2B: E2BConfig{APIKey: "e2b_test"}}, Runtime{HTTP: injected})
	if err != nil {
		t.Fatal(err)
	}
	client := api.(*e2bClient)
	if client.httpClient != injected || client.envdClient != injected {
		t.Fatalf("clients=control:%p envd:%p, want injected %p", client.httpClient, client.envdClient, injected)
	}
}

func TestE2BControlClientBoundsStalledResponseBody(t *testing.T) {
	const controlTimeout = 40 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"sandboxID":`)
		w.(http.Flusher).Flush()
		<-req.Context().Done()
	}))
	defer server.Close()

	controlClient, _ := shared.ControlAndDataHTTPClients(nil, controlTimeout)
	client := &e2bClient{apiKey: "e2b_test", apiURL: server.URL, httpClient: controlClient}
	started := time.Now()
	_, err := client.GetSandbox(context.Background(), "sbx_1")
	elapsed := time.Since(started)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetSandbox error=%v, want whole-request deadline", err)
	}
	if elapsed > time.Second {
		t.Fatalf("stalled response body bounded after %s, want under 1s", elapsed)
	}
	t.Logf("control response-body stall bounded: elapsed=%s timeout=%s", elapsed.Round(time.Millisecond), controlTimeout)
}

func TestE2BControlClientBoundsWithheldHeaders(t *testing.T) {
	const controlTimeout = 40 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		<-req.Context().Done()
	}))
	defer server.Close()

	controlClient, _ := shared.ControlAndDataHTTPClients(nil, controlTimeout)
	client := &e2bClient{apiKey: "e2b_test", apiURL: server.URL, httpClient: controlClient}
	started := time.Now()
	_, err := client.GetSandbox(context.Background(), "sbx_1")
	elapsed := time.Since(started)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetSandbox error=%v, want whole-request deadline", err)
	}
	if elapsed > time.Second {
		t.Fatalf("withheld headers bounded after %s, want under 1s", elapsed)
	}
	t.Logf("control withheld headers bounded: elapsed=%s timeout=%s", elapsed.Round(time.Millisecond), controlTimeout)
}

func TestE2BDataPlaneStreamOutlivesControlTimeout(t *testing.T) {
	const controlTimeout = 30 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testutil.GRPCWebEnvelope(0, map[string]any{"event": map[string]any{"start": map[string]any{"pid": 42}}}))
		w.(http.Flusher).Flush()
		time.Sleep(3 * controlTimeout)
		_, _ = w.Write(testutil.GRPCWebEnvelope(0, map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": 0, "exited": true}}}))
		_, _ = w.Write(testutil.GRPCWebEnvelope(2, map[string]any{}))
	}))
	defer server.Close()

	controlClient, _ := shared.ControlAndDataHTTPClients(nil, controlTimeout)
	client := &e2bClient{
		domain:     "e2b.test",
		httpClient: controlClient,
		envdClient: e2bLoopbackClient(t, server, 0),
	}
	started := time.Now()
	code, err := client.StartProcess(context.Background(), e2bSession{SandboxID: "sbx_1", Domain: "e2b.test"}, e2bProcessRequest{Command: "true"})
	elapsed := time.Since(started)
	if err != nil || code != 0 {
		t.Fatalf("StartProcess code=%d err=%v", code, err)
	}
	if elapsed <= controlTimeout {
		t.Fatalf("stream completed in %s, want it active beyond %s", elapsed, controlTimeout)
	}
	t.Logf("envd stream survived control deadline: elapsed=%s control_timeout=%s", elapsed.Round(time.Millisecond), controlTimeout)
}

func TestE2BDataPlaneUploadOutlivesControlTimeout(t *testing.T) {
	const controlTimeout = 30 * time.Millisecond
	type uploadResult struct {
		data []byte
		err  error
	}
	uploaded := make(chan uploadResult, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		reader, err := req.MultipartReader()
		if err != nil {
			uploaded <- uploadResult{err: err}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		part, err := reader.NextPart()
		if err != nil {
			uploaded <- uploadResult{err: err}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(part)
		uploaded <- uploadResult{data: data, err: err}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	payload := []byte("before-after")
	controlClient, _ := shared.ControlAndDataHTTPClients(nil, controlTimeout)
	client := &e2bClient{
		domain:     "e2b.test",
		httpClient: controlClient,
		envdClient: e2bLoopbackClient(t, server, 0),
	}
	reader := &e2bDelayedReader{data: payload, split: len("before-"), delay: 3 * controlTimeout}
	started := time.Now()
	err := client.UploadFile(context.Background(), e2bSession{SandboxID: "sbx_1", Domain: "e2b.test"}, "/tmp/archive.tgz", reader)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	result := <-uploaded
	if result.err != nil {
		t.Fatal(result.err)
	}
	if !bytes.Equal(result.data, payload) {
		t.Fatalf("uploaded=%q, want %q", result.data, payload)
	}
	if elapsed <= controlTimeout {
		t.Fatalf("upload completed in %s, want it active beyond %s", elapsed, controlTimeout)
	}
	t.Logf("envd upload completed without truncation: bytes=%d elapsed=%s control_timeout=%s", len(result.data), elapsed.Round(time.Millisecond), controlTimeout)
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

func TestE2BFlagsPreserveExactGuardAndDeferredValidation(t *testing.T) {
	cfg := Config{Provider: "e2b"}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterE2BProviderFlags(fs, cfg)
	fs.VisitAll(func(f *flag.Flag) {
		if strings.Contains(f.Name, "key") {
			t.Fatal("API key flag introduced")
		}
	})
	cfg.E2B = E2BConfig{APIURL: "https://example.invalid/api", Domain: "example.invalid", Template: "custom", Workdir: "work", User: "alice"}
	before := cfg
	if err := ApplyE2BProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, before) {
		t.Fatal("unvisited flags changed config")
	}
	if err := fs.Parse([]string{"--e2b-api-url=https://example.invalid/api", "--e2b-domain=example.invalid", "--e2b-template=custom", "--e2b-workdir=work", "--e2b-user=alice"}); err != nil {
		t.Fatal(err)
	}
	cfg.E2B = E2BConfig{}
	if err := ApplyE2BProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	core.RecordProviderFlagInputs(&before, true, "e2b")
	if !reflect.DeepEqual(cfg, before) {
		t.Fatal("positive flags changed binding or marker semantics")
	}
	args := []string{"--e2b-api-url=", "--e2b-domain=", "--e2b-template=", "--e2b-workdir=", "--e2b-user="}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if err := ApplyE2BProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal("wrapper added client validation")
	}
	before.E2B = E2BConfig{}
	if !reflect.DeepEqual(cfg, before) {
		t.Fatal("explicit empty values or central provenance side effects changed")
	}
	if _, err := (Provider{}).Configure(cfg, Runtime{}); err != nil {
		t.Fatalf("Configure added client validation: %v", err)
	}
	for _, name := range []string{"e2b", "E2B", " e2b "} {
		cfg := Config{Provider: name}
		fs := flag.NewFlagSet("guard", flag.ContinueOnError)
		fs.String("class", "", "")
		fs.String("type", "", "")
		values := RegisterE2BProviderFlags(fs, cfg)
		for _, args := range [][]string{{"--type=vm"}, {"--type=vm", "--class=large"}} {
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			for _, v := range []any{nil, struct{}{}, values} {
				err := ApplyE2BProviderFlags(&cfg, fs, v)
				if name != "e2b" {
					if err != nil {
						t.Fatalf("guard normalized provider: %v", err)
					}
					continue
				}
				want := "--type"
				if len(args) == 2 {
					want = "--class"
				}
				if err == nil || err.Error() != want+" is not supported for provider=e2b" {
					t.Fatalf("guard=%v", err)
				}
			}
		}
	}
}

func TestE2BConfiguredEndpointDefaultsAndRawScope(t *testing.T) {
	for _, raw := range []string{"", "  ", " https://example.invalid/api/ "} {
		cfg := Config{E2B: E2BConfig{APIKey: "inert", APIURL: raw}}
		before := cfg.E2B
		api, err := newE2BClient(cfg, Runtime{HTTP: &http.Client{}})
		if raw == "  " {
			if err == nil {
				t.Fatal("whitespace client endpoint unexpectedly defaulted")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			want := "https://api.e2b.app"
			if raw != "" {
				want = "https://example.invalid/api"
			}
			if api.(*e2bClient).apiURL != want {
				t.Fatal("client endpoint default changed")
			}
		}
		scoped := e2bClaimConfig(cfg)
		want := "https://api.e2b.app"
		if strings.TrimSpace(raw) != "" {
			want = raw
		}
		if scoped.E2B.APIURL != want {
			t.Fatal("claim-config trim-before-default changed")
		}
		scope := (Provider{}).ClaimScope(cfg)
		if strings.TrimSpace(raw) == "" && scope != "" {
			t.Fatal("raw scope gained endpoint default")
		}
		if strings.TrimSpace(raw) != "" && scope != "endpoint:https://example.invalid/api" {
			t.Fatalf("custom scope=%q", scope)
		}
		if cfg.E2B != before {
			t.Fatal("default helpers mutated config")
		}
	}
	for _, raw := range []string{"", "  ", " sandbox.example.invalid "} {
		cfg := Config{E2B: E2BConfig{APIKey: "inert", APIURL: "https://example.invalid/api", Domain: raw, User: " alice "}}
		api, err := newE2BClient(cfg, Runtime{HTTP: &http.Client{}})
		if err != nil {
			t.Fatal(err)
		}
		want := strings.TrimSpace(raw)
		if raw == "" {
			want = "e2b.app"
		}
		if api.(*e2bClient).domain != want || api.(*e2bClient).user != " alice " {
			t.Fatal("client domain/user predicate changed")
		}
	}
	for _, rawKey := range []string{"", "  "} {
		if _, err := newE2BClient(Config{E2B: E2BConfig{APIKey: rawKey, APIURL: "relative"}}, Runtime{HTTP: &http.Client{}}); err == nil || err.Error() != "provider=e2b requires E2B_API_KEY" {
			t.Fatalf("key-first=%v", err)
		}
	}
	cfg := Config{E2B: E2BConfig{APIURL: "https://example.invalid/api", Domain: " sandbox.example.invalid ", User: " alice ", Workdir: " project "}}
	got := (Provider{}).CommandRouting(cfg, core.CommandRoutingRequest{}).Args
	want := []string{"--e2b-api-url", "https://example.invalid/api", "--e2b-domain", " sandbox.example.invalid ", "--e2b-user", " alice ", "--e2b-workdir", " project "}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("raw routing=%v", got)
	}
}

func TestE2BTemplateAcquisitionAndMetadataDefaultAreDistinct(t *testing.T) {
	for _, raw := range []string{"", "  ", "custom-template"} {
		t.Run(fmt.Sprintf("template=%q", raw), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			cfg := core.BaseConfig()
			cfg.Provider = "e2b"
			cfg.E2B.Template = raw
			b := &e2bBackend{cfg: cfg, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
			fake := &fakeE2BSyncClient{}
			_, sandbox, _, err := b.createSandbox(context.Background(), fake, Repo{Name: "example", Root: t.TempDir()}, false, false, "")
			if err != nil {
				t.Fatal(err)
			}
			want := raw
			if want == "" {
				want = "base"
			}
			if fake.createReq.TemplateID != want {
				t.Fatalf("template=%q want=%q", fake.createReq.TemplateID, want)
			}
			if e2bSandboxToServer(sandbox).ServerType.Name != "base" {
				t.Fatal("missing remote metadata inferred configured template")
			}
		})
	}
}

func TestE2BWorkspacePath(t *testing.T) {
	if got := e2bWorkspacePath(Config{E2B: E2BConfig{User: " alice ", Workdir: "  "}}); got != "/home/alice/crabbox" {
		t.Fatalf("whitespace/default workspace=%q", got)
	}
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

func TestE2BClientBindsObservedSandboxID(t *testing.T) {
	for _, operation := range []string{"get", "connect"} {
		for _, observed := range []string{"sbx_a", "sbx_b", "", " ", "SBX_A", " sbx_a", "sbx_a ", "synthetic-api-token", "synthetic-envd-token"} {
			t.Run(operation+"/"+observed, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					path, method := "/sandboxes/sbx_a", http.MethodGet
					if operation == "connect" {
						path, method = path+"/connect", http.MethodPost
					}
					if r.URL.Path != path || r.Method != method || r.Header.Get("X-API-Key") != "synthetic-api-token" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"sandboxID": observed, "alias": "different-template-alias", "envdAccessToken": "synthetic-envd-token", "domain": "sandbox.example.test"})
				}))
				defer server.Close()
				client, err := newE2BClient(Config{E2B: E2BConfig{APIKey: "synthetic-api-token", APIURL: server.URL}}, Runtime{HTTP: server.Client()})
				if err != nil {
					t.Fatal(err)
				}
				var id, token string
				if operation == "connect" {
					var session e2bSession
					session, err = client.ConnectSandbox(t.Context(), "sbx_a", 120)
					id, token = session.SandboxID, session.EnvdAccessToken
				} else {
					var sandbox e2bSandbox
					sandbox, err = client.GetSandbox(t.Context(), "sbx_a")
					id, token = sandbox.SandboxID, sandbox.EnvdAccessToken
				}
				if observed == "sbx_a" {
					if err != nil || id != "sbx_a" || token != "synthetic-envd-token" {
						t.Fatalf("exact observation: id=%q token preserved=%v err=%v", id, token == "synthetic-envd-token", err)
					}
				} else if err == nil || id != "" || token != "" {
					t.Fatalf("unbound observation escaped: id=%q token present=%v err=%v", id, token != "", err)
				} else if strings.Contains(err.Error(), "synthetic-api-token") || strings.Contains(err.Error(), "synthetic-envd-token") {
					t.Fatal("identity refusal exposed a reflected credential")
				}
			})
		}
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
	resp, err := shared.SecureHTTPClient(trusted.Client(), trustedURL, e2bRedirectError).Do(req)
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
	if !testutil.TarGzipContains(t, client.uploaded.Bytes(), "go.mod") {
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

func TestE2BSyncDeletePreservesWorkspaceWhenReplacementFails(t *testing.T) {
	for _, tc := range []struct {
		name        string
		uploadErr   error
		failExtract bool
	}{
		{name: "upload", uploadErr: errors.New("upload failed")},
		{name: "extract", failExtract: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newE2BSyncTestRepo(t)
			workspace := filepath.Join(t.TempDir(), "workspace")
			if err := os.Mkdir(workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			previous := filepath.Join(workspace, "previous.txt")
			if err := os.WriteFile(previous, []byte("keep me"), 0o644); err != nil {
				t.Fatal(err)
			}
			client := &fakeE2BSyncClient{
				uploadErr:    tc.uploadErr,
				failExtract:  tc.failExtract,
				executeShell: true,
			}
			backend := &e2bBackend{rt: Runtime{Stderr: io.Discard}}
			backend.cfg.Sync.Delete = true

			_, _, err := backend.syncWorkspace(t.Context(), client, e2bSession{SandboxID: "sbx_1"}, RunRequest{
				Repo: Repo{Root: root, Name: "repo"},
			}, workspace)
			if err == nil {
				t.Fatal("sync unexpectedly succeeded")
			}
			content, err := os.ReadFile(previous)
			if err != nil || string(content) != "keep me" {
				t.Fatalf("previous workspace content=%q err=%v", content, err)
			}
			if matches, err := filepath.Glob(filepath.Join(filepath.Dir(workspace), ".workspace.crabbox-sync-*")); err != nil || len(matches) != 0 {
				t.Fatalf("staging directories=%v err=%v, want none", matches, err)
			}
			if !client.cleanupDeadlineSet {
				t.Fatal("failed sync cleanup did not use a bounded context")
			}
		})
	}
}

func TestE2BSyncWorkspaceHonorsConfiguredTimeout(t *testing.T) {
	root := newE2BSyncTestRepo(t)
	client := &fakeE2BSyncClient{uploadWaitForCancel: true}
	backend := &e2bBackend{rt: Runtime{Stderr: io.Discard}}
	backend.cfg.Sync.Timeout = 500 * time.Millisecond
	started := time.Now()

	_, _, err := backend.syncWorkspace(t.Context(), client, e2bSession{SandboxID: "sbx_1"}, RunRequest{
		Repo: Repo{Root: root, Name: "repo"},
	}, "/home/user/repo")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("sync err=%v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("sync timeout returned after %s", elapsed)
	}
	if !client.uploadDeadlineSet {
		t.Fatal("upload did not receive the configured sync deadline")
	}
	if !client.cleanupDeadlineSet {
		t.Fatal("timed-out sync cleanup did not use an independent bounded context")
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

func TestE2BStatusWaitBoundsInFlightSandboxLookup(t *testing.T) {
	for _, tc := range []struct {
		name              string
		getWaitAfterCalls int
	}{
		{name: "lease resolution"},
		{name: "readiness poll", getWaitAfterCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			client := &fakeE2BSyncClient{}
			restore := swapNewE2BClient(client)
			defer restore()
			backend := &e2bBackend{
				cfg: Config{E2B: E2BConfig{APIURL: "https://api.example.test", Template: "base"}},
				rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
			}
			leaseID, _, _, err := backend.createSandbox(t.Context(), client, Repo{Root: t.TempDir()}, true, false, "")
			if err != nil {
				t.Fatal(err)
			}
			client.getWaitForCancel = true
			client.getWaitAfterCalls = tc.getWaitAfterCalls
			started := time.Now()

			_, err = backend.Status(t.Context(), StatusRequest{ID: leaseID, Wait: true, WaitTimeout: 30 * time.Millisecond})
			var exitErr ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != 5 || !strings.Contains(err.Error(), "timed out waiting for sandbox") {
				t.Fatalf("status err=%v, want sandbox wait timeout with exit code 5", err)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("in-flight sandbox lookup returned after %s", elapsed)
			}
		})
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

func TestE2BRunKeepOnFailureRetainsSandboxAfterSetupFailure(t *testing.T) {
	for _, tc := range []struct {
		name         string
		noSync       bool
		uploadErr    error
		connectErr   error
		processCodes []int
	}{
		{name: "archive sync", uploadErr: errors.New("upload failed")},
		{name: "workspace setup", noSync: true, processCodes: []int{7}},
		{name: "sandbox connection", noSync: true, connectErr: errors.New("connect failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			client := &fakeE2BSyncClient{
				uploadErr:    tc.uploadErr,
				connectErr:   tc.connectErr,
				processCodes: tc.processCodes,
			}
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

			result, err := backend.Run(t.Context(), RunRequest{
				Repo:          Repo{Name: "repo", Root: newE2BSyncTestRepo(t)},
				Command:       []string{"true"},
				KeepOnFailure: true,
				NoSync:        tc.noSync,
			})
			if err == nil {
				t.Fatal("run unexpectedly succeeded")
			}
			if result.Session == nil || !result.Session.Kept || result.Session.Reused {
				t.Fatalf("session=%#v, want retained new sandbox", result.Session)
			}
			if len(client.deleteIDs) != 0 {
				t.Fatalf("deleted sandbox=%v, want retained sandbox", client.deleteIDs)
			}
			if _, exists, claimErr := readLeaseClaimWithPresence(result.Session.LeaseID); claimErr != nil || !exists {
				t.Fatalf("retained sandbox claim exists=%t err=%v", exists, claimErr)
			}
			if !strings.Contains(stderr.String(), "keep-on-failure") {
				t.Fatalf("stderr=%q, want retention guidance", stderr.String())
			}
		})
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
	commands            []string
	users               []string
	sandbox             e2bSandbox
	createReq           e2bCreateSandboxRequest
	createCalls         int
	getIDs              []string
	listFilters         []map[string]string
	listedSandboxes     []e2bSandbox
	connectIDs          []string
	getErr              error
	getWaitForCancel    bool
	getWaitAfterCalls   int
	deleteIDs           []string
	deleteErr           error
	deleteDeadlineSet   bool
	uploadPath          string
	uploaded            bytes.Buffer
	uploadErr           error
	uploadWaitForCancel bool
	uploadDeadlineSet   bool
	connectErr          error
	executeShell        bool
	failExtract         bool
	cleanupDeadlineSet  bool
	processCodes        []int
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

func (f *fakeE2BSyncClient) ConnectSandbox(_ context.Context, sandboxID string, _ int) (e2bSession, error) {
	f.connectIDs = append(f.connectIDs, sandboxID)
	return e2bSession{}, f.connectErr
}

func (f *fakeE2BSyncClient) GetSandbox(ctx context.Context, sandboxID string) (e2bSandbox, error) {
	f.getIDs = append(f.getIDs, sandboxID)
	if f.getWaitForCancel && len(f.getIDs) > f.getWaitAfterCalls {
		<-ctx.Done()
		return e2bSandbox{}, ctx.Err()
	}
	if f.getErr != nil {
		return e2bSandbox{}, f.getErr
	}
	return f.sandbox, nil
}

func (f *fakeE2BSyncClient) ListSandboxes(_ context.Context, filter map[string]string) ([]e2bSandbox, error) {
	f.listFilters = append(f.listFilters, filter)
	return f.listedSandboxes, nil
}

func (f *fakeE2BSyncClient) DeleteSandbox(ctx context.Context, sandboxID string) error {
	f.deleteIDs = append(f.deleteIDs, sandboxID)
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		f.deleteDeadlineSet = remaining > 0 && remaining <= e2bCleanupTimeout
	}
	return f.deleteErr
}

func (f *fakeE2BSyncClient) UploadFile(ctx context.Context, _ e2bSession, targetPath string, r io.Reader) error {
	f.uploadPath = targetPath
	_, f.uploadDeadlineSet = ctx.Deadline()
	if f.uploadWaitForCancel {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.uploadErr != nil {
		return f.uploadErr
	}
	if _, err := io.Copy(&f.uploaded, r); err != nil {
		return err
	}
	if f.executeShell {
		return os.WriteFile(targetPath, f.uploaded.Bytes(), 0o600)
	}
	return nil
}

func (f *fakeE2BSyncClient) StartProcess(ctx context.Context, _ e2bSession, req e2bProcessRequest) (int, error) {
	f.commands = append(f.commands, req.Command)
	f.users = append(f.users, req.User)
	if strings.HasPrefix(req.Command, "rm -f ") {
		_, f.cleanupDeadlineSet = ctx.Deadline()
	}
	if f.failExtract && strings.HasPrefix(req.Command, "tar -xzf ") {
		return 7, nil
	}
	if len(f.processCodes) > 0 {
		code := f.processCodes[0]
		f.processCodes = f.processCodes[1:]
		return code, nil
	}
	if f.executeShell {
		if err := exec.CommandContext(ctx, "sh", "-c", req.Command).Run(); err != nil {
			return 1, err
		}
	}
	return 0, nil
}

func newE2BSyncTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "init")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	return root
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

func TestE2BRunLifecycleArchiveGuardrailBeforeCreation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeE2BSyncClient{}
	restore := swapNewE2BClient(client)
	defer restore()
	backend := &e2bBackend{cfg: Config{E2B: E2BConfig{Workdir: "repo"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	backend.cfg.Sync.FailFiles = 1
	result, err := backend.Run(t.Context(), RunRequest{Repo: Repo{Name: "repo", Root: newE2BSyncTestRepo(t)}, Command: []string{"true"}})
	var ee ExitError
	if !errors.As(err, &ee) || ee.Code != 6 || result.ExitCode != 6 || client.createCalls != 0 || result.Session != nil {
		t.Fatalf("result=%#v err=%v creates=%d", result, err, client.createCalls)
	}
}

func TestE2BRunLifecycleCleanupOutcomeAndTiming(t *testing.T) {
	for _, code := range []int{0, 7} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			client := &fakeE2BSyncClient{processCodes: []int{0, code}, deleteErr: errors.New("delete unavailable")}
			restore := swapNewE2BClient(client)
			defer restore()
			var stderr bytes.Buffer
			backend := &e2bBackend{cfg: Config{TTL: time.Minute, E2B: E2BConfig{Workdir: "repo"}}, rt: Runtime{Stdout: io.Discard, Stderr: &stderr}}
			result, err := backend.Run(t.Context(), RunRequest{Repo: Repo{Name: "repo", Root: t.TempDir()}, Command: []string{"true"}, NoSync: true, TimingJSON: true})
			wantCode, wantKind := code, core.RunErrorCommandExit
			if code == 0 {
				wantCode, wantKind = 1, core.RunErrorProvider
			}
			var ee ExitError
			if !errors.As(err, &ee) || ee.Code != wantCode || result.ExitCode != wantCode || result.ErrorKind != wantKind || result.Session == nil || !result.Session.Kept || len(client.deleteIDs) != 1 || !client.deleteDeadlineSet {
				t.Fatalf("result=%#v err=%v deletes=%v", result, err, client.deleteIDs)
			}
			if _, exists, claimErr := readLeaseClaimWithPresence(result.LeaseID); claimErr != nil || !exists {
				t.Fatalf("cleanup failure lost claim: exists=%t err=%v", exists, claimErr)
			}
			var report core.TimingReport
			for _, line := range strings.Split(stderr.String(), "\n") {
				if strings.HasPrefix(line, "{") {
					if err := json.Unmarshal([]byte(line), &report); err != nil {
						t.Fatal(err)
					}
				}
			}
			if report.ExitCode != result.ExitCode || report.RunStatus != result.Status || report.ErrorKind != result.ErrorKind || report.TotalMs != result.Total.Milliseconds() {
				t.Fatalf("report=%#v result=%#v", report, result)
			}
		})
	}
}

func TestRunCommandIntentReachesExistingShell(t *testing.T) {
	testutil.VerifyExistingShellCommandIntent(t, func(t *testing.T, root string, intent testutil.CommandIntent) testutil.ExistingShellResult {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		client := &fakeE2BSyncClient{}
		restore := swapNewE2BClient(client)
		t.Cleanup(restore)
		backend := &e2bBackend{cfg: Config{E2B: E2BConfig{Template: "base", Workdir: root}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
		_, runErr := backend.Run(t.Context(), RunRequest{
			Repo:               Repo{Root: root},
			NoSync:             true,
			Keep:               true,
			Command:            intent.Command,
			CommandLiteralArgs: intent.LiteralArgs,
			ShellMode:          intent.ShellMode,
		})
		result := testutil.ExistingShellResult{
			Commands: append([]string(nil), client.commands...),
			Err:      runErr,
		}
		var exitErr core.ExitError
		if errors.As(runErr, &exitErr) {
			result.ErrorCode = exitErr.Code
		}
		return result
	})
}

func TestE2BRunCanonicalIDPreservesInventoryRecovery(t *testing.T) {
	const requestedID = "cbx_aaaaaaaaaaaa"
	for _, state := range []string{"missing", "inventory", "legacy exact claim"} {
		t.Run(state, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			client := &fakeE2BSyncClient{}
			restore := swapNewE2BClient(client)
			defer restore()
			backend := &e2bBackend{cfg: Config{E2B: E2BConfig{APIURL: "https://api.example.test", Template: "base"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
			repo := Repo{Root: t.TempDir()}
			otherID, _, _, err := backend.createSandbox(t.Context(), client, repo, true, false, "cbx-aaaaaaaaaaaa")
			if err != nil {
				t.Fatal(err)
			}
			if state != "missing" {
				client.listedSandboxes = []e2bSandbox{{SandboxID: "sbx_requested", Metadata: map[string]string{"lease": requestedID, "slug": "requested", "provider": e2bProvider, "crabbox": "true"}}}
			}
			if state == "legacy exact claim" {
				if err := claimLeaseForRepoProvider(requestedID, "legacy", e2bProvider, repo.Root, time.Minute, false); err != nil {
					t.Fatal(err)
				}
			}
			before, exists, err := core.ReadLeaseClaimWithPresence(otherID)
			if err != nil || !exists {
				t.Fatal(err)
			}
			result, err := backend.Run(t.Context(), RunRequest{ID: requestedID, Repo: repo, Command: []string{"true"}, NoSync: true, Keep: true})
			if state == "missing" {
				var ee core.ExitError
				if !errors.As(err, &ee) || ee.Code != 4 || len(client.connectIDs) != 0 || len(client.commands) != 0 {
					t.Errorf("missing canonical run: err=%v connects=%v commands=%v", err, client.connectIDs, client.commands)
				}
			} else if err != nil || result.Session.LeaseID != requestedID || len(client.connectIDs) != 1 || client.connectIDs[0] != "sbx_requested" {
				t.Errorf("inventory recovery: result=%#v err=%v connects=%v", result, err, client.connectIDs)
			}
			if len(client.getIDs) != 0 || len(client.listFilters) != 1 || client.listFilters[0]["lease"] != requestedID || client.listFilters[0]["provider"] != e2bProvider {
				t.Errorf("canonical lookup did not use its own inventory: gets=%v lists=%v", client.getIDs, client.listFilters)
			}
			after, exists, err := core.ReadLeaseClaimWithPresence(otherID)
			beforeJSON, beforeErr := json.Marshal(before)
			afterJSON, afterErr := json.Marshal(after)
			if err != nil || !exists || beforeErr != nil || afterErr != nil || !bytes.Equal(beforeJSON, afterJSON) {
				t.Errorf("unrelated claim changed: read=%v marshal=%v/%v", err, beforeErr, afterErr)
			}
		})
	}
}

func TestE2BStopRejectsMissingCanonicalIDMatchingAnotherSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeE2BSyncClient{}
	restore := swapNewE2BClient(client)
	defer restore()
	backend := &e2bBackend{cfg: Config{E2B: E2BConfig{APIURL: "https://api.example.test", Template: "base"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	const missingID = "cbx_aaaaaaaaaaaa"
	leaseID, sandbox, _, err := backend.createSandbox(t.Context(), client, Repo{Root: t.TempDir()}, true, false, "cbx-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	if leaseID == missingID {
		t.Fatal("fixture unexpectedly allocated requested ID")
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists {
		t.Fatalf("fixture claim exists=%v err=%v", exists, err)
	}
	before, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	client.getIDs = nil
	err = backend.Stop(t.Context(), StopRequest{ID: missingID})
	var ee core.ExitError
	if !errors.As(err, &ee) || ee.Code != 4 {
		t.Errorf("missing canonical ID selected a slug: err=%v", err)
	}
	if len(client.getIDs) != 0 || len(client.deleteIDs) != 0 {
		t.Errorf("wrong target reached provider: gets=%v deletes=%v", client.getIDs, client.deleteIDs)
	}
	afterClaim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	after, marshalErr := json.Marshal(afterClaim)
	if err != nil || marshalErr != nil || !exists || !bytes.Equal(before, after) {
		t.Fatalf("unrelated claim changed: exists=%v err=%v marshal=%v", exists, err, marshalErr)
	}
	if err := backend.Stop(t.Context(), StopRequest{ID: leaseID}); err != nil {
		t.Fatal(err)
	}
	if len(client.deleteIDs) != 1 || client.deleteIDs[0] != sandbox.SandboxID {
		t.Fatalf("exact positive target=%v", client.deleteIDs)
	}
}

func parseE2BProcessStream(r io.Reader, stdout, stderr io.Writer, secrets ...string) (int, error) {
	return shared.ParseEnvdProcessStream("e2b", r, stdout, stderr, interpretE2BProcessEnd, secrets...)
}

func TestE2BProcessEndPolicy(t *testing.T) {
	readFailure := errors.New("fixture stream read failed")
	for _, code := range []int{0, 7, -1, 137} {
		t.Run(fmt.Sprintf("normal/%d", code), func(t *testing.T) {
			var diagnostic bytes.Buffer
			got, err := interpretE2BProcessEnd(shared.EnvdProcessEnd{ExitCode: code, Exited: true, Error: "ignored normal detail"}, &diagnostic)
			if got != code || err != nil || diagnostic.Len() != 0 {
				t.Fatalf("code=%d err=%v diagnostic=%q", got, err, diagnostic.String())
			}
		})
		for _, ending := range []string{"clean", "RPC failure", "read failure", "truncated"} {
			t.Run(fmt.Sprintf("abnormal/%d/%s", code, ending), func(t *testing.T) {
				body := testutil.GRPCWebEnvelope(0, map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": code, "exited": false, "error": "fixture end"}}})
				if ending == "RPC failure" {
					body = append(body, testutil.GRPCWebEnvelope(2, map[string]any{"error": map[string]any{"code": "internal", "message": "fixture RPC failure"}})...)
				}
				if ending == "truncated" {
					body = append(body, 0)
				}
				var reader io.Reader = bytes.NewReader(body)
				if ending == "read failure" {
					reader = io.MultiReader(reader, iotest.ErrReader(readFailure))
				}
				var diagnostic bytes.Buffer
				got, err := parseE2BProcessStream(reader, io.Discard, &diagnostic)
				want := code
				if ending != "clean" {
					want = 1
				}
				if got != want || (err != nil) != (ending != "clean") || diagnostic.String() != "fixture end\n" {
					t.Fatalf("code=%d err=%v diagnostic=%q", got, err, diagnostic.String())
				}
				if ending == "RPC failure" && !strings.Contains(err.Error(), "fixture RPC failure") {
					t.Fatalf("did not drain RPC error: %v", err)
				}
				if ending == "read failure" && !errors.Is(err, readFailure) {
					t.Fatalf("did not preserve later read error: %v", err)
				}
				if ending == "truncated" && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("did not read later frame: %v", err)
				}
			})
		}
	}
}

func TestE2BProcessEndDoesNotUseStatusAsDiagnostic(t *testing.T) {
	var diagnostic bytes.Buffer
	code, err := interpretE2BProcessEnd(shared.EnvdProcessEnd{ExitCode: -1, Status: "fixture status"}, &diagnostic)
	if code != -1 || err != nil || diagnostic.Len() != 0 {
		t.Fatalf("code=%d err=%v diagnostic=%q", code, err, diagnostic.String())
	}
}

func assertAdoptionRequest(t *testing.T, req *http.Request, ctx context.Context, endpoint, body string, headers http.Header) {
	t.Helper()
	if req.Context() != ctx || req.Method != http.MethodPost || req.URL.String() != endpoint {
		t.Fatalf("request context/method/URL mismatch: %s %s", req.Method, req.URL)
	}
	if !reflect.DeepEqual(req.Header, headers) {
		t.Fatalf("headers=%v want %v", req.Header, headers)
	}
	if req.ContentLength != int64(len(body)) || (req.Body == nil) != (body == "") {
		t.Fatalf("body metadata length=%d nil=%v want length=%d", req.ContentLength, req.Body == nil, len(body))
	}
	if body == "" {
		if req.GetBody != nil {
			t.Fatal("nil body gained replay")
		}
		return
	}
	data, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != body {
		t.Fatalf("body=%q want %q", data, body)
	}
	if req.GetBody == nil {
		t.Fatal("encoded body lost replay")
	}
	replay, err := req.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	data, err = io.ReadAll(replay)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != body {
		t.Fatalf("replay=%q want %q", data, body)
	}
}

func TestJSONRequestAdoptionEnvelope(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "capture")
	var typedNil *struct{ Value string }
	const base = "https://api.example.test/base"
	sentinel := errors.New("synthetic captured transport stop")
	for _, tc := range []struct {
		name        string
		body        any
		want        string
		query, fail bool
	}{
		{name: "nil"},
		{name: "typed nil", body: typedNil, want: "null\n"},
		{name: "JSON bytes", body: map[string]string{"message": "<&>"}, want: "{\"message\":\"\\u003c\\u0026\\u003e\"}\n"},
		{name: "query without body", query: true},
		{name: "transport error", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			endpoint := "/records"
			if tc.query {
				endpoint += "?limit=2&prefix=two+words"
			}
			var query url.Values
			if tc.query {
				query = url.Values{"limit": []string{"2"}, "prefix": []string{"two words"}}
			}
			headers := http.Header{"X-Api-Key": []string{"synthetic-token"}}
			headers.Set("Accept", "application/json")
			if tc.body != nil {
				headers.Set("Content-Type", "application/json")
			}
			transport := &http.Client{Transport: e2bRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				assertAdoptionRequest(t, req, ctx, base+endpoint, tc.want, headers)
				if tc.fail {
					return nil, sentinel
				}
				return &http.Response{StatusCode: 204, Header: http.Header{"X-Capture": []string{"yes"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
			})}
			c := &e2bClient{apiURL: base, apiKey: "synthetic-token", httpClient: transport}
			gotHeaders, err := c.doJSONWithHeaders(ctx, http.MethodPost, "/records", query, tc.body, nil)
			if tc.fail {
				if !errors.Is(err, sentinel) || gotHeaders != nil {
					t.Fatalf("error/headers=%v %v", err, gotHeaders)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !tc.fail && (gotHeaders.Get("X-Capture") != "yes") {
				t.Fatalf("response headers=%v", gotHeaders)
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

type responseCloseObserver struct {
	io.Reader
	close func() error
}

func (b responseCloseObserver) Close() error { return b.close() }

func TestRawJSONResponseContract(t *testing.T) {
	readFailure := errors.New("synthetic response read failure")
	closeFailure := errors.New("synthetic ignored close failure")
	for _, tc := range []struct {
		name, data        string
		status            int
		nilOut, readError bool
		wantError         string
		wantValue         int
	}{
		{name: "nil output still reads invalid JSON", data: "not JSON", nilOut: true, wantValue: 99},
		{name: "raw empty", wantValue: 99},
		{name: "whitespace with output", data: " \t\n", wantError: "syntax", wantValue: 99},
		{name: "whitespace without output", data: " \t\n", nilOut: true, wantValue: 99},
		{name: "JSON object", data: `{"value":12}`, wantValue: 12},
		{name: "JSON null", data: "null", wantValue: 99},
		{name: "JSON trailing whitespace", data: "{\"value\":12}\n \t", wantValue: 12},
		{name: "trailing JSON value", data: `{"value":12}{"value":13}`, wantError: "syntax", wantValue: 99},
		{name: "wrong JSON field type", data: `{"value":"bad"}`, wantError: "type", wantValue: 99},
		{name: "status error", data: "  unavailable \n", status: 503, wantError: "status", wantValue: 99},
		{name: "status error with nil output", data: "  unavailable \n", status: 503, nilOut: true, wantError: "status", wantValue: 99},
		{name: "read error beats success", data: `{"value":12}`, readError: true, wantError: "read", wantValue: 99},
		{name: "read error beats status and nil output", data: "partial body", status: 503, nilOut: true, readError: true, wantError: "read", wantValue: 99},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bytes.NewReader([]byte(tc.data))
			var reader io.Reader = payload
			if tc.readError {
				reader = io.MultiReader(payload, iotest.ErrReader(readFailure))
			}
			closed, calls := 0, 0
			responseHeaders := http.Header{"X-Trace": []string{"before-close", "second"}}
			body := responseCloseObserver{Reader: reader, close: func() error { closed++; responseHeaders["X-Trace"][0] = "after-close"; return closeFailure }}
			status := tc.status
			if status == 0 {
				status = 200
			}
			const base = "https://api.example.test"
			httpClient := &http.Client{Transport: e2bRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: status, Status: strconv.Itoa(status) + " Synthetic", Header: responseHeaders, Body: body, Request: req}, nil
			})}
			client := &e2bClient{apiURL: base, apiKey: "synthetic-token", httpClient: httpClient}
			out := struct {
				Value int `json:"value"`
			}{Value: 99}
			var destination any = &out
			if tc.nilOut {
				destination = nil
			}
			headers, err := client.doJSONWithHeaders(context.Background(), http.MethodGet, "/records", nil, nil, destination)
			if calls != 1 || closed != 1 || payload.Len() != 0 {
				t.Fatalf("calls=%d closes=%d unread=%d", calls, closed, payload.Len())
			}
			if out.Value != tc.wantValue {
				t.Fatalf("value=%d want%d", out.Value, tc.wantValue)
			}
			switch tc.wantError {
			case "":
				if err != nil {
					t.Fatal(err)
				}
			case "read":
				if err != readFailure {
					t.Fatalf("read error=%T %v want exact sentinel", err, err)
				}
			case "syntax":
				if _, ok := err.(*json.SyntaxError); !ok {
					t.Fatalf("decode error=%T %v want unwrapped SyntaxError", err, err)
				}
			case "type":
				if _, ok := err.(*json.UnmarshalTypeError); !ok {
					t.Fatalf("decode error=%T %v want unwrapped UnmarshalTypeError", err, err)
				}
			case "status":
				api, ok := err.(*e2bAPIError)
				if !ok || api.StatusCode != 503 || api.Status != "503 Synthetic" || api.Body != "unavailable" {
					t.Fatalf("status error=%T %#v", err, err)
				}
			}
			if tc.wantError != "" {
				if headers != nil {
					t.Fatalf("error headers=%v", headers)
				}
			} else {
				want := http.Header{"X-Trace": []string{"before-close", "second"}}
				if !reflect.DeepEqual(headers, want) {
					t.Fatalf("headers=%v want pre-close clone%v", headers, want)
				}
				headers["X-Trace"][0] = "caller-change"
				if responseHeaders["X-Trace"][0] != "after-close" {
					t.Fatal("returned headers share backing values")
				}
			}
		})
	}
}
