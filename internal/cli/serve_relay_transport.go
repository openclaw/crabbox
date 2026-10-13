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
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

const serveRelayAuthEnv = "CRABBOX_INTERNAL_SERVE_RELAY_AUTH"
const serveRelayChunk = 32 * 1024

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
}

func (c *relayStream) Close() error {
	c.once.Do(func() { c.cancel(); _ = c.Conn.Close(); _ = c.other.Close(); _ = c.socket.CloseNow() })
	return nil
}

// DialServeRelay authenticates with the ordinary coordinator credentials. The
// one-time agent token is a header, never a URL, process argument, or disk file.
func DialServeRelay(ctx context.Context, coordinator *CoordinatorClient, sessionID, role, agentToken, machineID string) (net.Conn, error) {
	if coordinator == nil || !validServeRelayID(sessionID) || (role != "client" && role != "agent") {
		return nil, fmt.Errorf("invalid serve relay endpoint")
	}
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
		return nil, fmt.Errorf("serve relay connection: %w", err)
	}
	socket.SetReadLimit(serveRelayChunk + 1)
	// Both sides wait for the pairing acknowledgement before starting SSH.
	readyCtx, cancelReady := context.WithTimeout(connectCtx, 30*time.Second)
	kind, data, err := socket.Read(readyCtx)
	cancelReady()
	if err != nil || kind != websocket.MessageBinary || len(data) != 1 || data[0] != 2 {
		_ = socket.CloseNow()
		return nil, fmt.Errorf("serve relay pairing failed")
	}
	return newServeRelayStream(ctx, socket), nil
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
func newServeRelayStream(parent context.Context, socket *websocket.Conn) net.Conn {
	ctx, cancel := context.WithCancel(parent)
	local, bridge := net.Pipe()
	stream := &relayStream{Conn: local, other: bridge, socket: socket, cancel: cancel}
	incoming := make(chan []byte, 1)
	ack := make(chan struct{}, 1)
	go func() {
		defer stream.Close()
		for {
			kind, frame, err := socket.Read(ctx)
			if err != nil || kind != websocket.MessageBinary || len(frame) < 1 {
				return
			}
			switch frame[0] {
			case 0:
				if len(frame) < 2 {
					return
				}
				select {
				case incoming <- frame[1:]:
				default:
					return
				}
			case 1:
				if len(frame) != 1 {
					return
				}
				select {
				case ack <- struct{}{}:
				default:
					return
				}
			default:
				return
			}
		}
	}()
	go func() {
		defer stream.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case data := <-incoming:
				if _, err := bridge.Write(data); err != nil {
					return
				}
				if socket.Write(ctx, websocket.MessageBinary, []byte{1}) != nil {
					return
				}
			}
		}
	}()
	go func() {
		defer stream.Close()
		buffer := make([]byte, serveRelayChunk+1)
		for {
			n, err := bridge.Read(buffer[1:])
			if err != nil {
				return
			}
			buffer[0] = 0
			if socket.Write(ctx, websocket.MessageBinary, buffer[:n+1]) != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ack:
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
	conn, err := DialServeRelay(ctx, c, session.SessionID, "client", "", "")
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
