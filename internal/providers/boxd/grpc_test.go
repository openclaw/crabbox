package boxd

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/boxd/boxdapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testAPIKey = "bxd_" + "F1xtureF1xtureF1xtureF1xtureF1xtureF1xture0"
const testJWT = "fixture-jwt-never-log"

func noSecrets(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	for _, secret := range []string{testAPIKey, testJWT, "unsafe-body", "token="} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("unsafe diagnostic: %v", err)
		}
	}
}

// exchangeServer serves the HTTPS API-key exchange. Its TLS certificate also
// backs the fake gRPC listener so one injected HTTP client pins both.
func exchangeServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	return server
}

func validExchange(t *testing.T, exchanges *atomic.Int32) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if exchanges != nil {
			exchanges.Add(1)
		}
		if r.Method != http.MethodPost || r.URL.Path != exchangeRoute {
			t.Errorf("unexpected exchange request: %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			APIKey string `json:"api_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.APIKey != testAPIKey {
			t.Error("exchange did not carry the API key")
		}
		json.NewEncoder(w).Encode(map[string]any{"token": testJWT, "expires_at": time.Now().Add(time.Hour).Unix(), "user_id": "alice"})
	}
}

func newTestClient(t *testing.T, server *httptest.Server, grpcTarget string) *apiClient {
	t.Helper()
	cfg := core.BaseConfig()
	cfg.Boxd.APIURL = server.URL
	cfg.Boxd.GRPCURL = grpcTarget
	c, err := newAPIClient(cfg, core.Runtime{HTTP: server.Client()}, testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// execOnly is a minimal fake BoxdApi exposing a scripted Exec stream.
type execOnly struct {
	boxdapi.UnimplementedBoxdApiServer
	handle func(stream grpc.BidiStreamingServer[boxdapi.ExecChunk, boxdapi.ExecChunk]) error
}

func (f *execOnly) Exec(stream grpc.BidiStreamingServer[boxdapi.ExecChunk, boxdapi.ExecChunk]) error {
	return f.handle(stream)
}

func startGRPC(t *testing.T, server *httptest.Server, api boxdapi.BoxdApiServer) string {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: server.TLS.Certificates, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	boxdapi.RegisterBoxdApiServer(s, api)
	go s.Serve(listener)
	t.Cleanup(s.Stop)
	return listener.Addr().String()
}

func TestOriginTargetAndMachineIDValidation(t *testing.T) {
	for _, raw := range []string{"http://app.boxd.sh", "https://user:pass@app.boxd.sh", "https://app.boxd.sh/api", "https://app.boxd.sh?token=unsafe", "https://app.boxd.sh#unsafe", "https://app.boxd.sh?", "https://app.boxd.sh#", "//app.boxd.sh", "https://app.boxd.sh/%2f"} {
		if _, err := consoleURL(raw); err == nil {
			t.Fatalf("accepted origin %q", raw)
		}
	}
	for _, raw := range []string{"", "https://APP.BOXD.SH:443/"} {
		u, err := consoleURL(raw)
		if err != nil || u.String() != defaultConsoleURL {
			t.Fatalf("normalize %q: %v %v", raw, u, err)
		}
	}
	for _, raw := range []string{"https://boxd.sh:9443", "boxd.sh", "boxd.sh:0", "boxd.sh:70000", "boxd.sh:9443/x", "boxd.sh:9443?x", "a b:9443", ":9443"} {
		if _, err := grpcTarget(raw); err == nil {
			t.Fatalf("accepted gRPC target %q", raw)
		}
	}
	for raw, want := range map[string]string{"": defaultGRPCTarget, "BOXD.SH:9443": "boxd.sh:9443", " boxd.sh:9443 ": "boxd.sh:9443"} {
		got, err := grpcTarget(raw)
		if err != nil || got != want {
			t.Fatalf("target %q: %q %v", raw, got, err)
		}
	}
	for _, id := range []string{"", "..", "x/y", "x?token=y", "x%2fy", "a\n", strings.Repeat("a", 129)} {
		if err := validateMachineID(id); err == nil {
			t.Fatalf("accepted ID %q", id)
		}
	}
}

func TestAPIKeyValidationAndEnvMigration(t *testing.T) {
	for _, key := range []string{"", "bxd_short", "bxd_" + strings.Repeat("!", 43), testAPIKey + "x", strings.TrimPrefix(testAPIKey, "bxd_"), "eyJhbGciOi.fixture.jwt"} {
		if err := validateAPIKey(key); err == nil {
			t.Fatalf("accepted key %q", key)
		}
	}
	if err := validateAPIKey(testAPIKey); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_BOXD_API_KEY", "")
	t.Setenv("BOXD_API_KEY", "")
	t.Setenv("CRABBOX_BOXD_TOKEN", "fixture-legacy-session")
	t.Setenv("BOXD_TOKEN", "")
	_, err := apiKeyFromEnv()
	if err == nil || !strings.Contains(err.Error(), "no longer used") || !strings.Contains(err.Error(), "BOXD_API_KEY") {
		t.Fatalf("missing migration guidance: %v", err)
	}
	if strings.Contains(err.Error(), "fixture-legacy-session") {
		t.Fatal("echoed legacy credential")
	}
	t.Setenv("CRABBOX_BOXD_TOKEN", "")
	if _, err := apiKeyFromEnv(); err == nil || !strings.Contains(err.Error(), "BOXD_API_KEY") {
		t.Fatalf("missing key guidance: %v", err)
	}
	t.Setenv("BOXD_API_KEY", testAPIKey)
	key, err := apiKeyFromEnv()
	if err != nil || key != testAPIKey {
		t.Fatal("valid key not accepted")
	}
}

func TestExchangeCachesRefreshesAndSanitizes(t *testing.T) {
	var exchanges atomic.Int32
	server := exchangeServer(t, validExchange(t, &exchanges))
	cfg := core.BaseConfig()
	cfg.Boxd.APIURL = server.URL
	session, err := newAuthSession(cfg, core.Runtime{HTTP: server.Client()}, testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		jwt, err := session.bearer(context.Background())
		if err != nil || jwt != testJWT {
			t.Fatal("exchange failed")
		}
	}
	if exchanges.Load() != 1 {
		t.Fatal("valid session was re-exchanged")
	}
	session.expiry = time.Now().Add(jwtRefreshMargin - time.Second)
	if _, err := session.bearer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exchanges.Load() != 2 {
		t.Fatal("expiring session was not refreshed")
	}
	if server.Client().CheckRedirect != nil || server.Client().Timeout != 0 {
		t.Fatal("mutated runtime client")
	}

	for name, handler := range map[string]http.HandlerFunc{
		"rejected": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(401)
			fmt.Fprint(w, testAPIKey+testJWT+"unsafe-body")
		},
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://invalid.example/?token="+testJWT)
			w.WriteHeader(307)
		},
		"malformed": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, testJWT+"unsafe-body")
		},
		"empty-token": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"token": "", "expires_at": time.Now().Add(time.Hour).Unix()})
		},
		"expired": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"token": testJWT, "expires_at": time.Now().Unix()})
		},
		"large": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, strings.Repeat("x", maxExchangeResponse+1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := exchangeServer(t, handler)
			cfg := core.BaseConfig()
			cfg.Boxd.APIURL = server.URL
			session, err := newAuthSession(cfg, core.Runtime{HTTP: server.Client()}, testAPIKey)
			if err != nil {
				t.Fatal(err)
			}
			_, err = session.bearer(context.Background())
			noSecrets(t, err)
			if name == "rejected" && !strings.Contains(err.Error(), "API key") {
				t.Fatalf("missing rejection guidance: %v", err)
			}
		})
	}
}

