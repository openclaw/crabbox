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
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunDetachedGitSeedPreservesManifestAuthority(t *testing.T) {
	realRsync, err := exec.LookPath("rsync")
	if err != nil {
		t.Skip("rsync unavailable")
	}
	// Retain the real hydration and ownership scripts without paying remote
	// polling and runner-stop grace periods in this local fixture.
	pollInterval, stopDelay := localActionsHydrationPollInterval, actionsHydrationStopSettleDelay
	localActionsHydrationPollInterval, actionsHydrationStopSettleDelay = 10*time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		localActionsHydrationPollInterval, actionsHydrationStopSettleDelay = pollInterval, stopDelay
	})
	for _, tc := range []struct {
		name                                                            string
		published, checksum, delete, missingGit, githubSSH, hydrate     bool
		localBase, explicit, unavailableBase, manyExcluded, dropPublish bool
	}{
		{name: "unpublished-warm", delete: true, hydrate: true},
		{name: "unpublished-hydration", delete: true, hydrate: true},
		{name: "unpublished-explicit", delete: true, hydrate: true, explicit: true},
		{name: "local-base-script", delete: true, hydrate: true, localBase: true},
		{name: "local-base-many-exclusions", delete: true, hydrate: true, localBase: true, manyExcluded: true},
		{name: "local-base-retry-exclusions", delete: true, hydrate: true, localBase: true, manyExcluded: true, dropPublish: true},
		{name: "local-base-explicit", delete: true, hydrate: true, localBase: true, explicit: true},
		{name: "local-base-no-hydrate", delete: true, localBase: true},
		{name: "local-base-unavailable", delete: true, hydrate: true, localBase: true, unavailableBase: true},
		{name: "published-hydration", published: true, delete: true, hydrate: true},
		{name: "published-checksum", published: true, checksum: true, delete: true},
		{name: "published-delta", published: true, delete: true},
		{name: "github-ssh-origin", published: true, delete: true, githubSSH: true},
		{name: "unpublished", delete: true},
		{name: "delete-disabled", published: true},
		{name: "missing-git", published: true, delete: true, missingGit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			f := newGitCoherenceFixture(t)
			runGit(t, f.source, "checkout", "--quiet", "--detach", f.b)
			if tc.manyExcluded {
				for index := range 200 {
					mustWriteTestFile(t, filepath.Join(f.source, "other", fmt.Sprintf("public-%03d.txt", index)), "excluded published bytes\n")
				}
				runGit(t, f.source, "add", "other")
				runGit(t, f.source, "commit", "-qm", "published excluded files")
				f.b = gitOutput(f.source, "rev-parse", "HEAD")
				runGit(t, f.source, "push", "--quiet", "origin", "HEAD:refs/heads/fixture-base")
			}
			payload := strings.Repeat("unchanged seed payload\n", 32768)
			mustWriteTestFile(t, filepath.Join(f.source, "payload.txt"), payload)
			runGit(t, f.source, "add", "payload.txt")
			if tc.hydrate {
				runGit(t, f.source, "remote", "set-head", "origin", "main")
				mustWriteTestFile(t, filepath.Join(f.source, "other/private.txt"), "excluded private commit bytes\n")
				runGit(t, f.source, "add", "other/private.txt")
				runEnvProfileTestCloseBeforeRelease = true
				t.Cleanup(func() { runEnvProfileTestCloseBeforeRelease = false })
			}
			runGit(t, f.source, "commit", "-qm", "detached CI commit")
			head := gitOutput(f.source, "rev-parse", "HEAD")
			if tc.localBase {
				// Match a detached source mirror: borrowed objects, one local base
				// ref, and an origin URL without any fetched tracking refs.
				mirror := t.TempDir()
				runGit(t, mirror, "init", "--quiet", "--template=")
				objects := gitOutput(f.source, "rev-parse", "--path-format=absolute", "--git-path", "objects")
				mustWriteTestFile(t, filepath.Join(mirror, ".git/objects/info/alternates"), objects+"\n")
				runGit(t, mirror, "update-ref", "--no-deref", "HEAD", head)
				runGit(t, mirror, "update-ref", "refs/heads/main", f.b)
				runGit(t, mirror, "read-tree", "--reset", "-u", head)
				runGit(t, mirror, "remote", "add", "origin", f.origin)
				f.source = mirror
				requireGitOutput(t, f.source, "", "for-each-ref", "refs/remotes/origin")
				requireGitOutput(t, f.source, f.b, "merge-base", "HEAD", "main")
			}
			if tc.published {
				runGit(t, f.source, "push", "--quiet", "origin", "HEAD:refs/pull/12/merge")
			}
			if tc.githubSSH {
				runGit(t, f.source, "remote", "set-url", "origin", "git@github.com:example-org/project.git")
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
			if tc.hydrate {
				mustWriteTestFile(t, filepath.Join(f.source, "staged.txt"), "staged bytes\n")
				runGit(t, f.source, "add", "staged.txt")
			}
			sourceIndex := gitOutput(f.source, "write-tree")
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
			baseRef := "main"
			if tc.unavailableBase {
				baseRef = "unavailable"
			}
			mustWriteTestFile(t, configPath, fmt.Sprintf("workRoot: %q\nsync:\n  checksum: %t\n  delete: %t\n  baseRef: %s\n  exclude:\n    - other\n", remoteRoot, tc.checksum, tc.delete, baseRef))
			if tc.hydrate {
				workflow := `on:
  workflow_dispatch:
    inputs:
      crabbox_id: {required: true}
      crabbox_runner_label: {required: true}
      crabbox_keep_alive_minutes: {required: true}
jobs:
  hydrate:
    steps:
      - uses: actions/checkout@v4
      - run: |
          printf prepared > hydration-proof
          mkdir -p "$HOME/.crabbox/actions"
          printf 'WORKSPACE=%s\nJOB=hydrate\nREADY_AT=ready\n' "$GITHUB_WORKSPACE" > "$HOME/.crabbox/actions/${INPUT_CRABBOX_ID}.env"
`
				mustWriteTestFile(t, filepath.Join(f.source, ".github/workflows/hydrate.yml"), workflow)
				file, err := os.OpenFile(configPath, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteString("actions:\n  workflow: .github/workflows/hydrate.yml\n  repo: example-org/project\n")
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CRABBOX_CONFIG", configPath)
			binDir := filepath.Join(root, "bin")
			if err := os.Mkdir(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			installWorkspaceOwnerAwareSSH(t, filepath.Join(binDir, "ssh"), `#!/bin/bash
probe="$1"
for depth in 1 2 3 4 5 6 7 8; do
  case "$probe" in
    *'payload_b64="'*'"; decoded=; if command -v base64'*)
      encoded=${probe#*'payload_b64="'}; encoded=${encoded%%'"; decoded=; if command -v base64'*}
      probe=$(printf %s "$encoded" | /usr/bin/base64 --decode 2>/dev/null) || break ;;
    *) break ;;
  esac
done
case "$probe" in *crabbox-ready*) exit 0 ;; esac
if [ "$CRABBOX_SEED_MISSING_GIT" = true ]; then
  case "$1" in
    *"crabbox-git-seed phase=prerequisite"*) exec /usr/bin/env PATH=/nonexistent /bin/bash --noprofile --norc -c "$1" ;;
  esac
fi
# Substitute only the public network destination with the fixture's Git mirror.
# Planning, seed publication, file transfer and the Git-root guard execute normally.
remote="${1//https:\/\/github.com\/example-org\/project.git/$CRABBOX_SEED_ORIGIN}"
if [ "$CRABBOX_SEED_DROP_PUBLISH" = true ] && [ ! -f "$CRABBOX_SEED_DROP_FILE" ]; then
  case "$probe" in
    *"crabbox-git-seed phase=prerequisite"*)
      /bin/bash --noprofile --norc -c "$remote" > "$CRABBOX_SEED_DROP_FILE.output"
      result=$?
      cat "$CRABBOX_SEED_DROP_FILE.output"
      if [ "$result" = 0 ] && grep -Fxq 'crabbox-git-seed published' "$CRABBOX_SEED_DROP_FILE.output"; then
        touch "$CRABBOX_SEED_DROP_FILE"
        exit 255
      fi
      exit "$result"
      ;;
  esac
fi
exec /bin/bash --noprofile --norc -c "$remote"
`)
			if tc.hydrate {
				if runtime.GOOS == "darwin" {
					stat := "#!/bin/sh\nif [ \"$1\" = -c ] && [ \"$2\" = %u ]; then shift 2; exec /usr/bin/stat -f %u \"$@\"; fi\nexec /usr/bin/stat \"$@\"\n"
					if err := os.WriteFile(filepath.Join(binDir, "stat"), []byte(stat), 0755); err != nil {
						t.Fatal(err)
					}
				}
				// Keep real ownership and background lifecycle scripts for the combined path.
				script, err := os.ReadFile(filepath.Join(binDir, "ssh-command"))
				if err != nil {
					t.Fatal(err)
				}
				script = bytes.Replace(script, []byte("#!/bin/bash\n"), []byte("#!/bin/bash\nfor arg do if [ \"$arg\" = -G ]; then exec /usr/bin/ssh \"$@\"; fi; done\ncmd=; for arg do cmd=\"$arg\"; done; set -- \"$cmd\"\n"), 1)
				if err := os.WriteFile(filepath.Join(binDir, "ssh"), script, 0755); err != nil {
					t.Fatal(err)
				}
			}
			// Only the SSH destination is adapted; real rsync consumes the production
			// manifest, checksum settings, and seeded files.
			rsyncScript := `#!/bin/bash
set -euo pipefail
# Local destinations otherwise enable whole-file mode, unlike an SSH transfer.
args=(--no-whole-file)
while [ "$#" -gt 0 ]; do
  case "$1" in
    -e) shift 2 ;;
    --rsync-path) receiver="$2"; receiver="${receiver/#\~\//$HOME/}"; args+=("--rsync-path" "$receiver"); shift 2 ;;
    *) args+=("$1"); shift ;;
  esac
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
			t.Setenv("CRABBOX_SEED_DROP_PUBLISH", fmt.Sprint(tc.dropPublish))
			t.Setenv("CRABBOX_SEED_DROP_FILE", filepath.Join(root, "dropped-publication"))
			t.Setenv("CRABBOX_SEED_MISSING_GIT", fmt.Sprint(tc.missingGit))
			t.Setenv("CRABBOX_SEED_ORIGIN", f.origin)
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
			args := []string{"--provider", providerName, "--no-hydrate", "--sync-only", "--debug"}
			if tc.hydrate {
				args = []string{"--provider", providerName, "--debug", "--shell", "--", "test -f hydration-proof && printf workload-reached"}
			}
			if tc.name == "unpublished-warm" {
				runGit(t, f.source, "checkout", "--quiet", "--detach", f.b)
				bootstrapArgs := []string{"--provider", providerName, "--keep", "--shell", "--", "test -f hydration-proof"}
				if err := app.runCommand(context.Background(), bootstrapArgs); err != nil {
					t.Fatalf("published bootstrap: %v\n%s", err, stderr.String())
				}
				runGit(t, f.source, "checkout", "--quiet", "--detach", head)
				args = append([]string{"--id", "cbx_detached_seed", "--stop-after", "always"}, args...)
			}
			if tc.localBase && tc.hydrate {
				script := filepath.Join(root, "workload.sh")
				mustWriteTestFile(t, script, "test -f hydration-proof && printf workload-reached\n")
				args = []string{"--provider", providerName, "--debug", "--script", script}
			}
			if tc.explicit {
				if err := app.actionsHydrate(t.Context(), []string{"--provider", providerName, "--id", "cbx_detached_seed"}); err != nil {
					t.Fatalf("explicit hydration: %v\n%s", err, stderr.String())
				}
				if tc.localBase {
					cfg := baseConfig()
					cfg.Sync.BaseRef = "main"
					target := SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "22", TargetOS: targetLinux, SSHConfigProxy: true}
					fresh, err := app.seedLocalActionsAncestor(t.Context(), cfg, repo, target, workdir)
					if err != nil || fresh {
						t.Fatalf("existing workspace reported fresh publication: fresh=%t err=%v", fresh, err)
					}
				}
				args = append([]string{"--id", "cbx_detached_seed", "--stop-after", "always"}, args...)
			}
			err = app.runCommand(context.Background(), args)
			if tc.unavailableBase {
				if err == nil || !strings.Contains(err.Error(), "cannot verify Actions workspace Git root and origin") || strings.Contains(stdout.String(), "workload-reached") {
					t.Fatalf("unavailable base admitted workload: err=%v stdout=%s", err, stdout.String())
				}
				if _, err := os.Lstat(filepath.Join(workdir, ".git")); !os.IsNotExist(err) {
					t.Fatalf("unavailable base published Git metadata: %v", err)
				}
				requireGitOutput(t, f.source, sourceIndex, "write-tree")
				requireGitOutput(t, f.source, refsBefore, "show-ref")
				requireGitOutput(t, f.source, head, "rev-parse", "HEAD")
				return
			}
			if err != nil {
				t.Fatalf("sync: %v\n%s\n%s", err, stdout.String(), stderr.String())
			}
			if tc.dropPublish {
				if _, err := os.Stat(filepath.Join(root, "dropped-publication")); err != nil {
					t.Fatalf("publication transport failure was not exercised: %v", err)
				}
			}
			seeded, err := os.ReadFile(beforeRsync)
			if err != nil {
				t.Fatal(err)
			}
			if tc.hydrate && !tc.published {
				requireGitOutput(t, workdir, f.b, "rev-parse", "HEAD")
				if gitOutput(workdir, "cat-file", "-t", head) != "" {
					t.Fatal("private Git history was transferred")
				}
				if (tc.name == "unpublished-hydration" || tc.localBase) && !strings.Contains(stderr.String(), "local Actions Git baseline="+f.b) {
					t.Fatal("baseline identity was not reported")
				}
				if !strings.Contains(stdout.String(), "workload-reached") {
					t.Fatal("private workload was not reached")
				}
			} else if tc.published && tc.delete && !tc.missingGit {
				if strings.TrimSpace(string(seeded)) != head {
					t.Fatalf("detached commit was not seeded before rsync: %q", seeded)
				}
				requireGitOutput(t, workdir, head, "rev-parse", "HEAD")
				if tc.githubSSH {
					// Hydration may adopt the cold workspace only after sync supplies
					// an exact Git root with the authorized origin.
					target := SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "22", TargetOS: targetLinux, SSHConfigProxy: true}
					if err := verifyActionsWorkspace(t.Context(), target, Repo{RemoteURL: f.origin}, actionsHydrationState{Workspace: workdir}); err != nil {
						t.Fatalf("cold sync did not prepare an adoptable Actions workspace: %v", err)
					}
				}
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
			for _, name := range []string{"deleted.txt", "other/omit.txt", "other/private.txt", ".git/crabbox/sync-fingerprint"} {
				if _, err := os.Lstat(filepath.Join(workdir, name)); !os.IsNotExist(err) {
					t.Fatalf("unexpected remote path %s: %v", name, err)
				}
			}
			if tc.manyExcluded {
				if _, err := os.Lstat(filepath.Join(workdir, "other")); !os.IsNotExist(err) {
					t.Fatalf("excluded published files remain: %v", err)
				}
			}
			if tc.hydrate {
				if data, err := os.ReadFile(filepath.Join(workdir, "staged.txt")); err != nil || string(data) != "staged bytes\n" {
					t.Fatalf("staged bytes changed: %q %v", data, err)
				}
			}
			requireGitOutput(t, f.source, sourceIndex, "write-tree")
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
