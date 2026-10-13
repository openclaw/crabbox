package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

const serveRelayAuthEnv = "CRABBOX_INTERNAL_SERVE_RELAY_AUTH"
const serveRelayChunk = 32 * 1024

// Relay admission/pairing may take 30 seconds before either SSH handshake
// starts. The inner connection must also allow the front-door handshake.
func relaySSHConnectTimeout(requested string, relay bool) string {
	seconds, err := strconv.Atoi(requested)
	if relay && err == nil && seconds > 0 && seconds < 60 {
		return "60"
	}
	return requested
}

func usesServeRelay(target SSHTarget) bool {
	return target.SSHJump != nil && target.SSHJump.useRelay
}

type serveRelayAuth struct {
	BaseURL          string
	Token            string
	TokenCommand     []string
	Access           AccessConfig
	ChildEnvDenylist []string
}

type relayStream struct {
	net.Conn
	once   sync.Once
	cancel context.CancelFunc
	socket *websocket.Conn
	other  net.Conn
	diag   *serveRelayDiagnostics
}

func (c *relayStream) Close() error {
	c.closeReason("local-close", nil)
	return nil
}

func (c *relayStream) closeReason(reason string, err error) {
	c.once.Do(func() {
		c.diag.closed(reason, err)
		c.cancel()
		_ = c.Conn.Close()
		_ = c.other.Close()
		_ = c.socket.CloseNow()
	})
}

