package external

import (
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestExternalLeaseCommandRoutingArgsSafeFlagFallback(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SCREEN_SHARING_PASSWORD", "operator-secret")
	cfg := core.Config{Provider: "external"}
	cfg.External.Command = "/usr/local/bin/provider"
	cfg.External.WorkRoot = "/work/crabbox"
	cfg.External.Capabilities.IdempotentLeaseID = true
	cfg.External.Connection.Desktop.Username = "screen-user"
	cfg.External.Connection.Desktop.PasswordEnv = "SCREEN_SHARING_PASSWORD"
	got := (Provider{}).CommandRouting(cfg, core.CommandRoutingRequest{LeaseID: "cbx_abcdef123456", Purpose: core.CommandRoutingRescue}).Args
	want := []string{
		"--external-command", "/usr/local/bin/provider",
		"--external-work-root", "/work/crabbox",
		"--external-idempotent-lease-id=true",
		"--external-desktop-username", "screen-user",
		"--external-desktop-password-env", "SCREEN_SHARING_PASSWORD",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routing args=%#v, want %#v", got, want)
	}
	if strings.Contains(strings.Join(got, " "), "operator-secret") {
		t.Fatalf("routing args leaked desktop password: %#v", got)
	}
}

func TestExternalLeaseCommandRoutingArgsKeepComplexStateOffArgv(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const leaseID = "cbx_abcdef123456"
	cfg := core.Config{Provider: "external"}
	cfg.External.Command = "provider-command"
	cfg.External.Args = []string{"--token", "adapter-secret"}
	cfg.External.Config = map[string]any{"token": "config-secret"}
	got := (Provider{}).CommandRouting(cfg, core.CommandRoutingRequest{LeaseID: leaseID, Purpose: core.CommandRoutingRescue}).Args
	routingPath, err := core.ExternalRoutingPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--external-routing-file", routingPath, "--external-routing-digest", ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routing args=%#v, want fail-closed %#v", got, want)
	}
	joined := strings.Join(got, " ")
	for _, secret := range []string{"adapter-secret", "config-secret"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("routing args leaked %q: %#v", secret, got)
		}
	}
}

func TestExternalLeaseCommandRoutingArgsPreserveExplicitDesktopCredentialClears(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const leaseID = "cbx_abcdef123456"
	routing := core.ExternalConfig{Command: "provider-command", WorkRoot: "/work/crabbox"}
	routing.Connection.Desktop = core.ExternalDesktopConfig{
		Username:    "stored-user",
		PasswordEnv: "STORED_DESKTOP_PASSWORD",
	}
	path, err := core.PersistExternalRouting(leaseID, routing)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := core.LoadExternalRouting(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := core.Config{Provider: "external", External: loaded}
	cfg.External.Connection.Desktop = core.ExternalDesktopConfig{}
	core.MarkExternalDesktopUsernameExplicit(&cfg)
	core.MarkExternalDesktopPasswordEnvExplicit(&cfg)

	got := (Provider{}).CommandRouting(cfg, core.CommandRoutingRequest{LeaseID: leaseID, Purpose: core.CommandRoutingRescue}).Args
	want := []string{
		"--external-routing-file", path,
		"--external-routing-digest", core.ExternalRoutingDigest(loaded),
		"--external-desktop-username", "",
		"--external-desktop-password-env", "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routing args=%#v, want %#v", got, want)
	}
	route := core.CommandRoutingFor(cfg, leaseID, core.CommandRoutingRescue)
	command := route.ShellCommand(append([]string{"crabbox", "desktop", "doctor"}, route.Args...))
	for _, clear := range []string{
		"--external-desktop-username ''",
		"--external-desktop-password-env ''",
	} {
		if !strings.Contains(command, clear) {
			t.Fatalf("rescue command lost explicit clear %q: %s", clear, command)
		}
	}
}

func TestExternalCommandRoutingPurposePreservesPrivateStateContract(t *testing.T) {
	isolateCrabboxState(t)
	cfg := core.Config{Provider: "exec-provider", External: core.ExternalConfig{Command: "provider-command", WorkRoot: "/work/crabbox"}}
	for _, purpose := range []core.CommandRoutingPurpose{core.CommandRoutingReconnect, core.CommandRoutingRescue, core.CommandRoutingRetry, core.CommandRoutingStop} {
		route := core.CommandRoutingFor(cfg, "cbx_abcdef123456", purpose)
		args := strings.Join(route.Args, " ")
		if purpose == core.CommandRoutingRescue {
			if !strings.Contains(args, "--external-command provider-command") {
				t.Fatalf("rescue lost safe fallback: %v", route.Args)
			}
		} else {
			if !strings.Contains(args, "--external-routing-file") || strings.Contains(args, "provider-command") || route.Args[len(route.Args)-1] != "" {
				t.Fatalf("%s lost fail-closed private route: %v", purpose, route.Args)
			}
		}
		if route.Args[1] != "external" {
			t.Fatalf("alias not canonical: %v", route.Args)
		}
	}
	path, err := core.PersistExternalRouting("cbx_abcdef123457", cfg.External)
	if err != nil {
		t.Fatal(err)
	}
	cfg.External.RoutingFile = path
	for _, purpose := range []core.CommandRoutingPurpose{core.CommandRoutingReconnect, core.CommandRoutingRescue, core.CommandRoutingRetry, core.CommandRoutingStop} {
		route := core.CommandRoutingFor(cfg, "cbx_abcdef123456", purpose)
		if !strings.Contains(strings.Join(route.Args, " "), "--external-routing-file "+path) {
			t.Fatalf("%s ignored explicit routing path: %v", purpose, route.Args)
		}
	}
}
