package codesandbox

import (
	"context"
	"flag"
	"io"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type CodeSandboxConfig = core.CodeSandboxConfig
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type DoctorCheck = core.DoctorCheck
type WarmupRequest = core.WarmupRequest
type RunRequest = core.RunRequest
type RunResult = core.RunResult
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult

const (
	providerName         = "codesandbox"
	providerFamily       = "codesandbox"
	leasePrefix          = "csbx_"
	defaultWorkdir       = "/project/workspace"
	defaultBridgeCommand = "node"
	defaultSDKPackage    = "@codesandbox/sdk"
	targetLinux          = core.TargetLinux

	codesandboxPrimaryAPIKeyEnv  = "CRABBOX_CODESANDBOX_API_KEY"
	codesandboxFallbackAPIKeyEnv = "CSB_API_KEY"
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func inventoryDoctorResult(provider string, leases int) DoctorResult {
	return core.InventoryDoctorResult(provider, leases)
}

func operationTimeout(cfg CodeSandboxConfig) time.Duration {
	seconds := cfg.OperationTimeoutSecs
	if seconds <= 0 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}

func bridgeCommand(cfg CodeSandboxConfig) string {
	if command := strings.TrimSpace(cfg.BridgeCommand); command != "" {
		return command
	}
	return defaultBridgeCommand
}

func sdkPackage(cfg CodeSandboxConfig) string {
	if pkg := strings.TrimSpace(cfg.SDKPackage); pkg != "" {
		return pkg
	}
	return defaultSDKPackage
}

func doctorListLimit(cfg CodeSandboxConfig) int {
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

func doctorCheck(name string, err error, details map[string]string) DoctorCheck {
	if err != nil {
		return DoctorCheck{Status: "error", Check: name, Message: err.Error(), Details: details}
	}
	return DoctorCheck{Status: "ok", Check: name, Message: "ready", Details: details}
}

func discardRuntime() Runtime {
	return Runtime{Stdout: io.Discard, Stderr: io.Discard}
}

func withOperationTimeout(ctx context.Context, cfg CodeSandboxConfig) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, operationTimeout(cfg))
}
