//go:build !windows

package cli

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

const privateRunOutputUmaskTest = "CRABBOX_PRIVATE_RUN_OUTPUT_UMASK_TEST"

func TestPrivateRunOutputPermissionsUnderPermissiveUmask(t *testing.T) {
	if os.Getenv(privateRunOutputUmaskTest) == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPrivateRunOutputPermissionsUnderPermissiveUmask$")
		cmd.Env = append(os.Environ(), privateRunOutputUmaskTest+"=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("permission test subprocess failed: %v\n%s", err, output)
		}
		return
	}

	oldUmask := unix.Umask(0)
	defer unix.Umask(oldUmask)

	root := t.TempDir()
	downloadPath := filepath.Join(root, "downloads", "report.txt")
	if err := writeRunDownloadFile(downloadPath, []byte("download")); err != nil {
		t.Fatal(err)
	}
	assertRunOutputMode(t, filepath.Dir(downloadPath), privateRunOutputDirMode)
	assertRunOutputMode(t, downloadPath, privateRunOutputFileMode)

	delegatedPath := filepath.Join(root, "delegated", "report.txt")
	backend := &fakeDelegatedRunDownloadBackend{
		files:   map[string][]byte{"reports/report.txt": []byte("delegated")},
		fetches: map[string]int{},
	}
	if _, err := MaterializeDelegatedRunDownloads(t.Context(), backend, RunRequest{
		Downloads: []string{"reports/report.txt=" + delegatedPath},
	}, "lease-permissions", io.Discard); err != nil {
		t.Fatal(err)
	}
	assertRunOutputMode(t, filepath.Dir(delegatedPath), privateRunOutputDirMode)
	assertRunOutputMode(t, delegatedPath, privateRunOutputFileMode)

	readOnlyPath := filepath.Join(root, "read-only.txt")
	if err := os.WriteFile(readOnlyPath, []byte("old"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := preflightRunLocalOutputs(readOnlyPath, "", nil); err != nil {
		t.Fatalf("replacement preflight should not require old inode writability: %v", err)
	}

	symlinkTargetDir := filepath.Join(root, "symlink-target-dir")
	if err := os.Mkdir(symlinkTargetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	preflightSymlink := filepath.Join(root, "preflight-symlink")
	if err := os.Symlink(symlinkTargetDir, preflightSymlink); err != nil {
		t.Fatal(err)
	}
	if err := preflightRunLocalOutputs(preflightSymlink, "", nil); err != nil {
		t.Fatalf("replacement preflight should inspect the symlink entry, not its target: %v", err)
	}

	if os.Geteuid() != 0 {
		lockedDir := filepath.Join(root, "locked")
		if err := os.Mkdir(lockedDir, 0o700); err != nil {
			t.Fatal(err)
		}
		lockedPath := filepath.Join(lockedDir, "existing.txt")
		if err := os.WriteFile(lockedPath, []byte("existing"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(lockedDir, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(lockedDir, 0o700)
		if err := preflightRunLocalOutputs(lockedPath, "", nil); err == nil {
			t.Fatal("preflight should reject an existing output in a non-writable directory")
		}
	}

	capturePath := filepath.Join(root, "stdout.log")
	if err := os.WriteFile(capturePath, []byte("old"), 0o666); err != nil {
		t.Fatal(err)
	}
	oldCapture, err := os.Open(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	defer oldCapture.Close()
	capture := &failureStreamCapture{label: "stdout", explicitPath: capturePath}
	captureWriter, _, err := capture.writer(io.Discard, &phaseMarkerWriter{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureWriter.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := capture.closeQuiet(); err != nil {
		t.Fatal(err)
	}
	assertRunOutputMode(t, capturePath, privateRunOutputFileMode)
	if got, err := io.ReadAll(oldCapture); err != nil || !bytes.Equal(got, []byte("old")) {
		t.Fatalf("old capture inode data=%q err=%v", got, err)
	}

	victimPath := filepath.Join(root, "victim.txt")
	if err := os.WriteFile(victimPath, []byte("victim"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(root, "symlink-output")
	if err := os.Symlink(victimPath, symlinkPath); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateRunOutputFile(symlinkPath, []byte("output")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(victimPath); err != nil || !bytes.Equal(got, []byte("victim")) {
		t.Fatalf("symlink victim data=%q err=%v", got, err)
	}
	if info, err := os.Lstat(symlinkPath); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("output path should replace symlink with a regular file: info=%v err=%v", info, err)
	}
	assertRunOutputMode(t, symlinkPath, privateRunOutputFileMode)

	proofPath := filepath.Join(root, "proofs", "proof.md")
	if _, err := writeRunProof(proofPath, "", proofRenderInput{
		Provider: "aws",
		LeaseID:  "cbx_permissions",
		Command:  "true",
	}); err != nil {
		t.Fatal(err)
	}
	assertRunOutputMode(t, filepath.Dir(proofPath), privateRunOutputDirMode)
	assertRunOutputMode(t, proofPath, privateRunOutputFileMode)

	t.Chdir(root)
	if err := os.Mkdir(".crabbox", 0o700); err != nil {
		t.Fatal(err)
	}
	bundleTarget := filepath.Join(root, "bundle-target")
	if err := os.Mkdir(bundleTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bundleTarget, filepath.Join(".crabbox", "captures")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writeLocalFailureBundle("rejected.tar.gz", "", FailureCaptureMetadata{}); err == nil {
		t.Fatal("failure bundle directory symlink should be rejected")
	}
	assertRunOutputMode(t, bundleTarget, 0o755)
	if err := os.Remove(filepath.Join(".crabbox", "captures")); err != nil {
		t.Fatal(err)
	}

	bundlePath, _, err := writeLocalFailureBundle("failure.tar.gz", "", FailureCaptureMetadata{
		Provider: "aws",
		LeaseID:  "cbx_permissions",
		RunID:    "run_permissions",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRunOutputMode(t, filepath.Dir(bundlePath), privateRunOutputDirMode)
	assertRunOutputMode(t, bundlePath, privateRunOutputFileMode)
}

func TestStickyRunOutputReplaceAllowed(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		euid, fileUID, dirUID uint32
		sticky                bool
		want                  bool
	}{
		{name: "non-sticky", euid: 1000, fileUID: 2000, dirUID: 3000, want: true},
		{name: "root", euid: 0, fileUID: 2000, dirUID: 3000, sticky: true, want: true},
		{name: "file owner", euid: 1000, fileUID: 1000, dirUID: 3000, sticky: true, want: true},
		{name: "directory owner", euid: 1000, fileUID: 2000, dirUID: 1000, sticky: true, want: true},
		{name: "foreign entry", euid: 1000, fileUID: 2000, dirUID: 3000, sticky: true, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stickyRunOutputReplaceAllowed(tc.euid, tc.fileUID, tc.dirUID, tc.sticky); got != tc.want {
				t.Fatalf("replace allowed=%t want=%t", got, tc.want)
			}
		})
	}
}

func assertRunOutputMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode=%#o want=%#o", path, got, want)
	}
}
