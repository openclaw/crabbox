package cli

func UserConfigDirectory() (string, error) {
	return userConfigDirectory()
}

func SecurePrivatePath(path string, directory bool) error {
	return secureSSHTransportPath(path, directory)
}
