package phala

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type call struct {
	name string
	args []string
}

type fakeRunner struct {
	calls   []call
	results []core.LocalCommandResult
	errs    []error
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func (r *fakeRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, call{name: req.Name, args: append([]string(nil), req.Args...)})
	result := r.results[0]
	r.results = r.results[1:]
	var err error
	if len(r.errs) > 0 {
		err = r.errs[0]
		r.errs = r.errs[1:]
	}
	return result, err
}

func TestProviderContract(t *testing.T) {
	provider := Provider{}
	if provider.Name() != providerName || !reflect.DeepEqual(provider.Aliases(), []string{"phala-cloud", "dstack"}) {
		t.Fatalf("provider identity=%q aliases=%v", provider.Name(), provider.Aliases())
	}
	spec := provider.Spec()
	if spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever ||
		!spec.Features.Has(core.FeatureSSH) || !spec.Features.Has(core.FeatureCrabboxSync) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("spec=%#v", spec)
	}
}

func TestProviderRejectsNonLinuxInstanceTypeAndUnsafeWorkRoot(t *testing.T) {
	provider := Provider{}
	for _, test := range []struct {
		name string
		cfg  core.Config
		want string
	}{
		{
			name: "non-linux instance type",
			cfg: func() core.Config {
				cfg := core.BaseConfig()
				cfg.Provider = providerName
				cfg.Phala.InstanceType = "darwin/arm64:tdx.small"
				return cfg
			}(),
			want: "supports Linux instance types only",
		},
		{
			name: "unsafe work root",
			cfg: func() core.Config {
				cfg := core.BaseConfig()
				cfg.Provider = providerName
				cfg.Phala.WorkRoot = "../../etc"
				return cfg
			}(),
			want: "canonical absolute Linux path",
		},
		{
			name: "broad work root",
			cfg: func() core.Config {
				cfg := core.BaseConfig()
				cfg.Provider = providerName
				cfg.Phala.WorkRoot = "/tmp"
				return cfg
			}(),
			want: "too broad",
		},
		{
			name: "bare writable mount work root",
			cfg: func() core.Config {
				cfg := core.BaseConfig()
				cfg.Provider = providerName
				cfg.Phala.WorkRoot = "/var/volatile"
				return cfg
			}(),
			want: "too broad",
		},
		{
			name: "compose url",
			cfg: func() core.Config {
				cfg := core.BaseConfig()
				cfg.Provider = providerName
				cfg.Phala.Compose = "https://evil.example/compose.yml"
				return cfg
			}(),
			want: "must be a local file path",
		},
		{
			name: "compose escape",
			cfg: func() core.Config {
				cfg := core.BaseConfig()
				cfg.Provider = providerName
				cfg.Phala.Compose = "../../etc/compose.yml"
				return cfg
			}(),
			want: "escape the working directory",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := provider.Configure(test.cfg, core.Runtime{}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestInstanceTypeForClass(t *testing.T) {
	for _, test := range []struct {
		class string
		want  string
	}{
		{"standard", "tdx.small"},
		{"fast", "tdx.medium"},
		{"large", "tdx.large"},
		{"beast", "tdx.xlarge"},
		{"tdx.2xlarge", "tdx.2xlarge"},
	} {
		if got := instanceTypeForClass(test.class); got != test.want {
			t.Fatalf("instanceTypeForClass(%q)=%q want %q", test.class, got, test.want)
		}
	}
}

func TestFlagsApplyPhalaOptions(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--phala-cli", "/opt/phala",
		"--phala-instance-type", "tdx.medium",
		"--phala-node-id", "node-7",
		"--phala-work-root", "/workspace",
	}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Phala.CLIPath != "/opt/phala" || cfg.ServerType != "tdx.medium" ||
		cfg.Phala.NodeID != "node-7" || cfg.Phala.WorkRoot != "/workspace" {
		t.Fatalf("cfg=%#v server_type=%q", cfg.Phala, cfg.ServerType)
	}
}

func TestCreateBuildsPhalaDeployArguments(t *testing.T) {
	runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: `{"success":true,"id":"cvm-test"}`}}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Phala = core.PhalaConfig{
		CLIPath:      "/opt/phala",
		InstanceType: "tdx.small",
		WorkRoot:     "/var/volatile/crabbox",
		NodeID:       "node-7",
		Compose:      "/srv/compose.yml",
	}
	applyDefaults(&cfg)
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	id, err := b.create(context.Background(), cfg, "/tmp/id.pub", map[string]string{
		"crabbox":  "true",
		"lease":    "cbx_abcdef123456",
		"provider": providerName,
		"slug":     "blue-box",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "cvm-test" {
		t.Fatalf("id=%q", id)
	}
	got := strings.Join(runner.calls[0].args, " ")
	for _, want := range []string{
		"deploy --json",
		"--dev-os",
		"--ssh-pubkey /tmp/id.pub",
		"--wait",
		"-n crabbox-cbx-abcdef123456",
		"-t tdx.small",
		"--node-id node-7",
		"--compose /srv/compose.yml",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("args=%q missing %q", got, want)
		}
	}
	// The API key must never be passed as a CLI flag; phala uses stored auth.
	if strings.Contains(got, "--api-token") || strings.Contains(got, "--api-key") {
		t.Fatalf("deploy leaked an API key flag: %q", got)
	}
}

func TestCreateAlwaysSuppliesComposeFlag(t *testing.T) {
	// The Phala deploy handler refuses to provision a CVM in non-interactive mode
	// without a Compose file, so deploy must always carry --compose: the
	// configured path when present, else the embedded default written into the
	// per-lease temp dir.
	for _, test := range []struct {
		name    string
		compose string
	}{
		{name: "configured compose", compose: "/srv/compose.yml"},
		{name: "default compose", compose: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: `{"success":true,"id":"cvm-test"}`}}}
			cfg := core.BaseConfig()
			cfg.Provider = providerName
			cfg.Phala.Compose = test.compose
			applyDefaults(&cfg)
			b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
			pubKey := filepath.Join(t.TempDir(), "id_ed25519.pub")
			if _, err := b.create(context.Background(), cfg, pubKey, map[string]string{
				"crabbox":  "true",
				"lease":    "cbx_abcdef123456",
				"provider": providerName,
			}); err != nil {
				t.Fatal(err)
			}
			args := runner.calls[0].args
			composePath := ""
			for i, arg := range args {
				if arg == "--compose" && i+1 < len(args) {
					composePath = args[i+1]
					break
				}
			}
			if composePath == "" {
				t.Fatalf("deploy argv missing --compose <path>: %q", strings.Join(args, " "))
			}
			if test.compose != "" {
				if composePath != test.compose {
					t.Fatalf("compose path=%q want configured %q", composePath, test.compose)
				}
				return
			}
			// The default compose must be materialized on disk and parse as the
			// minimal long-lived SSH-lease box.
			data, err := os.ReadFile(composePath)
			if err != nil {
				t.Fatalf("read default compose %q: %v", composePath, err)
			}
			body := string(data)
			for _, want := range []string{"services:", "image: debian:stable-slim", "sleep", "infinity"} {
				if !strings.Contains(body, want) {
					t.Fatalf("default compose missing %q:\n%s", want, body)
				}
			}
		})
	}
}

