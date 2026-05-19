package wandb

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

//go:embed shim/wandb_sandbox.py
var shimSource []byte

// wandbMaxResponseBytes caps shim JSON-on-stdout reads (and other captured
// outputs) so a runaway shim can't OOM the parent.
const wandbMaxResponseBytes = 16 << 20

// wandbAPIError surfaces a non-zero shim exit. Tests assert that backend
// methods bubble this type up unchanged so callers can branch on it.
type wandbAPIError struct {
	ExitCode int
	Stderr   string
}

func (e *wandbAPIError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("wandb shim exited %d", e.ExitCode)
	}
	return fmt.Sprintf("wandb shim exited %d: %s", e.ExitCode, e.Stderr)
}

// wandbAPI is the minimal surface the backend uses; tests replace this with a
// fake so unit tests don't shell out to Python.
type wandbAPI interface {
	Version(ctx context.Context) (string, error)
	Acquire(ctx context.Context, req wandbAcquireRequest) (wandbSandbox, error)
	Exec(ctx context.Context, req wandbExecRequest) (int, error)
	Stop(ctx context.Context, id string, gracefulSeconds int, missingOK bool) error
	List(ctx context.Context, tags []string, status string) ([]wandbSandbox, error)
	Status(ctx context.Context, id string) (wandbSandbox, error)
}

type wandbSandbox struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

type wandbListResponse struct {
	Sandboxes []wandbSandbox `json:"sandboxes"`
}

type wandbAcquireRequest struct {
	Image           string
	MaxLifetimeSecs int
	Tags            []string
	EnvironmentVars map[string]string
}

type wandbExecRequest struct {
	SandboxID string
	Command   []string
	Cwd       string
	Timeout   int
	Stdout    io.Writer
	Stderr    io.Writer
}

// wandbShimClient executes the embedded Python shim through Runtime.Exec.
type wandbShimClient struct {
	cfg       Config
	rt        Runtime
	shimPath  string
	extractMu sync.Mutex
}

func newWandbClient(cfg Config, rt Runtime) (wandbAPI, error) {
	if rt.Exec == nil {
		return nil, exit(2, "provider=%s requires Runtime.Exec", providerName)
	}
	return &wandbShimClient{cfg: cfg, rt: rt}, nil
}

// shimDigest is computed once at init so the path is stable across processes.
var shimDigest = func() string {
	sum := sha256.Sum256(shimSource)
	return hex.EncodeToString(sum[:])[:16]
}()

// ensureShim writes the embedded Python shim to a stable temp path. If a file
// already exists at that path with the exact same bytes, the write is skipped
// (idempotent) so concurrent crabbox invocations do not stomp each other.
func (c *wandbShimClient) ensureShim() (string, error) {
	c.extractMu.Lock()
	defer c.extractMu.Unlock()
	if c.shimPath != "" {
		return c.shimPath, nil
	}
	dir := os.TempDir()
	path := filepath.Join(dir, "crabbox-wandb-shim-"+shimDigest+".py")
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, shimSource) {
		c.shimPath = path
		return path, nil
	}
	// Write atomically via a temp sibling rename so a half-written file is not
	// observable to a parallel invocation.
	tmp, err := os.CreateTemp(dir, "crabbox-wandb-shim-*.py.tmp")
	if err != nil {
		return "", fmt.Errorf("extract wandb shim: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(shimSource); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("write wandb shim: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("close wandb shim: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		// Lost a race with a concurrent extractor — accept the existing file as
		// long as its content matches.
		_ = os.Remove(tmpName)
		if existing, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(existing, shimSource) {
			c.shimPath = path
			return path, nil
		}
		return "", fmt.Errorf("install wandb shim: %w", err)
	}
	c.shimPath = path
	return path, nil
}

func (c *wandbShimClient) python() string {
	return blank(strings.TrimSpace(c.cfg.Wandb.Python), "python3")
}

// runShim invokes the shim with the given subcommand args, optionally streaming
// stdout/stderr through the provided writers. Returns the result and a typed
// *wandbAPIError on non-zero exit.
func (c *wandbShimClient) runShim(ctx context.Context, args []string, stdout, stderr io.Writer) (LocalCommandResult, error) {
	shim, err := c.ensureShim()
	if err != nil {
		return LocalCommandResult{}, err
	}
	full := append([]string{shim}, args...)
	res, err := c.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:   c.python(),
		Args:   full,
		Env:    os.Environ(),
		Stdout: stdout,
		Stderr: stderr,
	})
	if err != nil || res.ExitCode != 0 {
		return res, &wandbAPIError{ExitCode: nonZeroExit(res.ExitCode, err), Stderr: capStderr(res.Stderr)}
	}
	return res, nil
}

// runShimCapture buffers stdout and reads up to wandbMaxResponseBytes via
// io.LimitReader to defend against runaway shim output.
func (c *wandbShimClient) runShimCapture(ctx context.Context, args []string) (string, error) {
	var stdout bytes.Buffer
	limited := newLimitedWriter(&stdout, wandbMaxResponseBytes)
	res, err := c.runShim(ctx, args, limited, c.rt.Stderr)
	if err != nil {
		return "", err
	}
	_ = res
	if limited.exceeded {
		return "", fmt.Errorf("wandb shim response exceeds %d bytes", wandbMaxResponseBytes)
	}
	return stdout.String(), nil
}

