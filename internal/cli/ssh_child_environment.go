package cli

import (
	"os"
	"os/exec"
	"runtime"
)

const windowsOpenSSHInheritedFDState = "c28fc6f98a2c44abbbd89d6a3037d0d9_POSIX_FD_STATE"

// Go supplies new synchronous handles, not the socket descriptors inherited
// from an sshd-launched parent. Reusing that metadata can stall stdin or large
// stdout transfers even when the remote command has already exited.
func applyNativeSSHChildEnvironment(cmd *exec.Cmd) {
	if runtime.GOOS != "windows" {
		return
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = append(childEnvironmentWithout(env, "OPENSSH_STDIO_MODE", windowsOpenSSHInheritedFDState), "OPENSSH_STDIO_MODE=nonsock_sync")
}
