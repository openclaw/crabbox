package localcontainer

import (
	"context"
	"fmt"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const bootstrapDiagnosticLimit = 8 * 1024

// Read only the already-verified exact container, before any failure rollback.
// Logging failures must never replace the readiness failure or delay cleanup indefinitely.
func (b *backend) captureBootstrapDiagnostics(ctx context.Context, containerID string, failure *terminalContainerError) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req := containerRuntimeRequest(b.configForRun(), []string{"logs", "--tail", "100", containerID})
	req.MaxCapturedOutputBytes = bootstrapDiagnosticLimit
	result, err := b.rt.Exec.Run(ctx, req)
	var excerpts []string
	for _, output := range []string{result.Stdout, result.Stderr} {
		if len(output) > bootstrapDiagnosticLimit {
			output = output[:bootstrapDiagnosticLimit]
		}
		if output = strings.TrimSpace(core.RedactDiagnosticSecrets(output)); output != "" {
			excerpts = append(excerpts, output)
		}
	}
	if len(excerpts) > 0 {
		failure.err = fmt.Errorf("%w\nbootstrap logs (last 100 lines, bounded excerpt):\n%s", failure.err, strings.Join(excerpts, "\n"))
	} else if err != nil {
		failure.err = fmt.Errorf("%w; bootstrap logs unavailable", failure.err)
	}
}
