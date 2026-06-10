package wasi

import (
	"bytes"
	"context"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestWasiProviderRegistration(t *testing.T) {
	p := Provider{}
	if p.Name() != providerName {
		t.Fatalf("name=%s want=%s", p.Name(), providerName)
	}
	if len(p.Aliases()) == 0 {
		t.Fatal("expected aliases")
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("kind=%s want delegated-run", spec.Kind)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) {
		t.Fatal("expected FeatureArchiveSync")
	}
	if !spec.Features.Has(core.FeatureRunSession) || !spec.Features.Has(core.FeatureRunProof) {
		t.Fatal("expected run-session and run-proof features")
	}
}

func TestWasiFlagsAndApply(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	v := RegisterWasiProviderFlags(fs, core.Config{})
	if v == nil {
		t.Fatal("flags value nil")
	}
	cfg := &core.Config{}
	if err := ApplyWasiProviderFlags(cfg, fs, v); err != nil {
		t.Fatalf("apply: %v", err)
	}
	*cfg = core.Config{Provider: providerName}
	fs2 := flag.NewFlagSet("test2", flag.ContinueOnError)
	classF := fs2.String("class", "", "")
	_ = fs2.Parse([]string{"--class=foo"})
	if *classF == "" {
		t.Fatal("test setup")
	}
	if err := ApplyWasiProviderFlags(cfg, fs2, nil); err == nil {
		t.Fatal("expected --class reject for wasi")
	}
	fs3 := flag.NewFlagSet("test3", flag.ContinueOnError)
	typeF := fs3.String("type", "", "")
	_ = fs3.Parse([]string{"--type=foo"})
	if *typeF == "" {
		t.Fatal("test setup")
	}
	if err := ApplyWasiProviderFlags(cfg, fs3, nil); err == nil {
		t.Fatal("expected --type reject for wasi")
	}

	// Aliases must also reject (Apply may see cfg.Provider as alias before canonicalization).
	for _, alias := range []string{"wasm", "wazero"} {
		cfgA := &core.Config{Provider: alias}
		fsA := flag.NewFlagSet("alias-class", flag.ContinueOnError)
		_ = fsA.String("class", "", "")
		_ = fsA.Parse([]string{"--class=foo"})
		if err := ApplyWasiProviderFlags(cfgA, fsA, nil); err == nil {
			t.Fatalf("expected --class reject for alias %s", alias)
		}
		fsT := flag.NewFlagSet("alias-type", flag.ContinueOnError)
		_ = fsT.String("type", "", "")
		_ = fsT.Parse([]string{"--type=foo"})
		if err := ApplyWasiProviderFlags(cfgA, fsT, nil); err == nil {
			t.Fatalf("expected --type reject for alias %s", alias)
		}
	}
}

func TestWasiBackendBasics(t *testing.T) {
	spec := core.ProviderSpec{Name: providerName, Kind: core.ProviderKindDelegatedRun, Features: core.FeatureSet{core.FeatureArchiveSync}}
	b := NewBackend(spec, core.Config{}, core.Runtime{}).(interface {
		Spec() core.ProviderSpec
	})
	if b.Spec().Name != providerName {
		t.Fatal("backend spec name")
	}
	if d, ok := b.(core.DoctorBackend); ok {
		res, _ := d.Doctor(context.Background(), core.DoctorRequest{})
		if res.Provider != providerName {
			t.Fatal("doctor provider")
		}
	}
}

func TestWasiPureBuiltinSkipsSync(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	backend := newTestBackend(t, core.Config{}, &stdout, &stderr)
	root := t.TempDir()

	result, err := backend.Run(context.Background(), core.RunRequest{
		Repo:    core.Repo{Root: root, Name: "repo"},
		Command: []string{"echo", "ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d", result.ExitCode)
	}
	if strings.TrimSpace(stdout.String()) != "ok" {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "sync_skipped=true") {
		t.Fatalf("stderr missing sync skip: %q", stderr.String())
	}
}

func TestWasiRunWazeroModuleWithSyncedWorkdir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	module := buildWasip1Module(t, `package main

import (
	"fmt"
	"os"
)

func main() {
	data, err := os.ReadFile("/work/input.txt")
	if err != nil {
		fmt.Printf("read=%v\n", err)
		os.Exit(3)
	}
	fmt.Printf("args=%s,%s env=%s file=%s", os.Args[1], os.Args[2], os.Getenv("CRABBOX_WASI_TEST"), string(data))
}
`)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "input.txt"), "hello from sync")
	gitInit(t, root)

	var stdout, stderr bytes.Buffer
	backend := newTestBackend(t, core.Config{
		Wasi: core.WasiConfig{GuestBaseDir: t.TempDir()},
	}, &stdout, &stderr)
	result, err := backend.Run(context.Background(), core.RunRequest{
		Repo:    core.Repo{Root: root, Name: "repo"},
		Keep:    true,
		Command: []string{module, "one", "two"},
		Env:     map[string]string{"CRABBOX_WASI_TEST": "yes"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d", result.ExitCode)
	}
	want := "args=one,two env=yes file=hello from sync"
	if got := stdout.String(); got != want {
		t.Fatalf("stdout=%q want %q stderr=%q", got, want, stderr.String())
	}
	if result.Session == nil || !result.Session.Kept || result.Session.Provider != providerName {
		t.Fatalf("session=%#v", result.Session)
	}
}

func TestWasiBuiltinsRejectGuestPathEscapes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	backend := newTestBackend(t, core.Config{}, &stdout, &stderr)
	guestRoot := t.TempDir()
	workdir := filepath.Join(guestRoot, defaultWasiWorkdir)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(workdir, "inside.txt"), "inside")
	writeFile(t, filepath.Join(guestRoot, "secret.txt"), "secret")

	for _, command := range [][]string{
		{"cat", "../secret.txt"},
		{"cat", "/etc/passwd"},
		{"ls", ".."},
	} {
		t.Run(strings.Join(command, "_"), func(t *testing.T) {
			code, _, err := backend.executeCommand(context.Background(), guestRoot, core.RunRequest{}, command)
			if err == nil || !strings.Contains(err.Error(), "outside /work") {
				t.Fatalf("code=%d err=%v, want outside /work rejection", code, err)
			}
		})
	}
}

