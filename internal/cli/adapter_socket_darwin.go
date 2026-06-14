//go:build darwin

package cli

import (
	"fmt"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func verifyAdapterUnixPeer(conn net.Conn) error {
	raw, ok := conn.(syscall.Conn)
	if !ok {
		return fmt.Errorf("local adapter Unix connection does not expose peer credentials")
	}
	var credentials *unix.Xucred
	var controlErr error
	rawConn, err := raw.SyscallConn()
	if err != nil {
		return fmt.Errorf("inspect local adapter Unix peer: %w", err)
	}
	err = rawConn.Control(func(fd uintptr) {
		credentials, controlErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	})
	if err != nil {
		return fmt.Errorf("inspect local adapter Unix peer: %w", err)
	}
	if controlErr != nil {
		return fmt.Errorf("inspect local adapter Unix peer: %w", controlErr)
	}
	if credentials == nil || credentials.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("local adapter Unix peer is not owned by the current user")
	}
	return nil
}
