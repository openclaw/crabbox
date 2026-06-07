package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestTopLevelHelpListsRegisteredXCPNgProvider(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"--help"})
	if err != nil {
		t.Fatalf("crabbox --help error=%v stderr=%q", err, stderr.String())
	}
	text := stdout.String()
	line := helpLineContaining(text, "CRABBOX_PROVIDER")
	if line == "" {
		t.Fatalf("top-level help omitted CRABBOX_PROVIDER:\n%s", text)
	}
	if !strings.Contains(line, "xcp-ng") {
		t.Fatalf("top-level CRABBOX_PROVIDER help omitted registered xcp-ng provider:\n%s", line)
	}
}

func TestCleanupHelpListsRegisteredXCPNgProvider(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"cleanup", "--help"})
	if err != nil {
		var exitErr ExitError
		if !AsExitError(err, &exitErr) || exitErr.Code != 0 {
			t.Fatalf("crabbox cleanup --help error=%v stderr=%q", err, stderr.String())
		}
	}
	line := helpLineContaining(stderr.String(), "provider:")
	if line == "" {
		t.Fatalf("cleanup help omitted provider flag:\n%s", stderr.String())
	}
	if !strings.Contains(line, "xcp-ng") {
		t.Fatalf("cleanup provider help omitted registered xcp-ng cleanup provider:\n%s", line)
	}
}

func helpLineContaining(text, want string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	return ""
}
