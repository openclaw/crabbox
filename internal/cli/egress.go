package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

const (
	defaultEgressListen     = "127.0.0.1:3128"
	egressRemoteBinary      = "/tmp/crabbox-egress-client"
	egressRemoteLog         = "/tmp/crabbox-egress-client.log"
	egressMaxMessageBytes   = 2 * 1024 * 1024
	egressCopyChunkBytes    = 32 * 1024
	egressOpenTimeout       = 20 * time.Second
	egressDialTimeout       = 15 * time.Second
	egressRemoteReadyWait   = 5 * time.Second
	egressDaemonRestartWait = 1 * time.Second
	egressDaemonFatalCode   = 4
)

type egressProxyMessage struct {
	Type  string `json:"type"`
	ID    string `json:"id,omitempty"`
	Host  string `json:"host,omitempty"`
	Port  string `json:"port,omitempty"`
	Body  string `json:"body,omitempty"`
	Error string `json:"error,omitempty"`
}

type egressOpenResult struct {
	err error
}

func (a App) egress(ctx context.Context, args []string) error {
	if len(args) == 0 || isHelpArg(args[0]) {
		a.printEgressHelp()
		if len(args) == 0 {
			return exit(2, "missing egress subcommand")
		}
		return nil
	}
	switch args[0] {
	case "host":
		return a.egressHost(ctx, args[1:])
	case "client":
		return a.egressClient(ctx, args[1:])
	case "start":
		return a.egressStart(ctx, args[1:])
	case "status":
		return a.egressStatus(ctx, args[1:])
	case "stop":
		return a.egressStop(ctx, args[1:])
	default:
		a.printEgressHelp()
		return exit(2, "unknown egress subcommand %q", args[0])
	}
}

func (a App) printEgressHelp() {
	fmt.Fprintln(a.Stdout, `Usage:
  crabbox egress start --id <lease-id-or-slug> --profile discord [--daemon]
  crabbox egress host --id <lease-id-or-slug> --profile discord
  crabbox egress client --id <lease-id-or-slug> --listen 127.0.0.1:3128
  crabbox egress status --id <lease-id-or-slug>
  crabbox egress stop --id <lease-id-or-slug>

Mediated egress lets a lease-local browser/app proxy exit through the machine
running the egress host agent. The coordinator only mediates paired WebSocket
bridges; the host agent opens the real outbound TCP connections.`)
}

func (a App) egressHost(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("egress host", a.Stderr)
	provider := fs.String("provider", defaults.Provider, "provider: hetzner or aws")
	id := fs.String("id", "", "lease id or slug")
	coordinatorURL := fs.String("coordinator", "", "coordinator URL override")
	ticket := fs.String("ticket", "", "pre-created egress host ticket")
	sessionID := fs.String("session", "", "egress session id")
	profile := fs.String("profile", "", "egress profile name")
	allowCSV := fs.String("allow", "", "comma-separated allowed host patterns")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	allow := egressAllowlist(*profile, splitCSV(*allowCSV))
	if *id == "" {
		return exit(2, "usage: crabbox egress host --id <lease-id-or-slug> --profile <name>|--allow <hosts>")
	}
	if len(allow) == 0 {
		return exit(2, "egress host requires --profile or --allow; refusing to start an open proxy")
	}
	coord, leaseID, err := a.egressCoordinatorAndLease(ctx, *provider, *coordinatorURL, *id)
	if err != nil {
		return err
	}
	bridge, err := connectEgressBridge(ctx, coord, leaseID, "host", *ticket, *sessionID, *profile, allow)
	if err != nil {
		if fatalEgressBridgeSetupError(err) {
			return exit(egressDaemonFatalCode, "egress lease unavailable: %v", err)
		}
		return err
	}
	fmt.Fprintf(a.Stdout, "egress host: connected lease=%s session=%s profile=%s allow=%s\n", leaseID, bridge.sessionID, blank(*profile, "-"), strings.Join(allow, ","))
	return bridge.serveHost(ctx, allow)
}

