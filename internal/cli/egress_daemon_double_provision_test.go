//go:build !windows

package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMain keeps re-invoked test binaries alive as daemon children instead of
// recursively running the suite.
func TestMain(m *testing.M) {
	if os.Getenv("CRABBOX_EGRESS_DAEMON_TESTCHILD") == "1" {
		time.Sleep(10 * time.Minute)
		os.Exit(egressDaemonFatalCode)
	}
	os.Exit(m.Run())
}

var egressDaemonPIDRe = regexp.MustCompile(`egress host daemon: pid=(\d+)`)

func egressDaemonTestPID(t *testing.T, output *bytes.Buffer) int {
	t.Helper()
	match := egressDaemonPIDRe.FindSubmatch(output.Bytes())
	if match == nil {
		t.Fatalf("egress daemon start did not report a pid (output: %s)", strings.TrimSpace(output.String()))
	}
	pid, err := strconv.Atoi(string(match[1]))
	if err != nil {
		t.Fatalf("parse egress daemon pid: %v", err)
	}
	return pid
}

func egressDaemonTestAlive(pid int) bool {
	command, running := webVNCDaemonProcessCommand(pid)
	return running && !strings.Contains(strings.ToLower(command), "<defunct>")
}

func killEgressDaemonTestGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL) // Setpgid: pgid == supervisor pid
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// TestEgressDaemonConcurrentStartDoubleProvision covers the former
// stop-check-start-write race with real supervisor processes.
func TestEgressDaemonConcurrentStartDoubleProvision(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())          // isolate crabboxStateDir
	t.Setenv("CRABBOX_EGRESS_DAEMON_TESTCHILD", "1") // inherited by spawned supervisors

	const iterations = 20
	for i := 0; i < iterations; i++ {
		leaseID := fmt.Sprintf("-%d", i)
		var outs [2]bytes.Buffer
		var errs [2]error
		release := make(chan struct{})
		var wg sync.WaitGroup
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				app := App{Stdout: &outs[g], Stderr: &outs[g]}
				<-release
				errs[g] = app.startEgressHostDaemon(leaseID, []string{"host", "--id", leaseID, "crabbox-test-supervisor"})
			}(g)
		}
		close(release)
		wg.Wait()

		var pids []int
		for g := 0; g < 2; g++ {
			if errs[g] != nil {
				t.Fatalf("iteration %d: concurrent start %d failed: %v (output: %s)", i, g, errs[g], strings.TrimSpace(outs[g].String()))
			}
			pids = append(pids, egressDaemonTestPID(t, &outs[g]))
		}
		for _, pid := range pids {
			p := pid
			t.Cleanup(func() { killEgressDaemonTestGroup(p) })
		}

		time.Sleep(100 * time.Millisecond)
		var livePids []int
		for _, pid := range pids {
			if egressDaemonTestAlive(pid) {
				livePids = append(livePids, pid)
			}
		}
		_, pidPath, pErr := egressDaemonPaths(leaseID)
		if pErr != nil {
			t.Fatalf("egressDaemonPaths: %v", pErr)
		}
		recorded, readErr := os.ReadFile(pidPath)
		if readErr != nil {
			t.Fatalf("iteration %d: read egress daemon pid: %v", i, readErr)
		}
		recordedPID, parseErr := strconv.Atoi(strings.TrimSpace(string(recorded)))
		if parseErr != nil {
			t.Fatalf("iteration %d: parse recorded egress daemon pid: %v", i, parseErr)
		}
		if len(livePids) != 1 || livePids[0] != recordedPID {
			t.Fatalf("iteration %d: concurrent starts left live supervisors=%v but pid file records %d (start errors: %v / %v)", i, livePids, recordedPID, errs[0], errs[1])
		}

		var stopOutput bytes.Buffer
		stopped, stopErr := (App{Stdout: &stopOutput, Stderr: &stopOutput}).stopEgressHostDaemon(leaseID)
		if stopErr != nil || !stopped {
			t.Fatalf("iteration %d: stop recorded supervisor: stopped=%t err=%v output=%s", i, stopped, stopErr, strings.TrimSpace(stopOutput.String()))
		}
		for _, pid := range pids {
			if egressDaemonTestAlive(pid) {
				t.Fatalf("iteration %d: supervisor pid %d remained alive after stop", i, pid)
			}
		}
		if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
			t.Fatalf("iteration %d: pid file still exists after stop: %v", i, err)
		}
	}
}

