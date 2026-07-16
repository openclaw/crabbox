package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Cloudflare can truncate the final NDJSON event when a response opens and
// closes almost immediately, so keep very short exec streams alive briefly.
const minStreamLifetime = 75 * time.Millisecond
const finalStreamFlushDelay = 100 * time.Millisecond
const streamHeartbeatInterval = 15 * time.Second
const pipeReadPoll = 10 * time.Millisecond

// After the direct child is reaped, a background descendant may have
// inherited a stdout/stderr write-end (e.g. `sleep 30 & echo done`) and still
// hold it open, so the owned read-ends never reach a natural EOF on their own.
// runCommand handles that with an IDLE-AWARE DRAIN, not a single fixed grace:
//
//   - pipeDrainIdle is how long the drain waits for a lull with NO new bytes
//     copied before concluding the child's real output is fully flushed and
//     only a quiet (or absent) descendant remains. Once idle elapses with no
//     progress, we close the read-ends ourselves to unblock the copiers. A
//     descendant that keeps writing (even slowly) keeps resetting this timer,
//     so genuine trailing output is never cut short.
//   - pipeDrainPoll is the polling interval used to sample the shared `copied`
//     byte counter and decide whether progress was made since the last check.
//   - pipeDrainGrace is an ABSOLUTE CAP on descendant output, in case a
//     descendant writes continuously (e.g. `(while true; do echo x; done) &`)
//     and so never goes idle. Before starting the cap, each copier snapshots
//     and delivers every byte already buffered when it observes the direct
//     child's exit. The cap can therefore stop new descendant writes without
//     discarding buffered foreground output.
//
// None of this bounds the foreground command's own runtime: runCommand owns
// the pipe read-ends, so cmd.Wait never closes them, and a long-running
// foreground command is fully drained however long it takes — the drain phase
// (and these three timers) only start once cmd.Wait has already returned. All
// three are vars so tests can lower them.
var (
	pipeDrainIdle  = 300 * time.Millisecond
	pipeDrainPoll  = 100 * time.Millisecond
	pipeDrainGrace = 5 * time.Second
)

