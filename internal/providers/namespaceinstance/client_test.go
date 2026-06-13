package namespaceinstance

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type recordingRunner struct {
	results []LocalCommandResult
	errs    []error
	calls   []LocalCommandRequest
}

func (r *recordingRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	idx := len(r.calls) - 1
	var result LocalCommandResult
	if idx < len(r.results) {
		result = r.results[idx]
	}
	var err error
	if idx < len(r.errs) {
		err = r.errs[idx]
	}
	return result, err
}

func TestCheckReadinessUsesNonMutatingNSCCommands(t *testing.T) {
	runner := &recordingRunner{results: []LocalCommandResult{
		{},
		{},
		{Stdout: `[{"name":"one"},{"name":"two"}]`},
	}}
	client, err := newNSCClient(core.Config{NamespaceInstance: core.NamespaceInstanceConfig{
		Endpoint: "https://namespace.example.test",
		Keychain: "test-keychain",
		Region:   "us-west",
	}}, core.Runtime{Exec: runner})
	if err != nil {
		t.Fatal(err)
	}
	count, err := client.CheckReadiness(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != "2" {
		t.Fatalf("count=%q", count)
	}
	got := make([][]string, 0, len(runner.calls))
	for _, call := range runner.calls {
		if call.Name != "nsc" {
			t.Fatalf("Name=%q", call.Name)
		}
		got = append(got, call.Args)
		joined := strings.Join(call.Args, " ")
		for _, mutating := range []string{"create", "destroy", "extend"} {
			if strings.Contains(joined, mutating) {
				t.Fatalf("doctor used mutating command %q in %v", mutating, call.Args)
			}
		}
	}
	want := [][]string{
		{"--endpoint", "https://namespace.example.test", "--keychain", "test-keychain", "--region", "us-west", "--help"},
		{"--endpoint", "https://namespace.example.test", "--keychain", "test-keychain", "--region", "us-west", "auth", "check-login"},
		{"--endpoint", "https://namespace.example.test", "--keychain", "test-keychain", "--region", "us-west", "list", "-o", "json"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls=%#v want %#v", got, want)
	}
}

func TestCheckReadinessRedactsCommandFailureOutput(t *testing.T) {
	secret := "workspace-123 instance-456 login-code"
	runner := &recordingRunner{
		results: []LocalCommandResult{{}, {ExitCode: 1, Stderr: secret}},
		errs:    []error{nil, errors.New(secret)},
	}
	client, err := newNSCClient(core.Config{}, core.Runtime{Exec: runner})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CheckReadiness(context.Background())
	if err == nil {
		t.Fatal("expected auth error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked command output: %v", err)
	}
	if !strings.Contains(err.Error(), "auth check-login") {
		t.Fatalf("error=%v", err)
	}
}

func TestParseNSCListCountShapes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{name: "empty", in: "", want: 0},
		{name: "null", in: "null", want: 0},
		{name: "array", in: `[{},{}]`, want: 2},
		{name: "instances", in: `{"instances":[{},{}]}`, want: 2},
		{name: "items", in: `{"items":[{}]}`, want: 1},
		{name: "results", in: `{"results":[{},{},{}]}`, want: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNSCListCount(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("count=%d want %d", got, tt.want)
			}
		})
	}
}

func TestNewClientRequiresRunner(t *testing.T) {
	_, err := newNSCClient(core.Config{}, core.Runtime{})
	if err == nil || !strings.Contains(err.Error(), "requires a local command runner") {
		t.Fatalf("err=%v", err)
	}
}
