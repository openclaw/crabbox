//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func boundaryPondInputs(t *testing.T, target SSHTarget) ([]pondMember, pondMeshSummary) {
	t.Helper()
	p, _ := strconv.Atoi(boundaryPort(t))
	q, _ := strconv.Atoi(boundaryPort(t))
	for q == p {
		q, _ = strconv.Atoi(boundaryPort(t))
	}
	return []pondMember{{Lease: "fixture", SSH: target}}, pondMeshSummary{Forwards: []pondMeshForward{{Peer: "peer", LeaseID: "fixture", LocalPort: p, RemotePort: 8080}, {Peer: "peer", LeaseID: "fixture", LocalPort: q, RemotePort: 8081}}}
}

func TestSSHForwardDetachedStartupBoundaries(t *testing.T) {
	for _, owner := range []string{"vnc", "pond"} {
		for _, mode := range []string{"gate", "unready", "descendant", "incomplete", "exit"} {
			if owner == "vnc" && mode == "incomplete" {
				continue
			}
			t.Run(owner+"/"+mode, func(t *testing.T) {
				fixtureMode := mode
				if mode == "exit" {
					fixtureMode = "unready"
				}
				f := newForwardBoundaryFixture(t, false, fixtureMode)
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(context.Canceled)
				result := make(chan error, 1)
				members, summary := boundaryPondInputs(t, f.target)
				port := strconv.Itoa(summary.Forwards[0].LocalPort)
				go func() {
					if owner == "vnc" {
						_, err := startVNCTunnel(ctx, f.target, port, "127.0.0.1", "5900")
						result <- err
					} else {
						result <- startPondMeshDaemons(ctx, pondConnectOptions{HomeDir: f.home, Stderr: io.Discard}, "startup", members, summary)
					}
				}()
				r := f.waitRecord(t)
				f.assertAliveBoundary(t, r)
				if mode == "descendant" {
					boundaryEventually(t, "descendant listener", func() bool { _, err := os.Stat(filepath.Join(f.root, "descendant.json")); return err == nil })
					if err := controllerVerifyDaemonOwnedListener(port, r.PID); err != nil {
						t.Fatalf("fixture descendant not in tree: %v", err)
					}
					if err := sshForwardRootListenerReady(port, r.PID); err == nil {
						t.Fatal("descendant passed exact-root check")
					}
				}
				select {
				case err := <-result:
					t.Fatalf("incomplete authentication/listeners published: %v", err)
				case <-time.After(350 * time.Millisecond):
				}
				f.assertAliveBoundary(t, r)
				if owner == "pond" {
					path, _ := pondMeshDaemonStatePath(f.home, "startup", false)
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatal("state published before readiness")
					}
				}
				cancelErr := errors.New("synthetic readiness cancellation")
				switch mode {
				case "gate":
					if err := os.WriteFile(filepath.Join(f.root, "authenticate"), nil, 0600); err != nil {
						t.Fatal(err)
					}
				case "exit":
					f.exit(t)
				default:
					cancel(cancelErr)
				}
				err := boundaryResult(t, result)
				if mode == "gate" {
					if err != nil {
						t.Fatal(err)
					}
					f.assertDetachedBoundary(t, r)
					if _, err := os.Stat(filepath.Join(f.root, "deleted-before-ready.json")); !os.IsNotExist(err) {
						t.Fatal("config removed before authentication gate")
					}
					_ = syscall.Kill(-r.PID, syscall.SIGKILL)
					f.assertReapedBoundary(t, r, true)
				} else {
					if mode == "exit" {
						var exitErr *exec.ExitError
						if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
							t.Fatalf("natural failure lost: %v", err)
						}
						if strings.Contains(err.Error(), forwardBoundaryUser) {
							t.Fatal("diagnostic leaked synthetic user")
						}
						if owner == "vnc" {
							var cliErr ExitError
							if !AsExitError(err, &cliErr) || cliErr.Code != 5 {
								t.Fatalf("VNC startup failure lost exit code 5: %v", err)
							}
						}
					} else if !errors.Is(err, cancelErr) {
						t.Fatalf("cancellation cause lost: %v", err)
					}
					f.assertReapedBoundary(t, r)
					if mode == "descendant" {
						data, _ := os.ReadFile(filepath.Join(f.root, "descendant.json"))
						var child forwardBoundaryRecord
						_ = json.Unmarshal(data, &child)
						boundaryEventually(t, "descendant terminated", func() bool { return !webVNCDaemonProcessGroupAlive(r.PID) })
						if child.PID == 0 {
							t.Fatal("missing descendant proof")
						}
					}
				}
			})
		}
	}
}

