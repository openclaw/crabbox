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
		image = "golang:1.26-bookworm"
	}
	tag := strings.ToLower(strings.ReplaceAll(t.Name(), "_", "-"))
	if len(tag) > 16 {
		tag = tag[:16]
	}
	tag = strings.Trim(tag, "-") + "-" + time.Now().UTC().Format("150405")
	oneShotSlug := tag + "-one"
	warmSlug := tag + "-warm"
	staleSlug := tag + "-stale"

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cleanupCancel()
		_, _ = runCrabboxLocalContainerE2E(cleanupCtx, "stop", "--provider", "docker", oneShotSlug)
		_, _ = runCrabboxLocalContainerE2E(cleanupCtx, "stop", "--provider", "docker", warmSlug)
		_, _ = runCrabboxLocalContainerE2E(cleanupCtx, "stop", "--provider", "docker", staleSlug)
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
		"set -eu; test -f go.mod; test -f internal/providers/localcontainer/backend.go; printf 'CRABBOX_LOCAL_CONTAINER_IMAGE_PATH=%s\\n' \"$PATH\"; echo CRABBOX_LOCAL_CONTAINER_SYNC_OK",
	)
	if !strings.Contains(oneShot.Stdout, "CRABBOX_LOCAL_CONTAINER_SYNC_OK") {
		t.Fatalf("one-shot output missing sync marker: stdout=%q stderr=%q", oneShot.Stdout, oneShot.Stderr)
	}
	if imagePath := localContainerE2EImagePath(t, ctx, image); imagePath != "" && !strings.Contains(oneShot.Stdout, "CRABBOX_LOCAL_CONTAINER_IMAGE_PATH="+imagePath+"\n") {
		t.Fatalf("one-shot PATH did not preserve image PATH %q: stdout=%q stderr=%q", imagePath, oneShot.Stdout, oneShot.Stderr)
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
	firstLogin := runCrabboxLocalContainerE2EMust(t, ctx,
		"run",
		"--provider", "docker",
		"--id", leaseID,
		"--no-sync",
		"--timing-json",
		"--shell",
		"--",
		`set -eu; test "$(command -v go)" = /usr/local/go/bin/go; test "$(stat -c '%U:%G:%a' /etc/profile.d/crabbox-image-path.sh)" = root:root:644; mkdir -p "$HOME/profile-bin"; printf '%s\n' 'export PATH="$HOME/profile-bin:$PATH"' > "$HOME/.bash_profile"; echo CRABBOX_LOCAL_CONTAINER_FIRST_LOGIN_OK`,
	)
	if !strings.Contains(firstLogin.Stdout, "CRABBOX_LOCAL_CONTAINER_FIRST_LOGIN_OK") {
		t.Fatalf("first login output missing marker: stdout=%q stderr=%q", firstLogin.Stdout, firstLogin.Stderr)
	}
	secondLogin := runCrabboxLocalContainerE2EMust(t, ctx,
		"run",
		"--provider", "docker",
		"--id", leaseID,
		"--no-sync",
		"--timing-json",
		"--shell",
		"--",
		`set -eu; test "$(command -v go)" = /usr/local/go/bin/go; test "${PATH%%:*}" = "$HOME/profile-bin"; printf 'CRABBOX_LOCAL_CONTAINER_PROFILE_PATH=%s\n' "$PATH"; echo CRABBOX_LOCAL_CONTAINER_SECOND_LOGIN_OK`,
	)
	if !strings.Contains(secondLogin.Stdout, "CRABBOX_LOCAL_CONTAINER_SECOND_LOGIN_OK") ||
		!strings.Contains(secondLogin.Stdout, "CRABBOX_LOCAL_CONTAINER_PROFILE_PATH=/home/crabbox/profile-bin:") {
		t.Fatalf("second login did not retain profile precedence and image PATH: stdout=%q stderr=%q", secondLogin.Stdout, secondLogin.Stderr)
	}
	runCrabboxLocalContainerE2EMust(t, ctx, "stop", "--provider", "docker", leaseID)
	assertNoLocalContainerLeaseState(t, ctx, leaseID, warmSlug)

	staleWarmup := runCrabboxLocalContainerE2EMust(t, ctx,
		"warmup",
		"--provider", "docker",
		"--local-container-runtime", "docker",
		"--local-container-image", image,
		"--slug", staleSlug,
	)
	staleLeaseID := parseLocalContainerE2ELeaseID(staleWarmup.Stdout)
	if staleLeaseID == "" {
		t.Fatalf("could not parse stale local-container lease id: stdout=%q stderr=%q", staleWarmup.Stdout, staleWarmup.Stderr)
	}
	keyPath, err := cli.TestboxKeyPath(staleLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	containerID := localContainerIDForSlug(t, ctx, staleSlug)
	runDockerLocalContainerE2EMust(t, ctx, "rm", "-f", containerID)
	if _, err := runCrabboxLocalContainerE2E(ctx, "status", "--provider", "docker", "--id", staleSlug); err == nil {
		t.Fatal("normal status succeeded after external container removal")
	}
	runCrabboxLocalContainerE2EMust(t, ctx, "stop", "--provider", "docker", staleSlug)
	if claim, err := cli.ReadLeaseClaim(staleLeaseID); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("stale claim still exists after stop: %#v", claim)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("stale key still exists after stop: %v", err)
	}
	assertNoLocalContainerForSlug(t, ctx, staleSlug)
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
	if id := localContainerIDForSlug(t, ctx, slug); id != "" {
		t.Fatalf("local-container e2e left container for slug=%s: %s", slug, id)
	}
}

func assertNoLocalContainerLeaseState(t *testing.T, ctx context.Context, leaseID, slug string) {
	t.Helper()
	assertNoLocalContainerForSlug(t, ctx, slug)
	claim, err := cli.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID != "" {
		t.Fatalf("local-container e2e left lease claim after cleanup: %#v", claim)
	}
	keyPath, err := cli.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("local-container e2e left SSH key after cleanup: %v", err)
	}
}

func localContainerIDForSlug(t *testing.T, ctx context.Context, slug string) string {
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
	ids := strings.Fields(string(out))
	if len(ids) > 1 {
		t.Fatalf("multiple local-container e2e containers for slug=%s: %v", slug, ids)
	}
	if len(ids) == 1 {
		return ids[0]
	}
	return ""
}

func runDockerLocalContainerE2EMust(t *testing.T, ctx context.Context, args ...string) {
	t.Helper()
	commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(commandCtx, "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker %s failed: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
}

func localContainerE2EImagePath(t *testing.T, ctx context.Context, image string) string {
	t.Helper()
	commandCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(commandCtx, "docker", "image", "inspect", "--format", "{{range .Config.Env}}{{println .}}{{end}}", image).CombinedOutput()
	if err != nil {
		t.Fatalf("docker image inspect %s failed: %v: %s", image, err, strings.TrimSpace(string(out)))
	}
	var imagePath string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "PATH=") {
			imagePath = strings.TrimPrefix(line, "PATH=")
		}
	}
	return imagePath
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
