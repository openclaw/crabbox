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
	if len(spec.Features) != 1 || spec.Features[0] != "archive-sync" {
		t.Fatalf("spec.Features = %#v, want archive-sync", spec.Features)
	}
	if aliases := (Provider{}).Aliases(); len(aliases) != 1 || aliases[0] != "cf" {
		t.Fatalf("aliases = %#v, want [cf]", aliases)
	}
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
}

func TestCloudflareTokenFlagDoesNotDefaultToConfiguredSecret(t *testing.T) {
	cfg := Config{}
	cfg.Cloudflare.Token = "secret-token"
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterCloudflareProviderFlags(fs, cfg).(cloudflareFlagValues)
	if got := *values.Token; got != "" {
		t.Fatalf("token flag default = %q, want empty", got)
	}
}

func TestCloudflareFlagsApply(t *testing.T) {
	cfg := Config{Provider: providerName}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterCloudflareProviderFlags(fs, cfg)
	err := fs.Parse([]string{
		"--cloudflare-url", "https://current.example",
		"--cloudflare-token", "token",
		"--cloudflare-workdir", "/workspace/current",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyCloudflareProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Cloudflare.APIURL != "https://current.example" || cfg.Cloudflare.Token != "token" || cfg.Cloudflare.Workdir != "/workspace/current" {
		t.Fatalf("cloudflare flags not applied: %#v", cfg.Cloudflare)
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

func TestCloudflareAliasRejectsResourceFlags(t *testing.T) {
	cfg := Config{Provider: providerAlias}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	_ = fs.String("class", "", "")
	values := RegisterCloudflareProviderFlags(fs, cfg)
	if err := fs.Parse([]string{"--class", "standard"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCloudflareProviderFlags(&cfg, fs, values); err == nil {
		t.Fatal("expected provider alias to reject --class")
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

func TestCloudflareResolveClaimRequiresReclaimForOtherRepo(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoA := t.TempDir()
	repoB := t.TempDir()
	if err := claimLeaseForRepoProvider("cbx_claimed", "blue-lobster", providerName, repoA, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	backend := cloudflareBackend{}
	if _, _, _, err := backend.resolveSandboxID("blue-lobster", repoB, false); err == nil || !strings.Contains(err.Error(), "use --reclaim") {
		t.Fatalf("resolve without reclaim err=%v, want reclaim guard", err)
	}
	leaseID, sandboxID, slug, err := backend.resolveSandboxID("blue-lobster", repoB, true)
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
	view, err := backend.Status(context.Background(), StatusRequest{ID: "blue-lobster"})
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
