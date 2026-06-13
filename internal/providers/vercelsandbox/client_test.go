package vercelsandbox

import (
	"context"
	"errors"
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
