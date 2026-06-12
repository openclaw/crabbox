package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPrewarmDryRunPlansHydratedLease(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`provider: azure
target: linux
class: standard
actions:
  workflow: hydrate.yml
  job: hydrate
  ref: main
cache:
  volumes:
    - name: pnpm
      key: repo-pnpm
      path: /var/cache/crabbox/pnpm
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"prewarm", "--dry-run", "--provider", "azure", "--azure-backend", "vm", "--desktop", "--browser", "--os", "ubuntu:24.04", "--probe-command", "node -v && pnpm -v"}); err != nil {
		t.Fatalf("prewarm dry-run failed: %v\nstderr=%s", err, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{
		"crabbox warmup --provider azure --azure-backend vm --desktop --browser --os ubuntu:24.04 --keep=true",
		"crabbox actions hydrate --azure-backend vm --provider azure --target linux",
		"--workflow hydrate.yml --job hydrate --ref main",
		"crabbox run --azure-backend vm --provider azure --target linux",
		"--no-sync --no-hydrate --shell -- 'node -v && pnpm -v'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--cache-volume") {
		t.Fatalf("azure prewarm should not request unsupported cache volume flags:\n%s", got)
	}
}

func TestPrewarmDryRunKeepsBlacksmithProviderOwned(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`provider: blacksmith-testbox
blacksmith:
  org: example-org
  workflow: testbox.yml
  job: check
cache:
  volumes:
    - name: pnpm
      key: repo-pnpm
      path: /var/cache/crabbox/pnpm
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"prewarm", "--dry-run", "--provider", "blacksmith-testbox", "--blacksmith-workflow", "testbox.yml", "--blacksmith-job", "check", "--cache-volume", "pnpm=repo-pnpm:/var/cache/crabbox/pnpm", "--probe-command", "node -v"}); err != nil {
		t.Fatalf("prewarm dry-run failed: %v\nstderr=%s", err, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "crabbox warmup --provider blacksmith-testbox") ||
		!strings.Contains(got, "--blacksmith-workflow testbox.yml") ||
		!strings.Contains(got, "--blacksmith-job check") ||
		!strings.Contains(got, "--cache-volume pnpm=repo-pnpm:/var/cache/crabbox/pnpm") {
		t.Fatalf("blacksmith warmup plan missing sticky cache volume:\n%s", got)
	}
	if strings.Contains(got, "actions hydrate") {
		t.Fatalf("blacksmith prewarm should not run local Actions hydration:\n%s", got)
	}
	if !strings.Contains(got, "crabbox run --blacksmith-workflow testbox.yml --blacksmith-job check --provider blacksmith-testbox") ||
		!strings.Contains(got, "--no-sync --no-hydrate --shell -- 'node -v'") {
		t.Fatalf("blacksmith prewarm should still run explicit probe:\n%s", got)
	}
}

func TestPrewarmDryRunKeepsLocalContainerVolumeOnWarmupOnly(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{
		"prewarm",
		"--dry-run",
		"--no-hydrate",
		"--provider", "local-container",
		"--local-container-volume", "/host/cache:/cache:ro",
		"--probe-command", "test -r /cache",
	}); err != nil {
		t.Fatalf("prewarm dry-run failed: %v\nstderr=%s", err, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("dry-run output lines=%d, want warmup and probe:\n%s", len(lines), stdout.String())
	}
	if !strings.Contains(lines[0], "--local-container-volume /host/cache:/cache:ro") {
		t.Fatalf("warmup plan missing volume flag:\n%s", stdout.String())
	}
	if strings.Contains(lines[1], "--local-container-volume") || !strings.Contains(lines[1], "--id '<lease>'") {
		t.Fatalf("probe plan should reuse the mounted lease without forwarding the creation-only flag:\n%s", stdout.String())
	}
}

