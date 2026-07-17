//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
	if string(version) != "RFB 003.008\n" {
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

func TestForceRFBVNCAuthenticationFallsBackToRFB33ForUnknownServerVersion(t *testing.T) {
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
		if _, err := server.Write([]byte("RFB 003.009\n")); err != nil {
			serverResult <- err
			return
		}
		version := make([]byte, 12)
		if _, err := io.ReadFull(server, version); err != nil {
			serverResult <- err
			return
		}
		if string(version) != "RFB 003.003\n" {
			serverResult <- fmt.Errorf("browser version=%q", version)
			return
		}
		security := make([]byte, 4)
		binary.BigEndian.PutUint32(security, uint32(localWebVNCSecurityTypePassword))
		_, err := server.Write(security)
		serverResult <- err
	}()

	version := make([]byte, 12)
	if _, err := io.ReadFull(browser, version); err != nil {
		t.Fatal(err)
	}
	if string(version) != "RFB 003.003\n" {
		t.Fatalf("server version=%q", version)
	}
	if _, err := browser.Write([]byte("RFB 003.003\n")); err != nil {
		t.Fatal(err)
	}
	security := make([]byte, 4)
	if _, err := io.ReadFull(browser, security); err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint32(security); got != uint32(localWebVNCSecurityTypePassword) {
		t.Fatalf("security type=%d", got)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
	if err := <-negotiation; err != nil {
		t.Fatal(err)
	}
}

func TestForceRFBARDAuthenticationAdaptsToNoAuthBrowser(t *testing.T) {
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
		negotiation <- forceRFBARDAuthentication(ctx, bridgeBrowser, bridgeServer, rfbCredentials{
			Username: "ec2-user",
			Password: "example-pass",
		})
	}()
	serverResult := make(chan error, 1)
	go func() {
		serverResult <- serveTestARDHandshakeUntilSecurityResult(server, "ec2-user", "example-pass")
	}()

	version := make([]byte, 12)
	if _, err := io.ReadFull(browser, version); err != nil {
		t.Fatal(err)
	}
	if string(version) != "RFB 003.008\n" {
		t.Fatalf("server version=%q", version)
	}
	if _, err := browser.Write([]byte("RFB 003.008\n")); err != nil {
		t.Fatal(err)
	}
	filtered := make([]byte, 2)
	if _, err := io.ReadFull(browser, filtered); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(filtered, []byte{1, localWebVNCSecurityTypeNone}) {
		t.Fatalf("filtered security types=%v", filtered)
	}
	if _, err := browser.Write([]byte{localWebVNCSecurityTypeNone}); err != nil {
		t.Fatal(err)
	}
	result := make([]byte, 4)
	if _, err := io.ReadFull(browser, result); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result, []byte{0, 0, 0, 0}) {
		t.Fatalf("security result=%v", result)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
	if err := <-negotiation; err != nil {
		t.Fatal(err)
	}
}

