package serve

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/openclaw/crabbox/internal/providers/localcontainer"
	"golang.org/x/crypto/ssh"
)

const (
	frontDoorHandshakeTimeout = 10 * time.Second
	frontDoorHandshakes       = 32
	frontDoorConnections      = 16
	frontDoorStreams          = 16
)

type frontDoorEndpoint struct {
	Relay      bool               `json:"relay,omitempty"`
	Candidates []addressCandidate `json:"candidates,omitempty"`
	Port       int                `json:"port"`
	HostKey    string             `json:"hostKey"`
}
type frontLease struct {
	key         []byte
	port        string
	deadline    time.Time
	timer       *time.Timer
	connections map[net.Conn]struct{}
	upstreams   map[net.Conn]struct{}
	streams     int
}
type frontDoor struct {
	listeners  map[string]net.Listener
	candidates []addressCandidate
	signer     ssh.Signer
	mu         sync.Mutex
	leases     map[string]*frontLease
	sockets    map[net.Conn]struct{}
	closed     bool
	handshakes chan struct{}
	wg         sync.WaitGroup
}

func frontDoorHostKey(dir string) (ssh.Signer, error) {
	path := filepath.Join(dir, "ssh_host_ed25519_key")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		_, key, genErr := ed25519.GenerateKey(rand.Reader)
		if genErr != nil {
			return nil, genErr
		}
		block, marshalErr := ssh.MarshalPrivateKey(key, "crabbox serve")
		if marshalErr != nil {
			return nil, marshalErr
		}
		data = pem.EncodeToMemory(block)
		f, createErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if createErr != nil {
			return nil, createErr
		}
		_, err = f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	} else if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("serve host key %s must be a regular file with mode 0600", path)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("read serve host key: %w", err)
	}
	if signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		return nil, fmt.Errorf("serve host key must be ed25519")
	}
	return signer, nil
}

type frontDoorAddressError struct{ error }

func startFrontDoors(dir string, candidates []addressCandidate, port int, log io.Writer) (*frontDoor, error) {
	signer, err := frontDoorHostKey(dir)
	if err != nil {
		return nil, err
	}
	f := &frontDoor{listeners: map[string]net.Listener{}, signer: signer, leases: map[string]*frontLease{}, sockets: map[net.Conn]struct{}{}, handshakes: make(chan struct{}, frontDoorHandshakes)}
	if _, err := f.rebind(candidates, port, log); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Keep listeners for unchanged addresses (and all authenticated sessions).
// Newly bound addresses are the only addresses published to the coordinator.
func (f *frontDoor) rebind(candidates []addressCandidate, port int, log io.Writer) ([]addressCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, &frontDoorAddressError{fmt.Errorf("serve front door closed")}
	}
	next := map[string]net.Listener{}
	var bound []addressCandidate
	for _, candidate := range candidates {
		address := net.JoinHostPort(candidate.Address, strconv.Itoa(port))
		listener := f.listeners[address]
		if listener == nil {
			var err error
			listener, err = net.Listen("tcp4", address)
			if err != nil {
				if log != nil {
					fmt.Fprintf(log, "serve front door: bind %s: %v\n", address, err)
				}
				continue
			}
			f.wg.Add(1)
			go f.accept(listener)
		}
		next[address] = listener
		bound = append(bound, candidate)
	}
	for address, listener := range f.listeners {
		if next[address] == nil {
			_ = listener.Close()
		}
	}
	f.listeners, f.candidates = next, bound
	if len(bound) == 0 {
		return nil, &frontDoorAddressError{fmt.Errorf("no serve SSH front-door addresses bound; free the configured port or select --ssh-port and --advertise-address")}
	}
	return append([]addressCandidate(nil), bound...), nil
}

func (f *frontDoor) Close() {
	f.mu.Lock()
	if !f.closed {
		f.closed = true
		for _, listener := range f.listeners {
			_ = listener.Close()
		}
		for _, l := range f.leases {
			f.revoke(l)
		}
		for c := range f.sockets {
			_ = c.Close()
		}
	}
	f.mu.Unlock()
	f.wg.Wait()
}

