package applevz

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
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

func writeAppleVZInstanceClaim(t *testing.T, inst applevzhelper.Instance) {
	t.Helper()
	server := core.Server{CloudID: inst.Name, Provider: providerName, Name: inst.Name, Labels: map[string]string{
		"instance": inst.Name,
		"lease":    inst.LeaseID,
		"provider": providerName,
		"slug":     inst.Slug,
	}}
	target := core.SSHTarget{Host: inst.SSHHost, User: inst.SSHUser}
	if inst.SSHPort > 0 {
		target.Port = fmt.Sprint(inst.SSHPort)
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(inst.LeaseID, inst.Slug, providerName, instanceScope(inst.Name), "", t.TempDir(), 5*time.Minute, false, server, target); err != nil {
		t.Fatal(err)
	}
}

func testBackend(t *testing.T, runner *recordingRunner) *backend {
	t.Helper()
	oldGOOS, oldGOARCH := hostGOOS, hostGOARCH
	oldMacOSVersion := hostMacOSVersion
	hostGOOS, hostGOARCH = "darwin", "arm64"
	hostMacOSVersion = func() (string, error) { return "26.5", nil }
	t.Cleanup(func() {
		hostGOOS, hostGOARCH = oldGOOS, oldGOARCH
		hostMacOSVersion = oldMacOSVersion
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
	b.prepareExistingHelper = b.prepareHelper
	b.stateRoot = func() (string, error) { return root, nil }
	b.waitForSSH = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	return b
}

func TestRequireHostRequiresMacOS13(t *testing.T) {
	oldGOOS, oldGOARCH := hostGOOS, hostGOARCH
	oldMacOSVersion := hostMacOSVersion
	hostGOOS, hostGOARCH = "darwin", "arm64"
	t.Cleanup(func() {
		hostGOOS, hostGOARCH = oldGOOS, oldGOARCH
		hostMacOSVersion = oldMacOSVersion
	})

	for _, tc := range []struct {
		version string
		wantErr string
	}{
		{version: "12.7.6", wantErr: "requires macOS 13 or newer"},
		{version: "13.0"},
		{version: "26.5"},
		{version: "invalid", wantErr: "could not parse macOS version"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			hostMacOSVersion = func() (string, error) { return tc.version, nil }
			err := requireHost()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("requireHost(%q): %v", tc.version, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("requireHost(%q) error=%v want %q", tc.version, err, tc.wantErr)
			}
		})
	}
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

	cfg = core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleVZ.CPUs = 0
	cfg.AppleVZ.MemoryMiB = 0
	cfg.AppleVZ.DiskGiB = 0
	core.MarkAppleVZCPUsExplicit(&cfg)
	core.MarkAppleVZMemoryExplicit(&cfg)
	core.MarkAppleVZDiskExplicit(&cfg)
	applyDefaults(&cfg)
	if cfg.AppleVZ.CPUs != 0 || cfg.AppleVZ.MemoryMiB != 0 || cfg.AppleVZ.DiskGiB != 0 {
		t.Fatalf("explicit zero numeric settings defaulted: %+v", cfg.AppleVZ)
	}
}

func TestValidateConfigRejectsUnsafeGuestIdentity(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	if err := (Provider{}).ValidateConfig(cfg); err != nil {
		t.Fatalf("default config validation error=%v", err)
	}

	cfg.AppleVZ.User = "yes\nroot"
	cfg.AppleVZ.WorkRoot = "/work/crabbox"
	if err := (Provider{}).ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "valid POSIX") {
		t.Fatalf("unsafe user validation error=%v", err)
	}

	cfg.AppleVZ.User = "runner"
	cfg.AppleVZ.WorkRoot = "relative/work"
	if err := (Provider{}).ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "safe absolute POSIX path") {
		t.Fatalf("relative work root validation error=%v", err)
	}

	cfg.AppleVZ.WorkRoot = `C:\work\crabbox`
	if err := (Provider{}).ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "safe absolute POSIX path") {
		t.Fatalf("host-native work root validation error=%v", err)
	}

	cfg.AppleVZ.WorkRoot = "/work/$(touch)"
	if err := (Provider{}).ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "safe absolute POSIX path") {
		t.Fatalf("shell-active work root validation error=%v", err)
	}

	cfg = core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleVZ.MemoryMiB = 512
	if err := (Provider{}).ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "at least 1024 MiB") {
		t.Fatalf("low memory validation error=%v", err)
	}

	cfg = core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleVZ.MemoryMiB = 0
	core.MarkAppleVZMemoryExplicit(&cfg)
	if err := (Provider{}).ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "got 0") {
		t.Fatalf("explicit zero memory validation error=%v", err)
	}

	cfg = core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleVZ.MemoryMiB = -1
	if err := (Provider{}).ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "got -1") {
		t.Fatalf("negative memory validation error=%v", err)
	}

	for _, test := range []struct {
		name     string
		setValue func(*core.Config, int)
		mark     func(*core.Config)
		want     string
	}{
		{name: "cpus", setValue: func(cfg *core.Config, value int) { cfg.AppleVZ.CPUs = value }, mark: core.MarkAppleVZCPUsExplicit, want: "appleVZ.cpus must be positive"},
		{name: "disk", setValue: func(cfg *core.Config, value int) { cfg.AppleVZ.DiskGiB = value }, mark: core.MarkAppleVZDiskExplicit, want: "appleVZ.diskGiB must be positive"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, value := range []int{0, -1} {
				cfg := core.BaseConfig()
				cfg.Provider = providerName
				test.setValue(&cfg, value)
				if value == 0 {
					test.mark(&cfg)
				}
				if err := (Provider{}).ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("value=%d validation error=%v", value, err)
				}
			}
		})
	}
}