func (a App) egressClient(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("egress client", a.Stderr)
	provider := fs.String("provider", defaults.Provider, "provider: hetzner or aws")
	id := fs.String("id", "", "lease id or slug")
	coordinatorURL := fs.String("coordinator", "", "coordinator URL override")
	ticket := fs.String("ticket", "", "pre-created egress client ticket")
	sessionID := fs.String("session", "", "egress session id")
	listen := fs.String("listen", defaultEgressListen, "lease-local proxy listen address")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	if *id == "" {
		return exit(2, "usage: crabbox egress client --id <lease-id-or-slug> [--listen 127.0.0.1:3128]")
	}
	if err := validateEgressListen(*listen); err != nil {
		return err
	}
	coord, leaseID, err := a.egressCoordinatorAndLease(ctx, *provider, *coordinatorURL, *id)
	if err != nil {
		return err
	}
	bridge, err := connectEgressBridge(ctx, coord, leaseID, "client", *ticket, *sessionID, "", nil)
	if err != nil {
		if fatalEgressBridgeSetupError(err) {
			return exit(egressDaemonFatalCode, "egress lease unavailable: %v", err)
		}
		return err
	}
	fmt.Fprintf(a.Stdout, "egress client: connected lease=%s session=%s listen=%s\n", leaseID, bridge.sessionID, *listen)
	return bridge.serveClient(ctx, *listen)
}

func (a App) egressStart(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("egress start", a.Stderr)
	provider := fs.String("provider", defaults.Provider, "provider: hetzner or aws")
	id := fs.String("id", "", "lease id or slug")
	profile := fs.String("profile", "", "egress profile name")
	allowCSV := fs.String("allow", "", "comma-separated allowed host patterns")
	listen := fs.String("listen", defaultEgressListen, "lease-local proxy listen address")
	coordinatorURL := fs.String("coordinator", "", "coordinator URL override")
	daemon := fs.Bool("daemon", false, "start the local host bridge in the background")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	if *id == "" {
		return exit(2, "usage: crabbox egress start --id <lease-id-or-slug> --profile <name>|--allow <hosts>")
	}
	allow := egressAllowlist(*profile, splitCSV(*allowCSV))
	if len(allow) == 0 {
		return exit(2, "egress start requires --profile or --allow; refusing to start an open proxy")
	}
	if err := validateEgressListen(*listen); err != nil {
		return err
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{})
	if err != nil {
		return err
	}
	cfg, err = egressStartCoordinatorConfig(cfg, *coordinatorURL)
	if err != nil {
		return err
	}
	coord, useCoordinator, err := newTargetCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	if !useCoordinator || coord == nil || coord.Token == "" {
		return exit(2, "egress start requires a configured coordinator login; run crabbox login first")
	}
	server, target, leaseID, err := a.resolveNetworkLeaseTarget(ctx, cfg, *id, false)
	if err != nil {
		return err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	sessionID := newLocalEgressSessionID()
	if err := installRemoteEgressClient(ctx, target); err != nil {
		return err
	}
	clientTicket, err := coord.CreateEgressTicket(ctx, leaseID, "client", sessionID, *profile, allow)
	if err != nil {
		return err
	}
	remote := remoteEgressClientCommand(coord.BaseURL, leaseID, clientTicket.Ticket, sessionID, *listen)
	if err := runSSHQuiet(ctx, target, remote); err != nil {
		return exit(5, "start remote egress client: %v", err)
	}
	if err := waitRemoteEgressClient(ctx, target, *listen); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "egress client: lease=%s listen=%s log=%s\n", leaseID, *listen, egressRemoteLog)
	hostArgs := []string{
		"host",
		"--provider", cfg.Provider,
		"--id", leaseID,
		"--coordinator", coord.BaseURL,
		"--session", sessionID,
	}
	if strings.TrimSpace(*profile) != "" {
		hostArgs = append(hostArgs, "--profile", strings.TrimSpace(*profile))
	}
	if len(allow) > 0 {
		hostArgs = append(hostArgs, "--allow", strings.Join(allow, ","))
	}
	if *daemon {
		return a.startEgressHostDaemon(leaseID, hostArgs)
	}
	hostTicket, err := coord.CreateEgressTicket(ctx, leaseID, "host", sessionID, *profile, allow)
	if err != nil {
		return err
	}
	hostArgs = append(hostArgs, "--ticket", hostTicket.Ticket)
	return a.egressHost(ctx, hostArgs)
}

