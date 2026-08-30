package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
)

const workspaceOwnerSetupPrefix = "CRABBOX_OWNER_SETUP_V1 "

type workspaceOwnerSetupError struct {
	phase string
	cause error
}

func (e *workspaceOwnerSetupError) Error() string {
	return fmt.Sprintf("remote workspace owner setup failed (%s); workload was not started; check remote process-observation permissions and workspace-owner state", e.phase)
}

func (e *workspaceOwnerSetupError) Unwrap() error { return e.cause }

func workspaceOwnerReadinessError(err error, phase string) error {
	var setupErr *workspaceOwnerSetupError
	if errors.As(err, &setupErr) {
		return fmt.Errorf("%s: %w", phase, err)
	}
	return nil
}

// Attach only to the command execution, never staging, route probes, or cleanup.
// The returned finisher runs after both streams have stopped.
func workspaceOwnerSetupStreams(marker string, stdout, stderr io.Writer) (io.Writer, io.Writer, func(error) error) {
	if marker == "" {
		return stdout, stderr, func(err error) error { return err }
	}
	if stderr == nil {
		stderr = io.Discard
	}
	diagnostic := &workspaceOwnerSetupWriter{destination: stderr, marker: marker}
	if sameCommandStreamWriter(stdout, stderr) {
		stdout = workspaceOwnerSetupStdout{setup: diagnostic}
	}
	return stdout, diagnostic, func(err error) error {
		err = errors.Join(err, diagnostic.flush())
		if diagnostic.failure != "" {
			return &workspaceOwnerSetupError{phase: diagnostic.failure, cause: err}
		}
		return err
	}
}

// Only the current witness can report setup failure, and only before handing
// off to user code. Do not classify a workload's exit code or stderr as setup.
type workspaceOwnerSetupWriter struct {
	mu          sync.Mutex
	destination io.Writer
	marker      string
	pending     []byte
	passthrough bool
	started     bool
	failure     string
}

// Separating stderr for protocol filtering must retain os/exec's guarantee
// that a shared stdout/stderr writer is never called concurrently.
type workspaceOwnerSetupStdout struct{ setup *workspaceOwnerSetupWriter }

func (w workspaceOwnerSetupStdout) Write(data []byte) (int, error) {
	w.setup.mu.Lock()
	defer w.setup.mu.Unlock()
	return w.setup.destination.Write(data)
}

func (w *workspaceOwnerSetupWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	length := len(data)
	for len(data) > 0 {
		if w.started {
			n, err := w.destination.Write(data)
			return length - len(data) + n, err
		}
		if w.passthrough {
			count := len(data)
			if end := bytes.IndexByte(data, '\n'); end >= 0 {
				count = end + 1
				w.passthrough = false
			}
			n, err := w.destination.Write(data[:count])
			if err == nil && n != count {
				err = io.ErrShortWrite
			}
			if err != nil {
				return length - len(data) + n, err
			}
			data = data[count:]
			continue
		}
		b := data[0]
		data = data[1:]
		w.pending = append(w.pending, b)
		if b == '\n' {
			line := string(bytes.TrimSuffix(w.pending, []byte{'\n'}))
			matched := false
			if line == w.marker+" started" {
				w.started, matched = true, true
			} else {
				for _, phase := range []string{"staging", "input", "identity", "registration", "handoff"} {
					if line == w.marker+" failed "+phase {
						if w.failure == "" {
							w.failure = phase
						}
						matched = true
						break
					}
				}
			}
			if matched {
				w.pending = w.pending[:0]
			} else if err := w.flush(); err != nil {
				return length - len(data), err
			}
		} else if len(w.pending) >= 160 {
			if err := w.flush(); err != nil {
				return length - len(data), err
			}
			w.passthrough = true
		}
	}
	return length, nil
}

func (w *workspaceOwnerSetupWriter) flush() error {
	if len(w.pending) == 0 {
		return nil
	}
	n, err := w.destination.Write(w.pending)
	if err == nil && n != len(w.pending) {
		err = io.ErrShortWrite
	}
	w.pending = w.pending[:0]
	return err
}
