package runnerfs

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestArchiveUploadPreservesPermissions(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX permissions")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "run"), []byte("proof"), 0o751); err != nil {
		t.Fatal(err)
	}
	_, archive, err := CreateArchive(t.Context(), source, CreateOptions{}, DefaultArchiveLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	stage := t.TempDir()
	if err := ExtractArchive(t.Context(), archive, stage, ArchivePayloadRoot, ExtractOptions{PreservePermissions: true}, DefaultArchiveLimits()); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{"payload": 0o750, "payload/run": 0o751} {
		info, err := os.Stat(filepath.Join(stage, path))
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s: info=%v err=%v", path, info, err)
		}
	}
}

func TestCreateArchiveLimitsCompressedBytesDuringWrite(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	data := make([]byte, 128<<10)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, data, 0o600); err != nil {
		t.Fatal(err)
	}
	limits := DefaultArchiveLimits()
	limits.MaxCompressedBytes = 1024
	_, archive, err := CreateArchive(t.Context(), source, CreateOptions{}, limits)
	if archive != nil || err == nil || !strings.Contains(err.Error(), "compressed size limit") {
		t.Fatalf("archive=%v err=%v", archive, err)
	}
}

func TestArchiveSourceHardLinkPolicy(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX archive source")
	}
	root := t.TempDir()
	source := filepath.Join(root, "file")
	if err := os.WriteFile(source, []byte("proof"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	_, archive, err := CreateArchive(t.Context(), source, CreateOptions{RejectHardLinks: true}, DefaultArchiveLimits())
	if archive != nil || err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Fatalf("archive=%v err=%v", archive, err)
	}
	_, archive, err = CreateArchive(t.Context(), source, CreateOptions{}, DefaultArchiveLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
}

func TestRecoverLegacyRemoteArchiveTransaction(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX transaction ownership")
	}
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprintf("active=%v", active), func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "target")
			backup, marker := target+".crabbox-cp-backup", target+".crabbox-cp-transaction"
			state := "2147483646\ndead-process\n"
			if active {
				identity, ok := copyArchiveProcessIdentity(os.Getpid())
				if !ok {
					t.Fatal("identity unavailable")
				}
				state = fmt.Sprintf("%d\n %s\n", os.Getpid(), identity)
			}
			if err := os.WriteFile(backup, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(marker, []byte(state), 0o600); err != nil {
				t.Fatal(err)
			}
			err := recoverCopyArchiveTransaction(target, backup, marker, 0)
			if active {
				if err == nil || !strings.Contains(err.Error(), "another archive copy is active") {
					t.Fatalf("err=%v", err)
				}
				data, err := os.ReadFile(backup)
				if err != nil || !bytes.Equal(data, []byte("original")) {
					t.Fatalf("backup=%q err=%v", data, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(target)
				if err != nil || !bytes.Equal(data, []byte("original")) {
					t.Fatalf("target=%q err=%v", data, err)
				}
			}
		})
	}
}

func TestArchiveTargetRejectsUntrustedBase(t *testing.T) {
	for _, base := range []string{"", ".", "..", "../outside", "a/b", "a\x00b"} {
		if _, err := ArchiveTarget(t.TempDir(), base, false); err == nil {
			t.Fatalf("accepted %q", base)
		}
	}
	if runtime.GOOS == "windows" {
		if _, err := ArchiveTarget(t.TempDir(), `..\outside`, false); err == nil {
			t.Fatal("Windows separator accepted in source basename")
		}
	}
}
