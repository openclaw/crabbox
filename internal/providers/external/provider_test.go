package external

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func testConfig() core.Config {
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Command:  "provider-command",
		Args:     []string{"--profile", "test"},
		Config:   map[string]any{"namespace": "dev", "cpu": 32},
		WorkRoot: "/home/tester/crabbox",
	}
	return cfg
}

func TestProviderSpec(t *testing.T) {
	spec := (Provider{}).Spec()
	if spec.Name != providerName || spec.Family != "external" {
		t.Fatalf("spec=%#v", spec)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureDesktop, core.FeatureBrowser, core.FeatureCode} {
		if !spec.Features.Has(feature) {
			t.Fatalf("missing feature %s", feature)
		}
	}
}

func TestRouteConfigUsesProviderWorkRoot(t *testing.T) {
	cfg := testConfig()
	cfg.WorkRoot = core.BaseConfig().WorkRoot
	if err := (Provider{}).RouteConfig(&cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.WorkRoot != "/home/tester/crabbox" {
		t.Fatalf("work root=%q", cfg.WorkRoot)
	}
}

func TestFlagsOverrideArgsAndConfigJSON(t *testing.T) {
	cfg := testConfig()
	fs := flag.NewFlagSet("external", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--external-arg=/tmp/new provider.mjs",
		"--external-arg=--profile",
		"--external-config-json", `{"namespace":"prod","cpu":64}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.External.Args, "|") != "/tmp/new provider.mjs|--profile" {
		t.Fatalf("args=%#v", cfg.External.Args)
	}
	if cfg.External.Config["namespace"] != "prod" || cfg.External.Config["cpu"] != float64(64) {
		t.Fatalf("config=%#v", cfg.External.Config)
	}
}

func TestFlagHelpDoesNotExposeLoadedArgsOrConfig(t *testing.T) {
	cfg := testConfig()
	cfg.External.Args = []string{"--token", "secret-arg"}
	cfg.External.Config = map[string]any{"token": "secret-config"}
	fs := flag.NewFlagSet("external", flag.ContinueOnError)
	var output bytes.Buffer
	fs.SetOutput(&output)
	registerFlags(fs, cfg)
	fs.PrintDefaults()
	for _, secret := range []string{"secret-arg", "secret-config"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("help leaked %q:\n%s", secret, output.String())
		}
	}
}

func TestInvokeSendsVersionedJSONRequest(t *testing.T) {
	runner := &recordingRunner{stdout: `{"protocolVersion":1,"message":"ready"}`}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	response, err := backend.invoke(context.Background(), protocolRequest{Operation: "doctor"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message != "ready" {
		t.Fatalf("response=%#v", response)
	}
	if runner.name != "provider-command" || strings.Join(runner.args, " ") != "--profile test" {
		t.Fatalf("command=%q args=%#v", runner.name, runner.args)
	}
	var request protocolRequest
	if err := json.Unmarshal(runner.stdin, &request); err != nil {
		t.Fatal(err)
	}
	if request.ProtocolVersion != 1 || request.Operation != "doctor" || request.Config["namespace"] != "dev" {
		t.Fatalf("request=%#v", request)
	}
}

func TestInvokeRejectsUnversionedResponse(t *testing.T) {
	runner := &recordingRunner{stdout: `{}`}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if _, err := backend.invoke(context.Background(), protocolRequest{Operation: "doctor"}); err == nil || !strings.Contains(err.Error(), "protocol version 0") {
		t.Fatalf("err=%v", err)
	}
}

func TestProtocolLeaseMapsProxyAndServer(t *testing.T) {
	cfg := testConfig()
	lease := protocolLease{
		LeaseID:    "cbx_000000000123",
		Slug:       "test",
		Name:       "devbox-test",
		Status:     "running",
		ServerType: "cpu32",
		SSH: &protocolSSH{
			User:           "tester",
			Host:           "devbox-test",
			Port:           "22",
			SSHConfigProxy: true,
			ProxyCommand:   "provider proxy %h %p",
		},
	}.target(cfg, true)
	if lease.Server.Provider != providerName || lease.Server.ServerType.Name != "cpu32" {
		t.Fatalf("server=%#v", lease.Server)
	}
	if lease.Server.Labels["name"] != "devbox-test" {
		t.Fatalf("labels=%#v", lease.Server.Labels)
	}
	if !lease.SSH.SSHConfigProxy || lease.SSH.ProxyCommand != "provider proxy %h %p" {
		t.Fatalf("ssh=%#v", lease.SSH)
	}
}

func TestLeaseSlugForClaimUsesProviderReturnedSlug(t *testing.T) {
	lease := protocolLease{
		LeaseID: "provider-id",
		Slug:    "provider-slug",
		Name:    "provider-name",
	}.target(testConfig(), false)
	if got := leaseSlugForClaim(lease, "requested-slug"); got != "provider-slug" {
		t.Fatalf("slug=%q", got)
	}
}

func TestDoctorExecutesProviderAsChildProcess(t *testing.T) {
	cfg := testConfig()
	cfg.External.Command = os.Args[0]
	cfg.External.Args = []string{"-test.run=TestExternalProviderHelperProcess", "--"}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: processRunner{}}}
	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Message != "child process ready" {
		t.Fatalf("result=%#v", result)
	}
}

func TestAcquireReleasesInvalidLeaseResponse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1,"lease":{"name":"created-without-ssh"}}`,
		`{"protocolVersion":1}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "invalid", Keep: false})
	if err == nil || !strings.Contains(err.Error(), "SSH host and user are required") {
		t.Fatalf("err=%v", err)
	}
	if len(runner.operations) != 2 || runner.operations[0] != "acquire" || runner.operations[1] != "release" {
		t.Fatalf("operations=%#v", runner.operations)
	}
}

func TestResolveRejectsReplacementLeaseIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	if err := core.ClaimLeaseForRepoProvider("cbx_000000000001", "shared", providerName, repo, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	server := core.Server{Name: "devbox-shared", Labels: map[string]string{"name": "devbox-shared", "slug": "shared"}}
	if err := core.UpdateLeaseClaimEndpoint("cbx_000000000001", server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1,"lease":{"leaseId":"cbx_000000000002","slug":"shared","name":"devbox-shared"}}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "shared", ReleaseOnly: true}); err == nil || !strings.Contains(err.Error(), "lease identity changed") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveRejectsLeaseWithoutStableIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1,"lease":{"slug":"shared","name":"devbox-shared"}}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "shared", ReleaseOnly: true}); err == nil || !strings.Contains(err.Error(), "no stable leaseId") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveRejectsNonCanonicalLeaseID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1,"lease":{"leaseId":"../../outside","slug":"shared","name":"devbox-shared"}}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "shared"}); err == nil || !strings.Contains(err.Error(), "cbx_") {
		t.Fatalf("err=%v", err)
	}
}

func TestReleaseAllowsLegacyProviderLeaseID(t *testing.T) {
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	lease := core.LeaseTarget{LeaseID: "provider-id", Server: core.Server{Name: "legacy-devbox"}}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(runner.operations) != 1 || runner.operations[0] != "release" {
		t.Fatalf("operations=%#v", runner.operations)
	}
}

func TestResolvePersistsRoutingBeforeSSHReadiness(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1,"lease":{"leaseId":"cbx_abcdef123456","slug":"shared","name":"devbox-shared","ssh":{"host":"127.0.0.1","user":"tester","port":"1"}}}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.Resolve(ctx, core.ResolveRequest{ID: "shared"}); err == nil {
		t.Fatal("expected canceled SSH readiness")
	}
	path, err := core.ExternalRoutingPath("cbx_abcdef123456")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("routing state missing: %v", err)
	}
}

func TestResolvePreservesClaimedLifecycleLabels(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	if err := core.ClaimLeaseForRepoProvider("cbx_000000000003", "ephemeral", providerName, repo, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	server := core.Server{Name: "devbox-ephemeral", Labels: map[string]string{
		"name":         "devbox-ephemeral",
		"slug":         "ephemeral",
		"keep":         "false",
		"created_at":   "100",
		"expires_at":   "200",
		"ttl_secs":     "100",
		"idle_timeout": "50",
	}}
	if err := core.UpdateLeaseClaimEndpoint("cbx_000000000003", server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1,"lease":{"leaseId":"cbx_000000000003","slug":"ephemeral","name":"devbox-ephemeral"}}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "ephemeral", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Labels["keep"] != "false" || lease.Server.Labels["created_at"] != "100" || lease.Server.Labels["expires_at"] != "200" {
		t.Fatalf("labels=%#v", lease.Server.Labels)
	}
}

func TestCleanupReconcilesExternalClaims(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	if err := core.ClaimLeaseForRepoProvider("cbx_000000000004", "live", providerName, repo, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := core.ClaimLeaseForRepoProvider("cbx_000000000005", "stale", providerName, repo, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1}`,
		`{"protocolVersion":1,"leases":[{"leaseId":"cbx_000000000004","slug":"live","name":"live"}]}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("live", providerName); err != nil || !ok {
		t.Fatalf("live claim ok=%v err=%v", ok, err)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("stale", providerName); err != nil || ok {
		t.Fatalf("stale claim ok=%v err=%v", ok, err)
	}
}

func TestCleanupRejectsMalformedInventoryBeforeRemovingClaims(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := core.ClaimLeaseForRepoProvider("cbx_000000000006", "live", providerName, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1}`,
		`{"protocolVersion":1,"leases":[{"name":"missing-id"}]}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err == nil || !strings.Contains(err.Error(), "missing leaseId") {
		t.Fatalf("err=%v", err)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("live", providerName); err != nil || !ok {
		t.Fatalf("claim removed ok=%v err=%v", ok, err)
	}
}

func TestExternalProviderHelperProcess(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "TestExternalProviderHelperProcess") {
		return
	}
	var request protocolRequest
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
		os.Exit(2)
	}
	if request.ProtocolVersion != protocolVersion || request.Operation != "doctor" || request.Config["namespace"] != "dev" {
		os.Exit(3)
	}
	_, _ = io.WriteString(os.Stdout, `{"protocolVersion":1,"message":"child process ready"}`)
	os.Exit(0)
}

type recordingRunner struct {
	name   string
	args   []string
	stdin  []byte
	stdout string
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.name = req.Name
	r.args = append([]string(nil), req.Args...)
	if req.Stdin != nil {
		r.stdin, _ = io.ReadAll(req.Stdin)
	}
	return core.LocalCommandResult{Stdout: r.stdout}, nil
}

type processRunner struct{}

func (processRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	cmd := exec.CommandContext(ctx, req.Name, req.Args...)
	cmd.Stdin = req.Stdin
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return core.LocalCommandResult{Stdout: stdout.String(), Stderr: stderr.String()}, err
}

type sequenceRunner struct {
	responses  []string
	operations []string
}

func (r *sequenceRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	var request protocolRequest
	if err := json.NewDecoder(req.Stdin).Decode(&request); err != nil {
		return core.LocalCommandResult{}, err
	}
	r.operations = append(r.operations, request.Operation)
	response := r.responses[0]
	r.responses = r.responses[1:]
	return core.LocalCommandResult{Stdout: response}, nil
}
