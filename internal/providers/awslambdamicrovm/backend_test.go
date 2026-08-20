package awslambdamicrovm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const testImageARN = "arn:aws:lambda:eu-west-1:123456789012:microvm-image:crabbox-runner"

type fakeControlPlane struct {
	vm                  microVM
	calls               []string
	runRequest          runMicroVMRequest
	runErr              error
	terminateErr        error
	terminateDeadline   bool
	terminateContextErr error
}

func (f *fakeControlPlane) Run(_ context.Context, req runMicroVMRequest) (microVM, error) {
	f.calls = append(f.calls, "run")
	f.runRequest = req
	if f.runErr != nil {
		return microVM{}, f.runErr
	}
	f.vm = microVM{ID: "mvm-test", Endpoint: "mvm-test.lambda-microvm.eu-west-1.on.aws", ImageARN: req.ImageARN, ImageVersion: "1", State: "RUNNING", StartedAt: time.Now()}
	return f.vm, nil
}

func (f *fakeControlPlane) Get(_ context.Context, id string) (microVM, error) {
	f.calls = append(f.calls, "get:"+id)
	if f.vm.ID == "" || f.vm.State == "TERMINATED" {
		return microVM{}, errors.New("resource not found")
	}
	return f.vm, nil
}

func (f *fakeControlPlane) Probe(context.Context, string, string) error {
	f.calls = append(f.calls, "probe")
	return nil
}

func (f *fakeControlPlane) Terminate(ctx context.Context, id string) error {
	f.calls = append(f.calls, "terminate:"+id)
	_, f.terminateDeadline = ctx.Deadline()
	f.terminateContextErr = ctx.Err()
	if f.terminateErr != nil {
		return f.terminateErr
	}
	f.vm.State = "TERMINATED"
	return nil
}

func (f *fakeControlPlane) Suspend(_ context.Context, id string) error {
	f.calls = append(f.calls, "suspend:"+id)
	f.vm.State = "SUSPENDED"
	return nil
}

func (f *fakeControlPlane) Resume(_ context.Context, id string) error {
	f.calls = append(f.calls, "resume:"+id)
	f.vm.State = "RUNNING"
	return nil
}

func (f *fakeControlPlane) AuthToken(context.Context, string) (string, error) { return "token", nil }

