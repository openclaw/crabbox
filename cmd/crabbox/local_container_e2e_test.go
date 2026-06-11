//go:build localcontainer

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/crabbox/internal/cli"
)

func TestLocalContainerProviderE2E(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("missing docker CLI: %v", err)
	}
	dockerCtx, dockerCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dockerCancel()
	if out, err := exec.CommandContext(dockerCtx, "docker", "version").CombinedOutput(); err != nil {
		t.Skipf("docker daemon unavailable: %v: %s", err, strings.TrimSpace(string(out)))
	}

	repoRoot := localContainerRepoRoot(t)
	t.Chdir(repoRoot)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(home, "missing.yaml"))
	clearLocalContainerE2EEnv(t)

	image := strings.TrimSpace(os.Getenv("CRABBOX_LOCAL_CONTAINER_E2E_IMAGE"))
	if image == "" {
		image = "debian:bookworm"
	}
	tag := strings.ToLower(strings.ReplaceAll(t.Name(), "_", "-"))
	if len(tag) > 16 {
		tag = tag[:16]
	}
	tag = strings.Trim(tag, "-") + "-" + time.Now().UTC().Format("150405")
	oneShotSlug := tag + "-one"
	warmSlug := tag + "-warm"

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cleanupCancel()
		_, _ = runCrabboxLocalContainerE2E(cleanupCtx, "stop", "--provider", "docker", oneShotSlug)
		_, _ = runCrabboxLocalContainerE2E(cleanupCtx, "stop", "--provider", "docker", warmSlug)
	})

	oneShot := runCrabboxLocalContainerE2EMust(t, ctx,
		"run",
		"--provider", "docker",
		"--local-container-runtime", "docker",
		"--local-container-image", image,
		"--slug", oneShotSlug,
		"--timing-json",
		"--shell",
		"--",
		"set -eu; test -f go.mod; test -f internal/providers/localcontainer/backend.go; echo CRABBOX_LOCAL_CONTAINER_SYNC_OK",
	)
	if !strings.Contains(oneShot.Stdout, "CRABBOX_LOCAL_CONTAINER_SYNC_OK") {
		t.Fatalf("one-shot output missing sync marker: stdout=%q stderr=%q", oneShot.Stdout, oneShot.Stderr)
	}
	assertNoLocalContainerForSlug(t, ctx, oneShotSlug)

	warmup := runCrabboxLocalContainerE2EMust(t, ctx,
		"warmup",
		"--provider", "docker",
		"--local-container-runtime", "docker",
		"--local-container-image", image,
		"--slug", warmSlug,
		"--timing-json",
	)
	leaseID := parseLocalContainerE2ELeaseID(warmup.Stdout)
	if leaseID == "" {
		t.Fatalf("could not parse local-container lease id: stdout=%q stderr=%q", warmup.Stdout, warmup.Stderr)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cleanupCancel()
		_, _ = runCrabboxLocalContainerE2E(cleanupCtx, "stop", "--provider", "docker", leaseID)
	})

	runCrabboxLocalContainerE2EMust(t, ctx, "status", "--provider", "docker", "--id", leaseID, "--wait", "--json")
	reuse := runCrabboxLocalContainerE2EMust(t, ctx,
		"run",
		"--provider", "docker",
		"--id", leaseID,
		"--no-sync",
		"--timing-json",
		"--",
		"echo", "CRABBOX_LOCAL_CONTAINER_REUSE_OK",
	)
	if !strings.Contains(reuse.Stdout, "CRABBOX_LOCAL_CONTAINER_REUSE_OK") {
		t.Fatalf("reuse output missing marker: stdout=%q stderr=%q", reuse.Stdout, reuse.Stderr)
	}
	runCrabboxLocalContainerE2EMust(t, ctx, "stop", "--provider", "docker", leaseID)
	assertNoLocalContainerForSlug(t, ctx, warmSlug)
}

type localContainerE2EResult struct {
	Stdout string
	Stderr string
}

func runCrabboxLocalContainerE2EMust(t *testing.T, ctx context.Context, args ...string) localContainerE2EResult {
	t.Helper()
	result, err := runCrabboxLocalContainerE2E(ctx, args...)
	if err != nil {
		t.Fatalf("crabbox %s failed: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, result.Stdout, result.Stderr)
	}
	return result
}

func runCrabboxLocalContainerE2E(ctx context.Context, args ...string) (localContainerE2EResult, error) {
	var stdout, stderr bytes.Buffer
	err := (cli.App{Stdout: &stdout, Stderr: &stderr}).Run(ctx, args)
	return localContainerE2EResult{Stdout: stdout.String(), Stderr: stderr.String()}, err
}

func assertNoLocalContainerForSlug(t *testing.T, ctx context.Context, slug string) {
	t.Helper()
	commandCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(commandCtx, "docker", "ps", "-aq",
		"--filter", "label=crabbox=true",
		"--filter", "label=provider=local-container",
		"--filter", "label=slug="+slug,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("docker ps for slug %s failed: %v: %s", slug, err, strings.TrimSpace(string(out)))
	}
	if ids := strings.TrimSpace(string(out)); ids != "" {
		t.Fatalf("local-container e2e left containers for slug=%s: %s", slug, ids)
	}
}

func parseLocalContainerE2ELeaseID(stdout string) string {
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "leased" {
			return fields[1]
		}
	}
	return ""
}

func localContainerRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root containing go.mod")
		}
		dir = parent
	}
}

func clearLocalContainerE2EEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"CRABBOX_PROVIDER",
		"CRABBOX_LOCAL_CONTAINER_RUNTIME",
		"CRABBOX_LOCAL_CONTAINER_IMAGE",
		"CRABBOX_LOCAL_CONTAINER_USER",
		"CRABBOX_LOCAL_CONTAINER_WORK_ROOT",
		"CRABBOX_LOCAL_CONTAINER_CPUS",
		"CRABBOX_LOCAL_CONTAINER_MEMORY",
		"CRABBOX_LOCAL_CONTAINER_NETWORK",
		"CRABBOX_LOCAL_CONTAINER_DOCKER_SOCKET",
		"CRABBOX_COORDINATOR",
		"CRABBOX_COORDINATOR_TOKEN",
		"CRABBOX_COORDINATOR_ADMIN_TOKEN",
		"CRABBOX_ADMIN_TOKEN",
	} {
		t.Setenv(key, "")
	}
}
