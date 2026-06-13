package vercelsandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

type sandboxSummary struct {
	ID       string            `json:"id"`
	Name     string            `json:"name,omitempty"`
	Status   string            `json:"status,omitempty"`
	State    string            `json:"state,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type vercelSandboxClient interface {
	CheckSDK(ctx context.Context) error
	CheckCLI(ctx context.Context) (string, error)
	CheckAuth(ctx context.Context) error
	CheckProject(ctx context.Context) error
	ListSandboxes(ctx context.Context) ([]sandboxSummary, error)
	CreateSandbox(context.Context, createSandboxRequest) (sandboxSummary, error)
	GetSandbox(context.Context, string) (sandboxSummary, error)
	UpdateSandboxMetadata(context.Context, string, map[string]string) (sandboxSummary, error)
	DeleteSandbox(context.Context, string) error
	UploadFile(context.Context, string, string, io.Reader) error
	Exec(context.Context, string, execRequest, io.Writer, io.Writer) (execResult, error)
}

type commandSpec struct {
	Name string
	Args []string
	Env  []string
}

type bridgeClient struct {
	cfg      Config
	rt       Runtime
	lookup   func(string) (string, error)
	run      func(context.Context, commandSpec) error
	call     func(context.Context, bridgeRequest, any) error
	execCall func(context.Context, commandSpec, bridgeRequest, io.Writer, io.Writer) (execResult, error)
}

func newBridgeClient(cfg Config, rt Runtime) (vercelSandboxClient, error) {
	return &bridgeClient{
		cfg:    cfg,
		rt:     rt,
		lookup: exec.LookPath,
		run:    runBridgeCommand,
		call:   runBridgeJSON,
		execCall: func(ctx context.Context, spec commandSpec, req bridgeRequest, stdout, stderr io.Writer) (execResult, error) {
			return runBridgeExec(ctx, spec, req, stdout, stderr)
		},
	}, nil
}

func (c *bridgeClient) CheckSDK(ctx context.Context) error {
	return c.bridgeCall(ctx, bridgeRequest{Action: "check"}, nil)
}

func (c *bridgeClient) CheckCLI(context.Context) (string, error) {
	path, err := c.lookup("sandbox")
	if err != nil {
		return "", fmt.Errorf("sandbox CLI unavailable: %w", err)
	}
	return path, nil
}

func (c *bridgeClient) CheckAuth(ctx context.Context) error {
	spec := c.sandboxListCommand()
	if err := c.run(ctx, spec); err != nil {
		return fmt.Errorf("sandbox auth/readiness check failed: %s", redactSecrets(err.Error()))
	}
	return nil
}

func (c *bridgeClient) CheckProject(context.Context) error {
	if strings.TrimSpace(c.cfg.VercelSandbox.ProjectID) == "" && strings.TrimSpace(c.cfg.VercelSandbox.Scope) == "" && strings.TrimSpace(c.cfg.VercelSandbox.TeamID) == "" {
		return errors.New("set projectId, teamId, or scope for project-scoped readiness")
	}
	return nil
}

func (c *bridgeClient) ListSandboxes(context.Context) ([]sandboxSummary, error) {
	claims, err := listVercelSandboxLeaseClaims()
	if err != nil {
		return nil, err
	}
	out := make([]sandboxSummary, 0, len(claims))
	for _, claim := range claims {
		if claim.Provider == providerName && strings.HasPrefix(claim.LeaseID, leasePrefix) {
			out = append(out, sandboxSummary{ID: claim.LeaseID})
		}
	}
	return out, nil
}

type createSandboxRequest struct {
	Name           string            `json:"name,omitempty"`
	Runtime        string            `json:"runtime,omitempty"`
	ProjectID      string            `json:"projectId,omitempty"`
	TeamID         string            `json:"teamId,omitempty"`
	Scope          string            `json:"scope,omitempty"`
	VCPUs          float64           `json:"vcpus,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"`
	Persistent     bool              `json:"persistent,omitempty"`
	Snapshot       string            `json:"snapshot,omitempty"`
	SnapshotMode   string            `json:"snapshotMode,omitempty"`
	NetworkPolicy  string            `json:"networkPolicy,omitempty"`
	NetworkAllow   []string          `json:"networkAllow,omitempty"`
	NetworkDeny    []string          `json:"networkDeny,omitempty"`
	Ports          []string          `json:"ports,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

type execRequest struct {
	Command     string            `json:"command"`
	WorkingDir  string            `json:"workingDir,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	TimeoutSecs int               `json:"timeoutSecs,omitempty"`
}

type execResult struct {
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exitCode"`
}

type bridgeRequest struct {
	Action      string                `json:"action"`
	SandboxID   string                `json:"sandboxId,omitempty"`
	RemotePath  string                `json:"remotePath,omitempty"`
	PayloadPath string                `json:"payloadPath,omitempty"`
	Create      *createSandboxRequest `json:"create,omitempty"`
	Metadata    map[string]string     `json:"metadata,omitempty"`
	Exec        *execRequest          `json:"exec,omitempty"`
	Config      bridgeConfig          `json:"config"`
}

type bridgeConfig struct {
	Runtime        string   `json:"runtime,omitempty"`
	ProjectID      string   `json:"projectId,omitempty"`
	TeamID         string   `json:"teamId,omitempty"`
	Scope          string   `json:"scope,omitempty"`
	VCPUs          float64  `json:"vcpus,omitempty"`
	TimeoutSeconds int      `json:"timeoutSeconds,omitempty"`
	Persistent     bool     `json:"persistent,omitempty"`
	Snapshot       string   `json:"snapshot,omitempty"`
	SnapshotMode   string   `json:"snapshotMode,omitempty"`
	NetworkPolicy  string   `json:"networkPolicy,omitempty"`
	NetworkAllow   []string `json:"networkAllow,omitempty"`
	NetworkDeny    []string `json:"networkDeny,omitempty"`
	Ports          []string `json:"ports,omitempty"`
}

func (c *bridgeClient) CreateSandbox(ctx context.Context, req createSandboxRequest) (sandboxSummary, error) {
	req.Runtime = blank(req.Runtime, vercelSandboxRuntime(c.cfg))
	req.ProjectID = blank(req.ProjectID, strings.TrimSpace(c.cfg.VercelSandbox.ProjectID))
	req.TeamID = blank(req.TeamID, strings.TrimSpace(c.cfg.VercelSandbox.TeamID))
	req.Scope = blank(req.Scope, strings.TrimSpace(c.cfg.VercelSandbox.Scope))
	req.VCPUs = c.cfg.VercelSandbox.VCPUs
	req.TimeoutSeconds = c.cfg.VercelSandbox.TimeoutSecs
	req.Persistent = c.cfg.VercelSandbox.Persistent || req.Persistent
	req.Snapshot = strings.TrimSpace(c.cfg.VercelSandbox.Snapshot)
	req.SnapshotMode = strings.TrimSpace(c.cfg.VercelSandbox.SnapshotMode)
	req.NetworkPolicy = strings.TrimSpace(c.cfg.VercelSandbox.NetworkPolicy)
	req.NetworkAllow = append([]string(nil), c.cfg.VercelSandbox.NetworkAllow...)
	req.NetworkDeny = append([]string(nil), c.cfg.VercelSandbox.NetworkDeny...)
	req.Ports = append([]string(nil), c.cfg.VercelSandbox.Ports...)
	var out sandboxSummary
	if err := c.bridgeCall(ctx, bridgeRequest{Action: "create", Create: &req}, &out); err != nil {
		return sandboxSummary{}, err
	}
	if out.ID == "" {
		return sandboxSummary{}, exit(5, "vercel-sandbox create returned no sandbox id")
	}
	return out, nil
}

func (c *bridgeClient) GetSandbox(ctx context.Context, id string) (sandboxSummary, error) {
	var out sandboxSummary
	if err := c.bridgeCall(ctx, bridgeRequest{Action: "get", SandboxID: id}, &out); err != nil {
		return sandboxSummary{}, err
	}
	if out.ID == "" {
		out.ID = id
	}
	return out, nil
}

func (c *bridgeClient) UpdateSandboxMetadata(ctx context.Context, id string, metadata map[string]string) (sandboxSummary, error) {
	var out sandboxSummary
	if err := c.bridgeCall(ctx, bridgeRequest{Action: "update-metadata", SandboxID: id, Metadata: metadata}, &out); err != nil {
		return sandboxSummary{}, err
	}
	if out.ID == "" {
		out.ID = id
	}
	return out, nil
}

func (c *bridgeClient) DeleteSandbox(ctx context.Context, id string) error {
	return c.bridgeCall(ctx, bridgeRequest{Action: "delete", SandboxID: id}, nil)
}

func (c *bridgeClient) UploadFile(ctx context.Context, id, remotePath string, content io.Reader) error {
	tmp, err := os.CreateTemp("", "crabbox-vercel-sandbox-upload-*.tgz")
	if err != nil {
		return fmt.Errorf("create vercel-sandbox upload handoff: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if _, err := io.Copy(tmp, content); err != nil {
		return fmt.Errorf("stream vercel-sandbox upload handoff: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close vercel-sandbox upload handoff: %w", err)
	}
	req := bridgeRequest{Action: "upload", SandboxID: id, RemotePath: remotePath, PayloadPath: tmpName}
	return c.bridgeCall(ctx, req, nil)
}

func (c *bridgeClient) Exec(ctx context.Context, id string, req execRequest, stdout, stderr io.Writer) (execResult, error) {
	bridgeReq := bridgeRequest{Action: "exec", SandboxID: id, Exec: &req, Config: c.bridgeConfig()}
	if c.execCall != nil {
		return c.execCall(ctx, c.bridgeCommandSpec(), bridgeReq, stdout, stderr)
	}
	var out execResult
	if err := c.bridgeCall(ctx, bridgeReq, &out); err != nil {
		return execResult{}, err
	}
	if out.Stdout != "" {
		if _, err := io.WriteString(stdout, out.Stdout); err != nil {
			return execResult{}, fmt.Errorf("write vercel-sandbox stdout: %w", err)
		}
	}
	if out.Stderr != "" {
		if _, err := io.WriteString(stderr, out.Stderr); err != nil {
			return execResult{}, fmt.Errorf("write vercel-sandbox stderr: %w", err)
		}
	}
	return out, nil
}

func (c *bridgeClient) bridgeCall(ctx context.Context, req bridgeRequest, out any) error {
	return c.bridgeCallWithPayload(ctx, req, nil, out)
}

func (c *bridgeClient) bridgeCallWithPayload(ctx context.Context, req bridgeRequest, payload []byte, out any) error {
	req.Config = c.bridgeConfig()
	if c.call != nil && len(payload) == 0 {
		return c.call(ctx, req, out)
	}
	return runBridgeJSONWithPayload(ctx, c.bridgeCommandSpec(), req, payload, out)
}

func (c *bridgeClient) bridgeConfig() bridgeConfig {
	return bridgeConfig{
		Runtime:        vercelSandboxRuntime(c.cfg),
		ProjectID:      strings.TrimSpace(c.cfg.VercelSandbox.ProjectID),
		TeamID:         strings.TrimSpace(c.cfg.VercelSandbox.TeamID),
		Scope:          strings.TrimSpace(c.cfg.VercelSandbox.Scope),
		VCPUs:          c.cfg.VercelSandbox.VCPUs,
		TimeoutSeconds: c.cfg.VercelSandbox.TimeoutSecs,
		Persistent:     c.cfg.VercelSandbox.Persistent,
		Snapshot:       strings.TrimSpace(c.cfg.VercelSandbox.Snapshot),
		SnapshotMode:   strings.TrimSpace(c.cfg.VercelSandbox.SnapshotMode),
		NetworkPolicy:  strings.TrimSpace(c.cfg.VercelSandbox.NetworkPolicy),
		NetworkAllow:   append([]string(nil), c.cfg.VercelSandbox.NetworkAllow...),
		NetworkDeny:    append([]string(nil), c.cfg.VercelSandbox.NetworkDeny...),
		Ports:          append([]string(nil), c.cfg.VercelSandbox.Ports...),
	}
}

func (c *bridgeClient) bridgeCommandSpec() commandSpec {
	name := strings.TrimSpace(os.Getenv("CRABBOX_VERCEL_SANDBOX_BRIDGE"))
	if name == "" {
		if exe, err := os.Executable(); err == nil && strings.TrimSpace(exe) != "" {
			return commandSpec{Name: exe, Args: []string{"__vercel-sandbox-bridge"}, Env: vercelSandboxBridgeEnv(os.Environ())}
		}
		name = "crabbox"
		return commandSpec{Name: name, Args: []string{"__vercel-sandbox-bridge"}, Env: vercelSandboxBridgeEnv(os.Environ())}
	}
	return commandSpec{Name: name, Env: vercelSandboxBridgeEnv(os.Environ())}
}

func (c *bridgeClient) sandboxListCommand() commandSpec {
	return commandSpec{
		Name: "sandbox",
		Args: []string{"list", "--all", "--limit", "1"},
		Env:  vercelSandboxBridgeEnv(os.Environ()),
	}
}

func runBridgeCommand(ctx context.Context, spec commandSpec) error {
	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...)
	cmd.Env = spec.Env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, redactSecrets(string(out)))
	}
	return nil
}

