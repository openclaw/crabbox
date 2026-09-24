//go:build !windows

package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Execute the Windows raw-destination path in PowerShell even on POSIX hosts.
// A raw workspace never reaches the Win32 identity check for managed roots.
func TestSyncWindowsOriginSeedPreservesRawWorkspace(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell unavailable")
	}
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprintf("malformed-%t", malformed), func(t *testing.T) {
			f := newGitCoherenceFixture(t)
			root := t.TempDir()
			workdir := filepath.Join(root, "raw [workspace]")
			preserved := map[string]string{"node_modules/cache.bin": "dependency\x00\xff", "dist/build.txt": "build\n", ".hidden": "hidden\n"}
			for name, contents := range preserved {
				mustWriteTestFile(t, filepath.Join(workdir, name), contents)
			}
			if malformed {
				mustWriteTestFile(t, filepath.Join(workdir, ".git"), "gitdir: missing\n")
			}
			binDir := filepath.Join(root, "bin")
			mustWriteTestFile(t, filepath.Join(binDir, "powershell.exe"), "#!/bin/sh\nexec "+shellQuote(pwsh)+" \"$@\"\n")
			mustWriteTestFile(t, filepath.Join(binDir, "ssh"), "#!/bin/sh\nfor arg do remote=\"$arg\"; done\nexec /bin/bash -c \"$remote\"\n")
			for _, name := range []string{"ssh", "powershell.exe"} {
				if err := os.Chmod(filepath.Join(binDir, name), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			cfg := baseConfig()
			cfg.Sync.Delete = false
			manifest := SyncManifest{Files: []string{"modified.txt"}}
			var stderr bytes.Buffer
			err := syncWindowsNative(context.Background(), SSHTarget{User: "fixture", Host: "127.0.0.1", TargetOS: targetWindows}, Repo{Root: f.source}, cfg, f.plan(t, f.b), workdir, manifest, io.Discard, &stderr, rsyncOptions{FullResync: true})
			if malformed {
				if err == nil || !strings.Contains(err.Error(), "remote git seed failed") {
					t.Fatalf("unsafe Windows sync continued: %v\n%s", err, stderr.String())
				}
				if _, err := os.Stat(filepath.Join(workdir, "modified.txt")); !os.IsNotExist(err) {
					t.Fatalf("unsafe Windows sync transferred files: %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("Windows fallback: %v\n%s", err, stderr.String())
				}
				if !strings.Contains(stderr.String(), "reason=raw_workspace") {
					t.Fatalf("missing Windows fallback: %s", stderr.String())
				}
				requireWorkspaceFile(t, filepath.Join(workdir, "modified.txt"), "base\n")
				if _, err := os.Lstat(filepath.Join(workdir, ".git")); !os.IsNotExist(err) {
					t.Fatalf("Windows raw workspace gained metadata: %v", err)
				}
			}
			for name, want := range preserved {
				requireWorkspaceFile(t, filepath.Join(workdir, name), want)
			}
		})
	}
}

