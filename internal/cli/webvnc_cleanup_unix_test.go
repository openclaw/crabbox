//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type webVNCCleanupRecord struct {
	Child        int
	ChildStarted string
	Tunnel       int
	Started      string
}

// Replace only lease/coordinator resolution. The subprocess supervisor, signal
// handler, foreground tunnel owner, SSH executable, and daemon stop are real.
func init() {
	root := os.Getenv("CRABBOX_WEBVNC_CLEANUP_FIXTURE")
	if root == "" || len(os.Args) < 2 {
		return
	}
	if os.Args[1] != "__webvnc-supervisor" && os.Args[1] != "webvnc" {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Match cmd/crabbox: another signal must not be needed for orderly cleanup.
	go func() { <-ctx.Done(); stop() }()
	var err error
	if os.Args[1] == "__webvnc-supervisor" {
		err = (App{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}).webVNCDaemonSupervisor(ctx, os.Args[2:])
	} else {
		err = runWebVNCCleanupBridge(ctx, root)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runWebVNCCleanupBridge(ctx context.Context, root string) error {
	ctx, finish, err := beginSupervisedWebVNCCleanup(ctx)
	if err != nil {
		return err
	}
	defer finish()
	data, err := os.ReadFile(filepath.Join(root, "target.json"))
	if err != nil {
		return err
	}
	var target SSHTarget
	if err := json.Unmarshal(data, &target); err != nil {
		return err
	}
	tunnelCtx := ctx
	if os.Getenv("CRABBOX_WEBVNC_CLEANUP_MODE") == "blocked-cleanup" {
		// Model an unresponsive foreground owner without changing the real SSH
		// process or its listener, deadline, or ownership checks.
		tunnelCtx = context.WithoutCancel(ctx)
	}
	tunnel, err := startVNCForegroundTunnel(tunnelCtx, target, os.Getenv("CRABBOX_WEBVNC_CLEANUP_PORT"), "127.0.0.1", os.Getenv("CRABBOX_WEBVNC_CLEANUP_REMOTE"))
	if err != nil {
		return err
	}
	defer func() {
		if os.Getenv("CRABBOX_WEBVNC_CLEANUP_MODE") == "abandoned-tunnel" {
			return
		}
		stopProcess(tunnel)
		// Written only after the actual owner's Stop/Wait path completed.
		_ = os.WriteFile(filepath.Join(root, fmt.Sprintf("reaped-%d", os.Getpid())), nil, 0o600)
	}()
	started, err := webVNCDaemonProcessStartIdentity(tunnel.PID())
	if err != nil {
		return err
	}
	childStarted, err := webVNCDaemonProcessStartIdentity(os.Getpid())
	if err != nil {
		return err
	}
	record, err := json.Marshal(webVNCCleanupRecord{Child: os.Getpid(), ChildStarted: childStarted, Tunnel: tunnel.PID(), Started: started})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("ready-%d.json", os.Getpid())), record, 0o600); err != nil {
		return err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if os.Getenv("CRABBOX_WEBVNC_CLEANUP_MODE") == "blocked-cleanup" {
				select {}
			}
			if os.Getenv("CRABBOX_WEBVNC_CLEANUP_MODE") == "delayed-cleanup" {
				_ = os.WriteFile(filepath.Join(root, "cleaning"), nil, 0o600)
				for {
					if _, err := os.Stat(filepath.Join(root, "finish-cleanup")); err == nil {
						break
					}
					<-ticker.C
				}
			}
			return ctx.Err()
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(root, fmt.Sprintf("restart-%d", os.Getpid()))); err == nil {
				return errors.New("synthetic bridge failure")
			}
		}
	}
}

func TestWebVNCDaemonCleanupRealSSHAfterRestart(t *testing.T) {
	for _, shutdown := range []string{"stop", "cancel"} {
		t.Run(shutdown, func(t *testing.T) {
			root, port, cmd, identity, pidPath, _ := startWebVNCCleanupFixture(t, "")
			first := waitWebVNCCleanupRecord(t, root, 0)
			assertForwardPayload(t, port)
			if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("restart-%d", first.Child)), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			second := waitWebVNCCleanupRecord(t, root, first.Child)
			if first.Tunnel == second.Tunnel {
				t.Fatal("restart reused the old SSH process")
			}
			assertWebVNCCleanupReaped(t, root, first)
			group, err := syscall.Getpgid(second.Tunnel)
			if err != nil || group != second.Tunnel || group == cmd.Process.Pid {
				t.Fatalf("SSH is not in its separate owned group: group=%d err=%v", group, err)
			}
			assertForwardPayload(t, port)
			started := time.Now()
			if shutdown == "stop" {
				stopped, err := (App{Stdout: io.Discard, Stderr: io.Discard}).stopWebVNCDaemonIfRunning(t.Context(), identity.WorkspaceID)
				if err != nil || !stopped {
					t.Fatalf("stop=%t err=%v", stopped, err)
				}
				if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
					t.Fatalf("successful stop retained identity: %v", err)
				}
			} else if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			waitWebVNCCleanupSupervisor(t, cmd)
			if time.Since(started) > 8*time.Second {
				t.Fatal("cleanup exceeded unchanged eight-second proof bound")
			}
			if webVNCDaemonProcessGroupAlive(identity.PID) {
				t.Fatal("supervisor group survived shutdown")
			}
			for {
				conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", port), 200*time.Millisecond)
				if err != nil {
					break
				}
				conn.Close()
				if time.Since(started) >= 8*time.Second {
					assertForwardPayload(t, port)
					t.Error("real SSH listener still carries payload eight seconds after daemon shutdown")
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			assertWebVNCCleanupReaped(t, root, second)
			configs, err := filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), "crabbox-ssh-transport-*"))
			if err != nil || len(configs) != 0 {
				t.Fatalf("owned SSH configs survived cleanup: count=%d err=%v", len(configs), err)
			}
		})
	}
}

