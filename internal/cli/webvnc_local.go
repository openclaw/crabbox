package cli

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"nhooyr.io/websocket"
)

const maxLocalWebVNCPasswordBytes = 4096

const localWebVNCListenerOwnershipInterval = time.Second

const (
	localWebVNCSecurityAuto = "auto"
	localWebVNCSecurityVNC  = "vnc"
)

const localWebVNCSecurityTypePassword byte = 2

const (
	localWebVNCReadTimeout   = 10 * time.Second
	localWebVNCIdleTimeout   = 30 * time.Second
	localWebVNCMaxHeaderSize = 32 << 10
)

type localWebVNCDialer func(context.Context) (net.Conn, error)

type localWebVNCSourceIdentity struct {
	PID            int
	ProcessStarted string
}

type localWebVNCConnContextKey struct{}

// webVNCLocal serves the embedded noVNC viewer for an already-tunneled VNC
// socket. The source and browser listeners are both restricted to IPv4
// loopback; callers pass the VNC password through stdin so it never appears in
// process arguments or the environment.
func (a App) webVNCLocal(ctx context.Context, args []string) error {
	fs := newFlagSet("webvnc local", a.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintln(fs.Output(), "  crabbox webvnc local --vnc-host 127.0.0.1 --vnc-port <port> --username <user> --password-stdin [--security-type auto|vnc] [--local-port <port>] [--open]")
	}
	vncHost := fs.String("vnc-host", "127.0.0.1", "loopback VNC source host")
	vncPort := fs.String("vnc-port", "", "loopback VNC source port")
	username := fs.String("username", "", "VNC username")
	passwordStdin := fs.Bool("password-stdin", false, "read the VNC password from stdin")
	securityType := fs.String("security-type", localWebVNCSecurityAuto, "RFB security negotiation: auto or vnc")
	localPort := fs.String("local-port", "", "local WebVNC browser port")
	openViewer := fs.Bool("open", false, "open the local WebVNC viewer")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if !localWebVNCSupported() {
		return exit(2, "webvnc local is supported only on macOS and Linux")
	}
	if *vncHost != vncLoopbackHost {
		return exit(2, "--vnc-host must be exactly %s", vncLoopbackHost)
	}
	if !validWebVNCDaemonPort(*vncPort) {
		return exit(2, "--vnc-port must be an integer between 1 and 65535")
	}
	if err := validateLocalWebVNCUsername(*username); err != nil {
		return err
	}
	if *securityType != localWebVNCSecurityAuto && *securityType != localWebVNCSecurityVNC {
		return exit(2, "--security-type must be auto or vnc")
	}
	if !*passwordStdin {
		return exit(2, "webvnc local requires --password-stdin")
	}

	sourceIdentity, err := localWebVNCListenerIdentity(*vncPort)
	if err != nil {
		return exit(5, "pin local VNC source %s:%s: %v", *vncHost, *vncPort, err)
	}
	dialVNC := pinnedLocalWebVNCDialer(*vncHost, *vncPort, sourceIdentity)
	if err := probeLocalWebVNC(ctx, dialVNC); err != nil {
		return exit(5, "probe local VNC source %s:%s: %v", *vncHost, *vncPort, err)
	}
	bridgeCtx, cancelBridge := context.WithCancelCause(ctx)
	defer cancelBridge(context.Canceled)
	go func() {
		if err := monitorLocalWebVNCListenerOwner(bridgeCtx, *vncPort, sourceIdentity, localWebVNCListenerOwnershipInterval); err != nil {
			cancelBridge(fmt.Errorf("local VNC source ownership changed: %w", err))
		}
	}()

	reservation, err := reserveLocalWebVNCBrowserPort(*localPort, *vncPort)
	if err != nil {
		return exit(5, "reserve local WebVNC port: %v", err)
	}
	webPort := reservation.port
	webListener, err := reservation.listener()
	if err != nil {
		reservation.release()
		return exit(5, "open local WebVNC listener: %v", err)
	}
	defer webListener.Close()

	password, err := readLocalWebVNCPassword(bridgeCtx, a.input())
	if err != nil {
		return err
	}
	if err := context.Cause(bridgeCtx); err != nil {
		return err
	}
	credentials := rfbCredentials{Username: *username, Password: password}
	fmt.Fprintf(a.Stdout, "bridge: serving noVNC locally; VNC source %s:%s; keep this running while viewing\n", *vncHost, *vncPort)
	return a.serveLocalWebVNCBridge(
		bridgeCtx,
		webListener,
		webPort,
		credentials,
		*openViewer,
		*securityType == localWebVNCSecurityVNC,
		dialVNC,
		nil,
	)
}