type fakeRunner struct {
	healthErr   error
	healthCheck func(context.Context, microVM) error
	exitCode    int
	execErr     error
	commands    []string
	uploads     []string
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func (f *fakeRunner) Health(ctx context.Context, vm microVM) error {
	if f.healthCheck != nil {
		return f.healthCheck(ctx, vm)
	}
	return f.healthErr
}

func (f *fakeRunner) Upload(_ context.Context, _ microVM, path string, body io.Reader) error {
	_, _ = io.Copy(io.Discard, body)
	f.uploads = append(f.uploads, path)
	return nil
}

func (f *fakeRunner) Exec(_ context.Context, _ microVM, command, _ string, _ map[string]string, stdout, _ io.Writer) (int, error) {
	f.commands = append(f.commands, command)
	if strings.Contains(command, "runner-ok") {
		_, _ = io.WriteString(stdout, "runner-ok")
	}
	return f.exitCode, f.execErr
}

func TestRunSyncsExecutesAndTerminatesOneShot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := testRepo(t)
	control := &fakeControlPlane{}
	runner := &fakeRunner{}
	var stdout bytes.Buffer
	b := testBackend(control, runner, &stdout)
	result, err := b.Run(context.Background(), RunRequest{Repo: Repo{Root: repo, Name: "my-app"}, Command: []string{"printf", "runner-ok"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != providerName || result.ExitCode != 0 || !result.SyncDelegated || stdout.String() != "runner-ok" {
		t.Fatalf("result=%#v stdout=%q", result, stdout.String())
	}
	if len(runner.uploads) != 1 || !slices.ContainsFunc(runner.commands, func(command string) bool { return strings.Contains(command, "tar -xzf") }) {
		t.Fatalf("uploads=%v commands=%v", runner.uploads, runner.commands)
	}
	if !slices.Contains(control.calls, "terminate:mvm-test") || result.Session == nil || result.Session.Kept {
		t.Fatalf("calls=%v session=%#v", control.calls, result.Session)
	}
	if got := control.runRequest; got.ImageARN != testImageARN || got.MaximumSeconds != 28800 || len(got.IngressConnectors) != 1 || len(got.EgressConnectors) != 1 {
		t.Fatalf("run request=%#v", got)
	}
}

func TestRunRejectsOversizedWorkspaceBeforeProviderCalls(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	control := &fakeControlPlane{}
	runner := &fakeRunner{}
	b := testBackend(control, runner, io.Discard)
	b.cfg.Sync.FailBytes = 1

	_, err := b.Run(context.Background(), RunRequest{
		Repo: Repo{Root: testRepo(t), Name: "my-app"}, Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "sync candidate too large:") || !strings.Contains(err.Error(), "limit 1 B") {
		t.Fatalf("err=%v", err)
	}
	if len(control.calls) != 0 || len(runner.uploads) != 0 || len(runner.commands) != 0 {
		t.Fatalf("provider calls=%v uploads=%v commands=%v", control.calls, runner.uploads, runner.commands)
	}
}

func TestRunCleansPreparedArchiveWhenResourceCreationFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := testRepo(t)
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	control := &fakeControlPlane{runErr: errors.New("creation denied")}
	b := testBackend(control, &fakeRunner{}, io.Discard)

	_, err := b.Run(context.Background(), RunRequest{
		Repo: Repo{Root: repo, Name: "my-app"}, Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "creation denied") {
		t.Fatalf("err=%v", err)
	}
	if !slices.Equal(control.calls, []string{"run"}) {
		t.Fatalf("provider calls=%v", control.calls)
	}
	archives, globErr := filepath.Glob(filepath.Join(tempDir, "crabbox-aws-lambda-microvm-sync-*.tgz"))
	if globErr != nil || len(archives) != 0 {
		t.Fatalf("leaked prepared archives=%v err=%v", archives, globErr)
	}
}

func TestRetainedLifecyclePauseResumeStop(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	control := &fakeControlPlane{}
	runner := &fakeRunner{}
	b := testBackend(control, runner, io.Discard)
	result, err := b.Run(context.Background(), RunRequest{Repo: Repo{Root: testRepo(t), Name: "my-app"}, Command: []string{"true"}, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil || !result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	if err := b.Pause(context.Background(), PauseRequest{ID: result.LeaseID}); err != nil {
		t.Fatal(err)
	}
	if err := b.Resume(context.Background(), ResumeRequest{ID: result.LeaseID}); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(context.Background(), StopRequest{ID: result.LeaseID}); err != nil {
		t.Fatal(err)
	}
	want := []string{"suspend:mvm-test", "resume:mvm-test", "terminate:mvm-test"}
	for _, call := range want {
		if !slices.Contains(control.calls, call) {
			t.Fatalf("missing %q in %v", call, control.calls)
		}
	}
	if _, ok, err := resolveLeaseClaim(result.LeaseID); err != nil || ok {
		t.Fatalf("claim remains ok=%t err=%v", ok, err)
	}
}

func TestImageRotationKeepsOldLeaseManageableButBlocksReuse(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	control := &fakeControlPlane{}
	runner := &fakeRunner{}
	b := testBackend(control, runner, io.Discard)
	result, err := b.Run(context.Background(), RunRequest{Repo: Repo{Root: testRepo(t), Name: "my-app"}, Command: []string{"true"}, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	b.cfg.AWSLambdaMicroVM.Image = "arn:aws:lambda:eu-west-1:123456789012:microvm-image:new-runner"
	if _, err := b.Status(context.Background(), StatusRequest{ID: result.LeaseID}); err != nil {
		t.Fatalf("status after image rotation: %v", err)
	}
	servers, err := b.List(context.Background(), ListRequest{})
	if err != nil || len(servers) != 1 {
		t.Fatalf("list after image rotation: servers=%#v err=%v", servers, err)
	}
	if _, err := b.Run(context.Background(), RunRequest{Repo: Repo{Root: "/repo", Name: "my-app"}, ID: result.LeaseID, Command: []string{"true"}, NoSync: true}); err == nil || !strings.Contains(err.Error(), "image identity mismatch") {
		t.Fatalf("reuse after image rotation err=%v", err)
	}
	if err := b.Stop(context.Background(), StopRequest{ID: result.LeaseID}); err != nil {
		t.Fatalf("stop after image rotation: %v", err)
	}
}

func TestNoSyncWorkspaceFailureStopsBeforeCommand(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	control := &fakeControlPlane{}
	runner := &fakeRunner{exitCode: 9}
	b := testBackend(control, runner, io.Discard)
	result, err := b.Run(context.Background(), RunRequest{Repo: Repo{Root: testRepo(t), Name: "my-app"}, Command: []string{"must-not-run"}, NoSync: true})
	if err == nil || result.ExitCode != 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(runner.commands) != 1 || !strings.Contains(runner.commands[0], "mkdir -p") {
		t.Fatalf("commands=%v", runner.commands)
	}
	if !slices.Contains(control.calls, "terminate:mvm-test") {
		t.Fatalf("one-shot cleanup missing: %v", control.calls)
	}
}

func TestCreateRollsBackWhenRunnerIsUnhealthy(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	control := &fakeControlPlane{}
	runner := &fakeRunner{healthErr: errors.New("runner unavailable")}
	b := testBackend(control, runner, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Warmup(ctx, WarmupRequest{Repo: Repo{Root: "/repo", Name: "my-app"}, Keep: true}); err == nil {
		t.Fatal("unhealthy runner unexpectedly became ready")
	}
	if !slices.Contains(control.calls, "terminate:mvm-test") {
		t.Fatalf("rollback missing: %v", control.calls)
	}
}

func TestCreateRollbackFailurePersistsExactRecoveryClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	terminateErr := errors.New("termination denied")
	control := &fakeControlPlane{terminateErr: terminateErr}
	runner := &fakeRunner{healthErr: errors.New("runner unavailable")}
	b := testBackend(control, runner, io.Discard)
	var stderr bytes.Buffer
	b.rt.Stderr = &stderr
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := b.Warmup(ctx, WarmupRequest{Repo: Repo{Root: "/repo", Name: "my-app"}, Keep: true})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, terminateErr) {
		t.Fatalf("Warmup err=%v, want joined cancellation and termination failure", err)
	}
	if !strings.Contains(err.Error(), "mvm-test") || !strings.Contains(err.Error(), "recovery claim") {
		t.Fatalf("Warmup err=%v, want exact MicroVM and recovery claim", err)
	}
	if !strings.Contains(stderr.String(), "mvm-test") || !strings.Contains(stderr.String(), "crabbox stop --provider aws-lambda-microvm") {
		t.Fatalf("stderr=%q, want exact MicroVM and cleanup command", stderr.String())
	}
	if !control.terminateDeadline || control.terminateContextErr != nil {
		t.Fatalf("rollback context deadline=%t err=%v", control.terminateDeadline, control.terminateContextErr)
	}
	claims, err := listLeaseClaims()
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims=%#v err=%v, want one recovery claim", claims, err)
	}
	claim := claims[0]
	if claim.Provider != providerName || claim.ProviderScope != "eu-west-1" || claim.CloudID != "mvm-test" || claim.Labels["keep"] != "false" {
		t.Fatalf("recovery claim=%#v", claim)
	}
	control.terminateErr = nil
	if err := b.Stop(context.Background(), StopRequest{ID: claim.LeaseID}); err != nil {
		t.Fatalf("recovery stop: %v", err)
	}
	if _, ok, err := resolveLeaseClaim(claim.LeaseID); err != nil || ok {
		t.Fatalf("recovery claim remains ok=%t err=%v", ok, err)
	}
}

func TestCreateRollbackFailureReportsRecoveryClaimPersistenceFailure(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	terminateErr := errors.New("termination denied")
	control := &fakeControlPlane{terminateErr: terminateErr}
	runner := &fakeRunner{healthCheck: func(context.Context, microVM) error {
		return os.WriteFile(filepath.Join(stateDir, "crabbox"), []byte("blocked"), 0o600)
	}}
	b := testBackend(control, runner, io.Discard)
	var stderr bytes.Buffer
	b.rt.Stderr = &stderr

	err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: "/repo", Name: "my-app"}, Keep: true})
	if !errors.Is(err, terminateErr) || !strings.Contains(err.Error(), "mvm-test") || !strings.Contains(err.Error(), "could not be persisted") {
		t.Fatalf("Warmup err=%v, want termination and recovery persistence failures", err)
	}
	if !strings.Contains(stderr.String(), "mvm-test") || !strings.Contains(stderr.String(), "terminate the MicroVM manually") {
		t.Fatalf("stderr=%q, want actionable exact-resource warning", stderr.String())
	}
	if !slices.Contains(control.calls, "terminate:mvm-test") {
		t.Fatalf("rollback missing: %v", control.calls)
	}
}

func TestWarmupWarnsWhenKeepIsFalse(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	control := &fakeControlPlane{}
	runner := &fakeRunner{}
	b := testBackend(control, runner, io.Discard)
	var stderr bytes.Buffer
	b.rt.Stderr = &stderr
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: "/repo", Name: "my-app"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "warmup keeps the MicroVM until explicit stop") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestRunnerClientExecPreservesBinaryOutput(t *testing.T) {
	want := []byte{0xff, 0x00, 0xfe}
	dataEvent, err := json.Marshal(runnerEvent{Stream: "stdout", Data: want})
	if err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	exitEvent, err := json.Marshal(runnerEvent{ExitCode: &exitCode})
	if err != nil {
		t.Fatal(err)
	}
	body := append(append(dataEvent, '\n'), append(exitEvent, '\n')...)
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("X-aws-proxy-auth") != "token" {
			t.Fatalf("missing auth header: %v", req.Header)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    req,
		}, nil
	})}
	client := mustNewRunnerClient(t, &fakeControlPlane{}, httpClient, "eu-west-1")
	var stdout bytes.Buffer
	gotExit, err := client.Exec(context.Background(), microVM{ID: "mvm-test", Endpoint: "mvm-test.lambda-microvm.eu-west-1.on.aws"}, "true", "/workspace/crabbox", nil, &stdout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if gotExit != 0 || !bytes.Equal(stdout.Bytes(), want) {
		t.Fatalf("exit=%d stdout=%x want=%x", gotExit, stdout.Bytes(), want)
	}
}

func TestNewRunnerClientDefaultsResponseHeaderTimeout(t *testing.T) {
	client := mustNewRunnerClient(t, &fakeControlPlane{}, nil, "eu-west-1")
	if client.http == nil {
		t.Fatal("expected default HTTP client")
	}
	if client.http.Timeout != 0 {
		t.Fatalf("client timeout=%s, want no whole-request timeout", client.http.Timeout)
	}
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport=%T, want *http.Transport", client.http.Transport)
	}
	if transport == http.DefaultTransport {
		t.Fatal("fallback client reused the shared default transport")
	}
	if transport.ResponseHeaderTimeout != runnerResponseHeaderTimeout {
		t.Fatalf("response header timeout=%s want %s", transport.ResponseHeaderTimeout, runnerResponseHeaderTimeout)
	}
}