func startWebVNCCleanupFixture(t *testing.T, mode string) (string, string, *exec.Cmd, webVNCDaemonIdentity, string, *forwardSSHServer) {
	t.Helper()
	dirs := isolateTestUserDirs(t)
	root := dirs.Root
	if _, err := os.Stat("/usr/bin/ssh"); err != nil {
		t.Fatal("cleanup regression requires real /usr/bin/ssh")
	}
	for _, name := range []string{"tmp", "bin"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/usr/bin/ssh", filepath.Join(root, "bin", "ssh")); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"CRABBOX_WEBVNC_CLEANUP_FIXTURE": root,
		"CRABBOX_WEBVNC_CLEANUP_MODE":    mode,
		"TMPDIR":                         filepath.Join(root, "tmp"), "PATH": filepath.Join(root, "bin") + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"SSH_AUTH_SOCK": "", "SSH_AGENT_PID": "",
		"GORACE": strings.TrimSpace(os.Getenv("GORACE") + " atexit_sleep_ms=0"),
	} {
		t.Setenv(name, value)
	}
	remotePort := forwardEchoServer(t)
	server := newForwardSSHServer(t, "cleanup-fixture", remotePort)
	if mode != "blocked-auth" {
		close(server.release)
	}
	target := SSHTarget{Host: "127.0.0.1", Port: strconv.Itoa(server.port()), User: "cleanup-fixture", AuthSecret: true, SSHHostKey: server.hostKey, KnownHostsFile: filepath.Join(root, "known_hosts")}
	if err := os.WriteFile(target.KnownHostsFile, []byte(fmt.Sprintf("[127.0.0.1]:%d %s\n", server.port(), server.hostKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "target.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	port := boundaryPort(t)
	t.Setenv("CRABBOX_WEBVNC_CLEANUP_PORT", port)
	t.Setenv("CRABBOX_WEBVNC_CLEANUP_REMOTE", strconv.Itoa(remotePort))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	nonce := "1234567890abcdef1234567890abcdef"
	// The local alias deliberately differs from the child's resolved lease ID.
	cmd := exec.Command(executable, "__webvnc-supervisor", nonce, "crabbox-cleanup-fixture", "webvnc", "--id", "cbx_cleanup_fixture")
	gateReader, gateWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer gateReader.Close()
	defer gateWriter.Close()
	cmd.Stdin = gateReader
	log, err := os.Create(filepath.Join(root, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	cmd.Stdout, cmd.Stderr = log, log
	t.Cleanup(func() {
		if t.Failed() {
			data, _ := os.ReadFile(log.Name())
			t.Logf("synthetic daemon log: %s", data)
		}
	})
	configureDaemonCommand(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The fixture owns this unreaped child. The server remains open until
		// after assertions; disconnecting it must never make stop appear to pass.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	started, err := webVNCDaemonProcessStartIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	identity := webVNCDaemonIdentity{Version: webVNCDaemonIdentityVersion, WorkspaceID: "crabbox-cleanup-fixture", PID: cmd.Process.Pid, ProcessStarted: started, BootID: currentProcessBootIdentityForTest(t), Nonce: nonce, CleanupTracked: true}
	_, pidPath, err := webVNCDaemonPaths(identity.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeWebVNCDaemonIdentity(pidPath, identity); err != nil {
		t.Fatal(err)
	}
	if err := writeWebVNCDaemonSupervisorGate(gateWriter, ""); err != nil {
		t.Fatal(err)
	}
	return root, port, cmd, identity, pidPath, server
}

func waitWebVNCCleanupRecord(t *testing.T, root string, previousChild int) webVNCCleanupRecord {
	t.Helper()
	var record webVNCCleanupRecord
	boundaryEventually(t, "real SSH foreground child ready", func() bool {
		paths, _ := filepath.Glob(filepath.Join(root, "ready-*.json"))
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err == nil && json.Unmarshal(data, &record) == nil && record.Child != previousChild {
				return true
			}
		}
		return false
	})
	t.Cleanup(func() {
		if current, err := webVNCDaemonProcessStartIdentity(record.Child); err == nil && current == record.ChildStarted {
			_ = syscall.Kill(record.Child, syscall.SIGKILL)
		}
		if current, err := webVNCDaemonProcessStartIdentity(record.Tunnel); err == nil && current == record.Started {
			_ = syscall.Kill(-record.Tunnel, syscall.SIGKILL)
		}
	})
	return record
}

func TestWebVNCDaemonCleanupRefusesUnconfirmedRealSSH(t *testing.T) {
	for _, failure := range []string{"child-killed", "supervisor-killed", "blocked-cleanup", "abandoned-tunnel", "legacy-child-killed"} {
		t.Run(failure, func(t *testing.T) {
			root, port, cmd, identity, pidPath, _ := startWebVNCCleanupFixture(t, failure)
			record := waitWebVNCCleanupRecord(t, root, 0)
			assertForwardPayload(t, port)
			switch failure {
			case "child-killed", "legacy-child-killed":
				if failure == "legacy-child-killed" {
					identity.CleanupTracked = false
					if err := writeWebVNCDaemonIdentity(pidPath, identity); err != nil {
						t.Fatal(err)
					}
				}
				if err := syscall.Kill(record.Child, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				waitWebVNCCleanupSupervisor(t, cmd)
			case "abandoned-tunnel":
				if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("restart-%d", record.Child)), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				waitWebVNCCleanupSupervisor(t, cmd)
			case "supervisor-killed":
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				waitWebVNCCleanupSupervisor(t, cmd)
			}
			started := time.Now()
			app := App{Stdout: io.Discard, Stderr: io.Discard}
			stopped, err := app.stopWebVNCDaemonIfRunning(t.Context(), identity.WorkspaceID)
			if err == nil || stopped {
				t.Fatalf("unconfirmed cleanup reported success: stopped=%t err=%v", stopped, err)
			}
			if time.Since(started) > 8*time.Second {
				t.Fatal("forced cleanup exceeded existing proof bound")
			}
			if failure == "blocked-cleanup" {
				if !strings.Contains(err.Error(), "timed out") {
					t.Fatalf("forced stop did not report timeout: %v", err)
				}
				waitWebVNCCleanupSupervisor(t, cmd)
			}
			// Prove the reason for failure before fixture teardown closes SSH.
			assertForwardPayload(t, port)
			if _, err := os.Stat(pidPath + ".cleanup"); !os.IsNotExist(err) {
				t.Fatalf("unconfirmed shutdown published a receipt: %v", err)
			}
			retained, err := readWebVNCDaemonIdentity(pidPath)
			if err != nil || retained != identity {
				t.Fatalf("exact cleanup identity lost: %v", err)
			}
			if stopped, err := app.stopWebVNCDaemonIfRunning(t.Context(), identity.WorkspaceID); stopped || err == nil {
				t.Fatalf("retry blessed the orphan: stopped=%t err=%v", stopped, err)
			}
			paths, _ := filepath.Glob(filepath.Join(root, "ready-*.json"))
			if len(paths) != 1 {
				t.Fatal("supervisor restarted after unconfirmed child cleanup")
			}
		})
	}
}

func TestWebVNCDaemonCleanupPreservesMismatchedIdentity(t *testing.T) {
	root, port, cmd, identity, pidPath, _ := startWebVNCCleanupFixture(t, "")
	_ = waitWebVNCCleanupRecord(t, root, 0)
	mismatch := identity
	mismatch.ProcessStarted += "-stale"
	if err := writeWebVNCDaemonIdentity(pidPath, mismatch); err != nil {
		t.Fatal(err)
	}
	if stopped, err := (App{Stdout: io.Discard, Stderr: io.Discard}).stopWebVNCDaemonIfRunning(t.Context(), identity.WorkspaceID); stopped || err == nil {
		t.Fatalf("mismatched identity signaled: stopped=%t err=%v", stopped, err)
	}
	assertForwardPayload(t, port)
	if err := writeWebVNCDaemonIdentity(pidPath, identity); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitWebVNCCleanupSupervisor(t, cmd)
	// A receipt copied from another launch is never evidence for this one.
	mismatch.Nonce = "abcdef1234567890abcdef1234567890"
	if err := writeWebVNCDaemonIdentity(pidPath+".cleanup", mismatch); err != nil {
		t.Fatal(err)
	}
	if stopped, err := (App{Stdout: io.Discard, Stderr: io.Discard}).stopWebVNCDaemonIfRunning(t.Context(), identity.WorkspaceID); stopped || err == nil {
		t.Fatalf("copied receipt accepted: stopped=%t err=%v", stopped, err)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("identity lost after receipt mismatch: %v", err)
	}
}

func TestWebVNCDaemonCleanupDuringSSHStartup(t *testing.T) {
	_, port, cmd, identity, _, server := startWebVNCCleanupFixture(t, "blocked-auth")
	select {
	case <-server.entered:
	case <-time.After(8 * time.Second):
		t.Fatal("SSH did not reach the authentication gate")
	}
	stopped, err := (App{Stdout: io.Discard, Stderr: io.Discard}).stopWebVNCDaemonIfRunning(t.Context(), identity.WorkspaceID)
	if err != nil || !stopped {
		t.Fatalf("startup stop=%t err=%v", stopped, err)
	}
	waitWebVNCCleanupSupervisor(t, cmd)
	close(server.release)
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", port), 200*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("cancelled startup left an SSH listener")
	}
	configs, err := filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), "crabbox-ssh-transport-*"))
	if err != nil || len(configs) != 0 {
		t.Fatalf("startup cleanup retained private configs: count=%d err=%v", len(configs), err)
	}
}

func TestWebVNCDaemonCleanupSurvivesRepeatedSignals(t *testing.T) {
	root, _, cmd, identity, _, _ := startWebVNCCleanupFixture(t, "delayed-cleanup")
	record := waitWebVNCCleanupRecord(t, root, 0)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	boundaryEventually(t, "foreground cleanup started", func() bool {
		_, err := os.Stat(filepath.Join(root, "cleaning"))
		return err == nil
	})
	for _, pid := range []int{cmd.Process.Pid, record.Child, cmd.Process.Pid, record.Child} {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "finish-cleanup"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitWebVNCCleanupSupervisor(t, cmd)
	assertWebVNCCleanupReaped(t, root, record)
	if stopped, err := (App{Stdout: io.Discard, Stderr: io.Discard}).stopWebVNCDaemonIfRunning(t.Context(), identity.WorkspaceID); !stopped || err != nil {
		t.Fatalf("repeated signals lost cleanup: stopped=%t err=%v", stopped, err)
	}
}

func TestWebVNCDaemonCleanupDuringRestartBackoff(t *testing.T) {
	root, _, cmd, identity, _, _ := startWebVNCCleanupFixture(t, "")
	record := waitWebVNCCleanupRecord(t, root, 0)
	if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("restart-%d", record.Child)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	boundaryEventually(t, "supervisor restart backoff", func() bool {
		data, err := os.ReadFile(filepath.Join(root, "daemon.log"))
		return err == nil && strings.Contains(string(data), "restarting in 1s")
	})
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitWebVNCCleanupSupervisor(t, cmd)
	assertWebVNCCleanupReaped(t, root, record)
	if stopped, err := (App{Stdout: io.Discard, Stderr: io.Discard}).stopWebVNCDaemonIfRunning(t.Context(), identity.WorkspaceID); !stopped || err != nil {
		t.Fatalf("backoff cancellation lost cleanup: stopped=%t err=%v", stopped, err)
	}
	paths, _ := filepath.Glob(filepath.Join(root, "ready-*.json"))
	if len(paths) != 1 {
		t.Fatal("cancelled supervisor restarted the child")
	}
}

func TestWebVNCDaemonCleanupReceiptPublicationFailure(t *testing.T) {
	root, _, cmd, identity, pidPath, _ := startWebVNCCleanupFixture(t, "")
	record := waitWebVNCCleanupRecord(t, root, 0)
	if err := os.Mkdir(pidPath+".cleanup", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitWebVNCCleanupSupervisor(t, cmd)
	assertWebVNCCleanupReaped(t, root, record)
	if stopped, err := (App{Stdout: io.Discard, Stderr: io.Discard}).stopWebVNCDaemonIfRunning(t.Context(), identity.WorkspaceID); stopped || err == nil {
		t.Fatalf("receipt publication failure was hidden: stopped=%t err=%v", stopped, err)
	}
	retained, err := readWebVNCDaemonIdentity(pidPath)
	if err != nil || retained != identity {
		t.Fatalf("publication failure lost exact identity: %v", err)
	}
}

func assertWebVNCCleanupReaped(t *testing.T, root string, record webVNCCleanupRecord) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, fmt.Sprintf("reaped-%d", record.Child))); err != nil {
		t.Fatalf("foreground child exited without reaping its real SSH tunnel: %v", err)
	}
	if current, err := webVNCDaemonProcessStartIdentity(record.Tunnel); err == nil && current == record.Started {
		t.Fatal("recorded SSH process survived foreground cleanup")
	}
}

func waitWebVNCCleanupSupervisor(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("supervisor did not exit within cleanup bound")
	}
}
