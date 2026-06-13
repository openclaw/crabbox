package namespaceinstance

import (
	"context"
	"errors"
	"flag"
	"io"
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
	if provider.Name() != providerName || !reflect.DeepEqual(provider.Aliases(), []string{"namespace-compute"}) {
		t.Fatalf("provider identity=%q aliases=%v", provider.Name(), provider.Aliases())
	}
	spec := provider.Spec()
	if spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever ||
		!spec.Features.Has(core.FeatureSSH) || !spec.Features.Has(core.FeatureCrabboxSync) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("spec=%#v", spec)
	}
}

func TestProviderRejectsSecretEndpointAndNonLinuxMachineType(t *testing.T) {
	provider := Provider{}
	for _, test := range []struct {
		name string
		cfg  core.Config
		want string
	}{
		{
			name: "endpoint credentials",
			cfg: func() core.Config {
				cfg := core.BaseConfig()
				cfg.Provider = providerName
				cfg.NamespaceInstance.Endpoint = "https://user:secret@api.example.test"
				return cfg
			}(),
			want: "must not contain credentials",
		},
		{
			name: "non-linux machine",
			cfg: func() core.Config {
				cfg := core.BaseConfig()
				cfg.Provider = providerName
				cfg.NamespaceInstance.MachineType = "darwin/arm64:4x8"
				return cfg
			}(),
			want: "supports Linux machine types only",
		},
		{
			name: "unsafe work root",
			cfg: func() core.Config {
				cfg := core.BaseConfig()
				cfg.Provider = providerName
				cfg.NamespaceInstance.WorkRoot = "../../etc"
				return cfg
			}(),
			want: "canonical absolute Linux path",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := provider.Configure(test.cfg, core.Runtime{}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.NamespaceInstance.Endpoint = "https://user:secret@api.example.test/%zz"
	if _, err := provider.Configure(cfg, core.Runtime{}); err == nil ||
		strings.Contains(err.Error(), "user") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("malformed endpoint err=%v", err)
	}
}

func TestProviderRejectsInvalidVolumeSpecs(t *testing.T) {
	for _, spec := range []string{
		"cache:go:/root/.cache/go-build",
		"other:go:/root/.cache/go-build:10Gi",
		"cache::/root/.cache/go-build:10Gi",
		"cache:go:relative:10Gi",
		"cache:go:/root/../cache:10Gi",
		"cache:go:/root/.cache/go-build:",
	} {
		t.Run(spec, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.Provider = providerName
			cfg.NamespaceInstance.Volumes = []string{spec}
			if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
				t.Fatalf("volume %q accepted", spec)
			}
		})
	}
}

func TestMachineTypeForClass(t *testing.T) {
	for _, test := range []struct {
		class string
		want  string
	}{
		{"standard", "4x8"},
		{"fast", "8x16"},
		{"large", "16x32"},
		{"beast", "32x64"},
		{"2x4", "2x4"},
	} {
		if got := machineTypeForClass(test.class); got != test.want {
			t.Fatalf("machineTypeForClass(%q)=%q want %q", test.class, got, test.want)
		}
	}
}

func TestFlagsApplyNamespaceInstanceOptions(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--namespace-instance-cli", "/opt/nsc",
		"--namespace-instance-machine-type", "2x4",
		"--namespace-instance-duration", "20m",
		"--namespace-instance-region", "eu",
		"--namespace-instance-volume", "cache:go:/root/.cache/go-build:10Gi",
		"--namespace-instance-work-root", "/workspace",
	}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.NamespaceInstance.CLIPath != "/opt/nsc" || cfg.ServerType != "2x4" ||
		cfg.NamespaceInstance.Duration != 20*time.Minute || cfg.NamespaceInstance.Region != "eu" ||
		cfg.NamespaceInstance.WorkRoot != "/workspace" ||
		!reflect.DeepEqual(cfg.NamespaceInstance.Volumes, []string{"cache:go:/root/.cache/go-build:10Gi"}) {
		t.Fatalf("cfg=%#v", cfg.NamespaceInstance)
	}
}

