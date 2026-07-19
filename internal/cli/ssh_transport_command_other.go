//go:build !windows

package cli

func sshProxyCommandWords(words []string) []string {
	return shellWords(words)
}