func reserveLocalWebVNCBrowserPort(requested string, excluded ...string) (*webVNCDaemonPortReservation, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		return reserveWebVNCDaemonPort(requested, excluded...)
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, fmt.Errorf("reserve kernel-assigned local WebVNC port: %w", err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	return &webVNCDaemonPortReservation{port: port, tcpListener: listener}, nil
}

func exactLocalWebVNCListenerOwnerPID(owners []int) (int, error) {
	owners = append([]int(nil), owners...)
	sort.Ints(owners)
	if len(owners) == 0 {
		return 0, fmt.Errorf("no process owns the IPv4 loopback listener")
	}
	if len(owners) != 1 || owners[0] <= 0 {
		return 0, fmt.Errorf("IPv4 loopback listener must have exactly one process owner; found %v", owners)
	}
	return owners[0], nil
}

func verifyLocalWebVNCListenerOwner(port string, expected localWebVNCSourceIdentity) error {
	current, err := localWebVNCListenerIdentity(port)
	if err != nil {
		return err
	}
	if current != expected {
		return fmt.Errorf("IPv4 loopback listener identity is pid %d start %q, expected pid %d start %q", current.PID, current.ProcessStarted, expected.PID, expected.ProcessStarted)
	}
	return nil
}

func monitorLocalWebVNCListenerOwner(ctx context.Context, port string, expected localWebVNCSourceIdentity, interval time.Duration) error {
	if err := verifyLocalWebVNCListenerOwner(port, expected); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := verifyLocalWebVNCListenerOwner(port, expected); err != nil {
				return err
			}
		}
	}
}

func validateLocalWebVNCUsername(username string) error {
	if username == "" || username != strings.TrimSpace(username) || strings.IndexFunc(username, func(r rune) bool {
		return r < 32 || r == 127
	}) >= 0 {
		return exit(2, "--username must be non-empty and contain no surrounding whitespace or control characters")
	}
	return nil
}

func readLocalWebVNCPassword(ctx context.Context, input io.Reader) (string, error) {
	if input == nil {
		return "", exit(2, "read VNC password from stdin: stdin is unavailable")
	}
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	type readResult struct {
		data []byte
		err  error
	}
	result := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(input, maxLocalWebVNCPasswordBytes+1))
		result <- readResult{data: data, err: err}
	}()

	var data []byte
	var err error
	select {
	case <-ctx.Done():
		return "", context.Cause(ctx)
	case read := <-result:
		data, err = read.data, read.err
	}
	if err != nil {
		return "", exit(2, "read VNC password from stdin: %v", err)
	}
	if len(data) > maxLocalWebVNCPasswordBytes {
		return "", exit(2, "VNC password from stdin exceeds %d bytes", maxLocalWebVNCPasswordBytes)
	}
	password := string(data)
	password = strings.TrimSuffix(password, "\n")
	password = strings.TrimSuffix(password, "\r")
	if password == "" {
		return "", exit(2, "VNC password from stdin is empty")
	}
	if strings.ContainsAny(password, "\r\n\x00") {
		return "", exit(2, "VNC password from stdin must be one line without NUL bytes")
	}
	return password, nil
}

func directLocalWebVNCDialer(host, port string) localWebVNCDialer {
	address := net.JoinHostPort(host, port)
	return func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp4", address)
	}
}

func pinnedLocalWebVNCDialer(host, port string, sourceIdentity localWebVNCSourceIdentity) localWebVNCDialer {
	dial := directLocalWebVNCDialer(host, port)
	return func(ctx context.Context) (net.Conn, error) {
		if err := verifyLocalWebVNCListenerOwner(port, sourceIdentity); err != nil {
			return nil, fmt.Errorf("verify VNC listener owner before connect: %w", err)
		}
		conn, err := dial(ctx)
		if err != nil {
			return nil, err
		}
		if err := verifyLocalWebVNCListenerOwner(port, sourceIdentity); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("verify VNC listener owner after connect: %w", err)
		}
		return conn, nil
	}
}

