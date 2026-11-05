package main

import (
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// Keep SIGPIPE handling installed through cleanup and final diagnostics. It must
// not share the interrupt handler's stop-on-first-signal lifetime.
type brokenPipeHandler struct {
	broken atomic.Bool
	cancel func()
	stop   func()
}

func newBrokenPipeHandler(cancel func()) *brokenPipeHandler {
	h := &brokenPipeHandler{cancel: cancel}
	h.stop = notifyBrokenPipe(h.fail)
	return h
}

func (h *brokenPipeHandler) fail() {
	h.broken.Store(true)
	h.cancel()
}

func (h *brokenPipeHandler) writer(file *os.File) io.Writer {
	info, err := file.Stat()
	if err != nil || info.Mode()&(os.ModeNamedPipe|os.ModeSocket) == 0 {
		// Preserve terminal detection and direct file inheritance for log files.
		return file
	}
	return &brokenPipeWriter{file: file, fail: h.fail}
}

type brokenPipeWriter struct {
	mu   sync.Mutex
	file *os.File
	fail func()
	err  error
}

func (w *brokenPipeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.file.Write(p)
	if isBrokenPipe(err) {
		// Remember the error synchronously, even if signal delivery lags behind
		// command completion. Cleanup diagnostics never retry the failed pipe.
		w.err = err
		w.fail()
	}
	return n, err
}