func TestNewRunnerClientRejectsUnsupportedDefaultTransport(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	var calls atomic.Int32
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("deny all")
	})

	client, err := newRunnerClient(&fakeControlPlane{}, nil, "eu-west-1")
	if client != nil || err == nil || !strings.Contains(err.Error(), "non-nil *http.Transport") {
		t.Fatalf("client=%#v err=%v, want transport setup error", client, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("custom default invoked %d times, want 0", calls.Load())
	}
}

func TestNewRunnerClientAcceptsExplicitClientWithUnsupportedDefault(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("deny all")
	})
	injected := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("injected")
	})}

	client, err := newRunnerClient(&fakeControlPlane{}, injected, "eu-west-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.http.Transport.(roundTripFunc); !ok {
		t.Fatalf("transport=%T, want explicit roundTripFunc", client.http.Transport)
	}
}

func TestDefaultRunnerHTTPClientTimesOutBeforeResponseHeaders(t *testing.T) {
	const bound = 40 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(5 * bound)
	}))
	defer server.Close()

	started := time.Now()
	client := mustDefaultRunnerHTTPClient(t, bound)
	_, err := client.Get(server.URL)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("request with withheld response headers unexpectedly succeeded")
	}
	var timeoutError interface{ Timeout() bool }
	if !errors.As(err, &timeoutError) || !timeoutError.Timeout() {
		t.Fatalf("error=%v, want timeout", err)
	}
	if elapsed >= time.Second {
		t.Fatalf("withheld response headers failed after %s, want near %s", elapsed, bound)
	}
	t.Logf("withheld response headers failed after %s (configured bound %s)", elapsed.Round(time.Millisecond), bound)
}