func TestSSHForwardPartialStartupCleanup(t *testing.T) {
	for _, owner := range []string{"attached-pond", "detached-pond"} {
		for _, failure := range []string{"session", "start"} {
			t.Run(owner+"/"+failure, func(t *testing.T) {
				f := newForwardBoundaryFixture(t, false, "unready")
				// The second member's route probe waits for the first recorder.
				// Do not fall back to the system ssh after removing our symlink.
				t.Setenv("PATH", filepath.Join(f.root, "bin"))
				config := "Host *\n IdentityFile none\n CertificateFile none\n IdentityAgent none\n"
				if err := os.WriteFile(filepath.Join(f.home, ".ssh", "config"), []byte(config), 0600); err != nil {
					t.Fatal(err)
				}
				members, summary := boundaryPondInputs(t, f.target)
				other := f.target
				other.SSHConfigProxy = true
				other.Key = filepath.Join(f.root, "identity")
				other.CertificateFile = filepath.Join(f.root, "certificate.pub")
				other.ChildEnv = map[string]string{"CRABBOX_FORWARD_BOUNDARY_MODE": "partial-" + failure}
				if failure == "session" {
					other.Key = "invalid\nkey"
				}
				members = append(members, pondMember{Lease: "second", SSH: other})
				summary.Forwards = append(summary.Forwards, pondMeshForward{Peer: "second", LeaseID: "second", LocalPort: summary.Forwards[0].LocalPort + 1, RemotePort: 8082})
				var err error
				if owner == "attached-pond" {
					err = runPondMeshForwards(context.Background(), pondConnectOptions{Stderr: io.Discard, Runner: pondMeshExecRunner{}}, members, summary)
				} else {
					err = startPondMeshDaemons(context.Background(), pondConnectOptions{HomeDir: f.home, Stderr: io.Discard}, "partial", members, summary)
				}
				if err == nil {
					t.Fatal("partial startup failure hidden")
				}
				configs, _ := filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), "crabbox-ssh-transport-*"))
				if len(configs) != 0 {
					t.Fatal("partial startup retained config")
				}
				r := f.waitRecord(t)
				f.assertReapedBoundary(t, r)
			})
		}
	}
}

func TestPondSecretBoundaryPublicationFailure(t *testing.T) {
	f := newForwardBoundaryFixture(t, false, "gate")
	members, summary := boundaryPondInputs(t, f.target)
	result := make(chan error, 1)
	go func() {
		result <- startPondMeshDaemons(context.Background(), pondConnectOptions{HomeDir: f.home, Stderr: io.Discard}, "publication", members, summary)
	}()
	r := f.waitRecord(t)
	path, err := pondMeshDaemonStatePath(f.home, "publication", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "authenticate"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := boundaryResult(t, result); err == nil {
		t.Fatal("state publication failure hidden")
	}
	f.assertReapedBoundary(t, r, true)
}

func TestVNCSecretBoundaryLeaderExitWithDescendant(t *testing.T) {
	f := newForwardBoundaryFixture(t, false, "leader-exit")
	tunnel, err := startVNCForegroundTunnel(context.Background(), f.target, boundaryPort(t), "127.0.0.1", "5900")
	if err != nil {
		t.Fatal(err)
	}
	defer stopProcess(tunnel)
	r := f.waitRecord(t)
	f.assertAliveBoundary(t, r)
	f.exit(t)
	select {
	case <-tunnel.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("leader exit not propagated")
	}
	var exitErr *exec.ExitError
	err = tunnel.ExitError()
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("exit provenance: %v", err)
	}
	if strings.Contains(err.Error(), forwardBoundaryUser) {
		t.Fatal("exit diagnostic leaked synthetic user")
	}
	if webVNCDaemonProcessGroupAlive(r.PID) {
		t.Fatal("proxy descendant survived Done")
	}
	f.assertReapedBoundary(t, r)
}