// Called with mu held. Closing the outer transport also closes every channel;
// stream workers then close their loopback sockets.
func (f *frontDoor) revoke(l *frontLease) {
	if l.timer != nil {
		l.timer.Stop()
	}
	for c := range l.connections {
		_ = c.Close()
	}
	for c := range l.upstreams {
		_ = c.Close()
	}
}
func (a *agent) updateFrontDoor(containers []localcontainer.ServedContainer) {
	if a.frontDoor == nil {
		return
	}
	f := a.frontDoor
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	next := map[string]*frontLease{}
	for _, c := range containers {
		l, ok := a.state.Assignments[c.LeaseID]
		port, err := strconv.Atoi(c.Port)
		if !ok || !owned(c, a.state.Machine.MachineID) || l.MachineID != a.state.Machine.MachineID || !c.Running || c.Host != "127.0.0.1" || err != nil || port < 1 || port > 65535 || !deadline(l).After(time.Now()) || (l.State != "active" && l.State != "provisioning") {
			continue
		}
		key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(l.SSHPublicKey))
		if err != nil || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
			continue
		}
		entry := f.leases[l.ID]
		if entry != nil && (!bytes.Equal(entry.key, key.Marshal()) || entry.port != c.Port) {
			f.revoke(entry)
			entry = nil
		}
		if entry == nil {
			entry = &frontLease{key: key.Marshal(), port: c.Port, connections: map[net.Conn]struct{}{}, upstreams: map[net.Conn]struct{}{}}
		}
		entry.deadline = deadline(l)
		if entry.timer != nil {
			entry.timer.Stop()
		}
		id := l.ID
		current := entry
		entry.timer = time.AfterFunc(time.Until(entry.deadline), func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.leases[id] == current && !current.deadline.After(time.Now()) {
				delete(f.leases, id)
				f.revoke(current)
			}
		})
		next[l.ID] = entry
	}
	for id, entry := range f.leases {
		if next[id] != entry {
			f.revoke(entry)
		}
	}
	f.leases = next
}

func (f *frontDoor) accept(listener net.Listener) {
	defer f.wg.Done()
	for {
		c, err := listener.Accept()
		if err != nil {
			return
		}
		f.admit(c, "")
	}
}

