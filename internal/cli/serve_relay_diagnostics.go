package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

var serveRelayDiagnosticLine = regexp.MustCompile(`^serve relay session=[a-f0-9]{32} role=(client|agent) event=(dial|paired|send|receive|ack-sent|ack-received|close:(local-close|dial|pairing|socket-read|empty-data|receive-window-overflow|invalid-ack|ack-window-overflow|invalid-frame|stream-write|ack-write|stream-read|data-write|context)) elapsed=[0-9.hmsµ]+ sent=[0-9]+ received=[0-9]+ ack_sent=[0-9]+ ack_received=[0-9]+ pending=[0-9]+ window=32768 error=(none|canceled|deadline|eof|closed|timeout|transport-error|websocket-[0-9]+)$`)

// Readiness deliberately discards remote stderr. Forward only the bounded
// relay metadata grammar when diagnostics are requested, never arbitrary lines.
type serveRelayDiagnosticForwarder struct {
	mu       sync.Mutex
	out      io.Writer
	line     []byte
	dropping bool
	lines    int
}

func (f *serveRelayDiagnosticForwarder) Write(data []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range data {
		if b == '\n' {
			if !f.dropping && f.lines < 64 && serveRelayDiagnosticLine.Match(f.line) {
				_, _ = fmt.Fprintln(f.out, string(f.line))
				f.lines++
			}
			f.line, f.dropping = f.line[:0], false
		} else if len(f.line) < 512 && !f.dropping {
			f.line = append(f.line, b)
		} else {
			f.dropping = true
		}
	}
	return len(data), nil
}

// Only protocol metadata is logged, never payloads, URLs, credentials, or peer
// close text. Bound volume to the first 16 events, then one per five seconds.
type serveRelayDiagnostics struct {
	mu                            sync.Mutex
	out                           io.Writer
	session, role                 string
	start, last                   time.Time
	events                        int
	sent, received                int64
	ackSent, ackReceived, pending int64
}

func newServeRelayDiagnostics(session, role string, output ...io.Writer) *serveRelayDiagnostics {
	d := &serveRelayDiagnostics{session: session, role: role, start: time.Now()}
	if os.Getenv("CRABBOX_SERVE_RELAY_DEBUG") == "1" {
		d.out = os.Stderr
		if len(output) > 0 && output[0] != nil {
			d.out = output[0]
		}
	}
	return d
}

func (d *serveRelayDiagnostics) event(event string, n int) {
	if d == nil || d.out == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	switch event {
	case "send":
		d.sent += int64(n)
		d.pending = int64(n)
	case "receive":
		d.received += int64(n)
	case "ack-sent":
		d.ackSent++
	case "ack-received":
		d.ackReceived++
		d.pending = 0
	}
	d.events++
	if d.events <= 16 || time.Since(d.last) >= 5*time.Second {
		d.write(event, "none")
	}
}

func (d *serveRelayDiagnostics) closed(reason string, err error) {
	if d == nil || d.out == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	kind := "none"
	switch {
	case errors.Is(err, context.Canceled):
		kind = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		kind = "deadline"
	case errors.Is(err, io.EOF):
		kind = "eof"
	case errors.Is(err, net.ErrClosed), errors.Is(err, io.ErrClosedPipe):
		kind = "closed"
	case websocket.CloseStatus(err) != -1:
		kind = fmt.Sprintf("websocket-%d", websocket.CloseStatus(err))
	case err != nil:
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			kind = "timeout"
		} else {
			kind = "transport-error"
		}
	}
	d.write("close:"+reason, kind)
}

func (d *serveRelayDiagnostics) write(event, kind string) {
	d.last = time.Now()
	fmt.Fprintf(d.out, "serve relay session=%s role=%s event=%s elapsed=%s sent=%d received=%d ack_sent=%d ack_received=%d pending=%d window=%d error=%s\n", d.session, d.role, event, time.Since(d.start).Round(time.Millisecond), d.sent, d.received, d.ackSent, d.ackReceived, d.pending, serveRelayChunk, kind)
}