func TestServerFromInstanceMapsRuntimeErrorToTerminalFailure(t *testing.T) {
	b := testBackend(t, &recordingRunner{})
	inst := applevzhelper.Instance{
		Name:   "runtime-error",
		Status: applevzhelper.StatusError,
	}
	claim := core.LeaseClaim{Labels: map[string]string{"state": "ready"}}

	server := b.serverFromInstance(inst, claim, b.configForRun())
	if server.Status != "failed" || server.Labels["state"] != "failed" {
		t.Fatalf("runtime error server=%+v", server)
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
	if !core.AppleVZCPUsExplicit(cfg) || !core.AppleVZMemoryExplicit(cfg) || !core.AppleVZDiskExplicit(cfg) {
		t.Fatal("apple-vz numeric settings should be explicit after flags")
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
			leaseID := argumentValue(req.Args, "--lease-id")
			name := argumentValue(req.Args, "--name")
			claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
			if err != nil {
				t.Fatal(err)
			}
			if !ok || claim.ProviderScope != name {
				t.Fatalf("claim before helper start: ok=%v claim=%+v", ok, claim)
			}
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
	name := ""
	startInstance := applevzhelper.Instance{
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
	runner.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if req.Name != "helper" || len(req.Args) == 0 || req.Args[0] != "start" {
			return core.LocalCommandResult{}, nil, false
		}
		name = argumentValue(req.Args, "--name")
		instance := startInstance
		instance.Name = name
		instance.LeaseID = argumentValue(req.Args, "--lease-id")
		instance.Slug = argumentValue(req.Args, "--slug")
		return core.LocalCommandResult{Stdout: mustJSON(t, applevzhelper.StartResponse{Instance: instance})}, nil, true
	}

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

func TestResolveStatusOnlyAllowsStartingInstanceWithoutSSHEndpoint(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(t, runner)
	root, _ := b.stateRoot()
	inst := applevzhelper.Instance{
		Name:      "crabbox-cbx123-starting",
		LeaseID:   "cbx_starting123",
		Slug:      "starting",
		Status:    applevzhelper.StatusStarting,
		Image:     b.configForRun().AppleVZ.Image,
		SSHUser:   b.configForRun().AppleVZ.User,
		WorkRoot:  b.configForRun().AppleVZ.WorkRoot,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	runner.responses[commandKey("helper", []string{"list", "--state-root", root})] = core.LocalCommandResult{
		Stdout: mustJSON(t, applevzhelper.ListResponse{Instances: []applevzhelper.Instance{inst}}),
	}
	writeAppleVZInstanceClaim(t, inst)

	for _, readyProbe := range []bool{false, true} {
		lease, err := b.Resolve(context.Background(), core.ResolveRequest{
			ID:         inst.LeaseID,
			StatusOnly: true,
			ReadyProbe: readyProbe,
		})
		if err != nil {
			t.Fatalf("Resolve readyProbe=%v: %v", readyProbe, err)
		}
		if lease.Server.Status != applevzhelper.StatusStarting || lease.Server.PublicNet.IPv4.IP != "" {
			t.Fatalf("Resolve readyProbe=%v server=%+v", readyProbe, lease.Server)
		}
		if lease.SSH.Host != "" || lease.SSH.Port != "" {
			t.Fatalf("Resolve readyProbe=%v exposed premature SSH target=%+v", readyProbe, lease.SSH)
		}
	}
}

func TestReleaseLeasePreservesClaimAndKeyWhenInstanceLookupFails(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
		errors:    map[string]error{},
	}
	b := testBackend(t, runner)
	root, _ := b.stateRoot()
	lookupErr := errors.New("helper inventory unavailable")
	runner.errors[commandKey("helper", []string{"list", "--state-root", root})] = lookupErr

	leaseID := "cbx_release123456"
	slug := "release-lookup"
	name := core.LeaseProviderName(leaseID, slug)
	if err := core.ClaimLeaseForRepoProviderScopePond(
		leaseID,
		slug,
		providerName,
		name,
		"",
		t.TempDir(),
		5*time.Minute,
		false,
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { core.RemoveLeaseClaim(leaseID) })
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("test private key"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { core.RemoveStoredTestboxKey(leaseID) })

	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{LeaseID: leaseID},
	})
	if err == nil || !strings.Contains(err.Error(), lookupErr.Error()) {
		t.Fatalf("ReleaseLease error=%v, want message containing %q", err, lookupErr)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatalf("lease claim %s was removed after lookup failure", leaseID)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key was removed after lookup failure: %v", err)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "delete" {
			t.Fatalf("delete called after lookup failure: %+v", call)
		}
	}
}

