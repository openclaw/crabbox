//go:build darwin || linux

package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func init() {
	owner := os.Getenv("CRABBOX_FORWARD_REAL_PARENT")
	if owner == "" || len(os.Args) != 2 || os.Args[1] != "--forward-real-parent" {
		return
	}
	root := os.Getenv("CRABBOX_FORWARD_BOUNDARY_ROOT")
	hostKey, err := os.ReadFile(filepath.Join(root, "hostkey.pub"))
	if err != nil {
		os.Exit(2)
	}
	target := SSHTarget{Host: "127.0.0.1", Port: os.Getenv("CRABBOX_FORWARD_REAL_SSH_PORT"), User: forwardBoundaryUser, AuthSecret: true,
		SSHHostKey: string(hostKey), KnownHostsFile: filepath.Join(root, "known_hosts"), ChildEnvDenylist: []string{"TEST_FORWARD_DESKTOP_PASSWORD"}}
	if os.Getenv("CRABBOX_FORWARD_REAL_CONFIG_ROUTE") == "1" {
		target.Host, target.SSHConfigProxy = "fixture-route", true
	}
	port := os.Getenv("CRABBOX_FORWARD_REAL_LOCAL_PORT")
	remote := os.Getenv("CRABBOX_FORWARD_REAL_REMOTE_PORT")
	pid := 0
	if owner == "vnc" {
		pid, err = startVNCTunnel(context.Background(), target, port, "127.0.0.1", remote)
	} else {
		local, _ := strconv.Atoi(port)
		remotePort, _ := strconv.Atoi(remote)
		second, _ := strconv.Atoi(os.Getenv("CRABBOX_FORWARD_REAL_SECOND_PORT"))
		summary := pondMeshSummary{Forwards: []pondMeshForward{{Peer: "peer", LeaseID: "fixture", LocalPort: local, RemotePort: remotePort}, {Peer: "peer", LeaseID: "fixture", LocalPort: second, RemotePort: remotePort}}}
		err = startPondMeshDaemons(context.Background(), pondConnectOptions{HomeDir: os.Getenv("HOME"), Stderr: io.Discard}, "real-proof", []pondMember{{Lease: "fixture", SSH: target}}, summary)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(pid)
	os.Exit(0)
}

// This fixture authenticates the real OpenSSH executable, with no sshd,
// account keys, agent, external destinations, or subprocess argv wrapper.
// All direct-tcpip requests must name a fixture-owned loopback endpoint.
type forwardSSHServer struct {
	listener       net.Listener
	entered        chan struct{}
	release        chan struct{}
	mu             sync.Mutex
	conns          []net.Conn
	users          []string
	wg             sync.WaitGroup
	allowed        map[uint32]bool
	hostKey        string
	forwarded      int
	sessionHandler func(ssh.NewChannel, string)
}

func newForwardSSHServer(t *testing.T, user string, allowedPorts ...int) *forwardSSHServer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return newForwardSSHServerWithSigner(t, user, signer, allowedPorts...)
}