func TestExchangeTransportErrorsSanitized(t *testing.T) {
	server := exchangeServer(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected request") })
	client := *server.Client()
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New(testAPIKey + testJWT + "?token=unsafe-body")
	})
	cfg := core.BaseConfig()
	cfg.Boxd.APIURL = server.URL
	session, err := newAuthSession(cfg, core.Runtime{HTTP: &client}, testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.bearer(context.Background())
	noSecrets(t, err)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGRPCStatusErrorsSanitizedAndGuided(t *testing.T) {
	server := exchangeServer(t, validExchange(t, nil))
	target := startGRPC(t, server, &boxdapi.UnimplementedBoxdApiServer{})
	c := newTestClient(t, server, target)
	_, err := c.whoami(context.Background())
	noSecrets(t, err)
	var rejection *grpcStatusError
	if !errors.As(err, &rejection) {
		t.Fatalf("expected a status error: %v", err)
	}
	_, err = c.create(context.Background(), "fixture")
	noSecrets(t, err)
	unauth := &grpcStatusError{Code: codes.Unauthenticated}
	if !strings.Contains(unauth.Error(), "BOXD_API_KEY") {
		t.Fatal("missing API-key guidance")
	}
	if err := c.action(context.Background(), "vm-1", "arbitrary"); err == nil {
		t.Fatal("action whitelist bypassed")
	}
}

func TestExecBootstrapProtocolAndFailures(t *testing.T) {
	key := fixturePublicKey(t)
	for _, mode := range []string{"success", "clean-eof", "error", "exit", "bad-key", "duplicate", "missing-key", "stderr-marker", "large", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			server := exchangeServer(t, validExchange(t, nil))
			fake := &execOnly{handle: func(stream grpc.BidiStreamingServer[boxdapi.ExecChunk, boxdapi.ExecChunk]) error {
				first, err := stream.Recv()
				if err != nil {
					return err
				}
				if first.GetVmId() != "vm-1" || strings.Contains(first.GetCommand(), hostKeyMarker) || !strings.HasPrefix(first.GetCommand(), "bash -c 'set -o pipefail") {
					t.Error("unsafe exec input")
				}
				if mode == "cancel" {
					<-stream.Context().Done()
					return stream.Context().Err()
				}
				output := "\r\n" + hostKeyMarker + key + "\r\n"
				switch mode {
				case "bad-key":
					output = hostKeyMarker + testJWT
				case "duplicate":
					output += output
				case "missing-key":
					output = "unsafe-body"
				case "large":
					output = strings.Repeat("x", maxExecOutput+1)
				}
				isStderr := mode == "stderr-marker"
				if err := stream.Send(&boxdapi.ExecChunk{Data: []byte(output), IsStderr: isStderr}); err != nil {
					return err
				}
				switch mode {
				case "error":
					return status.Error(codes.Internal, "fixture stream failure "+testJWT)
				case "exit":
					return stream.Send(&boxdapi.ExecChunk{ExitCode: 1})
				case "clean-eof":
					return nil
				}
				return stream.Send(&boxdapi.ExecChunk{ExitCode: 0})
			}}
			target := startGRPC(t, server, fake)
			c := newTestClient(t, server, target)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			got, err := c.bootstrap(ctx, "vm-1", key)
			switch mode {
			case "success", "clean-eof":
				if err != nil || got != key {
					t.Fatalf("key=%q error=%v", got, err)
				}
			case "cancel":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("cancellation=%v", err)
				}
			default:
				noSecrets(t, err)
			}
		})
	}
}