func TestReleaseLeaseRejectsUnclaimedRawInstance(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"helper\x00delete": {Stdout: mustJSON(t, applevzhelper.DeleteResponse{Deleted: true})},
	}}
	b := testBackend(t, runner)
	leaseID := "cbx_unclaimed_apple_vz"
	name := core.LeaseProviderName(leaseID, "unclaimed")
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: leaseID,
		Server:  core.Server{CloudID: name},
	}})
	if err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("ReleaseLease error=%v, want exact-claim rejection", err)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "delete" {
			t.Fatalf("unclaimed instance reached helper delete: %+v", call)
		}
	}
}

func TestRequireExactAppleVZClaimRequiresInstanceBinding(t *testing.T) {
	for _, tc := range []struct {
		name          string
		providerScope string
		labelInstance string
		wantAllowed   bool
	}{
		{name: "unbound"},
		{name: "mismatched scope", providerScope: "other-instance"},
		{name: "matching scope", providerScope: "owned-instance", wantAllowed: true},
		{name: "matching legacy label", labelInstance: "owned-instance", wantAllowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
			_ = testBackend(t, runner)
			leaseID := "cbx_bound_apple_vz"
			if err := core.ClaimLeaseForRepoProviderScopePond(
				leaseID,
				"bound-apple-vz",
				providerName,
				tc.providerScope,
				"",
				t.TempDir(),
				5*time.Minute,
				false,
			); err != nil {
				t.Fatal(err)
			}
			if tc.labelInstance != "" {
				if err := core.UpdateLeaseClaimEndpoint(leaseID, core.Server{Provider: providerName, CloudID: tc.labelInstance, Labels: map[string]string{
					"instance": tc.labelInstance,
					"provider": providerName,
					"slug":     "bound-apple-vz",
				}}, core.SSHTarget{}); err != nil {
					t.Fatal(err)
				}
			}
			err := requireExactAppleVZClaim(leaseID, "owned-instance")
			if tc.wantAllowed && err != nil {
				t.Fatalf("bound claim rejected: %v", err)
			}
			if !tc.wantAllowed && (err == nil || !strings.Contains(err.Error(), "bound to instance")) {
				t.Fatalf("unbound claim error=%v", err)
			}
		})
	}
}

