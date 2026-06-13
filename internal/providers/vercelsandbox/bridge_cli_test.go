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
	for _, want := range []string{"opts.networkPolicy = policy", "opts.ports = ports.map", "cannot update sandbox metadata after creation", "process.exit(2)"} {
		if !strings.Contains(script, want) {
			t.Fatalf("bridge script missing %q", want)
		}
	}
}