func TestDefaultRunnerHTTPClientStreamsPastResponseHeaderTimeout(t *testing.T) {
	const bound = 30 * time.Millisecond
	const streamDelay = 3 * bound
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(streamDelay)
		_, _ = io.WriteString(w, "stream-complete")
	}))
	defer server.Close()

	started := time.Now()
	client := mustDefaultRunnerHTTPClient(t, bound)
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "stream-complete" {
		t.Fatalf("body=%q want stream-complete", body)
	}
	if elapsed <= bound {
		t.Fatalf("stream completed in %s, want it readable beyond %s", elapsed, bound)
	}
	t.Logf("response headers flushed promptly; body remained readable through %s, past the %s header bound", elapsed.Round(time.Millisecond), bound)
}

func TestDefaultRunnerHTTPClientUploadsPastResponseHeaderTimeout(t *testing.T) {
	const bound = 30 * time.Millisecond
	const uploadDelay = 3 * bound
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("read upload: %v", err)
			return
		}
		if string(body) != "upload-complete" {
			t.Errorf("upload body=%q want upload-complete", body)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	reader, writer := io.Pipe()
	go func() {
		_, _ = io.WriteString(writer, "upload-")
		time.Sleep(uploadDelay)
		_, _ = io.WriteString(writer, "complete")
		_ = writer.Close()
	}()
	started := time.Now()
	client := mustDefaultRunnerHTTPClient(t, bound)
	resp, err := client.Post(server.URL, "application/gzip", reader)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d want %d", resp.StatusCode, http.StatusNoContent)
	}
	if elapsed <= bound {
		t.Fatalf("upload completed in %s, want it to outlive %s", elapsed, bound)
	}
	t.Logf("upload completed after %s, beyond the %s response-header bound", elapsed.Round(time.Millisecond), bound)
}