type execRequest struct {
	Command   string            `json:"command"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	TimeoutMS int64             `json:"timeoutMs,omitempty"`
}

type streamEvent struct {
	Type     string `json:"type"`
	Data     string `json:"data,omitempty"`
	Error    string `json:"error,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/v1/files", handleFileUpload)
	mux.HandleFunc("/v1/exec", handleExec)

	addr := ":8787"
	log.Printf("crabbox container runner listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func handleFileUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := cleanAbsolutePath(r.URL.Query().Get("path"))
	if path == "" {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		http.Error(w, fmt.Sprintf("create parent directory: %v", err), http.StatusInternalServerError)
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		http.Error(w, fmt.Sprintf("open destination: %v", err), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(file, r.Body); err != nil {
		_ = file.Close()
		http.Error(w, fmt.Sprintf("write destination: %v", err), http.StatusInternalServerError)
		return
	}
	if err := file.Close(); err != nil {
		http.Error(w, fmt.Sprintf("close destination: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path})
}

func handleExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	req.Command = strings.TrimSpace(req.Command)
	if req.Command == "" {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}
	cwd := cleanAbsolutePath(req.Cwd)
	if cwd == "" {
		cwd = "/workspace"
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		http.Error(w, fmt.Sprintf("create cwd: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	flusher, _ := w.(http.Flusher)
	writer := &eventWriter{w: w, flusher: flusher}
	writer.write(streamEvent{Type: "start"})
	heartbeatDone := make(chan struct{})
	defer close(heartbeatDone)
	go streamHeartbeat(heartbeatDone, writer)

	ctx := r.Context()
	cancel := func() {}
	if req.TimeoutMS > 0 {
		var timeoutCancel context.CancelFunc
		ctx, timeoutCancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMS)*time.Millisecond)
		cancel = timeoutCancel
	}
	defer cancel()

	started := time.Now()
	exitCode, err := runCommand(ctx, req, cwd, writer)
	if err != nil {
		if exitCode != 0 && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
			writeCompleteAfterMinimumLifetime(writer, started, exitCode)
			return
		}
		writer.write(streamEvent{Type: "error", Error: err.Error()})
		return
	}
	writeCompleteAfterMinimumLifetime(writer, started, exitCode)
}

func writeCompleteAfterMinimumLifetime(writer *eventWriter, started time.Time, exitCode int) {
	if remaining := minStreamLifetime - time.Since(started); remaining > 0 {
		time.Sleep(remaining)
	}
	writer.write(streamEvent{Type: "complete", ExitCode: &exitCode})
	time.Sleep(finalStreamFlushDelay)
}

func streamHeartbeat(done <-chan struct{}, writer *eventWriter) {
	ticker := time.NewTicker(streamHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			writer.write(streamEvent{Type: "heartbeat"})
		}
	}
}

func runCommand(ctx context.Context, req execRequest, cwd string, writer *eventWriter) (int, error) {
	scriptPath, err := writeCommandScript(req.Command)
	if err != nil {
		return 0, err
	}
	defer os.Remove(scriptPath)

	cmd := exec.Command("/bin/bash", "-l", scriptPath)
	cmd.Dir = cwd
	cmd.Env = commandEnv(req.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Own the stdout/stderr pipes instead of using cmd.StdoutPipe/StderrPipe.
	//
	// os/exec documents that cmd.Wait closes the parent read-ends of
	// Stdout/StderrPipe as soon as the child is reaped: "it is incorrect to call
	// Wait before all reads from the pipe have completed". Any design that must
	// call cmd.Wait while a copier is still reading those read-ends therefore
	// races the copier and silently truncates buffered output — including a
	// legitimate foreground command that simply runs long and writes its result
	// right before exiting.
	//
	// By creating the pipes ourselves and reading OUR OWN read-ends, cmd.Wait
	// leaves them untouched. We can reap the child first, however long it runs,
	// and the copiers keep delivering every byte with no truncation. We drop the
	// parent's write-end copies right after Start so the child (and any
	// descendants it forks) are the only writers, letting the read-ends reach a
	// clean EOF once they all exit.
	rOut, wOut, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		_ = rOut.Close()
		_ = wOut.Close()
		return 0, err
	}
	rOutFD := int(rOut.Fd())
	rErrFD := int(rErr.Fd())
	if err := syscall.SetNonblock(rOutFD, true); err != nil {
		_ = rOut.Close()
		_ = wOut.Close()
		_ = rErr.Close()
		_ = wErr.Close()
		return 0, err
	}
	if err := syscall.SetNonblock(rErrFD, true); err != nil {
		_ = rOut.Close()
		_ = wOut.Close()
		_ = rErr.Close()
		_ = wErr.Close()
		return 0, err
	}
	cmd.Stdout = wOut
	cmd.Stderr = wErr

	if err := cmd.Start(); err != nil {
		_ = rOut.Close()
		_ = wOut.Close()
		_ = rErr.Close()
		_ = wErr.Close()
		return 0, err
	}
	// The child now holds the only write-ends; drop the parent's copies so the
	// read-ends can reach EOF once the child (and any descendants) exit.
	_ = wOut.Close()
	_ = wErr.Close()

	// Close the read-ends exactly once, whichever drain path we take, to force
	// any blocked copyPipe Read to return and to avoid leaking file descriptors.
	var closeReadersOnce sync.Once
	closeReaders := func() {
		closeReadersOnce.Do(func() {
			_ = rOut.Close()
			_ = rErr.Close()
		})
	}
	defer closeReaders()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-done:
		}
	}()

	var copied atomic.Int64
	// writesInFlight counts copiers currently blocked in a response write. A
	// slow write (downstream backpressure) freezes `copied`, so without this the
	// drain loop below could mistake an actively-flushing copier for an idle
	// pipe and close the read-ends mid-write, truncating buffered output.
	var writesInFlight atomic.Int64
	var wg sync.WaitGroup
	foregroundDone := make(chan struct{})
	wg.Add(2)
	go copyPipe(&wg, &interruptiblePipeReader{fd: rOutFD, stop: foregroundDone}, "stdout", writer, &copied, &writesInFlight)
	go copyPipe(&wg, &interruptiblePipeReader{fd: rErrFD, stop: foregroundDone}, "stderr", writer, &copied, &writesInFlight)

	// Reap the child first. Because we own rOut/rErr, cmd.Wait does NOT close
	// them, so the copiers keep delivering the child's output for the ENTIRE
	// lifetime of the command. A foreground command that runs arbitrarily long
	// is fully drained with no truncation — none of the drain timers below
	// bound the command's runtime, only what happens to a lingering descendant
	// after the direct child has already exited.
	waitErr := cmd.Wait()

	// Stop the foreground copiers without closing the read-ends. Each copier
	// finishes any in-flight HTTP write before observing the signal, leaving all
	// unread bytes intact in the owned kernel pipe.
	close(foregroundDone)
	wg.Wait()

	// Snapshot and deliver the bytes buffered at this foreground/descendant
	// boundary before any absolute timer starts. New descendant writes may land
	// behind the snapshot, but the finite prefix containing all foreground output
	// is guaranteed to reach the client.
	buf := make([]byte, 32*1024)
	if err := drainBufferedPipe(rOutFD, buf, "stdout", writer, &copied, &writesInFlight); err != nil {
		return 1, err
	}
	if err := drainBufferedPipe(rErrFD, buf, "stderr", writer, &copied, &writesInFlight); err != nil {
		return 1, err
	}

	descendantDone := make(chan struct{})
	wg.Add(2)
	go copyPipe(&wg, &interruptiblePipeReader{fd: rOutFD, stop: descendantDone}, "stdout", writer, &copied, &writesInFlight)
	go copyPipe(&wg, &interruptiblePipeReader{fd: rErrFD, stop: descendantDone}, "stderr", writer, &copied, &writesInFlight)

	// The direct child is reaped, but a background descendant may have
	// inherited a write-end (e.g. `sleep 30 & echo done`) and still hold the
	// pipe open, so the copiers won't reach EOF on their own. Drain
	// idle-aware: keep waiting as long as bytes keep arriving (a live
	// descendant still flushing real output), and only close the read-ends
	// once EITHER the copiers hit a genuine EOF, OR `copied` has made no
	// progress for pipeDrainIdle (a quiet/absent descendant — nothing left to
	// lose), OR the absolute pipeDrainGrace cap elapses (a descendant writing
	// continuously, e.g. `(while true; do echo x; done) &`, which would
	// otherwise keep resetting the idle timer forever). The copiers use
	// nonblocking reads, so signaling them first and joining them before closing
	// the read-ends avoids any read racing a reused file descriptor.
	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()

	ticker := time.NewTicker(pipeDrainPoll)
	defer ticker.Stop()
	deadline := time.Now().Add(pipeDrainGrace)
	idleDeadline := time.Now().Add(pipeDrainIdle)
	lastCopied := copied.Load()
drainLoop:
	for {
		select {
		case <-drained:
			break drainLoop
		case now := <-ticker.C:
			// A copier blocked in a slow response write is NOT idle: an in-flight
			// write counts as progress so the idle timer never fires mid-flush.
			// Only the absolute pipeDrainGrace cap can end the drain during a
			// persistently blocked write.
			if cur := copied.Load(); cur != lastCopied || writesInFlight.Load() > 0 {
				lastCopied = cur
				idleDeadline = now.Add(pipeDrainIdle)
			}
			if now.After(idleDeadline) || now.After(deadline) {
				close(descendantDone)
				<-drained
				closeReaders()
				break drainLoop
			}
		}
	}
	if ctx.Err() != nil {
		return 124, ctx.Err()
	}
	if waitErr == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return commandExitCode(exitErr), nil
	}
	return 1, waitErr
}

