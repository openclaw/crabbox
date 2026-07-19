//go:build windows

package cli

import "strings"

func sshProxyCommandWords(words []string) []string {
	quoted := make([]string, len(words))
	for index, word := range words {
		quoted[index] = quoteWindowsCommandArg(word)
	}
	return quoted
}

func quoteWindowsCommandArg(arg string) string {
	var quoted strings.Builder
	quoted.WriteByte('"')
	backslashes := 0
	for _, char := range arg {
		switch char {
		case '\\':
			backslashes++
		case '"':
			quoted.WriteString(strings.Repeat(`\`, backslashes*2+1))
			quoted.WriteRune(char)
			backslashes = 0
		default:
			quoted.WriteString(strings.Repeat(`\`, backslashes))
			quoted.WriteRune(char)
			backslashes = 0
		}
	}
	quoted.WriteString(strings.Repeat(`\`, backslashes*2))
	quoted.WriteByte('"')
	return quoted.String()
}