func TestWasiBuiltinsRejectSymlinkEscapes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires extra privileges on some Windows hosts")
	}
	var stdout, stderr bytes.Buffer
	backend := newTestBackend(t, core.Config{}, &stdout, &stderr)
	guestRoot := t.TempDir()
	workdir := filepath.Join(guestRoot, defaultWasiWorkdir)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeFile(t, outside, "secret")
	if err := os.Symlink(outside, filepath.Join(workdir, "link.txt")); err != nil {
		t.Fatal(err)
	}

	code, _, err := backend.executeCommand(context.Background(), guestRoot, core.RunRequest{}, []string{"cat", "link.txt"})
	if err == nil || !strings.Contains(err.Error(), "resolves outside /work") {
		t.Fatalf("code=%d err=%v, want symlink escape rejection", code, err)
	}
}

func TestWasiCopyManifestRejectsRepoSymlinkEscapes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires extra privileges on some Windows hosts")
	}
	var stdout, stderr bytes.Buffer
	backend := newTestBackend(t, core.Config{}, &stdout, &stderr)
	repo := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeFile(t, outside, "secret")
	if err := os.Symlink(outside, filepath.Join(repo, "link.txt")); err != nil {
		t.Fatal(err)
	}

	err := backend.copyManifestToGuest(repo, t.TempDir(), core.SyncManifest{Files: []string{"link.txt"}})
	if err == nil || !strings.Contains(err.Error(), "outside repo root") {
		t.Fatalf("err=%v, want symlink escape rejection", err)
	}
}

