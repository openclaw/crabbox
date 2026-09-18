package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/openclaw/crabbox/internal/runner"
	"github.com/openclaw/crabbox/internal/runtimeartifact"
)

func TestWindowsFilesystemNativeArchive(t *testing.T) {
	data := []byte("benign bootstrap fixture")
	file, size, err := windowsFilesystemArchive(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	defer os.Remove(file.Name())
	reader := tar.NewReader(file)
	header, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != "crabbox.exe" || header.Size != int64(len(data)) {
		t.Fatalf("header: %+v", header)
	}
	got, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("payload %q: %v", got, err)
	}
	if _, err := reader.Next(); err != io.EOF {
		t.Fatalf("archive completion: %v", err)
	}
	info, err := file.Stat()
	if err != nil || info.Size() != size {
		t.Fatalf("spool size: %v %d", err, size)
	}
}

func TestWindowsFilesystemNativeScripts(t *testing.T) {
	target, home, err := parseWindowsFilesystemProbe("X64\r\nC:\\Users\\Alice Smith\\AppData\\Local\\Temp\\\r\n")
	if err != nil || target != (runtimeartifact.Target{OS: "windows", Arch: "amd64"}) || !strings.HasPrefix(home, `C:\Users\Alice Smith`) {
		t.Fatalf("probe: %+v %q %v", target, home, err)
	}
	r := &remoteNativeRuntime{nonce: "0123456789abcdef", path: home + `crabbox-runtime-0123456789abcdef\crabbox.exe`, identity: runtimeartifact.Identity{Size: 23, SHA256: strings.Repeat("a", 64)}}
	install := windowsFilesystemInstallScript(r)
	for _, fragment := range []string{"& tar.exe -xf - -C $stage crabbox.exe", "[Int64]23", "$hash.ComputeHash($file)", ".nonce", "$file.Flush($true)"} {
		if !strings.Contains(install, fragment) {
			t.Errorf("missing installer contract %q", fragment)
		}
	}
	cleanup := windowsFilesystemRemoveScript(r)
	for _, fragment := range []string{"ReparsePoint", "[IO.File]::ReadAllText($witness) -cne", psQuote(r.nonce), "Remove-Item -LiteralPath $stage -Recurse -Force"} {
		if !strings.Contains(cleanup, fragment) {
			t.Errorf("missing cleanup contract %q", fragment)
		}
	}
	encodedBytes := base64.StdEncoding.EncodedLen(71)
	invoke := windowsFilesystemInvokeCommand(r, int64(encodedBytes))
	if !strings.Contains(invoke, "& $path "+runner.Command+" serve-base64 --input-bytes "+strconv.Itoa(encodedBytes)) {
		t.Fatalf("invocation: %s", invoke)
	}
	if !strings.Contains(invoke, psQuote(r.path)) || !strings.Contains(invoke, psQuote(r.identity.SHA256)) {
		t.Fatal("invocation lost installed artifact binding")
	}
}