func TestResolveRawAppleVZRequiresExplicitReclaimAndPersistsBinding(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(t, runner)
	root, _ := b.stateRoot()
	inst := applevzhelper.Instance{
		Name:      "crabbox-cbx123-raw",
		LeaseID:   "cbx_raw_apple_vz",
		Slug:      "raw-apple-vz",
		Status:    applevzhelper.StatusRunning,
		Image:     b.configForRun().AppleVZ.Image,
		SSHUser:   b.configForRun().AppleVZ.User,
		WorkRoot:  b.configForRun().AppleVZ.WorkRoot,
		SSHHost:   "127.0.0.1",
		SSHPort:   43023,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	runner.responses[commandKey("helper", []string{"list", "--state-root", root})] = core.LocalCommandResult{
		Stdout: mustJSON(t, applevzhelper.ListResponse{Instances: []applevzhelper.Instance{inst}}),
	}
	runner.responses["helper\x00delete"] = core.LocalCommandResult{Stdout: mustJSON(t, applevzhelper.DeleteResponse{Deleted: true, Instance: inst})}
	repo := core.Repo{Root: t.TempDir()}
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: inst.Name, Repo: repo}); err == nil || !strings.Contains(err.Error(), "explicit --reclaim") {
		t.Fatalf("Resolve without reclaim error=%v", err)
	}
	if claim, err := core.ReadLeaseClaim(inst.LeaseID); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("non-reclaim resolve minted claim: %#v", claim)
	}
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: inst.Name, Repo: repo, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(inst.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.ProviderScope != inst.Name || claim.CloudID != inst.Name {
		t.Fatalf("reclaim binding=%#v", claim)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(runner.calls, func(call core.LocalCommandRequest) bool {
		return len(call.Args) > 0 && call.Args[0] == "delete"
	}) {
		t.Fatalf("reclaimed instance was not released: %+v", runner.calls)
	}
}

func TestResolveReclaimDoesNotRetargetBoundAppleVZClaim(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(t, runner)
	root, _ := b.stateRoot()
	instA := applevzhelper.Instance{Name: "instance-a", LeaseID: "cbx_bound_apple_vz", Slug: "bound-apple-vz", Status: applevzhelper.StatusRunning, SSHHost: "127.0.0.1", SSHPort: 43024, SSHUser: "runner"}
	instB := instA
	instB.Name = "instance-b"
	instB.SSHPort = 43025
	writeAppleVZInstanceClaim(t, instA)
	runner.responses[commandKey("helper", []string{"list", "--state-root", root})] = core.LocalCommandResult{
		Stdout: mustJSON(t, applevzhelper.ListResponse{Instances: []applevzhelper.Instance{instB, instA}}),
	}
	repo := core.Repo{Root: t.TempDir()}
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: instA.LeaseID, Repo: repo, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != instA.Name {
		t.Fatalf("exact claim resolved instance %q, want %q", lease.Server.CloudID, instA.Name)
	}
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: instB.Name, Repo: repo, Reclaim: true}); err == nil || !strings.Contains(err.Error(), "bound to instance") {
		t.Fatalf("raw conflicting reclaim error=%v", err)
	}
	claim, err := core.ReadLeaseClaim(instA.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.ProviderScope != instA.Name || claim.CloudID != instA.Name {
		t.Fatalf("bound claim was retargeted: %#v", claim)
	}
}

func TestResolveStatusOnlyAllowsClaimlessAppleVZWithoutClaiming(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(t, runner)
	root, _ := b.stateRoot()
	inst := applevzhelper.Instance{Name: "status-instance", LeaseID: "cbx_status_apple_vz", Slug: "status-apple-vz", Status: applevzhelper.StatusStarting}
	runner.responses[commandKey("helper", []string{"list", "--state-root", root})] = core.LocalCommandResult{
		Stdout: mustJSON(t, applevzhelper.ListResponse{Instances: []applevzhelper.Instance{inst}}),
	}
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: inst.Name, Repo: core.Repo{Root: t.TempDir()}, StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != inst.Name {
		t.Fatalf("status lease=%#v", lease)
	}
	if claim, err := core.ReadLeaseClaim(inst.LeaseID); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("status-only resolve minted claim: %#v", claim)
	}
	writeAppleVZInstanceClaim(t, inst)
	before, err := core.ReadLeaseClaim(inst.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: inst.Name, Repo: core.Repo{Root: t.TempDir()}, StatusOnly: true}); err != nil {
		t.Fatalf("owned status-only resolve: %v", err)
	}
	after, err := core.ReadLeaseClaim(inst.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if after.RepoRoot != before.RepoRoot || after.LastUsedAt != before.LastUsedAt || after.CloudID != before.CloudID || after.ProviderScope != before.ProviderScope {
		t.Fatalf("status-only resolve mutated claim: before=%#v after=%#v", before, after)
	}
}

