package daytona

import (
	"net/http"
	"testing"
	"time"
)

func TestTransferAwareHTTPClientHasNoOverallTimeout(t *testing.T) {
	c := transferAwareHTTPClient()
	if c.Timeout != 0 {
		t.Fatalf("overall Timeout = %v, want 0 for streaming archive uploads", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type %T, want *http.Transport", c.Transport)
	}
	if tr.ResponseHeaderTimeout != 60*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %v, want 60s", tr.ResponseHeaderTimeout)
	}
	if tr.TLSHandshakeTimeout != 10*time.Second {
		t.Fatalf("TLSHandshakeTimeout = %v, want 10s", tr.TLSHandshakeTimeout)
	}
}
