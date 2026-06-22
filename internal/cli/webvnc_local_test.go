//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestReadLocalWebVNCPassword(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "newline", input: "secret\n", want: "secret"},
		{name: "crlf", input: "secret\r\n", want: "secret"},
		{name: "no newline", input: "secret", want: "secret"},
		{name: "spaces preserved", input: " secret value \n", want: " secret value "},
		{name: "empty", input: "\n", wantErr: true},
		{name: "multiple lines", input: "secret\nother\n", wantErr: true},
		{name: "nul", input: "secret\x00value\n", wantErr: true},
		{name: "too large", input: strings.Repeat("x", maxLocalWebVNCPasswordBytes+1), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readLocalWebVNCPassword(context.Background(), strings.NewReader(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("password=%q, want error", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("password=%q err=%v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestReadLocalWebVNCPasswordStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	input := newBlockingReader()
	defer close(input.release)
	result := make(chan error, 1)
	go func() {
		_, err := readLocalWebVNCPassword(ctx, input)
		result <- err
	}()
	<-input.started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("password read error=%v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("password read did not stop after cancellation")
	}
}

func TestReserveLocalWebVNCBrowserPortUsesKernelAssignedPort(t *testing.T) {
	reservation, err := reserveLocalWebVNCBrowserPort("")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.port
	listener, err := reservation.listener()
	if err != nil {
		reservation.release()
		t.Fatal(err)
	}
	defer listener.Close()
	if !validWebVNCDaemonPort(port) {
		t.Fatalf("kernel-assigned port=%q", port)
	}
	if got := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port); got != port {
		t.Fatalf("listener port=%q reservation port=%q", got, port)
	}
}

func TestForceRFBVNCAuthenticationFiltersServerPreference(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	browser, bridgeBrowser := net.Pipe()
	server, bridgeServer := net.Pipe()
	defer browser.Close()
	defer bridgeBrowser.Close()
	defer server.Close()
	defer bridgeServer.Close()

	negotiation := make(chan error, 1)
	go func() {
		negotiation <- forceRFBVNCAuthentication(ctx, bridgeBrowser, bridgeServer)
	}()
	serverResult := make(chan error, 1)
	go func() {
		if _, err := server.Write([]byte("RFB 003.889\n")); err != nil {
			serverResult <- err
			return
		}
		version := make([]byte, 12)
		if _, err := io.ReadFull(server, version); err != nil {
			serverResult <- err
			return
		}
		if string(version) != "RFB 003.008\n" {
			serverResult <- fmt.Errorf("browser version=%q", version)
			return
		}
		if _, err := server.Write([]byte{2, rfbSecurityARD, localWebVNCSecurityTypePassword}); err != nil {
			serverResult <- err
			return
		}
		selected := []byte{0}
		if _, err := io.ReadFull(server, selected); err != nil {
			serverResult <- err
			return
		}
		if selected[0] != localWebVNCSecurityTypePassword {
			serverResult <- fmt.Errorf("selected security type=%d", selected[0])
			return
		}
		serverResult <- nil
	}()

	version := make([]byte, 12)
	if _, err := io.ReadFull(browser, version); err != nil {
		t.Fatal(err)
	}
	if string(version) != "RFB 003.889\n" {
		t.Fatalf("server version=%q", version)
	}
	if _, err := browser.Write([]byte("RFB 003.008\n")); err != nil {
		t.Fatal(err)
	}
	filtered := make([]byte, 2)
	if _, err := io.ReadFull(browser, filtered); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(filtered, []byte{1, localWebVNCSecurityTypePassword}) {
		t.Fatalf("filtered security types=%v", filtered)
	}
	if _, err := browser.Write([]byte{localWebVNCSecurityTypePassword}); err != nil {
		t.Fatal(err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
	if err := <-negotiation; err != nil {
		t.Fatal(err)
	}
}

func TestValidateRFBServerVersionSupportsNoVNCAliases(t *testing.T) {
	for _, version := range []string{
		"RFB 003.889\n",
		"RFB 004.000\n",
		"RFB 004.001\n",
		"RFB 005.000\n",
	} {
		t.Run(version[4:11], func(t *testing.T) {
			if err := validateRFBServerVersion([]byte(version)); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := validateRFBServerVersion([]byte("RFB 006.000\n")); err == nil {
		t.Fatal("unsupported server version was accepted")
	}
}

func TestForceRFBVNCAuthenticationStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	browser, bridgeBrowser := net.Pipe()
	server, bridgeServer := net.Pipe()
	defer browser.Close()
	defer bridgeBrowser.Close()
	defer server.Close()
	defer bridgeServer.Close()
	result := make(chan error, 1)
	go func() {
		result <- forceRFBVNCAuthentication(ctx, bridgeBrowser, bridgeServer)
	}()
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled authentication negotiation returned no error")
		}
	case <-time.After(time.Second):
		t.Fatal("authentication negotiation did not stop after cancellation")
	}
}

func TestForceRFBVNCAuthenticationStopsAtDeadlineWhenBrowserStallsAfterServerVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	browser, bridgeBrowser := net.Pipe()
	server, bridgeServer := net.Pipe()
	defer browser.Close()
	defer bridgeBrowser.Close()
	defer server.Close()
	defer bridgeServer.Close()
	result := make(chan error, 1)
	go func() {
		result <- forceRFBVNCAuthentication(ctx, bridgeBrowser, bridgeServer)
	}()

	if _, err := server.Write([]byte("RFB 003.889\n")); err != nil {
		t.Fatal(err)
	}
	version := make([]byte, 12)
	if _, err := io.ReadFull(browser, version); err != nil {
		t.Fatal(err)
	}
	if string(version) != "RFB 003.889\n" {
		t.Fatalf("server version=%q", version)
	}

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("expired authentication negotiation returned no error")
		}
	case <-time.After(time.Second):
		t.Fatal("authentication negotiation did not reach its deadline while browser version was stalled")
	}
}

func TestForceRFBVNCAuthenticationStopsWhenBrowserStallsAfterSecurityOffer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	browser, bridgeBrowser := net.Pipe()
	server, bridgeServer := net.Pipe()
	defer browser.Close()
	defer bridgeBrowser.Close()
	defer server.Close()
	defer bridgeServer.Close()
	result := make(chan error, 1)
	go func() {
		result <- forceRFBVNCAuthentication(ctx, bridgeBrowser, bridgeServer)
	}()

	if _, err := server.Write([]byte("RFB 003.889\n")); err != nil {
		t.Fatal(err)
	}
	serverVersion := make([]byte, 12)
	if _, err := io.ReadFull(browser, serverVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := browser.Write([]byte("RFB 003.008\n")); err != nil {
		t.Fatal(err)
	}
	browserVersion := make([]byte, 12)
	if _, err := io.ReadFull(server, browserVersion); err != nil {
		t.Fatal(err)
	}
	if string(browserVersion) != "RFB 003.008\n" {
		t.Fatalf("browser version=%q", browserVersion)
	}
	if _, err := server.Write([]byte{2, rfbSecurityARD, localWebVNCSecurityTypePassword}); err != nil {
		t.Fatal(err)
	}
	filtered := make([]byte, 2)
	if _, err := io.ReadFull(browser, filtered); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(filtered, []byte{1, localWebVNCSecurityTypePassword}) {
		t.Fatalf("filtered security types=%v", filtered)
	}

	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled authentication negotiation returned no error")
		}
	case <-time.After(time.Second):
		t.Fatal("authentication negotiation did not stop while browser selection was stalled")
	}
}

type blockingReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingReader() *blockingReader {
	return &blockingReader{started: make(chan struct{}), release: make(chan struct{})}
}

func (r *blockingReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return 0, io.EOF
}

func TestExactLocalWebVNCListenerOwnerPID(t *testing.T) {
	for _, tt := range []struct {
		name    string
		owners  []int
		want    int
		wantErr bool
	}{
		{name: "single", owners: []int{42}, want: 42},
		{name: "none", wantErr: true},
		{name: "multiple", owners: []int{43, 42}, wantErr: true},
		{name: "invalid", owners: []int{0}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := exactLocalWebVNCListenerOwnerPID(tt.owners)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("owner=%d, want error", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("owner=%d err=%v, want %d", got, err, tt.want)
			}
		})
	}
}

func TestLocalWebVNCListenerIdentityPinsCurrentProcess(t *testing.T) {
	if !localWebVNCSupported() {
		t.Fatal("local WebVNC should be supported on this platform")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	identity, err := localWebVNCListenerIdentity(port)
	if err != nil {
		t.Fatal(err)
	}
	wantStarted, err := webVNCDaemonProcessStartIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if identity.PID != os.Getpid() || identity.ProcessStarted != wantStarted {
		t.Fatalf("listener identity=%#v, want pid=%d start=%q", identity, os.Getpid(), wantStarted)
	}

	wrongStart := identity
	wrongStart.ProcessStarted += "-wrong"
	if conn, err := pinnedLocalWebVNCDialer("127.0.0.1", port, wrongStart)(context.Background()); err == nil {
		_ = conn.Close()
		t.Fatal("pinned dialer accepted the wrong process start identity")
	} else if !strings.Contains(err.Error(), "before connect") {
		t.Fatalf("pinned dialer error=%v", err)
	}
}

func TestWebVNCLocalRejectsUnsafeInputBeforeReadingPassword(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "non-loopback host",
			args: []string{"--vnc-host", "localhost", "--vnc-port", "5900", "--username", "admin", "--password-stdin"},
			want: "must be exactly 127.0.0.1",
		},
		{
			name: "invalid port",
			args: []string{"--vnc-host", "127.0.0.1", "--vnc-port", "0", "--username", "admin", "--password-stdin"},
			want: "--vnc-port must be an integer",
		},
		{
			name: "missing username",
			args: []string{"--vnc-host", "127.0.0.1", "--vnc-port", "5900", "--password-stdin"},
			want: "--username must be non-empty",
		},
		{
			name: "missing stdin flag",
			args: []string{"--vnc-host", "127.0.0.1", "--vnc-port", "5900", "--username", "admin"},
			want: "requires --password-stdin",
		},
		{
			name: "invalid security type",
			args: []string{"--vnc-host", "127.0.0.1", "--vnc-port", "5900", "--username", "admin", "--password-stdin", "--security-type", "ard"},
			want: "--security-type must be auto or vnc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := &unexpectedRead{t: t}
			err := (App{Stdout: io.Discard, Stderr: io.Discard, Stdin: input}).webVNCLocal(context.Background(), tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v, want %q", err, tt.want)
			}
		})
	}
}