func newForwardSSHServerWithSigner(t *testing.T, user string, signer ssh.Signer, allowedPorts ...int) *forwardSSHServer {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &forwardSSHServer{listener: listener, entered: make(chan struct{}), release: make(chan struct{}), allowed: map[uint32]bool{}, hostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))}
	for _, port := range allowedPorts {
		s.allowed[uint32(port)] = true
	}
	var once sync.Once
	cfg := &ssh.ServerConfig{NoClientAuth: true, NoClientAuthCallback: func(meta ssh.ConnMetadata) (*ssh.Permissions, error) {
		s.mu.Lock()
		s.users = append(s.users, meta.User())
		s.mu.Unlock()
		if meta.User() != user {
			return nil, fmt.Errorf("unexpected synthetic SSH user")
		}
		once.Do(func() { close(s.entered) })
		<-s.release
		return nil, nil
	}}
	cfg.AddHostKey(signer)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, conn)
			s.mu.Unlock()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer conn.Close()
				transport, channels, requests, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer transport.Close()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					s.mu.Lock()
					handleSession := s.sessionHandler
					s.mu.Unlock()
					if incoming.ChannelType() == "session" && handleSession != nil {
						s.wg.Add(1)
						go func() {
							defer s.wg.Done()
							handleSession(incoming, transport.User())
						}()
						continue
					}
					var dest struct {
						Host       string
						Port       uint32
						OriginHost string
						OriginPort uint32
					}
					if incoming.ChannelType() != "direct-tcpip" || ssh.Unmarshal(incoming.ExtraData(), &dest) != nil || dest.Host != "127.0.0.1" || !s.allowed[dest.Port] {
						_ = incoming.Reject(ssh.Prohibited, "fixture destination rejected")
						continue
					}
					upstream, err := net.DialTimeout("tcp4", net.JoinHostPort(dest.Host, strconv.Itoa(int(dest.Port))), time.Second)
					if err != nil {
						_ = incoming.Reject(ssh.ConnectionFailed, "fixture unavailable")
						continue
					}
					ch, reqs, err := incoming.Accept()
					if err != nil {
						upstream.Close()
						continue
					}
					go ssh.DiscardRequests(reqs)
					s.mu.Lock()
					s.forwarded++
					s.mu.Unlock()
					s.wg.Add(1)
					go func() {
						defer s.wg.Done()
						defer upstream.Close()
						defer ch.Close()
						copied := make(chan struct{})
						go func() { _, _ = io.Copy(upstream, ch); _ = upstream.(*net.TCPConn).CloseWrite(); close(copied) }()
						_, _ = io.Copy(ch, upstream)
						_ = ch.CloseWrite()
						_ = ch.Close()
						<-copied
					}()
				}
			}()
		}
	}()
	t.Cleanup(func() {
		select {
		case <-s.release:
		default:
			close(s.release)
		}
		listener.Close()
		s.mu.Lock()
		for _, conn := range s.conns {
			conn.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s
}

func (s *forwardSSHServer) port() int { return s.listener.Addr().(*net.TCPAddr).Port }

func TestWaitForSSHReadyRejectsChangedHostKeyWithoutWaiting(t *testing.T) {
	isolateTestUserDirs(t)
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH unavailable")
	}
	for _, proxy := range []bool{false, true} {
		t.Run(fmt.Sprintf("proxy=%t", proxy), func(t *testing.T) {
			server := newForwardSSHServer(t, "synthetic-host-trust")
			close(server.release)
			oldServer := newForwardSSHServer(t, "synthetic-host-trust")
			close(oldServer.release)
			knownHosts := filepath.Join(t.TempDir(), "known_hosts")
			pin := fmt.Sprintf("[127.0.0.1]:%d %s\n", server.port(), oldServer.hostKey)
			if err := os.WriteFile(knownHosts, []byte(pin), 0o600); err != nil {
				t.Fatal(err)
			}
			target := SSHTarget{User: "synthetic-host-trust", Host: "127.0.0.1", Port: strconv.Itoa(server.port()),
				KnownHostsFile: knownHosts, NoControlMaster: true, FallbackPorts: []string{}, SSHConfigProxy: proxy}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			var progress strings.Builder
			err := waitForSSHReady(ctx, &target, &progress, "test", time.Minute)
			if err == nil || !strings.Contains(err.Error(), "SSH host-key verification failed") || ctx.Err() != nil {
				t.Fatalf("expected immediate host-key rejection, got %v (context=%v progress=%q)", err, ctx.Err(), progress.String())
			}
			if progress.Len() != 0 {
				t.Fatalf("permanent failure entered readiness backoff: %s", progress.String())
			}
			after, err := os.ReadFile(knownHosts)
			if err != nil || string(after) != pin {
				t.Fatalf("host trust changed: %q, %v", after, err)
			}
		})
	}
}

func writeForwardSSHHostKeys(t *testing.T, f forwardBoundaryFixture, servers ...*forwardSSHServer) {
	t.Helper()
	var knownHosts strings.Builder
	for _, server := range servers {
		fmt.Fprintf(&knownHosts, "[127.0.0.1]:%d %s\n", server.port(), server.hostKey)
	}
	if err := os.WriteFile(filepath.Join(f.root, "known_hosts"), []byte(knownHosts.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "hostkey.pub"), []byte(servers[0].hostKey), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorReleaseJoinsSSHControlMasters(t *testing.T) {
	testCoordinatorReleaseJoinsSSHControlMasters(t, "coordinator", "configured HostName", "configured ProxyJump", "partial local failure", "stale local socket", "direct artifact owner")
}

func testCoordinatorReleaseJoinsSSHControlMasters(t *testing.T, modes ...string) {
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			isolateTestUserDirs(t)
			sshExecutable, err := exec.LookPath("ssh")
			if err != nil {
				t.Skip("OpenSSH is unavailable")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			t.Cleanup(cancel)
			server := newForwardSSHServer(t, "synthetic-mux-owner")
			rotated := newForwardSSHServer(t, "synthetic-mux-owner")
			close(server.release)
			close(rotated.release)
			const releasedID = "cbx_001122334455"
			const companionID = "cbx_66778899aabb"
			type masterIdentity struct {
				target  SSHTarget
				path    string
				native  string
				pid     int
				started string
			}
			control := func(path, operation string) (string, error) {
				command := exec.CommandContext(ctx, sshExecutable, "-F", os.DevNull, "-S", path, "-O", operation, "--", "localhost")
				command.WaitDelay = sshCommandWaitDelay
				output, err := command.CombinedOutput()
				return strings.TrimSpace(string(output)), err
			}
			start := func(leaseID string, endpoint *forwardSSHServer, route string) masterIdentity {
				t.Helper()
				key, _, err := EnsureTestboxKey(leaseID)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := RemoveStoredTestboxConnectionArtifacts(leaseID); err != nil {
						t.Errorf("cleanup synthetic lease: %v", err)
					}
				})
				target := SSHTarget{User: "synthetic-mux-owner", Host: "127.0.0.1", Port: strconv.Itoa(endpoint.port()),
					Key: key, SSHHostKey: endpoint.hostKey, TargetOS: targetLinux}
				config := os.DevNull
				if route != "" {
					target.Host = "fixture-route"
					config = filepath.Join(t.TempDir(), "ssh_config")
					if err := os.WriteFile(config, []byte(route), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := prepareLeaseSSHTrust(&target, leaseID); err != nil {
					t.Fatal(err)
				}
				args := append([]string{"-F", config}, sshBaseArgs(target)...)
				args = append(args, target.User+"@"+target.Host)
				// Keep the installed client's %C semantics; ProxyJump joins that hash
				// only in OpenSSH 9.6+. Cleanup must not reread the original config.
				output, err := exec.CommandContext(ctx, sshExecutable, append([]string{"-G"}, args...)...).Output()
				if err != nil {
					t.Fatal(err)
				}
				identity := masterIdentity{target: target}
				for _, line := range strings.Split(string(output), "\n") {
					if value, ok := strings.CutPrefix(line, "controlpath "); ok {
						identity.path = value
					}
				}
				if identity.path == "" || len(identity.path)+18 > 104 {
					t.Fatalf("control path does not fit Darwin listener, including temporary suffix and NUL: %q", identity.path)
				}
				output, err = exec.CommandContext(ctx, sshExecutable, append([]string{"-G", "-o", "ControlPath=%C"}, args...)...).Output()
				if err != nil {
					t.Fatal(err)
				}
				for _, line := range strings.Split(string(output), "\n") {
					if value, ok := strings.CutPrefix(line, "controlpath "); ok {
						identity.native = value
					}
				}
				if identity.native == "" || !strings.HasSuffix(identity.path, "-"+identity.native) {
					t.Fatal("lease scope did not preserve the installed client's native connection hash")
				}
				command := sshCommandContext(ctx, target, append([]string{"-fN"}, args...)...)
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("start real local SSH master: %v: %s", err, output)
				}
				t.Cleanup(func() {
					_, _ = control(identity.path, "exit")
				})
				checked, err := control(identity.path, "check")
				if err != nil {
					t.Fatalf("master did not persist: %v: %s", err, checked)
				}
				if _, err := fmt.Sscanf(checked, "Master running (pid=%d)", &identity.pid); err != nil {
					t.Fatal(err)
				}
				identity.started, err = LocalProcessStartIdentity(identity.pid)
				if err != nil {
					t.Fatal(err)
				}
				return identity
			}
			var firstRoute, secondRoute string
			if mode == "configured HostName" {
				firstRoute = "Host fixture-route\n HostName 127.0.0.1\n"
				secondRoute = "Host fixture-route\n HostName localhost\n"
				rotated = server
			}
			if mode == "configured ProxyJump" {
				jump := newForwardSSHServer(t, "synthetic-jump", server.port())
				close(jump.release)
				jumpConfig := fmt.Sprintf("Host jump-one jump-two\n HostName 127.0.0.1\n Port %d\n User synthetic-jump\n IdentityFile none\n CertificateFile none\n IdentityAgent none\n IdentitiesOnly yes\n StrictHostKeyChecking no\n UserKnownHostsFile /dev/null\n GlobalKnownHostsFile none\n LogLevel ERROR\n", jump.port())
				firstRoute = "Host fixture-route\n HostName 127.0.0.1\n ProxyJump jump-one\n" + jumpConfig
				secondRoute = "Host fixture-route\n HostName 127.0.0.1\n ProxyJump jump-two\n" + jumpConfig
				rotated = server
			}
			first := start(releasedID, server, firstRoute)
			firstPID, err := control(first.path, "check")
			if err != nil {
				t.Fatal(err)
			}
			start(releasedID, server, firstRoute)
			if reused, err := control(first.path, "check"); err != nil || reused != firstPID {
				t.Fatalf("active lease did not reuse its master: before=%q after=%q err=%v", firstPID, reused, err)
			}
			second := start(releasedID, rotated, secondRoute)
			if mode == "configured ProxyJump" && first.native == second.native {
				if first.path != second.path || first.pid != second.pid {
					t.Fatal("unchanged native connection hash did not reuse its master")
				}
				t.Log("installed OpenSSH does not include ProxyJump in %C; existing sharing semantics preserved")
			} else if first.path == second.path || first.pid == second.pid {
				t.Fatal("different endpoint or effective route reused the previous master")
			}
			companion := start(companionID, server, "")
			companionBefore, err := control(companion.path, "check")
			if err != nil {
				t.Fatal(err)
			}
			if err := ClaimLeaseTargetForConfig(releasedID, "mux-release", Config{Provider: "aws"}, Server{Provider: "aws"}, second.target, time.Hour); err != nil {
				t.Fatal(err)
			}
			broker := coordinatorReleaseTestServer(t, func() CoordinatorLease {
				return CoordinatorLease{
					ID: releasedID, Provider: "aws", State: "released", CleanupStatus: "complete",
					CleanupCompletedAt: "2026-09-06T00:00:00Z",
				}
			})
			var blocked *net.UnixListener
			if mode == "partial local failure" || mode == "stale local socket" {
				blocked, err = net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(filepath.Dir(second.path), "00000000000000000000000000000000"), Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = blocked.Close() })
				if err := os.Chmod(blocked.Addr().String(), 0o600); err != nil {
					t.Fatal(err)
				}
				if mode == "stale local socket" {
					blocked.SetUnlinkOnClose(false)
					if err := blocked.Close(); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Remove(blocked.Addr().String()) })
				}
			}
			if mode == "direct artifact owner" {
				err = RemoveStoredTestboxConnectionArtifacts(releasedID)
			} else {
				var outcome ReleaseLeaseOutcome
				outcome, err = coordinatorReleaseTestBackend(broker, io.Discard).ReleaseLeaseWithOutcome(ctx, ReleaseLeaseRequest{
					Lease: LeaseTarget{LeaseID: releasedID, Server: Server{Provider: "aws"}, SSH: second.target},
				})
				if !outcome.Terminal {
					t.Fatal("confirmed remote deletion was lost")
				}
			}
			if mode == "partial local failure" {
				if err == nil || !strings.Contains(err.Error(), "remote deletion is confirmed") {
					t.Errorf("unresponsive endpoint cleanup error=%v", err)
				}
				if _, exists, err := ReadLeaseClaimWithPresence(releasedID); err != nil || !exists {
					t.Errorf("local cleanup debt lost its claim: exists=%t err=%v", exists, err)
				}
			} else if err != nil {
				states, _ := exec.Command("ps", "-o", "pid=,ppid=,stat=,comm=", "-p", fmt.Sprintf("%d,%d", first.pid, second.pid)).Output()
				t.Logf("exact native master states at cleanup failure:\n%s", states)
				t.Fatal(err)
			}
			for _, identity := range []masterIdentity{first, second} {
				if output, err := control(identity.path, "check"); err == nil {
					t.Errorf("confirmed lease release left a real SSH master alive: %s", output)
				}
				current, err := LocalProcessStartIdentity(identity.pid)
				state, stateErr := exec.CommandContext(ctx, "ps", "-o", "stat=", "-p", strconv.Itoa(identity.pid)).Output()
				zombie := stateErr == nil && strings.HasPrefix(strings.TrimSpace(string(state)), "Z")
				if mode == "unreaped native masters" && !zombie {
					t.Errorf("fixture did not retain the exited master for its parent to reap: pid=%d state=%q err=%v", identity.pid, state, stateErr)
				}
				if err == nil && current == identity.started && !zombie || err != nil && !errors.Is(syscall.Kill(identity.pid, 0), syscall.ESRCH) {
					t.Errorf("release returned before exact master exited: pid=%d started=%s current=%s err=%v", identity.pid, identity.started, current, err)
				} else {
					t.Logf("released master pid=%d started=%s is absent or exited (zombie=%t); exit status not observed", identity.pid, identity.started, zombie)
				}
			}
			if after, err := control(companion.path, "check"); err != nil || after != companionBefore {
				t.Fatalf("companion lease master changed: before=%q after=%q err=%v", companionBefore, after, err)
			}
		})
	}
}

