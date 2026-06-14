package agentsandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentSandboxMountReplaceCommandReplacesContentsNotMount(t *testing.T) {
	command := agentSandboxMountReplaceCommand("/workspace/crabbox/.crabbox-sync-fixed", "/workspace/crabbox")
	for _, want := range []string{
		"rollback()",
		"mv -- \"$entry\" '/workspace/crabbox/.crabbox-sync-fixed.previous/'",
		`for entry in '/workspace/crabbox/.crabbox-sync-fixed'/*; do cp -a -- "$entry" '/workspace/crabbox/'`,
		`[ "$entry" != '/workspace/crabbox/.crabbox-sync-fixed' ]`,
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("command missing %q: %s", want, command)
		}
	}
	if strings.Contains(command, "mv '/workspace/crabbox'") {
		t.Fatalf("command attempts to rename Agent Sandbox workspace mount: %s", command)
	}
}

func TestAgentSandboxMountReplaceCommandReplacesDotfiles(t *testing.T) {
	root := t.TempDir()
	workdir := filepath.Join(root, "workspace")
	stagingDir := filepath.Join(workdir, ".crabbox-sync-fixed")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workdir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stagingDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Chmod(root, 0o700)
	}()
	if err := os.WriteFile(filepath.Join(workdir, "old.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagingDir, ".new"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("bash", "-lc", agentSandboxMountReplaceCommand(stagingDir, workdir))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("replace failed: %v: %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(workdir, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old file remains: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(workdir, ".new")); err != nil || string(data) != "new\n" {
		t.Fatalf("new file=%q err=%v", data, err)
	}
	info, err := os.Stat(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Fatalf("workdir mode=%#o want=%#o", got, 0o750)
	}
	backupDir := agentSandboxBackupDir(stagingDir, workdir)
	if _, err := os.Stat(filepath.Join(backupDir, "old.txt")); err != nil {
		t.Fatalf("committed backup missing: %v", err)
	}
	if err := os.RemoveAll(backupDir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(stagingDir); err != nil {
		t.Fatal(err)
	}
}

func TestAgentSandboxMountReplaceCommandRestoresWorkspaceAfterCopyFailure(t *testing.T) {
	root := t.TempDir()
	workdir := filepath.Join(root, "workspace")
	stagingDir := filepath.Join(workdir, ".crabbox-sync-fixed")
	fakeBin := filepath.Join(root, "bin")
	for _, dir := range []string{workdir, stagingDir, fakeBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldFile := filepath.Join(workdir, "old.txt")
	if err := os.WriteFile(oldFile, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagingDir, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "cp"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("bash", "-c", agentSandboxMountReplaceCommand(stagingDir, workdir))
	command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("replace unexpectedly succeeded: %s", output)
	}
	got, err := os.ReadFile(oldFile)
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(got) != "old\n" {
		t.Fatalf("restored file=%q", got)
	}
	matches, err := filepath.Glob(filepath.Join(workdir, "*.previous"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("rollback directories remain: %v", matches)
	}
}

func TestAgentSandboxMountReplaceCommandPreservesUntouchedEntriesAfterMoveFailure(t *testing.T) {
	root := t.TempDir()
	workdir := filepath.Join(root, "workspace")
	stagingDir := filepath.Join(workdir, ".crabbox-sync-fixed")
	fakeBin := filepath.Join(root, "bin")
	for _, dir := range []string{workdir, stagingDir, fakeBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"a-moved.txt", "z-blocked.txt"} {
		if err := os.WriteFile(filepath.Join(workdir, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mvPath := filepath.Join(fakeBin, "mv")
	mvScript := `#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    *z-blocked.txt) exit 1 ;;
  esac
done
exec /bin/mv "$@"
`
	if err := os.WriteFile(mvPath, []byte(mvScript), 0o755); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("bash", "-c", agentSandboxMountReplaceCommand(stagingDir, workdir))
	command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("replace unexpectedly succeeded: %s", output)
	}
	for _, name := range []string{"a-moved.txt", "z-blocked.txt"} {
		data, err := os.ReadFile(filepath.Join(workdir, name))
		if err != nil || string(data) != name+"\n" {
			t.Fatalf("%s=%q err=%v", name, data, err)
		}
	}
}