func TestCreateRecoversCVMAfterInvalidOutput(t *testing.T) {
	runner := &fakeRunner{results: []core.LocalCommandResult{
		{Stdout: `not-json`},
		{Stdout: `{"success":true,"items":[{"id":"recovered","name":"crabbox-cbx-abcdef123456"}]}`},
	}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	id, err := b.create(context.Background(), cfg, "/tmp/id.pub", map[string]string{
		"crabbox":    "true",
		"created_by": "crabbox",
		"lease":      "cbx_abcdef123456",
		"provider":   providerName,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "recovered" {
		t.Fatalf("id=%q", id)
	}
}

func TestCreateRecoversAfterCallerCancellation(t *testing.T) {
	runner := &fakeRunner{
		results: []core.LocalCommandResult{
			{},
			{Stdout: `{"success":true,"items":[{"id":"recovered","name":"crabbox-cbx-abcdef123456"}]}`},
		},
		errs: []error{context.Canceled},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	id, err := b.create(ctx, cfg, "/tmp/id.pub", map[string]string{
		"crabbox":    "true",
		"created_by": "crabbox",
		"lease":      "cbx_abcdef123456",
		"provider":   providerName,
	})
	if err != nil || id != "recovered" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}

func TestCreateDoesNotReconcileDefinitiveError(t *testing.T) {
	runner := &fakeRunner{
		results: []core.LocalCommandResult{{Stderr: "InvalidArgument: bad instance type"}},
		errs:    []error{errors.New("exit status 1")},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	_, err := b.create(context.Background(), cfg, "/tmp/id.pub", map[string]string{
		"crabbox":  "true",
		"lease":    "cbx_abcdef123456",
		"provider": providerName,
	})
	if err == nil || len(runner.calls) != 1 {
		t.Fatalf("err=%v calls=%#v", err, runner.calls)
	}
}

func TestListParsesItemsWrapperAndFiltersOwnership(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := &fakeRunner{results: []core.LocalCommandResult{
		{Stdout: `{"success":true,"items":[]}`},
		{Stdout: `{"success":true,"items":[
			{"id":"owned","name":"crabbox-cbx-abcdef123456","status":"running"},
			{"id":"foreign","name":"someone-elses-cvm"}
		]}`},
	}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	// The owned CVM is surfaced only when a local claim backs its lease id.
	claimPhalaLease(t, cfg, "cbx_abcdef123456", "owned")
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	items, err := b.listInstances(context.Background())
	if err != nil || len(items) != 0 {
		t.Fatalf("empty list items=%v err=%v", items, err)
	}
	views, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].DisplayID() != "owned" {
		t.Fatalf("views=%#v", views)
	}
}

func TestListExcludesNamePrefixedCVMWithoutLocalClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// A CVM that merely carries the crabbox- name prefix (owned() passes) but has
	// no backing local claim must be excluded — it could be a foreign resource.
	runner := &fakeRunner{results: []core.LocalCommandResult{
		{Stdout: `{"success":true,"items":[
			{"id":"prefixed","name":"crabbox-cbx-abcdef123456","status":"running"}
		]}`},
	}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	views, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 0 {
		t.Fatalf("unclaimed name-prefixed CVM surfaced: views=%#v", views)
	}
}

func TestListIgnoresTrailingCLINoise(t *testing.T) {
	runner := &fakeRunner{results: []core.LocalCommandResult{
		{Stdout: "{\"success\":true,\"items\":[{\"id\":\"owned\",\"name\":\"crabbox-cbx-abcdef123456\"}]}\nAssertion failed: !(handle->flags & UV_HANDLE_CLOSING)\n"},
	}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	items, err := b.listInstances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].cloudID() != "owned" {
		t.Fatalf("items=%#v", items)
	}
}

func TestDoctorUsesVersionAndStatus(t *testing.T) {
	runner := &fakeRunner{results: []core.LocalCommandResult{
		{Stdout: "v1.1.19\n"},
		{Stdout: `{"success":true,"username":"tester"}`},
		{Stdout: `{"success":true,"items":[]}`},
	}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	if _, err := b.Doctor(context.Background(), core.DoctorRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runner.calls[0].args, " "); got != "--version" {
		t.Fatalf("version args=%q", got)
	}
	if got := strings.Join(runner.calls[1].args, " "); got != "status --json" {
		t.Fatalf("status args=%q", got)
	}
}

func TestProxyCommandPreservesConfiguredScope(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Phala.CLIPath = "/Applications/Phala CLI/phala"
	cfg.Phala.NodeID = "node 7"
	got := proxyCommand(cfg, "cvm-1")
	for _, want := range []string{
		`"/Applications/Phala CLI/phala"`,
		`--node-id "node 7"`,
		"cvm-1",
		"__phala-proxy",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("proxy=%q missing %q", got, want)
		}
	}
}

func TestProxyCommandEscapesOpenSSHPercentTokens(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Phala.CLIPath = "/opt/phala%test"
	got := proxyCommand(cfg, "cvm%1")
	if !strings.Contains(got, "/opt/phala%%test") || !strings.Contains(got, "cvm%%1") {
		t.Fatalf("proxy=%q", got)
	}
}

func TestMergeClaimLabelsUsesLatestLocalTimestamps(t *testing.T) {
	server := core.Server{Labels: map[string]string{
		"last_touched_at": "100",
		"expires_at":      "200",
	}}
	mergeClaimLabels(&server, core.LeaseClaim{
		LeaseID: "cbx_abcdef123456",
		Slug:    "blue-box",
		Labels: map[string]string{
			"last_touched_at": "300",
			"expires_at":      "400",
		},
	})
	if server.Labels["last_touched_at"] != "300" || server.Labels["expires_at"] != "400" ||
		server.Labels["lease"] != "cbx_abcdef123456" || server.Labels["slug"] != "blue-box" {
		t.Fatalf("labels=%v", server.Labels)
	}
}

func TestTouchPersistsUpdatedLabelsToClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.IdleTimeout = 5 * time.Minute
	applyDefaults(&cfg)
	leaseID := "cbx_abcdef123456"
	server := core.Server{
		CloudID:  "cvm-1",
		Provider: providerName,
		Name:     "blue-box",
		Labels: core.DirectLeaseLabels(
			cfg,
			leaseID,
			"blue-box",
			providerName,
			"",
			false,
			time.Now().Add(-time.Minute),
		),
	}
	target := core.SSHTarget{Host: "cvm-1", User: "root", Port: "22"}
	if err := core.ClaimLeaseTargetForConfig(leaseID, "blue-box", cfg, server, target, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{results: []core.LocalCommandResult{{}}}
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	touched, err := b.Touch(context.Background(), core.TouchRequest{
		Lease:       core.LeaseTarget{LeaseID: leaseID, Server: server, SSH: target},
		State:       "ready",
		IdleTimeout: cfg.IdleTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 ||
		claims[0].Labels["last_touched_at"] != touched.Labels["last_touched_at"] ||
		claims[0].Labels["expires_at"] != touched.Labels["expires_at"] {
		t.Fatalf("claims=%#v touched=%#v", claims, touched.Labels)
	}
}

func TestServerPersistsConfiguredWorkRoot(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Phala.WorkRoot = "/workspace/custom"
	applyDefaults(&cfg)
	b := &backend{}
	server := b.server(instance{ID: "cvm-1", Labels: map[string]string{
		"crabbox":  "true",
		"provider": providerName,
	}}, cfg)
	if server.Labels["work_root"] != "/workspace/custom" {
		t.Fatalf("work_root=%q", server.Labels["work_root"])
	}
	for _, state := range []string{"leased", "ready", "running"} {
		server = b.server(instance{ID: "cvm-1", Labels: map[string]string{"state": state}}, cfg)
		if server.Labels["state"] != state {
			t.Fatalf("state=%q got %q", state, server.Labels["state"])
		}
	}
	server = b.server(instance{ID: "cvm-1", Labels: map[string]string{}}, cfg)
	if server.Labels["state"] != "running" {
		t.Fatalf("empty state got %q", server.Labels["state"])
	}
}

func TestPhalaCVMNameRoundTrip(t *testing.T) {
	name := phalaCVMName("cbx_abcdef123456")
	if name != "crabbox-cbx-abcdef123456" {
		t.Fatalf("name=%q", name)
	}
	if got := leaseIDFromName(name); got != "cbx_abcdef123456" {
		t.Fatalf("lease from name=%q", got)
	}
	if leaseIDFromName("someone-elses-cvm") != "" {
		t.Fatal("foreign name yielded a lease id")
	}
	if phalaCVMName("") != "" || leaseIDFromName("crabbox-") != "" {
		t.Fatal("empty inputs not handled")
	}
}

func TestOwnedAcceptsCrabboxNamePrefix(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// owned() proves ownership two ways: a local claim keyed on the cloud id, OR
	// the crabbox-<lease> name prefix (the pre-claim recovery path). `cvms list`
	// omits the name on real hardware, so an item carrying neither is not ours.
	cfg := core.BaseConfig()
	item := instance{AppID: "appid123", Name: phalaCVMName("cbx_abcdef123456")}
	if !owned(item, cfg) {
		t.Fatal("crabbox- name-prefixed CVM rejected")
	}
	item.Name = "someone-elses-cvm"
	if owned(item, cfg) {
		t.Fatal("foreign-named CVM accepted")
	}
}

func TestOwnedScopesByPinnedNode(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Phala.NodeID = "node-7"
	item := instance{AppID: "appid123", NodeID: "node-7", Name: phalaCVMName("cbx_abcdef123456")}
	if !owned(item, cfg) {
		t.Fatal("matching node rejected")
	}
	item.NodeID = "node-9"
	item.Node = ""
	if owned(item, cfg) {
		t.Fatal("foreign node accepted")
	}
}

func TestPhalaRecoveryPendingUsesGraceWindow(t *testing.T) {
	now := time.Unix(1_000, 0).UTC()
	claim := core.LeaseClaim{Labels: map[string]string{"created_at": "900"}}
	if !phalaRecoveryPending(claim, now) {
		t.Fatal("recent recovery claim not pending")
	}
	if phalaRecoveryPending(claim, time.Unix(1_300, 0).UTC()) {
		t.Fatal("expired recovery grace still pending")
	}
}

// claimPhalaLease persists a local lease claim so destructive-op tests exercise
// the ownership/lease-label guards that run after the local-claim check, rather
// than tripping the "no local claim" refusal first.
func claimPhalaLease(t *testing.T, cfg core.Config, leaseID, cloudID string) {
	t.Helper()
	server := core.Server{
		CloudID:  cloudID,
		Provider: providerName,
		Labels: map[string]string{
			"crabbox":    "true",
			"created_by": "crabbox",
			"lease":      leaseID,
			"provider":   providerName,
		},
	}
	if err := core.ClaimLeaseTargetForConfig(leaseID, leaseID, cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseRejectsForeignCVM(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// validateDestroyTarget sources the CVM (and its name) from `cvms get`, whose
	// real-hardware payload is a snake_case object that DOES carry the name. A
	// foreign CVM has no crabbox- name prefix, so the destroy is refused.
	runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: `{"success":true,"app_id":"foreign","name":"someone-elses-cvm","status":"running"}`}}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	claimPhalaLease(t, cfg, "cbx_abcdef123456", "foreign")
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_abcdef123456",
		Server:  core.Server{CloudID: "foreign"},
	}})
	if err == nil || !strings.Contains(err.Error(), "without Crabbox ownership labels") {
		t.Fatalf("err=%v", err)
	}
	// Only the `cvms get` lookup runs; no `cvms delete` may be issued.
	if len(runner.calls) != 1 || strings.Join(runner.calls[0].args, " ") != "cvms get --cvm-id foreign --json" {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestReleaseRejectsMismatchedLeaseLabel(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// The `cvms get` payload names a crabbox- CVM, but its name encodes a
	// DIFFERENT lease than the claim, so the destroy is refused.
	runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: `{"success":true,"app_id":"owned","name":"crabbox-cbx-other00000000","status":"running"}`}}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	claimPhalaLease(t, cfg, "cbx_abcdef123456", "owned")
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_abcdef123456",
		Server:  core.Server{CloudID: "owned"},
	}})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err=%v", err)
	}
	// Only the `cvms get` lookup runs; no `cvms delete` may be issued.
	if len(runner.calls) != 1 || strings.Join(runner.calls[0].args, " ") != "cvms get --cvm-id owned --json" {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestReleaseRefusesNamePrefixedCVMWithoutLocalClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// A foreign CVM that merely carries the crabbox- name prefix must not be
	// deleted when no local claim backs its lease id. validateDestroyTarget runs
	// the local-claim check FIRST and returns before issuing any CLI call, so the
	// runner is never invoked.
	runner := &fakeRunner{}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_abcdef123456",
		Server:  core.Server{CloudID: "prefixed"},
	}})
	if err == nil || !strings.Contains(err.Error(), "no local claim for lease") {
		t.Fatalf("err=%v", err)
	}
	// The claim check refuses before any CLI call: no `cvms get`, no `cvms delete`.
	if len(runner.calls) != 0 {
		t.Fatalf("issued a CLI call before the local-claim check: calls=%#v", runner.calls)
	}
}

