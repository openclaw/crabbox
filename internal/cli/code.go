package cli

import (
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
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nhooyr.io/websocket"
)

type coordinatorCodeTicket struct {
	Ticket    string `json:"ticket"`
	LeaseID   string `json:"leaseID"`
	ExpiresAt string `json:"expiresAt"`
}

type codeProxyMessage struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Method  string            `json:"method,omitempty"`
	Path    string            `json:"path,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Status  int               `json:"status,omitempty"`
	Body    string            `json:"body,omitempty"`
	Error   string            `json:"error,omitempty"`
	Code    int               `json:"code,omitempty"`
	Reason  string            `json:"reason,omitempty"`
	Frame   string            `json:"frame,omitempty"`
	ChunkID string            `json:"chunkID,omitempty"`
}

const (
	maxCodeBridgeBodyChunkBytes           = 15 * 1024
	maxCodeBridgeReadBytes                = 64 * 1024 * 1024
	maxPendingCodeBridgeWebSocketMessages = 32
	codeBridgeBodyChunkDelay              = 5 * time.Millisecond
)

func (a App) webCode(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("code", a.Stderr)
	provider := fs.String("provider", defaults.Provider, "provider: hetzner, aws, or azure")
	id := fs.String("id", "", "lease id or slug")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	localPort := fs.String("local-port", "", "local code-server tunnel port")
	openPortal := fs.Bool("open", false, "open the web portal code page")
	networkFlags := registerNetworkModeFlag(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" && fs.NArg() > 0 {
		*id = fs.Arg(0)
	}
	if *id == "" {
		return exit(2, "usage: crabbox code --id <lease-id-or-slug>")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	cfg.Code = true
	if err := applyNetworkModeFlagOverride(&cfg, fs, networkFlags); err != nil {
		return err
	}
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return err
	}
	if err := validateRequestedCapabilities(cfg); err != nil {
		return err
	}
	if isBlacksmithProvider(cfg.Provider) || isStaticProvider(cfg.Provider) {
		return exit(2, "code currently supports coordinator-backed hetzner/aws Linux leases")
	}
	coord, useCoordinator, err := newTargetCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	if !useCoordinator || coord == nil || coord.Token == "" {
		return exit(2, "code requires a configured coordinator login; run crabbox login first")
	}
	server, target, leaseID, err := a.resolveLeaseTarget(ctx, cfg, *id)
	if err != nil {
		return err
	}
	if resolved, err := resolveNetworkTarget(ctx, cfg, server, target); err != nil {
		return err
	} else {
		target = resolved.Target
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if err := claimLeaseForRepoConfig(leaseID, serverSlug(server), cfg, repo.Root, cfg.IdleTimeout, *reclaim); err != nil {
		return err
	}
	a.touchLeaseTargetBestEffort(ctx, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, "")
	workspace, folder, hydratedByActions := codeWorkspace(ctx, target, cfg, leaseID, repo)
	if hydratedByActions {
		fmt.Fprintf(a.Stderr, "using GitHub Actions workspace %s\n", workspace)
	}
	if folder != workspace {
		fmt.Fprintf(a.Stderr, "opening remote folder %s\n", folder)
	}
	if err := ensureRemoteCodeServer(ctx, target, workspace); err != nil {
		return err
	}
	if *localPort == "" {
		*localPort = availableLocalCodePort()
	}
	tunnel, err := startVNCForegroundTunnel(ctx, target, *localPort, "127.0.0.1", managedCodePort)
	if err != nil {
		return err
	}
	defer stopProcess(tunnel)
	portal := webCodePortalURL(coord.BaseURL, leaseID, folder)

	opened := false
	for {
		bridge, err := connectCodeBridge(ctx, coord, leaseID, "127.0.0.1", *localPort)
		if err != nil {
			return err
		}
		fmt.Fprintln(a.Stdout, "bridge: connected; keep this process running while using Code")
		fmt.Fprintf(a.Stdout, "code: %s\n", portal)
		if *openPortal && !opened {
			if err := openLocalURL(portal); err != nil {
				bridge.Close(websocket.StatusNormalClosure, "bridge stopped")
				return err
			}
			opened = true
			fmt.Fprintf(a.Stdout, "opened: %s\n", portal)
		}
		err = bridge.Serve(ctx)
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if !isRetryableCodeBridgeError(err) {
			return err
		}
		fmt.Fprintln(a.Stdout, "bridge: disconnected; reconnecting")
		time.Sleep(300 * time.Millisecond)
	}
}

func ensureRemoteCodeServer(ctx context.Context, target SSHTarget, workdir string) error {
	if err := runSSHQuiet(ctx, target, startCodeServerCommand(workdir)); err != nil {
		return exit(5, "start code-server: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if err := runSSHQuiet(ctx, target, codeServerReadyCommand()); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return exit(5, "timed out waiting for code-server on 127.0.0.1:%s", managedCodePort)
}

func codeWorkspace(ctx context.Context, target SSHTarget, cfg Config, leaseID string, repo Repo) (string, string, bool) {
	workspace := remoteJoin(cfg, leaseID, repo.Name)
	hydrated := false
	if state, err := readActionsHydrationState(ctx, target, leaseID); err == nil && state.Workspace != "" {
		workspace = state.Workspace
		hydrated = true
	}
	return workspace, mappedRemoteCodeFolder(workspace, repo), hydrated
}

func mappedRemoteCodeFolder(workspace string, repo Repo) string {
	wd, err := os.Getwd()
	if err != nil || repo.Root == "" {
		return workspace
	}
	rel, err := filepath.Rel(repo.Root, wd)
	if err != nil || rel == "." {
		return workspace
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return workspace
	}
	return path.Join(workspace, rel)
}

func codeServerReadyCommand() string {
	return "curl -fsS http://127.0.0.1:" + managedCodePort + "/healthz >/dev/null || curl -fsS http://127.0.0.1:" + managedCodePort + "/ >/dev/null"
}

func startCodeServerCommand(workdir string) string {
	pidfile := "/tmp/crabbox-code-server.pid"
	return strings.Join([]string{
		"mkdir -p " + shellQuote(workdir),
		"pidfile=" + shellQuote(pidfile) + "; if [ -s \"$pidfile\" ]; then oldpid=$(cat \"$pidfile\" 2>/dev/null || true); if [ -n \"$oldpid\" ] && kill -0 \"$oldpid\" 2>/dev/null; then kill \"$oldpid\" 2>/dev/null || true; for i in 1 2 3 4 5 6 7 8 9 10; do kill -0 \"$oldpid\" 2>/dev/null || break; sleep 0.2; done; if kill -0 \"$oldpid\" 2>/dev/null; then kill -9 \"$oldpid\" 2>/dev/null || true; fi; fi; fi",
		codeServerSettingsCommand(),
		"(nohup env VSCODE_PROXY_URI='./proxy/{{port}}' " + codeServerBinary +
			" --auth none --bind-addr 127.0.0.1:" + managedCodePort +
			" --disable-telemetry --disable-update-check " + shellQuote(workdir) +
			" >/tmp/crabbox-code-server.log 2>&1 & echo $! >" + shellQuote(pidfile) + ")",
	}, " && ")
}

func codeServerSettingsCommand() string {
	return `settings="$HOME/.local/share/code-server/User/settings.json"; mkdir -p "$(dirname "$settings")"; tmp=$(mktemp); if [ -s "$settings" ] && command -v jq >/dev/null 2>&1 && jq '. + {"workbench.colorTheme":"Default Dark Modern"}' "$settings" > "$tmp"; then mv "$tmp" "$settings"; else printf '%s\n' '{"workbench.colorTheme":"Default Dark Modern"}' > "$settings"; rm -f "$tmp"; fi`
}

type codeBridge struct {
	ws             *websocket.Conn
	baseURL        string
	client         *http.Client
	debug          bool
	mu             sync.Mutex
	writeMu        sync.Mutex
	upstream       map[string]*websocket.Conn
	pending        map[string][]codeProxyMessage
	incomingFrames map[string]codePendingWebSocketFrame
	chunkSeq       atomic.Uint64
}

type codePendingWebSocketFrame struct {
	id     string
	frame  string
	chunks []string
}

func connectCodeBridge(ctx context.Context, coord *CoordinatorClient, leaseID, host, port string) (*codeBridge, error) {
	ticket, err := coord.CreateCodeTicket(ctx, leaseID)
	if err != nil {
		return nil, err
	}
	ws, resp, err := websocket.Dial(ctx, webCodeAgentURL(coord.BaseURL, leaseID), &websocket.DialOptions{
		HTTPHeader: bridgeTicketHeaders(coord, ticket.Ticket),
	})
	if retryBridgeTicketInQuery(resp, err) {
		ws, _, err = websocket.Dial(ctx, webCodeAgentURLWithTicket(coord.BaseURL, leaseID, ticket.Ticket), &websocket.DialOptions{
			HTTPHeader: coord.webVNCAccessHeaders(),
		})
	}
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(maxCodeBridgeReadBytes)
	return &codeBridge{
		ws:      ws,
		baseURL: "http://" + host + ":" + port,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		debug:          os.Getenv("CRABBOX_CODE_DEBUG") == "1",
		upstream:       map[string]*websocket.Conn{},
		pending:        map[string][]codeProxyMessage{},
		incomingFrames: map[string]codePendingWebSocketFrame{},
	}, nil
}

func (b *codeBridge) Serve(ctx context.Context) error {
	defer b.Close(websocket.StatusNormalClosure, "bridge stopped")
	for {
		_, data, err := b.ws.Read(ctx)
		if err != nil {
			b.trace("agent_read_error error=%v", err)
			return err
		}
		var msg codeProxyMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "http":
			go b.handleHTTP(ctx, msg)
		case "ws_open":
			go b.openUpstreamWebSocket(ctx, msg)
		case "ws_data":
			b.writeUpstreamWebSocket(ctx, msg)
		case "ws_start":
			b.startIncomingWebSocketFrame(msg)
		case "ws_body":
			b.appendIncomingWebSocketFrame(msg)
		case "ws_end":
			b.finishIncomingWebSocketFrame(ctx, msg)
		case "ws_close":
			b.closeUpstreamWebSocket(msg.ID, websocket.StatusCode(msg.Code), msg.Reason)
		}
	}
}

func (b *codeBridge) Close(code websocket.StatusCode, reason string) {
	if b == nil {
		return
	}
	if b.ws != nil {
		_ = b.ws.Close(code, reason)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, conn := range b.upstream {
		_ = conn.Close(websocket.StatusNormalClosure, "bridge stopped")
		delete(b.upstream, id)
	}
	for id := range b.pending {
		delete(b.pending, id)
	}
	for id := range b.incomingFrames {
		delete(b.incomingFrames, id)
	}
}

func (b *codeBridge) handleHTTP(ctx context.Context, msg codeProxyMessage) {
	body, _ := base64.StdEncoding.DecodeString(msg.Body)
	upstreamPath := codeUpstreamPath(msg.Path)
	upstream := b.baseURL + upstreamPath
	req, err := http.NewRequestWithContext(ctx, msg.Method, upstream, bytes.NewReader(body))
	if err != nil {
		_ = b.writeJSON(ctx, codeProxyMessage{Type: "http", ID: msg.ID, Status: 502, Error: err.Error()})
		return
	}
	for key, value := range msg.Headers {
		req.Header.Set(key, value)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		_ = b.writeJSON(ctx, codeProxyMessage{Type: "http", ID: msg.ID, Status: 502, Error: err.Error()})
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 25*1024*1024))
	if err != nil {
		_ = b.writeJSON(ctx, codeProxyMessage{Type: "http", ID: msg.ID, Status: 502, Error: err.Error()})
		return
	}
	if fallbackBody, fallbackHeaders, ok := codeServerStaticFallback(upstreamPath, resp.StatusCode); ok {
		respBody = fallbackBody
		resp.Header = fallbackHeaders
		resp.StatusCode = http.StatusOK
	}
	if isCodeHTML(resp.Header.Get("content-type")) {
		respBody = rewriteCodeHTML(respBody)
	}
	headers := map[string]string{}
	for key, values := range resp.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}
	message := codeProxyMessage{
		Type:    "http",
		ID:      msg.ID,
		Status:  resp.StatusCode,
		Headers: headers,
	}
	if len(respBody) <= maxCodeBridgeBodyChunkBytes {
		message.Body = base64.StdEncoding.EncodeToString(respBody)
		_ = b.writeJSON(ctx, message)
		return
	}
	message.Type = "http_start"
	if err := b.writeJSON(ctx, message); err != nil {
		return
	}
	for len(respBody) > 0 {
		n := min(len(respBody), maxCodeBridgeBodyChunkBytes)
		if err := b.writeJSON(ctx, codeProxyMessage{
			Type: "http_body",
			ID:   msg.ID,
			Body: base64.StdEncoding.EncodeToString(respBody[:n]),
		}); err != nil {
			return
		}
		respBody = respBody[n:]
		if len(respBody) > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(codeBridgeBodyChunkDelay):
			}
		}
	}
	_ = b.writeJSON(ctx, codeProxyMessage{Type: "http_end", ID: msg.ID})
}

func (b *codeBridge) openUpstreamWebSocket(ctx context.Context, msg codeProxyMessage) {
	upstream := "ws" + strings.TrimPrefix(b.baseURL, "http") + codeUpstreamPath(msg.Path)
	b.trace("ws_open id=%s path=%s upstream=%s", msg.ID, msg.Path, upstream)
	header, subprotocols := codeWebSocketDialHeaders(b.baseURL, msg.Headers)
	b.trace("ws_open_headers id=%s cookie=%t origin=%q subprotocols=%d", msg.ID, header.Get("Cookie") != "", header.Get("Origin"), len(subprotocols))
	conn, _, err := websocket.Dial(ctx, upstream, &websocket.DialOptions{
		HTTPHeader:   header,
		Subprotocols: subprotocols,
	})
	if err != nil {
		b.trace("ws_open_error id=%s error=%v", msg.ID, err)
		_ = b.writeJSON(ctx, codeProxyMessage{Type: "ws_close", ID: msg.ID, Code: int(websocket.StatusInternalError), Reason: err.Error()})
		return
	}
	conn.SetReadLimit(maxCodeBridgeReadBytes)
	b.mu.Lock()
	b.upstream[msg.ID] = conn
	pending := append([]codeProxyMessage(nil), b.pending[msg.ID]...)
	delete(b.pending, msg.ID)
	b.mu.Unlock()
	b.trace("ws_open_ok id=%s subprotocols=%d pending=%d", msg.ID, len(subprotocols), len(pending))
	for _, pendingMessage := range pending {
		if err := b.writeUpstreamFrame(ctx, conn, pendingMessage); err != nil {
			b.trace("ws_pending_write_error id=%s error=%v", msg.ID, err)
			b.closeUpstreamWebSocket(msg.ID, websocket.StatusInternalError, err.Error())
			_ = b.writeJSON(ctx, codeProxyMessage{Type: "ws_close", ID: msg.ID, Code: int(websocket.StatusInternalError), Reason: err.Error()})
			return
		}
	}
	go b.readUpstreamWebSocket(ctx, msg.ID, conn)
}

func (b *codeBridge) readUpstreamWebSocket(ctx context.Context, id string, conn *websocket.Conn) {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			reason := err.Error()
			var closeErr websocket.CloseError
			code := int(websocket.StatusNormalClosure)
			if errors.As(err, &closeErr) {
				code = int(closeErr.Code)
				reason = closeErr.Reason
			}
			b.trace("ws_upstream_close id=%s code=%d reason=%s", id, code, reason)
			_ = b.writeJSON(ctx, codeProxyMessage{Type: "ws_close", ID: id, Code: code, Reason: reason})
			b.closeUpstreamWebSocket(id, websocket.StatusNormalClosure, "closed")
			return
		}
		b.trace("ws_upstream_data id=%s frame=%s bytes=%d", id, codeFrameType(typ), len(data))
		_ = b.writeWebSocketData(ctx, id, typ, data)
	}
}

func (b *codeBridge) writeWebSocketData(ctx context.Context, id string, typ websocket.MessageType, data []byte) error {
	frame := codeFrameType(typ)
	if len(data) <= maxCodeBridgeBodyChunkBytes {
		return b.writeJSON(ctx, codeProxyMessage{Type: "ws_data", ID: id, Frame: frame, Body: base64.StdEncoding.EncodeToString(data)})
	}
	chunkID := fmt.Sprintf("%s-%d", id, b.chunkSeq.Add(1))
	if err := b.writeJSON(ctx, codeProxyMessage{Type: "ws_start", ID: id, Frame: frame, ChunkID: chunkID}); err != nil {
		return err
	}
	for len(data) > 0 {
		n := min(len(data), maxCodeBridgeBodyChunkBytes)
		if err := b.writeJSON(ctx, codeProxyMessage{Type: "ws_body", ID: id, ChunkID: chunkID, Body: base64.StdEncoding.EncodeToString(data[:n])}); err != nil {
			return err
		}
		data = data[n:]
		if len(data) > 0 {
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case <-time.After(codeBridgeBodyChunkDelay):
			}
		}
	}
	return b.writeJSON(ctx, codeProxyMessage{Type: "ws_end", ID: id, ChunkID: chunkID})
}

func (b *codeBridge) writeUpstreamWebSocket(ctx context.Context, msg codeProxyMessage) {
	b.mu.Lock()
	conn := b.upstream[msg.ID]
	if conn == nil {
		pending := b.pending[msg.ID]
		if len(pending) >= maxPendingCodeBridgeWebSocketMessages {
			b.mu.Unlock()
			b.trace("ws_downstream_drop id=%s frame=%s pending=%d", msg.ID, msg.Frame, len(pending))
			_ = b.writeJSON(ctx, codeProxyMessage{Type: "ws_close", ID: msg.ID, Code: int(websocket.StatusPolicyViolation), Reason: "too many pending websocket messages"})
			return
		}
		b.pending[msg.ID] = append(pending, msg)
		b.mu.Unlock()
		b.trace("ws_downstream_buffered id=%s frame=%s pending=%d", msg.ID, msg.Frame, len(pending)+1)
		return
	}
	b.mu.Unlock()
	if err := b.writeUpstreamFrame(ctx, conn, msg); err != nil {
		b.trace("ws_downstream_write_error id=%s error=%v", msg.ID, err)
		b.closeUpstreamWebSocket(msg.ID, websocket.StatusInternalError, err.Error())
		_ = b.writeJSON(ctx, codeProxyMessage{Type: "ws_close", ID: msg.ID, Code: int(websocket.StatusInternalError), Reason: err.Error()})
	}
}

func (b *codeBridge) writeUpstreamFrame(ctx context.Context, conn *websocket.Conn, msg codeProxyMessage) error {
	data, err := base64.StdEncoding.DecodeString(msg.Body)
	if err != nil {
		return err
	}
	frameType := websocketMessageType(msg.Frame)
	b.trace("ws_downstream_data id=%s frame=%s bytes=%d", msg.ID, codeFrameType(frameType), len(data))
	return conn.Write(ctx, frameType, data)
}

func (b *codeBridge) startIncomingWebSocketFrame(msg codeProxyMessage) {
	if msg.ChunkID == "" {
		return
	}
	b.mu.Lock()
	b.incomingFrames[msg.ChunkID] = codePendingWebSocketFrame{id: msg.ID, frame: msg.Frame}
	b.mu.Unlock()
}

func (b *codeBridge) appendIncomingWebSocketFrame(msg codeProxyMessage) {
	if msg.ChunkID == "" {
		return
	}
	b.mu.Lock()
	frame, ok := b.incomingFrames[msg.ChunkID]
	if ok {
		frame.chunks = append(frame.chunks, msg.Body)
		b.incomingFrames[msg.ChunkID] = frame
	}
	b.mu.Unlock()
}

func (b *codeBridge) finishIncomingWebSocketFrame(ctx context.Context, msg codeProxyMessage) {
	if msg.ChunkID == "" {
		return
	}
	b.mu.Lock()
	frame, ok := b.incomingFrames[msg.ChunkID]
	delete(b.incomingFrames, msg.ChunkID)
	b.mu.Unlock()
	if !ok {
		return
	}
	b.writeUpstreamWebSocket(ctx, codeProxyMessage{
		Type:  "ws_data",
		ID:    frame.id,
		Frame: frame.frame,
		Body:  strings.Join(frame.chunks, ""),
	})
}

func (b *codeBridge) closeUpstreamWebSocket(id string, code websocket.StatusCode, reason string) {
	if code == 0 {
		code = websocket.StatusNormalClosure
	}
	b.mu.Lock()
	conn := b.upstream[id]
	delete(b.upstream, id)
	delete(b.pending, id)
	b.mu.Unlock()
	if conn != nil {
		_ = conn.Close(code, reason)
	}
}

func (b *codeBridge) writeJSON(ctx context.Context, msg codeProxyMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	return b.ws.Write(ctx, websocket.MessageText, data)
}

func (b *codeBridge) trace(format string, args ...any) {
	if !b.debug {
		return
	}
	fmt.Fprintf(os.Stderr, "code-debug: "+format+"\n", args...)
}

func isRetryableCodeBridgeError(err error) bool {
	if err == nil {
		return false
	}
	var closeErr websocket.CloseError
	if errors.As(err, &closeErr) {
		return closeErr.Code == websocket.StatusInternalError || closeErr.Code == websocket.StatusServiceRestart
	}
	text := err.Error()
	return strings.Contains(text, "failed to read frame header: EOF") ||
		strings.Contains(text, "tls: bad record MAC")
}

func codeUpstreamPath(path string) string {
	u, err := url.Parse(path)
	if err != nil {
		return "/"
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) >= 4 && parts[0] == "portal" && parts[1] == "leases" && parts[3] == "code" {
		tail := strings.Join(parts[4:], "/")
		if tail == "" {
			u.Path = "/"
		} else {
			u.Path = "/" + tail
		}
		return u.RequestURI()
	}
	return u.RequestURI()
}

func isCodeHTML(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(contentType), "text/html")
}

func rewriteCodeHTML(body []byte) []byte {
	return bytes.ReplaceAll(body, []byte(`<script type="module" src=""></script>`), nil)
}

func codeServerStaticFallback(path string, status int) ([]byte, http.Header, bool) {
	if status != http.StatusNotFound {
		return nil, nil, false
	}
	headers := http.Header{}
	switch {
	case strings.HasSuffix(path, "/node_modules/vsda/rust/web/vsda.js"):
		headers.Set("content-type", "text/javascript")
		headers.Set("cache-control", "no-store")
		return []byte(`define([],()=>globalThis.vsda_web={default:async()=>{},sign:v=>v,validator:class{createNewMessage(v){return v}validate(){return "ok"}free(){}}});`), headers, true
	case strings.HasSuffix(path, "/node_modules/vsda/rust/web/vsda_bg.wasm"):
		headers.Set("content-type", "application/wasm")
		headers.Set("cache-control", "no-store")
		return []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}, headers, true
	default:
		return nil, nil, false
	}
}

func codeFrameType(typ websocket.MessageType) string {
	if typ == websocket.MessageText {
		return "text"
	}
	return "binary"
}

func websocketMessageType(frame string) websocket.MessageType {
	if frame == "text" {
		return websocket.MessageText
	}
	return websocket.MessageBinary
}

func codeWebSocketDialHeaders(baseURL string, values map[string]string) (http.Header, []string) {
	headers := http.Header{}
	for key, value := range values {
		headers.Set(key, value)
	}
	subprotocols := websocketSubprotocols(headers)
	headers.Del("Sec-WebSocket-Protocol")
	if headers.Get("Origin") != "" {
		headers.Set("Origin", baseURL)
	}
	return headers, subprotocols
}

func websocketSubprotocols(headers http.Header) []string {
	var out []string
	for _, value := range headers.Values("Sec-WebSocket-Protocol") {
		for _, part := range strings.Split(value, ",") {
			protocol := strings.TrimSpace(part)
			if protocol != "" {
				out = append(out, protocol)
			}
		}
	}
	return out
}

func availableLocalCodePort() string {
	for port := 8081; port <= 8180; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return fmt.Sprint(port)
	}
	return "8081"
}

func webCodeAgentURL(base, leaseID string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/leases/" + url.PathEscape(leaseID) + "/code/agent"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func webCodeAgentURLWithTicket(base, leaseID, ticket string) string {
	u, err := url.Parse(webCodeAgentURL(base, leaseID))
	if err != nil {
		return base
	}
	values := url.Values{}
	values.Set("ticket", ticket)
	u.RawQuery = values.Encode()
	u.Fragment = ""
	return u.String()
}

func webCodePortalURL(base, leaseID string, folder ...string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/portal/leases/" + url.PathEscape(leaseID) + "/code/"
	values := url.Values{}
	if len(folder) > 0 && folder[0] != "" {
		values.Set("folder", folder[0])
	}
	u.RawQuery = values.Encode()
	u.Fragment = ""
	return u.String()
}

func (c *CoordinatorClient) CreateCodeTicket(ctx context.Context, leaseID string) (coordinatorCodeTicket, error) {
	var res coordinatorCodeTicket
	err := c.do(ctx, http.MethodPost, "/v1/leases/"+url.PathEscape(leaseID)+"/code/ticket", map[string]any{}, &res)
	return res, err
}
