package tenki

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	core "github.com/openclaw/crabbox/internal/cli"
	tenkiproto "github.com/openclaw/crabbox/internal/providers/tenki/proto"
	"golang.org/x/crypto/ssh"
)

const authorityTestToken = "tk_synthetic_authority_test"

type authorityTestMaterial struct {
	output     tenkiSSHCommandOutput
	ca         ssh.PublicKey
	clientKey  ssh.PublicKey
	clientCert []byte
}

func authorityTestSigner(t *testing.T) ssh.Signer {
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

func authorityTestSetup(t *testing.T) authorityTestMaterial {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TENKI_CONFIG_FILE", "config.yaml")
	t.Setenv("TENKI_AUTH_TOKEN", authorityTestToken)
	t.Setenv("TENKI_API_KEY", "")
	t.Setenv("TENKI_API_ENDPOINT", "")
	ca, client := authorityTestSigner(t), authorityTestSigner(t)
	cert := &ssh.Certificate{Key: client.PublicKey(), CertType: ssh.UserCert, ValidBefore: ssh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	output := tenkiSSHCommandOutput{
		SessionID: "session-authority-test", Host: "sandbox", User: "tenki", Port: 22,
		CertificateFile: filepath.Join(home, "native-cert.pub"),
		IdentityFile:    filepath.Join(home, "native-key"),
		ProxyCommand:    "tenki sandbox ssh-proxy --gateway wss://gateway.example/bridge/v1/ssh/session-authority-test --session session-authority-test",
	}
	certData := ssh.MarshalAuthorizedKey(cert)
	if err := os.WriteFile(output.CertificateFile, certData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output.IdentityFile, []byte("native private key remains untouched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return authorityTestMaterial{output: output, ca: ca.PublicKey(), clientKey: client.PublicKey(), clientCert: certData}
}

func authorityTestGateways() []*tenkiproto.ActiveSSHGateway {
	return []*tenkiproto.ActiveSSHGateway{{GatewayId: "gateway-a", Healthy: true, WsBridgeEndpoint: "wss://gateway.example/bridge"}}
}

type authorityTestIssuer func(context.Context, *connect.Request[tenkiproto.IssueSandboxSSHCertRequest]) (*connect.Response[tenkiproto.IssueSandboxSSHCertResponse], error)
type authorityTestDiscovery func(context.Context, *connect.Request[tenkiproto.ListActiveSSHGatewaysRequest]) (*connect.Response[tenkiproto.ListActiveSSHGatewaysResponse], error)

func authorityTestServer(t *testing.T, issue authorityTestIssuer, discovery authorityTestDiscovery) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(tenkiSSHAuthorityService+"IssueSandboxSSHCert", connect.NewUnaryHandler(tenkiSSHAuthorityService+"IssueSandboxSSHCert", issue))
	mux.Handle(tenkiSSHAuthorityService+"ListActiveSSHGateways", connect.NewUnaryHandler(tenkiSSHAuthorityService+"ListActiveSSHGateways", discovery))
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 || r.Header.Get("Content-Type") != "application/grpc" {
			t.Errorf("authority request did not use HTTP/2 gRPC: protocol=%s content-type=%q", r.Proto, r.Header.Get("Content-Type"))
		}
		if r.Header.Get("Authorization") != "Bearer "+authorityTestToken {
			t.Error("authority request did not use the selected synthetic workspace key")
		}
		mux.ServeHTTP(w, r)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func authorityTestPrepare(ctx context.Context, server *httptest.Server, output tenkiSSHCommandOutput) (string, string, error) {
	backend := &tenkiBackend{rt: core.Runtime{HTTP: server.Client()}}
	return backend.prepareSSHAuthority(ctx, core.Config{Tenki: core.TenkiConfig{Endpoint: server.URL}}, output)
}

func TestTenkiSSHAuthorityFetchesAuthenticatedSessionCA(t *testing.T) {
	material := authorityTestSetup(t)
	var issueCalls, discoveryCalls atomic.Int32
	server := authorityTestServer(t,
		func(_ context.Context, req *connect.Request[tenkiproto.IssueSandboxSSHCertRequest]) (*connect.Response[tenkiproto.IssueSandboxSSHCertResponse], error) {
			issueCalls.Add(1)
			if req.Msg.GetSessionId() != material.output.SessionID || req.Msg.GetPublicKey() != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(material.clientKey))) {
				t.Error("certificate request did not bind the CLI session and certified public key")
			}
			return connect.NewResponse(&tenkiproto.IssueSandboxSSHCertResponse{CaPub: string(ssh.MarshalAuthorizedKey(material.ca)), SshCert: "not installed by Crabbox"}), nil
		},
		func(_ context.Context, req *connect.Request[tenkiproto.ListActiveSSHGatewaysRequest]) (*connect.Response[tenkiproto.ListActiveSSHGatewaysResponse], error) {
			discoveryCalls.Add(1)
			if req.Msg.GetSessionId() != material.output.SessionID || req.Msg.GetRegion() != "" {
				t.Error("discovery request did not bind the exact session")
			}
			return connect.NewResponse(&tenkiproto.ListActiveSSHGatewaysResponse{Gateways: authorityTestGateways()}), nil
		})
	file, alias, err := authorityTestPrepare(context.Background(), server, material.output)
	if err != nil {
		t.Fatal(err)
	}
	if alias != "gateway-a" || issueCalls.Load() != 1 || discoveryCalls.Load() != 1 {
		t.Fatalf("alias=%q issue=%d discovery=%d", alias, issueCalls.Load(), discoveryCalls.Load())
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	want := "# Crabbox Tenki SSH authority for " + material.output.SessionID + "\n@cert-authority gateway-a " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(material.ca))) + "\n"
	if string(data) != want {
		t.Fatalf("authority contents=%q want=%q", data, want)
	}
	authorityTestAssertNativeMaterial(t, material)
}

func TestTenkiSSHAuthorityReportedFileTakesPriority(t *testing.T) {
	t.Setenv("TENKI_CONFIG_FILE", "../invalid")
	output := tenkiSSHCommandOutput{KnownHostsFile: "  /provider-owned/missing-authority  "}
	backend := &tenkiBackend{}
	file, alias, err := backend.prepareSSHAuthority(context.Background(), core.Config{}, output)
	if err != nil || file != "/provider-owned/missing-authority" || alias != "" {
		t.Fatalf("reported authority was not preserved without reading config or client material: file=%q alias=%q err=%v", file, alias, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := backend.prepareSSHAuthority(ctx, core.Config{}, output); !errors.Is(err, context.Canceled) {
		t.Fatalf("reported path ignored cancellation: %v", err)
	}
}

func TestTenkiSSHAuthorityRejectsInvalidCA(t *testing.T) {
	material := authorityTestSetup(t)
	public := string(ssh.MarshalAuthorizedKey(material.ca))
	for _, tc := range []struct{ name, value string }{
		{"missing", ""}, {"empty", " \n"}, {"malformed", "ssh-ed25519 invalid-base64"},
		{"certificate", string(material.clientCert)}, {"options", "restrict " + public},
		{"second key", public + public}, {"trailing material", public + "untrusted extra bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var discoveryCalls atomic.Int32
			server := authorityTestServer(t,
				func(context.Context, *connect.Request[tenkiproto.IssueSandboxSSHCertRequest]) (*connect.Response[tenkiproto.IssueSandboxSSHCertResponse], error) {
					return connect.NewResponse(&tenkiproto.IssueSandboxSSHCertResponse{CaPub: tc.value}), nil
				},
				func(context.Context, *connect.Request[tenkiproto.ListActiveSSHGatewaysRequest]) (*connect.Response[tenkiproto.ListActiveSSHGatewaysResponse], error) {
					discoveryCalls.Add(1)
					return connect.NewResponse(&tenkiproto.ListActiveSSHGatewaysResponse{Gateways: authorityTestGateways()}), nil
				})
			file, alias, err := authorityTestPrepare(context.Background(), server, material.output)
			if err == nil || !strings.Contains(err.Error(), "invalid SSH certificate authority") || file != "" || alias != "" || discoveryCalls.Load() != 0 {
				t.Fatalf("invalid CA accepted or discovery continued: file=%q alias=%q calls=%d err=%v", file, alias, discoveryCalls.Load(), err)
			}
		})
	}
	authorityTestAssertNativeMaterial(t, material)
}

func TestTenkiSSHAuthorityRejectsInvalidClientCertificateBeforeRequest(t *testing.T) {
	material := authorityTestSetup(t)
	hostCert := &ssh.Certificate{Key: material.clientKey, CertType: ssh.HostCert, ValidBefore: ssh.CertTimeInfinity}
	if err := hostCert.SignCert(rand.Reader, authorityTestSigner(t)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"missing", nil}, {"empty", []byte{}}, {"public key", ssh.MarshalAuthorizedKey(material.clientKey)},
		{"host certificate", ssh.MarshalAuthorizedKey(hostCert)}, {"trailing material", append(append([]byte{}, material.clientCert...), []byte("unexpected trailing material")...)},
		{"oversized", bytes.Repeat([]byte("x"), (1<<20)+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := material.output
			output.CertificateFile = filepath.Join(t.TempDir(), "client-cert.pub")
			if tc.data != nil {
				if err := os.WriteFile(output.CertificateFile, tc.data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			defer server.Close()
			if _, _, err := authorityTestPrepare(context.Background(), server, output); err == nil || calls.Load() != 0 {
				t.Fatalf("invalid client certificate reached authority API: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestTenkiGatewayAuthorityBindsExactAuthenticatedRoute(t *testing.T) {
	gateway, err := url.Parse("wss://gateway.example/bridge/v1/ssh/session-one")
	if err != nil {
		t.Fatal(err)
	}
	valid := func(endpoint string) *tenkiproto.ActiveSSHGateway {
		return &tenkiproto.ActiveSSHGateway{GatewayId: "gateway-a", Healthy: true, WsBridgeEndpoint: endpoint}
	}
	for _, tc := range []struct {
		name    string
		records []*tenkiproto.ActiveSSHGateway
		want    []string
	}{
		{"wss", []*tenkiproto.ActiveSSHGateway{valid("wss://gateway.example/bridge/")}, []string{"gateway-a"}},
		{"https", []*tenkiproto.ActiveSSHGateway{valid("https://gateway.example/bridge")}, []string{"gateway-a"}},
		{"scheme relative", []*tenkiproto.ActiveSSHGateway{valid("//gateway.example/bridge")}, []string{"gateway-a"}},
		{"healthy identities sorted", []*tenkiproto.ActiveSSHGateway{{GatewayId: "gateway-z", Healthy: true, WsBridgeEndpoint: "wss://other.example"}, valid("wss://gateway.example/bridge"), {GatewayId: "gateway-offline", Healthy: false}}, []string{"gateway-a", "gateway-z"}},
		{"different route", []*tenkiproto.ActiveSSHGateway{valid("wss://gateway.example/other")}, nil},
		{"different port", []*tenkiproto.ActiveSSHGateway{valid("wss://gateway.example:8443/bridge")}, nil},
		{"insecure http", []*tenkiproto.ActiveSSHGateway{valid("http://gateway.example/bridge")}, nil},
		{"insecure ws", []*tenkiproto.ActiveSSHGateway{valid("ws://gateway.example/bridge")}, nil},
		{"userinfo", []*tenkiproto.ActiveSSHGateway{valid("wss://user@gateway.example/bridge")}, nil},
		{"query", []*tenkiproto.ActiveSSHGateway{valid("wss://gateway.example/bridge?route=other")}, nil},
		{"fragment", []*tenkiproto.ActiveSSHGateway{valid("wss://gateway.example/bridge#other")}, nil},
		{"unhealthy", []*tenkiproto.ActiveSSHGateway{{GatewayId: "gateway-a", WsBridgeEndpoint: "wss://gateway.example/bridge"}}, nil},
		{"ambiguous", []*tenkiproto.ActiveSSHGateway{valid("wss://gateway.example/bridge"), {GatewayId: "gateway-b", Healthy: true, WsBridgeEndpoint: "wss://gateway.example/bridge"}}, nil},
		{"invalid identity", []*tenkiproto.ActiveSSHGateway{{GatewayId: "gateway-a,*", Healthy: true, WsBridgeEndpoint: "wss://gateway.example/bridge"}}, nil},
		{"missing identity", []*tenkiproto.ActiveSSHGateway{{Healthy: true, WsBridgeEndpoint: "wss://gateway.example/bridge"}}, nil},
		{"empty discovery", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alias, identities, err := tenkiGatewayAuthority(gateway, "session-one", tc.records)
			if tc.want == nil {
				if err == nil || alias != "" || len(identities) != 0 {
					t.Fatalf("unbound gateway accepted: alias=%q identities=%v err=%v", alias, identities, err)
				}
			} else if err != nil || alias != "gateway-a" || !reflect.DeepEqual(identities, tc.want) {
				t.Fatalf("alias=%q identities=%v err=%v want=%v", alias, identities, err, tc.want)
			}
		})
	}
}

func TestTenkiCommandGatewayRequiresExactSessionAndSecureRoute(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		valid         bool
	}{
		{"separate arguments", "tenki sandbox ssh-proxy --session session-one --gateway wss://gateway.example/v1/ssh/session-one", true},
		{"equals and quoting", "'/native cli/tenki' sandbox ssh-proxy --session=session-one --gateway='wss://gateway.example/v1/ssh/session-one'", true},
		{"different session", "tenki sandbox ssh-proxy --session session-other --gateway wss://gateway.example/v1/ssh/session-one", false},
		{"missing session", "tenki sandbox ssh-proxy --gateway wss://gateway.example/v1/ssh/session-one", false},
		{"duplicate session", "tenki sandbox ssh-proxy --session session-one --session session-one --gateway wss://gateway.example/v1/ssh/session-one", false},
		{"duplicate gateway", "tenki sandbox ssh-proxy --session session-one --gateway wss://gateway.example --gateway wss://gateway.example", false},
		{"missing gateway value", "tenki sandbox ssh-proxy --session session-one --gateway", false},
		{"insecure gateway", "tenki sandbox ssh-proxy --session session-one --gateway ws://gateway.example/v1/ssh/session-one", false},
		{"malformed shell", "tenki sandbox ssh-proxy --session session-one --gateway 'unterminated", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway, err := tenkiCommandGateway(tenkiSSHCommandOutput{SessionID: "session-one", ProxyCommand: tc.command})
			if (err == nil) != tc.valid || (tc.valid && gateway.String() != "wss://gateway.example/v1/ssh/session-one") {
				t.Fatalf("gateway=%v err=%v valid=%t", gateway, err, tc.valid)
			}
		})
	}
}

func TestTenkiSSHAuthoritySuppressesRemoteErrorDetails(t *testing.T) {
	material := authorityTestSetup(t)
	for _, operation := range []string{"certificate", "discovery"} {
		t.Run(operation, func(t *testing.T) {
			remoteError := connect.NewError(connect.CodePermissionDenied, fmt.Errorf("server echoed %s and sensitive-server-details", authorityTestToken))
			server := authorityTestServer(t,
				func(context.Context, *connect.Request[tenkiproto.IssueSandboxSSHCertRequest]) (*connect.Response[tenkiproto.IssueSandboxSSHCertResponse], error) {
					if operation == "certificate" {
						return nil, remoteError
					}
					return connect.NewResponse(&tenkiproto.IssueSandboxSSHCertResponse{CaPub: string(ssh.MarshalAuthorizedKey(material.ca))}), nil
				},
				func(context.Context, *connect.Request[tenkiproto.ListActiveSSHGatewaysRequest]) (*connect.Response[tenkiproto.ListActiveSSHGatewaysResponse], error) {
					return nil, remoteError
				})
			_, _, err := authorityTestPrepare(context.Background(), server, material.output)
			if err == nil || !strings.Contains(err.Error(), "permission_denied") || strings.Contains(err.Error(), authorityTestToken) || strings.Contains(err.Error(), "sensitive-server-details") {
				t.Fatalf("authority error lost status or exposed server details: %v", err)
			}
		})
	}
}

func TestTenkiSSHAuthorityRejectsCrossOriginRedirect(t *testing.T) {
	material := authorityTestSetup(t)
	var destinationCalls atomic.Int32
	destination := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+authorityTestToken {
					t.Error("source request missing selected synthetic key")
				}
				http.Redirect(w, r, destination.URL+"/sensitive-server-details", code)
			}))
			defer source.Close()
			_, _, err := authorityTestPrepare(context.Background(), source, material.output)
			if err == nil || strings.Contains(err.Error(), "sensitive-server-details") || strings.Contains(err.Error(), authorityTestToken) {
				t.Fatalf("redirect error absent or leaked details: %v", err)
			}
			if destinationCalls.Load() != 0 {
				t.Fatal("authority request crossed credential origin")
			}
		})
	}
}