func TestSSHControlCleanupRejectsMalformedMuxWithoutNetworkFallback(t *testing.T) {
	isolateTestUserDirs(t)
	sshExecutable, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH is unavailable")
	}
	key, _, err := EnsureTestboxKey("cbx_001122334455")
	if err != nil {
		t.Fatal(err)
	}
	target := SSHTarget{Key: key}
	if err := ensureSSHControlDirectory(target); err != nil {
		t.Fatal(err)
	}
	dir := sshControlDirectory(filepath.Dir(key))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "malformed")
	mux, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	muxDone := make(chan struct{})
	go func() {
		defer close(muxDone)
		for {
			conn, err := mux.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			var hello [12]byte
			if _, err := io.ReadFull(conn, hello[:]); err == nil {
				// MUX_MSG_HELLO with unsupported version zero.
				_, _ = conn.Write([]byte{0, 0, 0, 8, 0, 0, 0, 1, 0, 0, 0, 0})
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = mux.Close(); <-muxDone })
	fallback, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	connected := make(chan struct{}, 1)
	fallbackDone := make(chan struct{})
	go func() {
		defer close(fallbackDone)
		conn, err := fallback.Accept()
		if err == nil {
			connected <- struct{}{}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = fallback.Close(); <-fallbackDone })
	root := t.TempDir()
	config := filepath.Join(root, "effective-config")
	// The real -G probe checks production options before any safety overrides.
	// The execution-only shim confines a regressing client's TCP fallback to our
	// listener and prevents it from reading the operator's default identities.
	wrapper := "#!/bin/sh\n" + shellQuote(sshExecutable) + " -G \"$@\" > " + shellQuote(config) + " || exit $?\nexec " + shellQuote(sshExecutable) +
		" -p " + strconv.Itoa(fallback.Addr().(*net.TCPAddr).Port) + " -o HostName=127.0.0.1 -o IdentityFile=none -o CertificateFile=none -o IdentityAgent=none \"$@\"\n"
	if err := os.WriteFile(filepath.Join(root, "ssh"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := closeSSHControlMaster(ctx, socket); err == nil {
		t.Error("malformed live mux endpoint was accepted as cleaned up")
	}
	if _, err := os.Lstat(socket); err != nil {
		t.Errorf("failed endpoint lost its cleanup identity: %v", err)
	}
	_ = fallback.Close()
	<-fallbackDone
	select {
	case <-connected:
		t.Error("malformed mux handshake fell back to a TCP connection")
	default:
	}
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range []string{"identityfile none", "certificatefile none", "identityagent none"} {
		if !strings.Contains(string(data), "\n"+setting+"\n") {
			t.Errorf("production control command did not disable %s", strings.Fields(setting)[0])
		}
	}
}

func TestSSHCommandRejectsUnsafeLeaseControlNamespace(t *testing.T) {
	for _, kind := range []string{"symlink", "public directory"} {
		t.Run(kind, func(t *testing.T) {
			isolateTestUserDirs(t)
			key, _, err := EnsureTestboxKey("cbx_001122334455")
			if err != nil {
				t.Fatal(err)
			}
			dir := sshControlDirectory(filepath.Dir(key))
			if kind == "symlink" {
				err = os.Symlink(t.TempDir(), dir)
			} else {
				err = os.Mkdir(dir, 0o700)
				if err == nil {
					err = os.Chmod(dir, 0o777)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(dir) })
			cmd := sshCommandContext(t.Context(), SSHTarget{Key: key}, "-V")
			if err := cmd.Run(); err == nil {
				t.Fatal("unsafe control namespace did not prevent SSH execution")
			}
		})
	}
}

func forwardEchoServer(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func assertForwardPayload(t *testing.T, port string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", port), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	const payload = "synthetic payload after private configs unlink\n"
	if _, err = io.WriteString(conn, payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err = io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatal("forward payload changed")
	}
}

func TestSSHForwardRealDetachedAfterUnlink(t *testing.T) {
	for _, owner := range []string{"vnc", "pond", "vnc-parent", "pond-parent"} {
		for _, hops := range []int{0, 2} {
			t.Run(fmt.Sprintf("%s/hops=%d", owner, hops), func(t *testing.T) {
				f := newForwardBoundaryFixture(t, false, "ready")
				// Replace the recorder symlink with real OpenSSH BEFORE any invocation.
				sshPath := filepath.Join(f.root, "bin", "ssh")
				if err := os.Remove(sshPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/usr/bin/ssh", sshPath); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CRABBOX_FORWARD_BOUNDARY_FIXTURE", "")
				echoPort := forwardEchoServer(t)
				server := newForwardSSHServer(t, forwardBoundaryUser, echoPort)
				target := f.target
				target.Port = strconv.Itoa(server.port())
				target.SSHHostKey = server.hostKey
				writeForwardSSHHostKeys(t, f, server)
				if hops > 0 {
					second := newForwardSSHServer(t, "synthetic-jump-two", server.port())
					first := newForwardSSHServer(t, "synthetic-jump-one", second.port())
					close(first.release)
					close(second.release)
					target.Host = "fixture-route"
					target.SSHConfigProxy = true
					config := fmt.Sprintf("Host fixture-route\n HostName 127.0.0.1\n ProxyJump jump-one,jump-two\nHost jump-one\n HostName 127.0.0.1\n Port %d\n User synthetic-jump-one\nHost jump-two\n HostName 127.0.0.1\n Port %d\n User synthetic-jump-two\nHost *\n IdentityFile none\n CertificateFile none\n IdentityAgent none\n IdentitiesOnly yes\n StrictHostKeyChecking no\n UserKnownHostsFile /dev/null\n GlobalKnownHostsFile none\n LogLevel ERROR\n", first.port(), second.port())
					if err := os.WriteFile(filepath.Join(f.home, ".ssh", "config"), []byte(config), 0600); err != nil {
						t.Fatal(err)
					}
				}
				port := boundaryPort(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				type result struct {
					pid int
					err error
				}
				done := make(chan result, 1)
				if strings.HasSuffix(owner, "-parent") {
					executable, err := os.Executable()
					if err != nil {
						t.Fatal(err)
					}
					second := boundaryPort(t)
					parent := exec.Command(executable, "--forward-real-parent")
					parent.Env = append(os.Environ(), "CRABBOX_FORWARD_REAL_PARENT="+strings.TrimSuffix(owner, "-parent"),
						"CRABBOX_FORWARD_REAL_SSH_PORT="+target.Port, "CRABBOX_FORWARD_REAL_LOCAL_PORT="+port,
						"CRABBOX_FORWARD_REAL_REMOTE_PORT="+strconv.Itoa(echoPort), "CRABBOX_FORWARD_REAL_SECOND_PORT="+second)
					if hops > 0 {
						parent.Env = append(parent.Env, "CRABBOX_FORWARD_REAL_CONFIG_ROUTE=1")
					}
					go func() {
						out, err := parent.Output()
						var pid int
						if err == nil {
							err = json.Unmarshal(out, &pid)
						}
						done <- result{pid, err}
					}()
				} else if owner == "vnc" {
					go func() {
						pid, err := startVNCTunnel(ctx, target, port, "127.0.0.1", strconv.Itoa(echoPort))
						done <- result{pid, err}
					}()
				} else {
					local, _ := strconv.Atoi(port)
					secondPort, _ := strconv.Atoi(boundaryPort(t))
					summary := pondMeshSummary{Forwards: []pondMeshForward{{Peer: "peer", LeaseID: "fixture", LocalPort: local, RemotePort: echoPort}, {Peer: "peer", LeaseID: "fixture", LocalPort: secondPort, RemotePort: echoPort}}}
					go func() {
						err := startPondMeshDaemons(ctx, pondConnectOptions{HomeDir: f.home, Stderr: io.Discard}, "real-proof", []pondMember{{Lease: "fixture", SSH: target}}, summary)
						done <- result{err: err}
					}()
				}
				select {
				case <-server.entered:
				case got := <-done:
					t.Fatalf("startup exited before auth: %v", got.err)
				case <-time.After(10 * time.Second):
					t.Fatal("real SSH never reached authentication")
				}
				configs, _ := filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), "crabbox-ssh-transport-*", "ssh_config"))
				if len(configs) != 1 {
					t.Fatalf("private root configs before auth: %d", len(configs))
				}
				configDir := filepath.Dir(configs[0])
				paths, _ := filepath.Glob(filepath.Join(configDir, "*"))
				if hops > 0 && len(paths) < 3 {
					t.Fatal("generated multihop configs missing")
				}
				for _, path := range paths {
					info, err := os.Stat(path)
					if err != nil || info.Mode().Perm() != 0600 {
						t.Fatal("private config permissions")
					}
				}
				info, err := os.Stat(configDir)
				if err != nil || info.Mode().Perm() != 0700 {
					t.Fatal("private directory permissions")
				}
				// Deliberately outlast the old export grace while the real server blocks auth.
				select {
				case got := <-done:
					t.Fatalf("published before authentication: %v", got.err)
				case <-time.After(350 * time.Millisecond):
				}
				for _, path := range paths {
					if _, err := os.Stat(path); err != nil {
						t.Fatal("config removed during delayed authentication")
					}
				}
				close(server.release)
				var got result
				select {
				case got = <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("authenticated forward did not become ready")
				}
				if got.err != nil {
					t.Fatal(got.err)
				}
				pid := got.pid
				if strings.HasPrefix(owner, "pond") {
					statePath, _ := pondMeshDaemonStatePath(f.home, "real-proof", false)
					data, err := os.ReadFile(statePath)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(data), forwardBoundaryUser) {
						t.Fatal("state leaked username")
					}
					var state pondMeshDaemonState
					if err := json.Unmarshal(data, &state); err != nil {
						t.Fatal(err)
					}
					if len(state.PIDs) != 1 || len(state.Forwards) != 2 {
						t.Fatal("grouped state changed")
					}
					pid = state.PIDs[0]
					t.Cleanup(func() { _, _ = stopPondMeshDaemonState(f.home, "real-proof") })
					for _, forward := range state.Forwards {
						assertForwardPayload(t, strconv.Itoa(forward.LocalPort))
					}
				}
				t.Cleanup(func() {
					_ = syscall.Kill(-pid, syscall.SIGKILL)
					boundaryEventually(t, "real SSH reap", func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH })
				})
				if _, err := os.Stat(configDir); !os.IsNotExist(err) {
					t.Fatal("successful detached forward retained config")
				}
				cancel()
				assertForwardPayload(t, port)
				assertForwardPayload(t, port) // A new channel after unlink needs no config reread.
			})
		}
	}
}

