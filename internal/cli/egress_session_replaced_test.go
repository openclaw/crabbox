package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestEgressHostReplacementCloseIsFatal(t *testing.T) {
	server := egressClosingCoordinator(
		t,
		"host",
		websocket.StatusServiceRestart,
		"replaced by a newer egress session",
	)
	defer server.Close()

	err := runEgressHostAgainstCoordinator(t, server.URL)
	assertEgressExitCode(t, err, egressDaemonFatalCode)
}

func TestEgressClientReplacementCloseIsFatal(t *testing.T) {
	server := egressClosingCoordinator(
		t,
		"client",
		websocket.StatusServiceRestart,
		"replaced by a newer egress client",
	)
	defer server.Close()

	err := runEgressClientAgainstCoordinator(t, server.URL)
	assertEgressExitCode(t, err, egressDaemonFatalCode)
}

func TestEgressOtherServeLoopClosesAreNotFatal(t *testing.T) {
	tests := []struct {
		name   string
		code   websocket.StatusCode
		reason string
	}{
		{
			name:   "different code",
			code:   websocket.StatusInternalError,
			reason: "replaced by a newer egress session",
		},
		{
			name:   "different reason",
			code:   websocket.StatusServiceRestart,
			reason: "coordinator maintenance",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := egressClosingCoordinator(t, "host", tt.code, tt.reason)
			defer server.Close()

			err := runEgressHostAgainstCoordinator(t, server.URL)
			if err == nil {
				t.Fatal("expected serve-loop close error")
			}
			var exitErr ExitError
			if errors.As(err, &exitErr) && exitErr.Code == egressDaemonFatalCode {
				t.Fatalf("close unexpectedly mapped to fatal exit: %v", err)
			}
		})
	}
}

func egressClosingCoordinator(
	t *testing.T,
	role string,
	code websocket.StatusCode,
	reason string,
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/v1/leases/cbx_abcdef123456/egress/" + role
		if r.URL.Path != wantPath {
			http.NotFound(w, r)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("websocket accept: %v", err)
			return
		}
		_ = conn.Close(code, reason)
	}))
}

func runEgressHostAgainstCoordinator(t *testing.T, coordinatorURL string) error {
	t.Helper()
	clearConfigEnv(t)
	isolateRunTestUserDirs(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return (App{Stdout: io.Discard, Stderr: io.Discard}).egressHost(ctx, []string{
		"--id", "cbx_abcdef123456",
		"--coordinator", coordinatorURL,
		"--ticket", "egress_abcdef1234567890abcdef1234567890",
		"--session", "egress_session",
		"--allow", "example.com",
	})
}

func runEgressClientAgainstCoordinator(t *testing.T, coordinatorURL string) error {
	t.Helper()
	clearConfigEnv(t)
	isolateRunTestUserDirs(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return (App{Stdout: io.Discard, Stderr: io.Discard}).egressClient(ctx, []string{
		"--id", "cbx_abcdef123456",
		"--coordinator", coordinatorURL,
		"--ticket", "egress_abcdef1234567890abcdef1234567890",
		"--session", "egress_session",
		"--listen", "127.0.0.1:0",
	})
}

func assertEgressExitCode(t *testing.T, err error, want int) {
	t.Helper()
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != want {
		t.Fatalf("exit error=%v want code %d", err, want)
	}
}