func runBridgeJSON(ctx context.Context, req bridgeRequest, out any) error {
	spec := commandSpec{
		Name: strings.TrimSpace(os.Getenv("CRABBOX_VERCEL_SANDBOX_BRIDGE")),
		Env:  vercelSandboxBridgeEnv(os.Environ()),
	}
	if spec.Name == "" {
		if exe, err := os.Executable(); err == nil && strings.TrimSpace(exe) != "" {
			spec.Name = exe
			spec.Args = []string{"__vercel-sandbox-bridge"}
		} else {
			spec.Name = "crabbox"
			spec.Args = []string{"__vercel-sandbox-bridge"}
		}
	}
	return runBridgeJSONWithPayload(ctx, spec, req, nil, out)
}

func runBridgeJSONWithPayload(ctx context.Context, spec commandSpec, req bridgeRequest, payload []byte, out any) error {
	buf, err := marshalBridgeRequest(req, payload)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...)
	cmd.Env = spec.Env
	cmd.Stdin = bytes.NewReader(buf)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("vercel-sandbox bridge %s failed: %s: %s", req.Action, err, redactSecrets(stderr.String()))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(&stdout).Decode(out); err != nil {
		return fmt.Errorf("decode vercel-sandbox bridge %s response: %w", req.Action, err)
	}
	return nil
}

