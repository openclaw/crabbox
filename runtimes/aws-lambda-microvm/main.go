package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	listenAddress = ":8080"
	maxExecBody   = 1 << 20
	maxUploadBody = 2 << 30
)

var (
	uploadNamePattern = regexp.MustCompile(`^crabbox-sync-[a-f0-9]+\.tgz$`)
	envNamePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type server struct{ execSlot chan struct{} }

type execRequest struct {
	Command string            `json:"command"`
	Workdir string            `json:"workdir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type streamEvent struct {
	Stream   string `json:"stream,omitempty"`
	Data     []byte `json:"data,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Error    string `json:"error,omitempty"`
}

func main() {
	s := &server{execSlot: make(chan struct{}, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("PUT /v1/files", s.upload)
	mux.HandleFunc("POST /v1/exec", s.exec)
	for _, hook := range []string{"ready", "validate", "run", "resume", "suspend", "terminate"} {
		mux.HandleFunc("POST /aws/lambda-microvms/runtime/v1/"+hook, lifecycle)
	}
	log.Printf("crabbox Lambda MicroVM runner listening on %s", listenAddress)
	httpServer := &http.Server{
		Addr:              listenAddress,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	log.Fatal(httpServer.ListenAndServe())
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}

func lifecycle(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *server) upload(w http.ResponseWriter, r *http.Request) {
	select {
	case s.execSlot <- struct{}{}:
		defer func() { <-s.execSlot }()
	default:
		http.Error(w, "another operation is active", http.StatusConflict)
		return
	}
	target, err := uploadPath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxUploadBody)
	tmp, err := os.OpenRoot("/tmp")
	if err != nil {
		http.Error(w, "open upload directory", http.StatusInternalServerError)
		return
	}
	defer tmp.Close()
	name := filepath.Base(target)
	_ = tmp.Remove(name)
	file, err := tmp.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		http.Error(w, "create upload", http.StatusInternalServerError)
		return
	}
	_, copyErr := io.Copy(file, body)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		_ = tmp.Remove(name)
		http.Error(w, "write upload", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func uploadPath(value string) (string, error) {
	clean := filepath.Clean(strings.TrimSpace(value))
	if filepath.Dir(clean) != "/tmp" || !uploadNamePattern.MatchString(filepath.Base(clean)) {
		return "", fmt.Errorf("upload path must be /tmp/crabbox-sync-<hex>.tgz")
	}
	return clean, nil
}

func (s *server) exec(w http.ResponseWriter, r *http.Request) {
	select {
	case s.execSlot <- struct{}{}:
		defer func() { <-s.execSlot }()
	default:
		http.Error(w, "another command is active", http.StatusConflict)
		return
	}
	var req execRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxExecBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "invalid exec request", http.StatusBadRequest)
		return
	}
	if err := validateExecRequest(req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	script, err := os.CreateTemp("/tmp", "crabbox-exec-*.sh")
	if err != nil {
		http.Error(w, "create command script", http.StatusInternalServerError)
		return
	}
	scriptPath := script.Name()
	defer func() { _ = os.Remove(scriptPath) }()
	if _, err := io.WriteString(script, req.Command); err != nil {
		_ = script.Close()
		http.Error(w, "write command script", http.StatusInternalServerError)
		return
	}
	if err := script.Close(); err != nil {
		http.Error(w, "close command script", http.StatusInternalServerError)
		return
	}
	// Keep user-provided script text out of argv while preserving child-process stdin.
	cmd := exec.CommandContext(r.Context(), "/bin/sh", scriptPath)
	cmd.Dir = req.Workdir
	cmd.Env = append(os.Environ(), envList(req.Env)...)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	stream := newEventStream(w)
	cmd.Stdout = stream.writer("stdout")
	cmd.Stderr = stream.writer("stderr")
	// Bound inherited pipes when a background child outlives the requested shell.
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		_ = stream.write(streamEvent{Error: "start command: " + err.Error()})
		return
	}
	waitErr := cmd.Wait()
	exitCode := 0
	if waitErr != nil && !errors.Is(waitErr, exec.ErrWaitDelay) {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			_ = stream.write(streamEvent{Error: "wait command: " + waitErr.Error()})
			return
		}
	}
	_ = stream.write(streamEvent{ExitCode: &exitCode})
}

func validateExecRequest(req execRequest) error {
	if strings.TrimSpace(req.Command) == "" || strings.ContainsRune(req.Command, '\x00') {
		return fmt.Errorf("command is required")
	}
	if req.Workdir == "" {
		return fmt.Errorf("workdir is required")
	}
	if !filepath.IsAbs(req.Workdir) || filepath.Clean(req.Workdir) != req.Workdir {
		return fmt.Errorf("workdir must be a clean absolute path")
	}
	for name, value := range req.Env {
		if !envNamePattern.MatchString(name) || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("invalid environment entry")
		}
	}
	return nil
}

func envList(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	return result
}

type eventStream struct {
	mu      sync.Mutex
	encoder *json.Encoder
	flusher http.Flusher
}

func newEventStream(w http.ResponseWriter) *eventStream {
	flusher, _ := w.(http.Flusher)
	return &eventStream{encoder: json.NewEncoder(w), flusher: flusher}
}

func (s *eventStream) write(event streamEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.encoder.Encode(event); err != nil {
		return err
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return nil
}

func (s *eventStream) writer(name string) io.Writer {
	return streamWriter{stream: s, name: name}
}

type streamWriter struct {
	stream *eventStream
	name   string
}

func (w streamWriter) Write(data []byte) (int, error) {
	if err := w.stream.write(streamEvent{Stream: w.name, Data: data}); err != nil {
		return 0, err
	}
	return len(data), nil
}
