package cua

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	bridgeOutputLimit = 1 << 20
	bridgeVersion     = 1
)

//go:embed bridge_script.py
var bridgeScript string

type bridgeClient struct {
	cfg Config
	rt  Runtime
}

type bridgeRequest struct {
	Version   int          `json:"version"`
	Action    string       `json:"action"`
	Config    bridgeConfig `json:"config,omitempty"`
	SandboxID string       `json:"sandboxId,omitempty"`
}

type bridgeConfig struct {
	APIURL         string `json:"apiUrl,omitempty"`
	Image          string `json:"image,omitempty"`
	Kind           string `json:"kind,omitempty"`
	Region         string `json:"region,omitempty"`
	Workdir        string `json:"workdir,omitempty"`
	VCPUs          int    `json:"vcpus,omitempty"`
	MemoryMB       int    `json:"memoryMb,omitempty"`
	DiskGB         int    `json:"diskGb,omitempty"`
	StartupTimeout int    `json:"startupTimeoutSecs,omitempty"`
	ExecTimeout    int    `json:"execTimeoutSecs,omitempty"`
	SDKPackage     string `json:"sdkPackage,omitempty"`
	SDKImport      string `json:"sdkImport,omitempty"`
	FallbackImport string `json:"fallbackImport,omitempty"`
}

type bridgeResponse struct {
	OK        bool                   `json:"ok"`
	Class     string                 `json:"class,omitempty"`
	Sandbox   bridgeSandboxSummary   `json:"sandbox,omitempty"`
	Sandboxes []bridgeSandboxSummary `json:"sandboxes,omitempty"`
	Stdout    string                 `json:"stdout,omitempty"`
	Stderr    string                 `json:"stderr,omitempty"`
	Doctor    bridgeDoctor           `json:"doctor,omitempty"`
	Error     *bridgeError           `json:"error,omitempty"`
}

