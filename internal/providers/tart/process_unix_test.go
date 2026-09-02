//go:build !windows

package tart

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

const startupFailure = "The number of VMs exceeds the system limit"

// The helper is the actual foreground child, not a shell with an unowned sleep
// process. Every command is routed through a PATH containing ONLY these helpers.
// A private loopback connection provides barriers and closes on test cleanup.
func TestTartProcessHelper(t *testing.T) {
	if os.Getenv("CRABBOX_TART_TEST_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 3 {
		os.Exit(90)
	}
	args = args[1:]
	conn, err := net.DialTimeout("tcp", os.Getenv("CRABBOX_TART_TEST_ADDRESS"), 5*time.Second)
	if err != nil {
		os.Exit(91)
	}
	stdout, _ := os.Stdout.Stat()
	stderr, _ := os.Stderr.Stat()
	enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
	if enc.Encode(processHello{Args: args, StdoutMode: stdout.Mode(), StderrMode: stderr.Mode()}) != nil {
		os.Exit(92)
	}
	for {
		var action processAction
		if dec.Decode(&action) != nil {
			os.Exit(93)
		}
		if action.Stdout != "" {
			_, _ = io.WriteString(os.Stdout, action.Stdout)
		}
		for range max(1, action.Repeat) {
			if action.Stderr != "" {
				_, _ = io.WriteString(os.Stderr, action.Stderr)
			}
		}
		if action.Exit {
			if action.Code == 0 && args[0] == "ssh-keygen" {
				// Synthetic key material is sufficient: only our fake SSH runs.
				path := args[len(args)-1]
				if os.WriteFile(path, []byte("synthetic test private key"), 0o600) != nil || os.WriteFile(path+".pub", []byte("ssh-ed25519 AAAA test\n"), 0o600) != nil {
					os.Exit(97)
				}
			}
			if action.Code == 0 && args[0] == "tart" {
				switch args[1] {
				case "clone":
					if os.MkdirAll(filepath.Join(os.Getenv("TART_HOME"), "vms", args[3]), 0o700) != nil {
						os.Exit(94)
					}
				case "delete":
					if os.RemoveAll(filepath.Join(os.Getenv("TART_HOME"), "vms", args[2])) != nil {
						os.Exit(95)
					}
				}
			}
			os.Exit(action.Code)
		}
		if enc.Encode("written") != nil {
			os.Exit(96)
		}
	}
}

type processHello struct {
	Args                   []string
	StdoutMode, StderrMode os.FileMode
}
type processAction struct {
	Stdout, Stderr string
	Repeat, Code   int
	Exit           bool
}
type processPeer struct {
	net.Conn
	hello processHello
}

func (p processPeer) send(t *testing.T, a processAction) {
	t.Helper()
	if err := json.NewEncoder(p.Conn).Encode(a); err != nil {
		t.Fatal(err)
	}
	if !a.Exit {
		_ = p.SetReadDeadline(time.Now().Add(5 * time.Second))
		var ack string
		if err := json.NewDecoder(p.Conn).Decode(&ack); err != nil || ack != "written" {
			t.Fatalf("helper ack=%q err=%v", ack, err)
		}
	}
}
func (p processPeer) exited(t *testing.T) {
	t.Helper()
	_ = p.SetReadDeadline(time.Now().Add(5 * time.Second))
	var b [1]byte
	if _, err := p.Read(b[:]); !processConnectionClosed(err) {
		t.Fatalf("child did not close its control descriptor: %v", err)
	}
}

func processConnectionClosed(err error) bool {
	// Killing a child with an unread control action may reset TCP instead of
	// sending FIN. Both prove its fd closed; timeouts and other errors do not.
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)
}

type processFixture struct {
	listener  net.Listener
	peers     chan processPeer
	done      chan struct{}
	closing   chan struct{}
	mu        sync.Mutex
	conns     []net.Conn
	calls     [][]string
	err       error
	closeOnce sync.Once
	bin, logs string
}

