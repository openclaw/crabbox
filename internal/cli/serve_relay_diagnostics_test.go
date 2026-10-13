package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestServeRelayDiagnosticsBoundedAndRedacted(t *testing.T) {
	t.Setenv("CRABBOX_SERVE_RELAY_DEBUG", "1")
	var output bytes.Buffer
	d := newServeRelayDiagnostics(strings.Repeat("a", 32), "client", &output)
	for range 1000 {
		d.event("send", 32768)
		d.event("ack-received", 0)
		d.event("receive", 123)
		d.event("ack-sent", 0)
	}
	d.closed("socket-read", errors.New("private-payload-and-token"))
	if lines := strings.Count(output.String(), "\n"); lines != 17 {
		t.Fatalf("diagnostics emitted %d lines, want 16 events and close", lines)
	}
	for _, want := range []string{"session=" + strings.Repeat("a", 32), "role=client", "event=close:socket-read", "sent=32768000", "received=123000", "ack_sent=1000", "ack_received=1000", "pending=0", "window=32768", "error=transport-error"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(output.String(), "private-payload-and-token") {
		t.Fatal("logged error contents")
	}
}

func TestServeRelayDiagnosticsOptIn(t *testing.T) {
	t.Setenv("CRABBOX_SERVE_RELAY_DEBUG", "")
	var output bytes.Buffer
	d := newServeRelayDiagnostics(strings.Repeat("a", 32), "agent", &output)
	d.event("send", 123)
	d.closed("local-close", nil)
	if output.Len() != 0 {
		t.Fatal("diagnostics enabled by default")
	}
}

func TestServeRelayDiagnosticForwarder(t *testing.T) {
	var source, output bytes.Buffer
	t.Setenv("CRABBOX_SERVE_RELAY_DEBUG", "1")
	d := newServeRelayDiagnostics(strings.Repeat("a", 32), "client", &source)
	d.event("paired", 0)
	f := &serveRelayDiagnosticForwarder{out: &output}
	_, _ = f.Write([]byte("secret remote stderr\n" + strings.Repeat("x", 1000) + "\n"))
	for _, b := range source.Bytes() {
		_, _ = f.Write([]byte{b})
	}
	_, _ = f.Write([]byte(strings.ReplaceAll(source.String(), "error=none", "error=secret contents")))
	if output.String() != source.String() {
		t.Fatal("forwarded unvalidated output or lost split metadata line")
	}
	for range 100 {
		_, _ = f.Write(source.Bytes())
	}
	if strings.Count(output.String(), "\n") != 64 {
		t.Fatal("forwarded unbounded diagnostics")
	}
}
