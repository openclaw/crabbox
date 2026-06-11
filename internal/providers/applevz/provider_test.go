package applevz

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/crabbox/internal/applevzhelper"
	core "github.com/openclaw/crabbox/internal/cli"
)

type recordingRunner struct {
	calls     []core.LocalCommandRequest
	responses map[string]core.LocalCommandResult
	errors    map[string]error
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	key := commandKey(req.Name, req.Args)
	if err, ok := r.errors[key]; ok {
		return r.responses[key], err
	}
	if result, ok := r.responses[key]; ok {
		return result, nil
	}
	if len(req.Args) > 0 {
		shortKey := req.Name + "\x00" + req.Args[0]
		if err, ok := r.errors[shortKey]; ok {
			return r.responses[shortKey], err
		}
		if result, ok := r.responses[shortKey]; ok {
			return result, nil
		}
	}
	return core.LocalCommandResult{}, nil
}

func commandKey(name string, args []string) string {
	return name + "\x00" + strings.Join(args, "\x00")
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func testBackend(t *testing.T, runner *recordingRunner) *backend {
	t.Helper()
	oldGOOS, oldGOARCH := hostGOOS, hostGOARCH
	hostGOOS, hostGOARCH = "darwin", "arm64"
	t.Cleanup(func() {
		hostGOOS, hostGOARCH = oldGOOS, oldGOARCH
	})
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
	root := t.TempDir()
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleVZ = core.AppleVZConfig{
		HelperPath: "/tmp/helper-source",
		Image:      "https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-arm64.img",
		User:       "runner",
		WorkRoot:   "/workspace/crabbox",
		CPUs:       4,
		MemoryMiB:  8192,
		DiskGiB:    40,
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	b.prepareHelper = func(context.Context, core.Config) (string, error) { return "helper", nil }
	b.stateRoot = func() (string, error) { return root, nil }
	b.waitForSSH = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	return b
}

func TestProviderSpecAndAliases(t *testing.T) {
	p := Provider{}
	if p.Name() != providerName {
		t.Fatalf("Name=%q want %s", p.Name(), providerName)
	}
	for _, alias := range []string{"apple-vz", "applevz"} {
		got, err := core.ProviderFor(alias)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", alias, err)
		}
		if got.Name() != providerName {
			t.Fatalf("ProviderFor(%q).Name=%q", alias, got.Name())
		}
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindSSHLease || spec.Family != "local-vm" {
		t.Fatalf("unexpected spec: %#v", spec)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}
}

func TestApplyDefaults(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleVZ = core.AppleVZConfig{}
	applyDefaults(&cfg)
	if cfg.AppleVZ.User != "crabbox" || cfg.AppleVZ.WorkRoot != "/work/crabbox" || cfg.AppleVZ.CPUs != 4 || cfg.AppleVZ.MemoryMiB != 8192 || cfg.AppleVZ.DiskGiB != 30 {
		t.Fatalf("defaults not applied: %#v", cfg.AppleVZ)
	}
	if cfg.SSHUser != "crabbox" || cfg.SSHPort != "22" || cfg.WorkRoot != "/work/crabbox" {
		t.Fatalf("derived SSH defaults wrong: user=%q port=%q work=%q", cfg.SSHUser, cfg.SSHPort, cfg.WorkRoot)
	}
}

func TestApplyDefaultsHonorsGlobalWorkRoot(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.WorkRoot = "/custom/crabbox"
	applyDefaults(&cfg)
	if cfg.WorkRoot != "/custom/crabbox" || cfg.AppleVZ.WorkRoot != "/custom/crabbox" {
		t.Fatalf("work root=%q apple-vz=%q want /custom/crabbox", cfg.WorkRoot, cfg.AppleVZ.WorkRoot)
	}

	cfg = core.BaseConfig()
	cfg.Provider = providerName
	cfg.WorkRoot = "/custom/crabbox"
	cfg.AppleVZ.WorkRoot = "/work/apple-vz"
	applyDefaults(&cfg)
	if cfg.WorkRoot != "/work/apple-vz" || cfg.AppleVZ.WorkRoot != "/work/apple-vz" {
		t.Fatalf("specific work root=%q apple-vz=%q want /work/apple-vz", cfg.WorkRoot, cfg.AppleVZ.WorkRoot)
	}
}

func TestApplyFlags(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("apple-vz", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--apple-vz-helper", "/opt/bin/helper",
		"--apple-vz-image", "https://example.test/custom.img",
		"--apple-vz-user", "ci",
		"--apple-vz-work-root", "/work/ci",
		"--apple-vz-cpus", "6",
		"--apple-vz-memory", "12288",
		"--apple-vz-disk", "64",
	}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.AppleVZ.HelperPath != "/opt/bin/helper" || cfg.AppleVZ.Image != "https://example.test/custom.img" || cfg.AppleVZ.User != "ci" || cfg.AppleVZ.WorkRoot != "/work/ci" || cfg.AppleVZ.CPUs != 6 || cfg.AppleVZ.MemoryMiB != 12288 || cfg.AppleVZ.DiskGiB != 64 {
		t.Fatalf("flags not applied: %#v", cfg.AppleVZ)
	}
	if !core.AppleVZImageExplicit(cfg) {
		t.Fatal("apple-vz image should be explicit after --apple-vz-image")
	}
}

func TestDoctorReady(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey("helper", []string{"doctor", "--state-root", "", "--image", "https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-arm64.img"}): {Stdout: mustJSON(t, applevzhelper.DoctorResponse{
			Status:    "ok",
			Message:   "runtime ready",
			Instances: 2,
			Details:   map[string]string{"runtime": "virtualization.framework"},
		})},
	}}
	b := testBackend(t, runner)
	root, _ := b.stateRoot()
	runner.responses[commandKey("helper", []string{"doctor", "--state-root", root, "--image", b.configForRun().AppleVZ.Image})] = runner.responses[commandKey("helper", []string{"doctor", "--state-root", "", "--image", b.configForRun().AppleVZ.Image})]
	delete(runner.responses, commandKey("helper", []string{"doctor", "--state-root", "", "--image", b.configForRun().AppleVZ.Image}))
	result, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Message, "leases=2") || !strings.Contains(result.Message, "virtualization.framework") {
		t.Fatalf("unexpected doctor message: %s", result.Message)
	}
}

