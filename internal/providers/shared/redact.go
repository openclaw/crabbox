package shared

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const redactedProviderSecret = "[redacted]"

// RedactErrorSecrets removes credentials from untrusted provider response text
// while preserving the surrounding status and diagnostic detail.
func RedactErrorSecrets(value string, secrets ...string) string {
	return core.RedactDiagnosticSecrets(value, secrets...)
}

// RedactedResponseBody formats a status-first API error without promoting the
// body-read error into its cause chain. Redact before truncating so a cutoff
// cannot leave credential fragments; apply the same policy to read diagnostics.
// A nonpositive limit preserves the whole redacted body. The adapter owns redact.
func RedactedResponseBody(data []byte, readErr error, limit int, redact func(string) string) string {
	body := redact(strings.TrimSpace(string(data)))
	if limit > 0 && len(body) > limit {
		body = body[:limit]
	}
	if readErr != nil {
		if body != "" {
			body += "; "
		}
		body += "response body read failed: " + redact(readErr.Error())
	}
	return body
}

// ErrorWithMessage replaces only Error's presentation while retaining cause.
// It does not redact, select an exit code, or override errors.As: a CLI rendering
// an inner ExitError can still select that inner message. Nil cause returns nil.
func ErrorWithMessage(message string, cause error) error {
	if cause == nil {
		return nil
	}
	return &messageCauseError{message: message, cause: cause}
}

type messageCauseError struct {
	message string
	cause   error
}

func (e *messageCauseError) Error() string { return e.message }
func (e *messageCauseError) Unwrap() error { return e.cause }