func TestResolveMetadataLessAppleVZDirectsCleanup(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(t, runner)
	root, _ := b.stateRoot()
	inst := applevzhelper.Instance{Name: "metadata-less", Status: applevzhelper.StatusStopped}
	runner.responses[commandKey("helper", []string{"list", "--state-root", root})] = core.LocalCommandResult{
		Stdout: mustJSON(t, applevzhelper.ListResponse{Instances: []applevzhelper.Instance{inst}}),
	}
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: inst.Name, ReleaseOnly: true}); err == nil || !strings.Contains(err.Error(), "crabbox cleanup --provider apple-vz") {
		t.Fatalf("metadata-less resolve error=%v", err)
	}
}

func TestReleaseLeaseRemovesClaimAndKeyWhenInstanceIsMissing(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
		errors:    map[string]error{},
	}
	b := testBackend(t, runner)
	root, _ := b.stateRoot()
	runner.responses[commandKey("helper", []string{"list", "--state-root", root})] = core.LocalCommandResult{
		Stdout: mustJSON(t, applevzhelper.ListResponse{}),
	}

	leaseID := "cbx_missing123456"
	slug := "missing-instance"
	name := core.LeaseProviderName(leaseID, slug)
	if err := core.ClaimLeaseForRepoProviderScopePond(
		leaseID,
		slug,
		providerName,
		name,
		"",
		t.TempDir(),
		5*time.Minute,
		false,
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { core.RemoveLeaseClaim(leaseID) })
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("test private key"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { core.RemoveStoredTestboxKey(leaseID) })

	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{LeaseID: leaseID},
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("lease claim %s remains after confirmed missing instance", leaseID)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stored key stat error=%v, want os.ErrNotExist", err)
	}
}

