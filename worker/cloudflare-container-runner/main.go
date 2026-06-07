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
	"syscall"
	"time"
)

// Cloudflare can truncate the final NDJSON event when a response opens and
// closes almost immediately, so keep very short exec streams alive briefly.
const minStreamLifetime = 75 * time.Millisecond
const finalStreamFlushDelay = 100 * time.Millisecond
const streamHeartbeatInterval = 15 * time.Second

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
	defer file.Close()
	if _, err := io.Copy(file, r.Body); err != nil {
		http.Error(w, fmt.Sprintf("write destination: %v", err), http.StatusInternalServerError)
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

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-done:
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go copyPipe(&wg, stdout, "stdout", writer)
	go copyPipe(&wg, stderr, "stderr", writer)

	waitErr := cmd.Wait()
	wg.Wait()
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

func copyPipe(wg *sync.WaitGroup, reader io.Reader, eventType string, writer *eventWriter) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			writer.write(streamEvent{Type: eventType, Data: string(buf[:n])})
		}
		if err != nil {
			if !benignPipeReadError(err) {
				writer.write(streamEvent{Type: "error", Error: err.Error()})
			}
			return
		}
	}
}

func benignPipeReadError(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed)
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
