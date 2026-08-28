package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrSSHOutputLimit means a successful command exceeded its stdout budget.
// Transport errors take precedence; callers must not treat those as missing data.
var ErrSSHOutputLimit = errors.New("SSH output exceeded limit")

// RunSSHOutputBounded uses the normal trusted transport and target wrappers,
// retaining at most maxBytes of stdout. The caller supplies a deadline. An empty
// non-nil FallbackPorts pins a previously resolved endpoint, as in other helpers.
// Neither remote stdout nor stderr is included in errors.
func RunSSHOutputBounded(ctx context.Context, target SSHTarget, remote string, maxBytes int) (output string, err error) {
	if maxBytes <= 0 {
		return "", fmt.Errorf("SSH output limit must be positive")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	prepared, err := prepareWorkspaceOwnerRemote(ctx, target, remote, nil)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			if cleanupErr := prepared.close(ctx, target); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
		}
	}()
	transport, err := prepareSSHTransport(target, prepared, nil, nil, 0, false, 0)
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := transport.close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port, probe.FallbackPorts = port, []string{}
		out := boundedSSHOutput{limit: maxBytes}
		err := transport.run(ctx, probe, "10", "1", &out, io.Discard)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err == nil {
			if out.exceeded {
				return "", ErrSSHOutputLimit
			}
			return strings.TrimSpace(out.String()), nil
		}
		lastErr = err
		if !shouldRetrySSHPort(err) {
			return "", err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("SSH target has no port")
	}
	return "", lastErr
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
