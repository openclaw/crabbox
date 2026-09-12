package shared

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// LocalCommandError preserves a failed tool's nonzero exit code and selects
// stderr before stdout. Callers retain execution, redaction, and retry policy.
func LocalCommandError(action string, result core.LocalCommandResult, err error) error {
	code := result.ExitCode
	if code == 0 {
		code = 1
	}
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail != "" {
		return core.Exit(code, "%s failed: %v: %s", action, err, detail)
	}
	return core.Exit(code, "%s failed: %v", action, err)
}