func TestReleaseSkipsDeleteWhenCVMMissingFromInventory(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// The claim exists but the CVM is gone: real `cvms get` for a missing CVM
	// exits non-zero with "CVM not found" on stderr, which getInstance tolerates
	// as found=false. validateDestroyTarget then returns present=false: no delete,
	// no error, and the local claim is reaped.
	runner := &fakeRunner{
		results: []core.LocalCommandResult{{Stderr: "Error: CVM not found"}},
		errs:    []error{errors.New("exit status 1")},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	leaseID := "cbx_abcdef123456"
	server := core.Server{
		CloudID:  "missing-cvm",
		Provider: providerName,
		Labels: map[string]string{
			"crabbox":    "true",
			"created_by": "crabbox",
			"lease":      leaseID,
			"provider":   providerName,
		},
	}
	if err := core.ClaimLeaseTargetForConfig(leaseID, "missing", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: leaseID,
		Server:  server,
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Only the `cvms get` lookup runs; the missing CVM means no `cvms delete`.
	if len(runner.calls) != 1 || strings.Join(runner.calls[0].args, " ") != "cvms get --cvm-id missing-cvm --json" {
		t.Fatalf("calls=%#v", runner.calls)
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("claims=%#v", claims)
	}
}

func TestReleaseOnlyResolveAllowsExpiredClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	leaseID := "cbx_abcdef123456"
	server := core.Server{
		CloudID:  "expired-cvm",
		Provider: providerName,
		Labels: map[string]string{
			"crabbox":    "true",
			"created_by": "crabbox",
			"lease":      leaseID,
			"provider":   providerName,
			"slug":       "expired",
		},
	}
	if err := core.ClaimLeaseTargetForConfig(leaseID, "expired", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: `{"success":true,"items":[]}`}}}
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	item, resolvedLeaseID, err := b.resolve(context.Background(), leaseID, cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if item.cloudID() != "expired-cvm" || resolvedLeaseID != leaseID {
		t.Fatalf("item=%#v lease=%q", item, resolvedLeaseID)
	}
}

func TestResolveExactCVMIDOutranksClaimSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	// A claim whose slug ("cvm-exact") collides with a different CVM's exact id.
	// resolve must prefer the exact-id instance over the slug-matched claim.
	slugServer := core.Server{
		CloudID:  "instance-slug",
		Provider: providerName,
		Name:     phalaCVMName("cbx_aaaaaaaaaaaa"),
		Labels: map[string]string{
			"crabbox":    "true",
			"created_by": "crabbox",
			"lease":      "cbx_aaaaaaaaaaaa",
			"provider":   providerName,
			"slug":       "cvm-exact",
		},
	}
	if err := core.ClaimLeaseTargetForConfig(
		"cbx_aaaaaaaaaaaa",
		"cvm-exact",
		cfg,
		slugServer,
		core.SSHTarget{},
		cfg.IdleTimeout,
	); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: `{"success":true,"items":[
		{"id":"cvm-exact","name":"crabbox-cbx-bbbbbbbbbbbb","status":"running"},
		{"id":"instance-slug","name":"crabbox-cbx-aaaaaaaaaaaa","status":"running"}
	]}`}}}
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	item, leaseID, err := b.resolve(context.Background(), "cvm-exact", cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if item.cloudID() != "cvm-exact" || leaseID != "cbx_bbbbbbbbbbbb" {
		t.Fatalf("item=%#v lease=%q", item, leaseID)
	}
}

func TestCleanupTransitionsAndRemovesClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	leaseID := "cbx_abcdef123456"
	labels := map[string]string{
		"crabbox":    "true",
		"created_by": "crabbox",
		"expires_at": "1",
		"lease":      leaseID,
		"provider":   providerName,
		"slug":       "expired",
	}
	server := core.Server{CloudID: "expired-cvm", Provider: providerName, Labels: labels}
	if err := core.ClaimLeaseTargetForConfig(leaseID, "expired", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{results: []core.LocalCommandResult{
		{Stdout: `{"success":true,"items":[
			{"id":"expired-cvm","name":"crabbox-cbx-abcdef123456","status":"running"}
		]}`},
		{},
	}}
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("claims=%#v", claims)
	}
	if len(runner.calls) != 2 || strings.Join(runner.calls[1].args, " ") != "cvms delete --cvm-id expired-cvm --force" {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestCleanupSkipsNamePrefixedCVMWithoutLocalClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	// A name-prefixed CVM with no backing local claim must never be deleted: with
	// no server-side ownership label it could be a foreign resource.
	runner := &fakeRunner{results: []core.LocalCommandResult{
		{Stdout: `{"success":true,"items":[
			{"id":"prefixed","name":"crabbox-cbx-abcdef123456","status":"running"}
		]}`},
	}}
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || strings.Join(runner.calls[0].args, " ") != "cvms list --json" {
		t.Fatalf("issued more than the inventory list (possible delete): calls=%#v", runner.calls)
	}
}

func TestCleanupAppliesRecoveryPolicyToLiveCVMs(t *testing.T) {
	now := time.Unix(10_000, 0).UTC()
	for _, test := range []struct {
		name       string
		recovery   string
		createdAt  time.Time
		wantCalls  int
		wantClaims int
	}{
		{
			name:       "ambiguous pending",
			recovery:   "ambiguous-create",
			createdAt:  now.Add(-time.Minute),
			wantCalls:  1,
			wantClaims: 1,
		},
		{
			name:       "ambiguous grace expired",
			recovery:   "ambiguous-create",
			createdAt:  now.Add(-phalaAmbiguousCreateRecoveryGrace - time.Second),
			wantCalls:  2,
			wantClaims: 0,
		},
		{
			name:       "rollback cleanup",
			recovery:   "rollback-cleanup",
			createdAt:  now,
			wantCalls:  2,
			wantClaims: 0,
		},
		{
			name:       "kept after failure",
			recovery:   "kept-after-failure",
			createdAt:  now.Add(-24 * time.Hour),
			wantCalls:  1,
			wantClaims: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			cfg := core.BaseConfig()
			cfg.Provider = providerName
			applyDefaults(&cfg)
			leaseID := "cbx_abcdef123456"
			labels := core.DirectLeaseLabels(cfg, leaseID, "recovery", providerName, "", false, test.createdAt)
			labels["recovery"] = test.recovery
			labels["state"] = "provisioning"
			server := core.Server{CloudID: "recovery-cvm", Provider: providerName, Labels: labels}
			if err := core.ClaimLeaseTargetForConfig(leaseID, "recovery", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
				t.Fatal(err)
			}
			runner := &fakeRunner{results: []core.LocalCommandResult{
				{Stdout: `{"success":true,"items":[
					{"id":"recovery-cvm","name":"crabbox-cbx-abcdef123456","status":"running"}
				]}`},
				{},
			}}
			b := &backend{
				cfg: cfg,
				rt: core.Runtime{
					Clock:  fixedClock{now: now},
					Exec:   runner,
					Stdout: io.Discard,
					Stderr: io.Discard,
				},
			}
			if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
				t.Fatal(err)
			}
			if len(runner.calls) != test.wantCalls {
				t.Fatalf("calls=%#v want %d", runner.calls, test.wantCalls)
			}
			claims, err := core.ListLeaseClaims()
			if err != nil {
				t.Fatal(err)
			}
			if len(claims) != test.wantClaims {
				t.Fatalf("claims=%#v want %d", claims, test.wantClaims)
			}
		})
	}
}

