package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestProbeUsesActualRuntime(t *testing.T) {
	for _, test := range []struct{ value, os, arch string }{
		{"Linux\nx86_64\n/home/alice\n", "linux", "amd64"},
		{"Darwin\narm64\n/Users/alice\n", "darwin", "arm64"},
		{"windows\r\nX64\r\nC:\\Users\\alice\r\n", "windows", "amd64"},
		{"Linux\naarch64\n/home/user\n", "linux", "arm64"},
	} {
		platform, err := ParseProbe(test.value)
		if err != nil || platform.Target != (Target{OS: test.os, Arch: test.arch}) {
			t.Fatalf("%q: platform=%v err=%v", test.value, platform, err)
		}
	}
	for _, value := range []string{"Linux\nmips\n/home/user\n", "Linux\namd64\nrelative\n", "Linux\namd64\n/home/user\nnoise\n", "windows\narm64\nrelative\n"} {
		if _, err := ParseProbe(value); err == nil {
			t.Fatalf("invalid probe accepted: %q", value)
		}
	}
}

func TestBootstrapInstalledRunnerVerifiesBytesBeforeExec(t *testing.T) {
	if testing.Short() || runtime.GOOS == "windows" {
		t.Skip("POSIX standalone bootstrap")
	}
	artifact, err := DevelopmentArtifact(t.Context(), Target{OS: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil {
		t.Fatal(err)
	}
	platform := Runtime{Target: Target{OS: runtime.GOOS, Arch: runtime.GOARCH}, Home: filepath.Join(t.TempDir(), "home with ' quote\\name\\")}
	if err := os.Mkdir(platform.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	prepare, err := PrepareInstallCommand(platform, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(t.Context(), "sh", "-c", prepare).CombinedOutput(); err != nil {
		t.Fatalf("prepare: %v: %s", err, output)
	}
	path, err := artifact.RemotePath(platform)
	if err != nil {
		t.Fatal(err)
	}
	uploaded := filepath.Join(filepath.Dir(path), ".task-owned-upload")
	if err := os.WriteFile(uploaded, artifact.Data, 0o600); err != nil {
		t.Fatal(err)
	}
	install, err := InstallUploadedCommand(platform, artifact, uploaded)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		if output, err := exec.CommandContext(t.Context(), "sh", "-c", install).CombinedOutput(); err != nil {
			t.Fatalf("install: %v: %s", err, output)
		}
	}
	for _, textOnly := range []bool{false, true} {
		command, err := InvokeCommand(platform, artifact, textOnly)
		if err != nil {
			t.Fatal(err)
		}
		transport := Transport(func(ctx context.Context, input io.Reader, output io.Writer) error {
			process := exec.CommandContext(ctx, "sh", "-c", command)
			process.Stdin, process.Stdout = input, output
			return process.Run()
		})
		if textOnly {
			transport = Base64Transport(transport)
		}
		client := Client{Identity: artifact.Identity, Transport: transport}
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "file"), []byte{0, 255, 13, 10, 3}, 0o600); err != nil {
			t.Fatal(err)
		}
		results, err := client.Collect(t.Context(), root, []string{"file"}, false, "")
		if err != nil || len(results.Files) != 1 || !bytes.Equal(results.Files[0].Data, []byte{0, 255, 13, 10, 3}) {
			t.Fatalf("text=%v results=%v err=%v", textOnly, results, err)
		}
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho must-not-run\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command, err := InvokeCommand(platform, artifact, false)
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(t.Context(), "sh", "-c", command).CombinedOutput()
	if err == nil || strings.Contains(string(output), "must-not-run") || !strings.Contains(string(output), "digest mismatch") {
		t.Fatalf("corrupt runner: err=%v output=%q", err, output)
	}
}

func TestBootstrapRejectsArtifactPathInjection(t *testing.T) {
	artifact := Artifact{Identity: testIdentity(), SHA256: strings.Repeat("a", 64)}
	platform := Runtime{Target: Target{OS: runtime.GOOS, Arch: runtime.GOARCH}, Home: "/home/user"}
	for _, digest := range []string{"../escape", "$(id)", "", hex.EncodeToString([]byte{1})} {
		artifact.SHA256 = digest
		if _, err := artifact.RemotePath(platform); err == nil {
			t.Fatalf("accepted %q", digest)
		}
	}
}

func TestWindowsInvocationWithoutHashModule(t *testing.T) {
	shell := "pwsh"
	if runtime.GOOS == "windows" {
		shell = "powershell.exe"
	}
	shell, err := exec.LookPath(shell)
	if err != nil {
		t.Skip("PowerShell unavailable")
	}
	artifact, err := DevelopmentArtifact(t.Context(), Target{OS: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the Windows command renderer with a native executable on either
	// host. The protocol identity remains that executable's actual identity.
	installed := artifact
	installed.Identity.OS = "windows"
	platform := Runtime{Target: Target{OS: "windows", Arch: runtime.GOARCH}, Home: t.TempDir()}
	name, err := installed.RemotePath(platform)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, artifact.Data, 0700); err != nil {
		t.Fatal(err)
	}
	command, err := InvokeCommand(platform, installed, true)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("EXACT"), 0600); err != nil {
		t.Fatal(err)
	}
	var request bytes.Buffer
	if err := WriteRequest(&request, Request{BuildID: artifact.Identity.BuildID, Operation: Collect, Workdir: root, Paths: []string{"file"}}, 0, nil); err != nil {
		t.Fatal(err)
	}
	run := func() ([]byte, []byte, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		child := exec.CommandContext(ctx, shell, "-NoProfile", "-NonInteractive", "-Command", "function Get-FileHash { throw 'hash module unavailable' };\n"+command)
		child.Stdin = strings.NewReader(base64.StdEncoding.EncodeToString(request.Bytes()))
		var output, diagnostic bytes.Buffer
		child.Stdout, child.Stderr = &output, &diagnostic
		err := child.Run()
		return output.Bytes(), diagnostic.Bytes(), err
	}
	output, diagnostic, err := run()
	if err != nil {
		t.Fatalf("invoke: %v: %s", err, diagnostic)
	}
	var received []byte
	_, err = ReadResponse(t.Context(), base64.NewDecoder(base64.StdEncoding, bytes.NewReader(output)), artifact.Identity, Collect, func(_ FileInfo, body io.Reader) error { received, err = io.ReadAll(body); return err })
	if err != nil || string(received) != "EXACT" {
		t.Fatalf("received=%q err=%v diagnostic=%s", received, err, diagnostic)
	}
	if err := os.WriteFile(name, []byte("corruption"), 0700); err != nil {
		t.Fatal(err)
	}
	output, diagnostic, err = run()
	if err == nil || len(output) != 0 || !bytes.Contains(diagnostic, []byte("digest mismatch")) {
		t.Fatalf("corruption accepted: %v stdout=%q stderr=%s", err, output, diagnostic)
	}
}
