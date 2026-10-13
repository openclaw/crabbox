package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func jumpTestSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}
func jumpProbeServer(t *testing.T, signer ssh.Signer, stall bool) (SSHJumpCandidate, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	calls := new(atomic.Int32)
	done := make(chan struct{})
	t.Cleanup(func() { listener.Close(); close(done) })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			calls.Add(1)
			go func() {
				defer conn.Close()
				if stall {
					<-done
					return
				}
				config := &ssh.ServerConfig{NoClientAuth: true}
				config.AddHostKey(signer)
				ssh.NewServerConn(conn, config)
			}()
		}
	}()
	return SSHJumpCandidate{"127.0.0.1", listener.Addr().(*net.TCPAddr).Port}, calls
}
func TestSSHJumpCandidateOrdering(t *testing.T) {
	candidates := []SSHJumpCandidate{{"10.9.0.1", 2222}, {"100.64.1.1", 2222}, {"192.168.2.3", 2222}, {"203.0.113.1", 2222}, {"192.168.2.4", 2222}}
	_, subnet, _ := net.ParseCIDR("192.168.2.8/24")
	want := []SSHJumpCandidate{candidates[2], candidates[4], candidates[1], candidates[0], candidates[3]}
	if got := orderJumpCandidates(candidates, []*net.IPNet{subnet}); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}
func TestSSHJumpCandidatePinnedProbe(t *testing.T) {
	goodKey := jumpTestSigner(t)
	wrong, _ := jumpProbeServer(t, jumpTestSigner(t), false)
	stalled, _ := jumpProbeServer(t, goodKey, true)
	good, calls := jumpProbeServer(t, goodKey, false)
	unused, unusedCalls := jumpProbeServer(t, goodKey, false)
	start := time.Now()
	route, err := chooseJumpCandidate(t.Context(), []SSHJumpCandidate{wrong, stalled, good, unused}, goodKey.PublicKey(), nil, nil, probeJumpCandidate)
	if err != nil || route.SSHJumpCandidate != good || calls.Load() != 1 || unusedCalls.Load() != 0 {
		t.Fatalf("%+v %v calls=%d unused=%d", route, err, calls.Load(), unusedCalls.Load())
	}
	if time.Since(start) > 6*time.Second {
		t.Fatal("probe budget exceeded")
	}
	cached := &sshJumpRoute{wrong, ssh.FingerprintSHA256(goodKey.PublicKey()), "lan"}
	route, err = chooseJumpCandidate(t.Context(), []SSHJumpCandidate{wrong, good}, goodKey.PublicKey(), cached, nil, probeJumpCandidate)
	if err != nil || route.SSHJumpCandidate != good {
		t.Fatalf("wrong-key cached host prevented fallback: %+v %v", route, err)
	}
}
func TestSSHJumpCandidateCacheAndReconnect(t *testing.T) {
	key := jumpTestSigner(t).PublicKey()
	first := SSHJumpCandidate{"10.1.1.1", 2222}
	second := SSHJumpCandidate{"100.64.1.1", 2222}
	cached := &sshJumpRoute{first, ssh.FingerprintSHA256(key), "lan"}
	var probes int
	probe := func(context.Context, SSHJumpCandidate, ssh.PublicKey) bool { probes++; return true }
	route, err := chooseJumpCandidate(t.Context(), []SSHJumpCandidate{first, second}, key, cached, nil, probe)
	if err != nil || route.Host != first.Host || probes != 1 {
		t.Fatalf("cache: %v %v %d", route, err, probes)
	}
	probes = 0
	route, err = chooseJumpCandidate(t.Context(), []SSHJumpCandidate{first, second}, key, cached, nil, func(_ context.Context, candidate SSHJumpCandidate, _ ssh.PublicKey) bool {
		probes++
		return candidate == second
	})
	if err != nil || route.Host != second.Host || probes != 2 {
		t.Fatalf("retry: %v %v %d", route, err, probes)
	}
	cached.Fingerprint = "wrong"
	probes = 0
	_, err = chooseJumpCandidate(t.Context(), []SSHJumpCandidate{first, second}, key, cached, nil, probe)
	if err != nil || probes != 1 {
		t.Fatal(err, probes)
	}
}
func TestSSHJumpCandidateProbeCancellation(t *testing.T) {
	signer := jumpTestSigner(t)
	stalled, _ := jumpProbeServer(t, signer, true)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if probeJumpCandidate(ctx, stalled, signer.PublicKey()) {
		t.Fatal("accepted stalled server")
	}
	if probeJumpCandidate(ctx, SSHJumpCandidate{"127.0.0.1", 0}, signer.PublicKey()) {
		t.Fatal("connected invalid port " + strconv.Itoa(0))
	}
}

func TestSSHJumpCandidateDurableCacheAndAlias(t *testing.T) {
	isolateTestUserDirs(t)
	// Use a real private interface; loopback is excluded from production candidates.
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	host := ""
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil && ip.IsPrivate() {
				host = ip.String()
				break
			}
		}
		if host != "" {
			break
		}
	}
	if host == "" {
		t.Skip("no private IPv4 interface")
	}
	signer := jumpTestSigner(t)
	listener, err := net.Listen("tcp4", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var authenticated atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
					authenticated.Add(1)
					return nil, fmt.Errorf("unexpected authentication")
				}}
				cfg.AddHostKey(signer)
				ssh.NewServerConn(conn, cfg)
			}()
		}
	}()
	id := "cbx_000000000123"
	port := listener.Addr().(*net.TCPAddr).Port
	alternate := port + 1
	if alternate > 65535 {
		alternate = port - 1
	}
	outer := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	target := SSHTarget{Host: id, Port: "22", User: "crabbox", Key: filepath.Join(t.TempDir(), "lease's %h key"), SSHHostKey: outer, SSHJump: &SSHJump{Host: host, Port: port, HostKey: outer, Candidates: []SSHJumpCandidate{{host, port}, {host, alternate}}}}
	if err := prepareLeaseSSHTrust(&target, id); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(target.KnownHostsFile), "jump_route.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cached sshJumpRoute
	if err := json.Unmarshal(data, &cached); err != nil || cached.Host != host {
		t.Fatalf("%s %v", data, err)
	}
	if authenticated.Load() != 0 {
		t.Fatal("probe tried authentication")
	}
	if !strings.Contains(target.ProxyCommand, "HostKeyAlias="+target.SSHJump.HostKeyAlias) {
		t.Fatal(target.ProxyCommand)
	}
	before, _ := os.Stat(path)
	if err := prepareLeaseSSHTrust(&target, id); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("cache was rewritten despite reuse")
	}
	if got := cachedSSHRouteDescription(id); got != "lan "+net.JoinHostPort(host, strconv.Itoa(port)) {
		t.Fatal(got)
	}
}