// TestPhalaToolBootstrapRequiresOnlyRsyncSyncEssentials pins the dev-os contract
// discovered on real TDX hardware: the dstack --dev-os guest is an immutable
// appliance (read-only root, NO package manager, no egress) that already ships
// rsync, tar and python3 but NOT git. The bootstrap must therefore require only
// the rsync-sync essentials and treat git as opportunistic, or it could never
// succeed on the supported guest (the earlier git-required form failed live with
// "Phala CVM tool bootstrap failed: exit status 1").
func TestPhalaToolBootstrapRequiresOnlyRsyncSyncEssentials(t *testing.T) {
	command := phalaToolBootstrapCommand()

	// The early-exit fast path (taken on the dev-os guest) must check exactly the
	// required set and must NOT require git.
	earlyExit := "if command -v rsync >/dev/null 2>&1 && command -v tar >/dev/null 2>&1 && command -v python3 >/dev/null 2>&1; then exit 0; fi"
	if !strings.Contains(command, earlyExit) {
		t.Fatalf("bootstrap missing rsync+tar+python3 early-exit:\n%s", command)
	}

	// The final verification line is the gate that returns the exit status; it
	// must require rsync+tar+python3 and must NOT require git.
	finalCheck := "command -v rsync >/dev/null && command -v tar >/dev/null && command -v python3 >/dev/null"
	if !strings.Contains(command, finalCheck) {
		t.Fatalf("bootstrap missing rsync+tar+python3 final check:\n%s", command)
	}
	if strings.Contains(command, "command -v git >/dev/null &&") {
		t.Fatalf("bootstrap must not REQUIRE git (unavailable on the immutable dev-os guest):\n%s", command)
	}

	// git stays in the opportunistic install lines for non-dev-os images that do
	// have a package manager.
	if !strings.Contains(command, "apk add --no-cache git rsync tar python3") {
		t.Fatalf("bootstrap should still opportunistically install git via apk for non-dev-os images:\n%s", command)
	}
}

// TestDefaultWorkRootIsWritableOnDevOsGuest pins the work-root default to a
// writable mount. The dstack --dev-os guest roots its filesystem on a read-only
// squashfs, so the previous /work/crabbox default could not be created and the
// sync failed live at "write sync manifests: exit status 1". /var/volatile is a
// writable tmpfs on every dstack guest; the default must live under it (and not
// be the bare mount, which ValidateConfig rejects as too broad).
func TestDefaultWorkRootIsWritableOnDevOsGuest(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	if cfg.Phala.WorkRoot != "/var/volatile/crabbox" {
		t.Fatalf("default WorkRoot = %q, want /var/volatile/crabbox", cfg.Phala.WorkRoot)
	}
	if cfg.WorkRoot != cfg.Phala.WorkRoot {
		t.Fatalf("cfg.WorkRoot %q not mirrored from Phala.WorkRoot %q", cfg.WorkRoot, cfg.Phala.WorkRoot)
	}
	if strings.HasPrefix(cfg.Phala.WorkRoot, "/work") {
		t.Fatalf("default WorkRoot must not sit on the read-only root: %q", cfg.Phala.WorkRoot)
	}
	// The default must survive its own validator.
	if err := (Provider{}).ValidateConfig(cfg); err != nil {
		t.Fatalf("default WorkRoot rejected by ValidateConfig: %v", err)
	}
}

// TestPhalaLeaseReadyCheckDropsGit guards the lease ReadyCheck the same way: the
// SSH readiness probe must not block on git, which the dev-os guest cannot
// provide.
func TestPhalaLeaseReadyCheckDropsGit(t *testing.T) {
	b := &backend{rt: core.Runtime{}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	lease := b.lease(instance{ID: "appid123", Labels: map[string]string{"lease": "cbx_test"}}, cfg, "cbx_test")
	if strings.Contains(lease.SSH.ReadyCheck, "git") {
		t.Fatalf("lease ReadyCheck must not require git:\n%s", lease.SSH.ReadyCheck)
	}
	for _, tool := range []string{"rsync", "tar", "python3"} {
		if !strings.Contains(lease.SSH.ReadyCheck, "command -v "+tool) {
			t.Fatalf("lease ReadyCheck missing %s:\n%s", tool, lease.SSH.ReadyCheck)
		}
	}
}

func TestDestroyOnlySuppressesExplicitMissingCVMResponse(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	for _, test := range []struct {
		name    string
		result  core.LocalCommandResult
		err     error
		wantErr bool
	}{
		{
			name:   "provider reports missing CVM",
			result: core.LocalCommandResult{Stderr: "Error: CVM not found"},
			err:    errors.New("exit status 1"),
		},
		{
			name:    "phala executable missing",
			err:     errors.New(`exec: "phala": executable file not found in $PATH`),
			wantErr: true,
		},
		{
			name:    "unrelated provider error",
			result:  core.LocalCommandResult{Stderr: "gateway host unreachable"},
			err:     errors.New("exit status 1"),
			wantErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{
				results: []core.LocalCommandResult{test.result},
				errs:    []error{test.err},
			}
			b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
			err := b.destroy(context.Background(), "missing-cvm")
			if (err != nil) != test.wantErr {
				t.Fatalf("err=%v wantErr=%t", err, test.wantErr)
			}
		})
	}
}

