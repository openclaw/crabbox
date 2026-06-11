package applevz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	hook      func(core.LocalCommandRequest) (core.LocalCommandResult, error, bool)
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if r.hook != nil {
		if result, err, handled := r.hook(req); handled {
			return result, err
		}
	}
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
		HelperPath:  "/tmp/helper-source",
		Image:       "https://cloud-images.ubuntu.com/releases/noble/release-20260518/ubuntu-24.04-server-cloudimg-arm64.img",
		ImageSHA256: "6a61b967ba4a27dd1966f835a67643073ed55c2860ce3dc1cb0517282e6b8bec",
		User:        "runner",
		WorkRoot:    "/workspace/crabbox",
		CPUs:        4,
		MemoryMiB:   8192,
		DiskGiB:     40,
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
		"--apple-vz-image", "/tmp/custom.img",
		"--apple-vz-image-sha256", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
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
	if cfg.AppleVZ.HelperPath != "/opt/bin/helper" || cfg.AppleVZ.Image != "/tmp/custom.img" || cfg.AppleVZ.ImageSHA256 != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || cfg.AppleVZ.User != "ci" || cfg.AppleVZ.WorkRoot != "/work/ci" || cfg.AppleVZ.CPUs != 6 || cfg.AppleVZ.MemoryMiB != 12288 || cfg.AppleVZ.DiskGiB != 64 {
		t.Fatalf("flags not applied: %#v", cfg.AppleVZ)
	}
	if !core.AppleVZImageExplicit(cfg) {
		t.Fatal("apple-vz image should be explicit after --apple-vz-image")
	}
}

func TestApplyFlagsRejectsRemoteImages(t *testing.T) {
	for _, image := range []string{
		"https://example.test/custom.img",
		"https://alice:secret@example.test/custom.img",
		"https://example.test/custom.img?token=private",
		"https://example.test/custom.img#fragment",
		"https://example.test/bearer-secret/custom.img",
	} {
		cfg := core.BaseConfig()
		cfg.Provider = providerName
		fs := flag.NewFlagSet("apple-vz", flag.ContinueOnError)
		values := registerFlags(fs, cfg)
		if err := fs.Parse([]string{"--apple-vz-image", image}); err != nil {
			t.Fatal(err)
		}
		err := applyFlags(&cfg, fs, values)
		if err == nil {
			t.Fatalf("applyFlags accepted remote URL %q", image)
		}
		if strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
			t.Fatalf("error exposes remote URL %q: %v", image, err)
		}
		if !strings.Contains(err.Error(), "CRABBOX_APPLE_VZ_IMAGE") {
			t.Fatalf("error=%v, want secret-safe input guidance", err)
		}
	}
}

func TestRegisterFlagsRedactsSignedImageDefault(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AppleVZ.Image = "https://downloads.example.test/bearer-secret/ubuntu.img?token=private"
	cfg.AppleVZ.ImageSHA256 = strings.Repeat("a", 64)
	fs := flag.NewFlagSet("apple-vz", flag.ContinueOnError)
	var output bytes.Buffer
	fs.SetOutput(&output)
	registerFlags(fs, cfg)
	fs.PrintDefaults()

	got := output.String()
	for _, secret := range []string{"downloads.example.test", "bearer-secret", "token=private"} {
		if strings.Contains(got, secret) {
			t.Fatalf("help output exposes signed image component %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "remote:sha256:aaaaaaaaaaaa") {
		t.Fatalf("help output missing safe image identity: %s", got)
	}
}

func TestDoctorReady(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey("helper", []string{"doctor", "--state-root", "", "--image-request-stdin"}): {Stdout: mustJSON(t, applevzhelper.DoctorResponse{
			Status:    "ok",
			Message:   "runtime ready",
			Instances: 2,
			Details:   map[string]string{"runtime": "virtualization.framework"},
		})},
	}}
	b := testBackend(t, runner)
	root, _ := b.stateRoot()
	runner.responses[commandKey("helper", []string{"doctor", "--state-root", root, "--image-request-stdin"})] = runner.responses[commandKey("helper", []string{"doctor", "--state-root", "", "--image-request-stdin"})]
	delete(runner.responses, commandKey("helper", []string{"doctor", "--state-root", "", "--image-request-stdin"}))
	result, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Message, "leases=2") || !strings.Contains(result.Message, "virtualization.framework") {
		t.Fatalf("unexpected doctor message: %s", result.Message)
	}
}

