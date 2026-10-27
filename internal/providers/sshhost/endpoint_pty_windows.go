package sshhost

import (
	"fmt"
	"os"
	"os/exec"
)

// MXC terminal allocation remains experimental; non-PTY shell/exec sessions
// still run inside the isolated account. Never substitute the operator's PTY.
func endpointPTYSupported() bool { return false }
func startEndpointPTY(*exec.Cmd, *endpointPTYRequest) (*os.File, error) {
	return nil, fmt.Errorf("managed Windows PTY is not supported")
}
func resizeEndpointPTY(*os.File, uint16, uint16) error {
	return fmt.Errorf("managed Windows PTY is not supported")
}