func TestSSHForwardRealPondWaitsForEveryGroup(t *testing.T) {
	for _, ending := range []string{"success", "cancel"} {
		t.Run(ending, func(t *testing.T) {
			f := newForwardBoundaryFixture(t, false, "ready")
			sshPath := filepath.Join(f.root, "bin", "ssh")
			if err := os.Remove(sshPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/usr/bin/ssh", sshPath); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_FORWARD_BOUNDARY_FIXTURE", "")
			echo := forwardEchoServer(t)
			first := newForwardSSHServer(t, forwardBoundaryUser, echo)
			second := newForwardSSHServer(t, forwardBoundaryUser, echo)
			target := f.target
			target.Port = strconv.Itoa(first.port())
			target.SSHHostKey = first.hostKey
			writeForwardSSHHostKeys(t, f, first, second)
			members, summary := boundaryPondInputs(t, target)
			other := target
			other.Port = strconv.Itoa(second.port())
			other.SSHHostKey = second.hostKey
			members = append(members, pondMember{Lease: "second", SSH: other})
			local, _ := strconv.Atoi(boundaryPort(t))
			for local == summary.Forwards[0].LocalPort || local == summary.Forwards[1].LocalPort {
				local, _ = strconv.Atoi(boundaryPort(t))
			}
			summary.Forwards = append(summary.Forwards, pondMeshForward{Peer: "second", LeaseID: "second", LocalPort: local, RemotePort: echo})
			for i := range summary.Forwards {
				summary.Forwards[i].RemotePort = echo
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- startPondMeshDaemons(ctx, pondConnectOptions{HomeDir: f.home, Stderr: io.Discard}, "groups", members, summary)
			}()
			for _, server := range []*forwardSSHServer{first, second} {
				select {
				case <-server.entered:
				case err := <-done:
					t.Fatalf("startup failed: %v", err)
				case <-time.After(10 * time.Second):
					t.Fatal("group did not authenticate")
				}
			}
			close(first.release)
			// First member owns BOTH its listeners; second member is still in auth.
			port := strconv.Itoa(summary.Forwards[0].LocalPort)
			boundaryEventually(t, "first group ready", func() bool { _, err := localWebVNCListenerIdentity(port); return err == nil })
			identity, err := localWebVNCListenerIdentity(port)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				t.Fatalf("export ignored unready group: %v", err)
			case <-time.After(350 * time.Millisecond):
			}
			configs, _ := filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), "crabbox-ssh-transport-*"))
			if len(configs) != 2 {
				t.Fatal("sessions removed before all-group barrier")
			}
			if ending == "cancel" {
				cancel()
			} else {
				close(second.release)
			}
			err = boundaryResult(t, done)
			if ending == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel lost: %v", err)
				}
				boundaryEventually(t, "ready sibling reaped", func() bool { return syscall.Kill(identity.PID, 0) == syscall.ESRCH })
			} else {
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _, _ = stopPondMeshDaemonState(f.home, "groups") })
				for _, forward := range summary.Forwards {
					assertForwardPayload(t, strconv.Itoa(forward.LocalPort))
				}
			}
			configs, _ = filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), "crabbox-ssh-transport-*"))
			if len(configs) != 0 {
				t.Fatal("group sessions retained after barrier/teardown")
			}
		})
	}
}

