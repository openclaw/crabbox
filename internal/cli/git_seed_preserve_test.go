package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGitSeedPreservesRawWorkspace(t *testing.T) {
	f := newGitCoherenceFixture(t)
	for _, branchless := range []bool{false, true} {
		if branchless && runtime.GOOS == "windows" {
			continue // Native Windows retains branch-only seeding.
		}
		for _, withManifest := range []bool{false, true} {
			name := "branch"
			if branchless {
				name = "exact-commit"
			}
			if withManifest {
				name += "-owned"
			}
			t.Run(name, func(t *testing.T) {
				workdir := filepath.Join(t.TempDir(), "raw [workspace]")
				files := map[string]string{
					"node_modules/fixture.bin": "dependency\x00\xff\n",
					"dist/proof.txt":           "build output\n",
					"tracked.txt":              "unmanaged upstream filename\n",
					".hidden":                  "hidden bytes\n",
				}
				if withManifest {
					files[".crabbox/sync-manifest"] = "previous.txt\x00"
					files["previous.txt"] = "previously managed\n"
				}
				for name, contents := range files {
					mustWriteTestFile(t, filepath.Join(workdir, name), contents)
				}
				plan := f.plan(t, f.b)
				if branchless {
					plan.Branch = ""
				}
				var out []byte
				var err error
				if runtime.GOOS == "windows" {
					out, err = runDecodedWindowsPowerShell(t, windowsGitSeed(workdir, plan))
				} else {
					out, err = exec.Command("bash", "-c", remoteGitSeed(workdir, plan)).CombinedOutput()
				}
				for name, want := range files {
					if got, readErr := os.ReadFile(filepath.Join(workdir, name)); readErr != nil || string(got) != want {
						t.Errorf("seed changed %s: data=%q err=%v", name, got, readErr)
					}
				}
				if reason, fallback := gitSeedRuntimeFallbackResult(plan, string(out), err); !fallback || reason != "raw_workspace" {
					t.Fatalf("seed did not request plain manifest fallback: err=%v output=%s reason=%q", err, out, reason)
				}
				if _, err := os.Lstat(filepath.Join(workdir, ".git")); !os.IsNotExist(err) {
					t.Fatalf("raw workspace gained Git metadata: %v", err)
				}
			})
		}
	}
}

func TestGitSeedRejectsMalformedWorkspace(t *testing.T) {
	f := newGitCoherenceFixture(t)
	for _, mode := range []string{"git-file", "git-directory", "git-symlink", "unborn", "missing-index", "corrupt-index", "bare"} {
		t.Run(mode, func(t *testing.T) {
			workdir := filepath.Join(t.TempDir(), "work")
			if err := os.Mkdir(workdir, 0o755); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "git-file":
				mustWriteTestFile(t, filepath.Join(workdir, ".git"), "gitdir: missing\n")
			case "git-directory":
				mustWriteTestFile(t, filepath.Join(workdir, ".git", "sentinel"), "partial metadata")
			case "git-symlink":
				if err := os.Symlink("missing", filepath.Join(workdir, ".git")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "unborn":
				runGit(t, workdir, "init")
			case "bare":
				runGit(t, workdir, "init", "--bare")
			default:
				runGit(t, workdir, "clone", "--quiet", f.origin, ".")
				index := filepath.Join(workdir, ".git", "index")
				if mode == "missing-index" {
					if err := os.Remove(index); err != nil {
						t.Fatal(err)
					}
				} else {
					mustWriteTestFile(t, index, "broken index")
				}
			}
			mustWriteTestFile(t, filepath.Join(workdir, "preserve.txt"), "preserve\x00bytes")
			plan := f.plan(t, f.b)
			var out []byte
			var err error
			if runtime.GOOS == "windows" {
				out, err = runDecodedWindowsPowerShell(t, windowsGitSeed(workdir, plan))
			} else {
				out, err = exec.Command("bash", "-c", remoteGitSeed(workdir, plan)).CombinedOutput()
			}
			if err == nil || exitCode(err) != gitSeedUnsafeWorkspaceExitCode || !strings.Contains(string(out), "unsafe workspace") {
				t.Fatalf("unsafe seed was not rejected: err=%v output=%s", err, out)
			}
			if _, fallback := gitSeedRuntimeFallbackResult(plan, string(out), err); fallback {
				t.Fatal("unsafe Git state permitted file transfer")
			}
			if got, err := os.ReadFile(filepath.Join(workdir, "preserve.txt")); err != nil || string(got) != "preserve\x00bytes" {
				t.Fatalf("rejection changed workspace: data=%q err=%v", got, err)
			}
		})
	}
}