func newProcessFixture(t *testing.T, blockStage string) *processFixture {
	t.Helper()
	testutil.IsolateUserDirs(t)
	t.Setenv("TART_HOME", t.TempDir())
	f := &processFixture{peers: make(chan processPeer, 20), done: make(chan struct{}), closing: make(chan struct{}), bin: t.TempDir(), logs: t.TempDir()}
	t.Setenv("TMPDIR", f.logs)
	t.Setenv("PATH", f.bin)
	t.Setenv("CRABBOX_TART_TEST_HELPER", "1")
	// Do not spend a second in the race runtime at each controlled child exit.
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tart", "ssh", "ssh-keygen"} {
		script := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestTartProcessHelper$ -- " + name + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(f.bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.listener, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_TART_TEST_ADDRESS", f.listener.Addr().String())
	go func() {
		defer close(f.done)
		runs := map[string]net.Conn{}
		for {
			conn, err := f.listener.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conns = append(f.conns, conn)
			f.mu.Unlock()
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			var hello processHello
			if err := json.NewDecoder(conn).Decode(&hello); err != nil {
				select {
				case <-f.closing:
					return
				default:
				}
				f.mu.Lock()
				f.err = err
				f.mu.Unlock()
				return
			}
			_ = conn.SetReadDeadline(time.Time{})
			f.mu.Lock()
			f.calls = append(f.calls, hello.Args)
			f.mu.Unlock()
			stage := helperStage(hello.Args)
			if stage == "run" {
				runs[hello.Args[2]] = conn
			}
			if stage == "run" || stage == blockStage {
				select {
				case f.peers <- processPeer{Conn: conn, hello: hello}:
				case <-f.closing:
					return
				}
				continue
			}
			action := processAction{Exit: true}
			switch stage {
			case "list":
				action.Stdout = "[]"
			case "ip":
				action.Stdout = "192.0.2.10\n"
			case "stop":
				// Cleanup may not rely on fake `tart stop` to kill the child.
				// Its control fd must already be closed before this command.
				if run := runs[hello.Args[2]]; run != nil {
					_ = run.SetReadDeadline(time.Now().Add(5 * time.Second))
					var data [1]byte
					if _, err := run.Read(data[:]); !processConnectionClosed(err) {
						f.mu.Lock()
						f.err = fmt.Errorf("VM child still alive at stop: %v", err)
						f.mu.Unlock()
						return
					}
				}
			}
			if err := json.NewEncoder(conn).Encode(action); err != nil {
				f.mu.Lock()
				f.err = err
				f.mu.Unlock()
				return
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		f.close()
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.err != nil {
			t.Errorf("helper server: %v", f.err)
		}
	})
	return f
}
func helperStage(args []string) string {
	if args[0] == "ssh-keygen" {
		return "keygen"
	}
	if args[0] == "ssh" {
		for _, arg := range args[1:] {
			if arg == "-G" {
				return "ssh-config"
			}
		}
		return "ssh"
	}
	if args[1] == "exec" {
		if len(args) == 4 && args[3] == "/usr/bin/true" {
			return "agent"
		}
		if strings.Contains(args[len(args)-1], "authorized_keys") {
			return "key"
		}
		return "desktop"
	}
	return args[1]
}
func (f *processFixture) close() {
	f.closeOnce.Do(func() {
		close(f.closing)
		_ = f.listener.Close()
		f.mu.Lock()
		for _, conn := range f.conns {
			_ = conn.Close()
		}
		f.mu.Unlock()
		<-f.done
	})
}
func (f *processFixture) next(t *testing.T, stage string) processPeer {
	t.Helper()
	select {
	case peer := <-f.peers:
		if got := helperStage(peer.hello.Args); got != stage {
			t.Fatalf("stage=%s want %s (args=%q)", got, stage, peer.hello.Args)
		}
		return peer
	case <-time.After(10 * time.Second):
		t.Fatalf("no helper handshake for %s", stage)
	}
	return processPeer{}
}
func (f *processFixture) assertLogsRemoved(t *testing.T) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(f.logs, "crabbox-tart-run-*.log"))
	if err != nil || len(paths) != 0 {
		t.Errorf("startup logs=%v err=%v", paths, err)
	}
}
func (f *processFixture) backend() *backend {
	cfg := core.BaseConfig()
	cfg.Tart.Image = "operator-managed-test-image"
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: processRunner{}}).(*backend)
	b.startupObserveTimeout = time.Millisecond
	return b
}

