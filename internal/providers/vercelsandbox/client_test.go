package vercelsandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBridgeListCommandKeepsSecretsOffArgv(t *testing.T) {
	t.Setenv("CRABBOX_VERCEL_SANDBOX_AUTH_TOKEN", "secret-auth-token")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_TOKEN", "secret-sdk-token")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_OIDC_TOKEN", "secret-oidc-token")
	var seen commandSpec
	client := &bridgeClient{
		lookup: func(name string) (string, error) { return "/usr/local/bin/" + name, nil },
		run: func(_ context.Context, spec commandSpec) error {
			seen = spec
			return nil
		},
	}
	if err := client.CheckAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen.Name != "sandbox" {
		t.Fatalf("command name=%q", seen.Name)
	}
	joinedArgs := strings.Join(seen.Args, " ")
	for _, forbidden := range []string{"--token", "secret-auth-token", "secret-sdk-token", "secret-oidc-token", "--env"} {
		if strings.Contains(joinedArgs, forbidden) {
			t.Fatalf("argv leaked forbidden value %q: %v", forbidden, seen.Args)
		}
	}
	for _, want := range []string{"list", "--all", "--limit", "1"} {
		if !slices.Contains(seen.Args, want) {
			t.Fatalf("argv missing %q: %v", want, seen.Args)
		}
	}
	env := strings.Join(seen.Env, "\n")
	for _, want := range []string{"VERCEL_AUTH_TOKEN=secret-auth-token", "VERCEL_TOKEN=secret-sdk-token", "VERCEL_OIDC_TOKEN=secret-oidc-token"} {
		if !strings.Contains(env, want) {
			t.Fatalf("env missing %q in %q", want, env)
		}
	}
}

