//go:build windows

package cli

import (
	"net"
	"os"
	"testing"
)

func TestWindowsLoopbackListenerRequiresExactOwningPID(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := controllerVerifyDaemonOwnedListener(port, os.Getpid()); err != nil {
		t.Fatalf("exact listener owner rejected: %v", err)
	}
	if err := controllerVerifyDaemonOwnedListener(port, os.Getpid()+1); err == nil {
		t.Fatal("unrelated pid accepted a prebound listener")
	}
}
