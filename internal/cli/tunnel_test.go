package cli

import (
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestParseTunnelPort(t *testing.T) {
	for _, value := range []string{"1", "3000", "65535"} {
		if got, err := parseTunnelPort(value, "remote port", false); err != nil || got != value {
			t.Fatalf("parse %q got=%q err=%v", value, got, err)
		}
	}
	for _, value := range []string{"", "0", "-1", "65536", "http"} {
		if _, err := parseTunnelPort(value, "remote port", false); err == nil {
			t.Fatalf("parse %q unexpectedly succeeded", value)
		}
	}
	if got, err := parseTunnelPort("0", "local port", true); err != nil || got != "" {
		t.Fatalf("auto local port got=%q err=%v", got, err)
	}
}

func TestResolvedSSHTunnelArgsBindLoopback(t *testing.T) {
	session := &sshTransportSession{configPath: "/private/config"}
	args := resolvedSSHTunnelArgs(session, "41000", "3000")
	got := strings.Join(args, " ")
	if !strings.Contains(got, "-L 127.0.0.1:41000:127.0.0.1:3000") {
		t.Fatalf("args=%q", got)
	}
	if strings.Contains(got, "0.0.0.0") || strings.Contains(got, "[::]") {
		t.Fatalf("tunnel exposed non-loopback bind: %q", got)
	}
}

func TestReserveSSHLocalForwardPort(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	reservation, err := reserveSSHLocalForwardPort("")
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.release()
	if net.ParseIP(sshTunnelLoopbackHost) == nil || reservation.port == "" {
		t.Fatalf("reservation=%#v", reservation)
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(sshTunnelLoopbackHost, reservation.port))
	if err != nil {
		t.Fatalf("reservation should not occupy TCP before SSH starts: %v", err)
	}
	_ = listener.Close()
}

func TestReserveSSHLocalForwardPortIgnoresUDPUse(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	port := strconv.Itoa(udp.LocalAddr().(*net.UDPAddr).Port)
	reservation, err := reserveSSHLocalForwardPort(port)
	if err != nil {
		t.Fatalf("UDP use should not reserve TCP port %s: %v", port, err)
	}
	reservation.release()
}

func TestSynchronizedTunnelTailBufferIsBounded(t *testing.T) {
	buffer := newSynchronizedTailBuffer(2)
	for _, line := range []string{"one\n", "two\n", "three\n"} {
		if _, err := buffer.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if got := buffer.String(); got != "two\nthree" {
		t.Fatalf("tail=%q", got)
	}
}
