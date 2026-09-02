package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestRemoteInvalidateSyncFingerprintWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	for _, tc := range []struct {
		name    string
		wantErr bool
	}{
		{name: "missing workspace"},
		{name: "existing fingerprint"},
		{name: "missing fingerprint"},
		{name: "regular file workspace", wantErr: true},
		{name: "inaccessible ancestor", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "lease")
			workdir := filepath.Join(parent, "repo")
			fingerprint := filepath.Join(workdir, ".crabbox", "sync-fingerprint")
			retained := filepath.Join(workdir, ".crabbox", "keep")
			switch tc.name {
			case "missing workspace":
			case "regular file workspace":
				mustWriteTestFile(t, workdir, "file target")
			default:
				mustWriteTestFile(t, retained, "keep")
				if tc.name != "missing fingerprint" {
					mustWriteTestFile(t, fingerprint, "reusable")
				}
			}
			if tc.name == "inaccessible ancestor" {
				if os.Geteuid() == 0 {
					t.Skip("root can traverse the permission-denied fixture")
				}
				if err := os.Chmod(parent, 0); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := os.Chmod(parent, 0o700); err != nil {
						t.Error(err)
					}
				}()
				if _, err := os.Stat(workdir); !os.IsPermission(err) {
					t.Fatalf("fixture must deny traversal: %v", err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := remoteInvalidateSyncFingerprintForTarget(SSHTarget{TargetOS: targetLinux}, workdir)
			cmd := exec.CommandContext(ctx, "bash", "-lc", command)
			cmd.WaitDelay = time.Second
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("invalidation did not finish: %v", ctx.Err())
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("invalidation error=%v, want error=%t (%d diagnostic bytes)", err, tc.wantErr, len(out))
			}
			if tc.wantErr {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() <= 0 || len(out) == 0 {
					t.Errorf("invalid target must report a command failure: %v (%d diagnostic bytes)", err, len(out))
				}
			}
			if tc.name == "inaccessible ancestor" {
				if err := os.Chmod(parent, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.name {
			case "missing workspace":
				if _, err := os.Lstat(parent); !os.IsNotExist(err) {
					t.Fatalf("invalidation created the missing workspace: %v", err)
				}
			case "regular file workspace":
				if data, err := os.ReadFile(workdir); err != nil || string(data) != "file target" {
					t.Fatalf("invalidation changed the file target: %v", err)
				}
			default:
				if tc.wantErr {
					if data, err := os.ReadFile(fingerprint); err != nil || string(data) != "reusable" {
						t.Fatalf("failed invalidation changed the fingerprint: %v", err)
					}
				} else if _, err := os.Lstat(fingerprint); !os.IsNotExist(err) {
					t.Fatalf("fingerprint remains after invalidation: %v", err)
				}
				if data, err := os.ReadFile(retained); err != nil || string(data) != "keep" {
					t.Fatalf("invalidation changed unrelated metadata: %v", err)
				}
			}
		})
	}
}

