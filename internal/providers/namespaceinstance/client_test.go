package namespaceinstance

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type recordingRunner struct {
	results []LocalCommandResult
	errs    []error
	calls   []LocalCommandRequest
}

type createErrorArtifactRunner struct {
	createID          string
	calls             []LocalCommandRequest
	sawCreateDeadline bool
}

func TestCreateInstanceBuildsFakeableNSCCommandAndParsesSSH(t *testing.T) {
	runner := &recordingRunner{results: []LocalCommandResult{
		{Stdout: `{"id":"inst-synthetic","status":"running","ssh":{"host":"203.0.113.10","user":"root","port":2222},"labels":{"crabbox":"true","provider":"namespace-instance","lease":"cbx_test"}}`},
	}}
	client, err := newNSCClient(core.Config{NamespaceInstance: core.NamespaceInstanceConfig{
		Endpoint: "https://namespace.example.test",
		Region:   "us-test",
		Keychain: "kc",
	}}, core.Runtime{Exec: runner})
	if err != nil {
		t.Fatal(err)
	}
	instance, err := client.CreateInstance(context.Background(), createInstanceRequest{
		MachineType:   "linux-small",
		Duration:      2 * time.Hour,
		Ephemeral:     true,
		PublicKeyPath: "/tmp/key.pub",
		UniqueTag:     "crabbox-cbx-test",
		Labels:        map[string]string{"crabbox": "true", "provider": "namespace-instance", "lease": "cbx_test"},
		Volumes:       []string{"cache:tag:/cache:10Gi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if instance.ID != "inst-synthetic" || instance.SSHHost != "203.0.113.10" || instance.SSHUser != "root" || instance.SSHPort != "2222" {
		t.Fatalf("instance=%#v", instance)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%d", len(runner.calls))
	}
	args := runner.calls[0].Args
	for _, want := range []string{"--endpoint", "https://namespace.example.test", "--region", "us-test", "--keychain", "kc", "create", "--machine_type", "linux-small", "--duration", "2h0m0s", "--ephemeral", "--ssh_key", "/tmp/key.pub", "--unique_tag", "crabbox-cbx-test", "--volume", "cache:tag:/cache:10Gi", "-o", "json"} {
		if !containsArg(args, want) {
			t.Fatalf("args missing %q: %#v", want, args)
		}
	}
	if !containsArg(args, "--label") || !containsArg(args, "provider=namespace-instance") {
		t.Fatalf("label args missing: %#v", args)
	}
}

func TestCreateInstanceReturnsCIDFileIDOnCommandError(t *testing.T) {
	runner := &createErrorArtifactRunner{createID: "inst-created"}
	client, err := newNSCClient(core.Config{}, core.Runtime{Exec: runner})
	if err != nil {
		t.Fatal(err)
	}
	instance, err := client.CreateInstance(context.Background(), createInstanceRequest{
		MachineType:   "linux-small",
		Duration:      time.Hour,
		PublicKeyPath: "/tmp/key.pub",
	})
	if err == nil {
		t.Fatal("expected create error")
	}
	if instance.ID != "inst-created" {
		t.Fatalf("instance=%#v", instance)
	}
	if !runner.sawCreateDeadline {
		t.Fatal("create did not receive an extended command deadline")
	}
}

func TestResolveSSHRequiresNormalTarget(t *testing.T) {
	client := &nscClient{}
	_, err := client.ResolveSSH(namespaceInstance{ID: "inst-synthetic"}, core.Config{}, "/tmp/key")
	if err == nil || !strings.Contains(err.Error(), "plan_gap") {
		t.Fatalf("err=%v", err)
	}
	target, err := client.ResolveSSH(namespaceInstance{ID: "inst-synthetic", SSHHost: "203.0.113.10", SSHUser: "ubuntu", SSHPort: "2202"}, core.Config{}, "/tmp/key")
	if err != nil {
		t.Fatal(err)
	}
	if target.Host != "203.0.113.10" || target.User != "ubuntu" || target.Port != "2202" || target.Key != "/tmp/key" {
		t.Fatalf("target=%#v", target)
	}
}

func TestParseNSCInstancesFiltersWrappedShapes(t *testing.T) {
	instances, err := parseNSCInstances(`{"instances":[{"instance_id":"inst-one","ssh_host":"203.0.113.10"},{"id":"inst-two","ssh":{"endpoint":"198.51.100.20","username":"root","port":22}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 || instances[0].ID != "inst-one" || instances[0].SSHHost != "203.0.113.10" || instances[1].SSHHost != "198.51.100.20" {
		t.Fatalf("instances=%#v", instances)
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func argAfter(args []string, flag string) string {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
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

func (r *createErrorArtifactRunner) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if !containsArg(req.Args, "create") {
		return LocalCommandResult{Stdout: "[]"}, nil
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) > 15*time.Minute {
		r.sawCreateDeadline = true
	}
	if path := argAfter(req.Args, "--cidfile"); path != "" {
		if err := os.WriteFile(path, []byte(r.createID+"\n"), 0o600); err != nil {
			return LocalCommandResult{ExitCode: 1}, err
		}
	}
	return LocalCommandResult{ExitCode: 124}, errors.New("nsc create timed out after allocating instance")
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
