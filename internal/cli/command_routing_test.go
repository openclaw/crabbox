package cli

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

type purposeRoutingTestProvider struct{ testAWSProvider }

func (purposeRoutingTestProvider) Name() string      { return "purpose-routing-test" }
func (purposeRoutingTestProvider) Aliases() []string { return []string{"purpose-routing-alias"} }
func (p purposeRoutingTestProvider) Spec() ProviderSpec {
	spec := p.testAWSProvider.Spec()
	spec.Name = p.Name()
	spec.Family = "routing-test"
	return spec
}
func (purposeRoutingTestProvider) CommandRouting(_ Config, request CommandRoutingRequest) CommandRouting {
	return CommandRouting{Args: []string{"--purpose", string(request.Purpose), "--context", "dev=west"}, Env: []string{"ROUTE_CONTEXT=/tmp/base file:/tmp/cluster"}}
}
func init() { RegisterProvider(purposeRoutingTestProvider{}) }

func TestCommandRoutingShellEnvironmentIsNotArgv(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("POSIX shell unavailable")
	}
	route := CommandRoutingFor(Config{Provider: "purpose-routing-alias"}, "lease", CommandRoutingStop)
	command := route.ShellCommand(append([]string{"sh", "-c", `printf '%s\n' "$ROUTE_CONTEXT" "$@"`, "routing-test"}, route.Args...))
	out, err := exec.Command("sh", "-c", command).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := append([]string{"/tmp/base file:/tmp/cluster"}, route.Args...)
	if got := strings.Split(strings.TrimSpace(string(out)), "\n"); !reflect.DeepEqual(got, want) {
		t.Fatalf("shell env/argv=%v want %v", got, want)
	}
	if route.Args[1] != "purpose-routing-test" {
		t.Fatalf("alias not canonical: %v", route.Args)
	}
}

func TestCommandRoutingPurposesReachRescueAndWebVNC(t *testing.T) {
	cfg := Config{Provider: "purpose-routing-test"}
	if got := runStopCommand(cfg, "lease"); !strings.Contains(got, "--purpose stop") || !strings.HasPrefix(got, "ROUTE_CONTEXT='") {
		t.Fatalf("stop=%s", got)
	}
	if got := desktopDoctorCommand(rescueContext{Cfg: cfg, LeaseID: "lease"}); !strings.Contains(got, "--purpose rescue") || !strings.HasPrefix(got, "ROUTE_CONTEXT='") {
		t.Fatalf("rescue=%s", got)
	}
	route := webVNCBridgeRouting(cfg, SSHTarget{}, "lease", false, false)
	if !strings.Contains(strings.Join(route.Args, " "), "--purpose reconnect") {
		t.Fatalf("bridge=%v", route)
	}
	env := webVNCDaemonChildEnvironment(append(os.Environ(), route.Env...), route.Args)
	if got, ok := childEnvironmentValue(env, "ROUTE_CONTEXT"); !ok || got != "/tmp/base file:/tmp/cluster" {
		t.Fatalf("daemon lost environment: %q %t", got, ok)
	}
	for _, arg := range route.Args {
		if strings.HasPrefix(arg, "ROUTE_CONTEXT=") {
			t.Fatalf("assignment in daemon argv: %v", route.Args)
		}
	}
}

func TestCommandRoutingURLExcludesCredentialParameters(t *testing.T) {
	for _, raw := range []string{
		"https://user:pass@api.example/root?view=1&auth=secret&api%5Fkey=secret#secret",
		"user:pass@api.example/root?view=1&auth=secret&signature=secret#secret",
		"https://user:pass@api.example/%zz?token=secret#secret",
		"user:pass@api.example/root?view=https://docs.example",
		"user:pass@api.example/root#https://docs.example",
		"user:pass@api.example/path/https://docs.example",
		"//user:pass@api.example/root?view=1",
	} {
		got := RoutingSafeURL(raw)
		for _, secret := range []string{"user:", "pass", "secret"} {
			if strings.Contains(got, secret) {
				t.Fatalf("URL leaked %q: %q", secret, got)
			}
		}
		if strings.Contains(raw, "view=1") && !strings.Contains(got, "view=1") {
			t.Fatalf("lost safe query: %q", got)
		}
	}
}

func TestCommandRoutingURLPreservesNonSecretSpelling(t *testing.T) {
	for _, raw := range []string{
		"HTTPS://API.EXAMPLE/graphql/?view=%2f",
		"https://API.EXAMPLE/?",
		"API.EXAMPLE/root/?",
		"//API.EXAMPLE/root/?",
	} {
		if got := RoutingSafeURL(raw); got != raw {
			t.Fatalf("endpoint changed: got %q want %q", got, raw)
		}
	}
	if got := RoutingSafeURL("HTTPS://API.EXAMPLE/root?api_key=secret&view=%2f#secret"); got != "HTTPS://API.EXAMPLE/root?view=%2f" {
		t.Fatalf("redaction changed endpoint spelling: %q", got)
	}
	if got := RoutingSafeURL("user:pass@api.example/root?view=https://docs.example"); got != "api.example/root?view=https://docs.example" {
		t.Fatalf("query scheme affected authority redaction: %q", got)
	}
}

func (purposeRoutingTestProvider) ClaimScope(Config) string { return " opaque routing identity " }

func TestFailureDigestUsesStopPurposeWithoutTimingReport(t *testing.T) {
	cfg := Config{Provider: "purpose-routing-test"}
	commands := failureDigestNextCommands(runFailureDigestInput{
		Provider: cfg.Provider, LeaseID: "lease", CommandDisplay: "true",
		Routing:     CommandRoutingFor(cfg, "lease", CommandRoutingRetry),
		SSHRouting:  CommandRoutingFor(cfg, "lease", CommandRoutingRetry),
		StopRouting: CommandRoutingFor(cfg, "lease", CommandRoutingStop),
	}, "unknown")
	if len(commands) != 3 || !strings.Contains(commands[0], "--purpose retry") || !strings.Contains(commands[1], "--purpose retry") || !strings.Contains(commands[2], "--purpose stop") {
		t.Fatalf("failure purposes=%v", commands)
	}
	for _, command := range commands {
		if !strings.HasPrefix(command, "ROUTE_CONTEXT='") {
			t.Fatalf("lost routing environment: %s", command)
		}
	}
}
