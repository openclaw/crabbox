//go:build !windows

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/tabwriter"

	"github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestBlacksmithStopReconciliationCLIExit255(t *testing.T) {
	if os.Getenv("CRABBOX_TEST_BLACKSMITH_CHILD") == "1" {
		os.Args = []string{"crabbox", "run", "--timing-json", "--", "test-runner"}
		main()
		return
	}

	dirs := testutil.IsolateUserDirs(t)
	repo, bin := t.TempDir(), t.TempDir()
	const id = "tbx_process123"
	config := filepath.Join(repo, "crabbox.yaml")
	if err := os.WriteFile(config, []byte("provider: blacksmith-testbox\nblacksmith:\n  org: example-org\n  workflow: .github/workflows/testbox.yml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := cli.TestboxKeyPath(id)
	if err != nil {
		t.Fatal(err)
	}
	claimPath := filepath.Join(dirs.StateHome, "crabbox", "claims", id+".json")
	callsPath := filepath.Join(bin, "calls")
	var nativeStatus strings.Builder
	w := tabwriter.NewWriter(&nativeStatus, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tIP\tWORKFLOW\tJOB\tREF\tCREATED\tRUN URL")
	fmt.Fprintf(w, "%s\tcompleted\t\t.github/workflows/testbox.yml\tgo\tmain\t2026-08-30T12:00:00.123456Z\thttps://github.com/example-org/my-app/actions/runs/123456789\n", id)
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(bin, "native-status.txt")
	if err := os.WriteFile(statusPath, []byte(nativeStatus.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusPath+".ready", []byte(strings.Replace(nativeStatus.String(), "completed", "ready    ", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := `#!/bin/sh
set -eu
case " $* " in *' --api-url https://backend.blacksmith.sh --org example-org '*) ;; *) exit 91;; esac
if [ "$1" = '--org' ]; then [ "$2" = 'example-org' ] || exit 91; shift 2; fi
printf '%s %s\n' "$1" "$2" >> "$CRABBOX_TEST_CALLS"
case "$1 $2" in
  'testbox warmup') printf 'tbx_process123\n' ;;
  'testbox run')
    [ "$3" = '--id' ] && [ "$4" = 'tbx_process123' ] || exit 92
    [ -f "$CRABBOX_TEST_CLAIM" ] && [ -f "$CRABBOX_TEST_KEY" ] || exit 93
    printf 'FAIL: synthetic assertion mismatch\n' >&2
    exit 255 ;;
  'testbox stop')
    [ "$3" = '--id' ] && [ "$4" = 'tbx_process123' ] || exit 94
    touch "$CRABBOX_TEST_STATUS.stopped"
    printf 'Error: stop failed: HTTP 409: testbox already stopped\n' >&2
    exit 1 ;;
  'testbox status')
    [ "$3" = '--id' ] && [ "$4" = 'tbx_process123' ] || exit 95
    if [ -f "$CRABBOX_TEST_STATUS.stopped" ]; then cat "$CRABBOX_TEST_STATUS"; else cat "$CRABBOX_TEST_STATUS.ready"; fi ;;
  *) exit 96 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "blacksmith"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	// The child receives only synthetic configuration and test-owned user state.
	childEnv := []string{
		"PATH=" + bin + ":/usr/bin:/bin",
		"HOME=" + dirs.Home,
		"XDG_CONFIG_HOME=" + dirs.ConfigHome,
		"XDG_STATE_HOME=" + dirs.StateHome,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"CRABBOX_CONFIG=" + config,
		"CRABBOX_TEST_BLACKSMITH_CHILD=1",
		"CRABBOX_TEST_CALLS=" + callsPath,
		"CRABBOX_TEST_CLAIM=" + claimPath,
		"CRABBOX_TEST_KEY=" + key,
		"CRABBOX_TEST_STATUS=" + statusPath,
	}
	git := exec.CommandContext(t.Context(), "/usr/bin/git", "init", "-q", repo)
	git.Env = childEnv
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("init synthetic repo: %v: %s", err, output)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(t.Context(), binary, "-test.run=^TestBlacksmithStopReconciliationCLIExit255$")
	child.Env, child.Dir = childEnv, repo
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	err = child.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 255 {
		t.Fatalf("CLI process err=%v; stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	wantCalls := []string{"testbox warmup", "testbox status", "testbox status", "testbox run", "testbox status", "testbox stop", "testbox status", "testbox status"}
	if strings.Join(lines, "\n") != strings.Join(wantCalls, "\n") {
		t.Fatalf("unexpected provider calls: %s", calls)
	}
	for _, path := range []string{claimPath, key} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("cleanup left %s: %v", path, err)
		}
	}
	for _, want := range []string{
		"FAIL: synthetic assertion mismatch",
		"blacksmith testbox run exited 255",
		`"exitCode":255`,
		"cleanup reconciled lease=" + id + " state=completed",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q: %s", want, stderr.String())
		}
	}
	if strings.Contains(stderr.String(), "Error: stop failed") || strings.Contains(stderr.String(), "cleanup failed") {
		t.Fatalf("reconciled cleanup printed an unqualified failure: %s", stderr.String())
	}
}
