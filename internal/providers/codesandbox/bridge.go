package codesandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type BridgeRequest struct {
	Operation string `json:"operation"`
	Limit     int    `json:"limit,omitempty"`
}

type BridgeResponse struct {
	OK         bool             `json:"ok"`
	Sandboxes  []SandboxSummary `json:"sandboxes,omitempty"`
	TotalCount int              `json:"totalCount,omitempty"`
	Error      *BridgeError     `json:"error,omitempty"`
}

type BridgeError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

type SDKBridge struct {
	cfg CodeSandboxConfig
	rt  Runtime
}

func NewSDKBridge(cfg CodeSandboxConfig, rt Runtime) *SDKBridge {
	return &SDKBridge{cfg: cfg, rt: rt}
}

func (b *SDKBridge) RoundTrip(ctx context.Context, token string, req BridgeRequest) (BridgeResponse, error) {
	if b.rt.Exec == nil {
		return BridgeResponse{}, exit(2, "codesandbox bridge requires Runtime.Exec")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return BridgeResponse{}, err
	}
	ctx, cancel := withOperationTimeout(ctx, b.cfg)
	defer cancel()
	var stdout, stderr bytes.Buffer
	result, runErr := b.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:                   bridgeCommand(b.cfg),
		Args:                   []string{"--input-type=module", "-e", codeSandboxBridgeScript},
		Env:                    bridgeEnv(b.cfg, token),
		Stdin:                  bytes.NewReader(payload),
		Stdout:                 &stdout,
		Stderr:                 &stderr,
		MaxCapturedOutputBytes: 1 << 20,
		CancelGracePeriod:      2 * time.Second,
	})
	if runErr != nil {
		return BridgeResponse{}, bridgeCommandError(result.ExitCode, stdout.String(), stderr.String(), token, runErr)
	}
	if result.ExitCode != 0 {
		return BridgeResponse{}, bridgeCommandError(result.ExitCode, stdout.String(), stderr.String(), token, nil)
	}
	data := bytes.TrimSpace(stdout.Bytes())
	if len(data) == 0 {
		return BridgeResponse{}, fmt.Errorf("codesandbox bridge returned empty JSON output")
	}
	var resp BridgeResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return BridgeResponse{}, fmt.Errorf("decode codesandbox bridge JSON: %w", err)
	}
	if !resp.OK {
		if resp.Error == nil {
			return BridgeResponse{}, fmt.Errorf("codesandbox bridge failed without error details")
		}
		return BridgeResponse{}, fmt.Errorf("codesandbox bridge %s: %s", blank(resp.Error.Code, "error"), redactToken(resp.Error.Message, token))
	}
	return resp, nil
}

func bridgeEnv(cfg CodeSandboxConfig, token string) []string {
	env := append([]string{}, os.Environ()...)
	env = upsertEnv(env, codesandboxFallbackAPIKeyEnv, token)
	env = upsertEnv(env, "CRABBOX_CODESANDBOX_SDK_PACKAGE", sdkPackage(cfg))
	return env
}

func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func bridgeCommandError(exitCode int, stdout, stderr, token string, err error) error {
	message := strings.TrimSpace(stderr)
	if message == "" {
		message = strings.TrimSpace(stdout)
	}
	if message == "" && err != nil {
		message = err.Error()
	}
	if message == "" {
		message = "no output"
	}
	message = redactToken(message, token)
	if err != nil {
		return fmt.Errorf("codesandbox bridge failed exit=%d: %s: %w", exitCode, message, err)
	}
	return fmt.Errorf("codesandbox bridge failed exit=%d: %s", exitCode, message)
}

const codeSandboxBridgeScript = `
const chunks = [];
for await (const chunk of process.stdin) chunks.push(chunk);
const input = Buffer.concat(chunks).toString("utf8").trim();
const req = input ? JSON.parse(input) : {};
function emit(value) {
  process.stdout.write(JSON.stringify(value));
}
function fail(code, message) {
  emit({ ok: false, error: { code, message: String(message || "") } });
}
try {
  const token = process.env.CSB_API_KEY || process.env.CRABBOX_CODESANDBOX_API_KEY || "";
  if (!token) {
    fail("auth_missing", "missing CodeSandbox API key");
  } else if (req.operation !== "list_sandboxes") {
    fail("unsupported_operation", "unsupported bridge operation: " + String(req.operation || ""));
  } else {
    const pkg = process.env.CRABBOX_CODESANDBOX_SDK_PACKAGE || "@codesandbox/sdk";
    const { CodeSandbox } = await import(pkg);
    const sdk = new CodeSandbox(token);
    const listed = await sdk.sandboxes.list({ limit: Number(req.limit || 1) });
    const sandboxes = (listed.sandboxes || []).map((sandbox) => ({
      id: sandbox.id || "",
      title: sandbox.title || "",
      privacy: sandbox.privacy || "",
      tags: sandbox.tags || []
    }));
    emit({ ok: true, sandboxes, totalCount: listed.totalCount || sandboxes.length });
  }
} catch (err) {
  fail(err && err.code ? err.code : "sdk_error", err && err.message ? err.message : err);
}
`

func blank(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