func TestForceRFBARDAuthenticationAdaptsLegacyVNCToNoAuthBrowser(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	browser, bridgeBrowser := net.Pipe()
	server, bridgeServer := net.Pipe()
	defer browser.Close()
	defer bridgeBrowser.Close()
	defer server.Close()
	defer bridgeServer.Close()

	const password = "example-pass"
	negotiation := make(chan error, 1)
	go func() {
		negotiation <- forceRFBMacOSAuthentication(ctx, bridgeBrowser, bridgeServer, rfbCredentials{
			Username: "screen-user",
			Password: password,
		}, localWebVNCAuthVNC)
	}()
	serverResult := make(chan error, 1)
	go func() {
		if _, err := server.Write([]byte("RFB 003.008\n")); err != nil {
			serverResult <- err
			return
		}
		version := make([]byte, 12)
		if _, err := io.ReadFull(server, version); err != nil {
			serverResult <- err
			return
		}
		if _, err := server.Write([]byte{1, rfbSecurityVNC}); err != nil {
			serverResult <- err
			return
		}
		selected := []byte{0}
		if _, err := io.ReadFull(server, selected); err != nil {
			serverResult <- err
			return
		}
		if selected[0] != rfbSecurityVNC {
			serverResult <- fmt.Errorf("selected security type=%d", selected[0])
			return
		}
		challenge := []byte("0123456789abcdef")
		if _, err := server.Write(challenge); err != nil {
			serverResult <- err
			return
		}
		response := make([]byte, len(challenge))
		if _, err := io.ReadFull(server, response); err != nil {
			serverResult <- err
			return
		}
		expected, err := directSSHWebVNCChallengeResponse(password, challenge)
		if err != nil {
			serverResult <- err
			return
		}
		if !bytes.Equal(response, expected) {
			serverResult <- fmt.Errorf("unexpected VNC challenge response")
			return
		}
		_, err = server.Write([]byte{0, 0, 0, 0})
		serverResult <- err
	}()

	version := make([]byte, 12)
	if _, err := io.ReadFull(browser, version); err != nil {
		t.Fatal(err)
	}
	if _, err := browser.Write([]byte("RFB 003.008\n")); err != nil {
		t.Fatal(err)
	}
	filtered := make([]byte, 2)
	if _, err := io.ReadFull(browser, filtered); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(filtered, []byte{1, localWebVNCSecurityTypeNone}) {
		t.Fatalf("filtered security types=%v", filtered)
	}
	if _, err := browser.Write([]byte{localWebVNCSecurityTypeNone}); err != nil {
		t.Fatal(err)
	}
	result := make([]byte, 4)
	if _, err := io.ReadFull(browser, result); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result, []byte{0, 0, 0, 0}) {
		t.Fatalf("security result=%v", result)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
	if err := <-negotiation; err != nil {
		t.Fatal(err)
	}
}

func TestForceRFBARDAuthenticationAdaptsRFB33VNCToNoAuthBrowser(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	browser, bridgeBrowser := net.Pipe()
	server, bridgeServer := net.Pipe()
	defer browser.Close()
	defer bridgeBrowser.Close()
	defer server.Close()
	defer bridgeServer.Close()

	const password = "example-pass"
	negotiation := make(chan error, 1)
	go func() {
		negotiation <- forceRFBMacOSAuthentication(ctx, bridgeBrowser, bridgeServer, rfbCredentials{Password: password}, localWebVNCAuthVNC)
	}()
	serverResult := make(chan error, 1)
	go func() {
		if _, err := server.Write([]byte("RFB 003.003\n")); err != nil {
			serverResult <- err
			return
		}
		version := make([]byte, 12)
		if _, err := io.ReadFull(server, version); err != nil {
			serverResult <- err
			return
		}
		security := make([]byte, 4)
		binary.BigEndian.PutUint32(security, uint32(rfbSecurityVNC))
		if _, err := server.Write(security); err != nil {
			serverResult <- err
			return
		}
		challenge := []byte("0123456789abcdef")
		if _, err := server.Write(challenge); err != nil {
			serverResult <- err
			return
		}
		response := make([]byte, len(challenge))
		if _, err := io.ReadFull(server, response); err != nil {
			serverResult <- err
			return
		}
		expected, err := directSSHWebVNCChallengeResponse(password, challenge)
		if err != nil {
			serverResult <- err
			return
		}
		if !bytes.Equal(response, expected) {
			serverResult <- fmt.Errorf("unexpected VNC challenge response")
			return
		}
		_, err = server.Write([]byte{0, 0, 0, 0})
		serverResult <- err
	}()

	version := make([]byte, 12)
	if _, err := io.ReadFull(browser, version); err != nil {
		t.Fatal(err)
	}
	if string(version) != "RFB 003.003\n" {
		t.Fatalf("server version=%q", version)
	}
	if _, err := browser.Write([]byte("RFB 003.003\n")); err != nil {
		t.Fatal(err)
	}
	security := make([]byte, 4)
	if _, err := io.ReadFull(browser, security); err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint32(security); got != uint32(localWebVNCSecurityTypeNone) {
		t.Fatalf("browser security type=%d", got)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
	if err := <-negotiation; err != nil {
		t.Fatal(err)
	}
}

func TestForceRFBARDAuthenticationOmitsNoneResultForRFB37Browser(t *testing.T) {
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
		negotiation <- forceRFBMacOSAuthentication(ctx, bridgeBrowser, bridgeServer, rfbCredentials{Password: "example-pass"}, localWebVNCAuthVNC)
	}()
	serverErr := make(chan error, 1)
	go func() {
		if _, err := server.Write([]byte("RFB 003.007\n")); err != nil {
			serverErr <- err
			return
		}
		version := make([]byte, 12)
		if _, err := io.ReadFull(server, version); err != nil {
			serverErr <- err
			return
		}
		if _, err := server.Write([]byte{1, rfbSecurityVNC}); err != nil {
			serverErr <- err
			return
		}
		selected := []byte{0}
		if _, err := io.ReadFull(server, selected); err != nil {
			serverErr <- err
			return
		}
		if _, err := server.Write([]byte("0123456789abcdef")); err != nil {
			serverErr <- err
			return
		}
		response := make([]byte, 16)
		if _, err := io.ReadFull(server, response); err != nil {
			serverErr <- err
			return
		}
		_, err := server.Write([]byte{0, 0, 0, 0})
		serverErr <- err
	}()

	version := make([]byte, 12)
	if _, err := io.ReadFull(browser, version); err != nil {
		t.Fatal(err)
	}
	if _, err := browser.Write([]byte("RFB 003.007\n")); err != nil {
		t.Fatal(err)
	}
	filtered := make([]byte, 2)
	if _, err := io.ReadFull(browser, filtered); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(filtered, []byte{1, localWebVNCSecurityTypeNone}) {
		t.Fatalf("filtered security types=%v", filtered)
	}
	if _, err := browser.Write([]byte{localWebVNCSecurityTypeNone}); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	if err := <-negotiation; err != nil {
		t.Fatal(err)
	}
	if err := browser.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	unexpected := []byte{0}
	if _, err := browser.Read(unexpected); err == nil {
		t.Fatalf("unexpected RFB 3.8 SecurityResult byte=%v", unexpected)
	}
}

