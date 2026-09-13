package cloudrunsandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderAliasesAndDiagnosticSecrets(t *testing.T) {
	p := Provider{}
	aliases := p.Aliases()
	if len(aliases) == 0 {
		t.Fatal("expected aliases")
	}
	for _, want := range []string{"gcrun-sandbox", "google-cloud-run-sandbox", "cloudrun-sandbox"} {
		found := false
		for _, got := range aliases {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing alias %q in %v", want, aliases)
		}
	}
	if p.ServerTypeForConfig(core.Config{}) != "" || p.ServerTypeForClass("any") != "" {
		t.Fatal("expected empty server type surface")
	}
	t.Setenv("CLOUD_RUN_SANDBOX_SECRET", "s1")
	t.Setenv("CRABBOX_CLOUD_RUN_SANDBOX_SECRET", "s2")
	t.Setenv("CLOUD_RUN_AUTH_TOKEN", "t1")
	t.Setenv("CRABBOX_CLOUD_RUN_SANDBOX_AUTH_TOKEN", "t2")
	secrets := p.DiagnosticSecrets(core.Config{})
	joined := strings.Join(secrets, ",")
	for _, want := range []string{"s1", "s2", "t1", "t2"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("secrets missing %q: %v", want, secrets)
		}
	}
}

