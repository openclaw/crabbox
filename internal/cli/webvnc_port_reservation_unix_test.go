//go:build !windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const webVNCTestInheritedListenerHelperEnv = "CRABBOX_TEST_INHERITED_WEBVNC_LISTENER"

func unusedWebVNCTestPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestWebVNCDaemonPortReservationIgnoresStateRoot(t *testing.T) {
	port := unusedWebVNCTestPort(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	first, err := reserveWebVNCDaemonPort(port)
	if err != nil {
		t.Fatal(err)
	}
	defer first.release()

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if second, err := reserveWebVNCDaemonPort(port); err == nil {
		second.release()
		t.Fatal("reservation was scoped to the first state root")
	}
}

func TestWebVNCDaemonPortReservationExcludesBridgePort(t *testing.T) {
	first, err := reserveWebVNCDaemonPort("")
	if err != nil {
		t.Fatal(err)
	}
	excluded := first.port
	first.release()

	second, err := reserveWebVNCDaemonPort("", excluded)
	if err != nil {
		t.Fatal(err)
	}
	defer second.release()
	if second.port == excluded {
		t.Fatalf("excluded bridge port was reused: %s", excluded)
	}
}

func TestWebVNCTunnelPortReservationSerializesSelection(t *testing.T) {
	first, err := reserveWebVNCTunnelPort("")
	if err != nil {
		t.Fatal(err)
	}
	defer first.release()
	second, err := reserveWebVNCTunnelPort("")
	if err != nil {
		t.Fatal(err)
	}
	defer second.release()
	if first.port == second.port {
		t.Fatalf("concurrent tunnel reservations reused port %s", first.port)
	}
	if duplicate, err := reserveWebVNCTunnelPort(first.port); err == nil {
		duplicate.release()
		t.Fatalf("explicit tunnel reservation reused held port %s", first.port)
	}
}

func TestVNCTunnelLocalBindConflictClassification(t *testing.T) {
	for _, message := range []string{
		"bind [127.0.0.1]:5901: Address already in use",
		"channel_setup_fwd_listener_tcpip: cannot listen to port: 5901",
		"local forwarding failed",
	} {
		if !vncTunnelLocalBindConflict(errors.New(message)) {
			t.Fatalf("bind conflict not recognized: %s", message)
		}
	}
	if vncTunnelLocalBindConflict(errors.New("permission denied")) {
		t.Fatal("unrelated SSH failure classified as bind collision")
	}
}

func TestWebVNCDaemonPortReservationSurvivesParentHandoff(t *testing.T) {
	port := unusedWebVNCTestPort(t)
	reservation, err := reserveWebVNCDaemonPort(port)
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.release()

	cmd := exec.Command("sh", "-c", "read _ || :")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reservation.inherit(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := reservation.handoff(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	if duplicate, err := reserveWebVNCDaemonPort(port); err == nil {
		duplicate.release()
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("child did not inherit the port reservation")
	}
	if err := stdin.Close(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		reacquired, err := reserveWebVNCDaemonPort(port)
		if err == nil {
			reacquired.release()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reservation remained locked after child exit: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWebVNCDaemonInheritedTCPListenerAcceptsConnection(t *testing.T) {
	port := unusedWebVNCTestPort(t)
	reservation, err := reserveWebVNCDaemonPort(port)
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.release()

	cmd := exec.Command(os.Args[0], "-test.run=TestWebVNCDaemonInheritedTCPListenerHelper", "--")
	descriptor, err := reservation.inherit(cmd)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(webVNCDaemonPortReservationEnvironment(os.Environ(), port, descriptor), webVNCTestInheritedListenerHelperEnv+"=1")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := reservation.handoff(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", port), time.Second)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("dial inherited TCP listener: %v output=%s", err, output.String())
	}
	data, readErr := io.ReadAll(conn)
	_ = conn.Close()
	waitErr := cmd.Wait()
	if readErr != nil || waitErr != nil || string(data) != "accepted" {
		t.Fatalf("inherited listener data=%q read=%v wait=%v output=%s", data, readErr, waitErr, output.String())
	}
}

func TestWebVNCDaemonInheritedTCPListenerHelper(t *testing.T) {
	if os.Getenv(webVNCTestInheritedListenerHelperEnv) != "1" {
		return
	}
	port := os.Getenv(webVNCDaemonPortReservationEnv)
	listener, err := inheritedWebVNCDaemonListener(port)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.WriteString(conn, "accepted")
	_ = conn.Close()
	if err != nil {
		t.Fatal(err)
	}
}

func TestWebVNCLoopbackProxyUsesReservedTCPListener(t *testing.T) {
	target, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		conn, acceptErr := target.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		data := make([]byte, 4)
		if _, readErr := io.ReadFull(conn, data); readErr == nil {
			_, _ = conn.Write(data)
		}
	}()

	reservation, err := reserveWebVNCDaemonPort("")
	if err != nil {
		t.Fatal(err)
	}
	proxyPort := reservation.port
	listener, err := reservation.listener()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := serveWebVNCLoopbackProxy(ctx, listener, strconv.Itoa(target.Addr().(*net.TCPAddr).Port))
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", proxyPort), time.Second)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "ping"); err != nil {
		cancel()
		_ = conn.Close()
		t.Fatal(err)
	}
	response := make([]byte, 4)
	_, err = io.ReadFull(conn, response)
	_ = conn.Close()
	if err != nil || string(response) != "ping" {
		cancel()
		t.Fatalf("proxy response=%q error=%v", response, err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("proxy shutdown error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy did not stop after cancellation")
	}
}

func TestWebVNCLoopbackProxyRejectsUnexpectedTunnelOwner(t *testing.T) {
	if !controllerListenerOwnershipSupported() {
		t.Skip("listener ownership verification unavailable")
	}
	target, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	targetPort := strconv.Itoa(target.Addr().(*net.TCPAddr).Port)
	if err := controllerVerifyDaemonOwnedListener(targetPort, os.Getpid()); err != nil {
		t.Skipf("listener ownership fixture unavailable: %v", err)
	}

	reservation, err := reserveWebVNCDaemonPort("")
	if err != nil {
		t.Fatal(err)
	}
	proxyPort := reservation.port
	listener, err := reservation.listener()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := serveWebVNCLoopbackProxy(ctx, listener, targetPort, os.Getpid()+1_000_000)
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", proxyPort), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(conn, "credential-bearing request")
	_ = conn.Close()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "verify local VNC tunnel listener") {
			t.Fatalf("proxy ownership error=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not reject the unexpected tunnel owner")
	}
}

func TestDialVNCForegroundTunnelRequiresTrackedOwner(t *testing.T) {
	if !controllerListenerOwnershipSupported() {
		t.Skip("listener ownership verification unavailable")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	if err := controllerVerifyDaemonOwnedListener(port, os.Getpid()); err != nil {
		t.Skipf("listener ownership fixture unavailable: %v", err)
	}

	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	tunnel := &vncForegroundTunnel{
		cmd:    &exec.Cmd{Process: process},
		done:   make(chan struct{}),
		output: &strings.Builder{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := dialVNCForegroundTunnel(ctx, tunnel, port)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()

	unrelated, err := os.FindProcess(os.Getpid() + 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	tunnel.cmd.Process = unrelated
	if conn, err := dialVNCForegroundTunnel(ctx, tunnel, port); err == nil {
		_ = conn.Close()
		t.Fatal("relay dial accepted an unrelated tunnel owner")
	}
}
