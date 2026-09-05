package upstashbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecAndAliases(t *testing.T) {
	p := Provider{}
	if p.Name() != providerName {
		t.Fatalf("Name=%q want %s", p.Name(), providerName)
	}
	for _, alias := range []string{"upstash", "box", "upstashbox"} {
		got, err := core.ProviderFor(alias)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", alias, err)
		}
		if got.Name() != providerName {
			t.Fatalf("ProviderFor(%q).Name=%q", alias, got.Name())
		}
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("kind=%v want delegated-run", spec.Kind)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%v want never", spec.Coordinator)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v want linux", spec.Targets)
	}
	if !hasFeature(spec.Features, core.FeatureArchiveSync) {
		t.Fatalf("features=%#v want archive sync", spec.Features)
	}
	if !hasFeature(spec.Features, core.FeatureRunSession) {
		t.Fatalf("features=%#v want run-session", spec.Features)
	}
}

func TestStopRequiresExactScopedUpstashBoxOwnership(t *testing.T) {
	for _, test := range []struct {
		name          string
		identifier    string
		seedClaim     bool
		staleIdentity bool
		changedScope  bool
	}{
		{name: "unclaimed raw provider ID", identifier: "box_foreign"},
		{name: "unclaimed lease ID", identifier: "cbx_0123456789ab"},
		{name: "unclaimed slug", identifier: "foreign"},
		{name: "stale provider resource", identifier: "box_foreign", seedClaim: true, staleIdentity: true},
		{name: "different provider endpoint", identifier: "box_foreign", seedClaim: true, changedScope: true},
		{name: "exact claim", identifier: "box_foreign", seedClaim: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			box := boxData{ID: "box_foreign", Name: "crabbox-foreign-0123456789ab", Status: "running"}
			fake := &fakeAPI{box: box}
			withFakeAPI(t, fake)
			cfg := Config{UpstashBox: UpstashBoxConfig{APIKey: "test-key", BaseURL: "https://one.example.test"}}
			backend := NewBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
			leaseID := "cbx_0123456789ab"
			if test.seedClaim {
				server := boxToServer(backend.cfg, box)
				if test.staleIdentity {
					server.CloudID = "box_original"
				}
				if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "foreign", providerName, upstashBoxClaimScope(backend.cfg), "", t.TempDir(), time.Hour, false, server, core.SSHTarget{}); err != nil {
					t.Fatal(err)
				}
			}
			if test.changedScope {
				backend.cfg.UpstashBox.BaseURL = "https://two.example.test"
			}
			err := backend.Stop(context.Background(), StopRequest{ID: test.identifier})
			if test.seedClaim && !test.staleIdentity && !test.changedScope {
				if err != nil || strings.Join(fake.deletedIDs, ",") != box.ID {
					t.Fatalf("owned stop err=%v deleted=%v", err, fake.deletedIDs)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "exact local ownership claim") {
				t.Fatalf("unowned stop err=%v", err)
			}
			if len(fake.deletedIDs) != 0 {
				t.Fatalf("unowned box was deleted: %v", fake.deletedIDs)
			}
		})
	}
}

func TestStopFinalizesClaimWhenUpstashBoxVanishesDuringLockedPreflight(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	box := boxData{ID: "box_vanished", Name: "crabbox-vanished-0123456789ab", Status: "running"}
	fake := &fakeAPI{box: box, notFoundOnGet: 2}
	withFakeAPI(t, fake)
	cfg := Config{UpstashBox: UpstashBoxConfig{APIKey: "test-key", BaseURL: "https://one.example.test"}}
	backend := NewBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	leaseID := "cbx_0123456789ab"
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "vanished", providerName, upstashBoxClaimScope(backend.cfg), "", t.TempDir(), time.Hour, false, boxToServer(backend.cfg, box), core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	if err := backend.Stop(context.Background(), StopRequest{ID: box.ID}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedIDs) != 0 {
		t.Fatalf("missing box reached deletion: %v", fake.deletedIDs)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("claim retained exists=%t err=%v", exists, err)
	}
}

func TestStopFinalizesRetainedClaimAfterUpstashBoxWasAlreadyDeleted(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	box := boxData{ID: "box_deleted", Name: "crabbox-deleted-0123456789ab", Status: "running"}
	fake := &fakeAPI{box: box, notFoundOnGet: 1}
	withFakeAPI(t, fake)
	cfg := Config{UpstashBox: UpstashBoxConfig{APIKey: "test-key", BaseURL: "https://one.example.test"}}
	backend := NewBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	leaseID := "cbx_0123456789ab"
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "deleted", providerName, upstashBoxClaimScope(backend.cfg), "", t.TempDir(), time.Hour, false, boxToServer(backend.cfg, box), core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	if err := backend.Stop(context.Background(), StopRequest{ID: leaseID}); err != nil {
		t.Fatal(err)
	}
	if fake.getBoxCalls != 1 || len(fake.deletedIDs) != 0 {
		t.Fatalf("deleted box was rediscovered or deleted again: gets=%d deleted=%v", fake.getBoxCalls, fake.deletedIDs)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("claim retained exists=%t err=%v", exists, err)
	}
}

