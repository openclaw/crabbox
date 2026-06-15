package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCloudflareProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName {
		t.Fatalf("spec.Name = %q, want %q", spec.Name, providerName)
	}
	if spec.Kind != "delegated-run" {
		t.Fatalf("spec.Kind = %q, want delegated-run", spec.Kind)
	}
	if !hasCloudflareFeature(spec.Features, "archive-sync") || !hasCloudflareFeature(spec.Features, "cleanup") {
		t.Fatalf("spec.Features = %#v, want archive-sync and cleanup", spec.Features)
	}
	if !hasCloudflareFeature(spec.Features, "run-session") {
		t.Fatalf("spec.Features = %#v, want run-session", spec.Features)
	}
	if hasCloudflareFeature(spec.Features, "url-bridge") {
		t.Fatalf("spec.Features = %#v, should not advertise unsupported URL bridge", spec.Features)
	}
	if aliases := (Provider{}).Aliases(); len(aliases) != 1 || aliases[0] != "cf" {
		t.Fatalf("aliases = %#v, want [cf]", aliases)
	}
}

func TestCloudflareWarmupRejectsActionsRunner(t *testing.T) {
	backend := &cloudflareBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	err := backend.Warmup(context.Background(), WarmupRequest{ActionsRunner: true})
	if err == nil {
		t.Fatal("Warmup accepted --actions-runner")
	}
	if !strings.Contains(err.Error(), "--actions-runner is not supported") {
		t.Fatalf("Warmup error = %v", err)
	}
}

func hasCloudflareFeature(features FeatureSet, want Feature) bool {
	for _, feature := range features {
		if feature == want {
			return true
		}
	}
	return false
}

func TestCloudflareWorkdirRejectsBroadPaths(t *testing.T) {
	cfg := Config{}
	cfg.Cloudflare.Workdir = "/workspace"
	if _, err := cloudflareWorkdir(cfg); err == nil {
		t.Fatal("cloudflareWorkdir accepted broad /workspace path")
	}
}

