package cli

import "strings"

// PowerShell also accepts U+2018, U+2019, U+201A and U+201B as single quotes,
// so each one must be doubled to stay inside the verbatim string.
var psQuoteReplacer = strings.NewReplacer(
	"'", "''",
	"\u2018", "\u2018\u2018",
	"\u2019", "\u2019\u2019",
	"\u201a", "\u201a\u201a",
	"\u201b", "\u201b\u201b",
)

func psQuote(s string) string {
	return "'" + psQuoteReplacer.Replace(s) + "'"
}

// PowerShellQuote returns s as one PowerShell verbatim string literal.
func PowerShellQuote(s string) string {
	return psQuote(s)
}