func TestBridgeExecSendsEnvInStructuredRequest(t *testing.T) {
	var seen bridgeRequest
	client := &bridgeClient{
		call: func(_ context.Context, req bridgeRequest, out any) error {
			seen = req
			if result, ok := out.(*execResult); ok {
				*result = execResult{}
			}
			return nil
		},
	}
	_, err := client.Exec(context.Background(), "sbx_1", execRequest{
		Command:    "printenv PUBLIC_VALUE",
		WorkingDir: "/work",
		Env:        map[string]string{"PUBLIC_VALUE": "visible"},
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if seen.Action != "exec" || seen.Exec == nil {
		t.Fatalf("request=%#v", seen)
	}
	if seen.Exec.Env["PUBLIC_VALUE"] != "visible" {
		t.Fatalf("env not carried in structured request: %#v", seen.Exec.Env)
	}
	if strings.Contains(strings.Join(client.bridgeCommandSpec().Args, " "), "PUBLIC_VALUE=visible") {
		t.Fatalf("env leaked through bridge argv")
	}
}

type notifyingWriter struct {
	bytes.Buffer
	once sync.Once
	ch   chan struct{}
}

func (w *notifyingWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if n > 0 {
		w.once.Do(func() { close(w.ch) })
	}
	return n, err
}

func (w *notifyingWriter) WriteString(value string) (int, error) {
	return w.Write([]byte(value))
}

func TestRunBridgeExecStreamsBeforeCommandCompletes(t *testing.T) {
	stdout := &notifyingWriter{ch: make(chan struct{})}
	var stderr bytes.Buffer
	spec := commandSpec{
		Name: "sh",
		Args: []string{"-c", `
printf '%s\n' '{"type":"stdout","data":"early\n"}'
sleep 0.4
printf '%s\n' '{"type":"stderr","data":"warning\n"}'
printf '%s\n' '{"type":"stdout","data":"late\n"}'
printf '%s\n' '{"type":"result","exitCode":23}'
`},
		Env: os.Environ(),
	}
	type outcome struct {
		result execResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := runBridgeExec(context.Background(), spec, bridgeRequest{Action: "exec"}, stdout, &stderr)
		done <- outcome{result: result, err: err}
	}()

	select {
	case <-stdout.ch:
	case got := <-done:
		t.Fatalf("bridge completed before first output: result=%+v err=%v", got.result, got.err)
	case <-time.After(2 * time.Second):
		t.Fatal("first streamed stdout did not arrive")
	}
	select {
	case got := <-done:
		t.Fatalf("bridge completed before delayed output: result=%+v err=%v", got.result, got.err)
	default:
	}
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.result.ExitCode != 23 {
		t.Fatalf("exit=%d want 23", got.result.ExitCode)
	}
	if got.result.Stdout != "early\nlate\n" || got.result.Stderr != "warning\n" {
		t.Fatalf("result=%+v", got.result)
	}
	if stdout.String() != "early\nlate\n" {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if stderr.String() != "warning\n" {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestRunBridgeExecAcceptsLegacyResult(t *testing.T) {
	var stdout, stderr bytes.Buffer
	spec := commandSpec{
		Name: "sh",
		Args: []string{"-c", `printf '%s\n' '{' '  "stdout": "legacy out\n",' '  "stderr": "legacy err\n",' '  "exitCode": 7' '}'`},
		Env:  os.Environ(),
	}
	result, err := runBridgeExec(context.Background(), spec, bridgeRequest{Action: "exec"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 7 || result.Stdout != "legacy out\n" || result.Stderr != "legacy err\n" ||
		stdout.String() != "legacy out\n" || stderr.String() != "legacy err\n" {
		t.Fatalf("result=%+v stdout=%q stderr=%q", result, stdout.String(), stderr.String())
	}
}

func TestRunBridgeExecPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stdout := &notifyingWriter{ch: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := runBridgeExec(ctx, commandSpec{
			Name: "sh", Args: []string{"-c", `printf '%s\n' '{"type":"stdout","data":"ready"}'; exec sleep 30`},
			Env: []string{"PATH=/usr/bin:/bin"},
		}, bridgeRequest{Action: "exec"}, stdout, io.Discard)
		done <- err
	}()
	select {
	case <-stdout.ch:
		cancel()
	case err := <-done:
		t.Fatalf("bridge ended before cancellation: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("bridge did not reach its running state")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("bridge lost cancellation cause: %v", err)
	}
}

func TestRunBridgeExecPreservesDeadlineDuringPartialFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := runBridgeExec(ctx, commandSpec{
		Name: "sh", Args: []string{"-c", `printf '{"type":'; exec sleep 30`},
		Env: []string{"PATH=/usr/bin:/bin"},
	}, bridgeRequest{Action: "exec"}, io.Discard, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("partial frame lost deadline cause: %v", err)
	}
}

func TestRunBridgeCommandsPreserveDeadline(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(context.Context, commandSpec) error
	}{
		{name: "command", run: runBridgeCommand},
		{name: "JSON", run: func(ctx context.Context, spec commandSpec) error {
			return runBridgeJSONWithPayload(ctx, spec, bridgeRequest{Action: "fixture"}, nil, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			err := tc.run(ctx, commandSpec{Name: "sh", Args: []string{"-c", "exec sleep 30"}, Env: []string{"PATH=/usr/bin:/bin"}})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("bridge lost deadline: %v", err)
			}
		})
	}
}

func TestAppendBridgeExecOutputBoundsCapturedResult(t *testing.T) {
	got := strings.Repeat("a", bridgeExecCaptureLimit-1)
	appendBridgeExecOutput(&got, "bc")
	if len(got) != bridgeExecCaptureLimit || !strings.HasSuffix(got, "ab") {
		t.Fatalf("captured length=%d suffix=%q", len(got), got[len(got)-2:])
	}
}

func TestRunBridgeJSONRejectsOversizedResponse(t *testing.T) {
	spec := commandSpec{
		Name: "sh",
		Args: []string{"-c", "printf '%*s' 4194305 ''"},
		Env:  os.Environ(),
	}
	var result sandboxSummary
	err := runBridgeJSONWithPayload(t.Context(), spec, bridgeRequest{Action: "get"}, nil, &result)
	if err == nil || !strings.Contains(err.Error(), "stdout exceeded 4194304-byte output limit") ||
		!strings.Contains(err.Error(), "[crabbox: vercel-sandbox bridge stdout truncated after 4194304 bytes]") {
		t.Fatalf("oversized bridge response err=%v", err)
	}
}

func TestRunBridgeExecBoundsBridgeStderr(t *testing.T) {
	spec := commandSpec{
		Name: "sh",
		Args: []string{"-c", "printf '%*s' 4194305 '' >&2; exit 9"},
		Env:  os.Environ(),
	}
	_, err := runBridgeExec(t.Context(), spec, bridgeRequest{Action: "exec"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("oversized bridge stderr unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "[crabbox: vercel-sandbox bridge stderr truncated after 4194304 bytes]") {
		t.Fatalf("oversized bridge stderr error omitted truncation marker; captured %d bytes", len(err.Error()))
	}
	if len(err.Error()) > bridgeExecCaptureLimit+256 {
		t.Fatalf("bridge stderr error captured %d bytes", len(err.Error()))
	}
}

func TestRedactSecretsRemovesTokenValues(t *testing.T) {
	t.Setenv("CRABBOX_VERCEL_SANDBOX_AUTH_TOKEN", "secret-auth-token")
	got := redactSecrets("request failed with secret-auth-token")
	if strings.Contains(got, "secret-auth-token") {
		t.Fatalf("secret was not redacted: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("redaction marker missing: %q", got)
	}
}

func TestCheckProjectUsesReadOnlyBridgeScopeValidation(t *testing.T) {
	cfg := Config{}
	cfg.VercelSandbox.ProjectID = "prj_123"
	cfg.VercelSandbox.TeamID = "team_123"
	var got bridgeRequest
	client := &bridgeClient{
		cfg: cfg,
		call: func(_ context.Context, req bridgeRequest, _ any) error {
			got = req
			return nil
		},
	}
	if err := client.CheckProject(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.Action != "check-project" || got.Config.ProjectID != "prj_123" || got.Config.TeamID != "team_123" {
		t.Fatalf("request=%#v", got)
	}
}

func TestResolveProjectScopeUsesBridge(t *testing.T) {
	for _, tc := range []struct {
		name     string
		readOnly bool
		action   string
	}{
		{name: "mutating", action: "resolve-scope"},
		{name: "read-only", readOnly: true, action: "resolve-scope-read-only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got bridgeRequest
			client := &bridgeClient{
				call: func(_ context.Context, req bridgeRequest, out any) error {
					got = req
					*(out.(*projectScope)) = projectScope{ProjectID: "prj_123", TeamID: "team_123"}
					return nil
				},
			}
			scope, err := client.ResolveProjectScope(context.Background(), tc.readOnly)
			if err != nil {
				t.Fatal(err)
			}
			if got.Action != tc.action || scope.ProjectID != "prj_123" || scope.TeamID != "team_123" {
				t.Fatalf("request=%#v scope=%#v", got, scope)
			}
			if client.cfg.VercelSandbox.ProjectID != "prj_123" || client.cfg.VercelSandbox.TeamID != "team_123" {
				t.Fatalf("resolved scope was not retained by bridge client: %#v", client.cfg.VercelSandbox)
			}
		})
	}
}

func TestResolveProjectScopeKeepsOIDCBridgeConfigImplicit(t *testing.T) {
	t.Setenv("VERCEL_OIDC_TOKEN", "header.payload.signature")
	client := &bridgeClient{
		call: func(_ context.Context, _ bridgeRequest, out any) error {
			*(out.(*projectScope)) = projectScope{ProjectID: "prj_oidc", TeamID: "team_oidc"}
			return nil
		},
	}
	if _, err := client.ResolveProjectScope(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if client.cfg.VercelSandbox.ProjectID != "" || client.cfg.VercelSandbox.TeamID != "" {
		t.Fatalf("OIDC scope should remain implicit: %#v", client.cfg.VercelSandbox)
	}
}

func TestCheckCLIMissingReportsEnvironmentBlocker(t *testing.T) {
	client := &bridgeClient{lookup: func(string) (string, error) { return "", os.ErrNotExist }}
	_, err := client.CheckCLI(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sandbox CLI unavailable") {
		t.Fatalf("CheckCLI err=%v", err)
	}
}

func TestCheckAuthRedactsCommandOutput(t *testing.T) {
	t.Setenv("VERCEL_AUTH_TOKEN", "secret-auth-token")
	client := &bridgeClient{
		run: func(context.Context, commandSpec) error {
			return errors.New("bad token secret-auth-token")
		},
	}
	err := client.CheckAuth(context.Background())
	if err == nil {
		t.Fatal("CheckAuth succeeded unexpectedly")
	}
	if strings.Contains(err.Error(), "secret-auth-token") {
		t.Fatalf("secret leaked in error: %v", err)
	}
}
