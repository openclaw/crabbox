package wandb

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sandboxv1 "github.com/openclaw/crabbox/internal/providers/wandb/gen/coreweave/sandbox/v1beta2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNormalizeStatus(t *testing.T) {
	for _, tc := range []struct {
		in   sandboxv1.SandboxStatus
		want string
	}{
		{sandboxv1.SandboxStatus_SANDBOX_STATUS_RUNNING, "running"},
		{sandboxv1.SandboxStatus_SANDBOX_STATUS_COMPLETED, "completed"},
		{sandboxv1.SandboxStatus_SANDBOX_STATUS_FAILED, "failed"},
	} {
		if got := normalizeStatus(tc.in); got != tc.want {
			t.Fatalf("normalizeStatus(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMapRPCErrorExitCodes(t *testing.T) {
	for _, tc := range []struct {
		code codes.Code
		want int
	}{
		{codes.Unauthenticated, 77},
		{codes.PermissionDenied, 77},
		{codes.Unavailable, 69},
		{codes.ResourceExhausted, 69},
		{codes.DeadlineExceeded, 124},
		{codes.NotFound, 4},
	} {
		err := mapRPCError(status.Error(tc.code, "boom"), "op")
		var apiErr *wandbAPIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("code=%v: err = %T, want *wandbAPIError", tc.code, err)
		}
		if apiErr.ExitCode != tc.want {
			t.Fatalf("code=%v: exit = %d, want %d", tc.code, apiErr.ExitCode, tc.want)
		}
	}
}

func TestWandbAPIErrorAsExitError(t *testing.T) {
	err := &wandbAPIError{ExitCode: 77, Stderr: "auth failed", Code: codes.Unauthenticated}
	var ee ExitError
	if !errors.As(err, &ee) {
		t.Fatal("errors.As failed for *wandbAPIError -> ExitError")
	}
	if ee.Code != 77 || !strings.Contains(ee.Message, "auth failed") {
		t.Fatalf("ExitError = %#v", ee)
	}
}

func TestResolveAuthPrecedence(t *testing.T) {
	home := t.TempDir()
	netrc := "machine api.wandb.ai login user password netrc-key\n"
	if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte(netrc), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CRABBOX_WANDB_API_KEY", "crabbox-key")
	t.Setenv("WANDB_API_KEY", "wandb-key")

	auth, err := resolveAuth(Config{Wandb: WandbConfig{APIKey: "cfg-key"}})
	if err != nil || auth.APIKey != "crabbox-key" {
		t.Fatalf("CRABBOX precedence: auth=%#v err=%v", auth, err)
	}

	t.Setenv("CRABBOX_WANDB_API_KEY", "")
	auth, err = resolveAuth(Config{Wandb: WandbConfig{APIKey: "cfg-key"}})
	if err != nil || auth.APIKey != "cfg-key" {
		t.Fatalf("cfg precedence: auth=%#v err=%v", auth, err)
	}

	auth, err = resolveAuth(Config{})
	if err != nil || auth.APIKey != "wandb-key" {
		t.Fatalf("WANDB precedence: auth=%#v err=%v", auth, err)
	}

	t.Setenv("WANDB_API_KEY", "")
	auth, err = resolveAuth(Config{})
	if err != nil || auth.APIKey != "netrc-key" {
		t.Fatalf("netrc precedence: auth=%#v err=%v", auth, err)
	}
}

func TestReadNetrcWandbKeyHosts(t *testing.T) {
	for _, host := range []string{"api.wandb.ai", "api.wandb.com"} {
		t.Run(host, func(t *testing.T) {
			home := t.TempDir()
			netrc := "machine " + host + " login user password host-key\n"
			if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte(netrc), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			if got := readNetrcWandbKey(); got != "host-key" {
				t.Fatalf("readNetrcWandbKey() = %q, want host-key", got)
			}
		})
	}
}

func TestStripScheme(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"api.cwsandbox.com:443", "api.cwsandbox.com:443"},
		{"https://api.cwsandbox.com:443", "api.cwsandbox.com:443"},
		{"http://api.cwsandbox.com:443/", "api.cwsandbox.com:443"},
	} {
		if got := stripScheme(tc.in); got != tc.want {
			t.Fatalf("stripScheme(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveAuthMissingKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRABBOX_WANDB_API_KEY", "")
	t.Setenv("WANDB_API_KEY", "")
	_, err := resolveAuth(Config{})
	if err == nil || !strings.Contains(err.Error(), "W&B API key") {
		t.Fatalf("err = %v, want missing-key error", err)
	}
}
