package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestSSHReadinessDiagnosticMatchesEveryWriteBoundary(t *testing.T) {
	const phrase = "Host key verification failed."
	for split := 0; split <= len(phrase); split++ {
		var diagnostic sshReadinessDiagnostic
		for _, fragment := range []string{"private-diagnostic-data", phrase[:split], phrase[split:], strings.Repeat("x", 128*1024)} {
			if n, err := io.WriteString(&diagnostic, fragment); err != nil || n != len(fragment) {
				t.Fatalf("split=%d: Write=(%d,%v), want full consumption", split, n, err)
			}
		}
		if !diagnostic.hostKeyRejected() {
			t.Fatalf("lost phrase at write boundary %d", split)
		}
	}
	for _, test := range []struct {
		text string
		want bool
	}{
		{text: "Host key verHost key verification failed.", want: true},
		{text: "Host key verification failed"},
		{text: "host key verification failed."},
		{text: "Host key verification denied."},
	} {
		var diagnostic sshReadinessDiagnostic
		for i := range len(test.text) {
			_, _ = diagnostic.Write([]byte{test.text[i]})
		}
		if diagnostic.hostKeyRejected() != test.want {
			t.Fatalf("single-byte detection=%t, want %t for %q", diagnostic.hostKeyRejected(), test.want, test.text)
		}
	}
}

func TestSSHReadinessProbePreservesHostKeyRejectionAcrossDiagnosticWrites(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake SSH fixture")
	}
	const phrase = "Host key verification failed."
	for _, test := range []struct {
		name       string
		diagnostic string
		permanent  bool
	}{
		{name: "split writes", diagnostic: phrase, permanent: true},
		{name: "rejection before overflow", diagnostic: phrase + strings.Repeat("x", 64*1024), permanent: true},
		{name: "rejection after overflow", diagnostic: strings.Repeat("x", 64*1024) + phrase, permanent: true},
		{name: "rejection across old limit", diagnostic: strings.Repeat("x", 64*1024-7) + phrase, permanent: true},
		{name: "overflow without rejection", diagnostic: strings.Repeat("x", 128*1024)},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			diagnostic := filepath.Join(dir, "diagnostic")
			if err := os.WriteFile(diagnostic, []byte(test.diagnostic), 0o600); err != nil {
				t.Fatal(err)
			}
			script := "#!/bin/sh\n/bin/dd if=\"$CRABBOX_READINESS_DIAGNOSTIC\" bs=7 >&2 2>/dev/null\nprintf private-diagnostic-data >&2\nexit 255\n"
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_READINESS_DIAGNOSTIC", diagnostic)
			target := SSHTarget{User: "fixture", Host: "readiness.example", Port: "22", FallbackPorts: []string{}, NoControlMaster: true}
			err := runSSHReadinessProbe(t.Context(), target, "exit 0", "1", "1")
			if got := errors.Is(err, errSSHHostKeyVerification); got != test.permanent || exitCode(err) != 255 {
				t.Fatalf("host-key rejection=%t exit=%d, want rejection=%t exit=255: %v", got, exitCode(err), test.permanent, err)
			}
			if strings.Contains(err.Error(), "private-diagnostic-data") {
				t.Fatal("probe exposed captured diagnostics")
			}
		})
	}
}

func TestSSHReadinessProbeError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell exit-status fixture")
	}
	for _, test := range []struct {
		name       string
		code       int
		diagnostic string
		canceled   bool
		permanent  bool
	}{
		{name: "changed host key", code: 255, diagnostic: "private-key-data\nHost key verification failed.\r\n", permanent: true},
		{name: "authentication pending", code: 255, diagnostic: "Permission denied (publickey)."},
		{name: "connection pending", code: 255, diagnostic: "Connection refused"},
		{name: "toolchain pending", code: 1, diagnostic: "command not found"},
		{name: "nontransport failure", code: 1, diagnostic: "Host key verification failed."},
		{name: "success", diagnostic: "Host key verification failed."},
		{name: "canceled", code: 255, diagnostic: "Host key verification failed.", canceled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.canceled {
				cancel()
			}
			var cause error
			if test.code != 0 {
				cause = exec.Command("sh", "-c", "exit "+strconv.Itoa(test.code)).Run()
				if exitCode(cause) != test.code {
					t.Fatalf("fixture exit status: %v", cause)
				}
			}
			var diagnostic sshReadinessDiagnostic
			_, _ = io.WriteString(&diagnostic, test.diagnostic)
			err := sshReadinessProbeError(ctx, cause, diagnostic.hostKeyRejected())
			if errors.Is(err, errSSHHostKeyVerification) != test.permanent || !errors.Is(err, cause) {
				t.Fatalf("incorrect classification or lost cause: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "private-key-data") {
				t.Fatal("captured diagnostic leaked into error")
			}
			if !test.permanent && err != cause {
				t.Fatalf("ordinary probe result changed: %v", err)
			}
		})
	}
}
