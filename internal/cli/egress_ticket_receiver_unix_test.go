//go:build !windows

package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

const receiverTestTicket = "egress_0123456789abcdef0123456789abcdef"

type egressReceiverObservation struct {
	PID                   int
	Argv                  []string
	TicketInEnvironment   bool
	Bytes                 int
	EOF, Closed           bool
	ShellExitedBeforeRead bool
	RegularLogs           bool
	ProcessGroup          int
	InputDevice           int64
	InputInode            uint64
}

// Observe a real os.Stdin without changing its read/close behavior. Only test
// metadata is written; credential bytes are checked at the WebSocket fixture.
type observedEgressReceiverInput struct {
	*os.File
	observation egressReceiverObservation
	path        string
}

func (r *observedEgressReceiverInput) Read(p []byte) (int, error) {
	n, err := r.File.Read(p)
	r.observation.Bytes += n
	r.observation.EOF = r.observation.EOF || err == io.EOF
	return n, err
}

func (r *observedEgressReceiverInput) Close() error {
	err := r.File.Close()
	_, readErr := r.File.Read(make([]byte, 1))
	r.observation.Closed = errors.Is(readErr, os.ErrClosed)
	data, marshalErr := json.Marshal(r.observation)
	if marshalErr != nil {
		return marshalErr
	}
	return errors.Join(err, os.WriteFile(r.path, data, 0o600))
}

