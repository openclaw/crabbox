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
		{Stdout: `{"success":true,"items":[{"appId":"recovered","cvmName":"crabbox-cbx-abcdef123456","status":"running"}]}`},
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

// TestFindByLeaseRecoversFromRealListCvmNameWithoutClaim pins the FIX A
// foundational property: a just-created CVM has NO local claim yet, and the
// real `cvms list` item carries its name ONLY under `cvmName` (no `name` key).
// findByLease must still recover it by deriving the lease from the cvmName
// prefix via owned()'s name-branch. Reading the name from `name`/`appName`
// alone (the pre-fix behavior) left item.Name blank and silently broke recovery.
func TestFindByLeaseRecoversFromRealListCvmNameWithoutClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := &fakeRunner{results: []core.LocalCommandResult{
		{Stdout: `{"success":true,"items":[
			{"appId":"b60d1f55","cvmName":"crabbox-cbx-abcdef123456","status":"running","uptime":"1 minute"}
		]}`},
	}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	item, err := b.findByLease(context.Background(), cfg, "cbx_abcdef123456")
	if err != nil {
		t.Fatalf("findByLease failed to recover from cvmName-only list item: %v", err)
	}
	if item.cloudID() != "b60d1f55" {
		t.Fatalf("cloudID=%q want app_id from list item", item.cloudID())
	}
	if item.Name != "crabbox-cbx-abcdef123456" {
		t.Fatalf("name=%q not decoded from cvmName", item.Name)
	}
	if item.Labels["lease"] != "cbx_abcdef123456" {
		t.Fatalf("lease label=%q not derived from cvmName prefix", item.Labels["lease"])
	}
}