func TestProviderConfigureAndFlags(t *testing.T) {
	p := Provider{}
	cfg := core.Config{CloudRunSandbox: core.CloudRunSandboxConfig{
		CLIPath: "/usr/local/gcp/bin/sandbox",
		Workdir: "/tmp/crabbox",
		Write:   true,
		Rootfs:  "/",
	}}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := p.RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--cloud-run-sandbox-gateway-url", "https://gw.example.run.app",
		"--cloud-run-sandbox-cli", "/opt/sandbox",
		"--cloud-run-sandbox-workdir", "/tmp/work",
		"--cloud-run-sandbox-allow-egress",
		"--cloud-run-sandbox-write=false",
		"--cloud-run-sandbox-rootfs", "/rootfs",
	}); err != nil {
		t.Fatal(err)
	}
	cfg.Provider = providerName
	if err := p.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatalf("ApplyFlags: %v", err)
	}
	if cfg.CloudRunSandbox.GatewayURL != "https://gw.example.run.app" ||
		cfg.CloudRunSandbox.CLIPath != "/opt/sandbox" ||
		cfg.CloudRunSandbox.Workdir != "/tmp/work" ||
		!cfg.CloudRunSandbox.AllowEgress ||
		cfg.CloudRunSandbox.Write ||
		cfg.CloudRunSandbox.Rootfs != "/rootfs" {
		t.Fatalf("flags not applied: %#v", cfg.CloudRunSandbox)
	}

	rt := core.Runtime{Stdout: io.Discard, Stderr: io.Discard}
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if backend.Spec().Name != providerName {
		t.Fatalf("spec name=%q", backend.Spec().Name)
	}
	doctor, err := p.ConfigureDoctor(cfg, rt)
	if err != nil {
		t.Fatalf("ConfigureDoctor: %v", err)
	}
	if doctor == nil {
		t.Fatal("nil doctor backend")
	}

	// class/type are rejected for this provider.
	fs2 := flag.NewFlagSet("test2", flag.ContinueOnError)
	_ = fs2.String("class", "", "")
	values2 := p.RegisterFlags(fs2, cfg)
	if err := fs2.Parse([]string{"--class", "xl"}); err != nil {
		t.Fatal(err)
	}
	cfg.Provider = providerName
	if err := p.ApplyFlags(&cfg, fs2, values2); err == nil || !strings.Contains(err.Error(), "--class") {
		t.Fatalf("expected class rejection, got %v", err)
	}

	fs3 := flag.NewFlagSet("test3", flag.ContinueOnError)
	_ = fs3.String("type", "", "")
	values3 := p.RegisterFlags(fs3, cfg)
	if err := fs3.Parse([]string{"--type", "n2"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyFlags(&cfg, fs3, values3); err == nil || !strings.Contains(err.Error(), "--type") {
		t.Fatalf("expected type rejection, got %v", err)
	}
}

func TestCloudRunConfigFlagPresenceAndGuardOrder(t *testing.T) {
	for _, name := range []string{"cloud-run-sandbox", "gcrun-sandbox", "google-cloud-run-sandbox", "cloudrun-sandbox", " Cloud-Run-Sandbox "} {
		cfg := core.BaseConfig()
		cfg.Provider = name
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.String("class", "", "")
		fs.String("type", "", "")
		values := RegisterCloudRunSandboxProviderFlags(fs, cfg)
		cfg.CloudRunSandbox = core.CloudRunSandboxConfig{GatewayURL: "https://example.invalid/env", CLIPath: "/opt/env", Workdir: "/tmp/env", Rootfs: "/env", Write: true, AllowEgress: true}
		before := cfg.CloudRunSandbox
		if err := ApplyCloudRunSandboxProviderFlags(&cfg, fs, values); err != nil {
			t.Fatal(err)
		}
		if cfg.CloudRunSandbox != before {
			t.Fatal("unvisited values changed config")
		}
		if err := fs.Parse([]string{"--cloud-run-sandbox-gateway-url=", "--cloud-run-sandbox-cli=", "--cloud-run-sandbox-workdir=", "--cloud-run-sandbox-rootfs=", "--cloud-run-sandbox-write=false", "--cloud-run-sandbox-allow-egress=false"}); err != nil {
			t.Fatal(err)
		}
		if err := ApplyCloudRunSandboxProviderFlags(&cfg, fs, values); err == nil || err.Error() != "cloudRunSandbox.cliPath must not be empty" {
			t.Fatalf("empty flags=%v", err)
		}
		if cfg.CloudRunSandbox != (core.CloudRunSandboxConfig{}) {
			t.Fatalf("all flags not copied before validation: %#v", cfg.CloudRunSandbox)
		}
		cfg.CloudRunSandbox.Workdir = "relative"
		if err := ApplyCloudRunSandboxProviderFlags(&cfg, fs, struct{}{}); err != nil {
			t.Fatalf("foreign type reached validation: %v", err)
		}
		if err := validateConfig(cfg); err == nil || err.Error() != "cloudRunSandbox.workdir must be an absolute path" {
			t.Fatalf("workdir must precede CLI validation: %v", err)
		}
		for _, args := range [][]string{{"--type=vm"}, {"--type=vm", "--class=large"}} {
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			want := "--type"
			if len(args) == 2 {
				want = "--class"
			}
			for _, v := range []any{nil, struct{}{}, values} {
				before := cfg.CloudRunSandbox
				err := ApplyCloudRunSandboxProviderFlags(&cfg, fs, v)
				if err == nil || err.Error() != want+" is not supported for provider=cloud-run-sandbox; sandboxes share Cloud Run service CPU/memory" {
					t.Fatalf("alias=%s guard=%v", name, err)
				}
				if cfg.CloudRunSandbox != before {
					t.Fatal("guard copied flags")
				}
			}
		}
	}
}

func TestWarmupRejectsUnsupportedOptions(t *testing.T) {
	b := NewBackend(Provider{}.Spec(), core.Config{CloudRunSandbox: core.CloudRunSandboxConfig{
		CLIPath: "/usr/local/gcp/bin/sandbox",
		Workdir: "/tmp/crabbox",
	}}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	if err := b.Warmup(context.Background(), core.WarmupRequest{ActionsRunner: true}); err == nil {
		t.Fatal("expected actions-runner rejection")
	}
	if err := b.Warmup(context.Background(), core.WarmupRequest{Options: core.LeaseOptions{Desktop: true}}); err == nil {
		t.Fatal("expected desktop rejection")
	}
	if err := b.Warmup(context.Background(), core.WarmupRequest{Options: core.LeaseOptions{Tailscale: core.TailscaleConfig{Enabled: true}}}); err == nil {
		t.Fatal("expected tailscale rejection")
	}
}

func TestRunRejectsUnsupportedOptionsAndMissingCommand(t *testing.T) {
	isolateLeaseHome(t)
	prev := newTransport
	newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) {
		return &fakeTransport{mode: "remote"}, nil
	}
	t.Cleanup(func() { newTransport = prev })

	b := NewBackend(Provider{}.Spec(), core.Config{CloudRunSandbox: core.CloudRunSandboxConfig{
		CLIPath: "/usr/local/gcp/bin/sandbox",
		Workdir: "/tmp/crabbox",
	}, IdleTimeout: time.Minute}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	if _, err := b.Run(context.Background(), core.RunRequest{Options: core.LeaseOptions{Browser: true}}); err == nil {
		t.Fatal("expected browser rejection")
	}
	if _, err := b.Run(context.Background(), core.RunRequest{Options: core.LeaseOptions{Tailscale: core.TailscaleConfig{Enabled: true}}}); err == nil {
		t.Fatal("expected tailscale rejection")
	}
	if _, err := b.Run(context.Background(), core.RunRequest{
		Repo:    core.Repo{Root: t.TempDir()},
		NoSync:  true,
		Command: nil,
	}); err == nil {
		t.Fatal("expected missing command rejection")
	}
}

func TestDoctorListStopStatusCleanup(t *testing.T) {
	isolateLeaseHome(t)
	var destroyed []string
	fake := &fakeTransport{
		mode: "remote",
		onDestroy: func(id string) error {
			destroyed = append(destroyed, id)
			return nil
		},
	}
	prev := newTransport
	newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) { return fake, nil }
	t.Cleanup(func() { newTransport = prev })

	var stdout, stderr bytes.Buffer
	cfg := core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{
			GatewayURL: "https://gw.example.run.app",
			CLIPath:    "/usr/local/gcp/bin/sandbox",
			Workdir:    "/tmp/crabbox",
		},
		IdleTimeout: time.Minute,
	}
	b := NewBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &stdout, Stderr: &stderr}).(*backend)

	scope, err := b.claimScope()
	if err != nil {
		t.Fatalf("claimScope: %v", err)
	}
	leaseID := leasePrefix + "demo-box-1"
	if err := claimTestCloudRunSandboxLease(leaseID, "slug-demo", scope, t.TempDir(), time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}

	doctor, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if doctor.Status != "ok" || doctor.Provider != providerName {
		t.Fatalf("doctor=%#v", doctor)
	}

	leases, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(leases) != 1 || leases[0].CloudID != "demo-box-1" {
		t.Fatalf("leases=%#v", leases)
	}

	status, err := b.Status(context.Background(), core.StatusRequest{ID: leaseID})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.ID != leaseID || status.ServerID != "demo-box-1" || !status.Ready {
		t.Fatalf("status=%#v", status)
	}

	// Dry-run cleanup should not destroy while idle timeout remains.
	if err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatalf("Cleanup dry-run: %v", err)
	}
	if len(destroyed) != 0 {
		t.Fatalf("dry-run destroyed=%v", destroyed)
	}

	if err := b.Stop(context.Background(), core.StopRequest{ID: leaseID}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(destroyed) != 1 || destroyed[0] != "demo-box-1" {
		t.Fatalf("destroyed=%v", destroyed)
	}
	if _, err := b.Status(context.Background(), core.StatusRequest{ID: leaseID}); err == nil {
		t.Fatal("expected status failure after stop")
	}
}