func TestBuildCloudflareCommandQuotesArgv(t *testing.T) {
	got, err := buildCloudflareCommand([]string{"node", "-e", "console.log('ok')"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "'node' '-e' 'console.log('\\''ok'\\'')'"
	if got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
}

func TestCloudflareHealthyStateIsReady(t *testing.T) {
	if !cloudflareReady("healthy") {
		t.Fatal("healthy state should be ready")
	}
	if cloudflareReady("running") {
		t.Fatal("running state should not be ready")
	}
}

func TestCloudflareStoppedWithCodeIsTerminal(t *testing.T) {
	if !cloudflareTerminalState("stopped_with_code") {
		t.Fatal("stopped_with_code state should be terminal")
	}
}

func TestCloudflareTokenFlagIsNotRegistered(t *testing.T) {
	cfg := Config{}
	cfg.Cloudflare.Token = "secret-token"
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterCloudflareProviderFlags(fs, cfg)
	if fs.Lookup("cloudflare-token") != nil {
		t.Fatal("cloudflare-token flag registered")
	}
}

func TestCloudflareFlagsApply(t *testing.T) {
	cfg := Config{Provider: providerName}
	cfg.Cloudflare.Token = "configured-token"
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterCloudflareProviderFlags(fs, cfg)
	err := fs.Parse([]string{
		"--cloudflare-url", "https://current.example",
		"--cloudflare-workdir", "/workspace/current",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyCloudflareProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Cloudflare.APIURL != "https://current.example" || cfg.Cloudflare.Token != "configured-token" || cfg.Cloudflare.Workdir != "/workspace/current" {
		t.Fatalf("cloudflare flags not applied: %#v", cfg.Cloudflare)
	}
}

func TestCloudflareClientNormalizesBaseURL(t *testing.T) {
	cfg := Config{}
	cfg.Cloudflare.APIURL = " https://runner.example.com/base/ "
	cfg.Cloudflare.Token = "token"
	client, err := newCloudflareClient(cfg, Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if client.baseURL != "https://runner.example.com/base" {
		t.Fatalf("baseURL = %q, want normalized base URL", client.baseURL)
	}
}

func TestCloudflareClientUsesBoundedDefaultTransport(t *testing.T) {
	cfg := Config{}
	cfg.Cloudflare.APIURL = "http://127.0.0.1:8787"
	cfg.Cloudflare.Token = "token"
	client, err := newCloudflareClient(cfg, Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if client.http == nil || client.http == http.DefaultClient {
		t.Fatalf("default http client=%#v, want bounded private client", client.http)
	}
	if client.http.Timeout != 0 {
		t.Fatalf("whole-response timeout=%s, want caller context to govern streams", client.http.Timeout)
	}
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport=%T, want *http.Transport", client.http.Transport)
	}
	if transport.ResponseHeaderTimeout != cloudflareDefaultResponseHeaderTimeout {
		t.Fatalf("response header timeout=%s, want %s", transport.ResponseHeaderTimeout, cloudflareDefaultResponseHeaderTimeout)
	}
}

func TestCloudflareClientRejectsURLQueryAndFragment(t *testing.T) {
	for _, rawURL := range []string{
		"https://runner.example.com?",
		"https://runner.example.com/?",
		"https://runner.example.com?token=leaky",
		"https://runner.example.com/#sandbox",
	} {
		t.Run(rawURL, func(t *testing.T) {
			cfg := Config{}
			cfg.Cloudflare.APIURL = rawURL
			cfg.Cloudflare.Token = "token"
			_, err := newCloudflareClient(cfg, Runtime{})
			if err == nil {
				t.Fatal("newCloudflareClient accepted URL query or fragment")
			}
			if !strings.Contains(err.Error(), "must not include query or fragment") {
				t.Fatalf("error = %v, want query or fragment message", err)
			}
		})
	}
}

func TestCloudflareClientRejectsURLUserinfoWithoutLeakingIt(t *testing.T) {
	for _, rawURL := range []string{
		"https://secret-token@runner.example.com",
		"http://secret-token@runner.example.com",
	} {
		t.Run(rawURL, func(t *testing.T) {
			cfg := Config{}
			cfg.Cloudflare.APIURL = rawURL
			cfg.Cloudflare.Token = "token"
			_, err := newCloudflareClient(cfg, Runtime{})
			if err == nil {
				t.Fatal("newCloudflareClient accepted URL userinfo")
			}
			if !strings.Contains(err.Error(), "must not include userinfo") {
				t.Fatalf("error = %v, want userinfo message", err)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("error leaked URL userinfo: %v", err)
			}
		})
	}
}

func TestCloudflareDoctorChecksRunnerAuth(t *testing.T) {
	var sawAuth bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/readiness" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		sawAuth = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"runner":"cloudflare"}`)
	}))
	defer server.Close()

	cfg := Config{Provider: providerName, Class: "beast"}
	cfg.Cloudflare.APIURL = server.URL
	cfg.Cloudflare.Token = "token"
	cfg.ServerType = cloudflareContainerInstanceTypeForClass(cfg.Class)
	backend := NewCloudflareBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*cloudflareBackend)
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !sawAuth {
		t.Fatal("doctor did not make authenticated runner request")
	}
	if result.Provider != providerName || !strings.Contains(result.Message, "auth=ready") || !strings.Contains(result.Message, "api=readiness") || !strings.Contains(result.Message, "type=standard-4") {
		t.Fatalf("doctor result = %#v", result)
	}
}

func TestCloudflareDoctorTimesOutStalledRunnerReadiness(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/readiness" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	oldTimeout := cloudflareDoctorTimeout
	cloudflareDoctorTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		cloudflareDoctorTimeout = oldTimeout
	})

	cfg := Config{Provider: providerName}
	cfg.Cloudflare.APIURL = server.URL
	cfg.Cloudflare.Token = "token"
	backend := NewCloudflareBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*cloudflareBackend)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	started := time.Now()
	_, err := backend.Doctor(ctx, DoctorRequest{})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("doctor succeeded against stalled runner")
	}
	if elapsed >= time.Second {
		t.Fatalf("doctor took %s, want bounded timeout", elapsed)
	}
}

func TestCloudflareDoctorRejectsInvalidReadinessPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/readiness" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "runner": "other"})
	}))
	defer server.Close()

	cfg := Config{Provider: providerName}
	cfg.Cloudflare.APIURL = server.URL
	cfg.Cloudflare.Token = "token"
	backend := NewCloudflareBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*cloudflareBackend)
	err := func() error {
		_, err := backend.Doctor(context.Background(), DoctorRequest{})
		return err
	}()
	if err == nil || !strings.Contains(err.Error(), "readiness response is invalid") {
		t.Fatalf("doctor err = %v, want invalid readiness payload", err)
	}
}

