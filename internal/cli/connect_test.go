package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunInteractiveSSHForwardsStreamsAndArguments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell script")
	}
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	sshPath := filepath.Join(dir, "ssh")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$CRABBOX_FAKE_SSH_ARGS"
printf 'out:'
cat
printf 'err\n' >&2
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_ARGS", argsPath)

	target := SSHTarget{
		User:                   "crabbox",
		Host:                   "203.0.113.10",
		Port:                   "2222",
		Key:                    "/tmp/crabbox key",
		CertificateFile:        "/tmp/crabbox-cert.pub",
		ProxyCommand:           "provider proxy %h %p",
		DisableHostKeyChecking: true,
		NoControlMaster:        true,
	}
	var stdout, stderr bytes.Buffer
	if err := runInteractiveSSH(context.Background(), target, strings.NewReader("hello\n"), &stdout, &stderr); err != nil {
		t.Fatalf("runInteractiveSSH: %v", err)
	}
	if got := stdout.String(); got != "out:hello\n" {
		t.Fatalf("stdout=%q", got)
	}
	if got := stderr.String(); got != "err\n" {
		t.Fatalf("stderr=%q", got)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	gotArgs := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	wantArgs := []string{
		"-i", "/tmp/crabbox key",
		"-o", "IdentitiesOnly=yes",
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "ForwardX11Trusted=no",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ConnectionAttempts=3",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=2",
		"-p", "2222",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ControlMaster=no",
		"-o", "CertificateFile=/tmp/crabbox-cert.pub",
		"-o", "ProxyCommand=provider proxy %h %p",
		"crabbox@203.0.113.10",
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("ssh args=%q\nwant=%q", gotArgs, wantArgs)
	}
}

func TestRunInteractiveSSHPreservesExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell script")
	}
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	if err := os.WriteFile(sshPath, []byte("#!/bin/sh\nexit 42\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := runInteractiveSSH(context.Background(), SSHTarget{
		User:                   "crabbox",
		Host:                   "203.0.113.10",
		Port:                   "22",
		DisableHostKeyChecking: true,
		NoControlMaster:        true,
	}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 42 || exitErr.Message != "" {
		t.Fatalf("error=%#v want silent exit 42", err)
	}
}

func TestRunInteractiveSSHRetriesFallbackPorts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell script")
	}
	dir := t.TempDir()
	portsPath := filepath.Join(dir, "ports")
	sshPath := filepath.Join(dir, "ssh")
	script := `#!/bin/sh
port=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-p" ]; then
    shift
    port="$1"
  fi
  shift || break
done
printf '%s\n' "$port" >> "$CRABBOX_FAKE_SSH_PORTS"
if [ "$port" = "2222" ]; then
  exit 255
fi
printf 'ok:'
cat
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_PORTS", portsPath)

	var stdout, stderr bytes.Buffer
	target := SSHTarget{
		User:                   "crabbox",
		Host:                   "203.0.113.10",
		Port:                   "2222",
		FallbackPorts:          []string{"22"},
		DisableHostKeyChecking: true,
		NoControlMaster:        true,
	}
	if !probeConnectSSHTransport(context.Background(), &target, 30*time.Second) {
		t.Fatal("resolve fallback port failed")
	}
	err := runInteractiveSSH(context.Background(), target, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatalf("runInteractiveSSH: %v", err)
	}
	if got := stdout.String(); got != "ok:" {
		t.Fatalf("stdout=%q", got)
	}
	data, err := os.ReadFile(portsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(data)), "2222\n22\n22"; got != want {
		t.Fatalf("ports=%q want %q", got, want)
	}
}
