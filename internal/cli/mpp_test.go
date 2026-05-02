package cli

import (
	"strings"
	"testing"
)

func TestParseMppxIncludeOutput(t *testing.T) {
	raw := strings.Join([]string{
		"HTTP/1.1 201 Created",
		"Content-Type: application/json",
		"Payment-Receipt: stub",
		"",
		`{"lease":{"id":"cbx_xx"}}`,
	}, "\r\n")
	status, body, err := parseMppxIncludeOutput([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if status != 201 {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(string(body), "cbx_xx") {
		t.Fatalf("body = %q", body)
	}
}

func TestParseMppxIncludeOutputMissingBlankLine(t *testing.T) {
	if _, _, err := parseMppxIncludeOutput([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nbody-no-blank")); err == nil {
		t.Fatalf("expected error for missing header/body separator")
	}
}

func TestMppxOptIn(t *testing.T) {
	t.Setenv("CRABBOX_MPP_PAY", "")
	if mppxOptIn() {
		t.Fatalf("expected off when env unset")
	}
	t.Setenv("CRABBOX_MPP_PAY", "auto")
	if !mppxOptIn() {
		t.Fatalf("expected on for auto")
	}
}