func TestWasiCopyManifestPreservesSymlinksWithoutLeakingIgnoredTargets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires extra privileges on some Windows hosts")
	}
	var stdout, stderr bytes.Buffer
	backend := newTestBackend(t, core.Config{}, &stdout, &stderr)
	repo := t.TempDir()
	// secret is "excluded" (not listed in manifest)
	secret := filepath.Join(repo, "secret.txt")
	writeFile(t, secret, "VERYSECRET")
	// symlink to secret; the *link path* is included in manifest (passes exclude)
	link := filepath.Join(repo, "link-to-secret.txt")
	if err := os.Symlink("secret.txt", link); err != nil {
		t.Fatal(err)
	}
	// manifest only includes the link (secret excluded)
	manifest := core.SyncManifest{Files: []string{"link-to-secret.txt"}}
	guestRoot := t.TempDir()
	if err := backend.copyManifestToGuest(repo, guestRoot, manifest); err != nil {
		t.Fatalf("copy: %v", err)
	}
	// guest tree has default workdir "crabbox" subdir
	guestWork := filepath.Join(guestRoot, "crabbox")
	linkInGuest := filepath.Join(guestWork, "link-to-secret.txt")
	fi, err := os.Lstat(linkInGuest)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected symlink at link-to-secret.txt, got mode %v", fi.Mode())
	}
	// no leak: reading the link should fail (target not present in guest)
	if _, err := os.ReadFile(linkInGuest); err == nil {
		t.Fatal("expected error reading via symlink to excluded target (leak of secret content)")
	}
}

func TestWasiCopyManifestRejectsGuestSymlinkDestination(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires extra privileges on some Windows hosts")
	}
	var stdout, stderr bytes.Buffer
	backend := newTestBackend(t, core.Config{}, &stdout, &stderr)
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "file.txt"), "safe")
	guestRoot := t.TempDir()
	workdir := filepath.Join(guestRoot, defaultWasiWorkdir)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeFile(t, outside, "secret")
	if err := os.Symlink(outside, filepath.Join(workdir, "file.txt")); err != nil {
		t.Fatal(err)
	}

	err := backend.copyManifestToGuest(repo, guestRoot, core.SyncManifest{Files: []string{"file.txt"}})
	if err == nil || !strings.Contains(err.Error(), "destination") || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v, want symlink destination rejection", err)
	}
}

func TestWasiCopyManifestRejectsGuestSymlinkParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires extra privileges on some Windows hosts")
	}
	var stdout, stderr bytes.Buffer
	backend := newTestBackend(t, core.Config{}, &stdout, &stderr)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "dir", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "dir", "sub", "file.txt"), "safe")
	guestRoot := t.TempDir()
	workdir := filepath.Join(guestRoot, defaultWasiWorkdir)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workdir, "dir")); err != nil {
		t.Fatal(err)
	}

	err := backend.copyManifestToGuest(repo, guestRoot, core.SyncManifest{Files: []string{"dir/sub/file.txt"}})
	if err == nil || !strings.Contains(err.Error(), "destination") || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v, want symlink parent rejection", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "sub")); !os.IsNotExist(err) {
		t.Fatalf("symlink parent was followed before rejection; outside/sub stat err=%v", err)
	}
}

func TestWasiCopyManifestRejectsSymlinkedGuestRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires extra privileges on some Windows hosts")
	}
	var stdout, stderr bytes.Buffer
	backend := newTestBackend(t, core.Config{}, &stdout, &stderr)
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "file.txt"), "safe")

	// A reused guest root that has been replaced with a symlink pointing at a
	// host directory must be rejected before any workdir mkdir/copy follows it.
	outside := t.TempDir()
	guestRoot := filepath.Join(t.TempDir(), "guest")
	if err := os.Symlink(outside, guestRoot); err != nil {
		t.Fatal(err)
	}

	err := backend.copyManifestToGuest(repo, guestRoot, core.SyncManifest{Files: []string{"file.txt"}})
	if err == nil || !strings.Contains(err.Error(), "guest root") || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v, want symlinked guest root rejection", err)
	}
	if _, err := os.Stat(filepath.Join(outside, defaultWasiWorkdir)); !os.IsNotExist(err) {
		t.Fatalf("symlinked guest root was followed before rejection; outside/%s stat err=%v", defaultWasiWorkdir, err)
	}
}