func TestClientUsesUpstashBoxRESTShape(t *testing.T) {
	var createBody map[string]any
	var deleteBody map[string]any
	uploadSeen := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Box-Api-Key"); got != "box_key" {
			t.Fatalf("X-Box-Api-Key=%q", got)
		}
		switch r.URL.Path {
		case "/v2/box":
			switch r.Method {
			case http.MethodPost:
				if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
					t.Fatal(err)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "box_1", "name": "crabbox-blue-123456789abc", "status": "running"})
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "box_1", "name": "crabbox-blue-123456789abc", "status": "running"}})
			case http.MethodDelete:
				if err := json.NewDecoder(r.Body).Decode(&deleteBody); err != nil {
					t.Fatal(err)
				}
				w.WriteHeader(http.StatusNoContent)
			default:
				t.Fatalf("unexpected method %s", r.Method)
			}
		case "/v2/box/box_1/exec":
			var body struct {
				Command []string `json:"command"`
				Folder  string   `json:"folder"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(body.Command, []string{"sh", "-c", "echo hi"}) || body.Folder != "crabbox" {
				t.Fatalf("exec body=%#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"exit_code": 0, "output": "hi\n"})
		case "/v2/box/box_1/exec-stream":
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, "hello\nevent: exit\ndata: {\"exit_code\":7}\n\n")
		case "/v2/box/box_1/files/upload":
			reader, err := r.MultipartReader()
			if err != nil {
				t.Fatal(err)
			}
			fields := readMultipart(t, reader)
			if fields["paths"] != "/tmp/archive.tgz" || fields["files"] != "archive" {
				t.Fatalf("multipart=%v", fields)
			}
			uploadSeen = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := &client{apiKey: "box_key", base: srv.URL, http: srv.Client()}
	box, err := client.CreateBox(context.Background(), createRequest{Name: "crabbox-blue-123456789abc", Runtime: "node", Size: "small", KeepAlive: true})
	if err != nil {
		t.Fatal(err)
	}
	if box.ID != "box_1" || createBody["runtime"] != "node" || createBody["size"] != "small" || createBody["keep_alive"] != true {
		t.Fatalf("create box=%#v body=%v", box, createBody)
	}
	if _, err := client.ListBoxes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if result, err := client.Exec(context.Background(), "box_1", "echo hi", "crabbox"); err != nil || result.Output != "hi\n" {
		t.Fatalf("exec result=%#v err=%v", result, err)
	}
	var stdout bytes.Buffer
	code, err := client.ExecStream(context.Background(), "box_1", "echo hi", "crabbox", &stdout)
	if err != nil || code != 7 || stdout.String() != "hello\n" {
		t.Fatalf("stream code=%d stdout=%q err=%v", code, stdout.String(), err)
	}
	archive := filepath.Join(t.TempDir(), "archive.tgz")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.UploadFile(context.Background(), "box_1", archive, "/tmp/archive.tgz"); err != nil {
		t.Fatal(err)
	}
	if !uploadSeen {
		t.Fatal("upload not seen")
	}
	if err := client.DeleteBoxes(context.Background(), []string{"box_1"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deleteBody["ids"], []any{"box_1"}) {
		t.Fatalf("delete body=%v", deleteBody)
	}
}

func TestClientRefusesCrossOriginRedirectBeforeReplay(t *testing.T) {
	var targetRequests int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests++
		t.Errorf("redirect target received %s %s key=%q", r.Method, r.URL.Path, r.Header.Get("X-Box-Api-Key"))
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer target.Close()

	trusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer trusted.Close()

	apiClient, err := newAPI(Config{UpstashBox: UpstashBoxConfig{APIKey: "box_key", BaseURL: trusted.URL}}, Runtime{HTTP: trusted.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = apiClient.ListBoxes(t.Context())
	if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("ListBoxes error = %v, want cross-origin refusal", err)
	}
	if targetRequests != 0 {
		t.Fatalf("redirect target received %d requests, want 0", targetRequests)
	}
}

func TestClientFollowsSameOriginRedirect(t *testing.T) {
	var redirectedKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/box":
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
		case "/redirected":
			redirectedKey = r.Header.Get("X-Box-Api-Key")
			_ = json.NewEncoder(w).Encode([]boxData{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	apiClient, err := newAPI(Config{UpstashBox: UpstashBoxConfig{APIKey: "box_key", BaseURL: server.URL}}, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apiClient.ListBoxes(t.Context()); err != nil {
		t.Fatal(err)
	}
	if redirectedKey != "box_key" {
		t.Fatalf("redirected key = %q", redirectedKey)
	}
}

func TestClientPreservesCallerRedirectPolicy(t *testing.T) {
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
	apiClient, err := newAPI(Config{UpstashBox: UpstashBoxConfig{APIKey: "box_key", BaseURL: server.URL}}, Runtime{HTTP: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	_, err = apiClient.ListBoxes(t.Context())
	if !errors.Is(err, callerErr) || callerChecks != 1 {
		t.Fatalf("ListBoxes error = %v, caller checks = %d", err, callerChecks)
	}
}

func TestClientRedactsAPIKeyFromErrors(t *testing.T) {
	const apiKey = "box_secret_live_proof"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Box-Api-Key"); got != apiKey {
			t.Errorf("X-Box-Api-Key=%q want configured key", got)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, "rejected X-Box-Api-Key %s", r.Header.Get("X-Box-Api-Key"))
	}))
	defer srv.Close()

	client := &client{apiKey: apiKey, base: srv.URL, http: srv.Client()}
	archive := filepath.Join(t.TempDir(), "archive.tgz")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "json", run: func() error {
			_, err := client.ListBoxes(context.Background())
			return err
		}},
		{name: "exec stream response", run: func() error {
			_, err := client.ExecStream(context.Background(), "box_1", "true", "", io.Discard)
			return err
		}},
		{name: "upload", run: func() error {
			return client.UploadFile(context.Background(), "box_1", archive, "/tmp/archive.tgz")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run()
			if err == nil {
				t.Fatal("expected API error")
			}
			message := err.Error()
			if strings.Contains(message, apiKey) {
				t.Fatalf("error contains API key: %q", message)
			}
			if !strings.Contains(message, "401 Unauthorized") || !strings.Contains(message, "[redacted]") {
				t.Fatalf("error=%q, want status and redaction marker", message)
			}
		})
	}
}

func TestUploadFileStopsProducerOnTransportFailure(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "archive.tgz")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	transportErr := errors.New("transport failed")
	client := &client{
		apiKey: "box_key",
		base:   "https://box.example.test",
		http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, transportErr
		})},
	}
	err := client.UploadFile(context.Background(), "box_1", archive, "/tmp/archive.tgz")
	if !errors.Is(err, transportErr) {
		t.Fatalf("UploadFile err=%v, want transport failure", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !uploadFileProducerRunning() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("upload producer goroutine still running after transport failure")
}

func TestNewAPIUsesBoundedDefaultHTTPClient(t *testing.T) {
	api, err := newAPI(testConfig(), Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	client, ok := api.(*client)
	if !ok {
		t.Fatalf("api=%T, want *client", api)
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
	if transport.ResponseHeaderTimeout != upstashBoxDefaultResponseHeaderTimeout {
		t.Fatalf("response header timeout=%s, want %s", transport.ResponseHeaderTimeout, upstashBoxDefaultResponseHeaderTimeout)
	}
}

func TestNewAPIRejectsUnsupportedDefaultTransport(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	calls := 0
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("deny all")
	})

	api, err := newAPI(testConfig(), Runtime{})
	if api != nil || err == nil || !strings.Contains(err.Error(), "non-nil *http.Transport") {
		t.Fatalf("api=%#v err=%v, want transport setup error", api, err)
	}
	if calls != 0 {
		t.Fatalf("custom default invoked %d times, want 0", calls)
	}
}

func TestNewAPIAcceptsExplicitClientWithUnsupportedDefault(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("deny all")
	})
	injectedTransport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("injected")
	})
	injected := &http.Client{Transport: injectedTransport}

	api, err := newAPI(testConfig(), Runtime{HTTP: injected})
	if err != nil {
		t.Fatal(err)
	}
	client, ok := api.(*client)
	if !ok {
		t.Fatalf("api=%T, want *client", api)
	}
	if _, ok := client.http.Transport.(roundTripFunc); !ok {
		t.Fatalf("transport=%T, want explicit roundTripFunc", client.http.Transport)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func uploadFileProducerRunning() bool {
	var stack bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&stack, 2); err != nil {
		return false
	}
	return strings.Contains(stack.String(), "upstashbox.(*client).UploadFile.func1")
}

func TestUpstashBoxCreateBoxDeletesFailedProvision(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/box":
			_ = json.NewEncoder(w).Encode(boxData{ID: "box_failed", Status: "failed"})
		case r.Method == http.MethodDelete && r.URL.Path == "/v2/box":
			var body struct {
				IDs []string `json:"ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			deleted = append(deleted, body.IDs...)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	client := &client{apiKey: "box_key", base: srv.URL, http: srv.Client()}
	_, err := client.CreateBox(context.Background(), createRequest{Name: "crabbox-failed"})
	if err == nil || !strings.Contains(err.Error(), "creation failed") {
		t.Fatalf("CreateBox err=%v, want creation failed", err)
	}
	if !reflect.DeepEqual(deleted, []string{"box_failed"}) {
		t.Fatalf("deleted=%v, want failed box cleanup", deleted)
	}
}

