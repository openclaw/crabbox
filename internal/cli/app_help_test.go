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

func TestTopLevelAndCommandHelpDescribeInteractiveConnect(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"--help"}); err != nil {
		t.Fatalf("crabbox --help error=%v", err)
	}
	if !strings.Contains(stdout.String(), "connect     Open an interactive SSH session to a lease") {
		t.Fatalf("top-level help omitted connect:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	err := app.Run(context.Background(), []string{"connect", "--help"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 0 {
		t.Fatalf("crabbox connect --help error=%v stderr=%q", err, stderr.String())
	}
	help := stderr.String()
	if !strings.Contains(help, "-id string") || !strings.Contains(help, "-network string") {
		t.Fatalf("connect help omitted lease flags:\n%s", help)
	}
	if strings.Contains(help, "show-secret") {
		t.Fatalf("connect help exposed print-only show-secret flag:\n%s", help)
	}
}

func TestCheckpointCreateHelpListsAzureSnapshotSKU(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"checkpoint", "create", "--help"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 0 {
		t.Fatalf("crabbox checkpoint create --help error=%v stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "-azure-snapshot-sku string") {
		t.Fatalf("checkpoint create help omitted Azure snapshot SKU:\n%s", stderr.String())
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