type bridgeExecFrame struct {
	Type     string `json:"type,omitempty"`
	Data     string `json:"data,omitempty"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exitCode"`
}

const bridgeExecCaptureLimit = 4 * 1024 * 1024

func appendBridgeExecOutput(dst *string, value string) {
	remaining := bridgeExecCaptureLimit - len(*dst)
	if remaining <= 0 {
		return
	}
	if len(value) > remaining {
		value = value[:remaining]
	}
	*dst += value
}

func runBridgeExec(ctx context.Context, spec commandSpec, req bridgeRequest, stdout, stderr io.Writer) (execResult, error) {
	buf, err := marshalBridgeRequest(req, nil)
	if err != nil {
		return execResult{}, err
	}
	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...)
	cmd.Env = spec.Env
	cmd.Stdin = bytes.NewReader(buf)
	bridgeStdout, err := cmd.StdoutPipe()
	if err != nil {
		return execResult{}, fmt.Errorf("open vercel-sandbox bridge stdout: %w", err)
	}
	var bridgeStderr bytes.Buffer
	cmd.Stderr = &bridgeStderr
	if err := cmd.Start(); err != nil {
		return execResult{}, fmt.Errorf("start vercel-sandbox bridge exec: %w", err)
	}

	var result execResult
	sawResult := false
	decoder := json.NewDecoder(bridgeStdout)
	for {
		var frame bridgeExecFrame
		if err := decoder.Decode(&frame); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return execResult{}, fmt.Errorf("decode vercel-sandbox bridge exec frame: %w", err)
		}
		switch frame.Type {
		case "stdout":
			appendBridgeExecOutput(&result.Stdout, frame.Data)
			if _, err := io.WriteString(stdout, frame.Data); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return execResult{}, fmt.Errorf("write vercel-sandbox stdout: %w", err)
			}
		case "stderr":
			appendBridgeExecOutput(&result.Stderr, frame.Data)
			if _, err := io.WriteString(stderr, frame.Data); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return execResult{}, fmt.Errorf("write vercel-sandbox stderr: %w", err)
			}
		case "result":
			result.ExitCode = frame.ExitCode
			sawResult = true
		case "":
			if frame.Stdout != "" {
				appendBridgeExecOutput(&result.Stdout, frame.Stdout)
				if _, err := io.WriteString(stdout, frame.Stdout); err != nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					return execResult{}, fmt.Errorf("write vercel-sandbox stdout: %w", err)
				}
			}
			if frame.Stderr != "" {
				appendBridgeExecOutput(&result.Stderr, frame.Stderr)
				if _, err := io.WriteString(stderr, frame.Stderr); err != nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					return execResult{}, fmt.Errorf("write vercel-sandbox stderr: %w", err)
				}
			}
			result.ExitCode = frame.ExitCode
			sawResult = true
		default:
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return execResult{}, fmt.Errorf("unsupported vercel-sandbox bridge exec frame %q", frame.Type)
		}
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		return execResult{}, fmt.Errorf("vercel-sandbox bridge exec failed: %s: %s", waitErr, redactSecrets(bridgeStderr.String()))
	}
	if !sawResult {
		return execResult{}, errors.New("vercel-sandbox bridge exec returned no result frame")
	}
	return result, nil
}

