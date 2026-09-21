//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestAuthoritativeKnownHostsCertificatesWithOpenSSH(t *testing.T) {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH unavailable")
	}
	newSigner := func() ssh.Signer {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		signer, err := ssh.NewSignerFromKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return signer
	}
	trustedCA, otherCA := newSigner(), newSigner()
	for _, transport := range []string{"argv", "config", "argv-alias", "config-alias"} {
		t.Run(transport, func(t *testing.T) {
			isolateTestUserDirs(t)
			knownHosts := filepath.Join(t.TempDir(), "known_hosts")
			trusted := "@cert-authority * " + string(ssh.MarshalAuthorizedKey(trustedCA.PublicKey()))
			for _, scenario := range []struct {
				name           string
				ca             ssh.Signer
				trust          string
				missing, allow bool
			}{
				{name: "trusted", ca: trustedCA, trust: trusted, allow: true},
				{name: "rotated leaf", ca: trustedCA, trust: trusted, allow: true},
				{name: "untrusted CA", ca: otherCA, trust: trusted},
				{name: "removed CA", ca: trustedCA},
				{name: "missing trust file", ca: trustedCA, missing: true},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					if err := os.WriteFile(knownHosts, []byte(scenario.trust), 0o600); err != nil {
						t.Fatal(err)
					}
					if scenario.missing {
						if err := os.Remove(knownHosts); err != nil {
							t.Fatal(err)
						}
					}
					leaf := newSigner()
					cert := &ssh.Certificate{Key: leaf.PublicKey(), CertType: ssh.HostCert,
						ValidPrincipals: []string{"127.0.0.1", "gateway.example.test"}, ValidBefore: ssh.CertTimeInfinity}
					if err := cert.SignCert(rand.Reader, scenario.ca); err != nil {
						t.Fatal(err)
					}
					signer, err := ssh.NewCertSigner(cert, leaf)
					if err != nil {
						t.Fatal(err)
					}
					echoPort := forwardEchoServer(t)
					server := newForwardSSHServerWithSigner(t, "synthetic-certificate-trust", signer, echoPort)
					close(server.release)
					target := SSHTarget{User: "synthetic-certificate-trust", Host: "127.0.0.1", Port: strconv.Itoa(server.port()),
						KnownHostsFile: knownHosts, AuthoritativeKnownHosts: true}
					if strings.HasSuffix(transport, "-alias") {
						target.HostKeyAlias = "gateway.example.test"
					}
					args := append(sshBaseArgs(target), "-F", os.DevNull)
					destination := target.User + "@" + target.Host
					if strings.HasPrefix(transport, "config") {
						config, err := renderSSHTransportConfig(target, false)
						if err != nil {
							t.Fatal(err)
						}
						configPath := filepath.Join(t.TempDir(), "config")
						if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
							t.Fatal(err)
						}
						args = []string{"-F", configPath}
						destination = sshTransportHostAlias
					}
					args = append(args, "-W", fmt.Sprintf("127.0.0.1:%d", echoPort), destination)
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, sshPath, args...)
					const marker = "authenticated synthetic payload\n"
					cmd.Stdin = strings.NewReader(marker)
					var stdout, stderr bytes.Buffer
					cmd.Stdout, cmd.Stderr = &stdout, &stderr
					err = cmd.Run()
					server.mu.Lock()
					authenticated := len(server.users)
					server.mu.Unlock()
					if scenario.allow {
						if err != nil || stdout.String() != marker || authenticated != 1 {
							t.Fatalf("trusted certificate failed: err=%v authenticated=%d stdout=%q stderr=%s", err, authenticated, stdout.String(), stderr.String())
						}
					} else if err == nil || stdout.Len() != 0 || authenticated != 0 || ctx.Err() != nil {
						t.Fatalf("untrusted certificate reached remote authentication: err=%v authenticated=%d stdout=%q stderr=%s", err, authenticated, stdout.String(), stderr.String())
					}
					after, readErr := os.ReadFile(knownHosts)
					if scenario.missing {
						if !os.IsNotExist(readErr) {
							t.Fatalf("missing trust file was created: %v", readErr)
						}
					} else if readErr != nil || string(after) != scenario.trust {
						t.Fatalf("provider trust file was changed: %v", readErr)
					}
				})
			}
		})
	}
}