func TestCreateBuildsNSCArguments(t *testing.T) {
	runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: `{"instance_id":"i-test"}`}}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TTL = 15 * time.Minute
	cfg.NamespaceInstance = core.NamespaceInstanceConfig{
		CLIPath:     "/opt/nsc",
		MachineType: "4x8",
		Region:      "eu",
		Endpoint:    "https://api.example.test",
		Keychain:    "ci",
		Volumes:     []string{"cache:go:/root/.cache/go-build:10Gi"},
		Bare:        true,
	}
	applyDefaults(&cfg)
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	id, err := b.create(context.Background(), cfg, "/tmp/id.pub", map[string]string{
		"crabbox":     "true",
		"lease":       "cbx_abcdef123456",
		"provider":    providerName,
		"server_type": "4x8",
		"slug":        "blue-box",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "i-test" {
		t.Fatalf("id=%q", id)
	}
	got := strings.Join(runner.calls[0].args, " ")
	for _, want := range []string{
		"--endpoint https://api.example.test",
		"--region eu",
		"--keychain ci",
		"create --output json",
		"--ssh_key /tmp/id.pub",
		"--bare",
		"--machine_type 4x8",
		"--duration 15m0s",
		"--unique_tag crabbox-cbx-abcdef123456",
		"--label provider=namespace-instance",
		"--label server-type=4x8",
		"--volume cache:go:/root/.cache/go-build:10Gi",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("args=%q missing %q", got, want)
		}
	}
	if strings.Contains(got, "--ephemeral") {
		t.Fatalf("removed nsc flag leaked into args: %q", got)
	}
}