func TestCloudflareDoctorRejectsWrongRunnerPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}))
	defer server.Close()

	cfg := Config{Provider: providerName}
	cfg.Cloudflare.APIURL = server.URL + "/wrong"
	cfg.Cloudflare.Token = "token"
	backend := NewCloudflareBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*cloudflareBackend)
	err := func() error {
		_, err := backend.Doctor(context.Background(), DoctorRequest{})
		return err
	}()
	if err == nil || !cloudflareNotFoundError(err) {
		t.Fatalf("doctor err = %v, want not-found for wrong runner path", err)
	}
}

func TestCloudflareDoctorRejectsUnauthorizedRunner(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	cfg := Config{Provider: providerName}
	cfg.Cloudflare.APIURL = server.URL
	cfg.Cloudflare.Token = "wrong"
	backend := NewCloudflareBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*cloudflareBackend)
	err := func() error {
		_, err := backend.Doctor(context.Background(), DoctorRequest{})
		return err
	}()
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("doctor err = %v, want unauthorized", err)
	}
}

func TestCloudflareDoctorRejectsMissingRunnerConfig(t *testing.T) {
	backend := NewCloudflareBackend(Provider{}.Spec(), Config{Provider: providerName}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*cloudflareBackend)
	err := func() error {
		_, err := backend.Doctor(context.Background(), DoctorRequest{})
		return err
	}()
	if err == nil || !strings.Contains(err.Error(), "requires --cloudflare-url") {
		t.Fatalf("doctor err = %v, want missing URL", err)
	}
}

func TestCloudflareCreateSandboxSendsInstanceType(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var got createSandboxRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandboxes" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode create request: %v", err)
		}
		_, _ = fmt.Fprintf(w, `{"id":%q,"state":"running","workdir":%q,"instanceType":%q}`, got.ID, got.Workdir, got.InstanceType)
	}))
	defer server.Close()

	cfg := Config{Provider: providerName, Class: "fast"}
	cfg.ServerType = cloudflareContainerInstanceTypeForClass(cfg.Class)
	cfg.Cloudflare.APIURL = server.URL
	cfg.Cloudflare.Token = "token"
	rt := Runtime{HTTP: server.Client()}
	backend := NewCloudflareBackend(Provider{}.Spec(), cfg, rt).(*cloudflareBackend)
	client, err := newCloudflareClient(cfg, rt)
	if err != nil {
		t.Fatal(err)
	}
	leaseID, _, slug, err := backend.createSandbox(context.Background(), client, Repo{Name: "my-app", Root: t.TempDir()}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.InstanceType != "standard-4" {
		t.Fatalf("instance type = %q, want standard-4", got.InstanceType)
	}
	claim, ok, err := resolveLeaseClaimForProvider(slug, providerName)
	if err != nil || !ok {
		t.Fatalf("claim for %s ok=%t err=%v", slug, ok, err)
	}
	if claim.LeaseID != leaseID || claim.Labels["instance_type"] != "standard-4" {
		t.Fatalf("claim lease=%q labels=%#v, want instance_type=standard-4", claim.LeaseID, claim.Labels)
	}
}

func TestCloudflareListRefreshChecksClaimState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := claimLeaseForRepoProvider("cbx_live", "blue-lobster", providerName, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepoProvider("cbx_missing", "red-lobster", providerName, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sandboxes/cbx_live":
			_, _ = io.WriteString(w, `{"id":"cbx_live","state":"healthy","instanceType":"lite","labels":{"slug":"blue-lobster"}}`)
		case "/v1/sandboxes/cbx_missing":
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := Config{}
	cfg.Cloudflare.APIURL = server.URL
	cfg.Cloudflare.Token = "token"
	cfg.ServerType = "lite"
	backend := cloudflareBackend{cfg: cfg, rt: Runtime{HTTP: server.Client(), Stderr: io.Discard}}
	servers, err := backend.List(context.Background(), ListRequest{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, server := range servers {
		states[server.Labels["slug"]] = server.Status
	}
	if states["blue-lobster"] != "healthy" || states["red-lobster"] != "missing" {
		t.Fatalf("states = %#v, want refreshed healthy and missing", states)
	}
}

func TestCloudflareStatusUsesClaimedInstanceType(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := claimLeaseForRepoProviderPondLabels("cbx_lite", "blue-lobster", providerName, "", t.TempDir(), time.Hour, false, map[string]string{"instance_type": "lite"}); err != nil {
		t.Fatal(err)
	}
	var gotInstanceType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandboxes/cbx_lite" {
			http.NotFound(w, r)
			return
		}
		gotInstanceType = r.URL.Query().Get("instanceType")
		_, _ = fmt.Fprint(w, `{"id":"cbx_lite","state":"healthy","workdir":"/workspace/repo","instanceType":"lite"}`)
	}))
	defer server.Close()

	cfg := Config{Provider: providerName, ServerType: "standard-4"}
	cfg.Cloudflare.APIURL = server.URL
	cfg.Cloudflare.Token = "token"
	backend := cloudflareBackend{cfg: cfg, rt: Runtime{HTTP: server.Client(), Stderr: io.Discard}}
	view, err := backend.Status(context.Background(), StatusRequest{ID: "blue-lobster"})
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != "cbx_lite" || gotInstanceType != "lite" {
		t.Fatalf("view=%#v instanceType=%q, want claimed lite sandbox", view, gotInstanceType)
	}
}

