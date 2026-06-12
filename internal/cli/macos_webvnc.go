package cli

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"nhooyr.io/websocket"
)

// macOSWebVNCBridge serves a browser noVNC viewer for a macOS (tart) lease
// without any noVNC/websockify tooling on the guest. It SSH-tunnels to the
// guest's built-in Screen Sharing port, creates a mode-0600 viewer file around
// the embedded noVNC module, and runs a loopback WebSocket relay. noVNC performs
// Apple (ARD) authentication with credentials fetched into browser memory only
// after the viewer proves its ephemeral session token.
func (a App) macOSWebVNCBridge(ctx context.Context, cfg Config, id, webPort string, openViewer, reclaim bool) error {
	server, target, leaseID, err := a.resolveNetworkLeaseTargetForRepo(ctx, cfg, id, false, reclaim)
	if err != nil {
		return err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	if err := a.claimAndTouchLeaseTarget(ctx, cfg, server, target, leaseID, reclaim); err != nil {
		return err
	}
	if _, err := resolveVNCEndpoint(ctx, cfg, &target); err != nil {
		return err
	}
	credentials, ok := providerDesktopCredentials(cfg, target)
	if !ok {
		return exit(2, "provider=%s does not supply macOS desktop credentials", cfg.Provider)
	}

	// Resolve the browser-facing port first (honoring an explicit --local-port),
	// then pick the SSH-tunnel port so the two never collide on the same value.
	if webPort == "" {
		webPort = availableLocalVNCPort()
	}

	// SSH tunnel: 127.0.0.1:vncPort -> guest 127.0.0.1:5900 (Screen Sharing).
	vncPort := availableLocalVNCPortExcept(webPort)
	tunnel, err := startVNCForegroundTunnel(ctx, target, vncPort, "127.0.0.1", managedVNCPort)
	if err != nil {
		return err
	}
	defer stopProcess(tunnel)

	// A per-session token is handed to the browser through a mode-0600 temporary
	// viewer file. It is used only in a credential POST body and WebSocket
	// subprotocol, so neither the account password nor its bearer capability
	// appears in argv, browser URLs, cookies, or DNS.
	session, err := newMacOSWebVNCSession()
	if err != nil {
		return exit(5, "generate viewer session: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/credentials", macOSWebVNCCredentialsHandler(session, credentials))
	mux.HandleFunc("/websockify", func(w http.ResponseWriter, r *http.Request) {
		if !macOSWebVNCProtocolAllowed(r, session.Protocol) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols:       []string{session.Protocol},
			InsecureSkipVerify: true, // file:// viewers send Origin: null; the subprotocol is the bearer.
		})
		if err != nil {
			return
		}
		ws.SetReadLimit(-1)
		defer ws.Close(websocket.StatusNormalClosure, "")
		tcp, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(r.Context(), "tcp", net.JoinHostPort("127.0.0.1", vncPort))
		if err != nil {
			_ = ws.Close(websocket.StatusInternalError, "vnc dial failed")
			return
		}
		defer tcp.Close()
		relayWebSocketVNC(r.Context(), ws, tcp)
	})

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", webPort))
	if err != nil {
		return exit(5, "start local WebVNC server on 127.0.0.1:%s: %v", webPort, err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	handoff, err := createMacOSWebVNCHandoff(webPort, session)
	if err != nil {
		return err
	}
	defer os.Remove(handoff.Path)

	fmt.Fprintf(a.Stdout, "lease: %s slug=%s provider=%s target=macos\n", leaseID, blank(serverSlug(server), "-"), blank(server.Provider, cfg.Provider))
	fmt.Fprintf(a.Stdout, "bridge: serving noVNC locally; SSH tunnel -> guest 127.0.0.1:%s; keep this running while viewing\n", managedVNCPort)
	fmt.Fprintf(a.Stdout, "webvnc: %s\n", handoff.URL)
	fmt.Fprintf(a.Stdout, "remote: forward port %s over SSH, copy %s to your machine, then open the copied file\n", webPort, handoff.Path)
	if openViewer {
		if err := openLocalURL(handoff.URL); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "opened: %s\n", handoff.URL)
	}
	<-ctx.Done()
	return context.Cause(ctx)
}

// relayWebSocketVNC pumps bytes bidirectionally between a browser WebSocket
// (noVNC) and the tunneled VNC TCP connection until either side closes.
func relayWebSocketVNC(ctx context.Context, ws *websocket.Conn, tcp net.Conn) {
	errc := make(chan error, 2)
	go func() {
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				errc <- err
				return
			}
			if _, err := tcp.Write(data); err != nil {
				errc <- err
				return
			}
		}
	}()
	go func() { errc <- copyTCPToWebSocket(ctx, ws, tcp) }()
	<-errc
}

