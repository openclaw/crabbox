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
	listener net.Listener
	entered  chan struct{}
	release  chan struct{}
	mu       sync.Mutex
	conns    []net.Conn
	wg       sync.WaitGroup
	allowed  map[uint32]bool
	hostKey  string
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
