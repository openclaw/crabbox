package codesandbox

import (
	"io"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName         = "codesandbox"
	providerFamily       = "codesandbox"
	leasePrefix          = "csbx_"
	defaultWorkdir       = "/project/workspace"
	defaultBridgeCommand = "node"
	defaultSDKPackage    = "@codesandbox/sdk@2.4.2"
	targetLinux          = core.TargetLinux
	NetworkPublic        = core.NetworkPublic

	codesandboxPrimaryAPIKeyEnv  = "CRABBOX_CODESANDBOX_API_KEY"
	codesandboxFallbackAPIKeyEnv = "CSB_API_KEY"
)

func listCodeSandboxLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}

func codeSandboxCleanupCommand(leaseID string) string {
	return "crabbox stop --provider " + providerName + " " + core.ShellQuote(leaseID)
}

func operationTimeout(cfg core.CodeSandboxConfig) time.Duration {
	seconds := cfg.OperationTimeoutSecs
	if seconds <= 0 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}

func bridgeCommand(cfg core.CodeSandboxConfig) string {
	if command := strings.TrimSpace(cfg.BridgeCommand); command != "" {
		return command
	}
	return defaultBridgeCommand
}

func sdkPackage(cfg core.CodeSandboxConfig) string {
	if pkg := strings.TrimSpace(cfg.SDKPackage); pkg != "" {
		return pkg
	}
	return defaultSDKPackage
}

func doctorListLimit(cfg core.CodeSandboxConfig) int {
	if cfg.DoctorListLimit <= 0 {
		return 1
	}
	return cfg.DoctorListLimit
}

func authFromEnv() (string, string, bool) {
	if token := strings.TrimSpace(os.Getenv(codesandboxPrimaryAPIKeyEnv)); token != "" {
		return token, codesandboxPrimaryAPIKeyEnv, true
	}
	if token := strings.TrimSpace(os.Getenv(codesandboxFallbackAPIKeyEnv)); token != "" {
		return token, codesandboxFallbackAPIKeyEnv, true
	}
	return "", "", false
}

func redactToken(text, token string) string {
	if token = strings.TrimSpace(token); token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "[redacted]")
}

func doctorCheck(name string, err error, details map[string]string) core.DoctorCheck {
	if err != nil {
		return core.DoctorCheck{Status: "error", Check: name, Message: err.Error(), Details: details}
	}
	return core.DoctorCheck{Status: "ok", Check: name, Message: "ready", Details: details}
}

func discardRuntime() core.Runtime {
	return core.Runtime{Stdout: io.Discard, Stderr: io.Discard}
}
