package shared

import (
	"os"
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

func UseStoredTestboxKey(target *core.SSHTarget, leaseID string) {
	if keyPath, err := core.TestboxKeyPath(leaseID); err == nil {
		if _, statErr := os.Stat(keyPath); statErr == nil {
			target.Key = keyPath
		}
	}
}
