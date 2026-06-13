package vercelsandbox

import (
	"strings"
	"testing"
)

func TestBridgeScriptUsesExecTimeoutAbortSignal(t *testing.T) {
	script := vercelSandboxBridgeScript()
	for _, want := range []string{"AbortController", "execReq.timeoutSecs * 1000", "signal: controller?.signal"} {
		if !strings.Contains(script, want) {
			t.Fatalf("bridge script missing %q", want)
		}
	}
}

func TestBridgeScriptPassesNetworkAndFailsClosedOnMetadataUpdate(t *testing.T) {
	script := vercelSandboxBridgeScript()
	for _, want := range []string{"opts.networkPolicy = policy", "expandPortSpecs", "opts.ports = ports", "cannot update sandbox metadata after creation", "process.exit(2)"} {
		if !strings.Contains(script, want) {
			t.Fatalf("bridge script missing %q", want)
		}
	}
}

func TestBridgeScriptBoundsCapturedCommandOutput(t *testing.T) {
	script := vercelSandboxBridgeScript()
	for _, want := range []string{"captureLimitBytes = 4 * 1024 * 1024", "truncated after", "stdoutCapture.value()", "stderrCapture.value()"} {
		if !strings.Contains(script, want) {
			t.Fatalf("bridge script missing %q", want)
		}
	}
}

func TestBridgeScriptSummarizesActualSandboxMetadata(t *testing.T) {
	script := vercelSandboxBridgeScript()
	for _, want := range []string{"metadata: sandbox?.tags || sandbox?.metadata || metadata || {}", "JSON.stringify(summary(sandbox))"} {
		if !strings.Contains(script, want) {
			t.Fatalf("bridge script missing %q", want)
		}
	}
	if strings.Contains(script, "summary(sandbox, create.metadata") {
		t.Fatalf("bridge script echoes requested create metadata")
	}
}
