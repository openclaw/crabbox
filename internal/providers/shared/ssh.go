package shared

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// QuoteSSHProxyCommandWord encodes a literal argument for OpenSSH's percent
// expansion followed by POSIX shell parsing. Intentional SSH tokens stay with
// the caller rather than passing through this literal-word encoder.
func QuoteSSHProxyCommandWord(word string) string {
	word = strings.ReplaceAll(word, "%", "%%")
	if word != "" && strings.IndexFunc(word, func(r rune) bool {
		return !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			strings.ContainsRune("_-./:,@%+=", r))
	}) == -1 {
		return word
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(word) + `"`
}

func UseStoredTestboxKey(target *core.SSHTarget, leaseID string) error {
	return core.UseStoredTestboxKey(target, leaseID)
}