func TestAcquireResolveListAndRelease(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(t, runner)
	root, _ := b.stateRoot()
	name := "crabbox-cbx123-demo"
	startInstance := applevzhelper.Instance{
		Name:      name,
		LeaseID:   "cbx_fake123456",
		Slug:      "demo",
		Status:    applevzhelper.StatusRunning,
		Image:     b.configForRun().AppleVZ.Image,
		SSHUser:   b.configForRun().AppleVZ.User,
		WorkRoot:  b.configForRun().AppleVZ.WorkRoot,
		SSHHost:   "127.0.0.1",
		SSHPort:   43022,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	runner.responses[commandKey("helper", []string{
		"list", "--state-root", root,
	})] = core.LocalCommandResult{Stdout: mustJSON(t, applevzhelper.ListResponse{})}
	runner.responses["helper\x00start"] = core.LocalCommandResult{Stdout: mustJSON(t, applevzhelper.StartResponse{Instance: startInstance})}

	req := core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "demo"}
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != name || lease.SSH.Port != "43022" || lease.SSH.Host != "127.0.0.1" {
		t.Fatalf("unexpected lease target: %#v", lease)
	}

	listResp := applevzhelper.ListResponse{Instances: []applevzhelper.Instance{{
		Name:      name,
		LeaseID:   lease.LeaseID,
		Slug:      "demo",
		Status:    applevzhelper.StatusRunning,
		Image:     b.configForRun().AppleVZ.Image,
		SSHUser:   b.configForRun().AppleVZ.User,
		WorkRoot:  b.configForRun().AppleVZ.WorkRoot,
		SSHHost:   "127.0.0.1",
		SSHPort:   43022,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}}}
	runner.responses[commandKey("helper", []string{"list", "--state-root", root})] = core.LocalCommandResult{Stdout: mustJSON(t, listResp)}
	resolved, err := b.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.SSH.Port != "43022" || resolved.Server.CloudID != name {
		t.Fatalf("unexpected resolved target: %#v", resolved)
	}
	views, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].CloudID != name {
		t.Fatalf("unexpected list output: %#v", views)
	}

	runner.responses["helper\x00delete"] = core.LocalCommandResult{Stdout: mustJSON(t, applevzhelper.DeleteResponse{Deleted: true, Instance: listResp.Instances[0]})}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(lease.LeaseID, providerName); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("lease claim %s should have been removed", lease.LeaseID)
	}
}

func TestCleanupRemovesStoppedInstance(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(t, runner)
	root, _ := b.stateRoot()
	leaseID := "cbx_cleanup123456"
	slug := "cleanup-demo"
	name := core.LeaseProviderName(leaseID, slug)
	server := core.Server{CloudID: name, Provider: providerName, Name: name, Status: "stopped", Labels: map[string]string{
		"lease":    leaseID,
		"slug":     slug,
		"instance": name,
		"provider": providerName,
	}}
	target := core.SSHTarget{Host: "127.0.0.1", User: "runner", Port: "43022"}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, name, "", t.TempDir(), 5*time.Minute, false, server, target); err != nil {
		t.Fatal(err)
	}
	instance := applevzhelper.Instance{
		Name:      name,
		LeaseID:   leaseID,
		Slug:      slug,
		Status:    applevzhelper.StatusStopped,
		Image:     b.configForRun().AppleVZ.Image,
		SSHUser:   b.configForRun().AppleVZ.User,
		WorkRoot:  b.configForRun().AppleVZ.WorkRoot,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	runner.responses[commandKey("helper", []string{"list", "--state-root", root})] = core.LocalCommandResult{Stdout: mustJSON(t, applevzhelper.ListResponse{Instances: []applevzhelper.Instance{instance}})}
	runner.responses["helper\x00delete"] = core.LocalCommandResult{Stdout: mustJSON(t, applevzhelper.DeleteResponse{Deleted: true, Instance: instance})}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("cleanup should remove claim for %s", leaseID)
	}
}