func TestDoctorFailsWhenLocalClaimsAreUnreadable(t *testing.T) {
	isolateLeaseHome(t)
	fake := &fakeTransport{mode: "remote"}
	previousTransport := newTransport
	newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) { return fake, nil }
	t.Cleanup(func() { newTransport = previousTransport })
	stateDir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "claims"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{GatewayURL: "https://gw.example.run.app", CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
	}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	result, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err == nil || result.Status != "error" || !strings.Contains(result.Message, "local_claims=blocked") {
		t.Fatalf("doctor result=%#v err=%v", result, err)
	}
}

func TestClaimCleanupDue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	if due, reason := claimCleanupDue(core.LeaseClaim{}, now); due || reason != "no-idle-timeout" {
		t.Fatalf("empty claim due=%v reason=%s", due, reason)
	}
	if due, reason := claimCleanupDue(core.LeaseClaim{IdleTimeoutSeconds: 60}, now); !due || reason != "missing-timestamps" {
		t.Fatalf("missing ts due=%v reason=%s", due, reason)
	}
	if due, reason := claimCleanupDue(core.LeaseClaim{
		IdleTimeoutSeconds: 60,
		LastUsedAt:         "not-a-time",
	}, now); !due || reason != "unparseable-timestamp" {
		t.Fatalf("bad ts due=%v reason=%s", due, reason)
	}
	if due, reason := claimCleanupDue(core.LeaseClaim{
		IdleTimeoutSeconds: 3600,
		LastUsedAt:         now.Add(-30 * time.Minute).Format(time.RFC3339),
	}, now); due || reason != "idle-timeout-remaining" {
		t.Fatalf("remaining due=%v reason=%s", due, reason)
	}
	if due, reason := claimCleanupDue(core.LeaseClaim{
		IdleTimeoutSeconds: 60,
		ClaimedAt:          now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
	}, now); !due || reason != "idle-timeout-expired" {
		t.Fatalf("expired due=%v reason=%s", due, reason)
	}
	creating := core.LeaseClaim{Labels: map[string]string{
		claimStateLabel:       "creating",
		claimActiveUntilLabel: now.Add(time.Minute).Format(time.RFC3339Nano),
		claimOwnershipLabel:   "sandbox-token",
	}}
	if due, reason := claimCleanupDue(creating, now); due || reason != "in-flight-creating" {
		t.Fatalf("active create due=%v reason=%s", due, reason)
	}
	if due, reason := claimCleanupDue(creating, now.Add(2*time.Minute)); !due || reason != "stale-creating" {
		t.Fatalf("stale create due=%v reason=%s", due, reason)
	}
}

