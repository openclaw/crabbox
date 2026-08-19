//go:build darwin

package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runStockMacBash(t *testing.T, script string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("/bin/bash", "-c", script)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	return cmd.CombinedOutput()
}

func TestRunArtifactScriptsWithStockMacOSTools(t *testing.T) {
	version, err := runStockMacBash(t, `printf '%s.%s' "${BASH_VERSINFO[0]}" "${BASH_VERSINFO[1]}"`)
	if err != nil {
		t.Fatalf("inspect /bin/bash: %v: %s", err, version)
	}
	if got := strings.TrimSpace(string(version)); got != "3.2" {
		t.Fatalf("/bin/bash version=%q, want the stock macOS-compatible 3.2 shell", got)
	}

	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "reports", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reports", "nested", "proof.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outsideProof := filepath.Join(outside, "outside.json")
	if err := os.WriteFile(outsideProof, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideProof, filepath.Join(dir, "reports", "leaf.json")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "reports", "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	t.Run("required present", func(t *testing.T) {
		out, err := runStockMacBash(t, runArtifactRequireScript(dir, []string{"reports/**/*.json"}))
		if err != nil {
			t.Fatalf("require script failed: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "matched=2") {
			t.Fatalf("required script did not include nested and leaf matches:\n%s", out)
		}
	})

	t.Run("required missing and symlink root", func(t *testing.T) {
		for _, glob := range []string{"reports/missing.json", "reports/link/*.json"} {
			out, err := runStockMacBash(t, runArtifactRequireScript(dir, []string{glob}))
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 8 {
				t.Fatalf("glob=%q error=%v, want exit 8; output=%s", glob, err, out)
			}
		}
	})

	t.Run("collect nested without following directory symlink", func(t *testing.T) {
		archivePath := filepath.Join(dir, ".crabbox", "artifacts.tgz")
		out, err := runStockMacBash(t, runArtifactCollectScript(dir, ".crabbox/artifacts.tgz", []string{"reports/**/*.json"}))
		if err != nil {
			t.Fatalf("collect script failed: %v\n%s", err, out)
		}
		names := tarGzNames(t, archivePath)
		for _, want := range []string{"reports/nested/proof.json", "reports/leaf.json"} {
			if !stringSliceContains(names, want) {
				t.Fatalf("archive missing %q: %#v", want, names)
			}
		}
		if stringSliceContains(names, "reports/link/outside.json") {
			t.Fatalf("archive followed directory symlink: %#v", names)
		}
	})

	t.Run("empty optional archive", func(t *testing.T) {
		archivePath := filepath.Join(dir, ".crabbox", "empty.tgz")
		out, err := runStockMacBash(t, runArtifactCollectScript(dir, ".crabbox/empty.tgz", []string{"missing/**/*.txt"}))
		if err != nil {
			t.Fatalf("empty collect script failed: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "warning: no artifact matches") {
			t.Fatalf("missing empty warning: %s", out)
		}
		if names := tarGzNames(t, archivePath); len(names) != 0 {
			t.Fatalf("empty archive names=%#v", names)
		}
	})
}
