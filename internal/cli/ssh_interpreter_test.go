package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSyncInterpreterAvoidsDarwinPythonDeveloperShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX remote control")
	}
	for _, system := range []string{"Darwin", "Linux"} {
		t.Run(system, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(t.TempDir(), "python-invoked")
			mustWriteTestFile(t, filepath.Join(dir, "uname"), "#!/bin/sh\nprintf '%s\\n' "+shellQuote(system)+"\n")
			if err := os.Chmod(filepath.Join(dir, "uname"), 0755); err != nil {
				t.Fatal(err)
			}
			mustWriteTestFile(t, filepath.Join(dir, "python3"), "#!/bin/sh\n: > "+shellQuote(marker)+"\nexit 23\n")
			if err := os.Chmod(filepath.Join(dir, "python3"), 0755); err != nil {
				t.Fatal(err)
			}
			mustWriteTestCommandWrapper(t, dir, "perl")
			command := remoteSyncInterpreterCommand("raise SystemExit(23)", `print "working-perl\n";`, "")
			cmd := exec.Command("/bin/sh", "-c", command)
			cmd.Env = []string{"PATH=" + dir}
			output, err := cmd.CombinedOutput()
			if system == "Darwin" {
				if err != nil || string(output) != "working-perl\n" {
					t.Fatalf("stock Darwin interpreter selection failed: %v %s", err, output)
				}
				if _, err = os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("Darwin launched the Python developer-tool shim")
				}
			} else {
				if exitCode(err) != 23 {
					t.Fatalf("Linux interpreter failure did not remain fatal: %v %s", err, output)
				}
				if _, err = os.Stat(marker); err != nil {
					t.Fatal("Linux Python preference changed")
				}
			}
		})
	}
}
func TestSyncDarwinPerlFailureDoesNotFallBackAfterPartialOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX remote control")
	}
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "python-invoked")
	for name, script := range map[string]string{"uname": "printf Darwin", "perl": "printf partial; exit 29", "python3": ": > " + shellQuote(marker) + "; exit 0"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("/bin/sh", "-c", remoteSyncInterpreterCommand("print('fallback')", "die 'fixture';", ""))
	cmd.Env = []string{"PATH=" + dir}
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 29 || string(output) != "partial" {
		t.Fatalf("chosen interpreter failure changed: %v %q", err, output)
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("interpreter failure incorrectly triggered fallback")
	}
}
func TestFreshMacOSPlainManifestPruneNeedsNoPythonOrGit(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native macOS stock-tool compatibility")
	}
	workdir := t.TempDir()
	token := "0123456789abcdef0123456789abcdef"
	mustWriteTestFile(t, filepath.Join(workdir, ".crabbox", "sync-manifest"), "keep.txt\x00stale.txt\x00")
	mustWriteTestFile(t, filepath.Join(workdir, ".crabbox", remoteSyncPendingManifestName(token)), "keep.txt\x00")
	mustWriteTestFile(t, filepath.Join(workdir, ".crabbox", remoteSyncPendingDeletedName(token)), "")
	mustWriteTestFile(t, filepath.Join(workdir, "keep.txt"), "keep")
	mustWriteTestFile(t, filepath.Join(workdir, "stale.txt"), "stale")
	mustWriteTestFile(t, filepath.Join(workdir, "unmanaged.txt"), "unmanaged")
	command := remotePruneSyncManifestForTargetMode(SSHTarget{TargetOS: targetMacOS}, workdir, token, true)
	cmd := exec.Command("/bin/sh", "-c", command)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("stock macOS plain-manifest prune failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(workdir, "stale.txt")); !os.IsNotExist(err) {
		t.Fatal("managed stale file was not pruned")
	}
	for _, path := range []string{"keep.txt", "unmanaged.txt"} {
		if _, err := os.Stat(filepath.Join(workdir, path)); err != nil {
			t.Fatal("prune removed a preserved file")
		}
	}
}
