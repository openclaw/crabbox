//go:build windows

package cli

import (
	"bytes"
	"io"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestReplayableSSHInputUsesPrivatePathBackedRegularFileAndResets(t *testing.T) {
	want := []byte{'a', 0, 'b', '\n'}
	input, err := newReplayableSSHInput(want)
	if err != nil {
		t.Fatal(err)
	}
	path := input.path
	t.Cleanup(func() {
		if err := input.close(); err != nil {
			t.Errorf("close input: %v", err)
		}
	})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("private input spool is not path-backed at %s: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("input mode=%v, want regular file", info.Mode())
	}
	user, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPrivateWindowsHandle(windows.Handle(input.reader.Fd()), false, user); err != nil {
		t.Fatalf("input spool is not current-user private: %v", err)
	}
	if _, err := input.reader.Write([]byte("x")); err == nil {
		t.Fatal("input spool reader is writable, want read-only handle")
	}

	first, err := input.reset()
	if err != nil {
		t.Fatal(err)
	}
	file, ok := first.(*os.File)
	if !ok {
		t.Fatalf("reader=%T, want *os.File", first)
	}
	prefix := make([]byte, 2)
	if _, err := io.ReadFull(file, prefix); err != nil {
		t.Fatal(err)
	}

	second, err := input.reset()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("replayed input=%v, want %v", got, want)
	}

	if err := input.close(); err != nil {
		t.Fatal(err)
	}
	if err := input.close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("input spool remains after close at %s: %v", path, err)
	}
}