func TestCreateRecoversAfterCallerCancellation(t *testing.T) {
	runner := &fakeRunner{
		results: []core.LocalCommandResult{
			{},
			{Stdout: `{"success":true,"items":[{"appId":"recovered","cvmName":"crabbox-cbx-abcdef123456","status":"running"}]}`},
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

// TestAmbiguousPhalaCreateOutcomeAnchorsMarkers pins FIX E.2: cancellation is
// detected structurally, anchored transport phrases trigger recovery, but the
// bare "eof"/"unavailable" substrings inside larger unrelated words must NOT
// false-match and waste the recovery window.
func TestAmbiguousPhalaCreateOutcomeAnchorsMarkers(t *testing.T) {
	for _, test := range []struct {
		name   string
		result core.LocalCommandResult
		err    error
		want   bool
	}{
		{name: "context canceled (structural)", err: context.Canceled, want: true},
		{name: "deadline exceeded (structural)", err: context.DeadlineExceeded, want: true},
		{name: "unexpected eof", result: core.LocalCommandResult{Stderr: "rpc error: unexpected EOF"}, err: errors.New("exit status 1"), want: true},
		{name: "service unavailable", result: core.LocalCommandResult{Stderr: "503 Service Unavailable"}, err: errors.New("exit status 1"), want: true},
		{name: "connection reset", result: core.LocalCommandResult{Stderr: "connection reset by peer"}, err: errors.New("exit status 1"), want: true},
		// "eof" embedded in an unrelated word must NOT trigger recovery.
		{name: "bare eof in word", result: core.LocalCommandResult{Stderr: "wrote /tmp/eofdata.json"}, err: errors.New("exit status 1"), want: false},
		// A definitive validation error must NOT be treated as ambiguous.
		{name: "definitive validation error", result: core.LocalCommandResult{Stderr: "InvalidArgument: bad instance type"}, err: errors.New("exit status 1"), want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ambiguousPhalaCreateOutcome(test.result, test.err); got != test.want {
				t.Fatalf("ambiguousPhalaCreateOutcome=%t want %t", got, test.want)
			}
		})
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
			{"appId":"owned","cvmName":"crabbox-cbx-abcdef123456","status":"running"},
			{"appId":"foreign","cvmName":"someone-elses-cvm","status":"running"}
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
			{"appId":"prefixed","cvmName":"crabbox-cbx-abcdef123456","status":"running"}
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
		{Stdout: "{\"success\":true,\"items\":[{\"appId\":\"owned\",\"cvmName\":\"crabbox-cbx-abcdef123456\",\"status\":\"running\"}]}\nAssertion failed: !(handle->flags & UV_HANDLE_CLOSING)\n"},
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
	got := proxyCommand(cfg, "cvm-1", "")
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
	// With no cached gateway host the proxy command must NOT carry --gateway-host,
	// so the proxy falls back to the per-connection cvms-get resolution.
	if strings.Contains(got, "--gateway-host") {
		t.Fatalf("proxy=%q carried --gateway-host with no cached host", got)
	}
}

func TestProxyCommandCarriesCachedGatewayHost(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Phala.CLIPath = "phala"
	const host = "b60d1f55-22.dstack-pha-prod5.phala.network"
	got := proxyCommand(cfg, "cvm-1", host)
	if !strings.Contains(got, "--gateway-host "+host) {
		t.Fatalf("proxy=%q missing --gateway-host %q", got, host)
	}
	// The CVM id must still be the trailing positional argument.
	if !strings.HasSuffix(strings.TrimSpace(got), "cvm-1") {
		t.Fatalf("proxy=%q does not end with the cvm id", got)
	}
}

func TestProxyCommandEscapesOpenSSHPercentTokens(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Phala.CLIPath = "/opt/phala%test"
	got := proxyCommand(cfg, "cvm%1", "")
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

// TestReleaseRetainsClaimOnTransientNotFound pins FIX C: a TRANSIENT `cvms get`
// failure whose body merely CONTAINS "not found" as part of an unrelated message
// (here a gateway endpoint error) must NOT be mistaken for a definitively-gone
// CVM. The local claim is the sole ownership anchor, so it must be retained (and
// the release returns an error) so a later `crabbox stop` can retry rather than
// orphaning a live billing CVM.
func TestReleaseRetainsClaimOnTransientNotFound(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := &fakeRunner{
		results: []core.LocalCommandResult{{Stderr: "Error: gateway endpoint not found (upstream route unavailable)"}},
		errs:    []error{errors.New("exit status 1")},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	leaseID := "cbx_abcdef123456"
	server := core.Server{
		CloudID:  "live-cvm",
		Provider: providerName,
		Labels: map[string]string{
			"crabbox":    "true",
			"created_by": "crabbox",
			"lease":      leaseID,
			"provider":   providerName,
		},
	}
	if err := core.ClaimLeaseTargetForConfig(leaseID, "live", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: leaseID,
		Server:  server,
	}})
	if err == nil {
		t.Fatal("transient cvms-get failure must surface as an error, not a silent reap")
	}
	// No delete may be issued, and the claim must SURVIVE for a later retry.
	for _, c := range runner.calls {
		if len(c.args) > 1 && c.args[1] == "delete" {
			t.Fatalf("issued a delete on a transient lookup failure: calls=%#v", runner.calls)
		}
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].LeaseID != leaseID {
		t.Fatalf("claim was dropped on a transient not-found: claims=%#v", claims)
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

// TestResolveStatusOnlyReadyProbeSkipsBootstrap pins FIX F: a `status --wait`
// resolve (StatusOnly + ReadyProbe) must NOT run prepareSSH (the FULL tool
// bootstrap over SSH). prepareSSH's first act is WaitForSSHReady, which returns
// the context error immediately on a cancelled context; so with a cancelled
// context, a Resolve that returns NIL proves prepareSSH was skipped, while a
// non-status Resolve returns that context error. The fakeRunner ignores the
// context, so the `cvms list` inside resolve() still succeeds.
func TestResolveStatusOnlyReadyProbeSkipsBootstrap(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.IdleTimeout = 5 * time.Minute
	applyDefaults(&cfg)
	leaseID := "cbx_abcdef123456"
	const host = "b60d1f55-22.dstack-pha-prod5.phala.network"
	labels := core.DirectLeaseLabels(cfg, leaseID, "blue-box", providerName, "", false, time.Now())
	labels["phala_cvm"] = "owned"
	labels["gateway_host"] = host
	server := core.Server{CloudID: "owned", Provider: providerName, Name: "blue-box", Labels: labels}
	if err := core.ClaimLeaseTargetForConfig(leaseID, "blue-box", cfg, server, core.SSHTarget{Host: "owned", User: "root", Port: "22"}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	listPayload := `{"success":true,"items":[{"appId":"owned","cvmName":"crabbox-cbx-abcdef123456","status":"running"}]}`

	// status --wait: StatusOnly + ReadyProbe, cancelled context. prepareSSH is
	// skipped, so Resolve returns the lease with no error.
	runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: listPayload}}}
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lease, err := b.Resolve(ctx, core.ResolveRequest{ID: leaseID, StatusOnly: true, ReadyProbe: true})
	if err != nil {
		t.Fatalf("status --wait Resolve must skip bootstrap, got err=%v", err)
	}
	if lease.LeaseID != leaseID {
		t.Fatalf("lease=%#v", lease)
	}
	// The cached gateway host must be on the status path's lease target so the
	// lightweight probe does not pay a per-connection `cvms get`.
	if !strings.Contains(lease.SSH.ProxyCommand, "--gateway-host "+host) {
		t.Fatalf("status lease ProxyCommand missing cached gateway host: %q", lease.SSH.ProxyCommand)
	}
	// Only the `cvms list` from resolve() ran; no per-connection `cvms get`.
	if len(runner.calls) != 1 || strings.Join(runner.calls[0].args, " ") != "cvms list --json" {
		t.Fatalf("status path issued unexpected CLI calls: %#v", runner.calls)
	}

	// Control: a non-status Resolve (acquire/run) on the same cancelled context
	// DOES run prepareSSH, which surfaces the cancellation. This proves the guard
	// is what suppressed the bootstrap above, not some other short-circuit.
	runner2 := &fakeRunner{results: []core.LocalCommandResult{{Stdout: listPayload}}}
	b2 := &backend{cfg: cfg, rt: core.Runtime{Exec: runner2, Stdout: io.Discard, Stderr: io.Discard}}
	if _, err := b2.Resolve(ctx, core.ResolveRequest{ID: leaseID}); err == nil {
		t.Fatal("non-status Resolve on a cancelled context must run prepareSSH and error")
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
		{"appId":"cvm-exact","cvmName":"crabbox-cbx-bbbbbbbbbbbb","status":"running"},
		{"appId":"instance-slug","cvmName":"crabbox-cbx-aaaaaaaaaaaa","status":"running"}
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

// TestSlugRoundTripsThroughResolveAndList pins FIX G: after a claim carries
// slug=X, (1) resolve("X") must find the lease, (2) List must surface slug=X,
// and (3) a Resolve re-claim (with a repo root) must NOT blank the stored slug.
// The regression was Resolve re-claiming with item.Labels["slug"] -- which is
// empty on the synthetic/list path -- writing an empty slug over the stored one,
// so List then showed slug=- and resolve-by-slug missed.
func TestSlugRoundTripsThroughResolveAndList(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.IdleTimeout = 5 * time.Minute
	applyDefaults(&cfg)
	leaseID := "cbx_abcdef123456"
	const slug = "blue-box"
	repoRoot := t.TempDir()
	// Mirror the fixed acquire: the slug is in BOTH claim.Slug (explicit arg) and
	// the server labels.
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now())
	labels["phala_cvm"] = "owned"
	labels["state"] = "ready"
	server := core.Server{CloudID: "owned", Provider: providerName, Name: slug, Labels: labels}
	target := core.SSHTarget{Host: "owned", User: "root", Port: "22"}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, target, repoRoot, cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	listPayload := `{"success":true,"items":[{"appId":"owned","cvmName":"crabbox-cbx-abcdef123456","status":"running"}]}`

	// (1) resolve-by-slug finds the lease, and the server label surfaces slug=X.
	runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: listPayload}}}
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	item, resolvedLease, err := b.resolve(context.Background(), slug, cfg, false)
	if err != nil {
		t.Fatalf("resolve-by-slug failed: %v", err)
	}
	if resolvedLease != leaseID {
		t.Fatalf("resolve-by-slug lease=%q want %q", resolvedLease, leaseID)
	}
	srv := b.server(item, cfg)
	if claim, ok, _ := resolvePhalaClaim(leaseID, cfg); ok {
		mergeClaimLabels(&srv, claim)
	}
	if srv.Labels["slug"] != slug {
		t.Fatalf("server slug=%q want %q", srv.Labels["slug"], slug)
	}

	// (2) List surfaces slug=X.
	runner2 := &fakeRunner{results: []core.LocalCommandResult{{Stdout: listPayload}}}
	b2 := &backend{cfg: cfg, rt: core.Runtime{Exec: runner2, Stdout: io.Discard, Stderr: io.Discard}}
	views, err := b2.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Labels["slug"] != slug {
		t.Fatalf("List did not surface slug=%q: views=%#v", slug, views)
	}

	// (3) A Resolve re-claim (with a repo root) must NOT blank the stored slug.
	// The resolve path returns a synthetic/list item whose labels may lack slug;
	// the re-claim must prefer the authoritative slug, not overwrite it with blank.
	runner3 := &fakeRunner{results: []core.LocalCommandResult{{Stdout: listPayload}}}
	b3 := &backend{cfg: cfg, rt: core.Runtime{Exec: runner3, Stdout: io.Discard, Stderr: io.Discard}}
	// ReadyProbe:true gets past the status-only early return; StatusOnly:true still
	// skips the SSH bootstrap (FIX F), so the re-claim block runs without SSH.
	if _, err := b3.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, StatusOnly: true, ReadyProbe: true, Repo: core.Repo{Root: repoRoot}}); err != nil {
		t.Fatalf("Resolve re-claim failed: %v", err)
	}
	claim, ok, err := resolvePhalaClaim(leaseID, cfg)
	if err != nil || !ok {
		t.Fatalf("claim missing after Resolve: ok=%t err=%v", ok, err)
	}
	if claim.Slug != slug {
		t.Fatalf("Resolve re-claim blanked the slug: claim.Slug=%q want %q", claim.Slug, slug)
	}
	// And resolve-by-slug must STILL work after the re-claim.
	runner4 := &fakeRunner{results: []core.LocalCommandResult{{Stdout: listPayload}}}
	b4 := &backend{cfg: cfg, rt: core.Runtime{Exec: runner4, Stdout: io.Discard, Stderr: io.Discard}}
	if _, lease2, err := b4.resolve(context.Background(), slug, cfg, false); err != nil || lease2 != leaseID {
		t.Fatalf("resolve-by-slug broke after re-claim: lease=%q err=%v", lease2, err)
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
			{"appId":"expired-cvm","cvmName":"crabbox-cbx-abcdef123456","status":"running"}
		]}`},
		{Stdout: `{"success":true,"app_id":"expired-cvm","name":"crabbox-cbx-abcdef123456","status":"running"}`},
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
	// Cleanup now corroborates ownership via `cvms get` before deleting (FIX B),
	// so the call sequence is list -> get -> delete.
	if len(runner.calls) != 3 ||
		strings.Join(runner.calls[1].args, " ") != "cvms get --cvm-id expired-cvm --json" ||
		strings.Join(runner.calls[2].args, " ") != "cvms delete --cvm-id expired-cvm --force" {
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
			{"appId":"prefixed","cvmName":"crabbox-cbx-abcdef123456","status":"running"}
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

// TestCleanupRejectsForeignAppIDReuse pins FIX B: a stale-but-unexpired local
// claim records an app_id that dstack has since reused for a FOREIGN CVM. The
// `cvms list` item's appId matches the claim (so owned() is true and the claim
// resolves), but the corroborating `cvms get` returns a name that encodes a
// DIFFERENT lease. Cleanup must NOT delete it, and must NOT drop the claim/key:
// it mirrors ReleaseLease's validateDestroyTarget gate.
func TestCleanupRejectsForeignAppIDReuse(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	leaseID := "cbx_abcdef123456"
	labels := map[string]string{
		"crabbox":    "true",
		"created_by": "crabbox",
		"expires_at": "1", // expired, so ShouldCleanupServer would want to delete
		"lease":      leaseID,
		"provider":   providerName,
		"slug":       "expired",
	}
	server := core.Server{CloudID: "reused-appid", Provider: providerName, Labels: labels}
	if err := core.ClaimLeaseTargetForConfig(leaseID, "expired", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{results: []core.LocalCommandResult{
		// list: the reused app_id is live again, name encodes a DIFFERENT lease.
		{Stdout: `{"success":true,"items":[
			{"appId":"reused-appid","cvmName":"crabbox-cbx-other00000000","status":"running"}
		]}`},
		// cvms get corroboration: name belongs to a different lease.
		{Stdout: `{"success":true,"app_id":"reused-appid","name":"crabbox-cbx-other00000000","status":"running"}`},
	}}
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	// list + get, but NO delete: the lease-name mismatch refuses the destroy.
	if len(runner.calls) != 2 ||
		strings.Join(runner.calls[1].args, " ") != "cvms get --cvm-id reused-appid --json" {
		t.Fatalf("calls=%#v (expected list+get, no delete)", runner.calls)
	}
	for _, c := range runner.calls {
		if len(c.args) > 1 && c.args[1] == "delete" {
			t.Fatalf("Cleanup deleted a foreign CVM on app_id reuse: calls=%#v", runner.calls)
		}
	}
	// The claim and key are retained (foreign reuse / transient must not drop them).
	claims, err := core.ListLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].LeaseID != leaseID {
		t.Fatalf("claim was dropped on a corroboration failure: claims=%#v", claims)
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
			wantCalls:  3,
			wantClaims: 0,
		},
		{
			name:       "rollback cleanup",
			recovery:   "rollback-cleanup",
			createdAt:  now,
			wantCalls:  3,
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
					{"appId":"recovery-cvm","cvmName":"crabbox-cbx-abcdef123456","status":"running"}
				]}`},
				// FIX B: Cleanup corroborates ownership via `cvms get` before delete.
				{Stdout: `{"success":true,"app_id":"recovery-cvm","name":"crabbox-cbx-abcdef123456","status":"running"}`},
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
		{
			// FIX C: a "not found" that is GATEWAY-scoped, not CVM-scoped, is a real
			// delete failure and must propagate so a live billing CVM is not orphaned.
			name:    "gateway endpoint not found is not an already-gone CVM",
			result:  core.LocalCommandResult{Stderr: "Error: gateway endpoint not found"},
			err:     errors.New("exit status 1"),
			wantErr: true,
		},
		{
			// An unambiguous CVM-scoped phrase is suppressed as already-gone.
			name:   "no such cvm is already gone",
			result: core.LocalCommandResult{Stderr: "Error: no such cvm: missing-cvm"},
			err:    errors.New("exit status 1"),
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
// real hardware: a {success,total,items:[...]} wrapper whose items carry ONLY
// appId, cvmName, status and uptime -- the CVM name is under `cvmName` (there
// is NO `name` key). The provider previously read the name from `name`/`appName`
// and so read a BLANK name for every real list item, silently breaking
// ownership and recovery.
const realCVMSListStdout = `{
  "success": true,
  "total": 1,
  "items": [
    {
      "appId": "b60d1f55eeb01f17e0a5220b4c03792248d49f92",
      "cvmName": "crabbox-cbx-abcdef123456",
      "status": "running",
      "uptime": "3 minutes"
    }
  ]
}`

// TestListParsesRealCamelCaseListPayload pins list parsing against the exact
// `cvms list --json` payload from real hardware: the item's appId, cvmName and
// status must all decode (reading the name from `name` only would have read a
// blank), and cloudID() must return the canonical app_id.
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
	// The list item carries the name under `cvmName`, NOT `name`.
	if item.Name != "crabbox-cbx-abcdef123456" {
		t.Fatalf("name=%q not decoded from cvmName", item.Name)
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
		// The real `cvms list` item carries the name under `cvmName`, not `name`.
		{name: "real list cvmName", json: `{"appId":"app1","vmUuid":"vm1","instanceId":"in1","cvmName":"n1","status":"running"}`},
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
		{"appId":"keeper","cvmName":"crabbox-cbx-abcdef123456","status":"running"}
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

// TestResolveGatewayHostParsesRealCVMSGetShape pins resolveGatewayHost against
// the EXACT snake_case `phala cvms get --cvm-id <id> --json` payload observed on
// real TDX hardware: top-level app_id/vm_uuid/name plus a nested
// gateway.base_domain. The cached host must be <appId>-22.<base_domain>, matching
// the proxy-side resolver so the cached and fallback hosts are identical.
func TestResolveGatewayHostParsesRealCVMSGetShape(t *testing.T) {
	const realGetStdout = `{
  "success": true,
  "app_id": "b60d1f55eeb01f17e0a5220b4c03792248d49f92",
  "vm_uuid": "42fd1f82-7b4c-47cc-92f9-a5d39476c649",
  "name": "crabbox-cbx-abcdef123456",
  "status": "running",
  "gateway": {
    "base_domain": "dstack-pha-prod5.phala.network",
    "cname": "abc.cname.phala.network"
  }
}`
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: realGetStdout}}}
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	host, err := b.resolveGatewayHost(context.Background(), "b60d1f55eeb01f17e0a5220b4c03792248d49f92")
	if err != nil {
		t.Fatalf("resolveGatewayHost failed on real cvms get shape: %v", err)
	}
	const want = "b60d1f55eeb01f17e0a5220b4c03792248d49f92-22.dstack-pha-prod5.phala.network"
	if host != want {
		t.Fatalf("host=%q want %q", host, want)
	}
	if strings.Join(runner.calls[0].args, " ") != "cvms get --cvm-id b60d1f55eeb01f17e0a5220b4c03792248d49f92 --json" {
		t.Fatalf("args=%q", strings.Join(runner.calls[0].args, " "))
	}
}

