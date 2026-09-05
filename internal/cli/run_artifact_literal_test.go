package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestRunArtifactLiteralMatching(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"proof.json", " proof.json ", "leading.json", "trailing.json ", "Case.json",
		"nested/proof.json", "nested/proof.json ", " nested/proof.json",
		"nested/.git/private", "nested/.crabbox/private",
		"\u00c2\u00a0proof.json",
	} {
		file := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]string{
		"leaf-link": "proof.json", "dangling": "missing", "parent-link": "nested",
	} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
	}
	for i, tt := range []struct {
		glob string
		want []string
	}{
		{"proof.json", []string{"proof.json"}},
		{"./proof.json", []string{"proof.json"}},
		{"case.json", nil},
		{"Case.json", []string{"Case.json"}},
		{" proof.json ", []string{" proof.json "}},
		// Trimmed Unicode whitespace retains the legacy byte-quoted regexp behavior.
		{"\u00a0proof.json", []string{"\u00c2\u00a0proof.json"}},
		{" leading.json", nil},
		{"trailing.json ", []string{"trailing.json "}},
		{"nested/proof.json", []string{"nested/proof.json"}},
		{"./nested/proof.json", []string{"nested/proof.json"}},
		{"nested/proof.json ", []string{"nested/proof.json "}},
		{" nested/proof.json", nil},
		{"nested//proof.json", nil},
		{"nested/./proof.json", nil},
		{"leaf-link", []string{"leaf-link"}},
		{"dangling", nil},
		{"parent-link/proof.json", nil},
		{"nested/.git/private", nil},
		{"nested/.crabbox/private", nil},
		{"nested", nil},
		{"missing.json", nil},
	} {
		t.Run(fmt.Sprintf("%q", tt.glob), func(t *testing.T) {
			globs := []string{tt.glob, tt.glob}
			out, err := exec.CommandContext(t.Context(), "bash", "-c", runArtifactRequireScript(dir, globs)).CombinedOutput()
			if len(tt.want) > 0 {
				if err != nil {
					t.Fatalf("required literal: %v\n%s", err, out)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 8 {
					t.Fatalf("error=%v, want missing-required exit 8; output=%s", err, out)
				}
			}
			archive := fmt.Sprintf(".crabbox/literal-%d.tgz", i)
			out, err = exec.CommandContext(t.Context(), "bash", "-c", runArtifactCollectScript(dir, archive, globs)).CombinedOutput()
			if err != nil {
				t.Fatalf("collect literal: %v\n%s", err, out)
			}
			if names := tarGzNames(t, filepath.Join(dir, archive)); !slices.Equal(names, tt.want) {
				t.Fatalf("archive names=%q, want %q", names, tt.want)
			}
		})
	}
}