func TestProbeLocalWebVNCRequiresRFB(t *testing.T) {
	for _, tt := range []struct {
		name    string
		banner  string
		wantErr bool
	}{
		{name: "rfb", banner: "RFB 003.008\n"},
		{name: "other", banner: "HTTP/1.1 200 OK\r\n", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dial := func(context.Context) (net.Conn, error) {
				client, server := net.Pipe()
				go func() {
					_, _ = io.WriteString(server, tt.banner)
					_ = server.Close()
				}()
				return client, nil
			}
			err := probeLocalWebVNC(context.Background(), dial)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestWebVNCLocalRunsWithPasswordOnlyOnStdin(t *testing.T) {
	source, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	go func() {
		for {
			conn, err := source.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.WriteString(conn, "RFB 003.008\n")
			}()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	browserPort := unusedWebVNCTestPort(t)
	output := newNotifyBuffer("webvnc:")
	app := App{Stdout: output, Stderr: io.Discard, Stdin: strings.NewReader("stdin-only-secret\n")}
	errCh := make(chan error, 1)
	go func() {
		errCh <- app.webvnc(ctx, []string{
			"local",
			"--vnc-host", "127.0.0.1",
			"--vnc-port", strconv.Itoa(source.Addr().(*net.TCPAddr).Port),
			"--username", "admin",
			"--password-stdin",
			"--local-port", browserPort,
		})
	}()
	select {
	case <-output.ready:
	case err := <-errCh:
		t.Fatalf("local WebVNC command exited before serving: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for local WebVNC command")
	}
	if got := output.String(); strings.Contains(got, "stdin-only-secret") || !strings.Contains(got, "VNC source 127.0.0.1:") || !strings.Contains(got, "webvnc: file:") {
		t.Fatalf("unexpected command output: %q", got)
	}
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("command error=%v", err)
	}
}

func TestWebVNCLocalStopsWhenSourceListenerDisappears(t *testing.T) {
	source, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := source.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.WriteString(conn, "RFB 003.008\n")
			}()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	browserPort := unusedWebVNCTestPort(t)
	output := newNotifyBuffer("webvnc:")
	errCh := make(chan error, 1)
	go func() {
		errCh <- (App{Stdout: output, Stderr: io.Discard, Stdin: strings.NewReader("stdin-only-secret\n")}).webVNCLocal(ctx, []string{
			"--vnc-host", "127.0.0.1",
			"--vnc-port", strconv.Itoa(source.Addr().(*net.TCPAddr).Port),
			"--username", "admin",
			"--password-stdin",
			"--local-port", browserPort,
		})
	}()
	select {
	case <-output.ready:
	case err := <-errCh:
		t.Fatalf("local WebVNC command exited before serving: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for local WebVNC command")
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "local VNC source ownership changed") {
			t.Fatalf("command error=%v", err)
		}
	case <-ctx.Done():
		t.Fatal("local WebVNC bridge did not stop after source ownership disappeared")
	}
}

func TestServeLocalWebVNCBridgeRelaysWithoutExposingCredentials(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	webPort := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := newNotifyBuffer("webvnc:")
	dial := func(context.Context) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			buffer := make([]byte, 1024)
			for {
				count, err := server.Read(buffer)
				if count > 0 {
					if _, writeErr := server.Write(buffer[:count]); writeErr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()
		return client, nil
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- (App{Stdout: output, Stderr: io.Discard}).serveLocalWebVNCBridge(
			ctx,
			listener,
			webPort,
			rfbCredentials{Username: "admin", Password: "super-secret"},
			false,
			false,
			dial,
			nil,
		)
	}()

	select {
	case <-output.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for WebVNC handoff")
	}
	text := output.String()
	if strings.Contains(text, "super-secret") || strings.Contains(text, "admin") {
		t.Fatalf("credentials leaked in output: %q", text)
	}
	handoffURL := strings.TrimSpace(strings.TrimPrefix(text, "webvnc:"))
	parsed, err := url.Parse(handoffURL)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(parsed.Path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(content, []byte("super-secret")) || bytes.Contains(content, []byte(`"admin"`)) {
		t.Fatal("credentials leaked in handoff file")
	}
	protocol := regexp.MustCompile(`crabbox\.[0-9a-f]{32}`).FindString(string(content))
	if protocol == "" || strings.Contains(text, protocol) {
		t.Fatalf("protocol=%q output=%q", protocol, text)
	}

	ws, _, err := websocket.Dial(context.Background(), "ws://127.0.0.1:"+webPort+"/websockify", &websocket.DialOptions{
		HTTPHeader:   http.Header{"Origin": []string{"null"}},
		Subprotocols: []string{protocol},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "")
	if err := ws.Write(context.Background(), websocket.MessageBinary, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	_, message, err := ws.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(message) != "ping" {
		t.Fatalf("relay response=%q", message)
	}

	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("bridge error=%v", err)
	}
	if _, err := os.Stat(parsed.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("handoff file remained after bridge exit: %v", err)
	}
}

type unexpectedRead struct {
	t *testing.T
}

func (r *unexpectedRead) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Fatal("stdin was read before validating local WebVNC flags")
	return 0, io.EOF
}

type notifyBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	needle string
	ready  chan struct{}
	once   sync.Once
}

func newNotifyBuffer(needle string) *notifyBuffer {
	return &notifyBuffer{needle: needle, ready: make(chan struct{})}
}

func (b *notifyBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written, err := b.buffer.Write(data)
	if strings.Contains(b.buffer.String(), b.needle) {
		b.once.Do(func() { close(b.ready) })
	}
	return written, err
}

func (b *notifyBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