// Only task-owned executables are reachable. Each Run joins its actual child,
// including when the startup handle cancels a blocked readiness command.
type processRunner struct{}

func (processRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	cmd := exec.CommandContext(ctx, req.Name, req.Args...)
	cmd.Env = req.Env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := core.LocalCommandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	return result, err
}
func awaitProcess(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("child/reaper did not finish promptly")
	}
}
func awaitAcquisition(t *testing.T, done <-chan struct{}) {
	t.Helper()
	// Successful acquisition includes IP polling and durable claim fsyncs, not
	// just child shutdown. Join it before fixture directories can be removed.
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("acquisition did not finish")
	}
}

func startProcess(t *testing.T, f *processFixture, ctx context.Context, keep bool) (*startupProcess, processPeer) {
	t.Helper()
	p, err := f.backend().startVM(ctx, "test-vm", keep)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = p.kill()
		awaitProcess(t, p.done)
		p.mu.Lock()
		defer p.mu.Unlock()
		if err := p.closeLog(); err != nil {
			t.Error(err)
		}
	})
	return p, f.next(t, "run")
}

func TestDetachCommandCreatesSession(t *testing.T) {
	cmd := exec.Command("tart", "run", "test")
	detachCommand(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("detached command must start in a new session: %#v", cmd.SysProcAttr)
	}
}

func TestStartupLateExit(t *testing.T) {
	for _, keep := range []bool{false, true} {
		for _, code := range []int{42, 0} {
			t.Run(fmt.Sprintf("keep=%t/code=%d", keep, code), func(t *testing.T) {
				f := newProcessFixture(t, "")
				p, child := startProcess(t, f, t.Context(), keep)
				// startVM has returned from its observation window; only now allow exit.
				detail := startupFailure
				if code == 0 {
					detail = ""
				}
				child.send(t, processAction{Stderr: detail, Exit: true, Code: code})
				awaitProcess(t, p.ctx.Done())
				err := p.abort(errors.New("later IP symptom"))
				want := "tart run test-vm failed during startup: " + startupFailure
				if code == 0 {
					want = "tart run test-vm exited unexpectedly during startup"
				}
				if err == nil || err.Error() != want {
					t.Fatalf("err=%v want %q", err, want)
				}
				wantArgs := []string{"tart", "run", "test-vm", "--no-graphics", "--no-clipboard", "--no-audio"}
				if !reflect.DeepEqual(child.hello.Args, wantArgs) {
					t.Fatalf("argv=%q", child.hello.Args)
				}
				child.exited(t)
				f.assertLogsRemoved(t)
			})
		}
	}
}

func TestStartupCancellation(t *testing.T) {
	for _, keep := range []bool{false, true} {
		for _, phase := range []string{"before", "during", "after"} {
			t.Run(fmt.Sprintf("keep=%t/%s", keep, phase), func(t *testing.T) {
				f := newProcessFixture(t, "")
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				cause := errors.New("operator canceled acquisition")
				if phase == "before" {
					cancel(cause)
					p, err := f.backend().startVM(ctx, "test-vm", keep)
					if p != nil || !errors.Is(err, cause) {
						t.Fatalf("p=%v err=%v", p, err)
					}
				} else if phase == "during" {
					b := f.backend()
					b.startupObserveTimeout = time.Hour
					var startErr error
					done := make(chan struct{})
					go func() { defer close(done); _, startErr = b.startVM(ctx, "test-vm", keep) }()
					t.Cleanup(func() { cancel(cause); f.close(); awaitProcess(t, done) })
					child := f.next(t, "run")
					cancel(cause)
					awaitProcess(t, done)
					if !errors.Is(startErr, cause) {
						t.Fatal(startErr)
					}

					child.exited(t)
				} else {
					p, child := startProcess(t, f, ctx, keep)
					cancel(cause)
					err := p.handoff()
					if !errors.Is(err, cause) {
						t.Fatalf("canceled handoff: %v", err)
					}
					if err := p.abort(err); !errors.Is(err, cause) {
						t.Fatal(err)
					}
					child.exited(t)
				}
				f.assertLogsRemoved(t)
			})
		}
	}
}