func (a App) egressStatus(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("egress status", a.Stderr)
	provider := fs.String("provider", defaults.Provider, "provider: hetzner or aws")
	id := fs.String("id", "", "lease id or slug")
	coordinatorURL := fs.String("coordinator", "", "coordinator URL override")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	if *id == "" {
		return exit(2, "usage: crabbox egress status --id <lease-id-or-slug>")
	}
	coord, leaseID, err := a.egressCoordinatorAndLease(ctx, *provider, *coordinatorURL, *id)
	if err != nil {
		return err
	}
	status, err := coord.EgressStatus(ctx, leaseID)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "egress: lease=%s session=%s profile=%s host=%t client=%t allow=%s\n", status.LeaseID, blank(status.SessionID, "-"), blank(status.Profile, "-"), status.HostConnected, status.ClientConnected, strings.Join(status.Allow, ","))
	return nil
}

func (a App) egressStop(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("egress stop", a.Stderr)
	provider := fs.String("provider", defaults.Provider, "provider: hetzner or aws")
	id := fs.String("id", "", "lease id or slug")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	if *id == "" {
		return exit(2, "usage: crabbox egress stop --id <lease-id-or-slug>")
	}
	stoppedLocal, err := a.stopEgressHostDaemon(*id)
	if err != nil {
		return err
	}
	cfg, cfgErr := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{})
	if cfgErr == nil {
		if _, target, leaseID, resolveErr := a.resolveNetworkLeaseTarget(ctx, cfg, *id, false); resolveErr == nil {
			_ = runSSHQuiet(ctx, target, "pkill -f '[c]rabbox-egress-client egress client' >/dev/null 2>&1 || true")
			if leaseID != *id && !stoppedLocal {
				stoppedLocal, _ = a.stopEgressHostDaemon(leaseID)
			}
			fmt.Fprintf(a.Stdout, "egress remote client: stopped lease=%s\n", leaseID)
		}
	}
	if !stoppedLocal {
		fmt.Fprintf(a.Stdout, "egress host daemon: no local daemon for %s\n", *id)
	}
	return nil
}

func (a App) egressCoordinatorAndLease(ctx context.Context, provider, coordinatorURL, id string) (*CoordinatorClient, string, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, "", err
	}
	cfg.Provider = provider
	if strings.TrimSpace(coordinatorURL) != "" {
		cfg.Coordinator = strings.TrimRight(strings.TrimSpace(coordinatorURL), "/")
		cfg.CoordToken = firstNonBlank(cfg.CoordToken, "ticket-only")
	}
	coord, useCoordinator, err := newTargetCoordinatorClient(cfg)
	if err != nil {
		return nil, "", err
	}
	if !useCoordinator || coord == nil || coord.BaseURL == "" {
		return nil, "", exit(2, "egress requires a configured coordinator")
	}
	if strings.TrimSpace(coordinatorURL) != "" && coord.Token == "ticket-only" {
		return coord, id, nil
	}
	if coord.Token == "" {
		return nil, "", exit(2, "egress requires a configured coordinator login; run crabbox login first")
	}
	lease, err := coord.GetLease(ctx, id)
	if err != nil {
		return nil, "", err
	}
	return coord, lease.ID, nil
}

type egressBridge struct {
	ws        *websocket.Conn
	sessionID string
	writeMu   sync.Mutex
	mu        sync.Mutex
	conns     map[string]net.Conn
	pending   map[string]chan egressOpenResult
}

func connectEgressBridge(ctx context.Context, coord *CoordinatorClient, leaseID, role, ticket, sessionID, profile string, allow []string) (*egressBridge, error) {
	if strings.TrimSpace(ticket) == "" {
		resolvedSessionID, err := reusableEgressSessionID(ctx, coord, leaseID, sessionID)
		if err != nil {
			return nil, err
		}
		sessionID = resolvedSessionID
		created, err := coord.CreateEgressTicket(ctx, leaseID, role, sessionID, profile, allow)
		if err != nil {
			return nil, err
		}
		ticket = created.Ticket
		sessionID = created.SessionID
	} else if strings.TrimSpace(sessionID) == "" {
		sessionID = "egress_manual"
	}
	ws, _, err := websocket.Dial(ctx, egressAgentURL(coord.BaseURL, leaseID, role, ticket), &websocket.DialOptions{
		HTTPHeader: coord.webVNCAccessHeaders(),
	})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(egressMaxMessageBytes)
	return &egressBridge{
		ws:        ws,
		sessionID: sessionID,
		conns:     map[string]net.Conn{},
		pending:   map[string]chan egressOpenResult{},
	}, nil
}