// TestResolveGatewayHostToleratesMissingAndIncomplete proves resolveGatewayHost
// is best-effort: a missing CVM and a payload missing the app id or gateway
// domain all return ("", nil) so the caller silently falls back to the
// per-connection proxy resolution rather than failing the acquire.
func TestResolveGatewayHostToleratesMissingAndIncomplete(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	for _, test := range []struct {
		name   string
		result core.LocalCommandResult
		err    error
	}{
		{name: "missing CVM", result: core.LocalCommandResult{Stderr: "Error: CVM not found"}, err: errors.New("exit status 1")},
		{name: "no gateway domain", result: core.LocalCommandResult{Stdout: `{"success":true,"app_id":"app1","gateway":{}}`}},
		{name: "no app id", result: core.LocalCommandResult{Stdout: `{"success":true,"gateway":{"base_domain":"gw.example"}}`}},
		{name: "non-json output", result: core.LocalCommandResult{Stdout: "libuv: assertion failed"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{results: []core.LocalCommandResult{test.result}}
			if test.err != nil {
				runner.errs = []error{test.err}
			}
			b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
			host, err := b.resolveGatewayHost(context.Background(), "cvm-1")
			if err != nil {
				t.Fatalf("expected best-effort nil error, got %v", err)
			}
			if host != "" {
				t.Fatalf("host=%q want empty for best-effort fallback", host)
			}
		})
	}
}