func TestStartupHandoffLifetime(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep=%t", keep), func(t *testing.T) {
			f := newProcessFixture(t, "")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p, child := startProcess(t, f, ctx, keep)
			log := p.log
			if err := p.handoff(); err != nil {
				t.Fatal(err)
			}
			if _, err := log.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("parent log fd still open: %v", err)
			}
			f.assertLogsRemoved(t)
			cancel()
			if keep {
				// Round-trip proves survival after cancel, and writes after closing the
				// parent's log fd. The inherited outputs must be files, never pipes.
				if !child.hello.StderrMode.IsRegular() || child.hello.StdoutMode&os.ModeCharDevice == 0 {
					t.Fatalf("stdio modes: %+v", child.hello)
				}
				child.send(t, processAction{Stderr: "post-ready output"})
				if p.stopLifetime != nil {
					t.Fatal("detached process retained a cancellation monitor")
				}
				child.send(t, processAction{Exit: true})
			}
			awaitProcess(t, p.done)
			child.exited(t)
		})
	}
}

func TestStartupDiagnosticsAndReadinessFailure(t *testing.T) {
	for _, keep := range []bool{false, true} {
		for _, mode := range []string{"readiness", "cancel", "exit"} {
			t.Run(fmt.Sprintf("keep=%t/%s", keep, mode), func(t *testing.T) {
				f := newProcessFixture(t, "")
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				p, child := startProcess(t, f, ctx, keep)
				log := p.log
				child.send(t, processAction{Stderr: strings.Repeat("x", 1024), Repeat: 96})
				readinessErr := errors.New("guest readiness failed while VM alive")
				if mode == "exit" {
					child.send(t, processAction{Exit: true, Code: 42})
					awaitProcess(t, p.ctx.Done())
				} else if mode == "cancel" {
					cancel(readinessErr)
				}
				err := p.abort(readinessErr)
				if mode == "exit" {
					want := "tart run test-vm failed during startup: " + strings.Repeat("x", startupDiagnosticLimit)
					if err == nil || err.Error() != want {
						t.Fatalf("truncated error length=%d", len(fmt.Sprint(err)))
					}
				} else {
					if !errors.Is(err, readinessErr) {
						t.Fatalf("cleanup obscured readiness error: %v", err)
					}
					want := "tart run test-vm startup stderr: " + strings.Repeat("x", startupDiagnosticLimit)
					if !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), strings.Repeat("x", startupDiagnosticLimit+1)) {
						t.Fatalf("returned error lost or exceeded bounded startup stderr (length=%d)", len(err.Error()))
					}
					if strings.Contains(err.Error(), "signal: killed") || strings.Contains(err.Error(), "failed during startup") {
						t.Fatal("cleanup termination was presented as the original startup failure")
					}
					// The CLI prints the first ExitError.Message, not the whole
					// error chain. Diagnostics must survive that presentation too.
					var cliErr core.ExitError
					if core.AsExitError(err, &cliErr) && !strings.Contains(cliErr.Message, want) {
						t.Fatal("CLI exit message discarded startup stderr")
					}
				}
				if len(p.detail) != startupDiagnosticLimit {
					t.Fatalf("retained bytes=%d", len(p.detail))
				}
				if _, err := log.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("log fd: %v", err)
				}
				child.exited(t)
				f.assertLogsRemoved(t)
			})
		}
	}
}