func TestForceRFBMacOSAuthenticationHandlesUpstreamNoneResultByVersion(t *testing.T) {
	for _, test := range []struct {
		name              string
		version           string
		serverResult      []byte
		wantBrowserResult bool
	}{
		{
			name:    "RFB 3.7 omits SecurityResult",
			version: "RFB 003.007\n",
		},
		{
			name:              "RFB 3.8 includes SecurityResult",
			version:           "RFB 003.008\n",
			serverResult:      []byte{0, 0, 0, 0},
			wantBrowserResult: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			browser, bridgeBrowser := net.Pipe()
			server, bridgeServer := net.Pipe()
			defer browser.Close()
			defer bridgeBrowser.Close()
			defer server.Close()
			defer bridgeServer.Close()
			if err := browser.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := server.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}

			negotiation := make(chan error, 1)
			go func() {
				negotiation <- forceRFBMacOSAuthenticationWithTimeout(
					ctx,
					bridgeBrowser,
					bridgeServer,
					rfbCredentials{},
					localWebVNCAuthAuto,
					250*time.Millisecond,
				)
			}()
			serverErr := make(chan error, 1)
			go func() {
				if _, err := server.Write([]byte(test.version)); err != nil {
					serverErr <- err
					return
				}
				version := make([]byte, 12)
				if _, err := io.ReadFull(server, version); err != nil {
					serverErr <- err
					return
				}
				if string(version) != test.version {
					serverErr <- fmt.Errorf("client version=%q, want %q", version, test.version)
					return
				}
				if _, err := server.Write([]byte{1, rfbSecurityNone}); err != nil {
					serverErr <- err
					return
				}
				selected := []byte{0}
				if _, err := io.ReadFull(server, selected); err != nil {
					serverErr <- err
					return
				}
				if selected[0] != rfbSecurityNone {
					serverErr <- fmt.Errorf("selected security type=%d, want %d", selected[0], rfbSecurityNone)
					return
				}
				if len(test.serverResult) > 0 {
					if _, err := server.Write(test.serverResult); err != nil {
						serverErr <- err
						return
					}
				}
				serverErr <- nil
			}()

			version := make([]byte, 12)
			if _, err := io.ReadFull(browser, version); err != nil {
				t.Fatal(err)
			}
			if string(version) != test.version {
				t.Fatalf("server version=%q, want %q", version, test.version)
			}
			if _, err := browser.Write([]byte(test.version)); err != nil {
				t.Fatal(err)
			}
			filtered := make([]byte, 2)
			if _, err := io.ReadFull(browser, filtered); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(filtered, []byte{1, localWebVNCSecurityTypeNone}) {
				t.Fatalf("filtered security types=%v", filtered)
			}
			if _, err := browser.Write([]byte{localWebVNCSecurityTypeNone}); err != nil {
				t.Fatal(err)
			}
			if test.wantBrowserResult {
				result := make([]byte, 4)
				if _, err := io.ReadFull(browser, result); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(result, test.serverResult) {
					t.Fatalf("browser security result=%v, want %v", result, test.serverResult)
				}
			} else {
				if err := browser.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
					t.Fatal(err)
				}
				probe := make([]byte, 1)
				n, readErr := browser.Read(probe)
				var netErr net.Error
				if n != 0 || !errors.As(readErr, &netErr) || !netErr.Timeout() {
					t.Fatalf("RFB 3.7 browser received unexpected security result: n=%d err=%v", n, readErr)
				}
				if err := browser.SetReadDeadline(time.Time{}); err != nil {
					t.Fatal(err)
				}
			}
			if err := <-serverErr; err != nil {
				t.Fatal(err)
			}
			if err := <-negotiation; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRelayWebSocketVNCWithARDAuthenticationTimeoutIsConfigurable(t *testing.T) {
	t.Run("short timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		viewerWS, bridgeWS := testWebSocketPair(t, ctx)
		defer viewerWS.Close(websocket.StatusNormalClosure, "")
		defer bridgeWS.Close(websocket.StatusNormalClosure, "")
		viewer := websocket.NetConn(ctx, viewerWS, websocket.MessageBinary)
		defer viewer.Close()
		server, bridgeTCP := net.Pipe()
		defer server.Close()
		defer bridgeTCP.Close()

		serverResult := make(chan error, 1)
		go func() {
			serverResult <- serveTestARDHandshakeUntilSecurityResult(server, "ec2-user", "example-pass")
		}()
		relayResult := make(chan error, 1)
		go func() {
			relayResult <- relayWebSocketVNCWithARDAuthenticationWithTimeout(ctx, bridgeWS, bridgeTCP, rfbCredentials{
				Username: "ec2-user",
				Password: "example-pass",
			}, 20*time.Millisecond)
		}()

		version := make([]byte, 12)
		if _, err := io.ReadFull(viewer, version); err != nil {
			t.Fatal(err)
		}
		if string(version) != "RFB 003.008\n" {
			t.Fatalf("server version=%q", version)
		}
		select {
		case err := <-relayResult:
			if err == nil || !strings.Contains(err.Error(), "timed out") {
				t.Fatalf("relay error=%v, want timeout", err)
			}
		case <-time.After(time.Second):
			t.Fatal("relay did not time out")
		}
		_ = bridgeTCP.Close()
		_ = server.Close()
		select {
		case <-serverResult:
		case <-time.After(time.Second):
			t.Fatal("fake RFB server did not exit")
		}
	})

	t.Run("portal delayed viewer", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		viewerWS, bridgeWS := testWebSocketPair(t, ctx)
		defer viewerWS.Close(websocket.StatusNormalClosure, "")
		defer bridgeWS.Close(websocket.StatusNormalClosure, "")
		viewer := websocket.NetConn(ctx, viewerWS, websocket.MessageBinary)
		defer viewer.Close()
		server, bridgeTCP := net.Pipe()
		defer server.Close()
		defer bridgeTCP.Close()

		serverResult := make(chan error, 1)
		go func() {
			serverResult <- serveTestARDHandshakeUntilSecurityResult(server, "ec2-user", "example-pass")
		}()
		relayResult := make(chan error, 1)
		go func() {
			relayResult <- relayWebSocketVNCWithARDAuthenticationWithTimeout(ctx, bridgeWS, bridgeTCP, rfbCredentials{
				Username: "ec2-user",
				Password: "example-pass",
			}, 0)
		}()

		version := make([]byte, 12)
		if _, err := io.ReadFull(viewer, version); err != nil {
			t.Fatal(err)
		}
		if string(version) != "RFB 003.008\n" {
			t.Fatalf("server version=%q", version)
		}
		time.Sleep(50 * time.Millisecond)
		if _, err := viewer.Write([]byte("RFB 003.008\n")); err != nil {
			t.Fatal(err)
		}
		filtered := make([]byte, 2)
		if _, err := io.ReadFull(viewer, filtered); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(filtered, []byte{1, localWebVNCSecurityTypeNone}) {
			t.Fatalf("filtered security types=%v", filtered)
		}
		if _, err := viewer.Write([]byte{localWebVNCSecurityTypeNone}); err != nil {
			t.Fatal(err)
		}
		result := make([]byte, 4)
		if _, err := io.ReadFull(viewer, result); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(result, []byte{0, 0, 0, 0}) {
			t.Fatalf("security result=%v", result)
		}
		if err := <-serverResult; err != nil {
			t.Fatal(err)
		}
		cancel()
		if err := <-relayResult; !errors.Is(err, context.Canceled) {
			t.Fatalf("relay error=%v, want context cancellation after successful negotiation", err)
		}
	})
}

