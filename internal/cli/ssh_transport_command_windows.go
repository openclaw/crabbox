//go:build windows

package cli

func sshProxyCommandWords(words []string) []string {
	quoted := make([]string, len(words))
	for index, word := range words {
		quoted[index] = quoteWindowsCommandArg(word)
	}
	return quoted
}