// TestGatewayHostRoundTripsThroughClaimToProxyCommand proves a gateway_host
// recorded on the lease claim at acquire time is surfaced back onto item.Labels
// during resolution (via phalaLabels) and baked into the SSH ProxyCommand as
// --gateway-host, so the cached host survives the claim round-trip and every SSH
// connection skips the per-connection cvms-get call.
func TestGatewayHostRoundTripsThroughClaimToProxyCommand(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.IdleTimeout = 5 * time.Minute
	applyDefaults(&cfg)
	leaseID := "cbx_abcdef123456"
	const host = "b60d1f55-22.dstack-pha-prod5.phala.network"
	labels := core.DirectLeaseLabels(cfg, leaseID, "blue-box", providerName, "", false, time.Now())
	labels["phala_cvm"] = "cvm-1"
	labels["gateway_host"] = host
	server := core.Server{CloudID: "cvm-1", Provider: providerName, Name: "blue-box", Labels: labels}
	target := core.SSHTarget{Host: "cvm-1", User: "root", Port: "22"}
	if err := core.ClaimLeaseTargetForConfig(leaseID, "blue-box", cfg, server, target, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	// phalaLabels resolves the claim by cloud id and must surface gateway_host.
	item := instance{ID: "cvm-1"}
	item.Labels = phalaLabels(item, cfg)
	if item.Labels["gateway_host"] != host {
		t.Fatalf("gateway_host not surfaced from claim: labels=%v", item.Labels)
	}
	b := &backend{cfg: cfg, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	lease := b.lease(item, cfg, leaseID)
	if !strings.Contains(lease.SSH.ProxyCommand, "--gateway-host "+host) {
		t.Fatalf("ProxyCommand=%q missing cached --gateway-host %q", lease.SSH.ProxyCommand, host)
	}
}

// TestReleaseDestroysOwnedCVM drives the ONLY branch of validateDestroyTarget
// that issues a delete: the present-and-owned success path (`return true, nil`).
// The sibling release tests all exercise the REFUSAL branches (foreign name,
// mismatched lease, missing CVM, transient failure), so a mutation that flips
// validateDestroyTarget's final `return true` to `return false` would otherwise
// survive -- no test asserts a delete is actually issued. Here a local claim
// maps the lease to a cloud id, the `cvms get` payload names a crabbox- CVM
// whose name encodes THIS lease, so the destroy proceeds: assert the
// `cvms delete --cvm-id <id> --force` call WAS issued, the release returned nil,
// and the claim + stored testbox key were reaped.
func TestReleaseDestroysOwnedCVM(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	leaseID := "cbx_abcdef123456"
	const cloudID = "owned"
	claimPhalaLease(t, cfg, leaseID, cloudID)
	// ReleaseLease's call sequence for an OWNED, present CVM:
	//   1. validateDestroyTarget -> getInstance -> `cvms get --cvm-id <id> --json`
	//      returns the real FLAT snake_case object whose `name` is the crabbox-
	//      CVM name encoding THIS lease, so ownership corroborates and the success
	//      branch (`return true, nil`) is taken.
	//   2. destroy -> `cvms delete --cvm-id <id> --force` succeeds.
	runner := &fakeRunner{results: []core.LocalCommandResult{
		{Stdout: `{"success":true,"app_id":"owned","vm_uuid":"42fd1f82","id":"owned","instance_id":"in1","name":"crabbox-cbx-abcdef123456","status":"running","gateway":{"base_domain":"dstack-pha-prod5.phala.network"}}`},
		{},
	}}
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: leaseID,
		Server:  core.Server{CloudID: cloudID},
	}}); err != nil {
		t.Fatalf("ReleaseLease on an owned present CVM must succeed: %v", err)
	}
	// The success branch must have issued a delete (this is the assertion the
	// `return true->false` mutation breaks).
	const wantGet = "cvms get --cvm-id owned --json"
	const wantDelete = "cvms delete --cvm-id owned --force"
	if len(runner.calls) != 2 ||
		strings.Join(runner.calls[0].args, " ") != wantGet ||
		strings.Join(runner.calls[1].args, " ") != wantDelete {
		t.Fatalf("expected get+delete, calls=%#v", runner.calls)
	}
	deleted := false
	for _, c := range runner.calls {
		if strings.Join(c.args, " ") == wantDelete {
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("no `cvms delete --cvm-id owned --force` was issued: calls=%#v", runner.calls)
	}
	// The CVM is gone, so the claim and stored testbox key are reaped: the claim
	// is no longer resolvable.
	if _, ok, err := resolvePhalaClaim(leaseID, cfg); err != nil || ok {
		t.Fatalf("claim still resolvable after destroy: ok=%t err=%v", ok, err)
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("claim not reaped after destroy: claims=%#v", claims)
	}
}

