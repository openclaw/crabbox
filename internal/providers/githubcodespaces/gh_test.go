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
		t.Fatalf("command=%q args=%q", call.Name, strings.Join(call.Args, " "))
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
	if strings.Join(call.Args, " ") != "auth status --hostname github.com" {
		t.Fatalf("args=%q", strings.Join(call.Args, " "))
	}
}

func TestGHRunnerRoutesAuthAndCodespaceCommandsToConfiguredAPIHost(t *testing.T) {
	t.Setenv("GH_TOKEN", "dotcom-test-token")
	t.Setenv("GITHUB_TOKEN", "dotcom-fallback-token")
	t.Setenv("GH_ENTERPRISE_TOKEN", "enterprise-test-token")
	runner := &recordingRunner{result: LocalCommandResult{Stdout: "token"}}
	gh := newGHRunner(GitHubCodespacesConfig{GHPath: "gh", APIURL: "https://api.enterprise.example:8443/api/v3"}, Runtime{Exec: runner})
	if _, err := gh.authToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := gh.codespaceSSHConfig(context.Background(), "sturdy-space"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runner.calls[0].Args, " "); got != "auth token --hostname api.enterprise.example:8443" {
		t.Fatalf("auth args=%q", got)
	}
	for i, call := range runner.calls {
		foundHost := false
		foundEnterpriseToken := false
		foundDotcomToken := false
		for _, entry := range call.Env {
			if entry == "GH_HOST=api.enterprise.example:8443" {
				foundHost = true
			}
			if strings.HasPrefix(entry, "GH_ENTERPRISE_TOKEN=") {
				foundEnterpriseToken = true
			}
			if strings.HasPrefix(entry, "GH_TOKEN=") || strings.HasPrefix(entry, "GITHUB_TOKEN=") {
				foundDotcomToken = true
			}
		}
		if !foundHost || !foundEnterpriseToken || foundDotcomToken {
			t.Fatalf("call %d host=%t enterprise_token=%t dotcom_token=%t", i, foundHost, foundEnterpriseToken, foundDotcomToken)
		}
	}
}

func TestGHRunnerMapsGHECloudAPIHostToCLIHost(t *testing.T) {
	runner := &recordingRunner{}
	gh := newGHRunner(GitHubCodespacesConfig{GHPath: "gh", APIURL: "https://api.octocorp.ghe.com"}, Runtime{Exec: runner})
	if err := gh.authStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	call := runner.onlyCall(t)
	if got := strings.Join(call.Args, " "); got != "auth status --hostname octocorp.ghe.com" {
		t.Fatalf("args=%q", got)
	}
	foundHost := false
	for _, entry := range call.Env {
		if entry == "GH_HOST=octocorp.ghe.com" {
			foundHost = true
		}
	}
	if !foundHost {
		t.Fatal("missing GHE.com CLI host")
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

func TestRedactSecretTextRedactsEmbeddedTokens(t *testing.T) {
	input := "clone https://x-access-token:ghp_this_token_value_is_redacted@github.com/example-org/my-app.git\nerror=github_pat_this_token_value_is_redacted"
	got := redactSecretText(input)
	if strings.Contains(got, "ghp_this_token_value_is_redacted") || strings.Contains(got, "github_pat_this_token_value_is_redacted") {
		t.Fatalf("token leaked: %q", got)
	}
	if want := "https://x-access-token:<redacted>@github.com/example-org/my-app.git"; !strings.Contains(got, want) {
		t.Fatalf("embedded URL was not preserved safely: %q", got)
	}
	if !strings.Contains(got, "error=<redacted>") {
		t.Fatalf("embedded error token was not redacted: %q", got)
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
		t.Fatalf("call count=%d, want 1", len(r.calls))
	}
	return r.calls[0]
}