func TestNoPlaintextFallbackOrCredentialBytes(t *testing.T) {
	server := exchangeServer(t, validExchange(t, nil))
	// A plain TCP listener stands in for a plaintext-only endpoint: the TLS
	// handshake must fail closed, with no h2c fallback and no credential
	// bytes on the wire (a ClientHello is expected; a JWT or API key is not).
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	var mu sync.Mutex
	var captured []byte
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				data, _ := io.ReadAll(io.LimitReader(conn, 1<<20))
				mu.Lock()
				captured = append(captured, data...)
				mu.Unlock()
				conn.Close()
			}()
		}
	}()
	c := newTestClient(t, server, listener.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = c.whoami(ctx)
	if err == nil {
		t.Fatal("connected without TLS")
	}
	noSecrets(t, err)
	mu.Lock()
	wire := string(captured)
	mu.Unlock()
	if strings.Contains(wire, testJWT) || strings.Contains(wire, testAPIKey) {
		t.Fatal("credential bytes reached the plaintext endpoint")
	}
}

func TestIdentityComesFromWhoamiNotLocalClaims(t *testing.T) {
	server := exchangeServer(t, func(w http.ResponseWriter, r *http.Request) {
		// The exchange result carries a user_id; only the Whoami RPC's answer
		// may be trusted for ownership fencing.
		json.NewEncoder(w).Encode(map[string]any{"token": testJWT, "expires_at": time.Now().Add(time.Hour).Unix(), "user_id": "forged-account"})
	})
	fake := &whoamiOnly{user: "server-account"}
	target := startGRPC(t, server, fake)
	c := newTestClient(t, server, target)
	user, err := c.whoami(context.Background())
	if err != nil || user != "server-account" {
		t.Fatalf("identity=%s err=%v", user, err)
	}
}

