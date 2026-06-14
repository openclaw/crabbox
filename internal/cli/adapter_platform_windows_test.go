//go:build windows

package cli

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestAdapterCommandsRejectWindowsAtCommandBoundary(t *testing.T) {
	app := App{Stdout: io.Discard, Stderr: io.Discard, Stdin: strings.NewReader("")}
	if err := app.controllerServe(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "adapter serve is not supported on Windows") {
		t.Fatalf("serve support error=%v", err)
	}
	if err := app.adapterConnect(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "adapter connect is not supported on Windows") {
		t.Fatalf("connect support error=%v", err)
	}
}