func TestNewRunnerClientPreservesInjectedHTTPSettingsOnClone(t *testing.T) {
	proxyURL, err := url.Parse("http://proxy.example.test:8080")
	if err != nil {
		t.Fatal(err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: tlsConfig}
	injected := &http.Client{
		Transport: transport,
		Jar:       jar,
		Timeout:   17 * time.Second,
	}

	client := mustNewRunnerClient(t, &fakeControlPlane{}, injected, "eu-west-1")
	if client.http == injected {
		t.Fatal("runner reused the injected client instead of cloning it")
	}
	if client.http.Transport != transport || client.http.Jar != jar || client.http.Timeout != 17*time.Second {
		t.Fatalf("cloned settings: transport=%p jar=%p timeout=%s", client.http.Transport, client.http.Jar, client.http.Timeout)
	}
	if transport.TLSClientConfig != tlsConfig || transport.Proxy == nil {
		t.Fatal("injected proxy or TLS configuration was not preserved")
	}
	if injected.Transport != transport || injected.Jar != jar || injected.Timeout != 17*time.Second || injected.CheckRedirect != nil {
		t.Fatal("newRunnerClient mutated the injected source client")
	}
}

func TestNewRunnerClientPreservesStricterSameOriginRedirectPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/redirected", http.StatusFound)
	}))
	defer server.Close()

	callerErr := errors.New("injected redirect policy")
	var callerChecks atomic.Int32
	injected := server.Client()
	injected.CheckRedirect = func(*http.Request, []*http.Request) error {
		callerChecks.Add(1)
		return callerErr
	}
	client := mustNewRunnerClient(t, &fakeControlPlane{}, injected, "eu-west-1")

	_, err := client.http.Get(server.URL)
	if !errors.Is(err, callerErr) {
		t.Fatalf("redirect error=%v want injected policy", err)
	}
	if callerChecks.Load() != 1 {
		t.Fatalf("injected redirect checks=%d want 1", callerChecks.Load())
	}
	if injected.CheckRedirect == nil {
		t.Fatal("newRunnerClient cleared the injected redirect policy")
	}
}