func TestJSONObjectPrefixHandlesNoiseAndStrings(t *testing.T) {
	if got := jsonObjectPrefix(`{"a":"}"}` + "\nnoise"); got != `{"a":"}"}` {
		t.Fatalf("prefix=%q", got)
	}
	if got := jsonObjectPrefix("   not json"); got != "" {
		t.Fatalf("non-json prefix=%q", got)
	}
	if got := jsonObjectPrefix(`[1,2,3]trailing`); got != `[1,2,3]` {
		t.Fatalf("array prefix=%q", got)
	}
	// A live `phala deploy --json` run prints a leading human progress line
	// before the JSON object; the extractor must skip leading non-JSON lines too.
	leading := "Provisioning CVM crabbox-cbx-abcdef123456...\n" + `{"success":true,"app_id":"abc"}`
	if got := jsonObjectPrefix(leading); got != `{"success":true,"app_id":"abc"}` {
		t.Fatalf("leading-noise prefix=%q", got)
	}
}

// realDeployStdout is the EXACT stdout a live `phala deploy --json --dev-os
// --ssh-pubkey <pub> --wait -t tdx.small -n <name> --compose <file>` run wrote
// against real Phala TDX hardware: a leading human progress line, then the JSON
// object. The provider previously reported "phala deploy produced no JSON
// output" because its extractor would not skip the leading line.
const realDeployStdout = `Provisioning CVM crabbox-cbx-abcdef123456...
{
  "success": true,
  "vm_uuid": "42fd1f82-7b4c-47cc-92f9-a5d39476c649",
  "name": "crabbox-cbx-abcdef123456",
  "app_id": "b60d1f55eeb01f17e0a5220b4c03792248d49f92",
  "dashboard_url": "https://cloud.phala.com/dashboard/cvms/42fd1f82-7b4c-47cc-92f9-a5d39476c649"
}`

// TestParseDeployIDFromRealHardwareStdout pins the deploy parser against the
// literal stdout observed on real TDX hardware: a leading "Provisioning CVM..."
// progress line ahead of the JSON object. The canonical id is the app_id.
func TestParseDeployIDFromRealHardwareStdout(t *testing.T) {
	id, err := parseDeployID(realDeployStdout)
	if err != nil {
		t.Fatalf("parseDeployID rejected real deploy stdout: %v", err)
	}
	if id != "b60d1f55eeb01f17e0a5220b4c03792248d49f92" {
		t.Fatalf("deploy id=%q want canonical app_id", id)
	}
}

// TestCreateParsesRealHardwareDeployStdout drives create() end-to-end with the
// exact real deploy stdout to prove the provider no longer fails with "produced
// no JSON output" and returns the app_id as the CVM handle.
func TestCreateParsesRealHardwareDeployStdout(t *testing.T) {
	runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: realDeployStdout}}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Phala.Compose = "/srv/compose.yml"
	applyDefaults(&cfg)
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	id, err := b.create(context.Background(), cfg, "/tmp/id.pub", map[string]string{
		"crabbox":    "true",
		"created_by": "crabbox",
		"lease":      "cbx_abcdef123456",
		"provider":   providerName,
	})
	if err != nil {
		t.Fatalf("create failed on real deploy stdout: %v", err)
	}
	if id != "b60d1f55eeb01f17e0a5220b4c03792248d49f92" {
		t.Fatalf("create id=%q want canonical app_id", id)
	}
}

// realCVMSListStdout is the EXACT `phala cvms list --json` payload observed on
// real hardware: a {success,total,items:[...]} wrapper whose items use
// camelCase keys (appId, status, name, vmUuid). The provider previously parsed
// only snake_case and so read blank identifiers from this payload.
const realCVMSListStdout = `{
  "success": true,
  "total": 1,
  "items": [
    {
      "appId": "b60d1f55eeb01f17e0a5220b4c03792248d49f92",
      "name": "crabbox-cbx-abcdef123456",
      "vmUuid": "42fd1f82-7b4c-47cc-92f9-a5d39476c649",
      "instanceId": "i-0a5d39476c649",
      "status": "running"
    }
  ]
}`

