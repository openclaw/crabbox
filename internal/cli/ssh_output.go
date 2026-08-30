package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ErrSSHOutputLimit means a successful command exceeded its stdout budget.
// Transport errors take precedence; callers must not treat those as missing data.
var ErrSSHOutputLimit = errors.New("SSH output exceeded limit")

// RunSSHOutputBounded uses the normal trusted transport and target wrappers,
// retaining at most maxBytes of stdout. The caller supplies a deadline. An empty
// non-nil FallbackPorts pins a previously resolved endpoint, as in other helpers.
// Neither remote stdout nor stderr is included in errors.
func RunSSHOutputBounded(ctx context.Context, target SSHTarget, remote string, maxBytes int) (string, error) {
	return runSSHOutputBoundedWithOptions(ctx, target, remote, maxBytes, 0, "10", "1")
}

func runSSHOutputBoundedWithOptions(ctx context.Context, target SSHTarget, remote string, maxBytes int, waitTimeout time.Duration, connectTimeout, attempts string) (string, error) {
	if maxBytes <= 0 {
		return "", fmt.Errorf("SSH output limit must be positive")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	out := boundedSSHOutput{limit: maxBytes}
	// The transport owner finishes deferred cleanup before any output escapes.
	if err := executeSSH(ctx, &target, remote, nil, 0, waitTimeout, connectTimeout, attempts, &out, io.Discard); err != nil {
		if ctx.Err() != nil {
			// Cancellation must not hide failures reported by deferred cleanup.
			return "", errors.Join(ctx.Err(), err)
		}
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if out.exceeded {
		return "", ErrSSHOutputLimit
	}
	return strings.TrimSpace(out.String()), nil
}

type boundedSSHOutput struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedSSHOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.buffer.Len()
	if n > remaining {
		b.exceeded = true
		p = p[:remaining]
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

func (b *boundedSSHOutput) String() string { return b.buffer.String() }
