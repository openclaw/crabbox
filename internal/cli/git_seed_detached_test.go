//go:build !windows

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestRunDetachedGitSeedPreservesManifestAuthority(t *testing.T) {
	realRsync, err := exec.LookPath("rsync")
	if err != nil {
		t.Skip("rsync unavailable")
	}
	for _, tc := range []struct {
		name                                    string
		published, checksum, delete, missingGit bool
	}{
		{"published-checksum", true, true, true, false},
		{"published-delta", true, false, true, false},
		{"unpublished", false, false, true, false},
		{"delete-disabled", true, false, false, false},
		{"missing-git", true, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			f := newGitCoherenceFixture(t)
			runGit(t, f.source, "checkout", "--quiet", "--detach", f.b)
			payload := strings.Repeat("unchanged seed payload\n", 32768)
			mustWriteTestFile(t, filepath.Join(f.source, "payload.txt"), payload)
			runGit(t, f.source, "add", "payload.txt")
			runGit(t, f.source, "commit", "-qm", "detached CI commit")
			head := gitOutput(f.source, "rev-parse", "HEAD")
			if tc.published {
				runGit(t, f.source, "push", "--quiet", "origin", "HEAD:refs/pull/12/merge")
			}
			if refs := gitOutput(f.source, "for-each-ref", "--contains="+head, "refs/remotes/origin"); refs != "" {
				t.Fatalf("detached fixture has a containing tracking ref: %s", refs)
			}
			refsBefore := gitOutput(f.source, "show-ref")
			mustWriteTestFile(t, filepath.Join(f.source, "modified.txt"), "local dirty bytes\n")
			mustWriteTestFile(t, filepath.Join(f.source, "untracked.txt"), "new local bytes\n")
			if err := os.Remove(filepath.Join(f.source, "deleted.txt")); err != nil {
				t.Fatal(err)
			}
			t.Chdir(f.source)
			repo, err := findRepo()
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			isolateRunTestUserDirs(t, root)
			remoteRoot := filepath.Join(root, "remote")
			workdir := filepath.Join(remoteRoot, "cbx_detached_seed", repo.Name)
			configPath := filepath.Join(root, "config.yaml")
			mustWriteTestFile(t, configPath, fmt.Sprintf("workRoot: %q\nsync:\n  checksum: %t\n  delete: %t\n  exclude:\n    - other\n", remoteRoot, tc.checksum, tc.delete))
			t.Setenv("CRABBOX_CONFIG", configPath)
			binDir := filepath.Join(root, "bin")
			if err := os.Mkdir(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			installWorkspaceOwnerAwareSSH(t, filepath.Join(binDir, "ssh"), `#!/bin/sh
case "$1" in *crabbox-ready*) exit 0 ;; esac
if [ "$CRABBOX_SEED_MISSING_GIT" = true ]; then
  case "$1" in
    *"crabbox-git-seed phase=prerequisite"*) exec /usr/bin/env PATH=/nonexistent /bin/bash --noprofile --norc -c "$1" ;;
  esac
fi
exec /bin/bash --noprofile --norc -c "$1"
`)
			// Only the SSH destination is adapted; real rsync consumes the production
			// manifest, checksum settings, and seeded files.
			rsyncScript := `#!/bin/bash
set -euo pipefail
# Local destinations otherwise enable whole-file mode, unlike an SSH transfer.
args=(--no-whole-file)
while [ "$#" -gt 0 ]; do
  case "$1" in -e) shift 2 ;; *) args+=("$1"); shift ;; esac
done
last=$((${#args[@]} - 1))
args[$last]="${args[$last]#*:}"
git -C "${args[$last]}" rev-parse --verify HEAD > "$CRABBOX_SEED_BEFORE_RSYNC" 2>/dev/null || true
exec ` + shellQuote(realRsync) + ` "${args[@]}"
`
			if err := os.WriteFile(filepath.Join(binDir, "rsync"), []byte(rsyncScript), 0o755); err != nil {
				t.Fatal(err)
			}
			beforeRsync := filepath.Join(root, "seed-before-rsync")
			t.Setenv("CRABBOX_SEED_BEFORE_RSYNC", beforeRsync)
			t.Setenv("CRABBOX_SEED_MISSING_GIT", fmt.Sprint(tc.missingGit))
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
			t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
			providerName := runEnvProfileTestProvider{}.Spec().Name
			runEnvProfileTestAcquireLease = func(AcquireRequest) (LeaseTarget, error) {
				return LeaseTarget{Server: Server{Provider: providerName}, SSH: SSHTarget{
					User: "crabbox", Host: "127.0.0.1", Port: "22", TargetOS: targetLinux, SSHConfigProxy: true,
				}, LeaseID: "cbx_detached_seed"}, nil
			}
			t.Cleanup(func() { runEnvProfileTestAcquireLease = nil })
			var stdout bytes.Buffer
			stderr := newSynchronizedBuffer(0)
			app := App{Stdout: &stdout, Stderr: &stderr}
			if err := app.runCommand(context.Background(), []string{"--provider", providerName, "--no-hydrate", "--sync-only", "--debug"}); err != nil {
				t.Fatalf("sync: %v\n%s\n%s", err, stdout.String(), stderr.String())
			}
			seeded, err := os.ReadFile(beforeRsync)
			if err != nil {
				t.Fatal(err)
			}
			if tc.published && tc.delete && !tc.missingGit {
				if strings.TrimSpace(string(seeded)) != head {
					t.Fatalf("detached commit was not seeded before rsync: %q", seeded)
				}
				requireGitOutput(t, workdir, head, "rev-parse", "HEAD")
				// Openrsync and rsync label unmatched bytes differently; tiny
				// unchanged files may be sent literally in ordinary delta mode.
				data := regexp.MustCompile(`(?m)^(?:Literal|Unmatched) data: ([0-9,]+) (?:bytes|B)$`).FindStringSubmatch(stdout.String())
				if len(data) != 2 {
					t.Fatalf("rsync omitted literal payload statistics: %s", stdout.String())
				}
				literalBytes, err := strconv.Atoi(strings.ReplaceAll(data[1], ",", ""))
				if err != nil || literalBytes >= 1024 {
					t.Fatalf("rsync retransferred the unchanged 736 KiB payload: bytes=%d err=%v", literalBytes, err)
				}
			} else {
				if len(seeded) != 0 {
					t.Fatalf("ineligible seed materialized a checkout: %q", seeded)
				}
				if tc.delete && !strings.Contains(stderr.String(), "git origin fallback reason=exact_commit_unavailable") {
					t.Fatalf("unavailable seed did not fall back safely: stderr=%s", stderr.String())
				}
			}
			for name, want := range map[string]string{"payload.txt": payload, "modified.txt": "local dirty bytes\n", "untracked.txt": "new local bytes\n", "tracked.txt": "B\n"} {
				if got, err := os.ReadFile(filepath.Join(workdir, name)); err != nil || string(got) != want {
					t.Fatalf("synced %s differs: err=%v", name, err)
				}
			}
			for _, name := range []string{"deleted.txt", "other/omit.txt", ".git/crabbox/sync-fingerprint"} {
				if _, err := os.Lstat(filepath.Join(workdir, name)); !os.IsNotExist(err) {
					t.Fatalf("unexpected remote path %s: %v", name, err)
				}
			}
			requireGitOutput(t, f.source, refsBefore, "show-ref")
			requireGitOutput(t, f.source, head, "rev-parse", "HEAD")
		})
	}
}

