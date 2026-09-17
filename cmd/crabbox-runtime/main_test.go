package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/openclaw/crabbox/internal/remoteruntime"
	"github.com/openclaw/crabbox/internal/runner"
)

func TestIdentity(t *testing.T) {
	output, err := os.CreateTemp(t.TempDir(), "identity")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	if code := runWithIO(t.Context(), []string{remoteruntime.Command, "identity"}, os.Stdin, output, os.Stderr); code != 0 {
		t.Fatalf("identity exit code = %d", code)
	}
	if _, err := output.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var got remoteruntime.Identity
	if err := json.NewDecoder(output).Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := remoteruntime.Identity{Protocol: remoteruntime.Protocol, OS: runtime.GOOS, Arch: runtime.GOARCH, Runnable: runtime.GOOS == "linux"}
	if got != want {
		t.Fatalf("identity = %+v, want %+v", got, want)
	}
}

func TestRequiresCommandMarker(t *testing.T) {
	for _, args := range [][]string{nil, {"identity"}} {
		if code := run(args); code != 74 {
			t.Fatalf("run(%q) = %d, want 74", args, code)
		}
	}
}

func TestFilesystemDispatchCollectsReport(t *testing.T) {
	dir := t.TempDir()
	want := []byte("<?xml version=\"1.0\"?><testsuite name=\"owned\" tests=\"0\"/>\n")
	if err := os.WriteFile(filepath.Join(dir, "report.xml"), want, 0600); err != nil {
		t.Fatal(err)
	}
	open := func(name string) *os.File {
		t.Helper()
		f, err := os.Create(filepath.Join(t.TempDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	input, output, diagnostic := open("input"), open("output"), open("diagnostic")
	identity := runner.CurrentIdentity()
	if err := runner.WriteRequest(input, runner.Request{BuildID: identity.BuildID, Operation: runner.Collect, Workdir: dir, Paths: []string{"report.xml"}}, 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if code := runWithIO(t.Context(), []string{runner.Command, "serve"}, input, output, diagnostic); code != 0 {
		t.Fatalf("filesystem dispatch exit=%d", code)
	}
	response, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	_, err = runner.ReadResponse(t.Context(), bytes.NewReader(response), identity, runner.Collect, func(info runner.FileInfo, contents io.Reader) error {
		count++
		got, err := io.ReadAll(contents)
		if err == nil && (info.Path != "report.xml" || !bytes.Equal(got, want)) {
			t.Fatalf("report changed: path=%q bytes=%q", info.Path, got)
		}
		return err
	})
	if err != nil || count != 1 {
		t.Fatalf("filesystem response: files=%d error=%v", count, err)
	}
}