func TestTenkiSSHAuthorityPreservesCancellation(t *testing.T) {
	material := authorityTestSetup(t)
	for _, operation := range []string{"certificate", "discovery"} {
		t.Run(operation, func(t *testing.T) {
			reached := make(chan struct{}, 1)
			wait := func(ctx context.Context) error {
				reached <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			}
			server := authorityTestServer(t,
				func(ctx context.Context, _ *connect.Request[tenkiproto.IssueSandboxSSHCertRequest]) (*connect.Response[tenkiproto.IssueSandboxSSHCertResponse], error) {
					if operation == "certificate" {
						return nil, wait(ctx)
					}
					return connect.NewResponse(&tenkiproto.IssueSandboxSSHCertResponse{CaPub: string(ssh.MarshalAuthorizedKey(material.ca))}), nil
				},
				func(ctx context.Context, _ *connect.Request[tenkiproto.ListActiveSSHGatewaysRequest]) (*connect.Response[tenkiproto.ListActiveSSHGatewaysResponse], error) {
					return nil, wait(ctx)
				})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			go func() {
				select {
				case <-reached:
					cancel()
				case <-ctx.Done():
				}
			}()
			_, _, err := authorityTestPrepare(ctx, server, material.output)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("request cancellation was not preserved: %v", err)
			}
		})
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, _, err := (&tenkiBackend{}).prepareSSHAuthority(ctx, core.Config{}, material.output); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired deadline was not preserved: %v", err)
	}
}

