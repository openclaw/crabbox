package cli

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestPortsCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.ports(context.Background(), []string{"--provider", "docker-sandbox", "--id", "blue-box", "--publish", "8080"})
	if err != nil {
		t.Fatalf("ports err=%v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "127.0.0.1:41000->3000/tcp") {
		t.Fatalf("stdout=%q", got)
	}

	stdout.Reset()
	err = app.ports(context.Background(), []string{"--provider", "docker-sandbox", "--id", "blue-box", "--json"})
	if err != nil {
		t.Fatalf("ports json err=%v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "127.0.0.1:41000->3000/tcp") {
		t.Fatalf("json stdout=%q", got)
	}

	err = app.ports(context.Background(), []string{"--provider", "docker-sandbox", "--id", "blue-box", "--publish", "8080", "--unpublish", "8080:3000"})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("conflicting flags err=%v", err)
	}
	err = app.ports(context.Background(), []string{"--provider", "docker-sandbox", "--id", "blue-box", "extra"})
	if err == nil || !strings.Contains(err.Error(), "usage: crabbox ports") {
		t.Fatalf("extra positional err=%v", err)
	}
	stderr.Reset()
	err = app.Run(context.Background(), []string{"ports", "--help"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 0 {
		t.Fatalf("ports --help err=%v", err)
	}
	if !strings.Contains(stderr.String(), "Usage") {
		t.Fatalf("ports help=%q", stderr.String())
	}
}

func TestCopyCommand(t *testing.T) {
	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	err := app.copyCommand(context.Background(), []string{"--provider", "docker-sandbox", "--id", "blue-box", "./coverage.xml", "SANDBOX:/tmp/coverage.xml"})
	if err != nil {
		t.Fatalf("copy err=%v", err)
	}
	err = app.copyCommand(context.Background(), []string{"--provider", "docker-sandbox", "--id", "blue-box", "./coverage.xml", "./out.xml"})
	if err == nil || !strings.Contains(err.Error(), "usage: crabbox cp") {
		t.Fatalf("missing sandbox path err=%v", err)
	}
	err = app.copyCommand(context.Background(), []string{"./coverage.xml", "SANDBOX:/tmp/coverage.xml"})
	if err == nil || !strings.Contains(err.Error(), "usage: crabbox cp") {
		t.Fatalf("missing id err=%v", err)
	}
}

func TestPortsRejectsUnsupportedProvider(t *testing.T) {
	err := (App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).ports(context.Background(), []string{"--provider", "aws", "--id", "cbx_123"})
	if err == nil || !strings.Contains(err.Error(), "does not support ports") {
		t.Fatalf("err=%v", err)
	}
}

func TestCopyRejectsUnsupportedProvider(t *testing.T) {
	err := (App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).copyCommand(context.Background(), []string{"--provider", "service-control-test", "--id", "service_123", "./file.txt", "SANDBOX:/tmp/file.txt"})
	if err == nil || !strings.Contains(err.Error(), "does not support cp") {
		t.Fatalf("err=%v", err)
	}
}

func TestSSHCopyArgsPreserveResolvedTransport(t *testing.T) {
	target := SSHTarget{
		User:                   "agent",
		Host:                   "box.example.test",
		Port:                   "2222",
		Key:                    "/tmp/agent key",
		CertificateFile:        "/tmp/agent-cert.pub",
		ProxyCommand:           "provider proxy %h %p",
		DisableHostKeyChecking: true,
		NoControlMaster:        true,
	}

	wantPrefix := []string{
		"-i", "/tmp/agent key",
		"-o", "IdentitiesOnly=yes",
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "ForwardX11Trusted=no",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ConnectionAttempts=3",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=2",
		"-P", "2222",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ControlMaster=no",
		"-o", "CertificateFile=/tmp/agent-cert.pub",
		"-o", "ProxyCommand=provider proxy %h %p",
	}

	toSandbox, err := sshCopyArgs(target, "./input.txt", "SANDBOX:/workspace/input.txt")
	if err != nil {
		t.Fatal(err)
	}
	wantToSandbox := append(append([]string{}, wantPrefix...), "./input.txt", "agent@box.example.test:/workspace/input.txt")
	if !reflect.DeepEqual(toSandbox, wantToSandbox) {
		t.Fatalf("to sandbox args=%q\nwant=%q", toSandbox, wantToSandbox)
	}

	fromSandbox, err := sshCopyArgs(target, "SANDBOX:/workspace/output.txt", "./output.txt")
	if err != nil {
		t.Fatal(err)
	}
	wantFromSandbox := append(append([]string{}, wantPrefix...), "agent@box.example.test:/workspace/output.txt", "./output.txt")
	if !reflect.DeepEqual(fromSandbox, wantFromSandbox) {
		t.Fatalf("from sandbox args=%q\nwant=%q", fromSandbox, wantFromSandbox)
	}
}