// DialServeRelay authenticates with the ordinary coordinator credentials. The
// one-time agent token is a header, never a URL, process argument, or disk file.
func DialServeRelay(ctx context.Context, coordinator *CoordinatorClient, sessionID, role, agentToken, machineID string, diagnosticOutput ...io.Writer) (net.Conn, error) {
	if coordinator == nil || !validServeRelayID(sessionID) || (role != "client" && role != "agent") {
		return nil, fmt.Errorf("invalid serve relay endpoint")
	}
	diag := newServeRelayDiagnostics(sessionID, role, diagnosticOutput...)
	diag.event("dial", 0)
	connectCtx, cancelConnect := context.WithTimeout(ctx, 30*time.Second)
	defer cancelConnect()
	endpoint, err := url.Parse(coordinator.BaseURL)
	if err != nil {
		return nil, err
	}
	switch endpoint.Scheme {
	case "https":
		endpoint.Scheme = "wss"
	case "http":
		endpoint.Scheme = "ws"
	default:
		return nil, fmt.Errorf("invalid serve relay coordinator scheme")
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v1/serve/relay/" + sessionID + "/" + role
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	headers := make(http.Header)
	if err := coordinator.addRequestHeaders(connectCtx, headers); err != nil {
		return nil, err
	}
	if role == "agent" {
		headers.Set("X-Crabbox-Relay-Token", agentToken)
		headers.Set("X-Crabbox-Serve-Machine", machineID)
	}
	socket, _, err := websocket.Dial(connectCtx, endpoint.String(), &websocket.DialOptions{HTTPHeader: headers, HTTPClient: coordinator.secureHTTPClient(), CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		diag.closed("dial", err)
		return nil, fmt.Errorf("serve relay connection: %w", err)
	}
	socket.SetReadLimit(serveRelayChunk + 1)
	// Both sides wait for the pairing acknowledgement before starting SSH.
	readyCtx, cancelReady := context.WithTimeout(connectCtx, 30*time.Second)
	kind, data, err := socket.Read(readyCtx)
	cancelReady()
	if err != nil || kind != websocket.MessageBinary || len(data) != 1 || data[0] != 2 {
		diag.closed("pairing", err)
		_ = socket.CloseNow()
		return nil, fmt.Errorf("serve relay pairing failed")
	}
	diag.event("paired", 0)
	return newServeRelayStream(ctx, socket, diag), nil
}
func validServeRelayID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func newServeRelayStream(parent context.Context, socket *websocket.Conn, diag *serveRelayDiagnostics) net.Conn {
	ctx, cancel := context.WithCancel(parent)
	local, bridge := net.Pipe()
	stream := &relayStream{Conn: local, other: bridge, socket: socket, cancel: cancel, diag: diag}
	incoming := make(chan []byte, 1)
	ack := make(chan struct{}, 1)
	go func() {
		for {
			kind, frame, err := socket.Read(ctx)
			if err != nil || kind != websocket.MessageBinary || len(frame) < 1 {
				stream.closeReason("socket-read", err)
				return
			}
			switch frame[0] {
			case 0:
				if len(frame) < 2 {
					stream.closeReason("empty-data", nil)
					return
				}
				diag.event("receive", len(frame)-1)
				select {
				case incoming <- frame[1:]:
				default:
					stream.closeReason("receive-window-overflow", nil)
					return
				}
			case 1:
				if len(frame) != 1 {
					stream.closeReason("invalid-ack", nil)
					return
				}
				select {
				case ack <- struct{}{}:
				default:
					stream.closeReason("ack-window-overflow", nil)
					return
				}
			default:
				stream.closeReason("invalid-frame", nil)
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case data := <-incoming:
				if _, err := bridge.Write(data); err != nil {
					stream.closeReason("stream-write", err)
					return
				}
				if err := socket.Write(ctx, websocket.MessageBinary, []byte{1}); err != nil {
					stream.closeReason("ack-write", err)
					return
				}
				diag.event("ack-sent", 0)
			}
		}
	}()
	go func() {
		buffer := make([]byte, serveRelayChunk+1)
		for {
			n, err := bridge.Read(buffer[1:])
			if err != nil {
				stream.closeReason("stream-read", err)
				return
			}
			buffer[0] = 0
			diag.event("send", n)
			if err := socket.Write(ctx, websocket.MessageBinary, buffer[:n+1]); err != nil {
				stream.closeReason("data-write", err)
				return
			}
			select {
			case <-ctx.Done():
				stream.closeReason("context", ctx.Err())
				return
			case <-ack:
				diag.event("ack-received", 0)
			}
		}
	}()
	return stream
}

func serveRelayCoordinatorBinding(raw string) string {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Host == "" {
		return ""
	}
	endpoint.User, endpoint.RawQuery, endpoint.Fragment = nil, "", ""
	endpoint.Path = strings.TrimRight(endpoint.Path, "/")
	sum := sha256.Sum256([]byte(endpoint.String()))
	return hex.EncodeToString(sum[:])
}

func serveRelayProxyCommand(executable, leaseID, binding string) string {
	args := []string{executable, "__serve-relay-proxy", "--lease", leaseID, "--coordinator-binding", binding}
	for i, arg := range args {
		args[i] = ShellQuote(strings.ReplaceAll(arg, "%", "%%"))
	}
	return strings.Join(args, " ")
}
func prepareServeRelayProxy(target *SSHTarget) (string, error) {
	if target.relayCoordinator == nil {
		return "", fmt.Errorf("serve relay coordinator is unavailable")
	}
	c := target.relayCoordinator
	auth := serveRelayAuth{c.BaseURL, c.Token, c.TokenCommand, c.Access, c.ChildEnvDenylist}
	data, err := json.Marshal(auth)
	if err != nil {
		return "", err
	}
	child := map[string]string{}
	for k, v := range target.ChildEnv {
		child[k] = v
	}
	child[serveRelayAuthEnv] = string(data)
	target.ChildEnv = child
	for _, secret := range []string{string(data), c.Token, c.Access.Token, c.Access.ClientSecret} {
		if secret != "" {
			target.DiagnosticSecrets = append(target.DiagnosticSecrets, secret)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return serveRelayProxyCommand(executable, target.Host, serveRelayCoordinatorBinding(c.BaseURL)), nil
}
func (a App) serveRelayProxy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("__serve-relay-proxy", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	lease := fs.String("lease", "", "Lease ID")
	binding := fs.String("coordinator-binding", "", "Pinned coordinator destination")
	if err := fs.Parse(args); err != nil {
		return err
	}
	decodedBinding, bindingErr := hex.DecodeString(*binding)
	if fs.NArg() != 0 || !IsCanonicalLeaseID(*lease) || bindingErr != nil || len(decodedBinding) != sha256.Size {
		return Exit(2, "invalid serve relay lease")
	}
	encoded := os.Getenv(serveRelayAuthEnv)
	if len(encoded) > 64*1024 {
		return Exit(2, "invalid serve relay authentication")
	}
	var c *CoordinatorClient
	if encoded != "" {
		var auth serveRelayAuth
		if json.Unmarshal([]byte(encoded), &auth) != nil || auth.BaseURL == "" {
			return Exit(2, "serve relay authentication is unavailable")
		}
		c = &CoordinatorClient{BaseURL: auth.BaseURL, Token: auth.Token, TokenCommand: auth.TokenCommand, Access: auth.Access, ChildEnvDenylist: auth.ChildEnvDenylist}
	} else {
		// Printed SSH commands start outside the parent's child environment.
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		c, _, err = newCoordinatorClient(cfg)
		if err != nil {
			return err
		}
	}
	_ = os.Unsetenv(serveRelayAuthEnv)
	if c == nil || serveRelayCoordinatorBinding(c.BaseURL) != *binding {
		return Exit(2, "serve relay coordinator changed; generate the SSH command again")
	}
	var session struct {
		SessionID string `json:"sessionID"`
	}
	if err := c.AgentRequest(ctx, "POST", "/v1/serve/leases/"+*lease+"/relay", map[string]string{}, &session); err != nil {
		return err
	}
	conn, err := DialServeRelay(ctx, c, session.SessionID, "client", "", "", a.Stderr)
	if err != nil {
		return err
	}
	defer conn.Close()
	return bridgeServeRelay(ctx, conn, a.input(), a.Stdout)
}
func bridgeServeRelay(ctx context.Context, conn net.Conn, input io.Reader, output io.Writer) error {
	result := make(chan error, 2)
	go func() { _, err := io.Copy(conn, input); result <- err }()
	go func() { _, err := io.Copy(output, conn); result <- err }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-result:
		if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
}