func TestUpstashBoxCreateBoxDeletesCancelledProvision(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/box":
			_ = json.NewEncoder(w).Encode(boxData{ID: "box_cancelled", Status: "provisioning"})
		case r.Method == http.MethodDelete && r.URL.Path == "/v2/box":
			var body struct {
				IDs []string `json:"ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			deleted = append(deleted, body.IDs...)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	client := &client{apiKey: "box_key", base: srv.URL, http: srv.Client()}
	_, err := client.CreateBox(ctx, createRequest{Name: "crabbox-cancelled"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CreateBox err=%v, want deadline exceeded", err)
	}
	if !reflect.DeepEqual(deleted, []string{"box_cancelled"}) {
		t.Fatalf("deleted=%v, want cancelled box cleanup", deleted)
	}
}

func TestCleanWorkdirAndCommand(t *testing.T) {
	if got, err := cleanWorkdir(" /workspace/home/crabbox/ "); err != nil || got != "/workspace/home/crabbox" {
		t.Fatalf("workdir=%q err=%v", got, err)
	}
	for _, value := range []string{"", "repo", "/", "/workspace", "/tmp"} {
		if _, err := cleanWorkdir(value); err == nil {
			t.Fatalf("cleanWorkdir(%q) succeeded", value)
		}
	}
}

func TestWarmupRejectsActionsRunner(t *testing.T) {
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	err := backend.Warmup(context.Background(), WarmupRequest{ActionsRunner: true})
	if err == nil || !strings.Contains(err.Error(), "--actions-runner is not supported") {
		t.Fatalf("err=%v, want actions-runner rejection", err)
	}
}

func TestParseExecStreamUsesFinalExitEvent(t *testing.T) {
	body := strings.Join([]string{
		"event: exit\n",
		"data: {\"exit_code\":0}\n\n",
		"still command output\n",
		"event: exit\n",
		"data: {\"exit_code\":7}\n\n",
	}, "")
	var stdout bytes.Buffer
	code, err := parseExecStream(strings.NewReader(body), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 {
		t.Fatalf("code=%d want 7", code)
	}
	if stdout.String() != "event: exit\ndata: {\"exit_code\":0}\n\nstill command output\n" {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestParseExecStreamRequiresExitEvent(t *testing.T) {
	var stdout bytes.Buffer
	code, err := parseExecStream(strings.NewReader("partial output"), &stdout)
	if err == nil || !strings.Contains(err.Error(), "without exit event") {
		t.Fatalf("code=%d err=%v, want missing exit event", code, err)
	}
	if stdout.String() != "partial output" {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestParseExecStreamRedactsAPIKeyFromErrorEvent(t *testing.T) {
	const apiKey = "box_secret_stream"
	body := "event: error\ndata: provider rejected " + apiKey + "\n\n"
	code, err := parseExecStream(strings.NewReader(body), io.Discard, apiKey)
	if code != 1 || err == nil {
		t.Fatalf("code=%d err=%v, want stream error", code, err)
	}
	if strings.Contains(err.Error(), apiKey) || !strings.Contains(err.Error(), "provider rejected [redacted]") {
		t.Fatalf("error=%q, want redacted API key", err)
	}
}

func TestRedactUpstashBoxSecretsIgnoresEmptyValues(t *testing.T) {
	const message = "upstash-box response"
	if got := redactUpstashBoxSecrets(message, "", "   "); got != message {
		t.Fatalf("redacted=%q want %q", got, message)
	}
}

func TestAPIErrorRedactsAPIKeyFromStatus(t *testing.T) {
	const apiKey = "box_secret_status"
	client := &client{apiKey: apiKey}
	err := client.apiError(&http.Response{
		Status: "401 rejected " + apiKey,
		Body:   io.NopCloser(strings.NewReader("")),
	})
	if strings.Contains(err.Error(), apiKey) || !strings.Contains(err.Error(), "401 rejected [redacted]") {
		t.Fatalf("error=%q, want redacted status", err)
	}
}

func TestRunCreatesExecsAndDeletesOneShotBox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{}
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "repo", Root: t.TempDir()},
		Command: []string{"echo", "hello"},
		NoSync:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Provider != providerName {
		t.Fatalf("result=%#v", result)
	}
	if result.Session == nil {
		t.Fatal("result.Session is nil")
	}
	if result.Session.Provider != providerName || result.Session.LeaseID != result.LeaseID || result.Session.Slug != result.Slug {
		t.Fatalf("session=%#v result=%#v", result.Session, result)
	}
	if result.Session.Reused {
		t.Fatalf("session.Reused=true, want false")
	}
	if result.Session.Kept {
		t.Fatalf("session.Kept=true, want false after cleanup")
	}
	if !strings.Contains(result.Session.CleanupCommand, "crabbox stop --provider upstash-box") {
		t.Fatalf("cleanup command=%q", result.Session.CleanupCommand)
	}
	if fake.createReq.Runtime != "node" || fake.createReq.Size != "small" {
		t.Fatalf("create req=%#v", fake.createReq)
	}
	if !reflect.DeepEqual(fake.verbs, []string{"create", "exec", "stream", "delete"}) {
		t.Fatalf("verbs=%v", fake.verbs)
	}
	if !strings.Contains(fake.execCommands[0], "mkdir -p 'crabbox'") || strings.Contains(fake.execCommands[0], "rm -rf") {
		t.Fatalf("prepare command=%q", fake.execCommands[0])
	}
	if fake.streamFolders[0] != "crabbox" || !strings.Contains(fake.streamCommands[0], "echo") {
		t.Fatalf("stream command=%q", fake.streamCommands[0])
	}
}

func TestRunCleanupDeleteUsesBoundedContext(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	withUpstashBoxCleanupTimeout(t, 20*time.Millisecond)
	fake := &fakeAPI{blockDelete: true}
	withFakeAPI(t, fake)
	var stderr bytes.Buffer
	backend := NewBackend(Provider{}.Spec(), testConfig(), Runtime{Stdout: io.Discard, Stderr: &stderr}).(*backend)
	start := time.Now()
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "repo", Root: t.TempDir()},
		Command: []string{"echo", "hello"},
		NoSync:  true,
	})
	if !errors.Is(err, context.DeadlineExceeded) || result.ExitCode != 1 || result.Status != core.RunStatusFailed || result.ErrorKind != core.RunErrorProvider {
		t.Fatalf("result=%#v err=%v, want failed deletion reported", result, err)
	}
	if result.Session == nil || !result.Session.Kept {
		t.Fatalf("session=%#v, want retained session after cleanup failure", result.Session)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Run took %s, want bounded cleanup", elapsed)
	}
	if claim, exists, err := core.ReadLeaseClaimWithPresence(result.LeaseID); err != nil || !exists || claim.LeaseID != result.LeaseID {
		t.Fatalf("failed deletion lost claim: exists=%v err=%v", exists, err)
	}
}

func TestRunKeepsFailuresBeforeCommand(t *testing.T) {
	for _, phase := range []string{"workspace", "archive upload", "command preparation", "environment upload"} {
		t.Run(phase, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			fake := &fakeAPI{}
			withFakeAPI(t, fake)
			req := RunRequest{Repo: Repo{Root: t.TempDir(), Name: "fixture"}, NoSync: true, KeepOnFailure: true, TimingJSON: true, Command: []string{"true"}}
			switch phase {
			case "workspace":
				fake.execHook = func(context.Context, string) (execResult, error) {
					return execResult{}, errors.New("workspace unavailable")
				}
			case "archive upload":
				req.NoSync = false
				req.Repo.Root = newGitRepo(t)
				fake.uploadHook = func(context.Context, string, string) error { return errors.New("archive upload unavailable") }
			case "command preparation":
				req.Command = nil
			case "environment upload":
				req.Env = map[string]string{"FIXTURE": "synthetic"}
				fake.uploadHook = func(context.Context, string, string) error { return errors.New("environment upload unavailable") }
			}
			var stderr bytes.Buffer
			b := NewBackend(Provider{}.Spec(), testConfig(), Runtime{Stdout: io.Discard, Stderr: &stderr}).(*backend)
			result, err := b.Run(t.Context(), req)
			if err == nil || result.Session == nil || !result.Session.Kept || result.Session.Reused || len(fake.deletedIDs) != 0 || len(fake.streamCommands) != 0 {
				t.Fatalf("early failure lost retention: result=%#v err=%v calls=%v", result, err, fake.verbs)
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence(result.LeaseID); err != nil || !exists {
				t.Fatalf("retained claim missing: exists=%v err=%v", exists, err)
			}
			lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
			var report core.TimingReport
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil || report.RunStatus != core.RunStatusFailed || report.ErrorKind != core.RunErrorProvider || report.ExitCode != result.ExitCode {
				t.Fatalf("early failure timing=%#v err=%v", report, err)
			}
		})
	}
}

type failingTimingWriter struct{ cause error }

func (w failingTimingWriter) Write(data []byte) (int, error) {
	if bytes.HasPrefix(data, []byte("{")) {
		return 0, w.cause
	}
	return len(data), nil
}

func TestRunTimingFailurePreservesCommandAndRetention(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{streamCode: 42}
	withFakeAPI(t, fake)
	cause := errors.New("synthetic timing write failure")
	b := NewBackend(Provider{}.Spec(), testConfig(), Runtime{Stdout: io.Discard, Stderr: failingTimingWriter{cause}}).(*backend)
	result, err := b.Run(t.Context(), RunRequest{Repo: Repo{Root: t.TempDir(), Name: "fixture"}, NoSync: true, KeepOnFailure: true, TimingJSON: true, Command: []string{"false"}})
	var exitErr ExitError
	if !errors.Is(err, cause) || !errors.As(err, &exitErr) || exitErr.Code != 42 || result.ExitCode != 42 || result.Status != core.RunStatusFailed || result.ErrorKind != core.RunErrorCommandExit || result.Session == nil || !result.Session.Kept || len(fake.deletedIDs) != 0 {
		t.Fatalf("timing masked command/retention: result=%#v err=%v deletes=%v", result, err, fake.deletedIDs)
	}
}

func TestRunEnvCleanupUsesBoundedContext(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	withUpstashBoxCleanupTimeout(t, 20*time.Millisecond)
	fake := &fakeAPI{blockEnvCleanup: true}
	withFakeAPI(t, fake)
	var stderr bytes.Buffer
	backend := NewBackend(Provider{}.Spec(), testConfig(), Runtime{Stdout: io.Discard, Stderr: &stderr}).(*backend)
	start := time.Now()
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:       Repo{Name: "repo", Root: t.TempDir()},
		Command:    []string{"echo", "hello"},
		Env:        map[string]string{"TOKEN": "secret"},
		NoSync:     true,
		Keep:       true,
		TimingJSON: true,
	})
	if err == nil || !strings.Contains(err.Error(), "upstash-box env cleanup failed for box_1: context deadline exceeded") {
		t.Fatalf("err=%v, want bounded env cleanup failure", err)
	}
	if result.ExitCode != 5 {
		t.Fatalf("result=%#v", result)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Run took %s, want bounded cleanup", elapsed)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	var report struct {
		ExitCode  int    `json:"exitCode"`
		RunStatus string `json:"runStatus"`
		ErrorKind string `json:"errorKind"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
		t.Fatalf("timing json: %v\nstderr=%s", err, stderr.String())
	}
	if report.ExitCode != 5 || report.RunStatus != "failed" || report.ErrorKind != "provider-error" {
		t.Fatalf("timing outcome exit=%d status=%q kind=%q", report.ExitCode, report.RunStatus, report.ErrorKind)
	}
}

