package codesandbox

import (
	"io"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	providerName   = "codesandbox"
	providerFamily = "codesandbox"
	leasePrefix    = "csbx_"
	// This SDK mount does not move when the configured workdir default changes.
	codeSandboxWorkspaceRoot = "/project/workspace"
	targetLinux              = core.TargetLinux
	NetworkPublic            = core.NetworkPublic

	codesandboxPrimaryAPIKeyEnv  = "CRABBOX_CODESANDBOX_API_KEY"
	codesandboxFallbackAPIKeyEnv = "CSB_API_KEY"
)

func listCodeSandboxLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}

func codeSandboxCleanupCommand(leaseID string) string {
	return "crabbox stop --provider " + providerName + " " + core.ShellQuote(leaseID)
}

func operationTimeout(cfg core.CodeSandboxConfig) (time.Duration, error) {
	seconds := cfg.OperationTimeoutSecs
	if seconds <= 0 {
		seconds = core.CodeSandboxConfigDefaultOperationTimeoutSecs
	}
	if timeout, ok := shared.SecondsWithGrace(int64(seconds), 0); ok {
		return timeout, nil
	}
	return 0, core.Exit(2, "codesandbox operation timeout exceeds the supported duration range")
}

func bridgeCommand(cfg core.CodeSandboxConfig) string {
	if command := strings.TrimSpace(cfg.BridgeCommand); command != "" {
		return command
	}
	return core.CodeSandboxConfigDefaultBridgeCommand
}

func sdkPackage(cfg core.CodeSandboxConfig) string {
	if pkg := strings.TrimSpace(cfg.SDKPackage); pkg != "" {
		return pkg
	}
	return core.CodeSandboxConfigDefaultSDKPackage
}

func doctorListLimit(cfg core.CodeSandboxConfig) int {
	if cfg.DoctorListLimit <= 0 {
		return core.CodeSandboxConfigDefaultDoctorListLimit
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
