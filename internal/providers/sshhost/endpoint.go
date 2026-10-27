package sshhost

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/openclaw/crabbox/internal/privatefile"
	"golang.org/x/crypto/ssh"
)

type endpointConfig struct {
	PublicKey  string `json:"publicKey"`
	PrivateKey string `json:"privateKey"`
	WorkRoot   string `json:"workRoot"`
	Deadline   int64  `json:"deadline"`
	Port       int    `json:"port"`
}

func generateHostKey() (string, string, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		return "", "", err
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(block)), string(ssh.MarshalAuthorizedKey(signer.PublicKey())), nil
}
func readEndpoint(path string) (endpointConfig, error) {
	var c endpointConfig
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, &c)
	}
	return c, err
}

// The endpoint runs INSIDE the lease account/session, never as the operator.
// Account/session teardown, not shell process tracking, is the cleanup boundary.
func runEndpoint(ctx context.Context, path string) error {
	c, err := readEndpoint(path)
	if err != nil {
		return err
	}
	signer, err := ssh.ParsePrivateKey([]byte(c.PrivateKey))
	if err != nil {
		return err
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.PublicKey))
	if err != nil {
		return err
	}
	if c.Port < 1 || c.Port > 65535 || c.WorkRoot == "" || c.Deadline <= time.Now().UnixMilli() {
		return fmt.Errorf("invalid endpoint configuration")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(c.Port)))
	if err != nil {
		return err
	}
	defer listener.Close()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = listener.Close()
				return
			case <-ticker.C:
				current, e := readEndpoint(path)
				if e != nil || current.PublicKey != c.PublicKey || current.PrivateKey != c.PrivateKey || current.Deadline <= time.Now().UnixMilli() {
					cancel()
				}
			}
		}
	}()
	config := &ssh.ServerConfig{MaxAuthTries: 3, PublicKeyCallback: func(meta ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if meta.User() != "crabbox" || !bytes.Equal(k.Marshal(), key.Marshal()) {
			return nil, fmt.Errorf("lease authentication denied")
		}
		return nil, nil
	}}
	config.AddHostKey(signer)
	var wg sync.WaitGroup
	defer wg.Wait()
	slots := make(chan struct{}, 16)
	for {
		conn, e := listener.Accept()
		if e != nil {
			if ctx.Err() != nil {
				return nil
			}
			return e
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			conn.Close()
			return nil
		default:
			conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer conn.Close()
			done := make(chan struct{})
			defer close(done)
			go func() {
				select {
				case <-ctx.Done():
					conn.Close()
				case <-done:
				}
			}()
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
			server, channels, requests, e := ssh.NewServerConn(conn, config)
			if e != nil {
				return
			}
			defer server.Close()
			_ = conn.SetDeadline(time.Time{})
			go ssh.DiscardRequests(requests)
			var sessions sync.WaitGroup
			defer sessions.Wait()
			for ch := range channels {
				if ch.ChannelType() != "session" {
					ch.Reject(ssh.Prohibited, "only sessions allowed")
					continue
				}
				stream, reqs, e := ch.Accept()
				if e != nil {
					continue
				}
				sessions.Add(1)
				go func() {
					defer sessions.Done()
					serveEndpointSession(ctx, c.WorkRoot, stream, reqs)
				}()
			}
		}()
	}
}
func writePrivateJSON(path string, value any) error {
	return writeJSONWithCreate(path, value, 0600, privatefile.CreateTemp)
}
func writeJSON(path string, value any, mode os.FileMode) error {
	return writeJSONWithCreate(path, value, mode, os.CreateTemp)
}
func writeJSONWithCreate(path string, value any, mode os.FileMode, create func(string, string) (*os.File, error)) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := create(filepath.Dir(path), ".host-state-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func waitEndpoint(ctx context.Context, r *runtimeState) error {
	expected, _, _, _, err := ssh.ParseAuthorizedKey([]byte(r.HostKey))
	if err != nil {
		return err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		socket, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(r.Port)))
		if err == nil {
			observed := false
			_ = socket.SetDeadline(time.Now().Add(time.Second))
			conn, _, _, _ := ssh.NewClientConn(socket, "endpoint", &ssh.ClientConfig{User: "crabbox", HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
				if !bytes.Equal(key.Marshal(), expected.Marshal()) {
					return fmt.Errorf("endpoint identity mismatch")
				}
				observed = true
				return nil
			}})
			if conn != nil {
				conn.Close()
			}
			socket.Close()
			if observed {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("isolated SSH endpoint did not become ready")
}