// Relay admissions are pre-bound to their minted lease, including while SSH
// authentication is pending. The same key and channel policy still applies.
func (f *frontDoor) admit(c net.Conn, leaseID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		_ = c.Close()
		return
	}
	var bound *frontLease
	if leaseID != "" {
		bound = f.leases[leaseID]
		if bound == nil || !bound.deadline.After(time.Now()) || len(bound.connections) >= frontDoorConnections {
			_ = c.Close()
			return
		}
	}
	select {
	case f.handshakes <- struct{}{}:
		f.sockets[c] = struct{}{}
		if bound != nil {
			bound.connections[c] = struct{}{}
		}
		f.wg.Add(1)
		go f.serve(c, bound, leaseID)
	default:
		_ = c.Close()
	}
}
func (f *frontDoor) serve(raw net.Conn, bound *frontLease, id string) {
	defer f.wg.Done()
	defer raw.Close()
	slot := true
	defer func() {
		if slot {
			<-f.handshakes
		}
		f.mu.Lock()
		delete(f.sockets, raw)
		if bound != nil {
			delete(bound.connections, raw)
		}
		f.mu.Unlock()
	}()
	config := &ssh.ServerConfig{MaxAuthTries: 3, PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		denied := fmt.Errorf("lease authentication denied")
		if f.closed || meta.User() != "crabbox" {
			return nil, denied
		}
		var selected *frontLease
		var selectedID string
		for leaseID, l := range f.leases {
			if l.deadline.After(time.Now()) && bytes.Equal(l.key, key.Marshal()) {
				// Ambiguous reused keys cannot select a lease safely.
				if selected != nil {
					return nil, denied
				}
				selected, selectedID = l, leaseID
			}
		}
		if selected == nil || (bound != nil && bound != selected) {
			return nil, denied
		}
		if bound == nil {
			if len(selected.connections) >= frontDoorConnections {
				return nil, denied
			}
			bound, id = selected, selectedID
			bound.connections[raw] = struct{}{}
		}
		return &ssh.Permissions{Extensions: map[string]string{"lease": id}}, nil
	}}
	config.AddHostKey(f.signer)
	_ = raw.SetDeadline(time.Now().Add(frontDoorHandshakeTimeout))
	conn, channels, requests, err := ssh.NewServerConn(raw, config)
	<-f.handshakes
	slot = false
	if err != nil {
		return
	}
	defer conn.Close()
	f.mu.Lock()
	live := !f.closed && bound != nil && f.leases[id] == bound && bound.deadline.After(time.Now())
	f.mu.Unlock()
	if !live {
		return
	}
	_ = raw.SetDeadline(time.Time{})
	go func() {
		for r := range requests {
			_ = r.Reply(r.Type == "keepalive@openssh.com", nil)
		}
	}()
	connectionDone := make(chan struct{})
	go func() { _ = conn.Wait(); close(connectionDone) }()
	var streams sync.WaitGroup
	defer func() { _ = conn.Close(); streams.Wait() }()
	for channel := range channels {
		if channel.ChannelType() != "direct-tcpip" {
			_ = channel.Reject(ssh.Prohibited, "only lease SSH forwarding is allowed")
			continue
		}
		var dest struct {
			Host       string
			Port       uint32
			Origin     string
			OriginPort uint32
		}
		if ssh.Unmarshal(channel.ExtraData(), &dest) != nil || dest.Host != conn.Permissions.Extensions["lease"] || dest.Port != 22 {
			_ = channel.Reject(ssh.Prohibited, "destination must be the authenticated lease on port 22")
			continue
		}
		f.mu.Lock()
		allowed := !f.closed && f.leases[id] == bound && bound.deadline.After(time.Now()) && bound.streams < frontDoorStreams
		if allowed {
			bound.streams++
		}
		f.mu.Unlock()
		if !allowed {
			_ = channel.Reject(ssh.ResourceShortage, "lease unavailable or stream limit reached")
			continue
		}
		// Dial and copy are bounded per lease. Revocation closes the outer channel,
		// which wakes the copies and closes the loopback socket, including idle streams.
		streams.Add(1)
		go func(channel ssh.NewChannel) {
			defer streams.Done()
			defer func() { f.mu.Lock(); bound.streams--; f.mu.Unlock() }()
			dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			upstream, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", net.JoinHostPort("127.0.0.1", bound.port))
			if err != nil {
				_ = channel.Reject(ssh.ConnectionFailed, "lease SSH unavailable")
				return
			}
			defer upstream.Close()
			f.mu.Lock()
			live := !f.closed && f.leases[id] == bound && bound.deadline.After(time.Now())
			if live {
				bound.upstreams[upstream] = struct{}{}
			}
			f.mu.Unlock()
			defer func() { f.mu.Lock(); delete(bound.upstreams, upstream); f.mu.Unlock() }()
			if !live {
				_ = channel.Reject(ssh.Prohibited, "lease revoked")
				return
			}
			stream, reqs, err := channel.Accept()
			if err != nil {
				return
			}
			defer stream.Close()
			streamDone := make(chan struct{})
			defer close(streamDone)
			go func() {
				select {
				case <-connectionDone:
					_ = upstream.Close()
				case <-streamDone:
				}
			}()
			go ssh.DiscardRequests(reqs)
			done := make(chan struct{})
			go func() { _, _ = io.Copy(upstream, stream); _ = upstream.(*net.TCPConn).CloseWrite(); close(done) }()
			_, _ = io.Copy(stream, upstream)
			_ = stream.Close()
			_ = upstream.Close()
			<-done
		}(channel)
	}
}
