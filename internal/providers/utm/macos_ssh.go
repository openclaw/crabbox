package utm

import (
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
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

var bootstrapMacOSGuest = (*backend).bootstrapMacOSSSH

func macOSKeyDir(r record) (string, error) {
	if r.LeaseID == "" || strings.ContainsAny(r.LeaseID, "/\\.\x00") {
		return "", fmt.Errorf("invalid UTM lease key identity")
	}
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "keys-"+r.LeaseID), nil
}
func (b *backend) generateMacOSKey(r *record) (string, error) {
	dir, err := macOSKeyDir(*r)
	if err != nil {
		return "", err
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	r.SSHKey = filepath.Join(dir, "id_ed25519")
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "crabbox-utm")
	if err != nil {
		return "", err
	}
	public, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	keyText := string(ssh.MarshalAuthorizedKey(public))
	for _, entry := range []struct {
		name string
		data []byte
	}{{"id_ed25519", pem.EncodeToMemory(block)}, {"id_ed25519.pub", []byte(keyText)}} {
		f, err := os.OpenFile(filepath.Join(dir, entry.name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return "", err
		}
		_, writeErr := f.Write(entry.data)
		syncErr := f.Sync()
		closeErr := f.Close()
		for _, err := range []error{writeErr, syncErr, closeErr} {
			if err != nil {
				return "", err
			}
		}
	}
	if err = syncDir(dir); err != nil {
		return "", err
	}
	return strings.TrimSpace(keyText), nil
}

func (b *backend) bootstrapMacOSSSH(ctx context.Context, r *record, password, publicKey string) error {
	for {
		// DHCP can change while the fresh account is being configured. Never treat
		// the earlier ARP response as a durable endpoint identity.
		if err := b.refreshMacOSAddress(ctx, r); err != nil {
			return err
		}
		hostKey, verified, err := bootstrapMacOSAttempt(ctx, *r, password, publicKey)
		if err == nil {
			r.SSHHostKey = hostKey
			return nil
		}
		if verified {
			return fmt.Errorf("UTM macOS bootstrap identity or key installation failed; guest output suppressed")
		}
		if err = waitPoll(ctx, time.Second); err != nil {
			return fmt.Errorf("UTM macOS password SSH bootstrap did not become ready: %w", err)
		}
	}
}

// Initial SSH host-key trust is local-network TOFU, scoped to a fresh random
// credential. The authenticated guest must prove its assigned MAC and exact OS
// before receiving a continuing key. No password reaches argv, child environment,
// local files, subprocess diagnostics, or remote command text.
func bootstrapMacOSAttempt(ctx context.Context, r record, password, publicKey string) (hostKey string, authenticated bool, err error) {
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(r.Address, strconv.Itoa(r.Port)))
	if err != nil {
		return "", false, fmt.Errorf("SSH unavailable")
	}
	defer conn.Close()
	deadline := time.Now().Add(20 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err = conn.SetDeadline(deadline); err != nil {
		return "", false, fmt.Errorf("SSH deadline failed")
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	config := &ssh.ClientConfig{User: r.User, Auth: []ssh.AuthMethod{ssh.Password(password)}, Timeout: 10 * time.Second, HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
		hostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		return nil
	}}
	cc, chans, requests, err := ssh.NewClientConn(conn, net.JoinHostPort(r.Address, strconv.Itoa(r.Port)), config)
	if err != nil {
		return "", false, fmt.Errorf("SSH authentication not ready")
	}
	client := ssh.NewClient(cc, chans, requests)
	defer client.Close()
	run := func(command string) error {
		session, err := client.NewSession()
		if err != nil {
			return err
		}
		defer session.Close()
		session.Stdout = io.Discard
		session.Stderr = io.Discard
		return session.Run(command)
	}
	if err = run(macOSReadyCheck(r)); err != nil {
		return "", true, fmt.Errorf("guest identity rejected")
	}
	command := `umask 077; test -d "$HOME" && mkdir -p "$HOME/.ssh" && chmod 700 "$HOME/.ssh" && touch "$HOME/.ssh/authorized_keys" && chmod 600 "$HOME/.ssh/authorized_keys" && printf '%s\n' ` + core.ShellQuote(publicKey) + ` >> "$HOME/.ssh/authorized_keys"`
	if err = run(command); err != nil {
		return "", true, fmt.Errorf("guest key installation rejected")
	}
	return hostKey, true, nil
}

// Reinstall only the already-recorded public host key in a provider-owned file.
// This callback also repairs a missing pin before subsequent workload connections.
func (b *backend) pinMacOSHostKey(r record) error {
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(r.SSHHostKey))
	if err != nil || len(options) != 0 || len(strings.TrimSpace(string(rest))) != 0 {
		return fmt.Errorf("UTM stored SSH host key is invalid")
	}
	dir, err := macOSKeyDir(r)
	if err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("UTM generated key directory is unavailable or unsafe")
	}
	f, err := os.CreateTemp(dir, ".known-hosts-")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	_, writeErr := f.WriteString(knownhosts.Line([]string{"crabbox-utm-" + r.LeaseID}, key) + "\n")
	syncErr := f.Sync()
	closeErr := f.Close()
	for _, err := range []error{writeErr, syncErr, closeErr} {
		if err != nil {
			return err
		}
	}
	if err = os.Rename(path, filepath.Join(dir, "known_hosts")); err != nil {
		return err
	}
	return syncDir(dir)
}
