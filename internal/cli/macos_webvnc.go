package cli

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"nhooyr.io/websocket"
)

// macOSWebVNCBridge serves a browser noVNC viewer for a macOS (tart) lease
// without any noVNC/websockify tooling on the guest. It SSH-tunnels to the
// guest's built-in Screen Sharing port, runs a local HTTP server that serves the
// embedded noVNC viewer plus a /websockify WebSocket relayed to the tunneled VNC
// port, and opens the browser. noVNC performs Apple (ARD) authentication with
// the lease account credentials, which are handed to the local viewer only --
// never written to the guest or placed on a command line.
func (a App) macOSWebVNCBridge(ctx context.Context, cfg Config, id, webPort string, openViewer, reclaim bool) error {
	server, target, leaseID, err := a.resolveNetworkLeaseTarget(ctx, cfg, id, false)
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

	// A per-session token gates the credentials endpoint. The viewer page reads it
	// from the URL fragment (which is never sent to the server, and only the token
	// -- not the account password -- ever appears in the URL/openers).
	token, err := randomToken()
	if err != nil {
		return exit(5, "generate viewer session token: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(webVNCAssets())))
	mux.HandleFunc("/credentials", func(w http.ResponseWriter, r *http.Request) {
		if !macOSWebVNCTokenAllowed(r, token) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"username": credentials.Username,
			"password": credentials.Password,
		})
	})
	mux.HandleFunc("/websockify", func(w http.ResponseWriter, r *http.Request) {
		if !macOSWebVNCTokenAllowed(r, token) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols:   []string{"binary"},
			OriginPatterns: []string{"127.0.0.1:*", "localhost:*"},
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

	viewerURL := macOSWebVNCViewerURL(webPort, token)
	fmt.Fprintf(a.Stdout, "lease: %s slug=%s provider=%s target=macos\n", leaseID, blank(serverSlug(server), "-"), blank(server.Provider, cfg.Provider))
	fmt.Fprintf(a.Stdout, "bridge: serving noVNC locally; SSH tunnel -> guest 127.0.0.1:%s; keep this running while viewing\n", managedVNCPort)
	fmt.Fprintf(a.Stdout, "webvnc: %s\n", viewerURL)
	fmt.Fprintf(a.Stdout, "remote: from another host first run  ssh -L %s:127.0.0.1:%s <user>@<this-host>  then open the URL there\n", webPort, webPort)
	if openViewer {
		if err := openLocalURL(viewerURL); err == nil {
			fmt.Fprintf(a.Stdout, "opened: %s\n", viewerURL)
		}
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

// macOSWebVNCViewerURL builds the local viewer URL. The session token is carried
// in the fragment (never sent to the server); credentials are fetched from the
// token-gated /credentials endpoint, so the account password never appears in
// the URL, terminal scrollback, browser history, or the OS opener's arguments.
func macOSWebVNCViewerURL(webPort, token string) string {
	return "http://127.0.0.1:" + webPort + "/vnc.html#token=" + token
}

func macOSWebVNCTokenAllowed(r *http.Request, token string) bool {
	return subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(token)) == 1
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
