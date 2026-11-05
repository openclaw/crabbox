//go:build windows

package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsSFTPGuestErrorsArePlainText(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.bin")
	command := strings.Fields(windowsSFTPGuestCommand(windowsSFTPMetadataScript(missing)))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := runWindowsSFTPProcess(ctx, SSHTarget{}, "powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", command[len(command)-1])
	if err == nil {
		t.Fatalf("metadata for a missing file succeeded: %q", out)
	}
	if message := err.Error(); strings.Contains(message, "CLIXML") || !strings.Contains(message, "Could not find file") {
		t.Fatalf("guest failure diagnostic = %q, want the plain exception message", message)
	}
}