// acquireDeployStdout is a real `phala deploy --json` stdout (leading
// "Provisioning CVM ..." progress line then the snake_case object) whose app_id
// is the canonical CVM handle Acquire threads through resolveGatewayHost,
// findInstance and -- on a post-create failure -- rollback's `cvms delete`.
const acquireDeployStdout = `Provisioning CVM ...
{
  "success": true,
  "vm_uuid": "42fd1f82-7b4c-47cc-92f9-a5d39476c649",
  "name": "crabbox-cbx-acquire000000",
  "app_id": "acq11f55eeb01f17e0a5220b4c03792248d49f92",
  "dashboard_url": "https://cloud.phala.com/dashboard/cvms/42fd1f82"
}`

const acquireAppID = "acq11f55eeb01f17e0a5220b4c03792248d49f92"

// TestAcquireHappyPath drives Acquire through every CLI call it makes up to the
// SSH bootstrap boundary and asserts the create/resolve sequence threads the
// canonical app_id through correctly.
//
// TESTABILITY GAP (documented, not forced): a fully successful Acquire (claim
// written, ready LeaseTarget returned) is NOT reachable in a unit test for this
// provider. After create succeeds, Acquire calls prepareSSH, which runs the REAL
// tool bootstrap over SSH (core.WaitForSSHReady + core.RunSSHQuiet) against the
// just-created CVM. Unlike the DigitalOcean backend -- whose struct exposes an
// injectable `waitSSH func(...)` field that its Acquire tests override with a
// no-op -- the Phala backend wires WaitForSSHReady/RunSSHQuiet directly with no
// seam, and this change set may touch ONLY backend_test.go, so no stub can be
// injected. The only way to make prepareSSH terminate without a live host is a
// cancelled context, which makes WaitForSSHReady return the cancellation
// immediately (proven by TestResolveStatusOnlyReadyProbeSkipsBootstrap's control
// case) -- and that necessarily routes Acquire into its rollback path. So the
// furthest reachable point on a "happy" create is the SSH boundary: this test
// queues a SUCCESSFUL create + gateway-resolve + findInstance, then lets
// prepareSSH fail on the cancelled context, and asserts (a) the create reached
// findInstance with the canonical app_id (the four pre-SSH CLI calls ran in
// order) and (b) the post-create failure deletes exactly the just-created CVM by
// that app_id so no confidential VM leaks. Closing this gap so the happy path
// can assert a written claim + ready lease would require a `waitSSH`-style seam
// on the backend struct (a production change), mirroring DigitalOcean.
func TestAcquireHappyPath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Phala.Compose = "/srv/compose.yml"
	applyDefaults(&cfg)
	// Acquire's CLI sequence (the fakeRunner is shared across every b.phala call):
	//   1. listInstances            -> `cvms list --json`           (no owned CVMs)
	//   2. create                   -> `deploy --json ...`          (app_id=acq...)
	//   3. resolveGatewayHost(id)   -> `cvms get --cvm-id <id> --json`
	//   4. findInstance(id)         -> `cvms list --json`           (the new CVM)
	//   5. prepareSSH               -> WaitForSSHReady fails (cancelled ctx)
	//   6. rollback -> destroy      -> `cvms delete --cvm-id <id> --force`
	listAfterCreate := `{"success":true,"items":[{"appId":"` + acquireAppID + `","cvmName":"crabbox-cbx-acquire000000","status":"running"}]}`
	runner := &fakeRunner{results: []core.LocalCommandResult{
		{Stdout: `{"success":true,"items":[]}`},
		{Stdout: acquireDeployStdout},
		{Stdout: `{"success":true,"app_id":"` + acquireAppID + `","name":"crabbox-cbx-acquire000000","status":"running","gateway":{"base_domain":"dstack-pha-prod5.phala.network"}}`},
		{Stdout: listAfterCreate},
		{}, // rollback delete
	}}
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := b.Acquire(ctx, core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "blue-box"})
	// prepareSSH fails on the cancelled context, so Acquire returns that error
	// after rolling back. (A fully successful Acquire is unreachable without an
	// SSH seam -- see the doc comment above.)
	if err == nil {
		t.Fatal("Acquire on a cancelled context must surface the prepareSSH failure")
	}
	// The four pre-SSH calls ran in order, proving create+gateway+findInstance
	// threaded the canonical app_id through.
	wantPreSSH := []string{
		"cvms list --json",
		"deploy --json", // prefix-checked below
		"cvms get --cvm-id " + acquireAppID + " --json",
		"cvms list --json",
	}
	if len(runner.calls) < 4 {
		t.Fatalf("Acquire did not reach the SSH boundary: calls=%#v", runner.calls)
	}
	if got := strings.Join(runner.calls[0].args, " "); got != wantPreSSH[0] {
		t.Fatalf("call 0 = %q want %q", got, wantPreSSH[0])
	}
	if got := strings.Join(runner.calls[1].args, " "); !strings.HasPrefix(got, "deploy --json") {
		t.Fatalf("call 1 = %q want a `deploy --json` invocation", got)
	}
	if got := strings.Join(runner.calls[2].args, " "); got != wantPreSSH[2] {
		t.Fatalf("call 2 (gateway resolve) = %q want %q", got, wantPreSSH[2])
	}
	if got := strings.Join(runner.calls[3].args, " "); got != wantPreSSH[3] {
		t.Fatalf("call 3 (findInstance) = %q want %q", got, wantPreSSH[3])
	}
	// The post-create failure must delete exactly the just-created CVM so no
	// confidential VM leaks.
	wantDelete := "cvms delete --cvm-id " + acquireAppID + " --force"
	deleted := false
	for _, c := range runner.calls {
		if strings.Join(c.args, " ") == wantDelete {
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("rollback did not delete the created CVM %s: calls=%#v", acquireAppID, runner.calls)
	}
}