func TestWasiRemoveAllNoFollowRejectsSymlinkedWorkdirParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires extra privileges on some Windows hosts")
	}
	guestRoot := t.TempDir()
	// Host directory whose contents must survive a delete-sync reset.
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "keep.txt"), "host-secret")
	// Kept/tampered guest: an intermediate workdir parent is a symlink to the
	// host directory, and the configured workdir nests below it.
	if err := os.Symlink(outside, filepath.Join(guestRoot, "work")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(guestRoot, "work", "sub")

	err := removeAllNoFollow(guestRoot, target)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v, want symlinked component rejection", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Fatalf("host file deleted through symlinked workdir parent: %v", err)
	}
}

func TestWasiRemoveAllNoFollowRemovesRealWorkdir(t *testing.T) {
	guestRoot := t.TempDir()
	target := filepath.Join(guestRoot, defaultWasiWorkdir)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(target, "stale.txt"), "old")

	if err := removeAllNoFollow(guestRoot, target); err != nil {
		t.Fatalf("removeAllNoFollow on real workdir: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("real workdir was not removed; stat err=%v", err)
	}
	// Removing a non-existent target is a no-op, not an error.
	if err := removeAllNoFollow(guestRoot, target); err != nil {
		t.Fatalf("removeAllNoFollow on missing target: %v", err)
	}
}

func TestWasiWorkdirRejectsEscapes(t *testing.T) {
	for _, workdir := range []string{"/work", "/etc", "../etc", "repo/../../../etc", ".", "./.."} {
		t.Run(workdir, func(t *testing.T) {
			if got, err := cleanWasiWorkdir(workdir); err == nil {
				t.Fatalf("workdir=%q, want error for %q", got, workdir)
			}
		})
	}
}

func TestWasiCleanupCommandQuotesLeaseID(t *testing.T) {
	got := wasiCleanupCommand("cbx_abc;touch")
	if got != "crabbox stop --provider wasi 'cbx_abc;touch'" {
		t.Fatalf("cleanup command=%q", got)
	}
}

func TestWasiListStatusAndDoctorUseLocalClaims(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	backend := newTestBackend(t, core.Config{}, &stdout, &stderr)
	root := t.TempDir()

	result, err := backend.Run(context.Background(), core.RunRequest{
		Repo:          core.Repo{Root: root, Name: "repo"},
		Keep:          true,
		RequestedSlug: "wasi-proof",
		Command:       []string{"echo", "ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	leases, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].CloudID != result.LeaseID || leases[0].Labels["slug"] != "wasi-proof" || leases[0].Status != "ready" {
		t.Fatalf("leases=%#v result=%#v", leases, result)
	}
	status, err := backend.Status(context.Background(), core.StatusRequest{ID: "wasi-proof"})
	if err != nil {
		t.Fatal(err)
	}
	if status.ID != result.LeaseID || status.Slug != "wasi-proof" || !status.Ready || status.Provider != providerName {
		t.Fatalf("status=%#v result=%#v", status, result)
	}
	doctor, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doctor.Message, "leases=1") {
		t.Fatalf("doctor=%#v", doctor)
	}
}

func TestWasiStatusAndStopRequireKnownClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	backend := newTestBackend(t, core.Config{}, &stdout, &stderr)

	status, err := backend.Status(context.Background(), core.StatusRequest{ID: "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if status.ID != "missing" || status.State != "not-found" {
		t.Fatalf("status=%#v", status)
	}
	err = backend.Stop(context.Background(), core.StopRequest{ID: "missing"})
	if err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("Stop err=%v, want not found", err)
	}
}

