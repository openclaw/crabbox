package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSSHTunnelArgsUseLoopbackAndResolvedTransport(t *testing.T) {
	target := SSHTarget{
		User:            "agent",
		Host:            "box.example.test",
		Port:            "2222",
		Key:             "/tmp/agent key",
		CertificateFile: "/tmp/agent-cert.pub",
		ProxyCommand:    "provider proxy %h %p",
		NoControlMaster: true,
	}

	got := sshTunnelArgs(target, 41000, 3000)
	want := append(sshBaseArgs(target),
		"-N",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "GatewayPorts=no",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "ControlPersist=no",
		"-L", "127.0.0.1:41000:127.0.0.1:3000",
		"agent@box.example.test",
	)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%q\nwant=%q", got, want)
	}
}

func TestTunnelCommandHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"tunnel", "--help"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 0 {
		t.Fatalf("tunnel --help err=%v", err)
	}
	for _, want := range []string{"-id string", "-port int", "-local-port int", "-json"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("tunnel help missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestRunSSHTunnelReportsReadinessAndStopsWithContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake ssh helper is only reliable on Unix hosts")
	}
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	if err := os.WriteFile(sshPath, []byte("#!/bin/sh\nexec \"$CRABBOX_TEST_BINARY\" -test.run=TestSSHTunnelHelperProcess -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_TEST_BINARY", executable)
	t.Setenv("CRABBOX_TUNNEL_HELPER", "1")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	localPort, err := availableTunnelPort()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr synchronizedBuffer
	done := make(chan error, 1)
	go func() {
		done <- runSSHTunnel(ctx, SSHTarget{Host: "example.invalid", User: "tester"}, localPort, 3000, true, &stdout, &stderr)
	}()

	deadline := time.After(3 * time.Second)
	for !strings.Contains(stdout.String(), fmt.Sprintf(`"port":%d`, localPort)) {
		select {
		case err := <-done:
			t.Fatalf("tunnel stopped before readiness: %v\nstderr=%s", err, stderr.String())
		case <-deadline:
			t.Fatalf("tunnel did not report readiness: %s", stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("tunnel stop error=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tunnel did not stop with its context")
	}
}

func TestSSHTunnelHelperProcess(t *testing.T) {
	if os.Getenv("CRABBOX_TUNNEL_HELPER") != "1" {
		return
	}
	forward := ""
	for i, arg := range os.Args {
		if arg == "-L" && i+1 < len(os.Args) {
			forward = os.Args[i+1]
			break
		}
	}
	parts := strings.Split(forward, ":")
	if len(parts) != 4 {
		fmt.Fprintln(os.Stderr, "missing SSH forwarding argument")
		os.Exit(22)
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(parts[0], parts[1]))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(22)
	}
	defer listener.Close()
	time.Sleep(time.Hour)
}