func TestBuildCommandAndHelpers(t *testing.T) {
	t.Parallel()
	if _, err := buildCommand(nil, false); err == nil {
		t.Fatal("expected missing command")
	}
	got, err := buildCommand([]string{"echo", "hi"}, false)
	if err != nil || len(got) != 2 || got[0] != "echo" {
		t.Fatalf("argv mode: %v %v", got, err)
	}
	got, err = buildCommand([]string{"echo", "hi"}, true)
	if err != nil || len(got) != 1 || got[0] != "echo hi" {
		t.Fatalf("shell mode: %v %v", got, err)
	}
	directCleanup := cleanupCommand(core.Config{CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox"}}, "gcrs_x")
	if directCleanup != `crabbox stop --provider cloud-run-sandbox --id 'gcrs_x'` {
		t.Fatalf("cleanupCommand=%q", directCleanup)
	}
	remoteCleanup := cleanupCommand(core.Config{CloudRunSandbox: core.CloudRunSandboxConfig{GatewayURL: "https://gateway.example.run.app"}}, "gcrs_x")
	if !strings.Contains(remoteCleanup, "--cloud-run-sandbox-gateway-url 'https://gateway.example.run.app'") {
		t.Fatalf("remote cleanupCommand=%q", remoteCleanup)
	}
	customDirectCleanup := cleanupCommand(core.Config{CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/opt/sandbox"}}, "gcrs_x")
	if !strings.Contains(customDirectCleanup, "--cloud-run-sandbox-cli '/opt/sandbox'") {
		t.Fatalf("custom direct cleanupCommand=%q", customDirectCleanup)
	}
	name, err := newSandboxName(core.Repo{Root: "/tmp/My Repo!"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "crabbox-my-repo-") {
		t.Fatalf("sandbox name=%q", name)
	}
	if suffix := strings.TrimPrefix(name, "crabbox-my-repo-"); len(suffix) != sandboxNameSuffix*2 {
		t.Fatalf("ownership suffix=%q len=%d", suffix, len(suffix))
	}
	longName, err := newSandboxName(core.Repo{Root: "/tmp/" + strings.Repeat("a", 100)})
	if err != nil {
		t.Fatal(err)
	}
	if len(longName) > maxSandboxNameLen {
		t.Fatalf("sandbox name length=%d name=%q", len(longName), longName)
	}
	if sanitizeName("Hello_World!!") != "hello-world" {
		t.Fatalf("sanitize=%q", sanitizeName("Hello_World!!"))
	}
	check := doctorCheck("x", nil, map[string]string{"a": "b"})
	if check.Status != "ok" || check.Check != "x" {
		t.Fatalf("ok check=%#v", check)
	}
	check = doctorCheck("x", errors.New("boom"), nil)
	if check.Status != "error" || !strings.Contains(check.Message, "boom") {
		t.Fatalf("err check=%#v", check)
	}
}

func TestExecCommandAndUploadArchive(t *testing.T) {
	var execs []string
	fake := &fakeTransport{
		mode: "remote",
		onWrite: func(_, path string) error {
			return nil
		},
		onExec: func(_ string, command string) (int, string, string, error) {
			execs = append(execs, command)
			if strings.Contains(command, "base64 -d") {
				return 0, "", "", nil
			}
			return 0, "out", "", nil
		},
	}
	transport := &writeCaptureTransport{fakeTransport: fake}

	b := NewBackend(Provider{}.Spec(), core.Config{CloudRunSandbox: core.CloudRunSandboxConfig{
		CLIPath: "/usr/local/gcp/bin/sandbox",
		Workdir: "/tmp/crabbox",
	}}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	if err := b.uploadArchive(context.Background(), transport, "box", "/tmp/a.tgz", strings.NewReader("payload")); err != nil {
		t.Fatalf("uploadArchive: %v", err)
	}
	if transport.wrotePath != "/tmp/a.tgz.b64" || transport.wroteContent == "" {
		t.Fatalf("write path=%q content empty=%v", transport.wrotePath, transport.wroteContent == "")
	}
	if transport.writeCount != 1 || len(transport.appendValues) != 1 || transport.appendValues[0] {
		t.Fatalf("small upload writes=%d append=%v", transport.writeCount, transport.appendValues)
	}
	if len(execs) == 0 || !strings.Contains(execs[0], "base64 -d") {
		t.Fatalf("expected decode exec, got %v", execs)
	}

	code, err := b.execCommand(context.Background(), fake, "box", "/tmp/work", []string{"echo", "hi"}, map[string]string{"A": "1"}, io.Discard, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("execCommand: code=%d err=%v", code, err)
	}
	code, err = b.execCommand(context.Background(), fake, "box", "/tmp/work", nil, nil, nil, nil)
	if code != 2 {
		t.Fatalf("missing command code=%d err=%v", code, err)
	}
	if err := b.execShell(context.Background(), &fakeTransport{
		onExec: func(string, string) (int, string, string, error) { return 3, "", "", nil },
	}, "box", "false"); err == nil {
		t.Fatal("expected non-zero exec shell error")
	}
	if err := b.ensureWorkspace(context.Background(), fake, "box", "/tmp/work"); err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
}

func TestUploadArchiveStreamsBoundedChunks(t *testing.T) {
	t.Parallel()
	input := strings.Repeat("a", archiveUploadChunkSize)
	transport := &writeCaptureTransport{fakeTransport: &fakeTransport{
		onExec: func(string, string) (int, string, string, error) { return 0, "", "", nil },
	}}
	b := NewBackend(Provider{}.Spec(), core.Config{CloudRunSandbox: core.CloudRunSandboxConfig{
		CLIPath: "/usr/local/gcp/bin/sandbox",
		Workdir: "/tmp/crabbox",
	}}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	if err := b.uploadArchive(context.Background(), transport, "box", "/tmp/large.tgz", strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if transport.writeCount < 2 || len(transport.appendValues) < 2 || transport.appendValues[0] || !transport.appendValues[1] {
		t.Fatalf("writes=%d append=%v", transport.writeCount, transport.appendValues)
	}
	decoded, err := base64.StdEncoding.DecodeString(transport.wroteContent)
	if err != nil || string(decoded) != input {
		t.Fatalf("decoded bytes=%d err=%v", len(decoded), err)
	}
}

type writeCaptureTransport struct {
	*fakeTransport
	wrotePath    string
	wroteContent string
	writeCount   int
	appendValues []bool
}

func (w *writeCaptureTransport) WriteFile(_ context.Context, sandboxID, path, content string, appendContent bool) error {
	w.wrotePath = path
	w.writeCount++
	w.appendValues = append(w.appendValues, appendContent)
	if appendContent {
		w.wroteContent += content
	} else {
		w.wroteContent = content
	}
	if w.onWrite != nil {
		return w.onWrite(sandboxID, path)
	}
	return nil
}

func TestRunSyncOnlyAndFailurePaths(t *testing.T) {
	isolateLeaseHome(t)
	fake := &fakeTransport{
		mode: "remote",
		onExec: func(_ string, command string) (int, string, string, error) {
			if strings.Contains(command, "mkdir") {
				return 0, "", "", nil
			}
			return 7, "", "boom", nil
		},
	}
	prev := newTransport
	newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) { return fake, nil }
	t.Cleanup(func() { newTransport = prev })

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox", Write: true},
		IdleTimeout:     time.Minute,
	}, core.Runtime{Stdout: &stdout, Stderr: &stderr}).(*backend)

	result, err := b.Run(context.Background(), core.RunRequest{
		Repo:     core.Repo{Root: root},
		NoSync:   true,
		SyncOnly: true,
		Keep:     true,
	})
	if err != nil {
		t.Fatalf("sync-only: %v\n%s", err, stderr.String())
	}
	if result.LeaseID == "" || !strings.Contains(stdout.String(), "synced") {
		t.Fatalf("result=%#v stdout=%q", result, stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	result, err = b.Run(context.Background(), core.RunRequest{
		Repo:    core.Repo{Root: root},
		Command: []string{"false"},
		NoSync:  true,
		Keep:    true,
	})
	if err == nil {
		t.Fatal("expected non-zero exit error")
	}
	if result.ExitCode != 7 {
		t.Fatalf("exit=%d err=%v", result.ExitCode, err)
	}
}

func isolateLeaseHome(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
}
