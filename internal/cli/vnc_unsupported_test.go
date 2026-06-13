//go:build !linux && !darwin && !windows

package cli

import (
	"context"
	"net"
	"testing"
)

func TestManualVNCTunnelsFailClosedWithoutListenerOwnershipSupport(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ready, err := startedTunnelListenerReady(context.Background(), port, 1)
	if err == nil || ready {
		t.Fatalf("unsupported listener ownership ready=%t err=%v", ready, err)
	}
}

func TestControllerHostFailsClosedOutsideLinuxAndDarwin(t *testing.T) {
	if err := controllerHostSupported(); err == nil {
		t.Fatal("unsupported controller host was accepted")
	}
}