func runEgressBootstrapTestProcess() int {
	dir := os.Getenv("CRABBOX_TEST_EGRESS_BOOTSTRAP")
	stage := "bootstrap"
	if len(os.Args) > 3 && os.Args[3] == egressTicketChildArg {
		stage = "child"
	}
	if stage == "bootstrap" {
		// Keep a test-only CLOEXEC witness until launch completes: macOS can
		// reuse a closed pipe's inode for the new pipe, defeating identity checks.
		witness, err := syscall.Dup(0)
		if err != nil {
			return 94
		}
		syscall.CloseOnExec(witness)
		defer syscall.Close(witness)
	}
	if err := os.WriteFile(filepath.Join(dir, stage+"-started"), []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
		return 90
	}
	input := &observedEgressReceiverInput{
		File: os.Stdin, path: filepath.Join(dir, stage+"-input.json"),
		observation: egressReceiverObservation{PID: os.Getpid(), Argv: os.Args},
	}
	input.observation.ProcessGroup, _ = syscall.Getpgid(0)
	var inputStat syscall.Stat_t
	if syscall.Fstat(0, &inputStat) == nil {
		input.observation.InputDevice, input.observation.InputInode = int64(inputStat.Dev), inputStat.Ino
	}
	stdout, outErr := os.Stdout.Stat()
	stderr, errErr := os.Stderr.Stat()
	input.observation.RegularLogs = outErr == nil && errErr == nil && stdout.Mode().IsRegular() && stderr.Mode().IsRegular()
	for _, entry := range os.Environ() {
		input.observation.TicketInEnvironment = input.observation.TicketInEnvironment || strings.Contains(entry, receiverTestTicket)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Delay only the detached child's read. Bootstrap must enqueue all bytes
	// and return without waiting for that child to consume the private pipe.
	deadline := time.Now().Add(5 * time.Second)
	for stage == "child" {
		if _, err := os.Stat(filepath.Join(dir, "shell-exited")); err == nil {
			input.observation.ShellExitedBeforeRead = true
			break
		} else if !os.IsNotExist(err) {
			return 91
		}
		if time.Now().After(deadline) {
			return 92
		}
		time.Sleep(10 * time.Millisecond)
	}
	err := (App{Stdout: os.Stdout, Stderr: os.Stderr, Stdin: input}).Run(ctx, os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	result := "nil"
	if err != nil {
		result = err.Error()
	}
	if err := os.WriteFile(filepath.Join(dir, stage+"-result"), []byte(result), 0o600); err != nil {
		return 93
	}
	if err != nil {
		return 1
	}
	return 0
}

func TestEgressTicketReceiverDetachedProcess(t *testing.T) {
	clearConfigEnv(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"cancel", "peer close", "upgrade failure", "empty", "malformed", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, data string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			// Real nohup and shell; only cleanup and the executable path are stubbed.
			write("pkill", "#!/bin/sh\nexit 0\n")
			write("helper", "#!/bin/sh\nexec "+shellQuote(exe)+" \"$@\"\n")
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			var calls atomic.Int32
			opened := make(chan *websocket.Conn, 1)
			bridgeClosed := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != "/v1/leases/cbx_0123456789ab/egress/client" || r.URL.RawQuery != "" {
					t.Error("unexpected request: mint/login fallback or wrong lease/role/query")
					http.NotFound(w, r)
					return
				}
				if r.Header.Get("X-Crabbox-Bridge-Ticket") != receiverTestTicket || r.Header.Get("Authorization") != "" {
					t.Error("ticket not received exactly in dedicated header")
				}
				if mode == "upgrade failure" {
					http.NotFound(w, r)
					return
				}
				ws, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer ws.CloseNow()
				opened <- ws
				defer func() { bridgeClosed <- struct{}{} }()
				for {
					_, data, err := ws.Read(ctx)
					if err != nil {
						return
					}
					var msg egressProxyMessage
					if err := json.Unmarshal(data, &msg); err != nil {
						t.Error(err)
						return
					}
					if msg.Type == "open" {
						if msg.Host != "example.com" || msg.Port != "443" {
							t.Error("unexpected proxy destination")
						}
						reply, _ := json.Marshal(egressProxyMessage{Type: "open_ok", ID: msg.ID})
						_ = ws.Write(ctx, websocket.MessageText, reply)
					}
				}
			}))
			defer server.Close()
			reservation, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			listen := reservation.Addr().String()
			_ = reservation.Close()
			remote := remoteEgressClientCommand(server.URL, "cbx_0123456789ab", "egress_receiver_test", listen)
			remote = strings.ReplaceAll(remote, egressRemoteLog, filepath.Join(dir, "helper.log"))
			remote = strings.ReplaceAll(remote, egressRemoteBinary, filepath.Join(dir, "helper"))
			// No appended wait: the foreground Go bootstrap owns input through EOF.
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", remote)
			cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "HOME=" + dir,
				"CRABBOX_CONFIG=" + filepath.Join(dir, "missing.yaml"), "CRABBOX_TEST_EGRESS_BOOTSTRAP=" + dir}
			configureDaemonCommand(cmd)
			cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			childPID := 0
			defer func() {
				if childPID != 0 {
					if err := terminateWebVNCDaemonProcessTree(childPID); err != nil {
						t.Error(err)
					}
				}
				if !waited {
					_ = cmd.Cancel()
					_ = cmd.Wait()
				}
			}()
			payload := receiverTestTicket
			switch mode {
			case "empty":
				payload = ""
			case "malformed":
				payload += "\n"
			case "oversized":
				payload += strings.Repeat("x", 4097)
			}
			if _, err := io.WriteString(input, payload); err != nil {
				t.Fatal(err)
			}
			_ = input.Close()
			err = cmd.Wait()
			waited = true
			invalid := mode == "empty" || mode == "malformed" || mode == "oversized"
			if (err != nil) != invalid || ctx.Err() != nil {
				t.Fatalf("launch shell did not exit before receiver read: %v", err)
			}
			if !invalid {
				childPID = waitForPIDFile(t, filepath.Join(dir, "child-started"))
			}
			if calls.Load() != 0 {
				t.Fatal("foreground bootstrap contacted coordinator before detached child read")
			}
			write("shell-exited", "exited and reaped\n")
			live := mode == "cancel" || mode == "peer close"
			if live {
				var ws *websocket.Conn
				select {
				case ws = <-opened:
				case <-ctx.Done():
					t.Fatal("real Go receiver did not redeem ticket")
				}
				var proxy net.Conn
				for ctx.Err() == nil {
					proxy, err = net.DialTimeout("tcp", listen, 50*time.Millisecond)
					if err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if proxy == nil {
					t.Fatal("receiver did not open loopback listener")
				}
				defer proxy.Close()
				_ = proxy.SetDeadline(time.Now().Add(5 * time.Second))
				_, _ = io.WriteString(proxy, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
				response, err := http.ReadResponse(bufio.NewReader(proxy), &http.Request{Method: "CONNECT"})
				if err != nil || response.StatusCode != 200 {
					t.Fatalf("real receiver did not relay proxy open: %v", err)
				}
				if mode == "cancel" {
					var observation egressReceiverObservation
					data, err := os.ReadFile(filepath.Join(dir, "child-input.json"))
					if err != nil || json.Unmarshal(data, &observation) != nil {
						t.Fatal("missing consumed-input observation")
					}
					if err := syscall.Kill(observation.PID, syscall.SIGTERM); err != nil {
						t.Fatal(err)
					}
				} else {
					_ = ws.Close(websocket.StatusNormalClosure, "fixture done")
				}
				if _, err := proxy.Read(make([]byte, 1)); err == nil {
					t.Fatal("active proxy connection remained open")
				} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
					t.Fatal("active proxy connection was not closed on shutdown")
				}
				select {
				case <-bridgeClosed:
				case <-ctx.Done():
					t.Fatal("bridge was not closed on shutdown")
				}
			}
			// The child is now an orphan, reaped by the OS. Wait for its group
			// to finish; the deferred group cleanup also handles assertion failures.
			deadline := time.Now().Add(5 * time.Second)
			for childPID != 0 && webVNCDaemonProcessGroupAlive(childPID) {
				if time.Now().After(deadline) {
					t.Fatal("receiver process group survived shutdown")
				}
				time.Sleep(10 * time.Millisecond)
			}
			childPID = 0
			stage := "child"
			if invalid {
				stage = "bootstrap"
			}
			data, err := os.ReadFile(filepath.Join(dir, stage+"-input.json"))
			var observation egressReceiverObservation
			if err != nil || json.Unmarshal(data, &observation) != nil || !observation.Closed || (!invalid && !observation.ShellExitedBeforeRead) || !observation.RegularLogs {
				t.Fatalf("receiver did not close consumed stdin: %v", err)
			}
			if observation.TicketInEnvironment || strings.Contains(strings.Join(observation.Argv, " "), receiverTestTicket) {
				t.Fatal("real receiver exposed ticket in argv/environment")
			}
			if mode != "oversized" && (!observation.EOF || observation.Bytes != len(payload)) {
				t.Fatal("receiver failed to consume exact input through EOF")
			}
			wantCalls := int32(0)
			if live || mode == "upgrade failure" {
				wantCalls = 1
			}
			if calls.Load() != wantCalls {
				t.Fatalf("unexpected coordinator calls: got %d want %d", calls.Load(), wantCalls)
			}
			if !invalid {
				var bootstrap egressReceiverObservation
				data, err := os.ReadFile(filepath.Join(dir, "bootstrap-input.json"))
				if err != nil || json.Unmarshal(data, &bootstrap) != nil || !bootstrap.Closed || !bootstrap.EOF || bootstrap.Bytes != len(receiverTestTicket) || !bootstrap.RegularLogs {
					t.Fatal("bootstrap failed to consume/close exact SSH input before launch")
				}
				if bootstrap.ProcessGroup == observation.ProcessGroup {
					t.Fatal("child did not detach its process group")
				}
				if bootstrap.InputInode != 0 && bootstrap.InputDevice == observation.InputDevice && bootstrap.InputInode == observation.InputInode {
					t.Fatal("child retained SSH stdin instead of private pipe")
				}
				if strings.Contains(strings.Join(bootstrap.Argv, " "), receiverTestTicket) || bootstrap.TicketInEnvironment {
					t.Fatal("bootstrap exposed ticket")
				}
			} else if _, err := os.Stat(filepath.Join(dir, "child-started")); !os.IsNotExist(err) {
				t.Fatal("invalid input launched child")
			}
			result, err := os.ReadFile(filepath.Join(dir, stage+"-result"))
			if err != nil || string(result) == "nil" || bytes.Contains(result, []byte(receiverTestTicket)) {
				t.Fatalf("missing failure/shutdown result or unsafe error: %v", err)
			}
			log, err := os.ReadFile(filepath.Join(dir, "helper.log"))
			if err != nil || bytes.Contains(log, []byte(receiverTestTicket)) {
				t.Fatalf("unsafe or missing helper log: %v", err)
			}
			if live && !bytes.Contains(log, []byte("lease=cbx_0123456789ab session=egress_receiver_test listen="+listen)) {
				t.Fatal("receiver lost lease/session/listen identity")
			}
			if conn, err := net.DialTimeout("tcp", listen, 50*time.Millisecond); err == nil {
				_ = conn.Close()
				t.Fatal("loopback proxy listener survived receiver shutdown")
			}
			t.Log("foreground Go bootstrap: SSH input closed before detached child, private pipe survives parent exit, headers/argv/log handles and cleanup verified")
		})
	}
}