func TestDoctorPassesSignedImageViaStdinAndRedactsDisplay(t *testing.T) {
	runner := &recordingRunner{}
	b := testBackend(t, runner)
	signedImage := "https://downloads.example.test/bearer-secret/ubuntu.img"
	t.Setenv("CRABBOX_APPLE_VZ_IMAGE", signedImage)
	t.Setenv("GITHUB_TOKEN", "github-secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("HTTPS_PROXY", "http://proxy.example.test:8080")
	b.cfg.AppleVZ.Image = signedImage
	b.cfg.AppleVZ.ImageSHA256 = strings.Repeat("a", 64)
	applyDefaults(&b.cfg)

	runner.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if req.Name != "helper" || len(req.Args) == 0 || req.Args[0] != "doctor" {
			return core.LocalCommandResult{}, nil, false
		}
		if args := strings.Join(req.Args, " "); strings.Contains(args, "secret") || strings.Contains(args, "private") {
			t.Fatalf("helper argv exposes signed image: %s", args)
		}
		for _, entry := range req.Env {
			if strings.HasPrefix(entry, "CRABBOX_APPLE_VZ_IMAGE=") {
				t.Fatalf("helper environment exposes signed image")
			}
			if strings.HasPrefix(entry, "GITHUB_TOKEN=") || strings.HasPrefix(entry, "AWS_SECRET_ACCESS_KEY=") {
				t.Fatalf("helper environment exposes caller credential: %s", strings.SplitN(entry, "=", 2)[0])
			}
		}
		if !slices.Contains(req.Env, "HTTPS_PROXY=http://proxy.example.test:8080") {
			t.Fatalf("helper environment missing configured HTTPS proxy: %q", req.Env)
		}
		if req.CancelGracePeriod != helperCancelGracePeriod {
			t.Fatalf("helper cancel grace period=%s want %s", req.CancelGracePeriod, helperCancelGracePeriod)
		}
		data, err := io.ReadAll(req.Stdin)
		if err != nil {
			t.Fatal(err)
		}
		var imageRequest applevzhelper.ImageRequest
		if err := json.Unmarshal(data, &imageRequest); err != nil {
			t.Fatal(err)
		}
		if imageRequest.Image != signedImage || imageRequest.SHA256 != strings.Repeat("a", 64) {
			t.Fatalf("image request=%+v", imageRequest)
		}
		return core.LocalCommandResult{Stdout: mustJSON(t, applevzhelper.DoctorResponse{
			Status:  "ok",
			Message: "runtime ready",
			Details: map[string]string{"image": signedImage},
		})}, nil, true
	}

	result, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Message, "secret") || strings.Contains(result.Message, "private") || strings.Contains(result.Message, "alice") {
		t.Fatalf("doctor message exposes signed image: %s", result.Message)
	}
	if !strings.Contains(result.Message, "remote:sha256:aaaaaaaaaaaa") {
		t.Fatalf("doctor message missing safe image identity: %s", result.Message)
	}
	for _, check := range result.Checks {
		for key, value := range check.Details {
			if strings.Contains(value, "secret") || strings.Contains(value, "private") || strings.Contains(value, "alice") {
				t.Fatalf("doctor detail %s exposes signed image: %s", key, value)
			}
		}
	}
	if got := (Provider{}).ServerTypeForConfig(b.cfg); got != "remote:sha256:aaaaaaaaaaaa" {
		t.Fatalf("ServerTypeForConfig=%q", got)
	}
}

