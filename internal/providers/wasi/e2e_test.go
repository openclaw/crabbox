package wasi

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestWasiE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E skipped in -short mode")
	}
	module := buildWasip1Module(t, `package main

import ("fmt"; "os")

func main() {
	b, _ := os.ReadFile("/work/input.txt")
	fmt.Print(string(b))
}
`)

	// Common setup helper to keep subtests concise (addresses boilerplate duplication).
	newWasi := func(t *testing.T) (*backend, *bytes.Buffer, context.Context) {
		t.Helper()
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		out := &bytes.Buffer{}
		b := newTestBackend(t, core.Config{Wasi: core.WasiConfig{GuestBaseDir: t.TempDir()}}, out, &bytes.Buffer{})
		return b, out, context.Background()
	}

	t.Run("agent_flow", func(t *testing.T) {
		b, out, ctx := newWasi(t)
		repo := initRepo(t, "v1")
		slug := "agent"
		req := func(cmd []string) core.RunRequest {
			return core.RunRequest{Repo: core.Repo{Root: repo, Name: "r"}, ID: slug, Keep: true, Command: cmd}
		}

		if err := b.Warmup(ctx, core.WarmupRequest{Repo: req(nil).Repo, Keep: true, RequestedSlug: slug}); err != nil {
			t.Fatal(err)
		}
		mustOK(t, b, ctx, req([]string{module}), out, "v1")
		writeFile(t, filepath.Join(repo, "input.txt"), "v2")
		mustOK(t, b, ctx, req([]string{module}), out, "v2")
		if _, err := b.Run(ctx, req([]string{"false"})); err == nil {
			t.Fatal("want error for false")
		}
		mustOK(t, b, ctx, req([]string{module}), out, "v2")

		status, err := b.Status(ctx, core.StatusRequest{ID: slug})
		if err != nil || !status.Ready {
			t.Fatalf("status=%#v err=%v", status, err)
		}
		if err := b.Stop(ctx, core.StopRequest{ID: slug}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("sync_only", func(t *testing.T) {
		b, out, ctx := newWasi(t)
		repo := initRepo(t, "synced")

		r, err := b.Run(ctx, core.RunRequest{
			Repo: core.Repo{Root: repo, Name: "r"}, Keep: true, RequestedSlug: "sync",
			SyncOnly: true,
		})
		if err != nil || r.ExitCode != 0 || !strings.Contains(out.String(), "synced ") {
			t.Fatalf("sync-only: err=%v exit=%d out=%q", err, r.ExitCode, out.String())
		}
	})

	t.Run("no_sync", func(t *testing.T) {
		b, out, ctx := newWasi(t)
		repo := initRepo(t, "old")
		slug := "nosync"

		if err := b.Warmup(ctx, core.WarmupRequest{Repo: core.Repo{Root: repo, Name: "r"}, Keep: true, RequestedSlug: slug}); err != nil {
			t.Fatal(err)
		}
		mustOK(t, b, ctx, core.RunRequest{
			Repo: core.Repo{Root: repo, Name: "r"}, ID: slug, Keep: true, Command: []string{module},
		}, out, "old")

		writeFile(t, filepath.Join(repo, "input.txt"), "new")
		out.Reset()
		r, err := b.Run(ctx, core.RunRequest{
			Repo: core.Repo{Root: repo, Name: "r"}, ID: slug, Keep: true, NoSync: true,
			Command: []string{module},
		})
		if err != nil || r.ExitCode != 0 || out.String() != "old" {
			t.Fatalf("no-sync: err=%v out=%q want old", err, out.String())
		}
	})

	t.Run("custom_workdir", func(t *testing.T) {
		repo := t.TempDir()
		gitInit(t, repo)
		writeFile(t, filepath.Join(repo, "input.txt"), "pkg-data")
		// custom workdir requires explicit cfg; use direct newTestBackend for this one
		out := &bytes.Buffer{}
		b := newTestBackend(t, core.Config{
			Wasi: core.WasiConfig{GuestBaseDir: t.TempDir(), Workdir: "pkg"},
		}, out, &bytes.Buffer{})
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		ctx := context.Background()

		mustOK(t, b, ctx, core.RunRequest{
			Repo: core.Repo{Root: repo, Name: "r"}, Keep: true, Command: []string{module},
		}, out, "pkg-data")
	})

	t.Run("builtins_ls_cat", func(t *testing.T) {
		b, out, ctx := newWasi(t)
		repo := initRepo(t, "seen")
		slug := "ls"

		if err := b.Warmup(ctx, core.WarmupRequest{Repo: core.Repo{Root: repo, Name: "r"}, Keep: true, RequestedSlug: slug}); err != nil {
			t.Fatal(err)
		}
		req := core.RunRequest{Repo: core.Repo{Root: repo, Name: "r"}, ID: slug, Keep: true, Command: []string{"ls"}}
		if _, err := b.Run(ctx, req); err != nil {
			t.Fatalf("ls: %v", err)
		}
		if !strings.Contains(out.String(), "input.txt") {
			t.Fatalf("ls out=%q", out.String())
		}
		out.Reset()
		req.Command = []string{"cat", "input.txt"}
		if _, err := b.Run(ctx, req); err != nil {
			t.Fatalf("cat: %v", err)
		}
		if out.String() != "seen" {
			t.Fatalf("cat out=%q", out.String())
		}
	})

	t.Run("wasm_relative_in_repo", func(t *testing.T) {
		b, out, ctx := newWasi(t)
		repo := initRepo(t, "rel")
		dst := filepath.Join(repo, "tool.wasm")
		if err := copyFile(module, dst, 0o644); err != nil {
			t.Fatal(err)
		}
		mustOK(t, b, ctx, core.RunRequest{
			Repo: core.Repo{Root: repo, Name: "r"}, Keep: true, Command: []string{"tool.wasm"},
		}, out, "rel")
	})

	t.Run("timing_json", func(t *testing.T) {
		// keep this one for now (uses special errOut); can be moved to unit later for more conciseness
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		repo := initRepo(t, "t")
		errOut := &bytes.Buffer{}
		b := newTestBackend(t, core.Config{Wasi: core.WasiConfig{GuestBaseDir: t.TempDir()}}, &bytes.Buffer{}, errOut)
		ctx := context.Background()

		_, err := b.Run(ctx, core.RunRequest{
			Repo: core.Repo{Root: repo, Name: "r"}, Command: []string{"false"},
			TimingJSON: true,
		})
		if err == nil {
			t.Fatal("want exit error")
		}
		var report timingReport
		if !parseTimingJSON(t, errOut.String(), &report) {
			t.Fatalf("no timing json: %q", errOut.String())
		}
		if report.Provider != providerName || report.ExitCode != 1 {
			t.Fatalf("report=%#v", report)
		}
	})

	t.Run("wasmtime_runtime", func(t *testing.T) {
		if _, err := exec.LookPath("wasmtime"); err != nil {
			t.Skip("wasmtime not in PATH")
		}
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		repo := initRepo(t, "wt")
		out := &bytes.Buffer{}
		b := newTestBackend(t, core.Config{
			Wasi: core.WasiConfig{GuestBaseDir: t.TempDir(), Runtime: "wasmtime"},
		}, out, &bytes.Buffer{})

		// Build a dedicated module for this subtest that also exercises env forwarding
		// (end-to-end for the wasmtime + --allow-env / Env path, using the secure
		// bare --env KEY + child env mechanism). The common `module` only reads a file.
		envModule := buildWasip1Module(t, `package main

import ("fmt"; "os")

func main() {
	b, _ := os.ReadFile("/work/input.txt")
	fmt.Print(string(b))
	if e := os.Getenv("CRABBOX_WASI_ENV_TEST"); e != "" {
		fmt.Print(" env=" + e)
	}
}
`)
		mustOK(t, b, context.Background(), core.RunRequest{
			Repo:    core.Repo{Root: repo, Name: "r"},
			Command: []string{envModule},
			Env:     map[string]string{"CRABBOX_WASI_ENV_TEST": "foo123"},
		}, out, "wt env=foo123")
	})
}

func TestWasiCLIE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("CLI e2e skipped in -short mode")
	}
	root := goModuleRoot(t)
	bin := filepath.Join(t.TempDir(), "crabbox")
	build := exec.Command("go", "build", "-trimpath", "-o", bin, "./cmd/crabbox")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build cli: %v\n%s", err, out)
	}

	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "input.txt"), "cli")
	gitInit(t, repo)
	module := buildWasip1Module(t, `package main

import ("fmt"; "os")

func main() {
	b, _ := os.ReadFile("/work/input.txt")
	fmt.Print(string(b))
}
`)
	wasm := filepath.Join(repo, "tool.wasm")
	if err := copyFile(module, wasm, 0o644); err != nil {
		t.Fatal(err)
	}

	stateHome := t.TempDir()
	run := func(args ...string) (string, string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "XDG_STATE_HOME="+stateHome)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}

	if _, _, err := run("doctor", "--provider", "wasi", "--json"); err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if out, _, err := run("run", "--provider", "wasi", "--", "echo", "ok"); err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("echo: err=%v out=%q", err, out)
	}
	if _, _, err := run("warmup", "--provider", "wasi", "--slug", "cli-e2e"); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	if stdout, stderr, err := run("run", "--provider", "wasi", "--id", "cli-e2e", "--sync-only"); err != nil || (!strings.Contains(stdout, "synced") && !strings.Contains(stderr, "sync")) {
		t.Fatalf("sync-only: err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
	if out, _, err := run("run", "--provider", "wasi", "--id", "cli-e2e", "--keep", "--", "tool.wasm"); err != nil || out != "cli" {
		t.Fatalf("wasm: err=%v out=%q", err, out)
	}
	if _, _, err := run("status", "--provider", "wasi", "--id", "cli-e2e"); err != nil {
		t.Fatalf("status: %v", err)
	}
	if _, _, err := run("stop", "--provider", "wasi", "cli-e2e"); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func initRepo(t *testing.T, input string) string {
	t.Helper()
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "input.txt"), input)
	gitInit(t, repo)
	return repo
}

func mustOK(t *testing.T, b *backend, ctx context.Context, req core.RunRequest, out *bytes.Buffer, want string) {
	t.Helper()
	out.Reset()
	r, err := b.Run(ctx, req)
	if err != nil || r.ExitCode != 0 || out.String() != want {
		t.Fatalf("run %v: err=%v exit=%d out=%q want %q", req.Command, err, r.ExitCode, out.String(), want)
	}
}

func parseTimingJSON(t *testing.T, stderr string, report *timingReport) bool {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		if err := json.Unmarshal([]byte(line), report); err != nil {
			t.Fatalf("decode timing: %v line=%q", err, line)
		}
		return true
	}
	return false
}

func goModuleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/openclaw/crabbox").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