func TestDetachedGitSeedRejectsUnverifiedTree(t *testing.T) {
	f := newGitCoherenceFixture(t)
	plan := gitCoherencePlan{RemoteURL: f.origin, Target: f.b, Tree: gitOutput(f.source, "rev-parse", f.a+"^{tree}")}
	root := t.TempDir()
	workdir := filepath.Join(root, "work")
	marker := filepath.Join(root, "preserve.txt")
	mustWriteTestFile(t, marker, "existing workspace\n")
	out, err := exec.Command("/bin/sh", "-c", remoteGitSeed(workdir, plan)).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "crabbox-git-seed phase=verify") {
		t.Fatalf("exact seed accepted an unverified tree: err=%v output=%s", err, out)
	}
	if exitCode(err) == gitOriginRuntimeFallbackExitCode {
		t.Fatal("verification failure reported as a speculative fetch failure")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "existing workspace\n" {
		t.Fatalf("failed seed changed sibling file: err=%v", err)
	}
	if _, err := os.Lstat(workdir); !os.IsNotExist(err) {
		t.Fatalf("failed seed published destination: %v", err)
	}
	if leftovers, err := filepath.Glob(filepath.Join(root, ".seed*")); err != nil || len(leftovers) != 0 {
		t.Fatalf("failed seed retained staging: paths=%v err=%v", leftovers, err)
	}
}