func TestWasiGuestRootPathDefaultsToTempDir(t *testing.T) {
	var stdout, stderr bytes.Buffer
	b, ok := NewBackend(Provider{}.Spec(), core.Config{}, core.Runtime{Stdout: &stdout, Stderr: &stderr}).(*backend)
	if !ok {
		t.Fatal("backend type mismatch")
	}
	guest, err := b.guestRootPath("lease")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(guest) != filepath.Clean(os.TempDir()) {
		t.Fatalf("guest=%q, want under %q", guest, os.TempDir())
	}
}

func TestWasiPrepareGuestRejectsRelativeGuestBase(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	var stdout, stderr bytes.Buffer
	backend := newTestBackend(t, core.Config{
		Wasi: core.WasiConfig{GuestBaseDir: "guest-roots"},
	}, &stdout, &stderr)
	_, _, err := backend.prepareGuest(context.Background(), repo, "lease", false, false)
	if err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("err=%v, want absolute path rejection", err)
	}
}

func TestWasiPrepareGuestRejectsGuestRootInsideRepo(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "input.txt"), "data")
	gitInit(t, repo)
	var stdout, stderr bytes.Buffer
	backend := newTestBackend(t, core.Config{
		Wasi: core.WasiConfig{GuestBaseDir: filepath.Join(repo, "guest-roots")},
	}, &stdout, &stderr)
	_, _, err := backend.prepareGuest(context.Background(), repo, "lease", false, false)
	if err == nil || !strings.Contains(err.Error(), "overlaps repo root") {
		t.Fatalf("err=%v, want overlap rejection", err)
	}
	// The rejection must land before any host-side write: an in-repo guest
	// root would otherwise enter later sync manifests and pollute /work.
	if _, statErr := os.Stat(filepath.Join(repo, "guest-roots")); !os.IsNotExist(statErr) {
		t.Fatalf("guest root was created inside the repo: stat err=%v", statErr)
	}
}

func TestWasiPrepareGuestRejectsRepoInsideGuestRoot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := t.TempDir()
	repo := filepath.Join(base, "crabbox-wasi-lease", "checkout")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	backend := newTestBackend(t, core.Config{
		Wasi: core.WasiConfig{GuestBaseDir: base},
	}, &stdout, &stderr)
	_, _, err := backend.prepareGuest(context.Background(), repo, "lease", false, false)
	if err == nil || !strings.Contains(err.Error(), "overlaps repo root") {
		t.Fatalf("err=%v, want overlap rejection", err)
	}
}

func newTestBackend(t *testing.T, cfg core.Config, stdout, stderr io.Writer) *backend {
	t.Helper()
	if cfg.Wasi.GuestBaseDir == "" {
		cfg.Wasi.GuestBaseDir = t.TempDir()
	}
	rt := core.Runtime{
		Stdout: stdout,
		Stderr: stderr,
	}
	if rt.Exec == nil {
		rt.Exec = core.NewExecCommandRunner()
	}
	b, ok := NewBackend(Provider{}.Spec(), cfg, rt).(*backend)
	if !ok {
		t.Fatal("backend type mismatch")
	}
	return b
}