const realSSHFailingReadiness = "exit 127"

type realSSHExecObservation struct {
	user    string
	command string
	status  uint32
}

// Only these fixture-owned shell builtins may execute. Never run the incoming
// command string, even after matching it, or inherit the runner's environment.
func (s *forwardSSHServer) enableReadinessSessions(t *testing.T, root string, transportSucceeds bool) (*sync.Mutex, *[]realSSHExecObservation) {
	t.Helper()
	var mu sync.Mutex
	var observed []realSSHExecObservation
	handler := func(incoming ssh.NewChannel, user string) {
		ch, requests, err := incoming.Accept()
		if err != nil {
			return
		}
		defer ch.Close()
		for request := range requests {
			var payload struct{ Command string }
			if request.Type != "exec" || ssh.Unmarshal(request.Payload, &payload) != nil {
				_ = request.Reply(false, nil)
				continue
			}
			var fixedCommand string
			switch payload.Command {
			case "exit 0":
				if transportSucceeds {
					fixedCommand = "exit 0"
				}
			case realSSHFailingReadiness:
				fixedCommand = realSSHFailingReadiness
			}
			if fixedCommand == "" {
				_ = request.Reply(false, nil)
				return
			}
			if err := request.Reply(true, nil); err != nil {
				return
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", fixedCommand)
			cmd.Dir = root
			cmd.Env = []string{"HOME=" + root, "XDG_CONFIG_HOME=" + root, "PATH=/usr/bin:/bin"}
			err = cmd.Run()
			cancel()
			var code uint32
			if err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() < 0 {
					t.Errorf("fixed readiness fixture command failed: %v", err)
					return
				}
				code = uint32(exitErr.ExitCode())
			}
			mu.Lock()
			observed = append(observed, realSSHExecObservation{user, fixedCommand, code})
			mu.Unlock()
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
			return
		}
	}
	s.mu.Lock()
	s.sessionHandler = handler
	s.mu.Unlock()
	return &mu, &observed
}