func probeLocalWebVNC(ctx context.Context, dialVNC localWebVNCDialer) error {
	conn, err := dialVNC(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(10 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return err
	}
	if string(header) != "RFB " {
		return fmt.Errorf("unexpected protocol banner")
	}
	return nil
}

func (a App) serveLocalWebVNCBridge(
	ctx context.Context,
	webListener net.Listener,
	webPort string,
	credentials rfbCredentials,
	openViewer bool,
	forceVNCAuthentication bool,
	dialVNC localWebVNCDialer,
	handoffOutput func(macOSWebVNCHandoff),
) error {
	// A per-session token is handed to the browser through a mode-0600 temporary
	// viewer file. It is used only in a credential POST body and WebSocket
	// subprotocol, so neither the password nor its bearer capability appears in
	// argv, browser URLs, cookies, or DNS.
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
		if conn, ok := r.Context().Value(localWebVNCConnContextKey{}).(net.Conn); ok {
			if err := conn.SetReadDeadline(time.Time{}); err != nil {
				http.Error(w, "connection setup failed", http.StatusInternalServerError)
				return
			}
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
		relayCtx, cancelRelay := context.WithCancelCause(ctx)
		stopRequestCancel := context.AfterFunc(r.Context(), func() {
			cancelRelay(context.Cause(r.Context()))
		})
		defer func() {
			stopRequestCancel()
			cancelRelay(context.Canceled)
		}()
		tcp, err := dialVNC(relayCtx)
		if err != nil {
			_ = ws.Close(websocket.StatusInternalError, "vnc dial failed")
			return
		}
		defer tcp.Close()
		if forceVNCAuthentication {
			_ = relayWebSocketVNCWithVNCAuthentication(relayCtx, ws, tcp)
			return
		}
		relayWebSocketVNC(relayCtx, ws, tcp)
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: localWebVNCReadTimeout,
		ReadTimeout:       localWebVNCReadTimeout,
		IdleTimeout:       localWebVNCIdleTimeout,
		MaxHeaderBytes:    localWebVNCMaxHeaderSize,
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			return context.WithValue(ctx, localWebVNCConnContextKey{}, conn)
		},
	}
	serverErrors := make(chan error, 1)
	go func() {
		if err := srv.Serve(webListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
	}()
	defer func() { _ = srv.Close() }()

	handoff, err := createMacOSWebVNCHandoff(webPort, session)
	if err != nil {
		return err
	}
	defer os.Remove(handoff.Path)

	fmt.Fprintf(a.Stdout, "webvnc: %s\n", handoff.URL)
	if handoffOutput != nil {
		handoffOutput(handoff)
	}
	if openViewer {
		if err := openLocalURL(handoff.URL); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "opened: %s\n", handoff.URL)
	}
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case err := <-serverErrors:
		return exit(5, "serve local WebVNC: %v", err)
	}
}

func relayWebSocketVNCWithVNCAuthentication(ctx context.Context, ws *websocket.Conn, tcp net.Conn) error {
	browser := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	negotiationCtx, cancelNegotiation := context.WithCancel(ctx)
	negotiation := make(chan error, 1)
	go func() {
		negotiation <- forceRFBVNCAuthentication(negotiationCtx, browser, tcp)
	}()
	timer := time.NewTimer(localWebVNCReadTimeout)
	defer timer.Stop()
	select {
	case err := <-negotiation:
		cancelNegotiation()
		if cause := context.Cause(ctx); cause != nil {
			_ = ws.Close(websocket.StatusGoingAway, "bridge stopped")
			return cause
		}
		if err != nil {
			_ = ws.Close(websocket.StatusPolicyViolation, "VNC authentication negotiation failed")
			return err
		}
	case <-ctx.Done():
		_ = ws.Close(websocket.StatusGoingAway, "bridge stopped")
		cancelNegotiation()
		<-negotiation
		return context.Cause(ctx)
	case <-timer.C:
		_ = ws.Close(websocket.StatusPolicyViolation, "VNC authentication negotiation timed out")
		cancelNegotiation()
		<-negotiation
		return fmt.Errorf("VNC authentication negotiation timed out")
	}
	errors := make(chan error, 2)
	go func() {
		_, err := io.Copy(tcp, browser)
		errors <- err
	}()
	go func() {
		_, err := io.Copy(browser, tcp)
		errors <- err
	}()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case err := <-errors:
		return err
	}
}

