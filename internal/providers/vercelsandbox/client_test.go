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

func TestAppendBridgeExecOutputBoundsCapturedResult(t *testing.T) {
	got := strings.Repeat("a", bridgeExecCaptureLimit-1)
	appendBridgeExecOutput(&got, "bc")
	if len(got) != bridgeExecCaptureLimit || !strings.HasSuffix(got, "ab") {
		t.Fatalf("captured length=%d suffix=%q", len(got), got[len(got)-2:])
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
