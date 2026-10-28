//go:build windows

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsFileSnapshotManagedTransferPath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "Evidence.bin")
	if err := os.WriteFile(path, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := normalizeManagedTransferPath(filepath.Join(root, "evidence.bin"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(got, want) || filepath.Base(got) != "Evidence.bin" {
		t.Fatalf("normalized path=%q want actual spelling %q", got, want)
	}
	data, err := os.ReadFile(got)
	if err != nil || string(data) != "evidence" {
		t.Fatalf("normalized path changed evidence: data=%q err=%v", data, err)
	}
}

func TestWindowsFileSnapshotHandleMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "original.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := fileSnapshotPathInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(handle), path)
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := os.Rename(path, path+".retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	opened, err := fileSnapshotHandleInfo(file)
	if err != nil {
		t.Fatal(err)
	}
	if opened.IsDir() || opened.Size() != int64(len("original")) || !sameFileSnapshotIdentity(original, opened) {
		t.Fatal("snapshot must read both metadata and identity from the retained handle")
	}
	replacement, err := fileSnapshotPathInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	if !replacement.IsDir() || sameFileSnapshotIdentity(opened, replacement) {
		t.Fatal("replacement directory must have a distinct identity")
	}
}

func TestWindowsFileSnapshotDoesNotFollowJunction(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "junction")
	if output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("create junction: %v: %s", err, output)
	}
	t.Cleanup(func() {
		if err := os.Remove(link); err != nil {
			t.Error(err)
		}
	})
	info, err := fileSnapshotPathInfo(link)
	if err != nil {
		t.Fatal(err)
	}
	targetInfo, err := fileSnapshotPathInfo(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) == 0 || sameFileSnapshotIdentity(info, targetInfo) {
		t.Fatal("junction snapshot must describe the reparse point, not its target")
	}
}
