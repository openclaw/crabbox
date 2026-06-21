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
}

func TestClientUsesUpstashBoxRESTShape(t *testing.T) {
	var createBody map[string]any
	var deleteBody map[string]any
	var writeBody map[string]any
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
		case "/v2/box/box_1/files/write":
			if err := json.NewDecoder(r.Body).Decode(&writeBody); err != nil {
				t.Fatal(err)
			}
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
	if err := client.WriteFile(context.Background(), "box_1", "/tmp/env.sh", "export A=1\n"); err != nil {
		t.Fatal(err)
	}
	if writeBody["path"] != "/tmp/env.sh" || writeBody["content"] != "export A=1\n" {
		t.Fatalf("write body=%v", writeBody)
	}
	if err := client.DeleteBoxes(context.Background(), []string{"box_1"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deleteBody["ids"], []any{"box_1"}) {
		t.Fatalf("delete body=%v", deleteBody)
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
	command, err := buildCommand([]string{"go", "test", "./..."}, false)
	if err != nil {
		t.Fatal(err)
	}
	if command != "exec 'go' 'test' './...'" {
		t.Fatalf("command=%q", command)
	}
	env := shellEnvProfile(map[string]string{"B": "two", "A": "one two", "BAD; id >&2 #": "boom"})
	if env != "set -a\nA='one two'\nB='two'\nset +a\n" {
		t.Fatalf("env profile=%q", env)
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
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("result=%#v", result)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Run took %s, want bounded cleanup", elapsed)
	}
	if !strings.Contains(stderr.String(), "warning: upstash-box delete failed for box_1: context deadline exceeded") {
		t.Fatalf("stderr=%q, want bounded cleanup warning", stderr.String())
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
		Repo:    Repo{Name: "repo", Root: t.TempDir()},
		Command: []string{"echo", "hello"},
		Env:     map[string]string{"TOKEN": "secret"},
		NoSync:  true,
		Keep:    true,
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
	if !reflect.DeepEqual(fake.verbs, []string{"exec", "upload", "exec", "exec"}) {
		t.Fatalf("verbs=%v", fake.verbs)
	}
	if !strings.Contains(fake.execCommands[2], "rm -f '.crabbox-upstash-box-sync-") {
		t.Fatalf("cleanup command=%q", fake.execCommands[2])
	}
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
	if !strings.Contains(stderr.String(), "warning: upstash-box sync cleanup failed for box_1") || !strings.Contains(stderr.String(), "cleanup denied") {
		t.Fatalf("stderr=%q, want cleanup warning", stderr.String())
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
	verbs           []string
	createReq       createRequest
	box             boxData
	execCommands    []string
	execFolders     []string
	streamCommands  []string
	streamFolders   []string
	execResults     []execResult
	deletedIDs      []string
	blockDelete     bool
	blockEnvCleanup bool
}

func (f *fakeAPI) CreateBox(_ context.Context, createRequest createRequest) (boxData, error) {
	f.verbs = append(f.verbs, "create")
	f.createReq = createRequest
	f.box = boxData{ID: "box_1", Name: createRequest.Name, Runtime: createRequest.Runtime, Size: createRequest.Size, Status: "running", KeepAlive: createRequest.KeepAlive}
	return f.box, nil
}

func (f *fakeAPI) GetBox(context.Context, string) (boxData, error) {
	if f.box.ID == "" {
		f.box = boxData{ID: "box_1", Name: "crabbox-blue-123456789abc", Status: "running"}
	}
	return f.box, nil
}

func (f *fakeAPI) ListBoxes(context.Context) ([]boxData, error) {
	if f.box.ID == "" {
		f.box = boxData{ID: "box_1", Name: "crabbox-blue-123456789abc", Status: "running"}
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
	_, _ = io.WriteString(stdout, "ok\n")
	return 0, nil
}

func (f *fakeAPI) UploadFile(context.Context, string, string, string) error {
	f.verbs = append(f.verbs, "upload")
	return nil
}

func (f *fakeAPI) WriteFile(context.Context, string, string, string) error {
	f.verbs = append(f.verbs, "write")
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
