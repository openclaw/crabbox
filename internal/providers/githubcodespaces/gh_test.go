package githubcodespaces

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestGHRunnerCodespaceSSHConfigArgv(t *testing.T) {
	runner := &recordingRunner{result: LocalCommandResult{Stdout: "Host sturdy-space\n"}}
	gh := newGHRunner(GitHubCodespacesConfig{GHPath: "/opt/gh"}, Runtime{Exec: runner})
	out, err := gh.codespaceSSHConfig(context.Background(), "sturdy-space")
	if err != nil {
		t.Fatal(err)
	}
	if out != "Host sturdy-space\n" {
		t.Fatalf("out=%q", out)
	}
	call := runner.onlyCall(t)
	if call.Name != "/opt/gh" || strings.Join(call.Args, " ") != "codespace ssh --config -c sturdy-space" {
		t.Fatalf("call=%#v", call)
	}
	for _, arg := range call.Args {
		if looksLikeGitHubToken(arg) {
			t.Fatalf("token arg leaked: %#v", call.Args)
		}
	}
}

func TestGHRunnerAuthStatusReadOnly(t *testing.T) {
	runner := &recordingRunner{}
	gh := newGHRunner(GitHubCodespacesConfig{GHPath: "gh"}, Runtime{Exec: runner})
	if err := gh.authStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	call := runner.onlyCall(t)
	if strings.Join(call.Args, " ") != "auth status" {
		t.Fatalf("call=%#v", call)
	}
}

func TestGHRunnerErrorRedactsToken(t *testing.T) {
	runner := &recordingRunner{
		result: LocalCommandResult{ExitCode: 1, Stderr: "denied ghp_this_token_value_is_redacted"},
		err:    fmt.Errorf("ghp_this_token_value_is_redacted failed"),
	}
	gh := newGHRunner(GitHubCodespacesConfig{GHPath: "gh"}, Runtime{Exec: runner})
	_, err := gh.codespaceSSHConfig(context.Background(), "sturdy-space")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "ghp_this_token_value_is_redacted") {
		t.Fatalf("token leaked: %v", err)
	}
}

type recordingRunner struct {
	calls  []LocalCommandRequest
	result LocalCommandResult
	err    error
}

func (r *recordingRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	return r.result, r.err
}

func (r *recordingRunner) onlyCall(t *testing.T) LocalCommandRequest {
	t.Helper()
	if len(r.calls) != 1 {
		t.Fatalf("calls=%#v", r.calls)
	}
	return r.calls[0]
}
