package cua

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	Version   int                 `json:"version"`
	Action    string              `json:"action"`
	Config    bridgeConfig        `json:"config,omitempty"`
	SandboxID string              `json:"sandboxId,omitempty"`
	Sandbox   bridgeSandboxConfig `json:"sandbox,omitempty"`
	Command   []string            `json:"command,omitempty"`
	Files     []bridgeFile        `json:"files,omitempty"`
	Timeout   int                 `json:"timeout,omitempty"`
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
	SDKPackage     string `json:"sdkPackage,omitempty"`
	SDKImport      string `json:"sdkImport,omitempty"`
	FallbackImport string `json:"fallbackImport,omitempty"`
}

type bridgeSandboxConfig struct {
	Name string            `json:"name,omitempty"`
	ID   string            `json:"id,omitempty"`
	Meta map[string]string `json:"metadata,omitempty"`
}

type bridgeFile struct {
	Path          string `json:"path,omitempty"`
	ContentBase64 string `json:"contentBase64,omitempty"`
}

type bridgeResponse struct {
	OK        bool                   `json:"ok"`
	Class     string                 `json:"class,omitempty"`
	Sandbox   bridgeSandboxSummary   `json:"sandbox,omitempty"`
	Sandboxes []bridgeSandboxSummary `json:"sandboxes,omitempty"`
	Stdout    string                 `json:"stdout,omitempty"`
	Stderr    string                 `json:"stderr,omitempty"`
	ExitCode  int                    `json:"exitCode,omitempty"`
	Doctor    bridgeDoctor           `json:"doctor,omitempty"`
	Error     *bridgeError           `json:"error,omitempty"`
}

type bridgeSandboxSummary struct {
	ID       string            `json:"id,omitempty"`
	Name     string            `json:"name,omitempty"`
	Status   string            `json:"status,omitempty"`
	State    string            `json:"state,omitempty"`
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

func (c *bridgeClient) RoundTrip(ctx context.Context, req bridgeRequest) (bridgeResponse, error) {
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
	result, runErr := c.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:                   bridgeCommand(c.cfg),
		Args:                   []string{"-c", bridgeScript},
		Env:                    bridgeEnv(c.cfg),
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
	if req.Action == "doctor" {
		seconds = 15
	}
	if req.Timeout > 0 {
		seconds = req.Timeout
	}
	if seconds <= 0 {
		seconds = 600
	}
	return time.Duration(seconds) * time.Second
}

func bridgeWorkingDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "crabbox", "cua-bridge")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create provider=cua bridge working directory: %w", err)
	}
	return dir, nil
}

func bridgeEnv(cfg Config) []string {
	env := make([]string, 0, 12)
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "TEMP", "TMP", "SystemRoot", "SYSTEMROOT", "COMSPEC", "PATHEXT"} {
		if value := os.Getenv(key); value != "" {
			env = upsertEnv(env, key, value)
		}
	}
	if key := os.Getenv("CRABBOX_CUA_API_KEY"); key != "" {
		env = upsertEnv(env, "CUA_API_KEY", key)
	} else if key := os.Getenv("CUA_API_KEY"); key != "" {
		env = upsertEnv(env, "CUA_API_KEY", key)
	}
	if apiURL, err := cuaAPIURL(cfg); err == nil && apiURL != "" {
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
		parts = append(parts, "stdout="+limitErrorText(text))
	}
	if text := strings.TrimSpace(stderr); text != "" {
		parts = append(parts, "stderr="+limitErrorText(text))
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
	for _, secret := range []string{os.Getenv("CRABBOX_CUA_API_KEY"), os.Getenv("CUA_API_KEY")} {
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
