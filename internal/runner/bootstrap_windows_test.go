package runner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"testing"
)

func TestWindowsBootstrapConcurrentInstall(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native Windows PowerShell installation")
	}
	data := bytes.Repeat([]byte("task-owned runner install fixture\n"), 2048)
	digest := sha256.Sum256(data)
	artifact := Artifact{Identity: CurrentIdentity(), SHA256: hex.EncodeToString(digest[:]), Data: data}
	platform := Runtime{Target: Target{OS: runtime.GOOS, Arch: runtime.GOARCH}, Home: t.TempDir()}
	install, err := PrepareInstallation(platform, artifact)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			command := exec.CommandContext(t.Context(), "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", install.Command)
			command.Stdin = bytes.NewReader(install.Input)
			if output, err := command.CombinedOutput(); err != nil {
				t.Errorf("concurrent installer: %v: %s", err, output)
			}
		}()
	}
	workers.Wait()
	name, err := artifact.RemotePath(platform)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(installed, data) {
		t.Fatalf("installed bytes differ: err=%v", err)
	}
	if err := os.WriteFile(name, []byte("corruption"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", install.Command)
	command.Stdin = bytes.NewReader(install.Input)
	if output, err := command.CombinedOutput(); err == nil || !bytes.Contains(output, []byte("digest mismatch")) {
		t.Fatalf("corruption accepted: %v: %s", err, output)
	}
}
