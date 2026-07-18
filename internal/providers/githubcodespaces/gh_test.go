package githubcodespaces

import (
	"context"
	"fmt"
	"slices"
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
	wantArgs := []string{"codespace", "ssh", "--config", "-c", "sturdy-space"}
	if call.Name != "/opt/gh" || !slices.Equal(call.Args, wantArgs) {
		t.Fatalf("command=%q args=%q", call.Name, call.Args)
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
	if want := []string{"auth", "status", "--active", "--hostname", "github.com"}; !slices.Equal(call.Args, want) {
		t.Fatalf("args=%q", call.Args)
	}
}

func TestGHRunnerAuthStatusFallsBackForOlderCLI(t *testing.T) {
	runner := &recordingRunner{results: []recordingResult{
		{result: LocalCommandResult{ExitCode: 1, Stderr: "unknown flag: --active"}},
		{},
	}}
	gh := newGHRunner(GitHubCodespacesConfig{GHPath: "gh"}, Runtime{Exec: runner})
	if err := gh.authStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls=%#v", runner.calls)
	}
	if want := []string{"auth", "status", "--active", "--hostname", "github.com"}; !slices.Equal(runner.calls[0].Args, want) {
		t.Fatalf("first args=%q", runner.calls[0].Args)
	}
	if want := []string{"auth", "status", "--hostname", "github.com"}; !slices.Equal(runner.calls[1].Args, want) {
		t.Fatalf("fallback args=%q", runner.calls[1].Args)
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
	if want := []string{"auth", "token", "--hostname", "api.enterprise.example:8443"}; !slices.Equal(runner.calls[0].Args, want) {
		t.Fatalf("auth args=%q", runner.calls[0].Args)
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
	t.Setenv("GH_TOKEN", "dotcom-test-token")
	t.Setenv("GH_ENTERPRISE_TOKEN", "enterprise-test-token")
	runner := &recordingRunner{result: LocalCommandResult{Stdout: "token"}}
	gh := newGHRunner(GitHubCodespacesConfig{GHPath: "gh", APIURL: "https://api.octocorp.ghe.com"}, Runtime{Exec: runner})
	if _, err := gh.authToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	call := runner.onlyCall(t)
	if want := []string{"auth", "token", "--hostname", "octocorp.ghe.com"}; !slices.Equal(call.Args, want) {
		t.Fatalf("args=%q", call.Args)
	}
	foundHost := false
	foundDotcomToken := false
	foundEnterpriseToken := false
	for _, entry := range call.Env {
		if entry == "GH_HOST=octocorp.ghe.com" {
			foundHost = true
		}
		if entry == "GH_TOKEN=dotcom-test-token" {
			foundDotcomToken = true
		}
		if strings.HasPrefix(entry, "GH_ENTERPRISE_TOKEN=") || strings.HasPrefix(entry, "GITHUB_ENTERPRISE_TOKEN=") {
			foundEnterpriseToken = true
		}
	}
	if !foundHost || !foundDotcomToken || foundEnterpriseToken {
		t.Fatalf("host=%t dotcom_token=%t enterprise_token=%t", foundHost, foundDotcomToken, foundEnterpriseToken)
	}
}

func TestGHRunnerMapsGHECloudDefaultHTTPSPortToPortlessCLIHost(t *testing.T) {
	runner := &recordingRunner{result: LocalCommandResult{Stdout: "value"}}
	gh := newGHRunner(GitHubCodespacesConfig{GHPath: "gh", APIURL: "https://api.octocorp.ghe.com:443"}, Runtime{Exec: runner})
	if _, err := gh.authToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	call := runner.onlyCall(t)
	if want := []string{"auth", "token", "--hostname", "octocorp.ghe.com"}; !slices.Equal(call.Args, want) {
		t.Fatalf("args=%q", call.Args)
	}
	if !slices.Contains(call.Env, "GH_HOST=octocorp.ghe.com") {
		t.Fatalf("env=%q", call.Env)
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

func TestRedactSecretTextRedactsStatelessInstallationToken(t *testing.T) {
	stateless := "ghs_" + strings.Repeat("1", 10) + "_" + strings.Repeat("a", 16) + "." + strings.Repeat("b", 16) + "." + strings.Repeat("c", 16)
	got := redactSecretText("denied " + stateless)
	if strings.Contains(got, stateless) || got != "denied <redacted>" {
		t.Fatalf("stateless installation token leaked: %q", got)
	}
}

type recordingRunner struct {
	calls   []LocalCommandRequest
	result  LocalCommandResult
	err     error
	results []recordingResult
}

type recordingResult struct {
	result LocalCommandResult
	err    error
}

func (r *recordingRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if len(r.results) > 0 {
		result := r.results[0]
		r.results = r.results[1:]
		return result.result, result.err
	}
	return r.result, r.err
}

func (r *recordingRunner) onlyCall(t *testing.T) LocalCommandRequest {
	t.Helper()
	if len(r.calls) != 1 {
		t.Fatalf("call count=%d, want 1", len(r.calls))
	}
	return r.calls[0]
}
