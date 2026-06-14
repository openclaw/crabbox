//go:build !darwin && !linux

package cli

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestWebVNCLocalFailsClosedOnUnsupportedPlatformBeforeReadingPassword(t *testing.T) {
	if localWebVNCSupported() {
		t.Fatal("local WebVNC unexpectedly supported")
	}
	err := (App{Stdout: io.Discard, Stderr: io.Discard, Stdin: unsupportedWebVNCStdin{t: t}}).webVNCLocal(context.Background(), []string{
		"--vnc-host", "127.0.0.1",
		"--vnc-port", "5900",
		"--username", "admin",
		"--password-stdin",
	})
	if err == nil || !strings.Contains(err.Error(), "supported only on macOS and Linux") {
		t.Fatalf("error=%v", err)
	}
}

type unsupportedWebVNCStdin struct {
	t *testing.T
}

func (r unsupportedWebVNCStdin) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Fatal("unsupported local WebVNC read the password")
	return 0, io.EOF
}