func TestAcquireRedactsSignedImageFromLogsAndLeaseMetadata(t *testing.T) {
	runner := &recordingRunner{}
	b := testBackend(t, runner)
	signedImage := "https://downloads.example.test/bearer-secret/ubuntu.img"
	b.cfg.AppleVZ.Image = signedImage
	b.cfg.AppleVZ.ImageSHA256 = strings.Repeat("a", 64)
	applyDefaults(&b.cfg)
	var stderr bytes.Buffer
	b.rt.Stderr = &stderr

	runner.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if req.Name != "helper" || len(req.Args) == 0 {
			return core.LocalCommandResult{}, nil, false
		}
		switch req.Args[0] {
		case "list":
			return core.LocalCommandResult{Stdout: mustJSON(t, applevzhelper.ListResponse{})}, nil, true
		case "start":
			if args := strings.Join(req.Args, " "); strings.Contains(args, "secret") || strings.Contains(args, "private") {
				t.Fatalf("helper argv exposes signed image: %s", args)
			}
			data, err := io.ReadAll(req.Stdin)
			if err != nil {
				t.Fatal(err)
			}
			var imageRequest applevzhelper.ImageRequest
			if err := json.Unmarshal(data, &imageRequest); err != nil {
				t.Fatal(err)
			}
			if imageRequest.Image != signedImage {
				t.Fatalf("image request=%+v", imageRequest)
			}
			inst := applevzhelper.Instance{
				Name:      argumentValue(req.Args, "--name"),
				LeaseID:   argumentValue(req.Args, "--lease-id"),
				Slug:      argumentValue(req.Args, "--slug"),
				Status:    applevzhelper.StatusRunning,
				Image:     applevzhelper.ImageIdentity(signedImage, b.cfg.AppleVZ.ImageSHA256),
				SSHUser:   argumentValue(req.Args, "--ssh-user"),
				WorkRoot:  argumentValue(req.Args, "--work-root"),
				SSHHost:   "127.0.0.1",
				SSHPort:   43022,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}
			return core.LocalCommandResult{Stdout: mustJSON(t, applevzhelper.StartResponse{Instance: inst})}, nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}

	lease, err := b.Acquire(context.Background(), core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "signed-image",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		core.RemoveLeaseClaim(lease.LeaseID)
		core.RemoveStoredTestboxKey(lease.LeaseID)
	})
	for label, value := range map[string]string{
		"stderr":      stderr.String(),
		"image":       lease.Server.Labels["image"],
		"server_type": lease.Server.ServerType.Name,
	} {
		if strings.Contains(value, "secret") || strings.Contains(value, "private") || strings.Contains(value, "alice") {
			t.Fatalf("%s exposes signed image: %s", label, value)
		}
	}
	if got := lease.Server.ServerType.Name; got != "remote:sha256:aaaaaaaaaaaa" {
		t.Fatalf("server type=%q, want safe image identity", got)
	}
	if got := lease.Server.Labels["server_type"]; got != lease.Server.ServerType.Name {
		t.Fatalf("server_type label=%q, server type=%q", got, lease.Server.ServerType.Name)
	}
}