func TestNewRunnerClientRejectsCrossOriginRedirectBeforeInjectedPolicy(t *testing.T) {
	origin, err := url.Parse("https://mvm-test.lambda-microvm.eu-west-1.on.aws")
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse("http://127.0.0.1:8080/stolen")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		callback bool
	}{
		{name: "nil callback"},
		{name: "permissive callback", callback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var callerChecks atomic.Int32
			injected := &http.Client{}
			if tc.callback {
				injected.CheckRedirect = func(*http.Request, []*http.Request) error {
					callerChecks.Add(1)
					return nil
				}
			}
			client := mustNewRunnerClient(t, &fakeControlPlane{}, injected, "eu-west-1")
			err := client.http.CheckRedirect(&http.Request{URL: target}, []*http.Request{{URL: origin}})
			if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
				t.Fatalf("redirect error=%v want cross-origin refusal", err)
			}
			if callerChecks.Load() != 0 {
				t.Fatalf("injected policy called %d times before cross-origin refusal", callerChecks.Load())
			}
		})
	}
}

func TestNewRunnerClientRetainsDefaultSameOriginRedirectLimit(t *testing.T) {
	origin, err := url.Parse("https://mvm-test.lambda-microvm.eu-west-1.on.aws")
	if err != nil {
		t.Fatal(err)
	}
	injected := &http.Client{}
	client := mustNewRunnerClient(t, &fakeControlPlane{}, injected, "eu-west-1")
	request := &http.Request{URL: origin}
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = &http.Request{URL: origin}
	}
	if err := client.http.CheckRedirect(request, via); err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("redirect error=%v want default redirect limit", err)
	}
	if injected.CheckRedirect != nil {
		t.Fatal("newRunnerClient mutated the source redirect policy")
	}
}

func TestRunnerClientCrossOriginRedirectNeverSendsProxyHeaders(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		targetRequests.Add(1)
		t.Errorf("cross-origin target received auth=%q port=%q", req.Header.Get("X-aws-proxy-auth"), req.Header.Get("X-aws-proxy-port"))
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer target.Close()
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}

	var injectedChecks atomic.Int32
	injected := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host == targetURL.Host {
				return http.DefaultTransport.RoundTrip(req)
			}
			if req.Header.Get("X-aws-proxy-auth") != "token" || req.Header.Get("X-aws-proxy-port") != fmt.Sprint(runnerPort) {
				t.Fatalf("runner request headers: auth=%q port=%q", req.Header.Get("X-aws-proxy-auth"), req.Header.Get("X-aws-proxy-port"))
			}
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Header:     http.Header{"Location": []string{target.URL + "/stolen"}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		}),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			injectedChecks.Add(1)
			return nil
		},
	}
	client := mustNewRunnerClient(t, &fakeControlPlane{}, injected, "eu-west-1")
	err = client.Health(context.Background(), microVM{ID: "mvm-test", Endpoint: "mvm-test.lambda-microvm.eu-west-1.on.aws"})
	if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("Health error=%v want cross-origin refusal", err)
	}
	if injectedChecks.Load() != 0 {
		t.Fatalf("injected redirect checks=%d want 0", injectedChecks.Load())
	}
	if targetRequests.Load() != 0 {
		t.Fatalf("cross-origin target requests=%d want 0", targetRequests.Load())
	}
}

func TestWaitReadyBoundsRunnerHealthProbe(t *testing.T) {
	control := &fakeControlPlane{
		vm: microVM{ID: "mvm-test", Endpoint: "mvm-test.lambda-microvm.eu-west-1.on.aws", ImageARN: testImageARN, ImageVersion: "1", State: "RUNNING"},
	}
	runner := &fakeRunner{
		healthCheck: func(ctx context.Context, _ microVM) error {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("runner health context had no deadline")
			}
			if remaining := time.Until(deadline); remaining <= 0 || remaining > runnerHealthProbeTimeout+time.Second {
				t.Fatalf("runner health deadline remaining=%s", remaining)
			}
			return nil
		},
	}
	b := testBackend(control, runner, io.Discard)
	if _, err := b.waitReady(context.Background(), control, runner, control.vm); err != nil {
		t.Fatal(err)
	}
}