func TestVNCSecretBoundaryStartFailureAndDisplay(t *testing.T) {
	f := newForwardBoundaryFixture(t, false, "ready")
	f.target.ProxyCommand = "provider-proxy " + forwardBoundaryUser
	for _, command := range []string{vncTunnelCommand(f.target, "5901"), vncTunnelCommandTo(f.target, "5901", "127.0.0.1", "6099")} {
		if strings.Contains(command, forwardBoundaryUser) || !strings.Contains(command, "<token>") || !strings.Contains(command, "<provider-proxy>") {
			t.Fatal("VNC and WebVNC displays must redact username and provider proxy")
		}
	}
	paths, _ := filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), "crabbox-ssh-*"))
	if len(paths) != 0 {
		t.Fatal("display allocated config")
	}
	f.target.ProxyCommand = ""
	f.target.ChildEnv = map[string]string{"TEST_INVALID_ENV": "contains\x00nul"}
	for _, detached := range []bool{false, true} {
		var err error
		if detached {
			_, err = startVNCTunnel(context.Background(), f.target, boundaryPort(t), "127.0.0.1", "5900")
		} else {
			_, err = startVNCForegroundTunnel(context.Background(), f.target, boundaryPort(t), "127.0.0.1", "5900")
		}
		if err == nil {
			t.Fatal("Start failure hidden")
		}
		paths, _ := filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), "crabbox-ssh-*"))
		if len(paths) != 0 {
			t.Fatal("Start failure retained config")
		}
	}
}

func TestPondSecretBoundaryChildEnvironment(t *testing.T) {
	f := newForwardBoundaryFixture(t, false, "ready")
	f.target.ChildEnv = map[string]string{"TEST_FORWARD_OVERRIDE": "target-value"}
	args, session, err := pondMeshForwardInvocation(context.Background(), pondMeshForwardGroup{Target: f.target})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := pondMeshRunnerCommand(ctx, pondMeshExecRunner{}, f.target, directSSHExecutable(), args...).(*pondMeshExecHandle)
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	wait := startSSHForwardWait(h.Wait)
	defer func() { cancel(); <-wait.done }()
	r := f.waitRecord(t)
	if r.EnvSecret || !r.OverrideOK {
		t.Fatal("SSH lost filtered/overridden environment")
	}
	// Inspect only the exact fixture anchor's constructed environment, never
	// dump an ambient process environment into the test output.
	joined := strings.Join(h.platform.anchor.Env, "\n")
	if strings.Contains(joined, forwardBoundaryPassword) || !strings.Contains(joined, "TEST_FORWARD_OVERRIDE=target-value") {
		t.Fatal("anchor lost target environment policy")
	}
	if !strings.Contains(r.Resolved["connectionattempts"][0], "3") {
		t.Fatal("pond attempts changed")
	}
}

func TestSSHForwardDetachedCleanupFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission failure needs an unprivileged fixture user")
	}
	for _, owner := range []string{"vnc", "pond"} {
		t.Run(owner, func(t *testing.T) {
			f := newForwardBoundaryFixture(t, false, "gate")
			members, summary := boundaryPondInputs(t, f.target)
			result := make(chan error, 1)
			go func() {
				if owner == "vnc" {
					_, err := startVNCTunnel(context.Background(), f.target, strconv.Itoa(summary.Forwards[0].LocalPort), "127.0.0.1", "5900")
					result <- err
				} else {
					result <- startPondMeshDaemons(context.Background(), pondConnectOptions{HomeDir: f.home, Stderr: io.Discard}, "close-failure", members, summary)
				}
			}()
			r := f.waitRecord(t)
			dir := filepath.Dir(r.ConfigPath)
			if err := os.Chmod(dir, 0500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
			if err := os.WriteFile(filepath.Join(f.root, "authenticate"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			err := boundaryResult(t, result)
			if err == nil || !strings.Contains(err.Error(), "remove private SSH transport config") {
				t.Fatalf("config cleanup failure hidden: %v", err)
			}
			boundaryEventually(t, "failed detachment reaped SSH", func() bool { return syscall.Kill(r.PID, 0) == syscall.ESRCH })
			if owner == "pond" {
				path, _ := pondMeshDaemonStatePath(f.home, "close-failure", false)
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("state published despite cleanup failure")
				}
			}
		})
	}
}

func TestSSHForwardRootReadinessTimeout(t *testing.T) {
	f := newForwardBoundaryFixture(t, false, "unready")
	args, session, err := vncTunnelInvocation(context.Background(), f.target, boundaryPort(t), "127.0.0.1", "5900")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	cmd := exec.Command(directSSHExecutable(), args...)
	applyTargetChildEnvironment(cmd, f.target)
	configureDaemonCommand(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	wait := startSSHForwardWait(cmd.Wait)
	defer func() { _ = stopDaemonProcess(cmd.Process, cmd.Process.Pid); <-wait.done }()
	r := f.waitRecord(t)
	port := strings.Split(r.Forwards[0], ":")[1]
	err = waitSSHForwardRoots(context.Background(), []sshForwardRoot{{pid: r.PID, ports: []string{port}, wait: wait}}, 150*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("readiness timeout: %v", err)
	}
	f.assertAliveBoundary(t, r)
}
