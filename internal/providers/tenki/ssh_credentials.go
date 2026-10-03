package tenki

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/openclaw/crabbox/internal/providers/shared"
	tenkiproto "github.com/openclaw/crabbox/internal/providers/tenki/proto"
	"golang.org/x/crypto/ssh"
)

const tenkiSSHRefreshAhead = 30 * time.Second

func readTenkiSSHCertificate(name string) (*ssh.Certificate, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("cannot read Tenki SSH certificate")
	}
	return parseTenkiSSHCertificate(data)
}

func parseTenkiSSHCertificate(data []byte) (*ssh.Certificate, error) {
	key, _, options, rest, err := ssh.ParseAuthorizedKey(data)
	cert, ok := key.(*ssh.Certificate)
	if err != nil || !ok || cert.CertType != ssh.UserCert || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("Tenki did not supply a single SSH user certificate")
	}
	return cert, nil
}

func validTenkiSSHCertificate(cert *ssh.Certificate, key ssh.PublicKey, session string, now time.Time) error {
	var identity struct{ Session string }
	if !bytes.Equal(cert.Key.Marshal(), key.Marshal()) || ssh.Unmarshal([]byte(cert.Extensions["tenki-session-id@tenki.cloud"]), &identity) != nil || identity.Session != session {
		return errors.New("Tenki SSH certificate does not match the session and native key")
	}
	principal := ""
	if len(cert.ValidPrincipals) > 0 {
		principal = cert.ValidPrincipals[0]
	}
	checker := ssh.CertChecker{Clock: func() time.Time { return now }}
	if err := checker.CheckCert(principal, cert); err != nil {
		return errors.New("Tenki SSH certificate is expired, not yet valid, or invalid")
	}
	if cert.ValidBefore != ssh.CertTimeInfinity && cert.ValidBefore <= uint64(now.Add(tenkiSSHRefreshAhead).Unix()) {
		return errors.New("Tenki SSH certificate expires within 30 seconds")
	}
	return nil
}

// Keep a separate atomic certificate snapshot: the native CLI owns its key and
// certificate, and existing SSH processes must survive renewal unchanged.
func (b *tenkiBackend) refreshSSHCertificate(ctx context.Context, output tenkiSSHCommandOutput) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	name := output.CertificateFile + ".crabbox.pub"
	unlock, err := shared.LockOperationFile(ctx, name+".lock", "Tenki SSH credential refresh")
	if err != nil {
		return fmt.Errorf("refresh Tenki SSH credentials: %w", err)
	}
	defer unlock()
	native, err := readTenkiSSHCertificate(output.CertificateFile)
	if err != nil {
		return fmt.Errorf("read native Tenki SSH credentials: %w", err)
	}
	now := time.Now
	if b.rt.Clock != nil {
		now = b.rt.Clock.Now
	}
	if info, err := os.Lstat(name); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("Tenki SSH credential cache is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Tenki SSH credential cache: %w", err)
	}
	cached, err := readTenkiSSHCertificate(name)
	if err == nil && validTenkiSSHCertificate(cached, native.Key, output.SessionID, now()) == nil {
		return nil
	}
	cert := native
	if validTenkiSSHCertificate(cert, native.Key, output.SessionID, now()) != nil {
		cert, err = b.issueSSHCertificate(ctx, output, native.Key)
		if err == nil {
			err = validTenkiSSHCertificate(cert, native.Key, output.SessionID, now())
		}
		if err != nil {
			previous := native
			if cached != nil {
				previous = cached
			}
			if previous.ValidBefore != ssh.CertTimeInfinity && previous.ValidBefore <= uint64(now().Unix()) {
				window := (float64(previous.ValidBefore) - float64(previous.ValidAfter)) / 60
				return fmt.Errorf("Tenki SSH credentials expired (signed validity window %.1f minutes); the command outlived them and credential refresh failed: %w", window, err)
			}
			return fmt.Errorf("refresh Tenki SSH credentials before connecting: %w", err)
		}
	}
	file, err := os.CreateTemp(filepath.Dir(name), ".crabbox-tenki-cert-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(ssh.MarshalAuthorizedKey(cert))
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), name)
}

func (b *tenkiBackend) issueSSHCertificate(ctx context.Context, output tenkiSSHCommandOutput, key ssh.PublicKey) (*ssh.Certificate, error) {
	credential, err := tenkiNativeCredentialContext(b.cfg)
	if err != nil {
		return nil, err
	}
	client := b.rt.HTTP
	if client == nil {
		client = &http.Client{}
	}
	client = shared.SecureHTTPClient(client, credential.endpoint, func(*url.URL) error {
		return errors.New("Tenki SSH authority redirect left its credential origin")
	})
	endpoint := strings.TrimRight(credential.endpoint.String(), "/") + tenkiSSHAuthorityService + "IssueSandboxSSHCert"
	issuer := connect.NewClient[tenkiproto.IssueSandboxSSHCertRequest, tenkiproto.IssueSandboxSSHCertResponse](client, endpoint, connect.WithGRPC(), connect.WithReadMaxBytes(1<<20))
	request := connect.NewRequest(&tenkiproto.IssueSandboxSSHCertRequest{SessionId: output.SessionID, PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))})
	request.Header().Set("Authorization", "Bearer "+credential.key)
	issued, err := issuer.CallUnary(ctx, request)
	if err != nil {
		return nil, tenkiAuthorityRequestError(ctx, "credential refresh", err)
	}
	cert, err := parseTenkiSSHCertificate([]byte(issued.Msg.GetSshCert()))
	if err != nil {
		return nil, err
	}
	ca, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(issued.Msg.GetCaPub()))
	if err != nil || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 || !bytes.Equal(ca.Marshal(), cert.SignatureKey.Marshal()) {
		return nil, errors.New("Tenki SSH credential refresh returned an invalid certificate authority")
	}
	return cert, nil
}
