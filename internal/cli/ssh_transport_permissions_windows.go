//go:build windows

package cli

func createPrivateSSHTransportDirectory(path string) error {
	return createPrivateRunOutputDir(path)
}

func secureSSHTransportPath(path string, directory bool) error {
	return securePrivateWindowsPath(path, directory)
}

func verifySSHTransportPathPrivate(path string, directory bool) error {
	return verifyPrivateWindowsPath(path, directory)
}