// TestAcquireRollbackDestroysLeakedCVM pins the rollback safety property: a
// POST-create failure (here prepareSSH failing on a cancelled context) must
// issue a `cvms delete` for the just-created CVM so a confidential VM is never
// leaked, and -- since req.Keep is false -- must NOT persist a recovery claim.
// This mirrors the DigitalOcean TestAcquireRollsBackDropletAndKeyOnSSHFailure,
// except the SSH failure is induced via the cancelled context rather than an
// injectable waitSSH stub (the Phala backend exposes no such seam; see
// TestAcquireHappyPath's doc comment).
// TestClaimAcquiredLeasePersistsNonRepoClaim pins the ClawSweeper P1: a
// successful acquire WITHOUT a repo root (e.g. `warmup`, or `run` outside a
// repo) must still write the local ownership claim -- the sole anchor List/
// stop/Cleanup use -- instead of silently no-opping through the repo-only claim
// writer and orphaning a live, billing, unmanageable CVM.
func TestClaimAcquiredLeasePersistsNonRepoClaim(t *testing.T) {
	for _, repoRoot := range []string{"", "  "} {
		t.Run("repoRoot="+repoRoot, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			cfg := core.BaseConfig()
			cfg.Provider = providerName
			applyDefaults(&cfg)
			leaseID := "cbx_nonrepo01234"
			lease := core.LeaseTarget{
				LeaseID: leaseID,
				Server: core.Server{
					CloudID:  "appid-nonrepo",
					Provider: providerName,
					Labels: map[string]string{
						"crabbox": "true", "created_by": "crabbox",
						"lease": leaseID, "provider": providerName, "slug": "green-box",
					},
				},
			}
			if err := claimAcquiredLease(leaseID, "green-box", cfg, lease, repoRoot, false); err != nil {
				t.Fatalf("claimAcquiredLease(repoRoot=%q): %v", repoRoot, err)
			}
			claim, ok, err := resolvePhalaClaim(leaseID, cfg)
			if err != nil || !ok {
				t.Fatalf("no local claim written for a non-repo acquire (ok=%v err=%v) -- CVM would be unmanageable", ok, err)
			}
			if claim.CloudID != "appid-nonrepo" {
				t.Fatalf("claim CloudID=%q want appid-nonrepo", claim.CloudID)
			}
			if claim.Slug != "green-box" {
				t.Fatalf("claim Slug=%q want green-box", claim.Slug)
			}
		})
	}
}