func TestRemoteInvalidateSyncFingerprintAncestors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	for _, tc := range []struct {
		name    string
		wantErr bool
	}{
		{name: "missing nested path with trailing slashes"},
		{name: "relative missing path with shell characters"},
		{name: "regular file ancestor", wantErr: true},
		{name: "regular file with trailing slashes", wantErr: true},
		{name: "dangling symlink ancestor", wantErr: true},
		{name: "inaccessible symlink ancestor", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "parent")
			workdir := filepath.Join(parent, "nested", "repo")
			retained := map[string]string{filepath.Join(root, "keep"): "keep"}
			missing, linkTarget, denied := "", "", ""
			switch tc.name {
			case "missing nested path with trailing slashes":
				workdir += "///"
				missing = parent
			case "relative missing path with shell characters":
				workdir = "-missing ' $%/repo"
				missing = filepath.Join(root, "-missing ' $%")
			case "regular file ancestor":
				retained[parent] = "file target"
			case "regular file with trailing slashes":
				retained[parent] = "file target"
				workdir = parent + "///"
			case "dangling symlink ancestor":
				linkTarget = filepath.Join(root, "absent")
				missing = linkTarget
			case "inaccessible symlink ancestor":
				if os.Geteuid() == 0 {
					t.Skip("root can traverse the permission-denied fixture")
				}
				linkTarget = filepath.Join(root, "hidden")
				retained[filepath.Join(linkTarget, "keep")] = "keep"
				denied = linkTarget
			}
			for path, value := range retained {
				mustWriteTestFile(t, path, value)
			}
			if linkTarget != "" {
				if err := os.Symlink(linkTarget, parent); err != nil {
					t.Fatal(err)
				}
			}
			if denied != "" {
				if err := os.Chmod(denied, 0); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := os.Chmod(denied, 0o755); err != nil {
						t.Error(err)
					}
				}()
				if _, err := os.Stat(workdir); !os.IsPermission(err) {
					t.Fatalf("fixture must deny traversal: %v", err)
				}
			}
			output, err := runFingerprintInvalidationForTest(t, root, workdir)
			if (err != nil) != tc.wantErr {
				t.Errorf("invalidation error=%v, want error=%t (%d diagnostic bytes)", err, tc.wantErr, len(output))
			}
			if tc.wantErr {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() <= 0 || len(output) == 0 {
					t.Errorf("invalid target must report a command failure: %v (%d diagnostic bytes)", err, len(output))
				}
			}
			if denied != "" {
				if err := os.Chmod(denied, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for path, value := range retained {
				if data, err := os.ReadFile(path); err != nil || string(data) != value {
					t.Errorf("invalidation changed retained fixture %s: %v", filepath.Base(path), err)
				}
			}
			if missing != "" {
				if _, err := os.Lstat(missing); !os.IsNotExist(err) {
					t.Errorf("invalidation created missing ancestor: %v", err)
				}
			}
			if linkTarget != "" {
				if got, err := os.Readlink(parent); err != nil || got != linkTarget {
					t.Errorf("invalidation changed the symlink: %v", err)
				}
			}
		})
	}
}

func TestRemoteInvalidateSyncFingerprintGitMetadata(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	fixture := newGitCoherenceFixture(t)
	for _, kind := range []string{"git root", "linked worktree", "nested non-root"} {
		t.Run(kind, func(t *testing.T) {
			var workdir string
			if kind == "linked worktree" {
				workdir = fixture.linkedWorkspace(t, fixture.b)
			} else {
				workdir = fixture.workspace(t, fixture.b, true)
			}
			fingerprint := filepath.Join(coherenceMetaDir(t, workdir), "sync-fingerprint")
			retained := filepath.Join(workdir, ".crabbox", "sync-fingerprint")
			if kind == "nested non-root" {
				retained = fingerprint
				workdir = filepath.Join(workdir, "nested")
				fingerprint = filepath.Join(workdir, ".crabbox", "sync-fingerprint")
			}
			mustWriteTestFile(t, fingerprint, "reusable")
			mustWriteTestFile(t, retained, "keep")
			if output, err := runFingerprintInvalidationForTest(t, "", workdir); err != nil {
				t.Fatalf("invalidate Git metadata: %v (%d diagnostic bytes)", err, len(output))
			}
			if _, err := os.Lstat(fingerprint); !os.IsNotExist(err) {
				t.Errorf("selected fingerprint remains: %v", err)
			}
			if data, err := os.ReadFile(retained); err != nil || string(data) != "keep" {
				t.Errorf("invalidation changed another metadata namespace: %v", err)
			}
		})
	}
}