func TestProviderFlagsAndEndpointValidation(t *testing.T) {
	cfg := core.BaseConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--aws-lambda-microvm-region", "eu-west-1", "--aws-lambda-microvm-image", testImageARN, "--aws-lambda-microvm-workdir", "/work/project", "--aws-lambda-microvm-forget-missing"}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.AWSLambdaMicroVM.Workdir != "/work/project" || !cfg.AWSLambdaMicroVM.ForgetMissing {
		t.Fatalf("config=%#v", cfg.AWSLambdaMicroVM)
	}
	if _, err := endpointURL("mvm-test.lambda-microvm.eu-west-1.on.aws", "eu-west-1"); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"http://mvm-test.lambda-microvm.eu-west-1.on.aws", "https://example.com", "https://mvm-test.lambda-microvm.us-east-1.on.aws"} {
		if _, err := endpointURL(endpoint, "eu-west-1"); err == nil {
			t.Fatalf("accepted endpoint %q", endpoint)
		}
	}
}

func TestProviderRejectsBroadWorkdirs(t *testing.T) {
	for _, workdir := range []string{"/", "/tmp", "/work", "/workspace", "/home", "/root", "/usr", "/var", "/etc"} {
		cfg := core.BaseConfig()
		cfg.Provider = providerName
		cfg.AWSRegion = "eu-west-1"
		cfg.AWSLambdaMicroVM.Image = testImageARN
		cfg.AWSLambdaMicroVM.Workdir = workdir
		if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "too broad") {
			t.Fatalf("validateConfig(%q) err=%v, want too broad", workdir, err)
		}
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AWSRegion = "eu-west-1"
	cfg.AWSLambdaMicroVM.Image = testImageARN
	cfg.AWSLambdaMicroVM.Workdir = "/workspace/crabbox"
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("validateConfig dedicated workdir: %v", err)
	}
}

func TestProviderRejectsTailscaleOptions(t *testing.T) {
	base := func() core.Config {
		cfg := core.BaseConfig()
		cfg.Provider = providerName
		cfg.AWSRegion = "eu-west-1"
		cfg.AWSLambdaMicroVM.Image = testImageARN
		cfg.AWSLambdaMicroVM.Workdir = "/workspace/crabbox"
		return cfg
	}
	cfg := base()
	cfg.Tailscale.Enabled = true
	if err := (Provider{}).ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "does not support Tailscale") {
		t.Fatalf("tailscale enabled err=%v", err)
	}
	cfg = base()
	cfg.Network = core.NetworkTailscale
	if err := (Provider{}).ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "does not support Tailscale") {
		t.Fatalf("tailscale network err=%v", err)
	}
}

func TestProviderRejectsSubMinuteIdleTimeout(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AWSRegion = "eu-west-1"
	cfg.AWSLambdaMicroVM.Image = testImageARN
	cfg.AWSLambdaMicroVM.Workdir = "/workspace/crabbox"
	cfg.IdleTimeout = 59 * time.Second
	if err := (Provider{}).ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "at least 60s") {
		t.Fatalf("sub-minute idle timeout err=%v", err)
	}
	cfg.IdleTimeout = time.Minute
	if err := (Provider{}).ValidateConfig(cfg); err != nil {
		t.Fatalf("one-minute idle timeout err=%v", err)
	}
}

func TestLeaseOperationLockRejectsConcurrentOwner(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := leasePrefix + "locktest"
	unlock, err := lockAWSLambdaMicroVMLeaseOperation(context.Background(), leaseID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := lockAWSLambdaMicroVMLeaseOperation(ctx, leaseID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock err=%v, want context deadline", err)
	}
}

func testBackend(control *fakeControlPlane, runner *fakeRunner, stdout io.Writer) *backend {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AWSRegion = "eu-west-1"
	cfg.AWSLambdaMicroVM.Image = testImageARN
	cfg.AWSLambdaMicroVM.Workdir = "/workspace/crabbox"
	cfg.IdleTimeout = time.Hour
	cfg.TTL = 0
	return &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: stdout, Stderr: io.Discard},
		newControl: func(context.Context, Config) (controlPlane, error) {
			return control, nil
		},
		newRunner: func(controlPlane, Config, Runtime) (runnerAPI, error) { return runner, nil },
	}
}

func mustNewRunnerClient(t *testing.T, control controlPlane, source *http.Client, region string) *runnerClient {
	t.Helper()
	client, err := newRunnerClient(control, source, region)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func mustDefaultRunnerHTTPClient(t *testing.T, timeout time.Duration) *http.Client {
	t.Helper()
	client, err := defaultRunnerHTTPClient(timeout)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func testRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", dir, "init", "--quiet").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", dir, "add", ".").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "init").Run(); err != nil {
		t.Fatal(err)
	}
	return dir
}