func TestRunEnvCleanupFailsOnNonzeroExit(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{execResults: []execResult{{ExitCode: 0}, {ExitCode: 7, Error: "permission denied"}}}
	withFakeAPI(t, fake)
	var stderr bytes.Buffer
	backend := NewBackend(Provider{}.Spec(), testConfig(), Runtime{Stdout: io.Discard, Stderr: &stderr}).(*backend)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:       Repo{Name: "repo", Root: t.TempDir()},
		Command:    []string{"echo", "hello"},
		Env:        map[string]string{"TOKEN": "secret"},
		NoSync:     true,
		Keep:       true,
		TimingJSON: true,
	})
	if err == nil || !strings.Contains(err.Error(), "upstash-box env cleanup failed for box_1") || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err=%v, want env cleanup exit failure", err)
	}
	if result.ExitCode != 5 {
		t.Fatalf("result=%#v", result)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	var report struct {
		ExitCode int `json:"exitCode"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
		t.Fatalf("timing json: %v\nstderr=%s", err, stderr.String())
	}
	if report.ExitCode != 5 {
		t.Fatalf("timing exitCode=%d, want 5\nstderr=%s", report.ExitCode, stderr.String())
	}
}

func TestSyncWorkspaceCleansRemoteArchiveWhenExtractFails(t *testing.T) {
	fake := &fakeAPI{execResults: []execResult{{ExitCode: 0}, {ExitCode: 9, Error: "extract failed"}, {ExitCode: 0}}}
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	_, _, err := backend.syncWorkspace(context.Background(), fake, "box_1", RunRequest{
		Repo: Repo{Name: "repo", Root: newGitRepo(t)},
	}, "/workspace/home/crabbox", "crabbox")
	if err == nil {
		t.Fatal("expected extract failure")
	}
	if !reflect.DeepEqual(fake.verbs, []string{"upload", "exec", "exec", "exec"}) {
		t.Fatalf("verbs=%v", fake.verbs)
	}
	if !strings.Contains(fake.execCommands[2], "rm -f '.crabbox-upstash-box-sync-") {
		t.Fatalf("cleanup command=%q", fake.execCommands[2])
	}
}

func TestSyncWorkspaceNativePreservesExistingFilesOnTransferFailure(t *testing.T) {
	for _, failure := range []string{"upload", "extract", ""} {
		t.Run(blank(failure, "success"), func(t *testing.T) {
			root := t.TempDir()
			work := filepath.Join(root, "crabbox")
			if err := os.Mkdir(work, 0o755); err != nil {
				t.Fatal(err)
			}
			old := filepath.Join(work, "old.txt")
			if err := os.WriteFile(old, []byte("preserve-me"), 0o600); err != nil {
				t.Fatal(err)
			}
			client := &filesystemSyncAPI{root: root, failure: failure}
			cfg := testConfig()
			cfg.Sync.Delete = true
			backend := NewBackend(Provider{}.Spec(), cfg, testRuntime()).(*backend)
			_, _, err := backend.syncWorkspace(context.Background(), client, "box_1", RunRequest{Repo: Repo{Name: "repo", Root: newGitRepo(t)}}, "/workspace/home/crabbox", "crabbox")
			if failure != "" {
				if err == nil {
					t.Fatal("expected transfer failure")
				}
				got, readErr := os.ReadFile(old)
				if readErr != nil || string(got) != "preserve-me" {
					t.Fatalf("previous workspace lost: content=%q err=%v syncErr=%v", got, readErr, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(old); !os.IsNotExist(err) {
					t.Fatalf("old file remains: %v", err)
				}
				if got, err := os.ReadFile(filepath.Join(work, "hello.txt")); err != nil || string(got) != "hello" {
					t.Fatalf("new content=%q err=%v", got, err)
				}
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() != "crabbox" {
					t.Errorf("sync residue: %s", entry.Name())
				}
			}
		})
	}
}

func TestRunChecksArchiveBeforeAllocation(t *testing.T) {
	fake := &fakeAPI{}
	withFakeAPI(t, fake)
	cfg := testConfig()
	cfg.Sync.FailFiles = 1
	backend := NewBackend(Provider{}.Spec(), cfg, testRuntime()).(*backend)
	_, err := backend.Run(context.Background(), RunRequest{Repo: Repo{Root: newGitRepo(t)}, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "sync candidate too large") {
		t.Fatalf("err=%v", err)
	}
	if len(fake.verbs) != 0 {
		t.Fatalf("provider called before archive admission: %v", fake.verbs)
	}
}

type filesystemSyncAPI struct {
	fakeAPI
	root, failure string
}

func (f *filesystemSyncAPI) UploadFile(_ context.Context, _, localPath, remotePath string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	if f.failure == "extract" {
		data = []byte("corrupt archive")
	}
	if err := os.WriteFile(filepath.Join(f.root, filepath.Base(remotePath)), data, 0o600); err != nil {
		return err
	}
	if f.failure == "upload" {
		return errors.New("synthetic upload failed after writing remote bytes")
	}
	return nil
}

func (f *filesystemSyncAPI) Exec(ctx context.Context, _ string, command, _ string) (execResult, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = f.root
	output, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exited *exec.ExitError
		if !errors.As(err, &exited) {
			return execResult{}, err
		}
		code = exited.ExitCode()
	}
	return execResult{ExitCode: code, Output: string(output)}, nil
}

func TestSyncWorkspaceWarnsWhenRemoteArchiveCleanupExitsNonzero(t *testing.T) {
	fake := &fakeAPI{execResults: []execResult{{ExitCode: 0}, {ExitCode: 9, Error: "extract failed"}, {ExitCode: 7, Error: "cleanup denied"}}}
	var stderr bytes.Buffer
	backend := NewBackend(Provider{}.Spec(), testConfig(), Runtime{Stdout: io.Discard, Stderr: &stderr}).(*backend)
	_, _, err := backend.syncWorkspace(context.Background(), fake, "box_1", RunRequest{
		Repo: Repo{Name: "repo", Root: newGitRepo(t)},
	}, "/workspace/home/crabbox", "crabbox")
	if err == nil {
		t.Fatal("expected extract failure")
	}
	if !strings.Contains(stderr.String(), "warning: upstash-box sync cleanup failed:") || !strings.Contains(stderr.String(), "cleanup denied") {
		t.Fatalf("stderr=%q, want cleanup warning", stderr.String())
	}
}

func TestRunReusedBoxReturnsSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_123456789abc"
	fake := &fakeAPI{box: boxData{ID: "box_1", Name: "crabbox-blue-123456789abc", Runtime: "node", Size: "small", Status: "running"}}
	withFakeAPI(t, fake)
	if err := claimLeaseForRepoProvider(leaseID, "blue", providerName, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	result, err := backend.Run(context.Background(), RunRequest{
		ID: leaseID, Repo: Repo{Name: "repo", Root: "/repo"}, Command: []string{"echo", "hello"}, NoSync: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil || !result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v, want retained reused box", result.Session)
	}
	if result.Session.LeaseID != leaseID || result.Session.Slug != "blue" {
		t.Fatalf("session=%#v, want lease=%s slug=blue", result.Session, leaseID)
	}
	if len(fake.deletedIDs) != 0 {
		t.Fatalf("deleted=%#v, want reused box retained", fake.deletedIDs)
	}
}

func TestStatusMapsBoxName(t *testing.T) {
	fake := &fakeAPI{box: boxData{ID: "box_1", Name: "crabbox-blue-123456789abc", Runtime: "python", Size: "medium", Status: "running"}}
	withFakeAPI(t, fake)
	cfg := testConfig()
	cfg.UpstashBox.BaseURL = "https://eu-west-1.box.upstash.com"
	backend := NewBackend(Provider{}.Spec(), cfg, testRuntime()).(*backend)
	view, err := backend.Status(context.Background(), StatusRequest{ID: "box_1"})
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != "cbx_123456789abc" || view.Slug != "blue" || view.ServerID != "box_1" || !view.Ready {
		t.Fatalf("view=%#v", view)
	}
	if view.Labels["runtime"] != "python" || view.Labels["size"] != "medium" {
		t.Fatalf("labels=%v", view.Labels)
	}
	server := boxToServer(cfg, fake.box)
	if server.PublicNet.IPv4.IP != "eu-west-1.box.upstash.com" {
		t.Fatalf("host=%q", server.PublicNet.IPv4.IP)
	}
}

func TestStatusReadyStates(t *testing.T) {
	tests := map[string]bool{
		"running":   true,
		"ready":     true,
		"idle":      true,
		"paused":    true,
		" RUNNING ": true,
		"":          false,
		"pending":   false,
		"creating":  false,
		"failed":    false,
		"unknown":   false,
	}
	for status, want := range tests {
		if got := statusReady(status); got != want {
			t.Fatalf("statusReady(%q)=%t want %t", status, got, want)
		}
	}
}

func TestStatusWaitTreatsMissingStatusAsNotReady(t *testing.T) {
	fake := &fakeAPI{box: boxData{ID: "box_1", Name: "crabbox-blue-123456789abc"}}
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), Runtime{
		Stdout: io.Discard,
		Stderr: io.Discard,
		Clock:  &advancingClock{current: time.Unix(100, 0), step: time.Second},
	}).(*backend)
	_, err := backend.Status(context.Background(), StatusRequest{ID: "box_1", Wait: true, WaitTimeout: time.Nanosecond})
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for upstash-box box_1 to become ready") {
		t.Fatalf("err=%v, want timeout for missing status", err)
	}
}

func hasFeature(features core.FeatureSet, want core.Feature) bool {
	for _, feature := range features {
		if feature == want {
			return true
		}
	}
	return false
}

func testConfig() Config {
	return Config{
		Provider: providerName,
		UpstashBox: UpstashBoxConfig{
			APIKey:  "box_key",
			BaseURL: "https://us-east-1.box.upstash.com",
			Runtime: "node",
			Size:    "small",
			Workdir: "/workspace/home/crabbox",
		},
	}
}

func testRuntime() Runtime {
	return Runtime{Stdout: io.Discard, Stderr: io.Discard}
}

type advancingClock struct {
	current time.Time
	step    time.Duration
}

func (c *advancingClock) Now() time.Time {
	c.current = c.current.Add(c.step)
	return c.current
}

func withFakeAPI(t *testing.T, fake *fakeAPI) {
	t.Helper()
	original := newAPI
	newAPI = func(Config, Runtime) (api, error) { return fake, nil }
	t.Cleanup(func() { newAPI = original })
}

func withUpstashBoxCleanupTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	original := upstashBoxCleanupTimeout
	upstashBoxCleanupTimeout = timeout
	t.Cleanup(func() { upstashBoxCleanupTimeout = original })
}

type fakeAPI struct {
	uploadHook func(context.Context, string, string) error
	execHook   func(context.Context, string) (execResult, error)
	streamHook func()
	getHook    func() (boxData, error)
	streamCode int
	streamErr  error

	verbs           []string
	createReq       createRequest
	box             boxData
	execCommands    []string
	execFolders     []string
	streamCommands  []string
	streamFolders   []string
	execResults     []execResult
	deletedIDs      []string
	getBoxCalls     int
	notFoundOnGet   int
	blockDelete     bool
	blockEnvCleanup bool
}

func (f *fakeAPI) CreateBox(_ context.Context, createRequest createRequest) (boxData, error) {
	f.verbs = append(f.verbs, "create")
	f.createReq = createRequest
	f.box = boxData{ID: "box_1", CreatedAt: 1700000000, Name: createRequest.Name, Runtime: createRequest.Runtime, Size: createRequest.Size, Status: "running", KeepAlive: createRequest.KeepAlive}
	return f.box, nil
}

func (f *fakeAPI) GetBox(context.Context, string) (boxData, error) {
	f.getBoxCalls++
	if f.getHook != nil {
		return f.getHook()
	}
	if f.notFoundOnGet == f.getBoxCalls {
		return boxData{}, errors.New("box not found")
	}
	if f.box.ID == "" {
		f.box = boxData{ID: "box_1", CreatedAt: 1700000000, Name: "crabbox-blue-123456789abc", Status: "running"}
	}
	return f.box, nil
}

func (f *fakeAPI) ListBoxes(context.Context) ([]boxData, error) {
	if f.box.ID == "" {
		f.box = boxData{ID: "box_1", CreatedAt: 1700000000, Name: "crabbox-blue-123456789abc", Status: "running"}
	}
	return []boxData{f.box}, nil
}

func (f *fakeAPI) DeleteBoxes(ctx context.Context, ids []string) error {
	f.verbs = append(f.verbs, "delete")
	f.deletedIDs = append(f.deletedIDs, ids...)
	if f.blockDelete {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (f *fakeAPI) Exec(ctx context.Context, _ string, command, folder string) (execResult, error) {
	f.verbs = append(f.verbs, "exec")
	f.execCommands = append(f.execCommands, command)
	f.execFolders = append(f.execFolders, folder)
	if f.execHook != nil {
		return f.execHook(ctx, command)
	}
	if f.blockEnvCleanup && strings.Contains(command, ".crabbox-env-") {
		<-ctx.Done()
		return execResult{}, ctx.Err()
	}
	if len(f.execResults) == 0 {
		return execResult{ExitCode: 0}, nil
	}
	result := f.execResults[0]
	f.execResults = f.execResults[1:]
	return result, nil
}

func (f *fakeAPI) ExecStream(_ context.Context, _ string, command, folder string, stdout io.Writer) (int, error) {
	f.verbs = append(f.verbs, "stream")
	f.streamCommands = append(f.streamCommands, command)
	f.streamFolders = append(f.streamFolders, folder)
	if f.streamHook != nil {
		f.streamHook()
	}
	_, _ = io.WriteString(stdout, "ok\n")
	return f.streamCode, f.streamErr
}

func (f *fakeAPI) UploadFile(ctx context.Context, _ string, localPath, remotePath string) error {
	f.verbs = append(f.verbs, "upload")
	if f.uploadHook != nil {
		return f.uploadHook(ctx, localPath, remotePath)
	}
	return nil
}

func readMultipart(t *testing.T, reader *multipart.Reader) map[string]string {
	t.Helper()
	fields := map[string]string{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return fields
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
		if part.FormName() == "files" {
			fields[part.FormName()] = string(data)
			continue
		}
		fields[part.FormName()] = string(data)
	}
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "alice@example.com")
	runGit(t, root, "config", "user.name", "Alice")
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "hello.txt")
	runGit(t, root, "commit", "-m", "initial")
	return root
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func TestRunPartialEnvUploadRetainsCleanup(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			root := t.TempDir()
			remote := filepath.Join(root, "partial-profile")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			primary := errors.New("synthetic upload failure")
			var localFile, remotePath string
			cleanups := 0
			write := func(_ context.Context, path, content string) error {
				remotePath = path
				if err := os.WriteFile(remote, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				if canceled {
					cancel()
					return context.Canceled
				}
				return primary
			}
			fake := &fakeAPI{}
			fake.uploadHook = func(ctx context.Context, local, path string) error {
				localFile = local
				data, err := os.ReadFile(local)
				if err != nil {
					return err
				}
				return write(ctx, path, string(data))
			}
			fake.execHook = func(cleanupCtx context.Context, command string) (execResult, error) {
				if !strings.HasPrefix(command, "rm -f ") {
					return execResult{}, nil
				}
				cleanups++
				if cleanupCtx.Err() != nil {
					t.Fatalf("cleanup canceled: %v", cleanupCtx.Err())
				}
				if _, ok := cleanupCtx.Deadline(); !ok {
					t.Fatal("unbounded cleanup")
				}
				if command != "rm -f "+shellQuote(remotePath) {
					t.Fatalf("wrong removal: %s", command)
				}
				return execResult{}, os.Remove(remote)
			}
			withFakeAPI(t, fake)
			b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
			_, err := b.Run(ctx, RunRequest{Repo: Repo{Root: root, Name: "fixture"}, NoSync: true, Keep: true, Command: []string{"true"}, Env: map[string]string{"FIXTURE": "synthetic"}})
			want := primary
			if canceled {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("primary lost: %v", err)
			}
			if cleanups != 1 || len(fake.streamCommands) != 0 || len(fake.deletedIDs) != 0 {
				t.Fatalf("cleanups=%d workload=%v deletes=%v", cleanups, fake.streamCommands, fake.deletedIDs)
			}
			if _, err := os.Stat(remote); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("remote profile remains: %v", err)
			}
			if localFile != "" {
				if _, err := os.Stat(localFile); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("local profile remains: %v", err)
				}
			}
		})
	}
}

func TestRunEnvDiscoveryDoesNotRequireOrCreateClaim(t *testing.T) {
	for _, id := range []string{"box_1", "blue", "cbx_123456789abc"} {
		t.Run(id, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			fake := &fakeAPI{}
			withFakeAPI(t, fake)
			b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
			result, err := b.Run(t.Context(), RunRequest{ID: id, Repo: Repo{Root: t.TempDir()}, NoSync: true, Command: []string{"true"}, Env: map[string]string{"FIXTURE": "synthetic"}})
			if err != nil || result.ExitCode != 0 || len(fake.streamCommands) != 1 || len(fake.deletedIDs) != 0 {
				t.Fatalf("result=%#v err=%v verbs=%v", result, err, fake.verbs)
			}
			if !reflect.DeepEqual(fake.verbs, []string{"exec", "upload", "stream", "exec"}) {
				t.Fatalf("file lifecycle=%v", fake.verbs)
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence("cbx_123456789abc"); err != nil || exists {
				t.Fatalf("discovery published claim: exists=%v err=%v", exists, err)
			}
		})
	}
}

func TestEnvProfileRefusesChangedReceipt(t *testing.T) {
	for _, change := range []string{"box ID", "box name", "box creation", "lookup failure", "claim appeared", "claim changed", "claim removed", "unchanged legacy claim"} {
		t.Run(change, func(t *testing.T) {
			state := t.TempDir()
			t.Setenv("XDG_STATE_HOME", state)
			const leaseID = "cbx_123456789abc"
			claimPath := filepath.Join(state, "crabbox", "claims", leaseID+".json")
			writeClaim := func(slug string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(claimPath), 0o700); err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(core.LeaseClaim{LeaseID: leaseID, Slug: slug, Provider: providerName})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(claimPath, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if change == "claim changed" || change == "claim removed" || change == "unchanged legacy claim" {
				writeClaim("blue")
			}
			fake := &fakeAPI{}
			var local string
			fake.uploadHook = func(_ context.Context, localPath, _ string) error { local = localPath; return nil }
			_, cleanup, err := uploadEnvProfile(t.Context(), fake, leaseID, "box_1", "blue", map[string]string{"FIXTURE": "synthetic"})
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "box ID":
				fake.box.ID = "box_replacement"
			case "box name":
				fake.box.Name = "crabbox-other-123456789abc"
			case "box creation":
				fake.box.CreatedAt++
			case "lookup failure":
				fake.notFoundOnGet = fake.getBoxCalls + 1
			case "claim appeared", "claim changed":
				writeClaim("replacement")
			case "claim removed":
				if err := os.Remove(claimPath); err != nil {
					t.Fatal(err)
				}
			}
			before, beforeExists, err := core.ReadLeaseClaimWithPresence(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			cleanupCtx, cancel := upstashBoxCleanupContext()
			defer cancel()
			err = cleanup(cleanupCtx)
			if change == "unchanged legacy claim" {
				if err != nil || len(fake.execCommands) != 1 {
					t.Fatalf("legacy cleanup=%v commands=%v", err, fake.execCommands)
				}
			} else if err == nil || len(fake.execCommands) != 0 {
				t.Fatalf("stale cleanup=%v commands=%v", err, fake.execCommands)
			}
			if _, err := os.Stat(local); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("local file remains: %v", err)
			}
			after, afterExists, err := core.ReadLeaseClaimWithPresence(leaseID)
			if err != nil || afterExists != beforeExists || !reflect.DeepEqual(after, before) {
				t.Fatalf("cleanup mutated claim: %v", err)
			}
			if len(fake.deletedIDs) != 0 {
				t.Fatalf("file receipt deleted Box: %v", fake.deletedIDs)
			}
		})
	}
}

func TestEnvProfileDeniedUploadHasNoRemoteCustody(t *testing.T) {
	for _, phase := range []string{"missing creation", "changed before upload", "canceled before upload"} {
		t.Run(phase, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			tmp := t.TempDir()
			for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
				t.Setenv(name, tmp)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fake := &fakeAPI{}
			box := boxData{ID: "box_1", Name: "crabbox-blue-123456789abc", CreatedAt: 1700000000}
			fake.getHook = func() (boxData, error) {
				if phase == "missing creation" {
					box.CreatedAt = 0
				}
				if fake.getBoxCalls == 2 && phase == "changed before upload" {
					box.CreatedAt++
				}
				if fake.getBoxCalls == 1 && phase == "canceled before upload" {
					cancel()
				}
				return box, nil
			}
			_, cleanup, err := uploadEnvProfile(ctx, fake, "cbx_123456789abc", "box_1", "blue", map[string]string{"FIXTURE": "synthetic"})
			if err == nil {
				t.Fatal("denied upload succeeded")
			}
			if cleanup != nil {
				cleanupCtx, cancel := upstashBoxCleanupContext()
				defer cancel()
				if err := cleanup(cleanupCtx); err != nil {
					t.Fatal(err)
				}
			}
			if len(fake.verbs) != 0 {
				t.Fatalf("remote custody after denial: %v", fake.verbs)
			}
			entries, err := os.ReadDir(tmp)
			if err != nil || len(entries) != 0 {
				t.Fatalf("local residue=%v err=%v", entries, err)
			}
		})
	}
}

func TestRunPreservesStreamErrorCause(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errors.New("synthetic transport failure")} {
		for _, cleanupFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cleanup=%t", cause, cleanupFails), func(t *testing.T) {
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				fake := &fakeAPI{streamErr: fmt.Errorf("stream: %w", cause)}
				fake.execHook = func(_ context.Context, command string) (execResult, error) {
					if cleanupFails && strings.HasPrefix(command, "rm -f ") {
						return execResult{}, context.Canceled
					}
					return execResult{}, nil
				}
				withFakeAPI(t, fake)
				b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
				result, err := b.Run(t.Context(), RunRequest{Repo: Repo{Root: t.TempDir()}, NoSync: true, Keep: true, Command: []string{"true"}, Env: map[string]string{"FIXTURE": "synthetic"}})
				var exitErr ExitError
				if !errors.Is(err, cause) || !errors.As(err, &exitErr) || exitErr.Code != 1 || len(fake.streamCommands) != 1 || t.Context().Err() != nil {
					t.Fatalf("stream cause lost: err=%v stream calls=%d parent=%v", err, len(fake.streamCommands), t.Context().Err())
				}
				if got, want := core.FinalizeRunResult(result, err).Status, core.RunStatusForResult(result, cause); got != want {
					t.Fatalf("primary status=%s, want %s", got, want)
				}
			})
		}
	}
}

func TestRunSkipsStreamAfterCanceledSuccessfulEnvUpload(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fake := &fakeAPI{uploadHook: func(context.Context, string, string) error { cancel(); return nil }}
	cleanups := 0
	fake.execHook = func(cleanupCtx context.Context, command string) (execResult, error) {
		if strings.HasPrefix(command, "rm -f ") {
			cleanups++
			if cleanupCtx.Err() != nil {
				t.Error("profile cleanup inherited cancellation")
			}
		}
		return execResult{}, nil
	}
	withFakeAPI(t, fake)
	b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	_, err := b.Run(ctx, RunRequest{Repo: Repo{Root: t.TempDir()}, NoSync: true, Keep: true, Command: []string{"true"}, Env: map[string]string{"FIXTURE": "synthetic"}})
	if !errors.Is(err, context.Canceled) || len(fake.streamCommands) != 0 || cleanups != 1 || len(fake.deletedIDs) != 0 {
		t.Fatalf("canceled successful upload: err=%v streams=%v cleanups=%d deleted=%v", err, fake.streamCommands, cleanups, fake.deletedIDs)
	}
}

func TestRunEnvCleanupPreservesPrimaryOutcome(t *testing.T) {
	for _, outcome := range []string{"command exit", "transport error", "upload cancellation"} {
		t.Run(outcome, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			fake := &fakeAPI{}
			fake.execHook = func(_ context.Context, command string) (execResult, error) {
				if strings.HasPrefix(command, "rm -f ") {
					return execResult{ExitCode: 9, Error: "synthetic cleanup failure"}, nil
				}
				return execResult{}, nil
			}
			wantCode := 42
			switch outcome {
			case "command exit":
				fake.streamCode = 42
			case "transport error":
				fake.streamErr = errors.New("synthetic transport failure")
				wantCode = 1
			case "upload cancellation":
				fake.uploadHook = func(context.Context, string, string) error { return context.Canceled }
			}
			withFakeAPI(t, fake)
			b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
			result, err := b.Run(t.Context(), RunRequest{Repo: Repo{Root: t.TempDir(), Name: "fixture"}, NoSync: true, Keep: true, Command: []string{"true"}, Env: map[string]string{"FIXTURE": "synthetic"}})
			if err == nil || !strings.Contains(err.Error(), "synthetic cleanup failure") {
				t.Fatalf("missing cleanup diagnosis: %v", err)
			}
			if outcome == "upload cancellation" {
				if !errors.Is(err, context.Canceled) || len(fake.streamCommands) != 0 {
					t.Fatalf("primary cancellation lost: %v", err)
				}
			} else {
				var exitErr ExitError
				if !errors.As(err, &exitErr) || exitErr.Code != wantCode || result.ExitCode != wantCode {
					t.Fatalf("primary outcome lost: result=%#v err=%v", result, err)
				}
			}
		})
	}
}

func TestEnvFileReceiptHoldsAbsentClaimFenceDuringMutation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_123456789abc"
	receipt, err := captureEnvFileReceipt(t.Context(), &fakeAPI{}, leaseID, "box_1", "blue")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() {
		done <- receipt.withUnchanged(ctx, func() error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("file operation did not enter: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	writerCtx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err = core.WithDurableLeaseClaimLockContext(writerCtx, leaseID, func(_ *core.LeaseClaim, _ bool, _ func() error) error {
		return errors.New("claim writer entered during file mutation")
	})
	unblock()
	if operationErr := <-done; operationErr != nil {
		t.Fatal(operationErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("absent-claim fence released early: %v", err)
	}
}

func TestEnvProfilesOnSameBoxHaveIndependentCustody(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	fake := &fakeAPI{}
	fake.uploadHook = func(_ context.Context, localPath, remotePath string) error {
		data, err := os.ReadFile(localPath)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(root, filepath.Base(remotePath)), data, 0o600)
	}
	var paths []string
	var cleanups []func(context.Context) error
	for range 2 {
		path, cleanup, err := uploadEnvProfile(t.Context(), fake, "cbx_123456789abc", "box_1", "blue", map[string]string{"FIXTURE": "synthetic"})
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
		cleanups = append(cleanups, cleanup)
	}
	if paths[0] == paths[1] {
		t.Fatal("profiles share a remote filename")
	}
	fake.execHook = func(_ context.Context, command string) (execResult, error) {
		for _, path := range paths {
			if command == "rm -f "+shellQuote(path) {
				return execResult{}, os.Remove(filepath.Join(root, filepath.Base(path)))
			}
		}
		return execResult{}, errors.New("unexpected cleanup target")
	}
	if err := cleanups[0](t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.Base(paths[1]))); err != nil {
		t.Fatalf("first cleanup affected second profile: %v", err)
	}
	if err := cleanups[0](t.Context()); err != nil {
		t.Fatalf("Close not idempotent: %v", err)
	}
	if err := cleanups[1](t.Context()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 || len(fake.execCommands) != 2 {
		t.Fatalf("custody residue=%v commands=%v err=%v", entries, fake.execCommands, err)
	}
}

func TestRunEnvProfileThroughNativeHTTPTransport(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("native POSIX transport")
	}
	for _, scenario := range []string{"healthy", "lost upload acknowledgement", "canceled upload", "changed generation", "source failure", "empty source", "owned success", "owned command exit", "owned workspace failure", "owned upload failure", "owned profile cleanup failure", "owned deletion failure", "owned timing failure"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			local := t.TempDir()
			t.Setenv("TMPDIR", local)
			remote := t.TempDir()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var mu sync.Mutex
			box := boxData{ID: "box_fixture", Name: "crabbox-blue-123456789abc", CreatedAt: 1700000000, Status: "running"}
			var profilePath string
			owned := strings.HasPrefix(scenario, "owned ")
			uploads, removals, workloads, deletes, creates := 0, 0, 0, 0, 0
			mapPath := func(value string) string { return strings.ReplaceAll(value, workspaceRoot, remote) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Header.Get("X-Box-Api-Key") != "synthetic-fixture" {
					t.Error("missing synthetic authentication")
					http.Error(w, "auth", 401)
					return
				}
				switch r.URL.Path {
				case "/v2/box/box_fixture":
					_ = json.NewEncoder(w).Encode(box)
				case "/v2/box":
					if owned && r.Method == http.MethodPost {
						var request struct{ Name string }
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Error(err)
							return
						}
						creates++
						box.Name = request.Name
						_ = json.NewEncoder(w).Encode(box)
						return
					}
					deletes++
					if owned && r.Method == http.MethodDelete {
						var request struct {
							IDs []string `json:"ids"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.IDs) != 1 || request.IDs[0] != box.ID {
							t.Errorf("unexpected deletion target: ids=%v err=%v", request.IDs, err)
							http.Error(w, "target", 400)
							return
						}
						if scenario == "owned deletion failure" {
							http.Error(w, "synthetic deletion failure", 500)
						} else {
							w.WriteHeader(http.StatusNoContent)
						}
						return
					}
					http.Error(w, "unexpected box mutation", 400)
				case "/v2/box/box_fixture/files/upload", "/v2/box/box_fixture/files/write":
					var content string
					if strings.HasSuffix(r.URL.Path, "/upload") {
						reader, err := r.MultipartReader()
						if err != nil {
							t.Error(err)
							http.Error(w, "multipart", 400)
							return
						}
						fields := readMultipart(t, reader)
						profilePath, content = fields["paths"], fields["files"]
					} else {
						// The baseline uses JSON; keep the server fixture identical for red/green proof.
						var body struct{ Path, Content string }
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
							return
						}
						profilePath, content = body.Path, body.Content
					}
					if !strings.HasPrefix(profilePath, workspaceRoot+"/.crabbox-env-") {
						t.Error("profile outside workspace")
						http.Error(w, "path", 400)
						return
					}
					if scenario == "source failure" {
						content = "return 9\n"
					}
					if err := os.WriteFile(mapPath(profilePath), []byte(content), 0o600); err != nil {
						t.Error(err)
						return
					}
					uploads++
					switch scenario {
					case "lost upload acknowledgement", "owned upload failure":
						http.Error(w, "synthetic upload acknowledgement failure", 500)
						return
					case "canceled upload":
						cancel()
						return
					case "changed generation":
						box.CreatedAt++
					}
					w.WriteHeader(http.StatusNoContent)
				case "/v2/box/box_fixture/exec", "/v2/box/box_fixture/exec-stream":
					var body struct {
						Command []string
						Folder  string
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					if len(body.Command) != 3 || body.Command[0] != "sh" || body.Command[1] != "-c" {
						t.Error("unexpected native shell envelope")
						http.Error(w, "command", 400)
						return
					}
					stream := strings.HasSuffix(r.URL.Path, "exec-stream")
					if scenario == "owned workspace failure" && strings.HasPrefix(body.Command[2], "mkdir -p ") {
						http.Error(w, "synthetic workspace failure", 500)
						return
					}
					if stream {
						workloads++
					}
					if strings.HasPrefix(body.Command[2], "rm -f ") {
						if body.Command[2] != "rm -f "+shellQuote(profilePath) {
							t.Error("removal targeted different file")
							http.Error(w, "target", 400)
							return
						}
						removals++
						if scenario == "owned profile cleanup failure" {
							_ = json.NewEncoder(w).Encode(execResult{ExitCode: 9, Error: "synthetic profile cleanup failure"})
							return
						}
					}
					cmd := exec.CommandContext(r.Context(), body.Command[0], body.Command[1], mapPath(body.Command[2]))
					cmd.Dir = filepath.Join(remote, body.Folder)
					cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + remote, "ENV=" + os.DevNull}
					output, runErr := cmd.CombinedOutput()
					code := 0
					if runErr != nil {
						var exitErr *exec.ExitError
						if !errors.As(runErr, &exitErr) {
							t.Error(runErr)
							http.Error(w, "native exec", 500)
							return
						}
						code = exitErr.ExitCode()
					}
					if stream {
						_, _ = w.Write(output)
						_, _ = fmt.Fprintf(w, "\nevent: exit\ndata: {\"exit_code\":%d}\n\n", code)
					} else {
						_ = json.NewEncoder(w).Encode(execResult{ExitCode: code, Output: string(output)})
					}
				default:
					t.Errorf("unexpected fixture route: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			cfg := testConfig()
			cfg.UpstashBox.BaseURL, cfg.UpstashBox.APIKey = server.URL, "synthetic-fixture"
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			var diagnostics io.Writer = &stderr
			if scenario == "owned timing failure" {
				diagnostics = failingTimingWriter{errors.New("synthetic timing write failure")}
			}
			b := NewBackend(Provider{}.Spec(), cfg, Runtime{HTTP: server.Client(), Stdout: &stdout, Stderr: diagnostics}).(*backend)
			command := `printf '%s\n' "$FIXTURE"`
			if scenario == "source failure" {
				command = "printf first; printf escaped"
			}
			if scenario == "empty source" {
				command = ""
			}
			if scenario == "owned command exit" || scenario == "owned timing failure" {
				command = "exit 42"
			}
			req := RunRequest{ID: "box_fixture", Repo: Repo{Root: t.TempDir(), Name: "fixture"}, NoSync: true, TimingJSON: true, Command: []string{command}, ShellMode: true, Env: map[string]string{"FIXTURE": "quote ' newline\n$literal"}}
			if owned {
				req.ID, req.KeepOnFailure = "", true
			}
			result, err := b.Run(ctx, req)
			result = core.FinalizeRunResult(result, err)
			mu.Lock()
			defer mu.Unlock()
			wantWorkloads, wantRemovals, wantUploads, wantDeletes := 1, 1, 1, 0
			switch scenario {
			case "lost upload acknowledgement", "owned upload failure":
				wantWorkloads = 0
				if err == nil || !strings.Contains(err.Error(), "synthetic upload acknowledgement failure") {
					t.Fatalf("lost primary upload error: %v", err)
				}
			case "canceled upload":
				wantWorkloads = 0
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
			case "changed generation":
				wantRemovals = 0
				if err == nil || result.ExitCode != 5 {
					t.Fatalf("stale cleanup not refused: result=%#v err=%v", result, err)
				}
			case "source failure":
				if err == nil || result.ExitCode != 9 || strings.Contains(stdout.String(), "escaped") {
					t.Fatalf("source failure escaped: code=%d err=%v out=%q", result.ExitCode, err, stdout.String())
				}
			case "owned workspace failure":
				wantWorkloads, wantRemovals, wantUploads = 0, 0, 0
				if err == nil || !strings.Contains(err.Error(), "synthetic workspace failure") {
					t.Fatalf("workspace error lost: %v", err)
				}
			case "owned command exit", "owned timing failure":
				var exitErr ExitError
				if !errors.As(err, &exitErr) || exitErr.Code != 42 || result.ExitCode != 42 || result.ErrorKind != core.RunErrorCommandExit {
					t.Fatalf("native command outcome lost: result=%#v err=%v", result, err)
				}
				if scenario == "owned timing failure" && !strings.Contains(err.Error(), "synthetic timing write failure") {
					t.Fatalf("timing diagnostic lost: %v", err)
				}
			case "owned profile cleanup failure":
				if err == nil || result.ExitCode != 5 || result.ErrorKind != core.RunErrorProvider {
					t.Fatalf("mandatory cleanup lost: result=%#v err=%v", result, err)
				}
			case "owned deletion failure":
				wantDeletes = 1
				if err == nil || result.ExitCode != 1 || result.ErrorKind != core.RunErrorProvider {
					t.Fatalf("deletion outcome lost: result=%#v err=%v", result, err)
				}
			default:
				if err != nil || result.ExitCode != 0 {
					t.Fatalf("result=%#v err=%v", result, err)
				}
				if scenario == "healthy" && !strings.Contains(stdout.String(), "quote ' newline\n$literal") {
					t.Fatalf("profile value changed: %q", stdout.String())
				}
			}
			if scenario == "owned success" {
				wantDeletes = 1
			}
			if uploads != wantUploads || removals != wantRemovals || workloads != wantWorkloads || deletes != wantDeletes {
				t.Fatalf("uploads=%d removals=%d workloads=%d deletes=%d", uploads, removals, workloads, deletes)
			}
			_, statErr := os.Stat(mapPath(profilePath))
			if wantRemovals == 1 && scenario != "owned profile cleanup failure" && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("remote residue: %v", statErr)
			}
			if (wantUploads > 0 && wantRemovals == 0 || scenario == "owned profile cleanup failure") && statErr != nil {
				t.Fatalf("refused cleanup changed remote file: %v", statErr)
			}
			wantKept := scenario != "owned success"
			if result.Session == nil || result.Session.Kept != wantKept || result.Session.Reused == owned || (creates == 1) != owned {
				t.Fatalf("session=%#v creates=%d owned=%v", result.Session, creates, owned)
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence(result.LeaseID); err != nil || exists != (owned && wantKept) {
				t.Fatalf("claim custody changed: exists=%v owned=%v kept=%v err=%v", exists, owned, wantKept, err)
			}
			if scenario != "owned timing failure" {
				var report core.TimingReport
				reports := 0
				for _, line := range strings.Split(stderr.String(), "\n") {
					if strings.HasPrefix(line, "{") {
						if err := json.Unmarshal([]byte(line), &report); err != nil {
							t.Fatal(err)
						}
						reports++
					}
				}
				if reports != 1 || report.ExitCode != result.ExitCode || report.RunStatus != result.Status || report.ErrorKind != result.ErrorKind {
					t.Fatalf("native reports=%d timing=%#v result=%#v", reports, report, result)
				}
			}
			entries, err := os.ReadDir(local)
			if err != nil || len(entries) != 0 {
				t.Fatalf("local residue=%v err=%v", entries, err)
			}
			t.Logf("uploads=%d removals=%d workloads=%d deletes=%d exit=%d", uploads, removals, workloads, deletes, result.ExitCode)
		})
	}
}

