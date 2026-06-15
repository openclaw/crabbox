package cli

import (
	"fmt"
	"io"
	"os"
)

const failureBundleStreamCaptureBytes int64 = 16 * 1024 * 1024

type failureStreamCaptures struct {
	stdout *failureStreamCapture
	stderr *failureStreamCapture
}

type failureStreamCapture struct {
	label         string
	explicitPath  string
	bundleFile    io.WriteCloser
	bundlePath    string
	bundleCleanup func()
	capture       *countingWriteCloser
}

func openFailureStreamCaptures(stdoutPath, stderrPath string) (*failureStreamCaptures, error) {
	stdout, err := openFailureStreamCapture("stdout", stdoutPath)
	if err != nil {
		return nil, err
	}
	stderr, err := openFailureStreamCapture("stderr", stderrPath)
	if err != nil {
		stdout.cleanup()
		return nil, err
	}
	return &failureStreamCaptures{stdout: stdout, stderr: stderr}, nil
}

func openFailureStreamCapture(label, explicitPath string) (*failureStreamCapture, error) {
	file, path, cleanup, err := openFailureStreamBundleFile(label, explicitPath)
	if err != nil {
		return nil, err
	}
	var bundleFile io.WriteCloser
	if file != nil {
		bundleFile = NewCappedFailureBundleStream(file)
	}
	return &failureStreamCapture{
		label:         label,
		explicitPath:  explicitPath,
		bundleFile:    bundleFile,
		bundlePath:    path,
		bundleCleanup: cleanup,
	}, nil
}

func (c *failureStreamCaptures) cleanup() {
	if c == nil {
		return
	}
	c.stdout.cleanup()
	c.stderr.cleanup()
}

func (c *failureStreamCapture) cleanup() {
	if c == nil || c.bundleCleanup == nil {
		return
	}
	c.bundleCleanup()
}

func (c *failureStreamCapture) writer(base io.Writer, phase *phaseMarkerWriter, status io.Writer) (io.Writer, bool, error) {
	if c == nil {
		return base, false, nil
	}
	if c.explicitPath != "" {
		file, err := openPrivateRunOutputFile(c.explicitPath)
		if err != nil {
			return nil, false, exit(2, "capture %s: %v", c.label, err)
		}
		c.capture = &countingWriteCloser{WriteCloser: file}
		fmt.Fprintf(status, "capturing %s to %s\n", c.label, c.explicitPath)
		return io.MultiWriter(c.capture, phase), true, nil
	}
	if c.bundleFile != nil {
		return io.MultiWriter(base, c.bundleFile), false, nil
	}
	return base, false, nil
}

func (c *failureStreamCaptures) closeAfterStream(streamErr error, code int, status io.Writer) error {
	if c == nil {
		return nil
	}
	if err := c.stdout.closeAfterStream(streamErr, code, status); err != nil {
		_ = c.stderr.closeQuiet()
		return err
	}
	return c.stderr.closeAfterStream(streamErr, code, status)
}

func (c *failureStreamCapture) closeAfterStream(streamErr error, code int, status io.Writer) error {
	if c == nil {
		return nil
	}
	if c.capture != nil {
		if streamErr != nil && !isSSHCommandExitError(streamErr) {
			_ = c.capture.Close()
			return exit(2, "capture %s: %v", c.label, streamErr)
		}
		if err := c.capture.Close(); err != nil && code == 0 {
			return exit(2, "capture %s close: %v", c.label, err)
		}
		fmt.Fprintf(status, "captured %s=%s bytes=%d\n", c.label, c.explicitPath, c.capture.N)
		c.capture = nil
		return nil
	}
	if c.bundleFile != nil {
		if err := c.bundleFile.Close(); err != nil && code == 0 {
			return exit(2, "failure bundle %s close: %v", c.label, err)
		}
		c.bundleFile = nil
	}
	return nil
}

func (c *failureStreamCapture) closeQuiet() error {
	if c == nil {
		return nil
	}
	if c.capture != nil {
		err := c.capture.Close()
		c.capture = nil
		return err
	}
	if c.bundleFile != nil {
		err := c.bundleFile.Close()
		c.bundleFile = nil
		return err
	}
	return nil
}

func (c *failureStreamCapture) path() string {
	if c == nil {
		return ""
	}
	return firstNonBlank(c.explicitPath, c.bundlePath)
}

type cappedFailureBundleStream struct {
	file      *os.File
	remain    int64
	truncated bool
}

func NewCappedFailureBundleStream(file *os.File) io.WriteCloser {
	return &cappedFailureBundleStream{file: file, remain: failureBundleStreamCaptureBytes}
}

func (w *cappedFailureBundleStream) Write(p []byte) (int, error) {
	if w == nil || w.file == nil {
		return len(p), nil
	}
	truncatedThisWrite := false
	if w.remain > 0 {
		limit := int64(len(p))
		if limit > w.remain {
			limit = w.remain
			truncatedThisWrite = true
		}
		if limit > 0 {
			n, err := w.file.Write(p[:limit])
			w.remain -= int64(n)
			if err != nil || int64(n) != limit {
				w.remain = 0
				w.truncated = true
				return len(p), nil
			}
		}
	} else if len(p) > 0 {
		truncatedThisWrite = true
	}
	if truncatedThisWrite && !w.truncated {
		w.truncated = true
		_, _ = fmt.Fprintf(w.file, "\n[crabbox: failure-bundle stream truncated after %d bytes]\n", failureBundleStreamCaptureBytes)
	}
	return len(p), nil
}

func (w *cappedFailureBundleStream) Close() error {
	if w == nil || w.file == nil {
		return nil
	}
	_ = w.file.Close()
	return nil
}
