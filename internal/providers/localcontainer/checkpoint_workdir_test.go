package localcontainer

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCheckpointCanonicalWorkdirLiteralPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	for _, name := range []string{"relative", "dash", "absolute"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			workdir := "work ' root/repo"
			if name == "dash" {
				workdir = "-"
			}
			selected := filepath.Join(root, workdir)
			searchRoot := filepath.Join(root, "search")
			for _, dir := range []string{selected, filepath.Join(searchRoot, workdir)} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if name == "absolute" {
				workdir = selected
			}
			// Emulate only Docker exec transport; run the owner's real sh command.
			runtimePath := filepath.Join(root, "docker")
			script := "#!/bin/sh\n[ \"$1 $2\" = 'exec path-fixture' ] || exit 90\nshift 2\ncd " + core.ShellQuote(root) + " || exit 91\nexport CDPATH=" + core.ShellQuote(searchRoot) + "\nexec \"$@\"\n"
			if err := os.WriteFile(runtimePath, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			got, err := checkpointCanonicalWorkdir(ctx, checkpointScope{Runtime: runtimePath}, "path-fixture", workdir)
			want, resolveErr := filepath.EvalSymlinks(selected)
			if err != nil || resolveErr != nil || got != want {
				t.Errorf("canonical workspace=%q error=%v, want=%q error=%v", got, err, want, resolveErr)
			}
		})
	}
}