func TestWaitForSSHReadyRealSSHTimeoutDiagnostic(t *testing.T) {
	// Resolve before changing PATH: the client must be installed OpenSSH, not
	// a command recorder. Missing CI prerequisites are failures, not proof skips.
	sshExecutable, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal("real OpenSSH is required:", err)
	}
	for _, proxy := range []bool{false, true} {
		for _, transportSucceeds := range []bool{true, false} {
			t.Run(fmt.Sprintf("proxy=%t/transport-success=%t", proxy, transportSucceeds), func(t *testing.T) {
				dirs := isolateTestUserDirs(t)
				root := t.TempDir()
				bin := filepath.Join(root, "bin")
				for _, dir := range []string{bin, filepath.Join(dirs.Home, ".ssh")} {
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(sshExecutable, filepath.Join(bin, "ssh")); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+":/usr/bin:/bin")
				t.Setenv("SSH_AUTH_SOCK", "")
				t.Setenv("SSH_AGENT_PID", "")
				version, err := exec.Command(sshExecutable, "-V").CombinedOutput()
				if err != nil || !strings.Contains(string(version), "OpenSSH") {
					t.Fatalf("installed client is not OpenSSH: %q, %v", version, err)
				}
				t.Logf("client: %s", strings.TrimSpace(string(version)))
				const user = "synthetic-readiness"
				server := newForwardSSHServer(t, user)
				mu, observed := server.enableReadinessSessions(t, root, transportSucceeds)
				close(server.release)
				knownHosts := filepath.Join(root, "known_hosts")
				pins := fmt.Sprintf("[127.0.0.1]:%d %s\n", server.port(), server.hostKey)
				config := ""
				var jump *forwardSSHServer
				if proxy {
					jump = newForwardSSHServer(t, "synthetic-jump", server.port())
					close(jump.release)
					pins += fmt.Sprintf("[127.0.0.1]:%d %s\n", jump.port(), jump.hostKey)
					config = fmt.Sprintf("Host 127.0.0.1\n ProxyJump readiness-jump\nHost readiness-jump\n HostName 127.0.0.1\n Port %d\n User synthetic-jump\n", jump.port())
				}
				config += "Host *\n IdentityFile none\n CertificateFile none\n IdentityAgent none\n IdentitiesOnly yes\n BatchMode yes\n ControlMaster no\n ControlPath none\n ControlPersist no\n StrictHostKeyChecking yes\n GlobalKnownHostsFile none\n UserKnownHostsFile " + strconv.Quote(knownHosts) + "\n"
				if err := os.WriteFile(knownHosts, []byte(pins), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dirs.Home, ".ssh", "config"), []byte(config), 0o600); err != nil {
					t.Fatal(err)
				}
				// Select the private config path without supplying any credentials.
				target := SSHTarget{User: user, Host: "127.0.0.1", Port: strconv.Itoa(server.port()),
					KnownHostsFile: knownHosts, SSHHostKey: server.hostKey, NoControlMaster: true, AuthSecret: true,
					FallbackPorts: []string{}, SSHConfigProxy: proxy, ReadyCheck: realSSHFailingReadiness}
				closedFallback := ""
				if !proxy && transportSucceeds {
					listener, err := net.Listen("tcp4", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					closedFallback = strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
					if err := listener.Close(); err != nil {
						t.Fatal(err)
					}
					target.FallbackPorts = []string{closedFallback}
				}
				var progress strings.Builder
				err = waitForSSHReady(t.Context(), &target, &progress, "bootstrap", 8*time.Second)
				if err == nil {
					t.Fatal("failing readiness unexpectedly succeeded")
				}
				message := err.Error()
				t.Logf("timeout: %s; progress: %s", message, progress.String())
				wantAuth, wantProbe, wantPorts := "unknown", "transport", target.Port+":tcp"
				if proxy {
					wantPorts = "proxy"
				}
				if transportSucceeds {
					wantAuth, wantProbe = "ok", "readiness"
					wantPorts = target.Port + ":ready"
					if closedFallback != "" {
						wantPorts += "," + closedFallback + ":closed"
					}
					if proxy {
						wantPorts = "proxy:ready"
					}
				}
				for _, want := range []string{"cause=deadline_exceeded", "authentication=" + wantAuth, "probe=" + wantProbe, "ports=" + wantPorts + ";"} {
					if !strings.Contains(message, want) {
						t.Errorf("timeout missing %q: %s", want, message)
					}
				}
				mu.Lock()
				evidence := append([]realSSHExecObservation(nil), (*observed)...)
				mu.Unlock()
				ready, transport := false, false
				for _, observation := range evidence {
					if observation.user != user {
						t.Errorf("unexpected authenticated user: %q", observation.user)
					}
					ready = ready || observation.command == realSSHFailingReadiness && observation.status == 127
					transport = transport || observation.command == "exit 0" && observation.status == 0
				}
				if !ready || transport != transportSucceeds {
					t.Fatalf("missing real authenticated execution evidence: %+v", evidence)
				}
				t.Logf("authenticated executions: %+v", evidence)
				if jump != nil {
					jump.mu.Lock()
					forwarded := jump.forwarded
					users := append([]string(nil), jump.users...)
					jump.mu.Unlock()
					if forwarded < 2 || len(users) < 2 {
						t.Fatalf("readiness and transport did not use real jump sessions: forwards=%d users=%v", forwarded, users)
					}
					t.Logf("real proxy forwards=%d authenticated users=%v", forwarded, users)
				}
			})
		}
	}
}