type bridgeSandboxSummary struct {
	ID       string            `json:"id,omitempty"`
	Name     string            `json:"name,omitempty"`
	Status   string            `json:"status,omitempty"`
	State    string            `json:"state,omitempty"`
	OSType   string            `json:"osType,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type bridgeDoctor struct {
	PythonVersion string            `json:"pythonVersion,omitempty"`
	ImportPath    string            `json:"importPath,omitempty"`
	SDKVersion    string            `json:"sdkVersion,omitempty"`
	Auth          string            `json:"auth,omitempty"`
	BaseURL       string            `json:"baseUrl,omitempty"`
	Checks        []bridgeCheck     `json:"checks,omitempty"`
	Details       map[string]string `json:"details,omitempty"`
}

type bridgeCheck struct {
	Status  string            `json:"status"`
	Check   string            `json:"check"`
	Message string            `json:"message,omitempty"`
	Class   string            `json:"class,omitempty"`
	Details map[string]string `json:"details,omitempty"`
}

type bridgeError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Class   string `json:"class,omitempty"`
}

func newBridgeClient(cfg Config, rt Runtime) *bridgeClient {
	cfg.Provider = providerName
	return &bridgeClient{cfg: cfg, rt: rt}
}

func (c *bridgeClient) CreateSandbox(ctx context.Context, metadata map[string]string) (bridgeSandboxSummary, error) {
	return bridgeSandboxSummary{}, provisioningUnsupported()
}

func (c *bridgeClient) ListSandboxes(ctx context.Context) ([]bridgeSandboxSummary, error) {
	resp, err := c.RoundTrip(ctx, bridgeRequest{Action: "list"})
	if err != nil {
		return nil, err
	}
	if err := bridgeResponseError("list", resp); err != nil {
		return nil, err
	}
	return resp.Sandboxes, nil
}

func (c *bridgeClient) GetSandbox(ctx context.Context, sandboxID string) (bridgeSandboxSummary, error) {
	resp, err := c.RoundTrip(ctx, bridgeRequest{Action: "info", SandboxID: sandboxID})
	if err != nil {
		return bridgeSandboxSummary{}, err
	}
	if err := bridgeResponseError("info", resp); err != nil {
		return bridgeSandboxSummary{}, err
	}
	return resp.Sandbox, nil
}

type bridgeActionError struct {
	action string
	code   string
	class  string
	msg    string
}

func (e *bridgeActionError) Error() string {
	parts := []string{"provider=cua", "bridge", e.action}
	if e.code != "" {
		parts = append(parts, "code="+e.code)
	}
	if e.class != "" {
		parts = append(parts, "class="+e.class)
	}
	if e.msg != "" {
		parts = append(parts, e.msg)
	}
	return redactSecrets(strings.Join(parts, " "))
}

func bridgeResponseError(action string, resp bridgeResponse) error {
	if resp.OK {
		return nil
	}
	if resp.Error == nil {
		return &bridgeActionError{action: action, class: strings.TrimSpace(resp.Class), msg: "operation failed"}
	}
	return &bridgeActionError{
		action: action,
		code:   strings.TrimSpace(resp.Error.Code),
		class:  strings.TrimSpace(blank(resp.Error.Class, resp.Class)),
		msg:    strings.TrimSpace(resp.Error.Message),
	}
}

func isCUANotFound(err error) bool {
	var actionErr *bridgeActionError
	if !errors.As(err, &actionErr) {
		return false
	}
	class := strings.ToLower(strings.TrimSpace(actionErr.class))
	code := strings.ToLower(strings.TrimSpace(actionErr.code))
	return class == "not_found" || code == "not_found" || code == "notfound" || code == "notfounderror" || code == "sandboxnotfounderror"
}

func (c *bridgeClient) RoundTrip(ctx context.Context, req bridgeRequest) (bridgeResponse, error) {
	if req.Action == "create" {
		return bridgeResponse{}, provisioningUnsupported()
	}
	if req.Action == "delete" {
		return bridgeResponse{}, mutationUnsupported()
	}
	if c.rt.Exec == nil {
		return bridgeResponse{}, exit(2, "provider=cua bridge requires Runtime.Exec")
	}
	req.Version = bridgeVersion
	req.Config = bridgeConfigForConfig(c.cfg)
	payload, err := json.Marshal(req)
	if err != nil {
		return bridgeResponse{}, err
	}
	timeout := bridgeTimeout(c.cfg, req)
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var stdout, stderr bytes.Buffer
	dir, err := bridgeWorkingDir()
	if err != nil {
		return bridgeResponse{}, err
	}
	defer os.RemoveAll(dir)
	result, runErr := c.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:                   bridgeCommand(c.cfg),
		Args:                   []string{"-I", "-c", bridgeScript},
		Env:                    bridgeEnv(c.cfg, dir),
		Dir:                    dir,
		Stdin:                  bytes.NewReader(payload),
		Stdout:                 &stdout,
		Stderr:                 &stderr,
		MaxCapturedOutputBytes: bridgeOutputLimit,
		CancelGracePeriod:      2 * time.Second,
	})
	if runErr != nil || result.ExitCode != 0 {
		return bridgeResponse{}, bridgeCommandError(result.ExitCode, stdout.String(), stderr.String(), runErr)
	}
	data := bytes.TrimSpace(stdout.Bytes())
	if len(data) == 0 {
		return bridgeResponse{}, fmt.Errorf("provider=cua bridge returned empty JSON output")
	}
	var resp bridgeResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return bridgeResponse{}, fmt.Errorf("decode provider=cua bridge JSON: %w", err)
	}
	resp = redactBridgeResponse(resp)
	return resp, nil
}

func bridgeConfigForConfig(cfg Config) bridgeConfig {
	apiURL, _ := cuaAPIURL(cfg)
	workdir, _ := cuaWorkdir(cfg)
	return bridgeConfig{
		APIURL:         apiURL,
		Image:          strings.TrimSpace(blank(cfg.Cua.Image, defaultImage)),
		Kind:           strings.ToLower(strings.TrimSpace(blank(cfg.Cua.Kind, defaultKind))),
		Region:         strings.TrimSpace(cfg.Cua.Region),
		Workdir:        workdir,
		VCPUs:          cfg.Cua.VCPUs,
		MemoryMB:       cfg.Cua.MemoryMB,
		DiskGB:         cfg.Cua.DiskGB,
		StartupTimeout: cfg.Cua.StartupTimeoutSecs,
		ExecTimeout:    cfg.Cua.ExecTimeoutSecs,
		SDKPackage:     strings.TrimSpace(blank(cfg.Cua.SDKPackage, defaultSDKPackage)),
		SDKImport:      strings.TrimSpace(blank(cfg.Cua.SDKImport, defaultSDKImport)),
		FallbackImport: strings.TrimSpace(blank(cfg.Cua.SDKFallbackImport, defaultSDKFallbackImport)),
	}
}

func bridgeCommand(cfg Config) string {
	return strings.TrimSpace(blank(cfg.Cua.BridgeCommand, defaultBridgeCommand))
}

func bridgeTimeout(cfg Config, req bridgeRequest) time.Duration {
	seconds := cfg.Cua.ExecTimeoutSecs
	switch req.Action {
	case "doctor":
		seconds = 15
	}
	if seconds <= 0 {
		seconds = 600
	}
	return time.Duration(seconds) * time.Second
}

func bridgeWorkingDir() (string, error) {
	dir, err := os.MkdirTemp("", "crabbox-cua-bridge-")
	if err != nil {
		return "", fmt.Errorf("create provider=cua bridge working directory: %w", err)
	}
	return dir, nil
}

func bridgeEnv(cfg Config, home string) []string {
	env := make([]string, 0, 28)
	for _, key := range []string{
		"PATH", "TMPDIR", "TEMP", "TMP", "SystemRoot", "SYSTEMROOT", "COMSPEC", "PATHEXT",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "all_proxy", "no_proxy",
		"SSL_CERT_FILE", "SSL_CERT_DIR",
	} {
		if value := os.Getenv(key); value != "" {
			env = upsertEnv(env, key, value)
		}
	}
	env = upsertEnv(env, "HOME", home)
	env = upsertEnv(env, "USERPROFILE", home)
	env = upsertEnv(env, "CUA_TELEMETRY_ENABLED", "false")
	if key := cuaAPIKey(); key != "" {
		env = upsertEnv(env, "CUA_API_KEY", key)
	}
	if apiURL, err := cuaAPIURL(cfg); err == nil && apiURL != "" {
		// Pinned cua-sandbox v0.1.17 reads CUA_BASE_URL in _config.get_base_url;
		// the cua package re-exports _config.configure for the same override.
		// https://github.com/trycua/cua/blob/sandbox-v0.1.17/libs/python/cua-sandbox/cua_sandbox/_config.py
		env = upsertEnv(env, "CUA_BASE_URL", apiURL)
	}
	env = upsertEnv(env, "CRABBOX_CUA_SDK_PACKAGE", strings.TrimSpace(blank(cfg.Cua.SDKPackage, defaultSDKPackage)))
	env = upsertEnv(env, "CRABBOX_CUA_SDK_IMPORT", strings.TrimSpace(blank(cfg.Cua.SDKImport, defaultSDKImport)))
	env = upsertEnv(env, "CRABBOX_CUA_SDK_FALLBACK_IMPORT", strings.TrimSpace(blank(cfg.Cua.SDKFallbackImport, defaultSDKFallbackImport)))
	return env
}

func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func bridgeCommandError(exitCode int, stdout, stderr string, runErr error) error {
	parts := []string{}
	if runErr != nil {
		parts = append(parts, runErr.Error())
	}
	if exitCode != 0 {
		parts = append(parts, fmt.Sprintf("exit=%d", exitCode))
	}
	if text := strings.TrimSpace(stdout); text != "" {
		parts = append(parts, "stdout="+limitErrorText(redactSecrets(text)))
	}
	if text := strings.TrimSpace(stderr); text != "" {
		parts = append(parts, "stderr="+limitErrorText(redactSecrets(text)))
	}
	return fmt.Errorf("provider=cua bridge failed: %s", redactSecrets(strings.Join(parts, " ")))
}

func redactBridgeResponse(resp bridgeResponse) bridgeResponse {
	resp.Stdout = redactSecrets(resp.Stdout)
	resp.Stderr = redactSecrets(resp.Stderr)
	if resp.Error != nil {
		resp.Error.Message = redactSecrets(resp.Error.Message)
	}
	for i := range resp.Doctor.Checks {
		resp.Doctor.Checks[i].Message = redactSecrets(resp.Doctor.Checks[i].Message)
	}
	return resp
}

func redactSecrets(value string) string {
	out := value
	for _, secret := range []string{
		os.Getenv("CRABBOX_CUA_API_KEY"), os.Getenv("CUA_API_KEY"),
		os.Getenv("HTTP_PROXY"), os.Getenv("HTTPS_PROXY"), os.Getenv("ALL_PROXY"),
		os.Getenv("http_proxy"), os.Getenv("https_proxy"), os.Getenv("all_proxy"),
	} {
		if secret = strings.TrimSpace(secret); secret != "" {
			out = strings.ReplaceAll(out, secret, "[redacted]")
		}
	}
	return out
}

func limitErrorText(value string) string {
	value = strings.TrimSpace(value)
	const limit = 512
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "...[truncated]"
}

func hashScope(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}