func TestStartupNaturalExitRefreshesSnapshot(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep=%t", keep), func(t *testing.T) {
			f := newProcessFixture(t, "")
			p, child := startProcess(t, f, t.Context(), keep)
			// Stage abort's pre-signal snapshot, then explicitly let natural
			// exit win. No timing-sensitive pause inside abort is needed.
			p.mu.Lock()
			p.capture()
			p.mu.Unlock()
			child.send(t, processAction{Stderr: startupFailure})
			child.send(t, processAction{Exit: true, Code: 42})
			awaitProcess(t, p.done)
			err := p.abort(errors.New("independent readiness failure"))
			want := "tart run test-vm failed during startup: " + startupFailure
			if err == nil || err.Error() != want {
				t.Fatalf("final startup error=%v want %q", err, want)
			}
			child.exited(t)
			f.assertLogsRemoved(t)
		})
	}
}

func TestStartupStartFailureRemovesLog(t *testing.T) {
	f := newProcessFixture(t, "")
	if err := os.WriteFile(filepath.Join(f.bin, "tart"), []byte("not an executable format"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, keep := range []bool{false, true} {
		before := processFileDescriptors(t)
		p, err := f.backend().startVM(t.Context(), "test-vm", keep)
		if p != nil || err == nil {
			t.Fatalf("p=%v err=%v", p, err)
		}
		f.assertLogsRemoved(t)
		if !reflect.DeepEqual(before, processFileDescriptors(t)) {
			t.Fatal("start failure leaked a file descriptor")
		}
	}
}

func processFileDescriptors(t *testing.T) map[int]uint64 {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]uint64{}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		var stat syscall.Stat_t
		if err == nil && syscall.Fstat(fd, &stat) == nil {
			kind := stat.Mode & syscall.S_IFMT
			if kind == syscall.S_IFREG || kind == syscall.S_IFCHR {
				out[fd] = uint64(stat.Ino)
			}
		}
	}
	return out
}

type acquisitionResult struct {
	lease LeaseTarget
	err   error
	done  chan struct{}
}