func TestAcquireRollbackDestroysLeakedCVM(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Phala.Compose = "/srv/compose.yml"
	applyDefaults(&cfg)
	listAfterCreate := `{"success":true,"items":[{"appId":"` + acquireAppID + `","cvmName":"crabbox-cbx-acquire000000","status":"running"}]}`
	runner := &fakeRunner{results: []core.LocalCommandResult{
		{Stdout: `{"success":true,"items":[]}`}, // listInstances
		{Stdout: acquireDeployStdout},           // create
		{Stdout: `{"success":true,"app_id":"` + acquireAppID + `","gateway":{"base_domain":"gw.example"}}`}, // resolveGatewayHost
		{Stdout: listAfterCreate}, // findInstance
		{},                        // rollback delete (succeeds)
	}}
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := b.Acquire(ctx, core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "rollback-box"})
	if err == nil {
		t.Fatal("post-create prepareSSH failure must surface as an Acquire error")
	}
	// The leaked CVM must be destroyed by its app_id.
	wantDelete := "cvms delete --cvm-id " + acquireAppID + " --force"
	deletes := 0
	for _, c := range runner.calls {
		if strings.Join(c.args, " ") == wantDelete {
			deletes++
		}
	}
	if deletes != 1 {
		t.Fatalf("expected exactly one rollback `%s`, calls=%#v", wantDelete, runner.calls)
	}
	// req.Keep is false, so a successful rollback-destroy leaves NO recovery claim
	// behind (the claim is only persisted when the destroy itself fails).
	claims, err := core.ListLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("rollback left a claim behind despite a successful destroy: claims=%#v", claims)
	}
}

