package serve

import (
	"bytes"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// HostFrontDoor reuses the serve SSH tunnel and revocation owner for an exclusive
// native-host allocation. It exposes no arbitrary TCP forwarding or host shell.
type HostFrontDoor struct{ door *frontDoor }

func NewHostFrontDoor(signer ssh.Signer) *HostFrontDoor {
	return &HostFrontDoor{door: &frontDoor{signer: signer, listeners: map[string]net.Listener{}, leases: map[string]*frontLease{}, sockets: map[net.Conn]struct{}{}, handshakes: make(chan struct{}, frontDoorHandshakes)}}
}

func (f *HostFrontDoor) HostKey() string {
	return string(ssh.MarshalAuthorizedKey(f.door.signer.PublicKey()))
}
func (f *HostFrontDoor) Close()                              { f.door.Close() }
func (f *HostFrontDoor) Admit(conn net.Conn, leaseID string) { f.door.admit(conn, leaseID) }
func (f *HostFrontDoor) Revoke() {
	f.door.mu.Lock()
	defer f.door.mu.Unlock()
	for id, l := range f.door.leases {
		f.door.revoke(l)
		delete(f.door.leases, id)
	}
}
func (f *HostFrontDoor) Set(leaseID, publicKey, port string, deadline time.Time) error {
	key, _, opts, rest, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil || len(opts) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return fmt.Errorf("invalid host lease public key")
	}
	d := f.door
	d.mu.Lock()
	defer d.mu.Unlock()
	l := d.leases[leaseID]
	if l != nil && (!bytes.Equal(l.key, key.Marshal()) || l.port != port) {
		d.revoke(l)
		l = nil
	}
	for id, other := range d.leases {
		if id != leaseID {
			d.revoke(other)
			delete(d.leases, id)
		}
	}
	if l == nil {
		l = &frontLease{key: key.Marshal(), port: port, connections: map[net.Conn]struct{}{}, upstreams: map[net.Conn]struct{}{}}
	}
	l.deadline = deadline
	if l.timer != nil {
		l.timer.Stop()
	}
	current := l
	l.timer = time.AfterFunc(time.Until(deadline), func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.leases[leaseID] == current && !current.deadline.After(time.Now()) {
			delete(d.leases, leaseID)
			d.revoke(current)
		}
	})
	d.leases[leaseID] = l
	return nil
}