func fatalEgressBridgeSetupError(err error) bool {
	var httpErr CoordinatorHTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	switch httpErr.StatusCode {
	case http.StatusForbidden, http.StatusNotFound, http.StatusGone, http.StatusConflict:
		return true
	default:
		return false
	}
}

func reusableEgressSessionID(ctx context.Context, coord *CoordinatorClient, leaseID, sessionID string) (string, error) {
	if strings.TrimSpace(sessionID) != "" {
		return strings.TrimSpace(sessionID), nil
	}
	status, err := coord.EgressStatus(ctx, leaseID)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(status.SessionID), nil
}

func (b *egressBridge) serveHost(ctx context.Context, allow []string) error {
	defer b.close()
	for {
		var msg egressProxyMessage
		if err := b.readMessage(ctx, &msg); err != nil {
			return err
		}
		switch msg.Type {
		case "open":
			go b.hostOpen(ctx, msg, allow)
		case "data":
			b.writeConn(msg)
		case "close":
			b.closeConn(msg.ID)
		}
	}
}

func (b *egressBridge) hostOpen(ctx context.Context, msg egressProxyMessage, allow []string) {
	if !egressHostAllowed(msg.Host, allow) {
		_ = b.writeJSON(ctx, egressProxyMessage{Type: "error", ID: msg.ID, Error: "host not allowed"})
		return
	}
	conn, err := (&net.Dialer{Timeout: egressDialTimeout}).DialContext(ctx, "tcp", net.JoinHostPort(msg.Host, msg.Port))
	if err != nil {
		_ = b.writeJSON(ctx, egressProxyMessage{Type: "error", ID: msg.ID, Error: err.Error()})
		return
	}
	b.mu.Lock()
	b.conns[msg.ID] = conn
	b.mu.Unlock()
	if err := b.writeJSON(ctx, egressProxyMessage{Type: "open_ok", ID: msg.ID}); err != nil {
		_ = conn.Close()
		return
	}
	go b.copyConnToBridge(ctx, msg.ID, conn)
}

func (b *egressBridge) serveClient(ctx context.Context, listen string) error {
	defer b.close()
	if err := validateEgressListen(listen); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	errc := make(chan error, 2)
	go func() { errc <- b.clientReadLoop(ctx) }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				errc <- err
				return
			}
			go b.handleProxyConn(ctx, conn)
		}
	}()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case err := <-errc:
		return err
	}
}

func (b *egressBridge) clientReadLoop(ctx context.Context) error {
	for {
		var msg egressProxyMessage
		if err := b.readMessage(ctx, &msg); err != nil {
			return err
		}
		switch msg.Type {
		case "open_ok":
			b.finishOpen(msg.ID, nil)
		case "error":
			b.finishOpen(msg.ID, errors.New(msg.Error))
			b.closeConn(msg.ID)
		case "data":
			b.writeConn(msg)
		case "close":
			b.closeConn(msg.ID)
		}
	}
}

func (b *egressBridge) handleProxyConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	host, port, err := egressRequestHostPort(req)
	if err != nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\n\r\n")
		return
	}
	id := newLocalEgressConnID()
	if err := b.openRemote(ctx, id, host, port); err != nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	b.mu.Lock()
	b.conns[id] = conn
	b.mu.Unlock()
	defer b.closeConn(id)
	if req.Method == http.MethodConnect {
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\nProxy-Agent: crabbox\r\n\r\n")
	} else {
		var buf bytes.Buffer
		req.RequestURI = ""
		req.URL.Scheme = ""
		req.URL.Host = ""
		if err := req.Write(&buf); err != nil {
			return
		}
		if err := b.writeJSON(ctx, egressProxyMessage{Type: "data", ID: id, Body: base64.StdEncoding.EncodeToString(buf.Bytes())}); err != nil {
			return
		}
	}
	if reader.Buffered() > 0 {
		buffered, _ := reader.Peek(reader.Buffered())
		if len(buffered) > 0 {
			_ = b.writeJSON(ctx, egressProxyMessage{Type: "data", ID: id, Body: base64.StdEncoding.EncodeToString(buffered)})
		}
	}
	b.copyConnToBridge(ctx, id, conn)
}