func writeCommandScript(command string) (string, error) {
	if strings.Contains(command, "\x00") {
		return "", errors.New("command contains NUL byte")
	}
	file, err := os.CreateTemp("", "crabbox-command-*.sh")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if _, err := file.WriteString(command + "\n"); err != nil {
		file.Close()
		os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

func commandExitCode(exitErr *exec.ExitError) int {
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
		if status.Signaled() {
			return 128 + int(status.Signal())
		}
		if status.Exited() {
			return status.ExitStatus()
		}
	}
	if code := exitErr.ExitCode(); code >= 0 {
		return code
	}
	return 1
}

// copyPipe reads reader until EOF (or a benign close), forwarding each chunk
// as a stream event and adding its length to copied so the idle-aware drain
// in runCommand can detect whether a background descendant is still
// producing real output after the direct child has exited.
func copyPipe(wg *sync.WaitGroup, reader io.Reader, eventType string, writer *eventWriter, copied, writesInFlight *atomic.Int64) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			writePipeChunk(buf[:n], eventType, writer, copied, writesInFlight)
		}
		if err != nil {
			if !benignPipeReadError(err) {
				writer.write(streamEvent{Type: "error", Error: err.Error()})
			}
			return
		}
	}
}

var errForegroundDrained = errors.New("foreground pipe drain boundary")