func TestEgressDaemonStopUsesLeaseLock(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_EGRESS_DAEMON_TESTCHILD", "1")
	leaseID := "stop-lock"
	var output bytes.Buffer
	app := App{Stdout: &output, Stderr: &output}
	if err := app.startEgressHostDaemon(leaseID, []string{"host", "--id", leaseID, "crabbox-test-supervisor"}); err != nil {
		t.Fatalf("start egress daemon: %v (output: %s)", err, strings.TrimSpace(output.String()))
	}
	pid := egressDaemonTestPID(t, &output)
	t.Cleanup(func() { killEgressDaemonTestGroup(pid) })

	unlock, err := acquireEgressDaemonLock(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	type stopResult struct {
		stopped bool
		err     error
	}
	result := make(chan stopResult, 1)
	go func() {
		stopped, err := app.stopEgressHostDaemon(leaseID)
		result <- stopResult{stopped: stopped, err: err}
	}()

	select {
	case got := <-result:
		t.Fatalf("stop bypassed the held lease lock: stopped=%t err=%v", got.stopped, got.err)
	case <-time.After(100 * time.Millisecond):
	}
	if !egressDaemonTestAlive(pid) {
		t.Fatalf("daemon pid %d stopped while its lease lock was held", pid)
	}
	_, pidPath, err := egressDaemonPaths(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(recorded)); got != strconv.Itoa(pid) {
		t.Fatalf("pid file changed while lease lock was held: got %q want %d", got, pid)
	}

	unlock()
	unlock = nil
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !got.stopped {
			t.Fatal("locked stop reported no daemon")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not complete after lease lock release")
	}
	if egressDaemonTestAlive(pid) {
		t.Fatalf("daemon pid %d remained alive after locked stop", pid)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("pid file still exists after locked stop: %v", err)
	}
}

func TestPrepareEgressClientCutoverPreservesDaemonWhenTicketCreationFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_EGRESS_DAEMON_TESTCHILD", "1")
	leaseID := "ticket-failure"
	var output bytes.Buffer
	app := App{Stdout: &output, Stderr: &output}
	if err := app.startEgressHostDaemon(leaseID, []string{"host", "--id", leaseID, "crabbox-test-supervisor"}); err != nil {
		t.Fatalf("start egress daemon: %v (output: %s)", err, strings.TrimSpace(output.String()))
	}
	pid := egressDaemonTestPID(t, &output)
	t.Cleanup(func() { killEgressDaemonTestGroup(pid) })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/"+leaseID+"/egress/ticket" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		http.Error(w, "temporary coordinator failure", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	coord := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}

	_, err := app.prepareEgressClientCutover(context.Background(), coord, leaseID, "egress_test", "", []string{"example.com"}, true)
	if err == nil {
		t.Fatal("expected ticket creation failure")
	}
	if !egressDaemonTestAlive(pid) {
		t.Fatalf("daemon pid %d stopped before ticket creation succeeded", pid)
	}
	_, pidPath, err := egressDaemonPaths(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(recorded)); got != strconv.Itoa(pid) {
		t.Fatalf("pid file changed after ticket failure: got %q want %d", got, pid)
	}
}

func TestEgressStopHoldsDaemonLockThroughRemoteCleanup(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	marker := filepath.Join(dir, "ssh-started")
	release := filepath.Join(dir, "ssh-release")
	sshScript := `#!/bin/sh
cmd=""
for arg do cmd="$arg"; done
case "$cmd" in
  *egress-client*)
    : > "$CRABBOX_FAKE_SSH_STARTED"
    while [ ! -e "$CRABBOX_FAKE_SSH_RELEASE" ]; do /bin/sleep 0.01; done
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(sshScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("CRABBOX_FAKE_SSH_STARTED", marker)
	t.Setenv("CRABBOX_FAKE_SSH_RELEASE", release)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")

	var stdout, stderr bytes.Buffer
	stopDone := make(chan error, 1)
	go func() {
		stopDone <- (App{Stdout: &stdout, Stderr: &stderr}).egressStop(context.Background(), []string{
			"--provider", "run-env-profile-test",
			"--id", "friendly-slug",
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = os.WriteFile(release, nil, 0o600)
			stopErr := <-stopDone
			t.Fatalf("egress stop did not reach remote cleanup: %v\nstdout=%s\nstderr=%s", stopErr, stdout.String(), stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	lockResult := make(chan error, 1)
	lockRelease := make(chan struct{})
	go func() {
		unlock, err := acquireEgressDaemonLock("cbx_env_profile_test")
		lockResult <- err
		if err == nil {
			<-lockRelease
			unlock()
		}
	}()
	select {
	case err := <-lockResult:
		close(lockRelease)
		_ = os.WriteFile(release, nil, 0o600)
		t.Fatalf("remote cleanup released the daemon lock early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("egress stop: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("egress stop did not finish after remote cleanup")
	}
	select {
	case err := <-lockResult:
		if err != nil {
			t.Fatal(err)
		}
		close(lockRelease)
	case <-time.After(2 * time.Second):
		t.Fatal("daemon lock did not release after remote cleanup")
	}
}