func buildWasip1Module(t *testing.T, source string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.test/wasi\n")
	writeFile(t, filepath.Join(dir, "main.go"), source)
	outPath := filepath.Join(dir, "main.wasm")
	cmd := exec.Command("go", "build", "-trimpath", "-o", outPath, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build wasip1 module: %v\n%s", err, out)
	}
	return outPath
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Concise unit tests for cases previously only in the (now trimmed for conciseness) E2E.
// These do not require full git sync flows or real module execution.

func TestWasiWarmupRejectsActionsRunner(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := newTestBackend(t, core.Config{}, &bytes.Buffer{}, &bytes.Buffer{})
	err := b.Warmup(context.Background(), core.WarmupRequest{
		Repo: core.Repo{Root: t.TempDir(), Name: "r"}, ActionsRunner: true,
	})
	if err == nil || !strings.Contains(err.Error(), "actions-runner") {
		t.Fatalf("err=%v", err)
	}
}

func TestWasiConfigureRejectsBadRuntime(t *testing.T) {
	_, err := (Provider{}).Configure(core.Config{Wasi: core.WasiConfig{Runtime: "bad"}}, core.Runtime{})
	if err == nil {
		t.Fatal("want configure error")
	}
}

// Test that the wasmtime invocation construction never puts secret *values*
// into the argv (only bare --env KEY names). Values live only in the returned
// child env. This is the key security property for --allow-env + wasmtime.
func TestWasiWasmtimeBuildArgsNeverPutsValuesInArgv(t *testing.T) {
	// Set an unrelated host env var. It must be absent from childEnv (the
	// wasmtime subprocess must not inherit ambient host state beyond the
	// explicit allow-list + minimal runtime base). This is the key regression
	// for the wasmtime child-env P1.
	t.Setenv("CRABBOX_TEST_UNRELATED_HOST_VAR", "must-not-leak-to-wasmtime")

	args, childEnv := buildWasmtimeRunArgsAndEnv("/guest/work", []string{"mod.wasm", "arg1"}, map[string]string{
		"SECRET":      "s3cr3t-value",
		"OTHER":       "public",
		"CRABBOX_FOO": "bar",
	})

	// argv must contain bare --env KEY (no =val anywhere in args)
	for _, a := range args {
		if strings.Contains(a, "=") && (strings.HasPrefix(a, "SECRET=") || strings.HasPrefix(a, "s3cr3t") || strings.Contains(a, "s3cr3t-value")) {
			t.Fatalf("secret value leaked into wasmtime argv: %q (full args=%v)", a, args)
		}
	}
	// must have the bare keys
	if !contains(args, "--env") || !contains(args, "SECRET") || !contains(args, "OTHER") {
		t.Fatalf("expected bare --env KEY in args, got %v", args)
	}

	// child env must have the values (but not the argv)
	envMap := envSliceToMap(childEnv)
	if envMap["SECRET"] != "s3cr3t-value" || envMap["OTHER"] != "public" {
		t.Fatalf("child env missing values: %v", childEnv)
	}
	if _, hasPath := envMap["PATH"]; !hasPath {
		t.Fatalf("expected PATH in childEnv for wasmtime binary: %v", childEnv)
	}
	// argv must not have the values
	for _, a := range args {
		if a == "s3cr3t-value" || a == "public" {
			t.Fatalf("value appeared in argv: %v", args)
		}
	}

	// Unrelated host var must be absent (restriction regression).
	if _, hasUnrel := envMap["CRABBOX_TEST_UNRELATED_HOST_VAR"]; hasUnrel {
		t.Fatalf("unrelated host env var leaked into wasmtime childEnv: %v", childEnv)
	}

	// --dir mapping must use guest::host form with :: (not =) so the guest
	// preopens exactly /work from the host guestWork dir. Matches wazero
	// WithDirMount and wasmtime tutorial remap examples (e.g. --dir /var/tmp::/tmp).
	if !contains(args, "--dir") {
		t.Fatal("missing --dir")
	}
	dirMapping := ""
	for i, a := range args {
		if a == "--dir" && i+1 < len(args) {
			dirMapping = args[i+1]
			break
		}
	}
	if dirMapping != "/work::/guest/work" {
		t.Fatalf("bad --dir mapping %q, want /work::/guest/work (guest::host with ::)", dirMapping)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func envSliceToMap(env []string) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			m[k] = v
		}
	}
	return m
}