func TestPrewarmRejectsServiceControlProviderBeforePlan(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`provider: service-control-test
actions:
  workflow: hydrate.yml
  job: hydrate
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run(context.Background(), []string{"prewarm", "--dry-run", "--provider", "service-control-test"})
	if err == nil {
		t.Fatalf("service-control prewarm succeeded; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(err.Error(), "prewarm is not supported for provider=service-control-test") {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(stdout.String(), "crabbox warmup") || strings.Contains(stdout.String(), "actions hydrate") {
		t.Fatalf("service-control prewarm emitted a plan:\n%s", stdout.String())
	}
}

func TestPrewarmPoolRequiresCoordinatorBeforeWarmup(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`provider: azure
target: linux
class: standard
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run(context.Background(), []string{"prewarm", "--dry-run", "--provider", "azure", "--pool", "example"})
	if err == nil {
		t.Fatalf("prewarm --pool without coordinator succeeded; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(err.Error(), "--pool requires a coordinator-backed SSH lease provider") {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(stdout.String(), "crabbox warmup") {
		t.Fatalf("prewarm --pool planned warmup before broker validation:\n%s", stdout.String())
	}
}

func TestPrewarmReadyPoolCommitUsesOnlyKnownHydratedSHA(t *testing.T) {
	repo := Repo{Root: t.TempDir(), Head: strings.Repeat("a", 40), BaseRef: "main"}
	cfg := Config{}
	if got := prewarmReadyPoolCommit(cfg, repo, false); got != repo.Head {
		t.Fatalf("empty actions ref should use local head: %q", got)
	}
	if got := prewarmReadyPoolCommit(cfg, repo, true); got != "" {
		t.Fatalf("github-runner default ref should omit commit, got %q", got)
	}

	cfg.Actions.Ref = strings.Repeat("b", 40)
	if got := prewarmReadyPoolCommit(cfg, repo, true); got != cfg.Actions.Ref {
		t.Fatalf("sha actions ref should be registered as commit: %q", got)
	}
	if got := prewarmReadyPoolCommit(cfg, repo, false); got != "" {
		t.Fatalf("local hydration should not register non-head sha as commit: %q", got)
	}

	cfg.Actions.Ref = "main"
	if got := prewarmReadyPoolCommit(cfg, repo, true); got != "" {
		t.Fatalf("github-runner branch ref should omit commit, got %q", got)
	}
	if got := prewarmReadyPoolCommit(cfg, repo, false); got != "" {
		t.Fatalf("non-checked-out actions ref should omit commit, got %q", got)
	}
}

func TestReadyPoolRunBorrowCommitOmitsBranchRef(t *testing.T) {
	repo := Repo{Head: strings.Repeat("a", 40)}
	cfg := Config{}
	if got := readyPoolRunBorrowCommit(cfg, repo); got != repo.Head {
		t.Fatalf("empty actions ref should borrow exact local head: %q", got)
	}
	if !readyPoolRunAllowsMissingCommit(cfg, repo) {
		t.Fatalf("empty actions ref should allow ref-only hydrated entries")
	}

	cfg.Actions.Ref = "main"
	if got := readyPoolRunBorrowCommit(cfg, repo); got != "" {
		t.Fatalf("branch actions ref should borrow by ref only, got %q", got)
	}

	cfg.Actions.Ref = strings.Repeat("b", 40)
	if got := readyPoolRunBorrowCommit(cfg, repo); got != repo.Head {
		t.Fatalf("sha actions ref should keep local commit filter: %q", got)
	}
	if readyPoolRunAllowsMissingCommit(cfg, repo) {
		t.Fatalf("sha actions ref should require exact commit")
	}

	dir := t.TempDir()
	runPrewarmGit(t, dir, "init", "-b", "main")
	runPrewarmGit(t, dir, "config", "user.email", "test@example.com")
	runPrewarmGit(t, dir, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPrewarmGit(t, dir, "add", "README.md")
	runPrewarmGit(t, dir, "commit", "-m", "initial")
	head := gitOutput(dir, "rev-parse", "HEAD")
	cfg.Actions.Ref = "main"
	if got := readyPoolRunBorrowCommit(cfg, Repo{Root: dir, Head: head}); got != head {
		t.Fatalf("checked-out actions branch should borrow exact head: %q", got)
	}
	if !readyPoolRunAllowsMissingCommit(cfg, Repo{Root: dir, Head: head}) {
		t.Fatalf("checked-out actions branch should allow GitHub-runner ref-only entries")
	}
}

func runPrewarmGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestPrewarmDryRunMapsGenericWorkflowFlagsForBlacksmith(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`provider: blacksmith-testbox
blacksmith:
  org: example-org
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"prewarm", "--dry-run", "--provider", "blacksmith-testbox", "--workflow", "testbox.yml", "--job", "check", "--ref", "main"}); err != nil {
		t.Fatalf("prewarm dry-run failed: %v\nstderr=%s", err, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{
		"--blacksmith-workflow testbox.yml",
		"--blacksmith-job check",
		"--blacksmith-ref main",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("blacksmith warmup plan missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "actions hydrate") || strings.Contains(got, "crabbox run") {
		t.Fatalf("blacksmith prewarm should stay provider-owned:\n%s", got)
	}
}

func TestPrewarmDryRunDoesNotBootstrapPondACL(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	t.Setenv(pondACLAutoBootstrapEnvVar, "1")
	t.Setenv("TS_API_KEY", "tskey-api-stub")
	t.Setenv("CRABBOX_TAILSCALE_AUTH_KEY", "tskey-auth-test")
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`provider: hetzner
target: linux
tailscale:
  enabled: true
  tags:
    - tag:crabbox
actions:
  workflow: hydrate.yml
  job: hydrate
`), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := &stubPondTailnetACLClient{policy: pondPolicyFixture(pondTailscaleTag(localCoordinatorOwner(), "alpha")), etag: `"v1"`}
	prev := pondTailnetACLClientFactory
	t.Cleanup(func() { pondTailnetACLClientFactory = prev })
	pondTailnetACLClientFactory = func(_ string) pondTailnetACLClient { return stub }

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"prewarm", "--dry-run", "--provider", "hetzner", "--pond", "alpha"}); err != nil {
		t.Fatalf("prewarm dry-run failed: %v\nstderr=%s", err, stderr.String())
	}
	if atomic.LoadInt32(&stub.gets) != 0 || atomic.LoadInt32(&stub.puts) != 0 {
		t.Fatalf("dry-run touched pond ACL API: gets=%d puts=%d", stub.gets, stub.puts)
	}
}