func (b *egressBridge) openRemote(ctx context.Context, id, host, port string) error {
	ch := make(chan egressOpenResult, 1)
	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()
	if err := b.writeJSON(ctx, egressProxyMessage{Type: "open", ID: id, Host: host, Port: port}); err != nil {
		return err
	}
	timer := time.NewTimer(egressOpenTimeout)
	defer timer.Stop()
	select {
	case result := <-ch:
		return result.err
	case <-timer.C:
		return errors.New("egress open timed out")
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (b *egressBridge) finishOpen(id string, err error) {
	b.mu.Lock()
	ch := b.pending[id]
	delete(b.pending, id)
	b.mu.Unlock()
	if ch != nil {
		ch <- egressOpenResult{err: err}
	}
}

func (b *egressBridge) copyConnToBridge(ctx context.Context, id string, conn net.Conn) {
	buf := make([]byte, egressCopyChunkBytes)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if writeErr := b.writeJSON(ctx, egressProxyMessage{
				Type: "data",
				ID:   id,
				Body: base64.StdEncoding.EncodeToString(buf[:n]),
			}); writeErr != nil {
				return
			}
		}
		if err != nil {
			_ = b.writeJSON(ctx, egressProxyMessage{Type: "close", ID: id})
			b.closeConn(id)
			return
		}
	}
}

func (b *egressBridge) writeConn(msg egressProxyMessage) {
	data, err := base64.StdEncoding.DecodeString(msg.Body)
	if err != nil {
		return
	}
	b.mu.Lock()
	conn := b.conns[msg.ID]
	b.mu.Unlock()
	if conn != nil {
		_, _ = conn.Write(data)
	}
}

func (b *egressBridge) closeConn(id string) {
	b.mu.Lock()
	conn := b.conns[id]
	delete(b.conns, id)
	delete(b.pending, id)
	b.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (b *egressBridge) readMessage(ctx context.Context, msg *egressProxyMessage) error {
	_, data, err := b.ws.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, msg)
}

func (b *egressBridge) writeJSON(ctx context.Context, msg egressProxyMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	return b.ws.Write(ctx, websocket.MessageText, data)
}

func (b *egressBridge) close() {
	if b == nil {
		return
	}
	_ = b.ws.Close(websocket.StatusNormalClosure, "egress stopped")
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, conn := range b.conns {
		_ = conn.Close()
		delete(b.conns, id)
	}
	for id, ch := range b.pending {
		ch <- egressOpenResult{err: errors.New("egress bridge stopped")}
		delete(b.pending, id)
	}
}

func egressRequestHostPort(req *http.Request) (string, string, error) {
	hostport := req.Host
	if req.URL != nil && req.URL.Host != "" {
		hostport = req.URL.Host
	}
	if hostport == "" {
		return "", "", errors.New("missing host")
	}
	host, port, err := net.SplitHostPort(hostport)
	if err == nil {
		return strings.ToLower(strings.Trim(host, "[]")), port, nil
	}
	host = strings.ToLower(strings.Trim(hostport, "[]"))
	if req.Method == http.MethodConnect {
		return "", "", fmt.Errorf("CONNECT target must include port: %s", hostport)
	}
	if req.URL != nil && req.URL.Scheme == "https" {
		return host, "443", nil
	}
	return host, "80", nil
}

func egressAllowlist(profile string, explicit []string) []string {
	out := sanitizeEgressAllowlist(explicit)
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "discord":
		out = append(out, "discord.com", "*.discord.com", "discordcdn.com", "*.discordcdn.com", "hcaptcha.com", "*.hcaptcha.com")
	case "slack":
		out = append(out, "slack.com", "*.slack.com", "slack-edge.com", "*.slack-edge.com")
	}
	return sanitizeEgressAllowlist(out)
}

func sanitizeEgressAllowlist(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized == "" || normalized == "*" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	return out
}

func egressHostAllowed(host string, allow []string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return false
	}
	for _, pattern := range allow {
		pattern = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(pattern)), ".")
		switch {
		case strings.HasPrefix(pattern, "*."):
			suffix := strings.TrimPrefix(pattern, "*.")
			if host == suffix || strings.HasSuffix(host, "."+suffix) {
				return true
			}
		case host == pattern:
			return true
		}
	}
	return false
}

func validateEgressListen(listen string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil || strings.TrimSpace(port) == "" {
		return exit(2, "invalid egress listen address %q; use 127.0.0.1:<port>", listen)
	}
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return exit(2, "egress listen address must be loopback-only; use 127.0.0.1:<port>")
	}
	return nil
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if normalized := strings.TrimSpace(part); normalized != "" {
			out = append(out, normalized)
		}
	}
	return out
}

