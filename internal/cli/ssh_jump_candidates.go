package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
)

type SSHJumpCandidate struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}
type sshJumpRoute struct {
	SSHJumpCandidate
	Fingerprint string `json:"fingerprint"`
	Kind        string `json:"kind"`
}

func jumpCandidateKind(c SSHJumpCandidate) string {
	ip := net.ParseIP(c.Host)
	if ip != nil && ip.IsPrivate() {
		return "lan"
	}
	if v := ip.To4(); v != nil && v[0] == 100 && v[1] >= 64 && v[1] <= 127 {
		return "tailnet"
	}
	return "explicit"
}
func localJumpSubnets() []*net.IPNet {
	interfaces, _ := net.Interfaces()
	var subnets []*net.IPNet
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			ip, subnet, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil {
				subnets = append(subnets, subnet)
			}
		}
	}
	return subnets
}
func orderJumpCandidates(candidates []SSHJumpCandidate, subnets []*net.IPNet) []SSHJumpCandidate {
	ordered := append([]SSHJumpCandidate(nil), candidates...)
	rank := func(c SSHJumpCandidate) int {
		if jumpCandidateKind(c) == "lan" {
			for _, subnet := range subnets {
				if subnet.Contains(net.ParseIP(c.Host)) {
					return 0
				}
			}
		}
		if jumpCandidateKind(c) == "tailnet" {
			return 1
		}
		return 2
	}
	sort.SliceStable(ordered, func(i, j int) bool { return rank(ordered[i]) < rank(ordered[j]) })
	return ordered
}

var errJumpKeyMatched = errors.New("SSH jump host key matched")

// SSH verifies the exchange signature before invoking HostKeyCallback. Stop at
// that callback: no authentication attempt (or lease private key) is needed.
func probeJumpCandidate(ctx context.Context, candidate SSHJumpCandidate, key ssh.PublicKey) bool {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	address := net.JoinHostPort(candidate.Host, strconv.Itoa(candidate.Port))
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", address)
	if err != nil {
		return false
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_, _, _, err = ssh.NewClientConn(conn, address, &ssh.ClientConfig{
		User: "crabbox", HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		HostKeyCallback: func(_ string, _ net.Addr, offered ssh.PublicKey) error {
			if bytes.Equal(key.Marshal(), offered.Marshal()) {
				return errJumpKeyMatched
			}
			return errors.New("SSH jump host key mismatch")
		},
	})
	return errors.Is(err, errJumpKeyMatched)
}
func chooseJumpCandidate(ctx context.Context, candidates []SSHJumpCandidate, key ssh.PublicKey, cached *sshJumpRoute, subnets []*net.IPNet, probe func(context.Context, SSHJumpCandidate, ssh.PublicKey) bool) (sshJumpRoute, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	fingerprint := ssh.FingerprintSHA256(key)
	var checked *SSHJumpCandidate
	if cached != nil && cached.Fingerprint == fingerprint {
		for _, candidate := range candidates {
			if candidate == cached.SSHJumpCandidate {
				checked = &candidate
				if probe(ctx, candidate, key) {
					return *cached, nil
				}
				break
			}
		}
	}
	for _, candidate := range orderJumpCandidates(candidates, subnets) {
		if checked != nil && candidate == *checked {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		if probe(ctx, candidate, key) {
			return sshJumpRoute{candidate, fingerprint, jumpCandidateKind(candidate)}, nil
		}
	}
	return sshJumpRoute{}, fmt.Errorf("no SSH jump candidate reachable with the pinned host key")
}

func selectSSHJumpCandidate(target *SSHTarget, key ssh.PublicKey) error {
	jump := target.SSHJump
	if jump.RelayOnly && (!jump.Relay || len(jump.Candidates) != 0) {
		return Exit(2, "relay-only jump must enable relay and omit direct candidates")
	}
	if len(jump.Candidates) <= 1 && !jump.Relay {
		return nil
	}
	if len(jump.Candidates) > 4 {
		return Exit(2, "too many SSH jump candidates")
	}
	if len(jump.Candidates) == 0 && !jump.RelayOnly {
		jump.Candidates = []SSHJumpCandidate{{Host: jump.Host, Port: jump.Port}}
	}
	seen := map[SSHJumpCandidate]bool{}
	for _, candidate := range jump.Candidates {
		ip := net.ParseIP(candidate.Host)
		if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || candidate.Port < 1 || candidate.Port > 65535 || seen[candidate] {
			return Exit(2, "invalid SSH jump candidate")
		}
		seen[candidate] = true
	}
	path := filepath.Join(filepath.Dir(target.KnownHostsFile), "jump_route.json")
	var cached *sshJumpRoute
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &cached)
	}
	route, err := chooseJumpCandidate(context.Background(), jump.Candidates, key, cached, localJumpSubnets(), probeJumpCandidate)
	if err != nil {
		if !jump.Relay || target.relayConfig == nil {
			return err
		}
		coordinator, _, authErr := newCoordinatorClient(*target.relayConfig)
		if authErr != nil {
			return authErr
		}
		if coordinator == nil {
			return err
		}
		target.relayCoordinator = coordinator
		jump.useRelay = true
		route = sshJumpRoute{Fingerprint: ssh.FingerprintSHA256(key), Kind: "relay"}
	}
	data, err := json.Marshal(route)
	if err != nil {
		return err
	}
	if err := writeAuthoritativeKnownHosts(path, data); err != nil {
		return fmt.Errorf("save SSH jump route: %w", err)
	}
	if jump.useRelay {
		fmt.Fprintln(os.Stderr, "serve route: relay")
	} else {
		jump.Host, jump.Port = route.Host, route.Port
		fmt.Fprintf(os.Stderr, "serve route: %s %s\n", route.Kind, net.JoinHostPort(route.Host, strconv.Itoa(route.Port)))
	}
	return nil
}

func cachedSSHRouteDescription(leaseID string) string {
	path, err := TestboxKeyPath(leaseID)
	if err != nil {
		return ""
	}
	var route sshJumpRoute
	data, err := os.ReadFile(filepath.Join(filepath.Dir(path), "jump_route.json"))
	if err != nil || json.Unmarshal(data, &route) != nil {
		return ""
	}
	if route.Kind == "relay" {
		return "relay"
	}
	return route.Kind + " " + net.JoinHostPort(route.Host, strconv.Itoa(route.Port))
}
