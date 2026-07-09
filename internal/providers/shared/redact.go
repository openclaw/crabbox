package shared

import core "github.com/openclaw/crabbox/internal/cli"

const redactedProviderSecret = "[redacted]"

// RedactErrorSecrets removes credentials from untrusted provider response text
// while preserving the surrounding status and diagnostic detail.
func RedactErrorSecrets(value string, secrets ...string) string {
	return core.RedactDiagnosticSecrets(value, secrets...)
}