func egressCoordinatorNeedsAccess(access AccessConfig) bool {
	return strings.TrimSpace(access.ClientID) != "" ||
		strings.TrimSpace(access.ClientSecret) != "" ||
		strings.TrimSpace(access.Token) != ""
}

func egressStartCoordinatorConfig(cfg Config, coordinatorURL string) (Config, error) {
	if override := strings.TrimSpace(coordinatorURL); override != "" {
		cfg.Coordinator = strings.TrimRight(override, "/")
		cfg.Access = AccessConfig{}
		return cfg, nil
	}
	if egressCoordinatorNeedsAccess(cfg.Access) {
		return cfg, exit(2, "egress start cannot install a remote client when coordinator Access credentials are configured; use --coordinator with a public coordinator route or run egress client manually with safe credentials")
	}
	return cfg, nil
}

func egressAgentURL(base, leaseID, role, ticket string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/leases/" + url.PathEscape(leaseID) + "/egress/" + role
	values := url.Values{}
	values.Set("ticket", ticket)
	u.RawQuery = values.Encode()
	u.Fragment = ""
	return u.String()
}

func newLocalEgressSessionID() string {
	return "egress_" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func newLocalEgressConnID() string {
	return "conn_" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func installRemoteEgressClient(ctx context.Context, target SSHTarget) error {
	exe, cleanup, err := egressClientBinaryForTarget(ctx, target)
	if err != nil {
		return err
	}
	defer cleanup()
	args := append(scpBaseArgs(target), exe, target.User+"@"+target.Host+":"+egressRemoteBinary)
	cmd := exec.CommandContext(ctx, "scp", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return exit(5, "copy egress client: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return runSSHQuiet(ctx, target, "chmod 700 "+shellQuote(egressRemoteBinary))
}

func egressClientBinaryForTarget(ctx context.Context, target SSHTarget) (string, func(), error) {
	exe, err := os.Executable()
	if err != nil {
		return "", func() {}, exit(2, "resolve crabbox executable: %v", err)
	}
	if target.TargetOS != "" && target.TargetOS != targetLinux {
		return "", func() {}, exit(2, "egress start only supports Linux lease targets; target=%s is not supported", target.TargetOS)
	}
	if runtime.GOOS == "linux" {
		return exe, func() {}, nil
	}
	repo, err := findRepo()
	if err != nil {
		return "", func() {}, exit(2, "cross-build egress client: %v", err)
	}
	out := filepath.Join(os.TempDir(), "crabbox-egress-client-linux-amd64-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", out, "./cmd/crabbox")
	cmd.Dir = repo.Root
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if data, err := cmd.CombinedOutput(); err != nil {
		return "", func() {}, exit(5, "cross-build linux egress client: %v: %s", err, strings.TrimSpace(string(data)))
	}
	return out, func() { _ = os.Remove(out) }, nil
}

func scpBaseArgs(target SSHTarget) []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + sshConfigFileValue(knownHostsFile(target)),
		"-o", "ConnectTimeout=10",
		"-o", "ConnectionAttempts=3",
		"-P", target.Port,
	}
	if target.Key != "" {
		args = append([]string{"-i", target.Key, "-o", "IdentitiesOnly=yes"}, args...)
	}
	return args
}

func remoteEgressClientCommand(coordinatorURL, leaseID, ticket, sessionID, listen string) string {
	args := []string{
		egressRemoteBinary,
		"egress",
		"client",
		"--coordinator", coordinatorURL,
		"--id", leaseID,
		"--ticket", ticket,
		"--session", sessionID,
		"--listen", listen,
	}
	var b strings.Builder
	b.WriteString("pkill -f '[c]rabbox-egress-client egress client' >/dev/null 2>&1 || true\n")
	b.WriteString("nohup ")
	for i, arg := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(shellQuote(arg))
	}
	b.WriteString(" >" + shellQuote(egressRemoteLog) + " 2>&1 < /dev/null &\n")
	return b.String()
}

func waitRemoteEgressClient(ctx context.Context, target SSHTarget, listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return exit(2, "invalid egress listen address %q", listen)
	}
	deadline := time.Now().Add(egressRemoteReadyWait)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if runSSHQuiet(ctx, target, egressRemoteProbeCommand(host, port)) == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return exit(5, "remote egress client did not listen on %s; inspect %s", listen, egressRemoteLog)
}

