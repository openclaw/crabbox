//go:build windows

package cli

func secureSSHTransportPath(path string, directory bool) error {
	return securePrivateWindowsPath(path, directory)
}

func verifySSHTransportPathPrivate(path string, directory bool) error {
	return verifyPrivateWindowsPath(path, directory)
}
