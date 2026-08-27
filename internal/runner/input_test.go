package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/crabbox/internal/runner/runnerfs"
)

type noEOFInput struct{ *bytes.Reader }

func (r noEOFInput) Read(p []byte) (int, error) {
	if r.Len() == 0 {
		return 0, errors.New("transport stdin remains open")
	}
	return r.Reader.Read(p)
}

func TestMainBoundedInputDoesNotRequireTransportEOF(t *testing.T) {
	var raw bytes.Buffer
	if err := WriteRequest(&raw, Request{BuildID: BuildID, Operation: Collect, Workdir: t.TempDir()}, 0, nil); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"serve", "serve-base64"} {
		data := raw.Bytes()
		if mode == "serve-base64" {
			data = []byte(base64.StdEncoding.EncodeToString(data))
		}
		var output, diagnostic bytes.Buffer
		code := Main(context.Background(), []string{mode, "--input-bytes", strconv.Itoa(len(data))}, noEOFInput{bytes.NewReader(data)}, &output, &diagnostic)
		if code != 0 {
			t.Fatalf("%s code=%d diagnostic=%s", mode, code, &diagnostic)
		}
		var response io.Reader = &output
		if mode == "serve-base64" {
			response = base64.NewDecoder(base64.StdEncoding, response)
		}
		if _, err := ReadResponse(t.Context(), response, CurrentIdentity(), Collect, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMainRejectsIncorrectInputBounds(t *testing.T) {
	var raw bytes.Buffer
	if err := WriteRequest(&raw, Request{BuildID: BuildID, Operation: Collect, Workdir: t.TempDir()}, 0, nil); err != nil {
		t.Fatal(err)
	}
	for _, size := range []string{"0", "-1", "not-a-number", strconv.Itoa(raw.Len() - 1), strconv.Itoa(raw.Len() + 1)} {
		if code := Main(t.Context(), []string{"serve", "--input-bytes", size}, bytes.NewReader(raw.Bytes()), io.Discard, io.Discard); code == 0 {
			t.Fatalf("size=%s accepted", size)
		}
	}
	withSuffix := append(append([]byte(nil), raw.Bytes()...), 'x')
	if code := Main(t.Context(), []string{"serve", "--input-bytes", strconv.Itoa(len(withSuffix))}, bytes.NewReader(withSuffix), io.Discard, io.Discard); code == 0 {
		t.Fatal("trailing data inside the declared transport frame accepted")
	}
}

func TestMainInputBoundsGateUploadPublication(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX archive publication")
	}
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	metadata, archive, err := runnerfs.CreateArchive(t.Context(), source, runnerfs.CreateOptions{}, runnerfs.DefaultArchiveLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	data, err := io.ReadAll(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int{-1, 1, 0} {
		destination := filepath.Join(t.TempDir(), "destination")
		if err := os.WriteFile(destination, []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
		var request bytes.Buffer
		if err := WriteRequest(&request, Request{BuildID: BuildID, Operation: Upload, Destination: destination, Source: metadata}, uint64(len(data)), bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		code := Main(t.Context(), []string{"serve", "--input-bytes", strconv.Itoa(request.Len() + delta)}, bytes.NewReader(request.Bytes()), io.Discard, io.Discard)
		got, err := os.ReadFile(destination)
		want := "old"
		if delta == 0 {
			want = "new"
		}
		if (code == 0) != (delta == 0) || err != nil || string(got) != want {
			t.Fatalf("delta=%d code=%d destination=%q want=%q err=%v", delta, code, got, want, err)
		}
	}
}

func TestWindowsInstallBindsNativeTransferToArtifact(t *testing.T) {
	data := []byte("fixture")
	digest := sha256.Sum256(data)
	artifact := Artifact{Identity: Identity{BuildID: "fixture", OS: "windows", Arch: "amd64", Protocol: 1}, SHA256: hex.EncodeToString(digest[:]), Data: data}
	install, err := PrepareInstallation(Runtime{Target: Target{OS: "windows", Arch: "amd64"}, Home: `C:\fixture`}, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(install.Command, "OpenStandardInput") || !strings.Contains(install.Command, "& tar.exe -xf - -C $stage runner.exe") || !strings.Contains(install.Command, ".Length -ne [Int64]7") {
		t.Fatalf("installer does not use a bounded native transfer: %s", install.Command)
	}
	reader := tar.NewReader(bytes.NewReader(install.Input))
	header, err := reader.Next()
	if err != nil || header.Name != "runner.exe" || header.Typeflag != tar.TypeReg {
		t.Fatalf("install header=%v err=%v", header, err)
	}
	got, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("install payload=%q err=%v", got, err)
	}
	if _, err := reader.Next(); err != io.EOF {
		t.Fatalf("unexpected additional install member: %v", err)
	}
}

func TestWindowsBootstrapTransfersLargeBinaryInput(t *testing.T) {
	shell := "pwsh"
	if runtime.GOOS == "windows" {
		shell = "powershell.exe"
	}
	shell, err := exec.LookPath(shell)
	if err != nil {
		t.Skip("PowerShell is unavailable")
	}
	data := bytes.Repeat([]byte{0, 0xff, 0x1a, '\r', '\n', 'M', 'Z', 0x90}, 1<<17)
	digest := sha256.Sum256(data)
	artifact := Artifact{Identity: Identity{BuildID: "fixture", OS: "windows", Arch: "amd64", Protocol: 1}, SHA256: hex.EncodeToString(digest[:]), Data: data}
	platform := Runtime{Target: Target{OS: "windows", Arch: "amd64"}, Home: t.TempDir()}
	install, err := PrepareInstallation(platform, artifact)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	// Windows PowerShell launched from pwsh can lack the Get-FileHash module.
	command := exec.CommandContext(ctx, shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "function Get-FileHash { throw 'hash module unavailable' };\n"+install.Command)
	if runtime.GOOS != "windows" {
		tarPath, err := exec.LookPath("tar")
		if err != nil {
			t.Skip("native tar is unavailable")
		}
		tools := t.TempDir()
		if err := os.Symlink(tarPath, filepath.Join(tools, "tar.exe")); err != nil {
			t.Fatal(err)
		}
		command.Env = append(os.Environ(), "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	command.WaitDelay = time.Second
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write(install.Input); err != nil {
		cancel()
		_ = command.Wait()
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		cancel()
		_ = command.Wait()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("install: %v: %s", err, &output)
		}
	case <-ctx.Done():
		_ = stdin.Close()
		<-done
		t.Fatalf("installer did not finish after complete input: %s", &output)
	}
	name, err := artifact.RemotePath(platform)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("installed bytes=%d want=%d err=%v", len(got), len(data), err)
	}
}