type whoamiOnly struct {
	boxdapi.UnimplementedBoxdApiServer
	user string
}

func (f *whoamiOnly) Whoami(context.Context, *boxdapi.WhoamiRequest) (*boxdapi.WhoamiResponse, error) {
	return &boxdapi.WhoamiResponse{UserId: f.user}, nil
}

// misroutedVM answers every GetVm and ExposePort with a fixed machine,
// standing in for a server that name-resolves onto the wrong resource.
type misroutedVM struct {
	boxdapi.UnimplementedBoxdApiServer
}

func (misroutedVM) GetVm(context.Context, *boxdapi.GetVmRequest) (*boxdapi.GetVmResponse, error) {
	return &boxdapi.GetVmResponse{VmId: "other-vm", Status: "running", Isolated: true}, nil
}
func (misroutedVM) ExposePort(context.Context, *boxdapi.ExposePortRequest) (*boxdapi.ExposePortResponse, error) {
	return &boxdapi.ExposePortResponse{VmId: "other-vm", VmPort: 2222, Protocol: "tcp", PublicPort: 32222}, nil
}

func TestMismatchedMachineIdentityFailsClosed(t *testing.T) {
	server := exchangeServer(t, validExchange(t, nil))
	target := startGRPC(t, server, misroutedVM{})
	c := newTestClient(t, server, target)
	if _, _, err := c.getVM(context.Background(), "vm-1"); err == nil {
		t.Fatal("accepted a machine whose immutable ID does not match the request")
	}
	if _, err := c.exposeSSH(context.Background(), "vm-1"); err == nil {
		t.Fatal("accepted a forward on a different machine")
	}
}

func TestBootstrapScriptUsesOnlyLeaseKey(t *testing.T) {
	key := fixturePublicKey(t)
	command, err := bootstrapCommand(key)
	if err != nil {
		t.Fatal(err)
	}
	encoded := strings.Split(strings.Split(command, "printf %s ")[1], " ")[0]
	script, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PasswordAuthentication no", "KbdInteractiveAuthentication no", "AuthenticationMethods publickey", "AllowUsers boxd", "AuthorizedKeysFile /etc/crabbox-ssh/authorized_keys", "HostKey /etc/ssh/ssh_host_ed25519_key", "sudo -n bash"} {
		if !strings.Contains(string(script)+command, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(command, hostKeyMarker) {
		t.Fatal("command transcript could spoof host-key marker")
	}
	if _, err := bootstrapCommand("ssh-ed25519 AAAA invalid trailing"); err == nil {
		t.Fatal("accepted malformed public key")
	}
}