// TestResolveGatewayHostDomainPreferenceTable pins the firstNonBlank preference
// the gatewayGetOutput parser applies to BOTH the gateway domain and the app id.
// The only sibling test (TestResolveGatewayHostParsesRealCVMSGetShape) feeds a
// nested gateway.base_domain, so a reorder of any other spelling -- nested
// gateway_domain, nested domain, top-level gateway_domain, the appId/id/
// instance_id app-id fallbacks, or the nested cvm wrapper -- would survive. This
// drives each spelling through b.resolveGatewayHost (the production parse path,
// via a fakeRunner returning the crafted `cvms get` payload) and asserts the
// derived host is `<app_id>-22.<expected_domain>` for each, including that a
// nested gateway_domain wins over a same-payload base_domain and that the app-id
// preference (app_id > appId > id > instance_id) holds.
func TestResolveGatewayHostDomainPreferenceTable(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{
			// gatewayDomain ranks gateway.gateway_domain ahead of base_domain, so
			// the nested gateway_domain wins when both are present.
			name: "nested gateway_domain wins over base_domain",
			body: `{"success":true,"app_id":"app1","gateway":{"gateway_domain":"win.example","base_domain":"lose.example"}}`,
			want: "app1-22.win.example",
		},
		{
			name: "nested base_domain",
			body: `{"success":true,"app_id":"app1","gateway":{"base_domain":"base.example"}}`,
			want: "app1-22.base.example",
		},
		{
			name: "nested domain",
			body: `{"success":true,"app_id":"app1","gateway":{"domain":"plain.example"}}`,
			want: "app1-22.plain.example",
		},
		{
			name: "top-level gateway_domain",
			body: `{"success":true,"app_id":"app1","gateway_domain":"top.example"}`,
			want: "app1-22.top.example",
		},
		{
			// app_id is absent, so the camelCase appId alias supplies the host label.
			name: "camelCase appId fallback",
			body: `{"success":true,"appId":"camel1","gateway":{"base_domain":"camel.example"}}`,
			want: "camel1-22.camel.example",
		},
		{
			// app_id/appId absent: the id fallback supplies the host label.
			name: "id app-id fallback",
			body: `{"success":true,"id":"idonly","gateway":{"base_domain":"id.example"}}`,
			want: "idonly-22.id.example",
		},
		{
			// app_id/appId/id absent: the instance_id fallback supplies the label.
			name: "instance_id app-id fallback",
			body: `{"success":true,"instance_id":"inst1","gateway":{"base_domain":"inst.example"}}`,
			want: "inst1-22.inst.example",
		},
		{
			// Both the app id and the gateway domain live ONLY on the nested cvm
			// wrapper, so the parser must fall through into it for each.
			name: "nested cvm wrapper",
			body: `{"success":true,"cvm":{"app_id":"nested1","gateway":{"gateway_domain":"nested.example"}}}`,
			want: "nested1-22.nested.example",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: test.body}}}
			b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
			host, err := b.resolveGatewayHost(context.Background(), "cvm-1")
			if err != nil {
				t.Fatalf("resolveGatewayHost failed: %v", err)
			}
			if host != test.want {
				t.Fatalf("host=%q want %q", host, test.want)
			}
			if got := strings.Join(runner.calls[0].args, " "); got != "cvms get --cvm-id cvm-1 --json" {
				t.Fatalf("args=%q", got)
			}
		})
	}
}