func forceRFBVNCAuthentication(ctx context.Context, browser, server net.Conn) error {
	deadline := time.Now().Add(localWebVNCReadTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := browser.SetDeadline(deadline); err != nil {
		return fmt.Errorf("set RFB browser authentication deadline: %w", err)
	}
	if err := server.SetDeadline(deadline); err != nil {
		_ = browser.SetDeadline(time.Time{})
		return fmt.Errorf("set RFB server authentication deadline: %w", err)
	}
	cancellationDone := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		defer close(cancellationDone)
		_ = browser.SetDeadline(time.Now())
		_ = server.SetDeadline(time.Now())
	})
	defer func() {
		if !stopCancellation() {
			<-cancellationDone
		}
		_ = browser.SetDeadline(time.Time{})
		_ = server.SetDeadline(time.Time{})
	}()

	serverVersion := make([]byte, 12)
	if _, err := io.ReadFull(server, serverVersion); err != nil {
		return fmt.Errorf("read RFB server version: %w", err)
	}
	if err := validateRFBServerVersion(serverVersion); err != nil {
		return fmt.Errorf("invalid RFB server version: %w", err)
	}
	if _, err := browser.Write(serverVersion); err != nil {
		return fmt.Errorf("write RFB server version: %w", err)
	}

	browserVersion := make([]byte, 12)
	if _, err := io.ReadFull(browser, browserVersion); err != nil {
		return fmt.Errorf("read RFB browser version: %w", err)
	}
	major, minor, err := parseRFBVersion(browserVersion)
	if err != nil {
		return fmt.Errorf("invalid RFB browser version: %w", err)
	}
	if _, err := server.Write(browserVersion); err != nil {
		return fmt.Errorf("write RFB browser version: %w", err)
	}

	if major == 3 && minor < 7 {
		security := make([]byte, 4)
		if _, err := io.ReadFull(server, security); err != nil {
			return fmt.Errorf("read RFB 3.3 security type: %w", err)
		}
		if binary.BigEndian.Uint32(security) != uint32(localWebVNCSecurityTypePassword) {
			return fmt.Errorf("RFB server did not select VNC password authentication")
		}
		if _, err := browser.Write(security); err != nil {
			return fmt.Errorf("write RFB 3.3 security type: %w", err)
		}
		return nil
	}

	count := []byte{0}
	if _, err := io.ReadFull(server, count); err != nil {
		return fmt.Errorf("read RFB security type count: %w", err)
	}
	if count[0] == 0 {
		return fmt.Errorf("RFB server rejected security negotiation")
	}
	types := make([]byte, int(count[0]))
	if _, err := io.ReadFull(server, types); err != nil {
		return fmt.Errorf("read RFB security types: %w", err)
	}
	passwordAuth := false
	for _, securityType := range types {
		if securityType == localWebVNCSecurityTypePassword {
			passwordAuth = true
			break
		}
	}
	if !passwordAuth {
		return fmt.Errorf("RFB server did not offer VNC password authentication")
	}
	if _, err := browser.Write([]byte{1, localWebVNCSecurityTypePassword}); err != nil {
		return fmt.Errorf("write filtered RFB security types: %w", err)
	}
	selected := []byte{0}
	if _, err := io.ReadFull(browser, selected); err != nil {
		return fmt.Errorf("read RFB browser security selection: %w", err)
	}
	if selected[0] != localWebVNCSecurityTypePassword {
		return fmt.Errorf("RFB browser did not select VNC password authentication")
	}
	if _, err := server.Write(selected); err != nil {
		return fmt.Errorf("write RFB server security selection: %w", err)
	}
	return nil
}

func validateRFBServerVersion(version []byte) error {
	if _, _, err := parseRFBVersion(version); err == nil {
		return nil
	}
	switch string(version) {
	case "RFB 004.000\n", "RFB 004.001\n", "RFB 005.000\n":
		return nil
	default:
		return fmt.Errorf("unsupported protocol version %q", string(version))
	}
}

func parseRFBVersion(version []byte) (int, int, error) {
	if len(version) != 12 || string(version[:4]) != "RFB " || version[7] != '.' || version[11] != '\n' {
		return 0, 0, fmt.Errorf("unexpected protocol banner")
	}
	major, majorErr := strconv.Atoi(string(version[4:7]))
	minor, minorErr := strconv.Atoi(string(version[8:11]))
	if majorErr != nil || minorErr != nil || major != 3 {
		return 0, 0, fmt.Errorf("unsupported protocol version %q", string(version))
	}
	return major, minor, nil
}