func acquireProcess(t *testing.T, f *processFixture, b *backend, ctx context.Context, cancel context.CancelCauseFunc, keep bool) *acquisitionResult {
	t.Helper()
	result := &acquisitionResult{done: make(chan struct{})}
	repo := t.TempDir()
	go func() {
		defer close(result.done)
		result.lease, result.err = b.Acquire(ctx, core.AcquireRequest{Keep: keep, Repo: core.Repo{Root: repo}})
	}()
	t.Cleanup(func() {
		cancel(nil)
		f.close() // Also releases a kept helper if an assertion failed after handoff.
		awaitAcquisition(t, result.done)
	})
	return result
}
func (f *processFixture) assertFailedAcquisitionCleaned(t *testing.T, child processPeer) {
	t.Helper()
	child.exited(t)
	f.assertLogsRemoved(t)
	f.mu.Lock()
	calls := append([][]string(nil), f.calls...)
	f.mu.Unlock()
	if len(calls) < 2 || helperStage(calls[len(calls)-2]) != "stop" || helperStage(calls[len(calls)-1]) != "delete" {
		t.Fatalf("missing fenced stop/delete: %q", calls)
	}
	name := child.hello.Args[2]
	if _, err := os.Stat(filepath.Join(os.Getenv("TART_HOME"), "vms", name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed VM directory remains: %v", err)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := filepath.Glob(filepath.Join(configDir, "crabbox", "testboxes", "*", "id_ed25519"))
	if err != nil || len(keys) != 0 {
		t.Fatalf("failed-acquisition keys=%v err=%v", keys, err)
	}
	claims, err := listLeaseClaims()
	if err != nil || len(claims) != 0 {
		t.Fatalf("failed-acquisition claims=%v err=%v", claims, err)
	}
}

func TestAcquireStartupExitCancelsReadiness(t *testing.T) {
	for _, keep := range []bool{false, true} {
		for _, stage := range []string{"ip", "agent", "key", "desktop", "ssh"} {
			t.Run(fmt.Sprintf("keep=%t/%s", keep, stage), func(t *testing.T) {
				f := newProcessFixture(t, stage)
				b := f.backend()
				b.cfg.Desktop = stage == "desktop"
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				result := acquireProcess(t, f, b, ctx, cancel, keep)
				child := result.next(t, f, "run")
				blocked := result.next(t, f, stage)
				// This handshake is after startVM's observation AND, for agent/key/SSH,
				// after a successful (potentially stale) IP lookup. The real readiness
				// child now blocks indefinitely unless startup exit cancels it.
				child.send(t, processAction{Stderr: startupFailure, Exit: true, Code: 42})
				awaitProcess(t, result.done)
				want := "tart run " + child.hello.Args[2] + " failed during startup: " + startupFailure
				if result.err == nil || result.err.Error() != want {
					t.Fatalf("Acquire: %v want %q", result.err, want)
				}
				blocked.exited(t)
				f.assertFailedAcquisitionCleaned(t, child)
			})
		}
	}
}

func TestAcquireStartupReadinessFailureAndCancellation(t *testing.T) {
	for _, keep := range []bool{false, true} {
		for _, mode := range []string{"readiness-error", "cancel", "claim-error"} {
			t.Run(fmt.Sprintf("keep=%t/%s", keep, mode), func(t *testing.T) {
				stage := "agent"
				if mode == "claim-error" {
					stage = "ssh"
				}
				f := newProcessFixture(t, stage)
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				result := acquireProcess(t, f, f.backend(), ctx, cancel, keep)
				child := result.next(t, f, "run")
				blocked := result.next(t, f, stage)
				cause := errors.New("operator canceled blocked readiness")
				var claimObstruction string
				switch mode {
				case "readiness-error":
					child.send(t, processAction{Stderr: startupFailure})
					blocked.send(t, processAction{Exit: true, Code: 1, Stderr: "permanent guest-agent failure"})
				case "cancel":
					child.send(t, processAction{Stderr: startupFailure})
					cancel(cause)
				case "claim-error":
					state, err := core.CrabboxStateDir()
					if err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(state, 0o700); err != nil {
						t.Fatal(err)
					}
					claimObstruction = filepath.Join(state, "claims")
					if err := os.WriteFile(claimObstruction, []byte("claim publication must fail"), 0o600); err != nil {
						t.Fatal(err)
					}
					blocked.send(t, processAction{Exit: true})
				}
				awaitProcess(t, result.done)
				if result.err == nil || strings.Contains(result.err.Error(), "signal: killed") {
					t.Fatalf("cleanup obscured failure: %v", result.err)
				}
				if mode == "cancel" && !errors.Is(result.err, cause) {
					t.Fatalf("lost cancel cause: %v", result.err)
				}
				if mode == "readiness-error" && !strings.Contains(result.err.Error(), "permanent guest-agent failure") {
					t.Fatal(result.err)
				}
				if mode != "claim-error" {
					want := "tart run " + child.hello.Args[2] + " startup stderr: " + startupFailure
					if !strings.Contains(result.err.Error(), want) {
						t.Fatalf("Acquire discarded acknowledged startup stderr: %v", result.err)
					}
					var cliErr core.ExitError
					if core.AsExitError(result.err, &cliErr) && !strings.Contains(cliErr.Message, want) {
						t.Fatalf("CLI discarded startup stderr: %s", cliErr.Message)
					}
				}
				if claimObstruction != "" {
					if err := os.Remove(claimObstruction); err != nil {
						t.Fatal(err)
					}
				}
				blocked.exited(t)
				f.assertFailedAcquisitionCleaned(t, child)
			})
		}
	}
}

func TestAcquireStartupSuccessLifetime(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep=%t", keep), func(t *testing.T) {
			f := newProcessFixture(t, "")
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			result := acquireProcess(t, f, f.backend(), ctx, cancel, keep)
			child := result.next(t, f, "run")
			awaitAcquisition(t, result.done)
			if result.err != nil {
				t.Fatal(result.err)
			}
			claims, err := listLeaseClaims()
			if err != nil || len(claims) != 1 || claims[0].LeaseID != result.lease.LeaseID || claims[0].CloudImmutableID == "" {
				t.Fatalf("claim=%+v err=%v", claims, err)
			}
			f.assertLogsRemoved(t)
			cancel(errors.New("caller returned"))
			if keep {
				child.send(t, processAction{Stdout: "ignored stdout", Stderr: "still alive after Acquire and caller cancellation"})
				child.send(t, processAction{Exit: true})
			}
			child.exited(t)
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, args := range f.calls {
				if helperStage(args) == "stop" || helperStage(args) == "delete" {
					t.Fatalf("successful acquisition cleaned VM: %q", args)
				}
			}
		})
	}
}