func TestCloudflarePrepareWorkspacePreservesWhenRequested(t *testing.T) {
	for _, tc := range []struct {
		name           string
		deleteContents bool
		wantDelete     bool
	}{
		{name: "preserve", deleteContents: false, wantDelete: false},
		{name: "delete", deleteContents: true, wantDelete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got execStreamRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/sandboxes/cbx_test/exec-stream" {
					http.NotFound(w, r)
					return
				}
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Fatalf("decode exec request: %v", err)
				}
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = io.WriteString(w, `{"type":"complete","exitCode":0}`+"\n")
			}))
			defer server.Close()

			cfg := Config{}
			cfg.Cloudflare.APIURL = server.URL
			cfg.Cloudflare.Token = "token"
			backend := cloudflareBackend{cfg: cfg, rt: Runtime{HTTP: server.Client(), Stderr: io.Discard}}
			client, err := newCloudflareClient(cfg, backend.rt)
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.prepareWorkspace(context.Background(), client, "cbx_test", "/workspace/repo", tc.deleteContents); err != nil {
				t.Fatal(err)
			}
			hasDelete := strings.Contains(got.Command, "rm -rf")
			if hasDelete != tc.wantDelete {
				t.Fatalf("prepare command = %q, rm -rf presence = %t, want %t", got.Command, hasDelete, tc.wantDelete)
			}
		})
	}
}

func TestCloudflareRemoteDiskCheckRejectsSmallContainer(t *testing.T) {
	var got execStreamRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandboxes/cbx_test/exec-stream" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode exec request: %v", err)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"type":"stdout","data":"1048576 /workspace/repo\n"}`+"\n")
		_, _ = io.WriteString(w, `{"type":"complete","exitCode":0}`+"\n")
	}))
	defer server.Close()

	cfg := Config{}
	cfg.Cloudflare.APIURL = server.URL
	cfg.Cloudflare.Token = "token"
	backend := cloudflareBackend{cfg: cfg, rt: Runtime{HTTP: server.Client(), Stderr: io.Discard}}
	client, err := newCloudflareClient(cfg, backend.rt)
	if err != nil {
		t.Fatal(err)
	}
	err = backend.checkRemoteDiskForSync(context.Background(), client, "cbx_test", "/workspace/repo", 2<<20, 1<<20)
	if err == nil {
		t.Fatal("expected disk check to reject sync")
	}
	if !strings.Contains(err.Error(), "remote disk too small for sync") {
		t.Fatalf("error = %v, want remote disk message", err)
	}
	if !strings.Contains(got.Command, "df -B1") {
		t.Fatalf("disk check command = %q, want df probe", got.Command)
	}
}

func TestCloudflareAliasAcceptsResourceFlags(t *testing.T) {
	cfg := Config{Provider: providerAlias, ServerType: cloudflareContainerInstanceTypeForClass("standard")}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	_ = fs.String("class", "", "")
	values := RegisterCloudflareProviderFlags(fs, cfg)
	if err := fs.Parse([]string{"--class", "standard"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCloudflareProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.ServerType != "standard-4" {
		t.Fatalf("server type = %q, want standard-4", cfg.ServerType)
	}
}

func TestCloudflareRejectsUnsupportedInstanceType(t *testing.T) {
	cfg := Config{Provider: providerName, ServerType: "ccx63", ServerTypeExplicit: true}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterCloudflareProviderFlags(fs, cfg)
	if err := ApplyCloudflareProviderFlags(&cfg, fs, values); err == nil {
		t.Fatal("expected unsupported instance type error")
	}
}

func TestCloudflareClientExecStream(t *testing.T) {
	var token string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"heartbeat"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"stdout","data":"hello\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"stderr","data":"warn\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"complete","exitCode":7}` + "\n"))
	}))
	defer server.Close()

	token = "test-token"
	cfg := Config{}
	cfg.Cloudflare.APIURL = server.URL
	cfg.Cloudflare.Token = token
	client, err := newCloudflareClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code, err := client.execStream(context.Background(), "cbx_test", execStreamRequest{Command: "true"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
	if stdout.String() != "hello\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.String() != "warn\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestCloudflareClientExecStreamPropagatesWriterErrors(t *testing.T) {
	tests := []struct {
		name      string
		event     string
		stdout    io.Writer
		stderr    io.Writer
		want      string
		writerErr string
	}{
		{
			name:      "stdout",
			event:     `{"type":"stdout","data":"hello"}`,
			stdout:    cloudflareErrWriter("stdout closed"),
			stderr:    io.Discard,
			want:      "write cloudflare stdout",
			writerErr: "stdout closed",
		},
		{
			name:      "stderr",
			event:     `{"type":"stderr","data":"warn"}`,
			stdout:    io.Discard,
			stderr:    cloudflareErrWriter("stderr closed"),
			want:      "write cloudflare stderr",
			writerErr: "stderr closed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = io.WriteString(w, tc.event+"\n")
			}))
			defer server.Close()

			cfg := Config{}
			cfg.Cloudflare.APIURL = server.URL
			cfg.Cloudflare.Token = "test-token"
			client, err := newCloudflareClient(cfg, Runtime{HTTP: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.execStream(context.Background(), "cbx_test", execStreamRequest{Command: "true"}, tc.stdout, tc.stderr)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.writerErr) {
				t.Fatalf("execStream error = %v, want %q and %q", err, tc.want, tc.writerErr)
			}
		})
	}
}