func TestRelayWebSocketVNCWithMacOSAuthenticationUsesVNCFailureDiagnostics(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	viewerWS, bridgeWS := testWebSocketPair(t, ctx)
	defer viewerWS.Close(websocket.StatusNormalClosure, "")
	defer bridgeWS.Close(websocket.StatusNormalClosure, "")
	server, bridgeTCP := net.Pipe()
	defer server.Close()
	defer bridgeTCP.Close()

	relayResult := make(chan error, 1)
	go func() {
		relayResult <- relayWebSocketVNCWithMacOSAuthenticationWithTimeout(
			ctx,
			bridgeWS,
			bridgeTCP,
			rfbCredentials{Password: "example-pass"},
			localWebVNCAuthVNC,
			time.Second,
		)
	}()
	if _, err := server.Write([]byte("RFB 006.000\n")); err != nil {
		t.Fatal(err)
	}
	_, _, closeErr := viewerWS.Read(ctx)
	if status := websocket.CloseStatus(closeErr); status != websocket.StatusPolicyViolation {
		t.Fatalf("close status=%v error=%v", status, closeErr)
	}
	if !strings.Contains(closeErr.Error(), "VNC authentication negotiation failed") || strings.Contains(closeErr.Error(), "ARD authentication") {
		t.Fatalf("close error=%v", closeErr)
	}

	err := <-relayResult
	if err == nil || !strings.Contains(err.Error(), "VNC authentication negotiation failed") || strings.Contains(err.Error(), "ARD authentication") {
		t.Fatalf("relay error=%v", err)
	}
}

