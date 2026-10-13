//go:build !windows

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/openclaw/crabbox/internal/runtimeartifact"
)

var egressFixtureOnce sync.Once
var egressFixtureBytes = map[string][]byte{}
var egressFixtureError error

func egressPackFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	egressFixtureOnce.Do(func() {
		// Remote execution is mocked; only package and target metadata are real.
		source := filepath.Join(dir, "source")
		pkg := filepath.Join(source, "cmd", "crabbox-runtime")
		if err := os.MkdirAll(pkg, 0700); err != nil {
			egressFixtureError = err
			return
		}
		for _, file := range []struct{ path, content string }{
			{filepath.Join(source, "go.mod"), "module github.com/openclaw/crabbox\n\ngo 1.26\n"},
			{filepath.Join(pkg, "main.go"), "package main\nfunc main() {}\n"},
		} {
			if err := os.WriteFile(file.path, []byte(file.content), 0600); err != nil {
				egressFixtureError = err
				return
			}
		}
		for _, arch := range []string{"amd64", "arm64"} {
			name := "linux-" + arch
			cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", filepath.Join(dir, name), "./cmd/crabbox-runtime")
			cmd.Dir = source
			cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0", "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=", "GOPROXY=off", "GOSUMDB=off")
			if out, err := cmd.CombinedOutput(); err != nil {
				egressFixtureError = fmt.Errorf("build helper: %w: %s", err, out)
				return
			}
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				egressFixtureError = err
				return
			}
			egressFixtureBytes[arch] = data
		}
	})
	if egressFixtureError != nil {
		t.Fatal(egressFixtureError)
	}
	controller, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var inputs []runtimeartifact.CapabilityInput
	for _, arch := range []string{"amd64", "arm64"} {
		name := "linux-" + arch
		if err := os.WriteFile(filepath.Join(dir, name), egressFixtureBytes[arch], 0600); err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, runtimeartifact.CapabilityInput{Target: runtimeartifact.Target{OS: "linux", Arch: arch}, Path: name, Capabilities: []runtimeartifact.CapabilityClaim{{Name: runtimeartifact.Egress, ProtocolVersion: runtimeartifact.EgressProtocol}}})
	}
	data, err := runtimeartifact.MarshalCapabilities(t.Context(), dir, controller, inputs)
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifest, data, 0600); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestEgressHelperPackSelectionAndAtomicInstall(t *testing.T) {
	manifest := egressPackFixture(t)
	controller, _ := os.Executable()
	source, err := egressRuntimeSource(t.Context(), controller, manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			artifact, err := source.Open(t.Context(), runtimeartifact.Target{OS: "linux", Arch: arch})
			if err != nil {
				t.Fatal(err)
			}
			digest := artifact.Identity().SHA256
			if err := artifact.Close(); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			log := filepath.Join(dir, "ssh.log")
			t.Setenv("CRABBOX_FAKE_SSH_LOG", log)
			t.Setenv("CRABBOX_FAKE_DIGEST", digest)
			machine := "x86_64"
			if arch == "arm64" {
				machine = "aarch64"
			}
			t.Setenv("CRABBOX_FAKE_MACHINE", machine)
			identity, _ := json.Marshal(egressRuntimeIdentity{Protocol: runtimeartifact.EgressProtocol, OS: "linux", Arch: arch})
			t.Setenv("CRABBOX_FAKE_IDENTITY", string(identity))
			python, err := exec.LookPath("python3")
			if err != nil {
				t.Fatal(err)
			}
			mustWriteTestFile(t, filepath.Join(dir, "ssh"), "#!"+python+"\n"+`import os, sys, shlex, re
remote = sys.argv[-1]
with open(os.environ["CRABBOX_FAKE_SSH_LOG"], "a") as log:
    log.write(remote + "\n")
script = shlex.split(remote)[-1] if "/bin/sh -c " in remote else remote
if "uname -m" in script:
    print(os.environ["CRABBOX_FAKE_MACHINE"] + "\nraw")
elif "sha256sum --" in script:
    filename = re.search(r"sha256sum -- '([^']+)'", script)[1]
    print(os.environ["CRABBOX_FAKE_DIGEST"] + "  " + filename)
elif "egress client --internal-identity" in script:
    print(os.environ["CRABBOX_FAKE_IDENTITY"])
else:
    sys.stdin.buffer.read()
`)

			if err := os.Chmod(filepath.Join(dir, "ssh"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			t.Setenv(runtimeArtifactsEnv, manifest)
			t.Chdir(t.TempDir())
			app := App{Stdout: io.Discard, Stderr: io.Discard}
			if err := app.installRemoteEgressClient(context.Background(), SSHTarget{User: "fixture", Host: "runner.example", Port: "22", TargetOS: targetLinux}); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(log)
			for _, want := range []string{"cp --", "chmod 700", "mv -f --", egressRemoteBinary + ".tmp-", "sha256sum --", "egress client --internal-identity"} {
				if !strings.Contains(string(data), want) {
					t.Fatalf("installation missing %q: %s", want, data)
				}
			}
		})
	}
	for _, test := range []struct{ name, controller, manifest, want string }{
		{"wrong controller", filepath.Join(t.TempDir(), "controller"), manifest, "controller SHA-256 mismatch"},
		{"missing pack", controller, filepath.Join(t.TempDir(), "missing.json"), "reinstall"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "wrong controller" {
				if err := os.WriteFile(test.controller, []byte("other"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := egressRuntimeSource(t.Context(), test.controller, test.manifest)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	// The snapshot still hashes the selected bytes at every open.
	file := filepath.Join(filepath.Dir(manifest), "linux-arm64")
	if err := os.WriteFile(file, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Open(t.Context(), runtimeartifact.Target{OS: "linux", Arch: "arm64"}); err == nil {
		t.Fatal("tampered helper accepted")
	}
}
