package localcontainer

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const endpointProbeTestAddress = "CRABBOX_TEST_LOCAL_CONTAINER_ENDPOINT_ADDR"

func runEndpointProbeTestProcess(address string) int {
	if len(os.Args) != 3 || os.Args[1] != "inspect" || os.Args[2] != "endpoint-container" {
		return 92
	}
	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		return 93
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "R"); err != nil {
		return 94
	}
	_, _ = io.Copy(io.Discard, conn)
	return 0
}

// This controlled runtime process exercises the production command runner,
// not an installed Docker CLI or daemon.
func TestWaitForContainerEndpointTimeoutJoinsRealProcess(t *testing.T) {
	testReadinessInspectionTimeoutJoinsRealProcess(t, false)
}

func TestExactContainerReadinessTimeoutJoinsRealProcess(t *testing.T) {
	testReadinessInspectionTimeoutJoinsRealProcess(t, true)
}

func testReadinessInspectionTimeoutJoinsRealProcess(t *testing.T, exactReadiness bool) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv(endpointProbeTestAddress, listener.Addr().String())
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b := testBackend(&recordingRunner{})
	b.cfg.LocalContainer.Runtime = executable
	b.rt.Exec = core.RuntimeForProviderOperation(io.Discard).Exec
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	var lease core.LeaseTarget
	var waitErr error
	var started time.Time
	done := make(chan struct{})
	go func() {
		started = time.Now()
		if exactReadiness {
			lease = core.LeaseTarget{LeaseID: "cbx_endpoint", Server: core.Server{CloudID: "endpoint-container"}}
			waitErr = b.waitForExactContainerSSHReady(ctx, &lease, time.Minute)
		} else {
			lease, waitErr = b.waitForContainerEndpoint(ctx, b.configForRun(), "endpoint-container", "cbx_endpoint", "endpoint")
		}
		close(done)
	}()
	var conn net.Conn
	defer func() {
		cancel()
		if conn != nil {
			_ = conn.Close()
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("endpoint inspection did not join during cleanup")
		}
	}()
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	conn, err = listener.Accept()
	if err != nil {
		t.Fatalf("inspection process did not connect: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var marker [1]byte
	if _, err := io.ReadFull(conn, marker[:]); err != nil || marker[0] != 'R' {
		t.Fatalf("inspection process did not announce readiness: %v", err)
	}
	select {
	case <-done:
	case <-time.After(40 * time.Second):
		t.Fatal("inspection did not return within its readiness budget")
	}
	elapsed := time.Since(started)
	if exactReadiness {
		if waitErr == nil || !strings.HasPrefix(waitErr.Error(), "container inspect failed:") || !strings.Contains(waitErr.Error(), "context deadline exceeded") {
			t.Fatalf("unexpected exact-container inspection timeout: %v", waitErr)
		}
	} else {
		var exit core.ExitError
		if !core.AsExitError(waitErr, &exit) || exit.Code != 5 || !errors.Is(waitErr, context.DeadlineExceeded) || !strings.HasPrefix(waitErr.Error(), "timed out waiting for SSH port on local-container endpoint-con: container inspect failed:") {
			t.Fatalf("unexpected endpoint timeout: %v", waitErr)
		}
	}
	wantLease := core.LeaseTarget{LeaseID: "cbx_endpoint", Server: core.Server{CloudID: "endpoint-container"}}
	if !reflect.DeepEqual(lease, wantLease) {
		t.Fatalf("recovery identity changed: %+v", lease)
	}
	if elapsed < 29*time.Second || elapsed > 40*time.Second {
		t.Fatalf("endpoint budget elapsed=%s, want approximately 30 seconds", elapsed)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(marker[:]); err != io.EOF {
		t.Fatalf("inspection process control socket remained open: %v", err)
	}
	t.Logf("production runner joined the stalled inspection after %s; control socket closed and recovery identity retained", elapsed)
}

func TestRecordedCommandFailureOmitsEnvironmentAndInput(t *testing.T) {
	const child = "CRABBOX_TEST_RECORDED_COMMAND_FAILURE"
	const envValue = "synthetic-environment-value"
	const inputValue = "synthetic-input-value"
	if os.Getenv(child) == "1" {
		runner := &recordingRunner{}
		_, _ = runner.Run(t.Context(), core.LocalCommandRequest{
			Name:  "docker",
			Args:  []string{"inspect", "fixture-container"},
			Env:   []string{"TEST_DIAGNOSTIC_VALUE=" + envValue},
			Stdin: strings.NewReader(inputValue),
		})
		_ = runner.commandSummary()
		input, err := io.ReadAll(runner.calls[0].Stdin)
		if err != nil || string(input) != inputValue || runner.calls[0].Env[0] != "TEST_DIAGNOSTIC_VALUE="+envValue {
			t.Fatal("diagnostic summary altered the captured request")
		}
		recordedArgsForCommand(t, runner, "rm")
		return
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestRecordedCommandFailureOmitsEnvironmentAndInput$", "-test.count=1")
	home := t.TempDir()
	// A failing recorder must never receive the test runner's real credentials.
	cmd.Env = []string{
		child + "=1", "HOME=" + home, "USERPROFILE=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"APPDATA=" + filepath.Join(home, "appdata"),
		"LOCALAPPDATA=" + filepath.Join(home, "localappdata"),
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("diagnostic subprocess did not finish: %v", ctx.Err())
	}
	if err == nil {
		t.Fatal("missing-command assertion unexpectedly succeeded")
	}
	for _, want := range []string{"rm command was not recorded", "docker", "inspect", "fixture-container"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("failure omitted command context %q: %s", want, out)
		}
	}
	for _, forbidden := range []string{envValue, inputValue} {
		if strings.Contains(string(out), forbidden) {
			t.Errorf("failure exposed synthetic request value %q: %s", forbidden, out)
		}
	}
}