func testWebSocketPair(t *testing.T, ctx context.Context) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	acceptErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- ws
		<-ctx.Done()
	}))
	t.Cleanup(server.Close)
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case serverWS := <-accepted:
		return client, serverWS
	case err := <-acceptErr:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return nil, nil
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

func TestRFBClientVersionForServer(t *testing.T) {
	for _, test := range []struct {
		server string
		want   string
	}{
		{server: "RFB 003.003\n", want: "RFB 003.003\n"},
		{server: "RFB 003.006\n", want: "RFB 003.003\n"},
		{server: "RFB 003.007\n", want: "RFB 003.007\n"},
		{server: "RFB 003.008\n", want: "RFB 003.008\n"},
		{server: "RFB 003.009\n", want: "RFB 003.003\n"},
		{server: "RFB 003.889\n", want: "RFB 003.008\n"},
		{server: "RFB 004.000\n", want: "RFB 003.008\n"},
	} {
		t.Run(test.server[4:11], func(t *testing.T) {
			got, err := rfbClientVersionForServer([]byte(test.server))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != test.want {
				t.Fatalf("client version=%q, want %q", got, test.want)
			}
		})
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
	if string(version) != "RFB 003.008\n" {
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
			localWebVNCAuthAuto,
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
	token := strings.TrimPrefix(protocol, "crabbox.")
	credentialRequest, err := http.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:"+webPort+"/credentials",
		strings.NewReader(url.Values{"token": {token}}.Encode()),
	)
	if err != nil {
		t.Fatal(err)
	}
	credentialRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	credentialRequest.Header.Set("Origin", "null")
	credentialResponse, err := http.DefaultClient.Do(credentialRequest)
	if err != nil {
		t.Fatal(err)
	}
	credentialBody, readErr := io.ReadAll(credentialResponse.Body)
	credentialResponse.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if credentialResponse.StatusCode != http.StatusOK ||
		!bytes.Contains(credentialBody, []byte(`"username":"admin"`)) ||
		!bytes.Contains(credentialBody, []byte(`"password":"super-secret"`)) {
		t.Fatalf("credentials status=%d body=%s", credentialResponse.StatusCode, credentialBody)
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

func TestServeLocalWebVNCBridgeScrubsPassedTargetEnvironmentFromViewer(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	webPort := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	name, _ := openURLCommand("https://example.test")
	if name == "" {
		listener.Close()
		t.Skip("local URL opening unsupported")
	}
	dir := t.TempDir()
	result := filepath.Join(dir, "viewer-environment")
	opener := filepath.Join(dir, name)
	script := "#!/bin/sh\nif [ \"${TEST_EXTERNAL_DESKTOP_PASSWORD+x}\" = x ]; then printf leaked > " + shellQuote(result) + "; else printf scrubbed > " + shellQuote(result) + "; fi\n"
	if err := os.WriteFile(opener, []byte(script), 0o755); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_EXTERNAL_DESKTOP_PASSWORD", "must-not-reach-viewer")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := newNotifyBuffer("webvnc:")
	errCh := make(chan error, 1)
	go func() {
		errCh <- (App{Stdout: output, Stderr: io.Discard}).serveLocalWebVNCBridge(
			ctx,
			listener,
			webPort,
			rfbCredentials{Password: "viewer-password"},
			true,
			localWebVNCAuthVNC,
			func(context.Context) (net.Conn, error) {
				return nil, errors.New("unexpected viewer VNC dial")
			},
			nil,
			"TEST_EXTERNAL_DESKTOP_PASSWORD",
		)
	}()

	select {
	case <-output.ready:
	case err := <-errCh:
		t.Fatalf("bridge exited before viewer launch: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for WebVNC viewer launch")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(result); err == nil {
			if string(data) != "scrubbed" {
				t.Fatalf("viewer environment=%q", data)
			}
			cancel()
			if err := <-errCh; !errors.Is(err, context.Canceled) {
				t.Fatalf("bridge error=%v", err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-errCh
	t.Fatal("fake viewer did not report its environment")
}

func TestServeLocalWebVNCBridgeKeepsARDCredentialsServerSide(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	webPort := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := newNotifyBuffer("webvnc:")
	errCh := make(chan error, 1)
	go func() {
		errCh <- (App{Stdout: output, Stderr: io.Discard}).serveLocalWebVNCBridge(
			ctx,
			listener,
			webPort,
			rfbCredentials{Username: "ard-admin", Password: "ard-super-secret"},
			false,
			localWebVNCAuthARD,
			func(context.Context) (net.Conn, error) {
				t.Error("ARD credential boundary test unexpectedly dialed VNC")
				return nil, errors.New("unexpected VNC dial")
			},
			nil,
		)
	}()

	select {
	case <-output.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ARD WebVNC handoff")
	}
	handoffURL := strings.TrimSpace(strings.TrimPrefix(output.String(), "webvnc:"))
	parsed, err := url.Parse(handoffURL)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(parsed.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"ard-admin",
		"ard-super-secret",
		"/credentials",
		`"credentialsURL"`,
	} {
		if bytes.Contains(content, []byte(forbidden)) {
			t.Fatalf("ARD handoff contains %q", forbidden)
		}
	}

	protocol := regexp.MustCompile(`crabbox\.[0-9a-f]{32}`).FindString(string(content))
	if protocol == "" {
		t.Fatal("ARD handoff missing session protocol")
	}
	credentialRequest, err := http.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:"+webPort+"/credentials",
		strings.NewReader(url.Values{"token": {strings.TrimPrefix(protocol, "crabbox.")}}.Encode()),
	)
	if err != nil {
		t.Fatal(err)
	}
	credentialRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	credentialRequest.Header.Set("Origin", "null")
	credentialResponse, err := http.DefaultClient.Do(credentialRequest)
	if err != nil {
		t.Fatal(err)
	}
	credentialBody, readErr := io.ReadAll(credentialResponse.Body)
	credentialResponse.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if credentialResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("ARD credentials status=%d body=%s", credentialResponse.StatusCode, credentialBody)
	}

	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("ARD bridge error=%v", err)
	}
	if _, err := os.Stat(parsed.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ARD handoff file remained after bridge exit: %v", err)
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