func (c *wandbShimClient) Version(ctx context.Context) (string, error) {
	out, err := c.runShimCapture(ctx, []string{"version"})
	if err != nil {
		return "", err
	}
	var payload struct {
		Cwsandbox string `json:"cwsandbox"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &payload); err != nil {
		return "", fmt.Errorf("decode wandb shim version: %w", err)
	}
	return strings.TrimSpace(payload.Cwsandbox), nil
}

func (c *wandbShimClient) Acquire(ctx context.Context, req wandbAcquireRequest) (wandbSandbox, error) {
	args := []string{"acquire", "--image", req.Image}
	if req.MaxLifetimeSecs > 0 {
		args = append(args, "--max-lifetime", strconv.Itoa(req.MaxLifetimeSecs))
	}
	for _, tag := range req.Tags {
		if strings.TrimSpace(tag) == "" {
			continue
		}
		args = append(args, "--tag", tag)
	}
	for k, v := range req.EnvironmentVars {
		args = append(args, "--env", k+"="+v)
	}
	out, err := c.runShimCapture(ctx, args)
	if err != nil {
		return wandbSandbox{}, err
	}
	var sb wandbSandbox
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &sb); err != nil {
		return wandbSandbox{}, fmt.Errorf("decode wandb acquire response: %w", err)
	}
	if sb.ID == "" {
		return wandbSandbox{}, fmt.Errorf("wandb acquire returned empty sandbox id")
	}
	return sb, nil
}

func (c *wandbShimClient) Exec(ctx context.Context, req wandbExecRequest) (int, error) {
	if req.SandboxID == "" {
		return 0, fmt.Errorf("wandb exec: sandbox id is required")
	}
	if len(req.Command) == 0 {
		return 0, fmt.Errorf("wandb exec: command is required")
	}
	args := []string{"exec", "--id", req.SandboxID}
	if req.Cwd != "" {
		args = append(args, "--cwd", req.Cwd)
	}
	if req.Timeout > 0 {
		args = append(args, "--timeout", strconv.Itoa(req.Timeout))
	}
	args = append(args, "--")
	args = append(args, req.Command...)
	stdout := req.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := req.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	res, err := c.runShim(ctx, args, stdout, stderr)
	if err != nil {
		var apiErr *wandbAPIError
		if errors.As(err, &apiErr) {
			// `exec` exits with the sandbox process's return code; surface that as
			// the result code without flagging it as a transport-level error.
			return apiErr.ExitCode, nil
		}
		return res.ExitCode, err
	}
	return res.ExitCode, nil
}

func (c *wandbShimClient) Stop(ctx context.Context, id string, gracefulSeconds int, missingOK bool) error {
	args := []string{"stop", "--id", id}
	if gracefulSeconds > 0 {
		args = append(args, "--graceful-seconds", strconv.Itoa(gracefulSeconds))
	}
	if missingOK {
		args = append(args, "--missing-ok")
	}
	_, err := c.runShimCapture(ctx, args)
	return err
}

func (c *wandbShimClient) List(ctx context.Context, tags []string, status string) ([]wandbSandbox, error) {
	args := []string{"list"}
	for _, tag := range tags {
		if strings.TrimSpace(tag) == "" {
			continue
		}
		args = append(args, "--tag", tag)
	}
	if status != "" {
		args = append(args, "--status", status)
	}
	out, err := c.runShimCapture(ctx, args)
	if err != nil {
		return nil, err
	}
	var payload wandbListResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &payload); err != nil {
		return nil, fmt.Errorf("decode wandb list response: %w", err)
	}
	return payload.Sandboxes, nil
}

func (c *wandbShimClient) Status(ctx context.Context, id string) (wandbSandbox, error) {
	out, err := c.runShimCapture(ctx, []string{"status", "--id", id})
	if err != nil {
		return wandbSandbox{}, err
	}
	var sb wandbSandbox
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &sb); err != nil {
		return wandbSandbox{}, fmt.Errorf("decode wandb status response: %w", err)
	}
	return sb, nil
}

// limitedWriter wraps a writer with a hard byte cap, mirroring io.LimitReader
// semantics on the write side so callers can detect runaway shim output.
type limitedWriter struct {
	w        io.Writer
	limit    int64
	written  int64
	exceeded bool
}

func newLimitedWriter(w io.Writer, limit int64) *limitedWriter {
	return &limitedWriter{w: w, limit: limit}
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	remaining := l.limit - l.written
	if remaining <= 0 {
		l.exceeded = true
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		l.exceeded = true
		n, err := l.w.Write(p[:remaining])
		l.written += int64(n)
		return len(p), err
	}
	n, err := l.w.Write(p)
	l.written += int64(n)
	return n, err
}

func nonZeroExit(code int, err error) int {
	if code != 0 {
		return code
	}
	if err != nil {
		return 1
	}
	return 0
}

func capStderr(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 4096 {
		return value[:4096] + "…"
	}
	return value
}