func (r *acquisitionResult) next(t *testing.T, f *processFixture, stage string) processPeer {
	t.Helper()
	select {
	case peer := <-f.peers:
		if got := helperStage(peer.hello.Args); got != stage {
			t.Fatalf("stage=%s want %s", got, stage)
		}
		return peer
	case <-r.done:
		t.Fatalf("Acquire ended before %s: %v", stage, r.err)
	case <-time.After(10 * time.Second):
		t.Fatalf("no acquisition handshake for %s", stage)
	}
	return processPeer{}
}

func TestStartupInitialExit(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep=%t", keep), func(t *testing.T) {
			f := newProcessFixture(t, "")
			b := f.backend()
			b.startupObserveTimeout = time.Hour
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			var startErr error
			go func() { defer close(done); _, startErr = b.startVM(ctx, "test-vm", keep) }()
			t.Cleanup(func() { cancel(); f.close(); awaitProcess(t, done) })
			child := f.next(t, "run")
			child.send(t, processAction{Exit: true, Code: 42, Stderr: startupFailure})
			awaitProcess(t, done)
			if startErr == nil || startErr.Error() != "tart run test-vm failed during startup: "+startupFailure {
				t.Fatal(startErr)
			}
			child.exited(t)
			f.assertLogsRemoved(t)
		})
	}
}

func TestStartupIndependentAcquisitions(t *testing.T) {
	f := newProcessFixture(t, "")
	first, child1 := startProcess(t, f, t.Context(), true)
	second, child2 := startProcess(t, f, t.Context(), true)
	child1.send(t, processAction{Exit: true, Code: 42, Stderr: startupFailure})
	awaitProcess(t, first.ctx.Done())
	if err := first.abort(errors.New("first failed")); err == nil {
		t.Fatal("missing failure")
	}
	if err := context.Cause(second.ctx); err != nil {
		t.Fatalf("first acquisition canceled second: %v", err)
	}
	if err := second.handoff(); err != nil {
		t.Fatal(err)
	}
	child2.send(t, processAction{Stderr: "second still alive"})
	child2.send(t, processAction{Exit: true})
	awaitProcess(t, second.done)
	f.assertLogsRemoved(t)
}

func TestStartupConcurrentHandoffExitAndCancellation(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep=%t", keep), func(t *testing.T) {
			f := newProcessFixture(t, "")
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			p, child := startProcess(t, f, ctx, keep)
			barrier := make(chan struct{})
			handedOff := make(chan error, 1)
			exited := make(chan error, 1)
			canceled := make(chan struct{})
			cause := errors.New("concurrent caller cancellation")
			go func() { <-barrier; handedOff <- p.handoff() }()
			go func() {
				<-barrier
				exited <- json.NewEncoder(child.Conn).Encode(processAction{Exit: true, Code: 42, Stderr: startupFailure})
			}()
			go func() { defer close(canceled); <-barrier; cancel(cause) }()
			close(barrier)
			err := <-handedOff
			_ = <-exited // Cancellation may close the child's socket before this write.
			<-canceled
			if err != nil {
				err = p.abort(err)
				if !errors.Is(err, cause) && !strings.Contains(err.Error(), startupFailure) {
					t.Fatalf("unexpected race result: %v", err)
				}
			}
			awaitProcess(t, p.done)
			child.exited(t)
			f.assertLogsRemoved(t)
		})
	}
}