func TestCreateRecoversInstanceAfterInvalidOutput(t *testing.T) {
	runner := &fakeRunner{results: []core.LocalCommandResult{
		{Stdout: `not-json`},
		{Stdout: `[
			{"cluster_id":"recovered","labels":{"crabbox":"true","created-by":"crabbox","provider":"namespace-instance","lease":"cbx_abcdef123456","namespace-tenant":"tenant-test"}}
		]`},
	}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	cfg.NamespaceInstance.TenantID = "tenant-test"
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	id, err := b.create(context.Background(), cfg, "/tmp/id.pub", map[string]string{
		"crabbox":          "true",
		"created_by":       "crabbox",
		"lease":            "cbx_abcdef123456",
		"namespace_tenant": "tenant-test",
		"provider":         providerName,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "recovered" {
		t.Fatalf("id=%q", id)
	}
}

func TestCreateRetriesAmbiguousOutcomeRecovery(t *testing.T) {
	runner := &fakeRunner{
		results: []core.LocalCommandResult{
			{},
			{Stdout: "null\n"},
			{Stdout: `[
				{"cluster_id":"recovered","labels":{"crabbox":"true","created-by":"crabbox","provider":"namespace-instance","lease":"cbx_abcdef123456","namespace-tenant":"tenant-test"}}
			]`},
		},
		errs: []error{errors.New("connection reset")},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	cfg.NamespaceInstance.TenantID = "tenant-test"
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	id, err := b.create(context.Background(), cfg, "/tmp/id.pub", map[string]string{
		"crabbox":          "true",
		"created_by":       "crabbox",
		"lease":            "cbx_abcdef123456",
		"namespace_tenant": "tenant-test",
		"provider":         providerName,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "recovered" || len(runner.calls) != 3 {
		t.Fatalf("id=%q calls=%#v", id, runner.calls)
	}
}

func TestCreateRecoversAfterCallerCancellation(t *testing.T) {
	runner := &fakeRunner{
		results: []core.LocalCommandResult{
			{},
			{Stdout: `[
				{"cluster_id":"recovered","labels":{"crabbox":"true","created-by":"crabbox","provider":"namespace-instance","lease":"cbx_abcdef123456","namespace-tenant":"tenant-test"}}
			]`},
		},
		errs: []error{context.Canceled},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	cfg.NamespaceInstance.TenantID = "tenant-test"
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	id, err := b.create(ctx, cfg, "/tmp/id.pub", map[string]string{
		"crabbox":          "true",
		"created_by":       "crabbox",
		"lease":            "cbx_abcdef123456",
		"namespace_tenant": "tenant-test",
		"provider":         providerName,
	})
	if err != nil || id != "recovered" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}

func TestCreateDoesNotReconcileDefinitiveError(t *testing.T) {
	runner := &fakeRunner{
		results: []core.LocalCommandResult{{Stderr: "InvalidArgument: malformed volume"}},
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

func TestListAcceptsNullAndFiltersOwnership(t *testing.T) {
	runner := &fakeRunner{results: []core.LocalCommandResult{
		{Stdout: "null\n"},
		{Stdout: `[
			{"cluster_id":"owned","platform":["linux/amd64"],"labels":{"crabbox":"true","created-by":"crabbox","provider":"namespace-instance","lease":"cbx_abcdef123456","namespace-tenant":"tenant-test","slug":"owned"}},
			{"cluster_id":"foreign","labels":{"provider":"other"}}
		]`},
	}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	cfg.NamespaceInstance.TenantID = "tenant-test"
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	items, err := b.listInstances(context.Background())
	if err != nil || len(items) != 0 {
		t.Fatalf("null list items=%v err=%v", items, err)
	}
	views, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].DisplayID() != "owned" {
		t.Fatalf("views=%#v", views)
	}
}

func TestDoctorUsesSupportedNSCVersionCommand(t *testing.T) {
	runner := &fakeRunner{results: []core.LocalCommandResult{
		{Stdout: "v0.0.522\n"},
		{},
		{Stdout: "null\n"},
	}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	cfg.NamespaceInstance.TenantID = "tenant-test"
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	if _, err := b.Doctor(context.Background(), core.DoctorRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runner.calls[0].args, " "); got != "version" {
		t.Fatalf("version args=%q", got)
	}
	if got := strings.Join(runner.calls[1].args, " "); got != "auth check-login" {
		t.Fatalf("auth args=%q", got)
	}
}

func TestScopedConfigLoadsNamespaceTenant(t *testing.T) {
	runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: `{"tenant_id":"tenant-test","name":"Test"}`}}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	got, err := b.scopedConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.NamespaceInstance.TenantID != "tenant-test" {
		t.Fatalf("tenant_id=%q", got.NamespaceInstance.TenantID)
	}
	if args := strings.Join(runner.calls[0].args, " "); args != "workspace describe --output json" {
		t.Fatalf("args=%q", args)
	}
}

func TestProxyCommandPreservesConfiguredScope(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.NamespaceInstance.CLIPath = "/Applications/Namespace CLI/nsc"
	cfg.NamespaceInstance.Endpoint = "https://api.example.test"
	cfg.NamespaceInstance.Region = "eu west"
	got := proxyCommand(cfg, "instance-1")
	for _, want := range []string{
		`"/Applications/Namespace CLI/nsc"`,
		"--endpoint https://api.example.test",
		`--region "eu west"`,
		"instance-1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("proxy=%q missing %q", got, want)
		}
	}
}

func TestProxyCommandEscapesOpenSSHPercentTokens(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.NamespaceInstance.CLIPath = "/opt/nsc%test"
	cfg.NamespaceInstance.Endpoint = "https://api.example.test/%2F"
	got := proxyCommand(cfg, "instance-1")
	if !strings.Contains(got, "/opt/nsc%%test") || !strings.Contains(got, "https://api.example.test/%%2F") {
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

func TestServerPersistsConfiguredWorkRoot(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.NamespaceInstance.WorkRoot = "/workspace/custom"
	applyDefaults(&cfg)
	b := &backend{}
	server := b.server(instance{ClusterID: "instance-1", Labels: map[string]string{
		"crabbox":  "true",
		"provider": providerName,
	}}, cfg)
	if server.Labels["work_root"] != "/workspace/custom" {
		t.Fatalf("work_root=%q", server.Labels["work_root"])
	}
}

func TestFilterNamespaceClaimsUsesProviderScope(t *testing.T) {
	claims := []core.LeaseClaim{
		{LeaseID: "matching", Provider: providerName, ProviderScope: "region:eu", CloudID: "instance-eu"},
		{LeaseID: "other-scope", Provider: providerName, ProviderScope: "region:us", CloudID: "instance-us"},
		{LeaseID: "other-provider", Provider: "other", ProviderScope: "region:eu", CloudID: "instance-other"},
	}
	for i := range claims {
		if claims[i].Labels == nil {
			claims[i].Labels = map[string]string{}
		}
		claims[i].Labels["namespace_tenant"] = "tenant-test"
	}
	got := filterNamespaceClaims(claims, "region:eu", "tenant-test")
	if len(got) != 1 || got["matching"].CloudID != "instance-eu" {
		t.Fatalf("claims=%#v", got)
	}
}

func TestOwnedRequiresLeaseCreatorAndTenantLabels(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.NamespaceInstance.TenantID = "tenant-test"
	item := instance{Labels: map[string]string{
		"crabbox":          "true",
		"created_by":       "crabbox",
		"lease":            "cbx_abcdef123456",
		"namespace_tenant": "tenant-test",
		"provider":         providerName,
	}}
	if !owned(item, cfg) {
		t.Fatal("complete ownership labels rejected")
	}
	delete(item.Labels, "lease")
	if owned(item, cfg) {
		t.Fatal("instance without lease label accepted")
	}
}

func TestNamespaceRemainingLifetimeCapsAtOriginalTTL(t *testing.T) {
	now := time.Unix(1_000, 0).UTC()
	labels := map[string]string{
		"created_at": "900",
		"ttl_secs":   "300",
	}
	if got := namespaceRemainingLifetime(labels, now); got != 200*time.Second {
		t.Fatalf("remaining=%s", got)
	}
	if got := namespaceRemainingLifetime(labels, time.Unix(1_300, 0).UTC()); got != 0 {
		t.Fatalf("expired remaining=%s", got)
	}
}

func TestNamespaceRecoveryPendingUsesGraceWindow(t *testing.T) {
	now := time.Unix(1_000, 0).UTC()
	claim := core.LeaseClaim{Labels: map[string]string{"created_at": "900"}}
	if !namespaceRecoveryPending(claim, now) {
		t.Fatal("recent recovery claim not pending")
	}
	if namespaceRecoveryPending(claim, time.Unix(1_300, 0).UTC()) {
		t.Fatal("expired recovery grace still pending")
	}
}

func TestTouchPersistsUpdatedLabelsToClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.IdleTimeout = 5 * time.Minute
	cfg.NamespaceInstance.Duration = 20 * time.Minute
	applyDefaults(&cfg)
	cfg.NamespaceInstance.TenantID = "tenant-test"
	leaseID := "cbx_abcdef123456"
	server := core.Server{
		CloudID:  "instance-1",
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
	target := core.SSHTarget{Host: "instance-1", User: "root", Port: "22"}
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

func TestReleaseRejectsForeignInstance(t *testing.T) {
	runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: `[
		{"cluster_id":"foreign","labels":{"provider":"other"}}
	]`}}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	cfg.NamespaceInstance.TenantID = "tenant-test"
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_abcdef123456",
		Server:  core.Server{CloudID: "foreign"},
	}})
	if err == nil || !strings.Contains(err.Error(), "without Crabbox ownership labels") {
		t.Fatalf("err=%v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestReleaseRejectsMismatchedLeaseLabel(t *testing.T) {
	runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: `[
		{"cluster_id":"owned","labels":{"crabbox":"true","created-by":"crabbox","provider":"namespace-instance","lease":"cbx_other","namespace-tenant":"tenant-test"}}
	]`}}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	cfg.NamespaceInstance.TenantID = "tenant-test"
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_abcdef123456",
		Server:  core.Server{CloudID: "owned"},
	}})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err=%v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestReleaseOnlyResolveAllowsExpiredClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	cfg.NamespaceInstance.TenantID = "tenant-test"
	leaseID := "cbx_abcdef123456"
	server := core.Server{
		CloudID:  "expired-instance",
		Provider: providerName,
		Labels: map[string]string{
			"crabbox":          "true",
			"created_by":       "crabbox",
			"lease":            leaseID,
			"namespace_tenant": "tenant-test",
			"provider":         providerName,
			"slug":             "expired",
		},
	}
	if err := core.ClaimLeaseTargetForConfig(leaseID, "expired", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: "null\n"}}}
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	item, resolvedLeaseID, err := b.resolve(context.Background(), leaseID, cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if item.ClusterID != "expired-instance" || resolvedLeaseID != leaseID {
		t.Fatalf("item=%#v lease=%q", item, resolvedLeaseID)
	}
}

func TestResolveExactInstanceIDOutranksClaimSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	cfg.NamespaceInstance.TenantID = "tenant-test"
	claimLabels := map[string]string{
		"crabbox":          "true",
		"created_by":       "crabbox",
		"lease":            "cbx_aaaaaaaaaaaa",
		"namespace_tenant": "tenant-test",
		"provider":         providerName,
		"slug":             "instance-exact",
	}
	if err := core.ClaimLeaseTargetForConfig(
		"cbx_aaaaaaaaaaaa",
		"instance-exact",
		cfg,
		core.Server{CloudID: "instance-slug", Provider: providerName, Labels: claimLabels},
		core.SSHTarget{},
		cfg.IdleTimeout,
	); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: `[
		{"cluster_id":"instance-exact","labels":{"crabbox":"true","created-by":"crabbox","lease":"cbx_bbbbbbbbbbbb","namespace-tenant":"tenant-test","provider":"namespace-instance","slug":"other"}},
		{"cluster_id":"instance-slug","labels":{"crabbox":"true","created-by":"crabbox","lease":"cbx_aaaaaaaaaaaa","namespace-tenant":"tenant-test","provider":"namespace-instance","slug":"instance-exact"}}
	]`}}}
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	item, leaseID, err := b.resolve(context.Background(), "instance-exact", cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if item.ClusterID != "instance-exact" || leaseID != "cbx_bbbbbbbbbbbb" {
		t.Fatalf("item=%#v lease=%q", item, leaseID)
	}
}

func TestCleanupTransitionsAndRemovesClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	cfg.NamespaceInstance.TenantID = "tenant-test"
	leaseID := "cbx_abcdef123456"
	labels := map[string]string{
		"crabbox":          "true",
		"created_by":       "crabbox",
		"expires_at":       "1",
		"lease":            leaseID,
		"namespace_tenant": "tenant-test",
		"provider":         providerName,
		"slug":             "expired",
	}
	server := core.Server{CloudID: "expired-instance", Provider: providerName, Labels: labels}
	if err := core.ClaimLeaseTargetForConfig(leaseID, "expired", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{results: []core.LocalCommandResult{
		{Stdout: `[
			{"cluster_id":"expired-instance","labels":{"crabbox":"true","created-by":"crabbox","expires-at":"1","lease":"cbx_abcdef123456","namespace-tenant":"tenant-test","provider":"namespace-instance","slug":"expired"}}
		]`},
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
	if len(runner.calls) != 2 || strings.Join(runner.calls[1].args, " ") != "destroy expired-instance --force" {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestCleanupDestroysOwnedExpiredInstanceWithoutLocalClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	cfg.NamespaceInstance.TenantID = "tenant-test"
	runner := &fakeRunner{results: []core.LocalCommandResult{
		{Stdout: `[
			{"cluster_id":"expired-instance","labels":{"crabbox":"true","created-by":"crabbox","expires-at":"1","lease":"cbx_abcdef123456","namespace-tenant":"tenant-test","provider":"namespace-instance","slug":"expired","state":"ready"}}
		]`},
		{},
	}}
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || strings.Join(runner.calls[1].args, " ") != "destroy expired-instance --force" {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestCleanupRetainsPendingAmbiguousCreateClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	cfg.NamespaceInstance.TenantID = "tenant-test"
	leaseID := "cbx_abcdef123456"
	labels := core.DirectLeaseLabels(cfg, leaseID, "pending", providerName, "", false, time.Now())
	labels["namespace_tenant"] = "tenant-test"
	labels["recovery"] = "ambiguous-create"
	labels["state"] = "provisioning"
	server := core.Server{Provider: providerName, Labels: labels}
	if err := core.ClaimLeaseTargetForConfig(leaseID, "pending", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{results: []core.LocalCommandResult{{Stdout: "null\n"}}}
	b := &backend{cfg: cfg, rt: core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].LeaseID != leaseID {
		t.Fatalf("claims=%#v", claims)
	}
}

func TestCleanupAppliesRecoveryPolicyToLiveInstances(t *testing.T) {
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
			createdAt:  now.Add(-namespaceAmbiguousCreateRecoveryGrace - time.Second),
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
			cfg.NamespaceInstance.TenantID = "tenant-test"
			leaseID := "cbx_abcdef123456"
			labels := core.DirectLeaseLabels(cfg, leaseID, "recovery", providerName, "", false, test.createdAt)
			labels["namespace_tenant"] = "tenant-test"
			labels["recovery"] = test.recovery
			labels["state"] = "provisioning"
			server := core.Server{CloudID: "recovery-instance", Provider: providerName, Labels: labels}
			if err := core.ClaimLeaseTargetForConfig(leaseID, "recovery", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
				t.Fatal(err)
			}
			runner := &fakeRunner{results: []core.LocalCommandResult{
				{Stdout: `[
					{"cluster_id":"recovery-instance","labels":{"crabbox":"true","created-by":"crabbox","lease":"cbx_abcdef123456","namespace-tenant":"tenant-test","provider":"namespace-instance","slug":"recovery"}}
				]`},
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

func TestNamespaceToolBootstrapUsesAlpinePythonPackage(t *testing.T) {
	command := namespaceToolBootstrapCommand()
	if !strings.Contains(command, "apk add --no-cache git rsync python3") {
		t.Fatalf("bootstrap command missing Alpine python3 package:\n%s", command)
	}
	if strings.Contains(command, "python-3.13") {
		t.Fatalf("bootstrap command uses invalid Alpine package:\n%s", command)
	}
}

func TestDestroyOnlySuppressesExplicitMissingInstanceResponse(t *testing.T) {
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
			name:   "provider reports missing instance",
			result: core.LocalCommandResult{Stderr: "Failed: missing-instance: does not exist."},
			err:    errors.New("exit status 1"),
		},
		{
			name:    "nsc executable missing",
			err:     errors.New(`exec: "nsc": executable file not found in $PATH`),
			wantErr: true,
		},
		{
			name:    "unrelated provider error",
			result:  core.LocalCommandResult{Stderr: "endpoint host not found"},
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
			err := b.destroy(context.Background(), "missing-instance")
			if (err != nil) != test.wantErr {
				t.Fatalf("err=%v wantErr=%t", err, test.wantErr)
			}
		})
	}
}