func marshalBridgeRequest(req bridgeRequest, payload []byte) ([]byte, error) {
	body := struct {
		Request bridgeRequest `json:"request"`
		Payload []byte        `json:"payload,omitempty"`
	}{Request: req, Payload: payload}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal vercel-sandbox bridge request: %w", err)
	}
	return buf, nil
}

func vercelSandboxBridgeEnv(base []string) []string {
	env := append([]string{}, base...)
	if token := firstEnv("CRABBOX_VERCEL_SANDBOX_AUTH_TOKEN", "CRABBOX_VERCEL_AUTH_TOKEN", "VERCEL_AUTH_TOKEN"); token != "" {
		env = setEnv(env, "VERCEL_AUTH_TOKEN", token)
	}
	if token := firstEnv("CRABBOX_VERCEL_SANDBOX_TOKEN", "CRABBOX_VERCEL_TOKEN", "VERCEL_TOKEN"); token != "" {
		env = setEnv(env, "VERCEL_TOKEN", token)
	}
	if token := firstEnv("CRABBOX_VERCEL_SANDBOX_OIDC_TOKEN", "VERCEL_OIDC_TOKEN"); token != "" {
		env = setEnv(env, "VERCEL_OIDC_TOKEN", token)
	}
	return env
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func redactSecrets(value string) string {
	redacted := value
	for _, env := range os.Environ() {
		key, secret, ok := strings.Cut(env, "=")
		if !ok || !strings.Contains(strings.ToLower(key), "token") || strings.TrimSpace(secret) == "" {
			continue
		}
		redacted = strings.ReplaceAll(redacted, secret, "[REDACTED]")
	}
	return redacted
}