func TestTouchPreservesSafeServerTypeIdentity(t *testing.T) {
	b := testBackend(t, &recordingRunner{})
	identity := "remote:sha256:aaaaaaaaaaaa"
	server := core.Server{
		Labels: map[string]string{
			"image":       identity,
			"server_type": identity,
		},
	}
	server.ServerType.Name = identity
	lease := core.LeaseTarget{
		Server: server,
	}

	server, err := b.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if got := server.Labels["server_type"]; got != identity {
		t.Fatalf("server_type label=%q, want %q", got, identity)
	}
	if got := server.ServerType.Name; got != identity {
		t.Fatalf("server type=%q, want %q", got, identity)
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

func TestAcquireKeepRollsBackFailedProvisioning(t *testing.T) {
	runner := &recordingRunner{}
	b := testBackend(t, runner)
	root, _ := b.stateRoot()
	var leaseID, name string
	deleted := false
	runner.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if req.Name != "helper" || len(req.Args) == 0 {
			return core.LocalCommandResult{}, nil, false
		}
		switch req.Args[0] {
		case "list":
			return core.LocalCommandResult{Stdout: mustJSON(t, applevzhelper.ListResponse{})}, nil, true
		case "start":
			name = argumentValue(req.Args, "--name")
			leaseID = argumentValue(req.Args, "--lease-id")
			if err := os.MkdirAll(applevzhelper.InstanceDir(root, name), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(applevzhelper.HelperLogPath(root, name), []byte("helper failed after boot\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			inst := applevzhelper.Instance{
				Name:      name,
				LeaseID:   leaseID,
				Slug:      argumentValue(req.Args, "--slug"),
				Status:    applevzhelper.StatusRunning,
				SSHUser:   argumentValue(req.Args, "--ssh-user"),
				WorkRoot:  argumentValue(req.Args, "--work-root"),
				SSHHost:   "127.0.0.1",
				SSHPort:   43022,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}
			return core.LocalCommandResult{Stdout: mustJSON(t, applevzhelper.StartResponse{Instance: inst})}, nil, true
		case "delete":
			deleted = true
			if err := os.RemoveAll(applevzhelper.InstanceDir(root, name)); err != nil {
				t.Fatal(err)
			}
			return core.LocalCommandResult{Stdout: mustJSON(t, applevzhelper.DeleteResponse{Deleted: true})}, nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b.waitForSSH = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		return core.Exit(7, "injected SSH readiness failure")
	}

	_, err := b.Acquire(context.Background(), core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "keep-failure",
		Keep:          true,
	})
	if err == nil || !strings.Contains(err.Error(), "injected SSH readiness failure") || !strings.Contains(err.Error(), "helper failed after boot") {
		t.Fatalf("Acquire error=%v", err)
	}
	var exitErr core.ExitError
	if !core.AsExitError(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("Acquire error=%v, want exit code 7", err)
	}
	for _, want := range []string{"injected SSH readiness failure", "helper failed after boot"} {
		if !strings.Contains(exitErr.Message, want) {
			t.Fatalf("ExitError message=%q, want %q", exitErr.Message, want)
		}
	}
	if !deleted {
		t.Fatal("failed keep acquisition did not delete the instance")
	}
	if _, statErr := os.Stat(applevzhelper.InstanceDir(root, name)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("instance directory stat error=%v, want os.ErrNotExist", statErr)
	}
	if keyPath, keyErr := core.TestboxKeyPath(leaseID); keyErr != nil {
		t.Fatal(keyErr)
	} else if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("lease key stat error=%v, want os.ErrNotExist", statErr)
	}
}

func TestAcquirePreservesKeyWhenRollbackFails(t *testing.T) {
	runner := &recordingRunner{}
	b := testBackend(t, runner)
	root, _ := b.stateRoot()
	var leaseID, name string
	runner.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if req.Name != "helper" || len(req.Args) == 0 {
			return core.LocalCommandResult{}, nil, false
		}
		switch req.Args[0] {
		case "list":
			return core.LocalCommandResult{Stdout: mustJSON(t, applevzhelper.ListResponse{})}, nil, true
		case "start":
			name = argumentValue(req.Args, "--name")
			leaseID = argumentValue(req.Args, "--lease-id")
			if err := os.MkdirAll(applevzhelper.InstanceDir(root, name), 0o755); err != nil {
				t.Fatal(err)
			}
			inst := applevzhelper.Instance{
				Name:      name,
				LeaseID:   leaseID,
				Slug:      argumentValue(req.Args, "--slug"),
				Status:    applevzhelper.StatusRunning,
				SSHUser:   argumentValue(req.Args, "--ssh-user"),
				WorkRoot:  argumentValue(req.Args, "--work-root"),
				SSHHost:   "127.0.0.1",
				SSHPort:   43022,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}
			return core.LocalCommandResult{Stdout: mustJSON(t, applevzhelper.StartResponse{Instance: inst})}, nil, true
		case "delete":
			return core.LocalCommandResult{Stdout: mustJSON(t, applevzhelper.DeleteResponse{Deleted: false})}, nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b.waitForSSH = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		return errors.New("injected SSH readiness failure")
	}
	t.Cleanup(func() {
		core.RemoveStoredTestboxKey(leaseID)
		_ = os.RemoveAll(applevzhelper.InstanceDir(root, name))
	})

	_, err := b.Acquire(context.Background(), core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "rollback-failure",
	})
	if err == nil || !strings.Contains(err.Error(), "apple-vz cleanup failed") {
		t.Fatalf("Acquire error=%v", err)
	}
	keyPath, keyErr := core.TestboxKeyPath(leaseID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("rollback failure should preserve lease key: %v", statErr)
	}
}

func TestEnsureHelperBinarySignsOnlyWhenSourceChanges(t *testing.T) {
	runner := &recordingRunner{}
	b := testBackend(t, runner)
	sourcePath := filepath.Join(t.TempDir(), applevzhelper.ManagedHelperName)
	if err := os.WriteFile(sourcePath, []byte("first"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := b.configForRun()
	cfg.AppleVZ.HelperPath = sourcePath

	managedPath, err := b.ensureHelperBinary(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := codesignCallCount(runner.calls); got != 1 {
		t.Fatalf("codesign calls=%d want 1", got)
	}
	if _, err := b.ensureHelperBinary(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := codesignCallCount(runner.calls); got != 1 {
		t.Fatalf("unchanged source codesign calls=%d want 1", got)
	}

	root, err := b.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	digestPath := filepath.Join(applevzhelper.HelperDir(root), managedHelperDigestFileName)
	digestData, err := os.ReadFile(digestPath)
	if err != nil {
		t.Fatal(err)
	}
	var digests managedHelperDigests
	if err := json.Unmarshal(digestData, &digests); err != nil {
		t.Fatal(err)
	}
	digests.EntitlementsSHA256 = "stale"
	digestData, err = json.Marshal(digests)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(digestPath, digestData, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ensureHelperBinary(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := codesignCallCount(runner.calls); got != 2 {
		t.Fatalf("stale entitlements codesign calls=%d want 2", got)
	}

	if err := os.WriteFile(managedPath, []byte("wrong"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ensureHelperBinary(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := codesignCallCount(runner.calls); got != 3 {
		t.Fatalf("tampered managed helper codesign calls=%d want 3", got)
	}

	if err := os.WriteFile(sourcePath, []byte("other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ensureHelperBinary(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := codesignCallCount(runner.calls); got != 4 {
		t.Fatalf("changed source codesign calls=%d want 4", got)
	}
	data, err := os.ReadFile(managedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "other" {
		t.Fatalf("managed helper=%q want changed source", string(data))
	}
}

func TestResolveHelperSourcePathDoesNotTrustCheckoutBin(t *testing.T) {
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalDir) })
	checkout := t.TempDir()
	binDir := filepath.Join(checkout, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	checkoutHelper := filepath.Join(binDir, applevzhelper.ManagedHelperName)
	if err := os.WriteFile(checkoutHelper, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(checkout); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())

	cfg := core.BaseConfig()
	cfg.AppleVZ.HelperPath = ""
	path, err := resolveHelperSourcePath(cfg)
	if err == nil {
		t.Fatalf("resolveHelperSourcePath trusted checkout helper %q", path)
	}
	if strings.Contains(err.Error(), checkoutHelper) {
		t.Fatalf("error exposes or selects checkout helper: %v", err)
	}
}

func TestEnsureHelperBinarySignsManagedSourcePath(t *testing.T) {
	runner := &recordingRunner{}
	b := testBackend(t, runner)
	root, err := b.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	helperDir := applevzhelper.HelperDir(root)
	if err := os.MkdirAll(helperDir, 0o755); err != nil {
		t.Fatal(err)
	}
	managedPath := filepath.Join(helperDir, applevzhelper.ManagedHelperName)
	if err := os.WriteFile(managedPath, []byte("managed source"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := b.configForRun()
	cfg.AppleVZ.HelperPath = managedPath

	got, err := b.ensureHelperBinary(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != managedPath {
		t.Fatalf("managed helper path=%q want %q", got, managedPath)
	}
	if got := codesignCallCount(runner.calls); got != 1 {
		t.Fatalf("codesign calls=%d want 1", got)
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

func argumentValue(args []string, name string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			return args[index+1]
		}
	}
	return ""
}

func codesignCallCount(calls []core.LocalCommandRequest) int {
	count := 0
	for _, call := range calls {
		if call.Name == "codesign" {
			count++
		}
	}
	return count
}
