//go:build windows

package cli

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const windowsWebVNCTestInheritedListenerHelperEnv = "CRABBOX_TEST_WINDOWS_INHERITED_WEBVNC_LISTENER"

func TestWindowsWebVNCPortReservationBusyErrors(t *testing.T) {
	for _, err := range []error{windows.WSAEADDRINUSE, windows.WSAEACCES} {
		if !webVNCDaemonPortReservationUnavailable(err) {
			t.Fatalf("busy bind error was not classified as unavailable: %v", err)
		}
	}
	if webVNCDaemonPortReservationUnavailable(errors.New("unexpected")) {
		t.Fatal("unrelated bind error was classified as unavailable")
	}
}

func TestWindowsWebVNCDaemonInheritedRawSocketAcceptsConnection(t *testing.T) {
	reservation, err := reserveWebVNCDaemonPort("")
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.release()
	port := reservation.port
	cmd := exec.Command(os.Args[0], "-test.run=TestWindowsWebVNCDaemonInheritedRawSocketHelper", "--")
	descriptor, err := reservation.inherit(cmd)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(),
		windowsWebVNCTestInheritedListenerHelperEnv+"=1",
		webVNCDaemonPortReservationEnv+"="+port,
		webVNCDaemonPortReservationFDEnv+"="+descriptor,
	)
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

	var conn net.Conn
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err = net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", port), 100*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("dial inherited Windows listener: %v output=%s", err, output.String())
	}
	if _, err := io.WriteString(conn, "ping"); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	data := make([]byte, 4)
	_, readErr := io.ReadFull(conn, data)
	_ = conn.Close()
	waitErr := cmd.Wait()
	if readErr != nil || waitErr != nil || string(data) != "ping" {
		t.Fatalf("inherited listener data=%q read=%v wait=%v output=%s", data, readErr, waitErr, output.String())
	}
}

func TestWindowsWebVNCDaemonInheritedRawSocketHelper(t *testing.T) {
	if os.Getenv(windowsWebVNCTestInheritedListenerHelperEnv) != "1" {
		return
	}
	port := os.Getenv(webVNCDaemonPortReservationEnv)
	if _, err := strconv.ParseUint(os.Getenv(webVNCDaemonPortReservationFDEnv), 10, 64); err != nil {
		t.Fatal(err)
	}
	listener, err := inheritedWebVNCDaemonListener(port)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	data := make([]byte, 4)
	if _, err := io.ReadFull(conn, data); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(data); err != nil {
		t.Fatal(err)
	}
}
