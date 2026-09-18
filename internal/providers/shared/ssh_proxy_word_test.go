package shared

import (
	"os/exec"
	"strings"
	"testing"
)

func TestQuoteSSHProxyCommandWord(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{"", `""`},
		{"safe/path:22", "safe/path:22"},
		{"%h", "%%h"},
		{"%%", "%%%%"},
		{"with spaces", `"with spaces"`},
	} {
		if got := QuoteSSHProxyCommandWord(tt.input); got != tt.want {
			t.Errorf("quote(%q)=%q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSSHProxyWordsRemainLiteralAfterPercentAndShellParsing(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell is unavailable")
	}
	for _, word := range []string{"", "with spaces", "$HOME", "$(printf unexpected)", "`printf unexpected`", `a"b\c`, "it's literal", "%h-%%-%p", "a\nb"} {
		t.Run(word, func(t *testing.T) {
			encoded := QuoteSSHProxyCommandWord(word)
			// OpenSSH consumes percent escapes before invoking the shell.
			expanded := strings.NewReplacer("%%", "%", "%h", "example.invalid", "%p", "2222").Replace(encoded)
			out, err := exec.CommandContext(t.Context(), sh, "-c", "printf '%s' "+expanded).Output()
			if err != nil || string(out) != word {
				t.Fatalf("round trip=(%q, %v), want %q", out, err, word)
			}
		})
	}
}