func egressRemoteProbeCommand(host, port string) string {
	return "if command -v nc >/dev/null 2>&1; then nc -z " + shellQuote(host) + " " + shellQuote(port) + " >/dev/null 2>&1; else timeout 1 bash -lc " + shellQuote("</dev/tcp/"+host+"/"+port) + " >/dev/null 2>&1; fi"
}

func (a App) startEgressHostDaemon(leaseID string, args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return exit(2, "resolve crabbox executable: %v", err)
	}
	if stopped, err := a.stopEgressHostDaemon(leaseID); err != nil {
		return err
	} else if stopped {
		fmt.Fprintln(a.Stdout, "egress host daemon: replacing previous daemon")
	}
	logPath, pidPath, err := egressDaemonPaths(leaseID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return exit(2, "create egress daemon directory: %v", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return exit(2, "open egress daemon log: %v", err)
	}
	defer logFile.Close()
	childArgs := append([]string{"egress"}, args...)
	cmd := exec.Command("sh", "-c", egressDaemonSupervisorScript(exe, childArgs))
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	configureDaemonCommand(cmd)
	if err := cmd.Start(); err != nil {
		return exit(5, "start egress daemon: %v", err)
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", pid)), 0o600); err != nil {
		_ = cmd.Process.Kill()
		return exit(2, "write egress daemon pid: %v", err)
	}
	if err := cmd.Process.Release(); err != nil {
		return exit(5, "release egress daemon process: %v", err)
	}
	fmt.Fprintf(a.Stdout, "egress host daemon: pid=%d log=%s\n", pid, logPath)
	return nil
}

func (a App) stopEgressHostDaemon(leaseID string) (bool, error) {
	_, pidPath, err := egressDaemonPaths(leaseID)
	if err != nil {
		return false, err
	}
	pid, err := readWebVNCDaemonPID(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	command, alive := webVNCDaemonProcessCommand(pid)
	if !alive {
		_ = os.Remove(pidPath)
		fmt.Fprintf(a.Stdout, "egress host daemon: removed stale pid=%d\n", pid)
		return true, nil
	}
	if !isEgressDaemonCommand(command) {
		return false, exit(5, "refusing to stop pid %d; command does not look like crabbox egress: %s", pid, strings.TrimSpace(command))
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, exit(5, "find egress daemon pid %d: %v", pid, err)
	}
	if err := stopDaemonProcess(process, pid); err != nil {
		return false, exit(5, "stop egress daemon pid %d: %v", pid, err)
	}
	_ = os.Remove(pidPath)
	fmt.Fprintf(a.Stdout, "egress host daemon: stopped pid=%d\n", pid)
	return true, nil
}

func isEgressDaemonCommand(command string) bool {
	command = strings.ToLower(command)
	return strings.Contains(command, "crabbox") && strings.Contains(command, "egress")
}

func egressDaemonSupervisorScript(exe string, args []string) string {
	argv := make([]string, 0, len(args)+1)
	argv = append(argv, shellQuote(exe))
	for _, arg := range args {
		argv = append(argv, shellQuote(arg))
	}
	return "set -u\n" +
		"echo 'egress daemon supervisor: starting'\n" +
		"while :; do\n" +
		"  " + strings.Join(argv, " ") + "\n" +
		"  code=$?\n" +
		"  if [ \"$code\" = " + strconv.Itoa(egressDaemonFatalCode) + " ]; then\n" +
		"    echo \"egress daemon supervisor: child exited fatal code=$code; stopping\"\n" +
		"    exit \"$code\"\n" +
		"  fi\n" +
		"  echo \"egress daemon supervisor: child exited code=$code; restarting in 1s\"\n" +
		"  sleep " + strconv.Itoa(int(egressDaemonRestartWait/time.Second)) + "\n" +
		"done\n"
}

func egressDaemonPaths(leaseID string) (string, string, error) {
	dir, err := crabboxStateDir()
	if err != nil {
		return "", "", err
	}
	bridgeDir := filepath.Join(dir, "egress")
	name := safeWebVNCDaemonName(leaseID)
	return filepath.Join(bridgeDir, name+".log"), filepath.Join(bridgeDir, name+".pid"), nil
}