// TestListParsesRealCamelCaseListPayload pins list parsing against the exact
// camelCase `cvms list --json` payload from real hardware: the item's appId,
// name, vmUuid, instanceId and status must all decode (snake_case-only parsing
// would have read blanks), and cloudID() must return the canonical app_id.
func TestListParsesRealCamelCaseListPayload(t *testing.T) {
	runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: realCVMSListStdout}}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	items, err := b.listInstances(context.Background())
	if err != nil {
		t.Fatalf("listInstances failed on real camelCase payload: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items=%#v want exactly one", items)
	}
	item := items[0]
	if item.AppID != "b60d1f55eeb01f17e0a5220b4c03792248d49f92" {
		t.Fatalf("appId=%q not decoded from camelCase", item.AppID)
	}
	if item.Name != "crabbox-cbx-abcdef123456" {
		t.Fatalf("name=%q not decoded from camelCase", item.Name)
	}
	if item.VMUUID != "42fd1f82-7b4c-47cc-92f9-a5d39476c649" {
		t.Fatalf("vmUuid=%q not decoded from camelCase", item.VMUUID)
	}
	if item.InstanceID != "i-0a5d39476c649" {
		t.Fatalf("instanceId=%q not decoded from camelCase", item.InstanceID)
	}
	if item.Status != "running" {
		t.Fatalf("status=%q not decoded", item.Status)
	}
	// app_id is the canonical handle passed to cvms get/delete --cvm-id.
	if item.cloudID() != "b60d1f55eeb01f17e0a5220b4c03792248d49f92" {
		t.Fatalf("cloudID=%q want canonical app_id", item.cloudID())
	}
	// Ownership is derived from the name prefix, so the lease label must resolve.
	if item.Labels["lease"] != "cbx_abcdef123456" {
		t.Fatalf("lease label=%q not derived from camelCase name", item.Labels["lease"])
	}
}

// TestInstanceUnmarshalAcceptsBothCaseStyles asserts the tolerant decoder reads
// both the snake_case (`cvms get`) and camelCase (`cvms list`) spelling of
// every identifier, accepting appId OR app_id et al.
func TestInstanceUnmarshalAcceptsBothCaseStyles(t *testing.T) {
	for _, test := range []struct {
		name string
		json string
	}{
		{name: "snake_case (cvms get)", json: `{"app_id":"app1","vm_uuid":"vm1","instance_id":"in1","name":"n1","status":"running"}`},
		{name: "camelCase (cvms list)", json: `{"appId":"app1","vmUuid":"vm1","instanceId":"in1","name":"n1","status":"running"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var item instance
			if err := json.Unmarshal([]byte(test.json), &item); err != nil {
				t.Fatal(err)
			}
			if item.AppID != "app1" || item.VMUUID != "vm1" || item.InstanceID != "in1" ||
				item.Name != "n1" || item.Status != "running" {
				t.Fatalf("item=%#v", item)
			}
			if item.cloudID() != "app1" {
				t.Fatalf("cloudID=%q want app_id", item.cloudID())
			}
		})
	}
}

// TestGetInstanceParsesRealCVMSGetShapes pins getInstance against the real
// `cvms get --json` payloads: a flat snake_case object that carries the name,
// the same object nested under a top-level cvm key, and the missing-CVM error
// (non-zero exit with "not found" on stderr) which must read as found=false with
// no error so an already-gone CVM is not an error on the destroy path.
func TestGetInstanceParsesRealCVMSGetShapes(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	for _, test := range []struct {
		name string
		body string
	}{
		{
			name: "flat object",
			body: `{"success":true,"app_id":"b60d1f55","vm_uuid":"42fd1f82","name":"crabbox-cbx-abcdef123456","status":"running"}`,
		},
		{
			name: "nested cvm object",
			body: `{"success":true,"cvm":{"app_id":"b60d1f55","vm_uuid":"42fd1f82","name":"crabbox-cbx-abcdef123456","status":"running"}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: test.body}}}
			b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
			item, ok, err := b.getInstance(context.Background(), "b60d1f55")
			if err != nil || !ok {
				t.Fatalf("ok=%t err=%v", ok, err)
			}
			if strings.Join(runner.calls[0].args, " ") != "cvms get --cvm-id b60d1f55 --json" {
				t.Fatalf("args=%q", strings.Join(runner.calls[0].args, " "))
			}
			if item.cloudID() != "b60d1f55" || item.Name != "crabbox-cbx-abcdef123456" {
				t.Fatalf("item=%#v", item)
			}
			if item.Labels["lease"] != "cbx_abcdef123456" {
				t.Fatalf("lease label=%q not derived from name", item.Labels["lease"])
			}
		})
	}
	t.Run("missing CVM", func(t *testing.T) {
		runner := &fakeRunner{
			results: []core.LocalCommandResult{{Stderr: "Error: CVM not found"}},
			errs:    []error{errors.New("exit status 1")},
		}
		b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
		item, ok, err := b.getInstance(context.Background(), "gone")
		if err != nil || ok {
			t.Fatalf("missing CVM should be found=false nil-err: ok=%t err=%v", ok, err)
		}
		if item.cloudID() != "" {
			t.Fatalf("missing CVM yielded an item: %#v", item)
		}
	})
}

// TestListSkipsItemMissingName proves listInstances skips (rather than crashes
// on or surfaces) a list item with no name and no usable handle.
func TestListSkipsItemMissingName(t *testing.T) {
	runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: `{"success":true,"items":[
		{"status":"running"},
		{"appId":"keeper","name":"crabbox-cbx-abcdef123456","status":"running"}
	]}`}}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	items, err := b.listInstances(context.Background())
	if err != nil {
		t.Fatalf("listInstances crashed on a nameless item: %v", err)
	}
	if len(items) != 1 || items[0].cloudID() != "keeper" {
		t.Fatalf("items=%#v want only the named keeper", items)
	}
}