type macOSWebVNCHandoff struct {
	Path string
	URL  string
}

type macOSWebVNCSession struct {
	Token    string
	Protocol string
}

func newMacOSWebVNCSession() (macOSWebVNCSession, error) {
	token, err := randomToken()
	if err != nil {
		return macOSWebVNCSession{}, err
	}
	return macOSWebVNCSession{
		Token:    token,
		Protocol: "crabbox." + token,
	}, nil
}

func createMacOSWebVNCHandoff(webPort string, session macOSWebVNCSession) (macOSWebVNCHandoff, error) {
	file, err := os.CreateTemp("", "crabbox-webvnc-*.html")
	if err != nil {
		return macOSWebVNCHandoff{}, exit(5, "create WebVNC browser handoff: %v", err)
	}
	path := file.Name()
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	rfbSource, err := fs.ReadFile(webVNCAssets(), "rfb.js")
	if err != nil {
		return macOSWebVNCHandoff{}, exit(5, "read embedded WebVNC viewer: %v", err)
	}
	rfbJSON, err := json.Marshal(string(rfbSource))
	if err != nil {
		return macOSWebVNCHandoff{}, exit(5, "encode embedded WebVNC viewer: %v", err)
	}
	configJSON, err := json.Marshal(map[string]string{
		"credentialsURL": "http://127.0.0.1:" + webPort + "/credentials",
		"protocol":       session.Protocol,
		"token":          session.Token,
		"websocketURL":   "ws://127.0.0.1:" + webPort + "/websockify",
	})
	if err != nil {
		return macOSWebVNCHandoff{}, exit(5, "encode WebVNC viewer config: %v", err)
	}
	content := `<!doctype html><html><head><meta charset="utf-8"><title>Crabbox WebVNC</title><style>` +
		`html,body{margin:0;height:100%;background:#111;overflow:hidden}#screen{width:100%;height:100%}` +
		`#status{position:fixed;top:0;left:0;right:0;color:#ddd;font:12px/1.6 ui-monospace,monospace;padding:4px 8px;background:rgba(0,0,0,.7);z-index:10}` +
		`</style></head><body><div id="status">connecting...</div><div id="screen"></div><script type="module">` +
		`const source=` + string(rfbJSON) + `;const moduleURL=URL.createObjectURL(new Blob([source],{type:"text/javascript"}));` +
		`const{default:RFB}=await import(moduleURL);const config=` + string(configJSON) + `;const status=document.getElementById("status");` +
		`let creds={};try{const body=new URLSearchParams({token:config.token});const response=await fetch(config.credentialsURL,{method:"POST",body});` +
		`if(response.ok)creds=await response.json();else status.textContent="could not load VNC credentials"}catch(error){status.textContent="could not load VNC credentials"}` +
		`const rfb=new RFB(document.getElementById("screen"),config.websocketURL,{credentials:creds,wsProtocols:[config.protocol]});` +
		`rfb.scaleViewport=true;rfb.focusOnClick=true;rfb.addEventListener("connect",()=>{status.textContent="connected";setTimeout(()=>{status.style.display="none"},1500)});` +
		`rfb.addEventListener("disconnect",event=>{status.style.display="block";status.textContent="disconnected"+(event.detail&&event.detail.clean?"":" (connection error)")});` +
		`rfb.addEventListener("credentialsrequired",()=>{status.style.display="block";status.textContent="VNC credentials required"});` +
		`</script></body></html>`
	if _, err := file.WriteString(content); err != nil {
		return macOSWebVNCHandoff{}, exit(5, "write WebVNC browser handoff: %v", err)
	}
	if err := file.Close(); err != nil {
		return macOSWebVNCHandoff{}, exit(5, "close WebVNC browser handoff: %v", err)
	}
	ok = true
	return macOSWebVNCHandoff{
		Path: path,
		URL:  (&url.URL{Scheme: "file", Path: path}).String(),
	}, nil
}

func macOSWebVNCCredentialsHandler(session macOSWebVNCSession, credentials rfbCredentials) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "null")
		w.Header().Set("Cache-Control", "no-store")
		if r.Header.Get("Origin") != "null" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if err := r.ParseForm(); err != nil ||
			subtle.ConstantTimeCompare([]byte(r.Form.Get("token")), []byte(session.Token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"username": credentials.Username,
			"password": credentials.Password,
		})
	}
}

func macOSWebVNCProtocolAllowed(r *http.Request, expected string) bool {
	for _, protocol := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(protocol)), []byte(expected)) == 1 {
			return true
		}
	}
	return false
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
