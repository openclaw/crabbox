package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNativeLinuxTransportAdmissionAndFrame(t *testing.T) {
	s := testNativeRuntimeScope()
	s.manifest = "selected fixture"
	ctx := context.WithValue(runtimeLeaseContext(t.Context(), "lease-a"), nativeRuntimeScopeKey{}, s)
	target := SSHTarget{Host: "example.invalid", Port: "22", TargetOS: targetLinux}
	s.ready = func(context.Context, *SSHTarget, io.Writer) error { return nil }
	bootstrap, err := prepareSSHTransport(ctx, target, "true", nil, 0, sshCommandLimit{})
	if err != nil || bootstrap.native != nil {
		t.Fatalf("bootstrap unexpectedly selected native transport: %v", err)
	}
	ctx, err = s.prepareCommandRuntime(ctx, &target, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	control, err := prepareSSHTransport(ctx, target, "true", nil, 0, sshCommandLimit{execution: time.Second, control: true})
	if err != nil || control.native != nil || control.replay != nil {
		t.Fatalf("metadata control became another native workload: %v", err)
	}
	command, payload := "printf 'café\n'; cat", "input\nwith\x00bytes"
	transport, err := prepareSSHTransport(ctx, target, command, strings.NewReader(payload), int64(len(payload)), sshCommandLimit{execution: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.close()
	if transport.native == nil || transport.stage != nil || !strings.Contains(transport.command, "preflight") || !strings.Contains(transport.command, "watched") {
		t.Fatalf("Linux command did not select retained watched native frame: %+v", transport)
	}
	frame, err := transport.reset()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(frame)
	if err != nil || string(got) != command+payload {
		t.Fatalf("frame changed command/input: %q, %v", got, err)
	}
	if _, err := prepareSSHTransport(runtimeLeaseContext(ctx, "lease-b"), target, "true", nil, 0, sshCommandLimit{}); err == nil {
		t.Fatal("another lease borrowed the admitted runtime")
	}
	changed := target
	changed.Host = "other.invalid"
	if _, err := prepareSSHTransport(ctx, changed, "true", nil, 0, sshCommandLimit{}); !errors.Is(err, errNativeRuntimeUnprepared) {
		t.Fatalf("changed route reused runtime: %v", err)
	}
	if err := s.finish(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareSSHTransport(ctx, target, "true", nil, 0, sshCommandLimit{}); err == nil {
		t.Fatal("finalized lease admitted a command")
	}
}

func TestNativeCommandInputKeepsWatchOpen(t *testing.T) {
	cmd := &exec.Cmd{}
	closeInput, err := nativeCommandInput(cmd, strings.NewReader("commandinput"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeInput()
	reader := cmd.Stdin.(*os.File)
	frame := make([]byte, len("commandinput"))
	if _, err := io.ReadFull(reader, frame); err != nil || string(frame) != "commandinput" {
		t.Fatalf("frame: %q %v", frame, err)
	}
	if err := reader.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Skipf("pipe deadlines unavailable: %v", err)
	}
	_, err = reader.Read(make([]byte, 1))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("finite frame closed the watch before SSH returned: %v", err)
	}
}

func TestNativeLinuxAdmissionBindsPreparedRelay(t *testing.T) {
	s := testNativeRuntimeScope()
	s.manifest = "selected fixture"
	s.ready = func(context.Context, *SSHTarget, io.Writer) error { return nil }
	target := SSHTarget{
		Host: "cbx_001122334455", Port: "22", User: "fixture", TargetOS: targetLinux,
		Key: "fixture-key", KnownHostsFile: "fixture-hosts", SSHHostKey: "fixture-host-key",
		ProxyCommand: "original-relay", SSHJump: &SSHJump{Host: "relay.invalid", HostKey: "fixture-relay-key", HostKeyAlias: "fixture-relay", useRelay: true},
	}
	controls := &serveRelayControls{entries: map[[32]byte]serveRelayControl{
		serveRelayControlKey(target): {proxy: "prepared-relay", path: "fixture-control"},
	}}
	ctx := context.WithValue(t.Context(), serveRelayControlsKey{}, controls)
	ctx = context.WithValue(ctx, nativeRuntimeScopeKey{}, s)
	ctx, err := s.prepareCommandRuntime(ctx, &target, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := prepareSSHTransport(ctx, target, "true", nil, 0, sshCommandLimit{})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.close()
	dispatch, err := prepareServeRelayControl(ctx, target)
	if err != nil || transport.native == nil || dispatch.ProxyCommand != "prepared-relay" || nativeRuntimeRouteKey(dispatch) != nativeRuntimeRouteKey(transport.native.target) {
		t.Fatalf("admission and dispatch used different relay routes: %v", err)
	}
}

func TestNativeLinuxSeparateBackgroundTelemetryAndProbeLifetimes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX transport fixture")
	}
	tools := t.TempDir()
	writeExecutable(t, filepath.Join(tools, "ssh"), "#!/bin/sh\nfor arg; do remote=\"$arg\"; done\nexec /bin/sh -c \"$remote\"\n")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := testNativeRuntimeScope()
	s.manifest = "selected fixture"
	ctx := context.WithValue(t.Context(), nativeRuntimeScopeKey{}, s)
	target := SSHTarget{Host: "example.invalid", Port: "22", TargetOS: targetLinux, NoControlMaster: true}
	ctx, err := s.admit(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture runtime path is deliberately not executable: these existing
	// owners must not dispatch a foreground native stage, even after admission.
	out, err := runWorkspaceOwnerBackgroundOutput(ctx, target, nil, "printf background-owner")
	if err != nil || out != "background-owner" {
		t.Fatalf("background owner became a foreground stage: %q %v", out, err)
	}
	telemetry, err := collectLeaseTelemetry(ctx, target)
	if err != nil || telemetry == nil {
		t.Fatalf("read-only telemetry became a foreground stage: %v", err)
	}
	candidate := target
	candidate.Host, candidate.Port = "other.invalid", "2222"
	if !probeSSHTransport(ctx, &candidate, time.Second) {
		t.Fatal("route selection required admission on an unselected route")
	}
}