func TestRunSourceIntentSurvivesNativeShell(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("POSIX source transport")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	for _, tc := range []struct {
		name    string
		command []string
		literal map[int]bool
		want    string
		code    int
	}{
		{"literal separator", []string{"printf", "<%s>", ";", "touch", "sentinel"}, map[int]bool{2: true}, "<;><touch><sentinel>", 0},
		{"literal executable", []string{"FOO=x", "argument"}, map[int]bool{0: true}, "literal:argument", 42},
		{"mixed intent", []string{"printf", "%s", ";", "&&", "printf", "%s", "done"}, map[int]bool{2: true}, ";done", 0},
		{"inferred singleton", []string{"printf 'source value'"}, nil, "source value", 0},
		{"ordinary argv", []string{"printf", "%s", "value"}, nil, "value", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			root := t.TempDir()
			program := filepath.Join(root, "FOO=x")
			if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf 'literal:%s' \"$*\"\nexit 42\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			fake := &fakeAPI{}
			withFakeAPI(t, fake)
			b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
			_, err := b.Run(t.Context(), RunRequest{Repo: Repo{Root: root, Name: "fixture"}, NoSync: true, Keep: true, Command: tc.command, CommandLiteralArgs: tc.literal})
			if err != nil {
				t.Fatal(err)
			}
			if len(fake.streamCommands) != 1 {
				t.Fatalf("stream commands=%v", fake.streamCommands)
			}
			cmd := exec.Command(sh, "-c", fake.streamCommands[0])
			cmd.Dir = root
			cmd.Env = []string{"HOME=" + root, "PATH=" + root + ":/usr/bin:/bin", "ENV=" + os.DevNull}
			out, runErr := cmd.CombinedOutput()
			code := 0
			if runErr != nil {
				var ee *exec.ExitError
				if !errors.As(runErr, &ee) {
					t.Fatal(runErr)
				}
				code = ee.ExitCode()
			}
			if string(out) != tc.want || code != tc.code {
				t.Fatalf("source=%q output=%q code=%d want=%q/%d", fake.streamCommands[0], out, code, tc.want, tc.code)
			}
			if _, err := os.Stat(filepath.Join(root, "sentinel")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("literal created sentinel: %v", err)
			}
		})
	}
}