func TestRunOriginSeedRetryPreservesRawWorkspace(t *testing.T) {
	realRsync, err := exec.LookPath("rsync")
	if err != nil {
		t.Skip("rsync unavailable")
	}
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprintf("malformed-%t", malformed), func(t *testing.T) {
			clearConfigEnv(t)
			f := newGitCoherenceFixture(t)
			t.Chdir(f.source)
			repo, err := findRepo()
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			isolateRunTestUserDirs(t, root)
			remoteRoot := filepath.Join(root, "remote")
			workdir := filepath.Join(remoteRoot, "cbx_seed_retry", repo.Name)
			configPath := filepath.Join(root, "config.yaml")
			mustWriteTestFile(t, configPath, fmt.Sprintf("workRoot: %q\nsync:\n  delete: true\n  exclude: [other]\n", remoteRoot))
			t.Setenv("CRABBOX_CONFIG", configPath)
			binDir := filepath.Join(root, "bin")
			if err := os.Mkdir(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			installWorkspaceOwnerAwareSSH(t, filepath.Join(binDir, "ssh"), `#!/bin/sh
case "$1" in *crabbox-ready*) exit 0 ;; esac
exec /bin/bash --noprofile --norc -c "$1"
`)
			mustWriteTestFile(t, filepath.Join(binDir, "rsync"), `#!/bin/bash
set -euo pipefail
args=()
while [ "$#" -gt 0 ]; do
  case "$1" in -e) shift 2 ;; *) args+=("$1"); shift ;; esac
done
last=$((${#args[@]} - 1))
args[$last]="${args[$last]#*:}"
exec `+shellQuote(realRsync)+` "${args[@]}"
`)
			if err := os.Chmod(filepath.Join(binDir, "rsync"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
			t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
			providerName := runEnvProfileTestProvider{}.Spec().Name
			runEnvProfileTestAcquireLease = func(AcquireRequest) (LeaseTarget, error) {
				return LeaseTarget{Server: Server{Provider: providerName}, SSH: SSHTarget{
					User: "crabbox", Host: "127.0.0.1", Port: "22", TargetOS: targetLinux, SSHConfigProxy: true,
				}, LeaseID: "cbx_seed_retry"}, nil
			}
			t.Cleanup(func() { runEnvProfileTestAcquireLease = nil })
			var stdout bytes.Buffer
			stderr := newSynchronizedBuffer(0)
			app := App{Stdout: &stdout, Stderr: &stderr}
			sync := func() error {
				return app.runCommand(context.Background(), []string{"--provider", providerName, "--no-hydrate", "--sync-only"})
			}
			if err := os.Rename(f.origin, f.origin+".offline"); err != nil {
				t.Fatal(err)
			}
			if err := sync(); err != nil {
				t.Fatalf("initial fallback: %v\n%s", err, stderr.String())
			}
			if err := os.Rename(f.origin+".offline", f.origin); err != nil {
				t.Fatal(err)
			}
			preserved := map[string]string{"node_modules/cache.bin": "dependency\x00\xff", "dist/build.txt": "build\n", "other/omit.txt": "unmanaged upstream name\n"}
			for name, contents := range preserved {
				mustWriteTestFile(t, filepath.Join(workdir, name), contents)
			}
			mustWriteTestFile(t, filepath.Join(f.source, "modified.txt"), "updated local bytes\n")
			if err := os.Remove(filepath.Join(f.source, "deleted.txt")); err != nil {
				t.Fatal(err)
			}
			if malformed {
				mustWriteTestFile(t, filepath.Join(workdir, ".git"), "gitdir: missing\n")
			}
			err = sync()
			if malformed {
				if err == nil || !strings.Contains(err.Error(), "remote git seed failed") {
					t.Fatalf("malformed state allowed transfer: %v\n%s", err, stderr.String())
				}
				requireWorkspaceFile(t, filepath.Join(workdir, "modified.txt"), "base\n")
			} else {
				if err != nil {
					t.Fatalf("retry: %v\n%s", err, stderr.String())
				}
				if !strings.Contains(stderr.String(), "reason=raw_workspace; using plain manifest sync") {
					t.Fatalf("missing plain fallback: %s", stderr.String())
				}
				requireWorkspaceFile(t, filepath.Join(workdir, "modified.txt"), "updated local bytes\n")
				for _, name := range []string{"deleted.txt", ".git", ".crabbox/sync-fingerprint"} {
					if _, err := os.Lstat(filepath.Join(workdir, name)); !os.IsNotExist(err) {
						t.Errorf("unexpected %s: %v", name, err)
					}
				}
				manifest, err := os.ReadFile(filepath.Join(workdir, ".crabbox/sync-manifest"))
				if err != nil || !bytes.Contains(manifest, []byte("modified.txt\x00")) || bytes.Contains(manifest, []byte("deleted.txt\x00")) {
					t.Fatalf("fallback manifest not finalized: %q %v", manifest, err)
				}
			}
			for name, want := range preserved {
				requireWorkspaceFile(t, filepath.Join(workdir, name), want)
			}
		})
	}
}