func TestAcquireStartupExitWhileClaimFenceHeld(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep=%t", keep), func(t *testing.T) {
			f := newProcessFixture(t, "ssh")
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			result := acquireProcess(t, f, f.backend(), ctx, cancel, keep)
			child := result.next(t, f, "run")
			ssh := result.next(t, f, "ssh")
			var leaseID string
			f.mu.Lock()
			for _, args := range f.calls {
				if helperStage(args) == "keygen" {
					leaseID = filepath.Base(filepath.Dir(args[len(args)-1]))
				}
			}
			f.mu.Unlock()
			if leaseID == "" {
				t.Fatal("no task lease ID")
			}
			held, release, unlocked := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var lockErr error
			var releaseOnce sync.Once
			go func() {
				defer close(unlocked)
				lockErr = core.WithDurableLeaseClaimLockContext(t.Context(), leaseID, func(_ *core.LeaseClaim, _ bool, _ func() error) error {
					close(held)
					<-release
					return nil
				})
			}()
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); awaitProcess(t, unlocked) })
			awaitProcess(t, held)
			ssh.send(t, processAction{Exit: true})
			child.send(t, processAction{Exit: true, Code: 42, Stderr: startupFailure})
			// Acquisition must finish without us releasing the claim lock.
			awaitProcess(t, result.done)
			if result.err == nil || !strings.Contains(result.err.Error(), startupFailure) {
				t.Fatal(result.err)
			}
			releaseOnce.Do(func() { close(release) })
			awaitProcess(t, unlocked)
			if lockErr != nil {
				t.Fatal(lockErr)
			}
			f.assertFailedAcquisitionCleaned(t, child)
		})
	}
}

func TestAcquireStartupExitPreservesOwnershipFence(t *testing.T) {
	f := newProcessFixture(t, "agent")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	result := acquireProcess(t, f, f.backend(), ctx, cancel, true)
	child := result.next(t, f, "run")
	_ = result.next(t, f, "agent")
	marker := filepath.Join(os.Getenv("TART_HOME"), "vms", child.hello.Args[2], tartOwnershipFile)
	if err := os.WriteFile(marker, []byte(strings.Repeat("0", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	child.send(t, processAction{Exit: true, Code: 42, Stderr: startupFailure})
	awaitProcess(t, result.done)
	if result.err == nil || !strings.Contains(result.err.Error(), startupFailure) || !strings.Contains(result.err.Error(), "ownership marker changed") {
		t.Fatalf("failure/fence: %v", result.err)
	}
	child.exited(t)
	f.assertLogsRemoved(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, args := range f.calls {
		if helperStage(args) == "stop" || helperStage(args) == "delete" {
			t.Fatalf("cleanup crossed replaced marker: %q", args)
		}
	}
}

func TestAcquireStartupHandoffFailureRollsBackClaim(t *testing.T) {
	f := newProcessFixture(t, "ssh")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	result := acquireProcess(t, f, f.backend(), ctx, cancel, true)
	child := result.next(t, f, "run")
	ssh := result.next(t, f, "ssh")
	logs, err := filepath.Glob(filepath.Join(f.logs, "crabbox-tart-run-*.log"))
	if err != nil || len(logs) != 1 {
		t.Fatalf("startup log=%v err=%v", logs, err)
	}
	// Fault the final log removal. SSH and claim publication still succeed,
	// forcing the acquisition to roll back the exact claim it just published.
	if err := os.Remove(logs[0]); err != nil {
		t.Fatal(err)
	}
	ssh.send(t, processAction{Exit: true})
	awaitProcess(t, result.done)
	if result.err == nil || !errors.Is(result.err, os.ErrNotExist) {
		t.Fatalf("handoff error: %v", result.err)
	}
	f.assertFailedAcquisitionCleaned(t, child)
}
