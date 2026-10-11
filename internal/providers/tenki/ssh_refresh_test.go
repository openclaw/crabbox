package tenki

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	core "github.com/openclaw/crabbox/internal/cli"
	tenkiproto "github.com/openclaw/crabbox/internal/providers/tenki/proto"
	"golang.org/x/crypto/ssh"
)

type credentialTestClock struct{ unix atomic.Int64 }

func (c *credentialTestClock) Now() time.Time { return time.Unix(c.unix.Load(), 0) }

func credentialTestCert(t *testing.T, ca ssh.Signer, key ssh.PublicKey, session string, now time.Time) []byte {
	t.Helper()
	// Match the native CLI certificate: Marshal encodes extension strings once.
	const principal = "11111111-1111-4111-8111-111111111111"
	const nonce = "22222222-2222-4222-8222-222222222222"
	cert := &ssh.Certificate{Key: key, CertType: ssh.UserCert, KeyId: principal + ":" + session + ":" + nonce,
		ValidPrincipals: []string{"session:" + session, "user:" + principal, "tenki"},
		ValidAfter:      uint64(now.Add(-30 * time.Second).Unix()), ValidBefore: uint64(now.Add(10 * time.Minute).Unix()),
		Permissions: ssh.Permissions{Extensions: map[string]string{
			"permit-agent-forwarding": "", "permit-port-forwarding": "", "permit-pty": "",
			"tenki-api-key-id@tenki.cloud":      "33333333-3333-4333-8333-333333333333",
			"tenki-issuer@tenki.cloud":          "sandbox-engine-example",
			"tenki-nonce@tenki.cloud":           nonce,
			"tenki-principal-id@tenki.cloud":    principal,
			"tenki-principal-type@tenki.cloud":  "WORKSPACE",
			"tenki-runtime-secrets@tenki.cloud": "false",
			"tenki-session-id@tenki.cloud":      session,
			"tenki-user-id@tenki.cloud":         principal,
			"tenki-workspace-id@tenki.cloud":    principal,
		}}}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	return ssh.MarshalAuthorizedKey(cert)
}

func TestTenkiSSHCertificateSessionBinding(t *testing.T) {
	const session = "44444444-4444-4444-8444-444444444444"
	ca, key := authorityTestSigner(t), authorityTestSigner(t).PublicKey()
	now := time.Now()
	for _, tc := range []struct {
		name    string
		mutate  func(*ssh.Certificate)
		wantErr bool
	}{
		{name: "native CLI shape"},
		{name: "different session", wantErr: true, mutate: func(c *ssh.Certificate) {
			c.Extensions["tenki-session-id@tenki.cloud"] = "55555555-5555-4555-8555-555555555555"
		}},
		{name: "missing session", wantErr: true, mutate: func(c *ssh.Certificate) {
			delete(c.Extensions, "tenki-session-id@tenki.cloud")
		}},
		{name: "empty session", wantErr: true, mutate: func(c *ssh.Certificate) {
			c.Extensions["tenki-session-id@tenki.cloud"] = ""
		}},
		{name: "nested SSH string", wantErr: true, mutate: func(c *ssh.Certificate) {
			c.Extensions["tenki-session-id@tenki.cloud"] = string(ssh.Marshal(struct{ Session string }{session}))
		}},
		{name: "different key", wantErr: true, mutate: func(c *ssh.Certificate) {
			c.Key = authorityTestSigner(t).PublicKey()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert, err := parseTenkiSSHCertificate(credentialTestCert(t, ca, key, session, now))
			if err != nil {
				t.Fatal(err)
			}
			if tc.mutate != nil {
				tc.mutate(cert)
				if err := cert.SignCert(rand.Reader, ca); err != nil {
					t.Fatal(err)
				}
			}
			if err := validTenkiSSHCertificate(cert, key, session, now); (err != nil) != tc.wantErr {
				t.Fatalf("validTenkiSSHCertificate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func writeCredentialTestCert(t *testing.T, name, session string) {
	t.Helper()
	if err := os.WriteFile(name, credentialTestCert(t, authorityTestSigner(t), authorityTestSigner(t).PublicKey(), session, time.Now()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTenkiSSHCredentialRefresh(t *testing.T) {
	for _, failure := range []string{"", "denied", "expired", "wrong session", "wrong key", "bad signature"} {
		t.Run(failure, func(t *testing.T) {
			material := authorityTestSetup(t)
			clock := &credentialTestClock{}
			clock.unix.Store(time.Now().Unix())
			ca := authorityTestSigner(t)
			initial := credentialTestCert(t, ca, material.clientKey, material.output.SessionID, clock.Now())
			if err := os.WriteFile(material.output.CertificateFile, initial, 0o600); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			server := authorityTestServer(t, func(ctx context.Context, req *connect.Request[tenkiproto.IssueSandboxSSHCertRequest]) (*connect.Response[tenkiproto.IssueSandboxSSHCertResponse], error) {
				calls.Add(1)
				if req.Msg.SessionId != material.output.SessionID || req.Msg.PublicKey != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(material.clientKey))) {
					t.Error("unbound issuance")
				}
				if failure == "denied" {
					return nil, connect.NewError(connect.CodePermissionDenied, errors.New("secret must not appear"))
				}
				session, key, issuedAt := material.output.SessionID, material.clientKey, clock.Now()
				if failure == "wrong session" {
					session = "other-session"
				}
				if failure == "wrong key" {
					key = authorityTestSigner(t).PublicKey()
				}
				if failure == "expired" {
					issuedAt = issuedAt.Add(-11 * time.Minute)
				}
				data := credentialTestCert(t, ca, key, session, issuedAt)
				if failure == "bad signature" {
					cert, _ := parseTenkiSSHCertificate(data)
					cert.Signature.Blob[0] ^= 1
					data = ssh.MarshalAuthorizedKey(cert)
				}
				return connect.NewResponse(&tenkiproto.IssueSandboxSSHCertResponse{SshCert: string(data), CaPub: string(ssh.MarshalAuthorizedKey(ca.PublicKey()))}), nil
			}, nil)
			backend := &tenkiBackend{cfg: core.Config{Tenki: core.TenkiConfig{Endpoint: server.URL}}, rt: core.Runtime{Clock: clock, HTTP: server.Client()}}
			target := backend.sshTarget(material.output, "authority", "")
			if err := target.PrepareConnection(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 0 {
				t.Fatal("fresh native credential was not cached")
			}
			clock.unix.Add(11 * 60)
			err := target.PrepareConnection(context.Background())
			if failure != "" {
				if err == nil || !strings.Contains(err.Error(), "Tenki SSH credentials expired") || !strings.Contains(err.Error(), "10.5 minutes") || strings.Contains(err.Error(), "secret must not appear") {
					t.Fatalf("cause lost: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					if err := target.PrepareConnection(context.Background()); err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			// A separate target for a run retry shares the per-session disk cache.
			if err := backend.sshTarget(material.output, "authority", "").PrepareConnection(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatalf("refresh calls=%d", calls.Load())
			}
			got, err := os.ReadFile(material.output.CertificateFile)
			if err != nil || string(got) != string(initial) {
				t.Fatal("native credential was changed")
			}
			info, err := os.Stat(target.CertificateFile)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("cache permissions: %v %v", info, err)
			}
		})
	}
}

func TestTenkiSSHCredentialRefreshCancelled(t *testing.T) {
	material := authorityTestSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (&tenkiBackend{}).sshTarget(material.output, "authority", "").PrepareConnection(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}
