package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
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

	// SSH tunnel: 127.0.0.1:vncPort -> guest 127.0.0.1:5900 (Screen Sharing).
	vncPort := availableLocalVNCPort()
	tunnel, err := startVNCForegroundTunnel(ctx, target, vncPort, "127.0.0.1", managedVNCPort)
	if err != nil {
		return err
	}
	defer stopProcess(tunnel)

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(webVNCAssets())))
	mux.HandleFunc("/websockify", func(w http.ResponseWriter, r *http.Request) {
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

	if webPort == "" {
		webPort = availableLocalVNCPort()
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", webPort))
	if err != nil {
		return exit(5, "start local WebVNC server on 127.0.0.1:%s: %v", webPort, err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	viewerURL := macOSWebVNCViewerURL(webPort, target.User, vncViewerPassword(cfg))
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

func macOSWebVNCViewerURL(webPort, username, password string) string {
	v := url.Values{}
	if u := strings.TrimSpace(username); u != "" {
		v.Set("username", u)
	}
	if p := strings.TrimSpace(password); p != "" {
		v.Set("password", p)
	}
	q := ""
	if len(v) > 0 {
		q = "?" + v.Encode()
	}
	return "http://127.0.0.1:" + webPort + "/vnc.html" + q
}

// vncViewerPassword returns the account password the local noVNC viewer uses for
// macOS Apple (ARD) authentication. It defaults to the cirruslabs base-image
// account password; override with --tart-password / CRABBOX_TART_PASSWORD. This
// value is only handed to the local browser viewer.
func vncViewerPassword(cfg Config) string {
	if p := strings.TrimSpace(cfg.Tart.Password); p != "" {
		return p
	}
	return "admin"
}