type cloudflareErrWriter string

func (w cloudflareErrWriter) Write([]byte) (int, error) {
	return 0, fmt.Errorf("%s", string(w))
}

func TestCloudflareRunReportsCommandErrorAsFailure(t *testing.T) {
	execCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandboxes/cbx_test/exec-stream" {
			http.NotFound(w, r)
			return
		}
		execCalls++
		w.Header().Set("Content-Type", "application/x-ndjson")
		if execCalls == 1 {
			_, _ = io.WriteString(w, `{"type":"complete","exitCode":0}`+"\n")
			return
		}
		http.Error(w, "expired", http.StatusGone)
	}))
	defer server.Close()

	var stderr bytes.Buffer
	cfg := Config{}
	cfg.Cloudflare.APIURL = server.URL
	cfg.Cloudflare.Token = "token"
	backend := cloudflareBackend{cfg: cfg, rt: Runtime{HTTP: server.Client(), Stderr: &stderr, Stdout: io.Discard}}
	_, err := backend.Run(context.Background(), RunRequest{
		ID:         "cbx_test",
		NoSync:     true,
		Command:    []string{"true"},
		TimingJSON: true,
	})
	if err == nil {
		t.Fatal("Run succeeded after command stream error")
	}
	if !strings.Contains(stderr.String(), "exit=1") {
		t.Fatalf("stderr = %q, want summary exit=1", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"exitCode":1`) {
		t.Fatalf("stderr = %q, want timing exitCode=1", stderr.String())
	}
}

func TestCloudflareRunCleanupDestroyUsesBoundedContext(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	withCloudflareCleanupTimeout(t, 20*time.Millisecond)
	var createdID string
	execCalls := 0
	deleteSeen := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			var req createSandboxRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode create request: %v", err)
			}
			createdID = req.ID
			_, _ = fmt.Fprintf(w, `{"id":%q,"state":"running","workdir":%q}`, req.ID, req.Workdir)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/"+createdID+"/exec-stream":
			execCalls++
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, `{"type":"complete","exitCode":0}`+"\n")
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/sandboxes/"+createdID:
			deleteSeen <- struct{}{}
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := Config{Provider: providerName}
	cfg.Cloudflare.APIURL = server.URL
	cfg.Cloudflare.Token = "token"
	var stderr bytes.Buffer
	backend := cloudflareBackend{cfg: cfg, rt: Runtime{HTTP: server.Client(), Stderr: &stderr, Stdout: io.Discard}}
	start := time.Now()
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "repo", Root: t.TempDir()},
		Command: []string{"true"},
		NoSync:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("result=%#v", result)
	}
	if execCalls != 2 {
		t.Fatalf("exec calls=%d, want prepare and command", execCalls)
	}
	select {
	case <-deleteSeen:
	default:
		t.Fatal("destroy was not attempted")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Run took %s, want bounded cleanup", elapsed)
	}
	if !strings.Contains(stderr.String(), "warning: cloudflare destroy failed for "+createdID+":") || !strings.Contains(stderr.String(), "context deadline exceeded") {
		t.Fatalf("stderr=%q, want destroy timeout warning", stderr.String())
	}
}

func TestCloudflareRunKeepReturnsSessionHandle(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var createdID string
	deleteCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			var req createSandboxRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode create request: %v", err)
			}
			createdID = req.ID
			_, _ = fmt.Fprintf(w, `{"id":%q,"state":"running","workdir":%q}`, req.ID, req.Workdir)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/"+createdID+"/exec-stream":
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, `{"type":"complete","exitCode":0}`+"\n")
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/sandboxes/"+createdID:
			deleteCalls++
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := Config{Provider: providerName}
	cfg.Cloudflare.APIURL = server.URL
	cfg.Cloudflare.Token = "token"
	backend := cloudflareBackend{cfg: cfg, rt: Runtime{HTTP: server.Client(), Stderr: io.Discard, Stdout: io.Discard}}
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "repo", Root: t.TempDir()},
		Command: []string{"true"},
		NoSync:  true,
		Keep:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if deleteCalls != 0 {
		t.Fatalf("destroy called %d times with --keep, want 0", deleteCalls)
	}
	if result.Session == nil {
		t.Fatal("result.Session = nil, want a run-session handle")
	}
	if result.Session.Provider != providerName {
		t.Fatalf("Session.Provider = %q, want %q", result.Session.Provider, providerName)
	}
	if result.Session.Reused {
		t.Fatal("Session.Reused = true, want false for a freshly acquired lease")
	}
	if !result.Session.Kept {
		t.Fatal("Session.Kept = false, want true under --keep")
	}
	if result.Session.LeaseID != createdID {
		t.Fatalf("Session.LeaseID = %q, want %q", result.Session.LeaseID, createdID)
	}
	if result.Session.RunID != "" {
		t.Fatalf("Session.RunID = %q, want empty because Cloudflare has no distinct run id", result.Session.RunID)
	}
	wantCleanup := cloudflareCleanupCommand(createdID)
	if result.Session.CleanupCommand != wantCleanup {
		t.Fatalf("Session.CleanupCommand = %q, want %q", result.Session.CleanupCommand, wantCleanup)
	}
}

func TestCloudflareCleanupCommandQuotesLeaseID(t *testing.T) {
	got := cloudflareCleanupCommand("cbx_test; touch /tmp/unsafe")
	want := "crabbox stop --provider cloudflare --id 'cbx_test; touch /tmp/unsafe'"
	if got != want {
		t.Fatalf("cloudflareCleanupCommand() = %q, want %q", got, want)
	}
}

func TestCloudflareRunKeepOnFailureRetainsSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var createdID string
	execCalls := 0
	deleteCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			var req createSandboxRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode create request: %v", err)
			}
			createdID = req.ID
			_, _ = fmt.Fprintf(w, `{"id":%q,"state":"running","workdir":%q}`, req.ID, req.Workdir)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/"+createdID+"/exec-stream":
			execCalls++
			w.Header().Set("Content-Type", "application/x-ndjson")
			if execCalls == 1 { // prepareWorkspace
				_, _ = io.WriteString(w, `{"type":"complete","exitCode":0}`+"\n")
				return
			}
			_, _ = io.WriteString(w, `{"type":"complete","exitCode":2}`+"\n") // command fails
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/sandboxes/"+createdID:
			deleteCalls++
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := Config{Provider: providerName}
	cfg.Cloudflare.APIURL = server.URL
	cfg.Cloudflare.Token = "token"
	backend := cloudflareBackend{cfg: cfg, rt: Runtime{HTTP: server.Client(), Stderr: io.Discard, Stdout: io.Discard}}
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:          Repo{Name: "repo", Root: t.TempDir()},
		Command:       []string{"false"},
		NoSync:        true,
		KeepOnFailure: true,
	})
	if err == nil {
		t.Fatal("Run succeeded, want non-nil error for failing command")
	}
	if deleteCalls != 0 {
		t.Fatalf("destroy called %d times with --keep-on-failure, want 0", deleteCalls)
	}
	if result.Session == nil {
		t.Fatal("result.Session = nil, want a retained run-session handle")
	}
	if !result.Session.Kept {
		t.Fatal("Session.Kept = false, want true after keep-on-failure")
	}
	if result.Session.LeaseID != createdID {
		t.Fatalf("Session.LeaseID = %q, want %q", result.Session.LeaseID, createdID)
	}
}

func TestCloudflareClientUploadSendsContentLength(t *testing.T) {
	var gotLength int64
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandboxes/cbx_test/files" {
			http.NotFound(w, r)
			return
		}
		gotLength = r.ContentLength
		gotPath = r.URL.Query().Get("path")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upload body: %v", err)
		}
		if string(body) != "archive" {
			t.Fatalf("upload body = %q, want archive", body)
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	cfg := Config{}
	cfg.Cloudflare.APIURL = server.URL
	cfg.Cloudflare.Token = "token"
	client, err := newCloudflareClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir() + "/archive.tgz"
	if err := os.WriteFile(local, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.uploadFile(context.Background(), "cbx_test", local, "/tmp/archive.tgz"); err != nil {
		t.Fatal(err)
	}
	if gotLength != int64(len("archive")) {
		t.Fatalf("ContentLength = %d, want %d", gotLength, len("archive"))
	}
	if gotPath != "/tmp/archive.tgz" {
		t.Fatalf("upload path = %q, want /tmp/archive.tgz", gotPath)
	}
}

func TestCloudflareClientRejectsPlainHTTPExceptLoopback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		apiURL  string
		wantErr bool
	}{
		{name: "https", apiURL: "https://runner.example.test", wantErr: false},
		{name: "loopback", apiURL: "http://127.0.0.1:8787", wantErr: false},
		{name: "localhost", apiURL: "http://localhost:8787", wantErr: false},
		{name: "remote http", apiURL: "http://runner.example.test", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{}
			cfg.Cloudflare.APIURL = tc.apiURL
			cfg.Cloudflare.Token = "token"
			_, err := newCloudflareClient(cfg, Runtime{})
			if tc.wantErr && err == nil {
				t.Fatal("expected URL validation error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected URL validation error: %v", err)
			}
		})
	}
}

func TestDurationCeil(t *testing.T) {
	if got := durationMillisecondsCeil(1500 * time.Microsecond); got != 2 {
		t.Fatalf("durationMillisecondsCeil = %d, want 2", got)
	}
}

func withCloudflareCleanupTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	original := cloudflareCleanupTimeout
	cloudflareCleanupTimeout = timeout
	t.Cleanup(func() { cloudflareCleanupTimeout = original })
}

func TestCloudflareResolveClaimRequiresReclaimForOtherRepo(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoA := t.TempDir()
	repoB := t.TempDir()
	if err := claimLeaseForRepoProvider("cbx_claimed", "blue-lobster", providerName, repoA, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	backend := cloudflareBackend{}
	if _, _, _, _, err := backend.resolveSandboxID("blue-lobster", repoB, false); err == nil || !strings.Contains(err.Error(), "use --reclaim") {
		t.Fatalf("resolve without reclaim err=%v, want reclaim guard", err)
	}
	leaseID, sandboxID, slug, _, err := backend.resolveSandboxID("blue-lobster", repoB, true)
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "cbx_claimed" || sandboxID != "cbx_claimed" || slug != "blue-lobster" {
		t.Fatalf("resolved lease=%q sandbox=%q slug=%q", leaseID, sandboxID, slug)
	}
	claim, ok, err := resolveLeaseClaimForProvider("blue-lobster", providerName)
	if err != nil || !ok {
		t.Fatalf("resolve claim after reclaim ok=%t err=%v", ok, err)
	}
	if claim.RepoRoot != repoB {
		t.Fatalf("claim repo = %q, want %q", claim.RepoRoot, repoB)
	}
}

func TestCloudflareStatusPrunesExpiredClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandboxes/cbx_expired" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"cbx_expired","state":"expired","workdir":"/workspace/repo"}`)
	}))
	defer server.Close()

	if err := claimLeaseForRepoProvider("cbx_expired", "blue-lobster", providerName, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	backend := cloudflareBackend{
		cfg: Config{
			Provider: providerName,
			Cloudflare: CloudflareConfig{
				APIURL: server.URL,
				Token:  "token",
			},
		},
		rt: Runtime{HTTP: server.Client()},
	}
	view, err := backend.Status(context.Background(), StatusRequest{ID: "blue-lobster", Wait: true, WaitTimeout: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "expired" {
		t.Fatalf("state = %q, want expired", view.State)
	}
	if _, ok, err := resolveLeaseClaimForProvider("blue-lobster", providerName); err != nil || ok {
		t.Fatalf("claim resolved after expired status ok=%t err=%v", ok, err)
	}
}

func TestCloudflareStopPrunesMissingClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/sandboxes/cbx_missing" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}))
	defer server.Close()

	if err := claimLeaseForRepoProvider("cbx_missing", "stale-claim", providerName, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	backend := cloudflareBackend{
		cfg: Config{
			Provider: providerName,
			Cloudflare: CloudflareConfig{
				APIURL: server.URL,
				Token:  "token",
			},
		},
		rt: Runtime{HTTP: server.Client(), Stdout: &stdout},
	}
	if err := backend.Stop(context.Background(), StopRequest{ID: "stale-claim"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolveLeaseClaimForProvider("stale-claim", providerName); err != nil || ok {
		t.Fatalf("claim resolved after stale stop ok=%t err=%v", ok, err)
	}
	if !strings.Contains(stdout.String(), "removed stale cloudflare claim cbx_missing reason=not-found") {
		t.Fatalf("stdout = %q, want stale claim removal", stdout.String())
	}
}

func TestCloudflareRemoteDiskCheckRejectsZeroOrUnknownAvailable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stdout string
		want   string
	}{
		{name: "zero", stdout: "0 /workspace/repo\n", want: "remote disk too small for sync"},
		{name: "unknown", stdout: "not-a-number /workspace/repo\n", want: "could not determine remote disk headroom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/sandboxes/cbx_test/exec-stream" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = fmt.Fprintf(w, `{"type":"stdout","data":%q}`+"\n", tc.stdout)
				_, _ = io.WriteString(w, `{"type":"complete","exitCode":0}`+"\n")
			}))
			defer server.Close()

			cfg := Config{}
			cfg.Cloudflare.APIURL = server.URL
			cfg.Cloudflare.Token = "token"
			backend := cloudflareBackend{cfg: cfg, rt: Runtime{HTTP: server.Client(), Stderr: io.Discard}}
			client, err := newCloudflareClient(cfg, backend.rt)
			if err != nil {
				t.Fatal(err)
			}
			err = backend.checkRemoteDiskForSync(context.Background(), client, "cbx_test", "/workspace/repo", 1024, 1024)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCloudflareExtractArchiveCommandRemovesArchiveAfterFailure(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is required for extract cleanup command test")
	}
	dir := t.TempDir()
	archive := dir + "/bad archive.tgz"
	if err := os.WriteFile(archive, []byte("not a tar archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, "-lc", cloudflareExtractArchiveCommand(archive, dir))
	err = cmd.Run()
	if err == nil {
		t.Fatal("expected invalid archive extraction to fail")
	}
	if _, statErr := os.Stat(archive); !os.IsNotExist(statErr) {
		t.Fatalf("archive still exists after failed extract: %v", statErr)
	}
}

func TestCloudflareCleanupPrunesTerminalClaims(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sandboxes/cbx_expired":
			_, _ = fmt.Fprint(w, `{"id":"cbx_expired","state":"expired","workdir":"/workspace/repo"}`)
		case "/v1/sandboxes/cbx_running":
			_, _ = fmt.Fprint(w, `{"id":"cbx_running","state":"running","workdir":"/workspace/repo"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	repo := t.TempDir()
	if err := claimLeaseForRepoProvider("cbx_expired", "blue-lobster", providerName, repo, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepoProvider("cbx_running", "green-lobster", providerName, repo, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	backend := cloudflareBackend{
		cfg: Config{
			Provider: providerName,
			Cloudflare: CloudflareConfig{
				APIURL: server.URL,
				Token:  "token",
			},
		},
		rt: Runtime{HTTP: server.Client(), Stdout: &stdout},
	}
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolveLeaseClaimForProvider("blue-lobster", providerName); err != nil || ok {
		t.Fatalf("expired claim resolved after cleanup ok=%t err=%v", ok, err)
	}
	if _, ok, err := resolveLeaseClaimForProvider("green-lobster", providerName); err != nil || !ok {
		t.Fatalf("running claim missing after cleanup ok=%t err=%v", ok, err)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("removed=1 checked=2")) {
		t.Fatalf("cleanup output = %q, want removed summary", stdout.String())
	}
}
