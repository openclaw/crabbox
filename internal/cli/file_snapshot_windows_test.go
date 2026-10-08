//go:build windows

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

func windowsSnapshotTestRoot(t *testing.T) string {
	t.Helper()
	if parent := os.Getenv("CRABBOX_TEST_FILESYSTEM_ROOT"); parent != "" {
		root, err := os.MkdirTemp(parent, "identity-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(root); err != nil {
				t.Error(err)
			}
		})
		return root
	}
	return t.TempDir()
}

func windowsSnapshotStateRoot(t *testing.T, base string, total int) string {
	t.Helper()
	const suffix = `\crabbox\claims\cbx_identity.json`
	left := total - len(utf16.Encode([]rune(base))) - len(suffix)
	if left < 2 {
		t.Fatalf("fixture base is too long for the requested %d-character path", total)
	}
	directory := base
	for left > 0 {
		n := min(80, left-1)
		if left-n-1 == 1 {
			n--
		}
		directory = filepath.Join(directory, strings.Repeat("p", n))
		left -= n + 1
	}
	return directory
}

func TestWindowsFileSnapshotIdentity(t *testing.T) {
	base := windowsSnapshotTestRoot(t)
	for _, length := range []int{220, 247, 248, 259, 260, 261, 320, 400} {
		state := windowsSnapshotStateRoot(t, base, length)
		t.Setenv("XDG_STATE_HOME", state)
		writeClaimsListFixture(t, "cbx_identity.json", leaseClaim{
			LeaseID: "cbx_identity", Provider: "hyperv", RepoRoot: base,
			Labels: map[string]string{"target": targetLinux},
		})
		path := filepath.Join(state, "crabbox", "claims", "cbx_identity.json")
		if got := len(utf16.Encode([]rune(path))); got != length {
			t.Fatalf("fixture path length=%d want=%d", got, length)
		}
		reader := func(path, id string, expected os.FileInfo) (leaseClaim, bool, error) {
			snapshot, ok := expected.(windowsSnapshotInfo)
			if !ok {
				t.Fatal("claim scan did not capture the native full-file-ID route")
			}
			file, err := openArtifactReadOnly(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			var identity windowsFullFileID
			if err := windows.GetFileInformationByHandleEx(windows.Handle(file.Fd()), windows.FileIdInfo,
				(*byte)(unsafe.Pointer(&identity)), uint32(unsafe.Sizeof(identity))); err != nil {
				t.Fatal(err)
			}
			if snapshot.identity != identity {
				t.Fatal("scan identity is not the full native volume/file identity")
			}
			var filesystem [32]uint16
			if err := windows.GetVolumeInformationByHandle(windows.Handle(file.Fd()), nil, 0, nil, nil, nil,
				&filesystem[0], uint32(len(filesystem))); err != nil {
				t.Fatal(err)
			}
			format := windows.UTF16ToString(filesystem[:])
			if os.Getenv("CRABBOX_TEST_REQUIRE_REFS") == "1" && format != "ReFS" {
				t.Fatalf("required ReFS coverage ran on %s instead", format)
			}
			t.Logf("native FileIdInfo: filesystem=%s characters=%d", format, length)
			// Keep another read handle open while the real reader verifies
			// identity; a stable concurrent reader is not a replacement.
			return readLeaseClaimSnapshotWithPresence(path, id, expected)
		}
		snapshot, err := snapshotLeaseClaimsReadOnlyWithReader(reader)
		if err != nil || len(snapshot.claims) != 1 || len(snapshot.invalid) != 0 {
			t.Fatalf("native snapshot failed: claims=%d problems=%v err=%v", len(snapshot.claims), snapshot.invalid, err)
		}
		stdout, _, err := runClaimsList(t, "--json")
		if err != nil {
			t.Fatal(err)
		}
		var output localClaimsListOutput
		if err := json.Unmarshal([]byte(stdout), &output); err != nil {
			t.Fatal(err)
		}
		if len(output.Claims) != 1 || len(output.Problems) != 0 || output.Claims[0].TargetOS != targetLinux || output.Claims[0].WindowsMode != "" {
			t.Fatal("long-path native identity did not preserve the public admission contract")
		}
	}
}

func TestWindowsFileSnapshotRejectsReplacedIdentity(t *testing.T) {
	base := windowsSnapshotTestRoot(t)
	t.Setenv("XDG_STATE_HOME", base)
	writeClaimsListFixture(t, "cbx_identity.json", leaseClaim{LeaseID: "cbx_identity", Provider: "hyperv"})
	reader := func(path, id string, expected os.FileInfo) (leaseClaim, bool, error) {
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		backup := path + ".old"
		if err := os.Rename(path, backup); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(backup)
		if err := os.WriteFile(path, original, expected.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, expected.ModTime(), expected.ModTime()); err != nil {
			t.Fatal(err)
		}
		return readLeaseClaimSnapshotWithPresence(path, id, expected)
	}
	snapshot, err := snapshotLeaseClaimsReadOnlyWithReader(reader)
	var problem *leaseClaimFileError
	if err != nil || len(snapshot.claims) != 0 || !errors.As(snapshot.invalid["cbx_identity"], &problem) || problem.code != "read_error" {
		t.Fatalf("same-size/time replacement was accepted: %+v err=%v", snapshot, err)
	}
}

func TestWindowsFileSnapshotManagedTransferPath(t *testing.T) {
	root := windowsSnapshotTestRoot(t)
	path := filepath.Join(root, "Evidence.bin")
	if err := os.WriteFile(path, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := normalizeManagedTransferPath(filepath.Join(root, "evidence.bin"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(got, path) || filepath.Base(got) != "Evidence.bin" {
		t.Fatalf("normalized path=%q want actual spelling %q", got, path)
	}
	data, err := os.ReadFile(got)
	if err != nil || string(data) != "evidence" {
		t.Fatalf("normalized path changed evidence: data=%q err=%v", data, err)
	}
}

func TestWindowsFileSnapshotRejectsSymlinkReplacement(t *testing.T) {
	base := windowsSnapshotTestRoot(t)
	t.Setenv("XDG_STATE_HOME", base)
	writeClaimsListFixture(t, "cbx_identity.json", leaseClaim{LeaseID: "cbx_identity", Provider: "hyperv"})
	reader := func(path, id string, expected os.FileInfo) (leaseClaim, bool, error) {
		original := path + ".original"
		if err := os.Rename(path, original); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(original)
		if err := os.Symlink(original, path); err != nil {
			t.Fatalf("native identity lane requires symlink fixture support: %v", err)
		}
		return readLeaseClaimSnapshotWithPresence(path, id, expected)
	}
	snapshot, err := snapshotLeaseClaimsReadOnlyWithReader(reader)
	if err != nil || len(snapshot.claims) != 0 || len(snapshot.invalid) != 1 {
		t.Fatalf("symlink replacement was accepted: %+v err=%v", snapshot, err)
	}
}