func TestResolveStatusReadyProbeIncludesPublishedSSHEndpoint(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(t, runner)
	root, _ := b.stateRoot()
	inst := applevzhelper.Instance{
		Name:      "crabbox-cbx123-running",
		LeaseID:   "cbx_running123",
		Slug:      "running",
		Status:    applevzhelper.StatusRunning,
		Image:     b.configForRun().AppleVZ.Image,
		SSHUser:   b.configForRun().AppleVZ.User,
		WorkRoot:  b.configForRun().AppleVZ.WorkRoot,
		SSHHost:   "127.0.0.1",
		SSHPort:   43022,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	runner.responses[commandKey("helper", []string{"list", "--state-root", root})] = core.LocalCommandResult{
		Stdout: mustJSON(t, applevzhelper.ListResponse{Instances: []applevzhelper.Instance{inst}}),
	}
	writeAppleVZInstanceClaim(t, inst)

	for _, readyProbe := range []bool{false, true} {
		lease, err := b.Resolve(context.Background(), core.ResolveRequest{
			ID:         inst.LeaseID,
			StatusOnly: true,
			ReadyProbe: readyProbe,
		})
		if err != nil {
			t.Fatalf("Resolve readyProbe=%v: %v", readyProbe, err)
		}
		if lease.SSH.Host != inst.SSHHost || lease.SSH.Port != "43022" {
			t.Fatalf("Resolve readyProbe=%v SSH target=%+v", readyProbe, lease.SSH)
		}
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

func TestInstanceDiagnosticsEscapesTerminalControls(t *testing.T) {
	stateRoot := t.TempDir()
	name := "diagnostic-controls"
	if err := os.MkdirAll(applevzhelper.InstanceDir(stateRoot, name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		applevzhelper.ConsoleLogPath(stateRoot, name),
		[]byte("guest failed\x1b]0;owned\x07\rmoved"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	err := instanceDiagnostics(stateRoot, name)
	if err == nil {
		t.Fatal("instanceDiagnostics returned nil")
	}
	if strings.ContainsAny(err.Error(), "\x1b\x07\r") || !strings.Contains(err.Error(), `\x1b]0;owned\x07\x0dmoved`) {
		t.Fatalf("instanceDiagnostics exposes terminal controls: %q", err)
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

	digestPath := managedHelperDigestPath(managedPath)
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
	changedPath, err := b.ensureHelperBinary(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := codesignCallCount(runner.calls); got != 4 {
		t.Fatalf("changed source codesign calls=%d want 4", got)
	}
	if changedPath == managedPath {
		t.Fatalf("changed source reused managed helper path %q", managedPath)
	}
	data, err := os.ReadFile(changedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "other" {
		t.Fatalf("managed helper=%q want changed source", string(data))
	}
	data, err = os.ReadFile(managedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "first" {
		t.Fatalf("previous managed helper changed to %q", string(data))
	}

	if err := os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ensureHelperBinary(context.Background(), cfg); err == nil {
		t.Fatal("strict helper preparation succeeded after source removal")
	}
	fallbackPath, err := b.ensureExistingHelperBinary(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if fallbackPath != changedPath {
		t.Fatalf("fallback helper path=%q want %q", fallbackPath, changedPath)
	}
	if got := codesignCallCount(runner.calls); got != 4 {
		t.Fatalf("fallback codesign calls=%d want 4", got)
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
	sourceDigest := dataSHA256([]byte("managed source"))
	entitlementsDigest := dataSHA256([]byte(applevzhelper.HelperEntitlements))
	want := managedHelperInstallPath(helperDir, sourceDigest, entitlementsDigest)
	if got != want {
		t.Fatalf("managed helper path=%q want %q", got, want)
	}
	if got := codesignCallCount(runner.calls); got != 1 {
		t.Fatalf("codesign calls=%d want 1", got)
	}
}

func TestManagedHelperPathsIncludeEntitlementsDigest(t *testing.T) {
	helperDir := t.TempDir()
	sourceDigest := strings.Repeat("a", 64)
	firstEntitlements := strings.Repeat("b", 64)
	secondEntitlements := strings.Repeat("c", 64)

	firstHelper := managedHelperInstallPath(helperDir, sourceDigest, firstEntitlements)
	secondHelper := managedHelperInstallPath(helperDir, sourceDigest, secondEntitlements)
	if firstHelper == secondHelper {
		t.Fatalf("helper paths collide across entitlement sets: %q", firstHelper)
	}
	firstFile := managedHelperEntitlementsPath(helperDir, firstEntitlements)
	secondFile := managedHelperEntitlementsPath(helperDir, secondEntitlements)
	if firstFile == secondFile || !strings.Contains(firstFile, firstEntitlements) || !strings.Contains(secondFile, secondEntitlements) {
		t.Fatalf("entitlements paths are not content-specific: %q %q", firstFile, secondFile)
	}
}

func TestCleanupObsoleteManagedHelpersKeepsCurrentAndNewest(t *testing.T) {
	helperDir := t.TempDir()
	now := time.Now()
	paths := make([]string, 6)
	for index := range paths {
		digest := strings.Repeat(string(rune('a'+index)), sha256.Size*2)
		paths[index] = filepath.Join(helperDir, applevzhelper.ManagedHelperName+"-"+digest)
		if err := os.WriteFile(paths[index], []byte(digest), 0o755); err != nil {
			t.Fatal(err)
		}
		digestPath := managedHelperDigestPath(paths[index])
		if err := os.WriteFile(digestPath, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		modTime := now.Add(-managedHelperRecentGrace - time.Duration(index+1)*time.Minute)
		if err := os.Chtimes(paths[index], modTime, modTime); err != nil {
			t.Fatal(err)
		}
	}
	unmanagedPath := filepath.Join(helperDir, applevzhelper.ManagedHelperName+"-custom")
	if err := os.WriteFile(unmanagedPath, []byte("leave me"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(unmanagedPath, now.Add(-2*managedHelperRecentGrace), now.Add(-2*managedHelperRecentGrace)); err != nil {
		t.Fatal(err)
	}
	bareDigestPath := filepath.Join(helperDir, strings.Repeat("f", sha256.Size*2))
	if err := os.WriteFile(bareDigestPath, []byte("leave me too"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bareDigestPath, now.Add(-2*managedHelperRecentGrace), now.Add(-2*managedHelperRecentGrace)); err != nil {
		t.Fatal(err)
	}

	currentPath := paths[len(paths)-1]
	if err := cleanupObsoleteManagedHelpers(helperDir, currentPath, now); err != nil {
		t.Fatal(err)
	}
	remaining := 0
	for _, path := range paths {
		_, err := os.Stat(path)
		if err == nil {
			remaining++
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if _, err := os.Stat(managedHelperDigestPath(path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("removed helper sidecar still exists for %s: %v", path, err)
		}
	}
	if remaining != managedHelperKeepVersions {
		t.Fatalf("remaining managed helpers=%d want %d", remaining, managedHelperKeepVersions)
	}
	if _, err := os.Stat(currentPath); err != nil {
		t.Fatalf("current managed helper was removed: %v", err)
	}
	if _, err := os.Stat(unmanagedPath); err != nil {
		t.Fatalf("unmanaged helper was removed: %v", err)
	}
	if _, err := os.Stat(bareDigestPath); err != nil {
		t.Fatalf("bare digest file was removed: %v", err)
	}
}

func TestCleanupObsoleteManagedHelpersPreservesActiveVersion(t *testing.T) {
	helperDir := t.TempDir()
	now := time.Now()
	paths := make([]string, 6)
	for index := range paths {
		digest := strings.Repeat(string(rune('a'+index)), sha256.Size*2)
		paths[index] = filepath.Join(helperDir, applevzhelper.ManagedHelperName+"-"+digest)
		if err := os.WriteFile(paths[index], []byte(digest), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(managedHelperDigestPath(paths[index]), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		modTime := now.Add(-managedHelperRecentGrace - time.Duration(index+1)*time.Minute)
		if err := os.Chtimes(paths[index], modTime, modTime); err != nil {
			t.Fatal(err)
		}
	}
	activePath := paths[4]
	useLock := flock.New(applevzhelper.ManagedHelperUseLockPath(activePath), flock.SetPermissions(0o600))
	if err := useLock.RLock(); err != nil {
		t.Fatal(err)
	}
	defer useLock.Unlock()

	if err := cleanupObsoleteManagedHelpers(helperDir, paths[5], now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(activePath); err != nil {
		t.Fatalf("active managed helper was removed: %v", err)
	}
	if _, err := os.Stat(managedHelperDigestPath(activePath)); err != nil {
		t.Fatalf("active managed helper sidecar was removed: %v", err)
	}
	if _, err := os.Stat(paths[3]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inactive obsolete managed helper was not removed: %v", err)
	}
}

func TestAppleVZHelperEnvAddsUseLockOnlyForManagedCopies(t *testing.T) {
	managedPath := filepath.Join(t.TempDir(), applevzhelper.ManagedHelperName+"-"+strings.Repeat("a", sha256.Size*2))
	lockEntry := applevzhelper.ManagedHelperUseLockEnv + "=" + applevzhelper.ManagedHelperUseLockPath(managedPath)
	if !slices.Contains(appleVZHelperEnv(managedPath), lockEntry) {
		t.Fatalf("managed helper environment missing %q", lockEntry)
	}
	if slices.ContainsFunc(appleVZHelperEnv("/tmp/custom-helper"), func(entry string) bool {
		return strings.HasPrefix(entry, applevzhelper.ManagedHelperUseLockEnv+"=")
	}) {
		t.Fatal("custom helper environment unexpectedly includes managed helper use lock")
	}
	if slices.ContainsFunc(appleVZHelperEnv(filepath.Join(t.TempDir(), strings.Repeat("a", sha256.Size*2))), func(entry string) bool {
		return strings.HasPrefix(entry, applevzhelper.ManagedHelperUseLockEnv+"=")
	}) {
		t.Fatal("bare digest helper environment unexpectedly includes managed helper use lock")
	}
}

func TestManagedHelperInstallLockExcludesSecondOwner(t *testing.T) {
	helperDir := t.TempDir()
	first, err := lockManagedHelperDir(helperDir)
	if err != nil {
		t.Fatal(err)
	}
	second := flock.New(filepath.Join(helperDir, ".install.lock"), flock.SetPermissions(0o600))
	locked, err := second.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	if locked {
		_ = second.Unlock()
		t.Fatal("second owner acquired managed helper install lock")
	}
	if err := unlockManagedHelperDir(first); err != nil {
		t.Fatal(err)
	}
	locked, err = second.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("second owner could not acquire released managed helper install lock")
	}
	if err := second.Unlock(); err != nil {
		t.Fatal(err)
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

func TestCleanupRemovesOldRunningInstanceWithoutPersistedClaim(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(t, runner)
	root, _ := b.stateRoot()
	leaseID := "cbx_orphan123456"
	name := core.LeaseProviderName(leaseID, "orphan-demo")
	instance := applevzhelper.Instance{
		Name:      name,
		LeaseID:   leaseID,
		Slug:      "orphan-demo",
		Status:    applevzhelper.StatusRunning,
		CreatedAt: time.Now().UTC().Add(-unclaimedInstanceGrace - time.Minute),
		UpdatedAt: time.Now().UTC().Add(-unclaimedInstanceGrace - time.Minute),
	}
	runner.responses[commandKey("helper", []string{"list", "--state-root", root})] = core.LocalCommandResult{
		Stdout: mustJSON(t, applevzhelper.ListResponse{Instances: []applevzhelper.Instance{instance}}),
	}
	runner.responses["helper\x00delete"] = core.LocalCommandResult{
		Stdout: mustJSON(t, applevzhelper.DeleteResponse{Deleted: true, Instance: instance}),
	}

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	deleteCalls := 0
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "delete" {
			deleteCalls++
		}
	}
	if deleteCalls != 1 {
		t.Fatalf("delete calls=%d want 1", deleteCalls)
	}
}

func TestCleanupPreservesFreshStartupClaimWithoutVisibleInstance(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(t, runner)
	root, _ := b.stateRoot()
	runner.responses[commandKey("helper", []string{"list", "--state-root", root})] = core.LocalCommandResult{
		Stdout: mustJSON(t, applevzhelper.ListResponse{}),
	}
	leaseID := "cbx_startup123456"
	if err := core.ClaimLeaseForRepoProviderScopePond(
		leaseID,
		"startup-demo",
		providerName,
		"crabbox-startup-demo",
		"",
		t.TempDir(),
		5*time.Minute,
		false,
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { core.RemoveLeaseClaim(leaseID) })

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatalf("fresh startup claim %s was removed", leaseID)
	}
}

func TestShouldCleanupUsesLatestLifecycleTimestampForUnclaimedInstance(t *testing.T) {
	now := time.Now().UTC()
	inst := applevzhelper.Instance{
		Status:    applevzhelper.StatusRunning,
		CreatedAt: now.Add(-unclaimedInstanceGrace - time.Hour),
		UpdatedAt: now.Add(-unclaimedInstanceGrace + time.Minute),
	}
	server := core.Server{Status: "running", Labels: map[string]string{"keep": "false"}}
	if cleanup, reason := shouldCleanup(inst, server, core.LeaseClaim{}, false, now); cleanup || reason != "missing claim within grace period" {
		t.Fatalf("cleanup=%v reason=%q", cleanup, reason)
	}
}

func TestClaimWithinStartupGrace(t *testing.T) {
	now := time.Now().UTC()
	if !claimWithinStartupGrace(core.LeaseClaim{ClaimedAt: now.Add(-time.Minute).Format(time.RFC3339)}, now) {
		t.Fatal("fresh claim should remain in startup grace")
	}
	if claimWithinStartupGrace(core.LeaseClaim{ClaimedAt: now.Add(-unclaimedInstanceGrace - time.Minute).Format(time.RFC3339)}, now) {
		t.Fatal("old claim should be outside startup grace")
	}
}

func TestShouldCleanupPreservesFreshRunningInstanceWithoutClaim(t *testing.T) {
	now := time.Now().UTC()
	inst := applevzhelper.Instance{
		Status:    applevzhelper.StatusRunning,
		CreatedAt: now.Add(-unclaimedInstanceGrace + time.Minute),
	}
	server := core.Server{Status: "running", Labels: map[string]string{"keep": "false"}}
	if cleanup, reason := shouldCleanup(inst, server, core.LeaseClaim{}, false, now); cleanup || reason != "missing claim within grace period" {
		t.Fatalf("cleanup=%v reason=%q", cleanup, reason)
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
