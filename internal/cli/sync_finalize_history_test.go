//go:build !windows

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRemoteFinalizeSyncDoesNotDownloadHistoricalBlobs(t *testing.T) {
	f := newGitCoherenceFixture(t)
	runGit(t, f.origin, "config", "uploadpack.allowFilter", "true")
	oldBlob := gitOutput(f.source, "rev-parse", f.a+":tracked.txt")
	for _, scenario := range []struct {
		name               string
		coherence, hydrate bool
	}{
		{name: "coherence", coherence: true},
		{name: "hydrate", hydrate: true},
		{name: "coherence-and-hydrate", coherence: true, hydrate: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			workdir := filepath.Join(t.TempDir(), "work")
			origin := "file://" + filepath.ToSlash(f.origin)
			if out, err := exec.Command("git", "clone", "--quiet", "--depth=1", origin, workdir).CombinedOutput(); err != nil {
				t.Fatalf("shallow clone: %v\n%s", err, out)
			}
			assertOldBlobMissing := func() {
				t.Helper()
				cmd := exec.Command("git", "-C", workdir, "-c", "protocol.allow=never", "cat-file", "-e", oldBlob)
				cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
				if err := cmd.Run(); err == nil {
					t.Fatal("downloaded a blob that exists only in history")
				}
			}
			assertOldBlobMissing()
			plan := gitCoherencePlan{}
			if scenario.coherence {
				plan = f.plan(t, f.c)
				plan.RemoteURL = origin
			}
			const token = "1234567890abcdef1234567890abcdef"
			stageCoherenceFinalize(t, workdir, token)
			command := remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{
				Token: token, Coherence: plan, HydrateGit: scenario.hydrate, BaseRef: "main",
			})
			if out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", command).CombinedOutput(); err != nil {
				t.Fatalf("finalize: %v\n%s", err, out)
			}
			assertOldBlobMissing()
			requireGitOutput(t, workdir, "false", "rev-parse", "--is-shallow-repository")
			requireGitOutput(t, workdir, f.c, "rev-parse", "HEAD")
			// Commit ancestry remains available even though historical file bytes
			// are deferred until a caller explicitly asks Git for them.
			runGit(t, workdir, "merge-base", "--is-ancestor", f.a, "HEAD")
			requireGitOutput(t, workdir, "A", "show", f.a+":tracked.txt")
		})
	}
}