type interruptiblePipeReader struct {
	fd   int
	stop <-chan struct{}
}

func (r *interruptiblePipeReader) Read(buf []byte) (int, error) {
	for {
		if r.stop != nil {
			select {
			case <-r.stop:
				return 0, errForegroundDrained
			default:
			}
		}

		n, err := syscall.Read(r.fd, buf)
		if n == 0 && err == nil {
			return 0, io.EOF
		}
		if !errors.Is(err, syscall.EAGAIN) {
			return n, err
		}
		if r.stop == nil {
			time.Sleep(pipeReadPoll)
			continue
		}
		select {
		case <-r.stop:
			return 0, errForegroundDrained
		case <-time.After(pipeReadPoll):
		}
	}
}

func drainBufferedPipe(fd int, buf []byte, eventType string, writer *eventWriter, copied, writesInFlight *atomic.Int64) error {
	remaining, err := pipeBufferedBytes(fd)
	if err != nil {
		return fmt.Errorf("inspect buffered %s: %w", eventType, err)
	}
	for remaining > 0 {
		chunkSize := min(remaining, len(buf))
		n, readErr := syscall.Read(fd, buf[:chunkSize])
		if n > 0 {
			writePipeChunk(buf[:n], eventType, writer, copied, writesInFlight)
			remaining -= n
		}
		if readErr != nil {
			return readErr
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func writePipeChunk(chunk []byte, eventType string, writer *eventWriter, copied, writesInFlight *atomic.Int64) {
	// Mark the write in flight for the whole (potentially blocking) write so
	// the drain loop never treats a slow flush as an idle pipe.
	writesInFlight.Add(1)
	writer.write(streamEvent{Type: eventType, Data: string(chunk)})
	writesInFlight.Add(-1)
	copied.Add(int64(len(chunk)))
}

func benignPipeReadError(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, errForegroundDrained)
}

type eventWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
}

func (w *eventWriter) write(event streamEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	data, err := json.Marshal(event)
	if err != nil {
		data = []byte(`{"type":"error","error":"encode event"}`)
	}
	_, _ = w.w.Write(append(data, '\n'))
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

func commandEnv(extra map[string]string) []string {
	env := os.Environ()
	for key, value := range extra {
		if isEnvName(key) {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func cleanAbsolutePath(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || !strings.HasPrefix(trimmed, "/") || strings.Contains(trimmed, "\x00") {
		return ""
	}
	clean := filepath.Clean(trimmed)
	if clean == "." {
		return ""
	}
	return clean
}

func isEnvName(value string) bool {
	if value == "" {
		return false
	}
	reader := bufio.NewReader(strings.NewReader(value))
	first, _, err := reader.ReadRune()
	if err != nil || !isEnvFirstRune(first) {
		return false
	}
	for {
		r, _, err := reader.ReadRune()
		if errors.Is(err, io.EOF) {
			return true
		}
		if err != nil || !isEnvRune(r) {
			return false
		}
	}
}

func isEnvFirstRune(r rune) bool {
	return r == '_' || ('A' <= r && r <= 'Z') || ('a' <= r && r <= 'z')
}

func isEnvRune(r rune) bool {
	return isEnvFirstRune(r) || ('0' <= r && r <= '9')
}