func TestRemoteInvalidateSyncFingerprintCDPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	for _, tc := range []struct {
		name    string
		workdir string
		wantErr bool
	}{
		{name: "existing workspace", workdir: "repo"},
		{name: "regular file ancestor", workdir: "parent/nested/repo", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			decoyRoot := filepath.Join(root, "decoy")
			fingerprint := filepath.Join(root, tc.workdir, ".crabbox", "sync-fingerprint")
			decoyFingerprint := filepath.Join(decoyRoot, tc.workdir, ".crabbox", "sync-fingerprint")
			fileParent := filepath.Join(root, "parent")
			if tc.wantErr {
				mustWriteTestFile(t, fileParent, "file target")
			} else {
				mustWriteTestFile(t, fingerprint, "reusable")
			}
			mustWriteTestFile(t, decoyFingerprint, "decoy")
			output, err := runFingerprintInvalidationForTest(t, root, tc.workdir, "CDPATH="+decoyRoot)
			if (err != nil) != tc.wantErr {
				t.Errorf("invalidation error=%v, want error=%t (%d diagnostic bytes)", err, tc.wantErr, len(output))
			}
			if tc.wantErr {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() <= 0 || len(output) == 0 {
					t.Errorf("invalid target must report a command failure: %v (%d diagnostic bytes)", err, len(output))
				}
				if data, err := os.ReadFile(fileParent); err != nil || string(data) != "file target" {
					t.Errorf("invalidation changed the file ancestor: %v", err)
				}
			} else if _, err := os.Lstat(fingerprint); !os.IsNotExist(err) {
				t.Errorf("selected fingerprint remains: %v", err)
			}
			if data, err := os.ReadFile(decoyFingerprint); err != nil || string(data) != "decoy" {
				t.Errorf("invalidation changed the CDPATH metadata namespace: %v", err)
			}
		})
	}
}

func TestRemoteInvalidateSyncFingerprintOldpwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	for _, tc := range []struct {
		name    string
		workdir string
		wantErr bool
	}{
		{name: "existing dash workspace", workdir: "-"},
		{name: "dangling dash ancestor", workdir: "-/repo", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dash := filepath.Join(root, "-")
			missing := filepath.Join(root, "missing")
			decoy := filepath.Join(root, "decoy")
			fingerprint := filepath.Join(dash, ".crabbox", "sync-fingerprint")
			decoyFingerprint := filepath.Join(decoy, ".crabbox", "sync-fingerprint")
			if tc.wantErr {
				if err := os.Symlink(missing, dash); err != nil {
					t.Fatal(err)
				}
			} else {
				mustWriteTestFile(t, fingerprint, "reusable")
			}
			mustWriteTestFile(t, decoyFingerprint, "decoy")
			output, err := runFingerprintInvalidationForTest(t, root, tc.workdir, "OLDPWD="+decoy)
			if (err != nil) != tc.wantErr {
				t.Errorf("invalidation error=%v, want error=%t (%d diagnostic bytes)", err, tc.wantErr, len(output))
			}
			if tc.wantErr {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() <= 0 || len(output) == 0 {
					t.Errorf("invalid target must report a command failure: %v (%d diagnostic bytes)", err, len(output))
				}
				if got, err := os.Readlink(dash); err != nil || got != missing {
					t.Errorf("invalidation changed the dash symlink: %v", err)
				}
				if _, err := os.Lstat(missing); !os.IsNotExist(err) {
					t.Errorf("invalidation created the missing symlink target: %v", err)
				}
			} else if _, err := os.Lstat(fingerprint); !os.IsNotExist(err) {
				t.Errorf("selected fingerprint remains: %v", err)
			}
			if data, err := os.ReadFile(decoyFingerprint); err != nil || string(data) != "decoy" {
				t.Errorf("invalidation changed the OLDPWD metadata namespace: %v", err)
			}
		})
	}
}

func runFingerprintInvalidationForTest(t *testing.T, cwd, workdir string, env ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := remoteInvalidateSyncFingerprintForTarget(SSHTarget{TargetOS: targetLinux}, workdir)
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = cwd
	cmd.Env = append(cmd.Environ(), env...)
	cmd.WaitDelay = time.Second
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("invalidation did not finish: %v", ctx.Err())
	}
	return output, err
}