func TestWriteTenkiSSHAuthorityReplacesOnlyOwnedAuthority(t *testing.T) {
	material := authorityTestSetup(t)
	firstPath, err := writeTenkiSSHAuthority(material.output, []string{"gateway-a", "gateway-old"}, material.ca)
	if err != nil {
		t.Fatal(err)
	}
	oldData, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	oldInfo, err := os.Stat(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	newCA := authorityTestSigner(t).PublicKey()
	secondPath, err := writeTenkiSSHAuthority(material.output, []string{"gateway-a"}, newCA)
	if err != nil || firstPath != secondPath {
		t.Fatalf("replacement path=%q original=%q err=%v", secondPath, firstPath, err)
	}
	newData, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(oldData, newData) || bytes.Contains(newData, bytes.TrimSpace(ssh.MarshalAuthorizedKey(material.ca))) || bytes.Contains(newData, []byte("gateway-old")) || !bytes.Contains(newData, bytes.TrimSpace(ssh.MarshalAuthorizedKey(newCA))) {
		t.Fatalf("authority replacement retained stale trust: %s", newData)
	}
	newInfo, err := os.Stat(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && (newInfo.Mode().Perm() != 0o600 || os.SameFile(oldInfo, newInfo)) {
		t.Fatalf("authority was not privately and atomically replaced: mode=%v same-file=%t", newInfo.Mode(), os.SameFile(oldInfo, newInfo))
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(secondPath), ".crabbox-tenki-authority-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary authority files left behind: %v err=%v", matches, err)
	}
	authorityTestAssertNativeMaterial(t, material)
}

func TestWriteTenkiSSHAuthorityRefusesUnownedDestinations(t *testing.T) {
	for _, kind := range []string{"unmarked", "another session", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			material := authorityTestSetup(t)
			name := filepath.Join(filepath.Dir(material.output.CertificateFile), "crabbox_known_hosts_"+core.NormalizeLeaseSlug(material.output.SessionID))
			original := []byte("provider-owned contents\n")
			switch kind {
			case "unmarked", "another session":
				if kind == "another session" {
					original = []byte("# Crabbox Tenki SSH authority for another-session\n")
				}
				if err := os.WriteFile(name, original, 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(material.output.CertificateFile, name); err != nil {
					if runtime.GOOS == "windows" {
						t.Skipf("symlinks unavailable: %v", err)
					}
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(name, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if file, err := writeTenkiSSHAuthority(material.output, []string{"gateway-a"}, material.ca); err == nil || file != "" {
				t.Fatalf("unowned %s replaced: file=%q err=%v", kind, file, err)
			}
			if kind == "unmarked" || kind == "another session" {
				got, err := os.ReadFile(name)
				if err != nil || !bytes.Equal(got, original) {
					t.Fatalf("unowned file changed: contents=%q err=%v", got, err)
				}
			}
			authorityTestAssertNativeMaterial(t, material)
		})
	}
}

func TestWriteTenkiSSHAuthorityRejectsEmptyOrControlSession(t *testing.T) {
	material := authorityTestSetup(t)
	for _, session := range []string{"", "---", " \t ", "session\nother", "session\r", "session\x00"} {
		t.Run(fmt.Sprintf("%q", session), func(t *testing.T) {
			output := material.output
			output.SessionID = session
			if file, err := writeTenkiSSHAuthority(output, []string{"gateway-a"}, material.ca); err == nil || file != "" {
				t.Fatalf("invalid session accepted: file=%q err=%v", file, err)
			}
		})
	}
}

func authorityTestAssertNativeMaterial(t *testing.T, material authorityTestMaterial) {
	t.Helper()
	cert, err := os.ReadFile(material.output.CertificateFile)
	if err != nil || !bytes.Equal(cert, material.clientCert) {
		t.Fatalf("native certificate changed: err=%v", err)
	}
	key, err := os.ReadFile(material.output.IdentityFile)
	if err != nil || string(key) != "native private key remains untouched\n" {
		t.Fatalf("native identity changed: err=%v", err)
	}
}
