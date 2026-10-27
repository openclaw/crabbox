//go:build !windows

package sshhost

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

func endpointPTYSupported() bool { return true }
func startEndpointPTY(command *exec.Cmd, request *endpointPTYRequest) (*os.File, error) {
	return pty.StartWithSize(command, &pty.Winsize{Rows: uint16(request.Rows), Cols: uint16(request.Columns)})
}
func resizeEndpointPTY(terminal *os.File, rows, columns uint16) error {
	return pty.Setsize(terminal, &pty.Winsize{Rows: rows, Cols: columns})
}
