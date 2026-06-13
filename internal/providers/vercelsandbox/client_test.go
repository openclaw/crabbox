package vercelsandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
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

func TestCheckProjectAcceptsProjectScopeOrTeam(t *testing.T) {
	projectCfg := Config{}
	projectCfg.VercelSandbox.ProjectID = "prj_123"
	teamCfg := Config{}
	teamCfg.VercelSandbox.TeamID = "team_123"
	scopeCfg := Config{}
	scopeCfg.VercelSandbox.Scope = "example-org"
	for _, cfg := range []Config{projectCfg, teamCfg, scopeCfg} {
		client := &bridgeClient{cfg: cfg}
		if err := client.CheckProject(context.Background()); err != nil {
			t.Fatalf("CheckProject(%#v): %v", cfg.VercelSandbox, err)
		}
	}
	client := &bridgeClient{}
	if err := client.CheckProject(context.Background()); err == nil {
		t.Fatal("CheckProject without project/team/scope succeeded")
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
