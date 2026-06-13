package vercelsandbox

import (
	"strings"
	"testing"
)

func TestBridgeScriptUsesServiceExecTimeout(t *testing.T) {
	script := vercelSandboxBridgeScript()
	for _, want := range []string{"timeoutMs: execReq.timeoutSecs > 0 ? execReq.timeoutSecs * 1000 : undefined"} {
		if !strings.Contains(script, want) {
			t.Fatalf("bridge script missing %q", want)
		}
	}
	if strings.Contains(script, "AbortController") {
		t.Fatal("bridge script uses client-side timeout instead of the sandbox timeout")
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

func TestBridgeScriptStreamsCommandOutput(t *testing.T) {
	script := vercelSandboxBridgeScript()
	for _, want := range []string{"writeFrame(type, data", "frameStream('stdout')", "frameStream('stderr')", "writeFrame('result'"} {
		if !strings.Contains(script, want) {
			t.Fatalf("bridge script missing %q", want)
		}
	}
}

func TestBridgeScriptUsesOfficialAuthAndResumesMutationPaths(t *testing.T) {
	script := vercelSandboxBridgeScript()
	for _, want := range []string{
		"@vercel/sandbox/dist/auth/index.js",
		"let auth = authMod.getAuth()",
		"authMod.inferScope",
		"linkedProjectCwd(process.cwd())",
		"path.join(current, '.vercel', 'project.json')",
		"fs.mkdtempSync(path.join(os.tmpdir(), 'crabbox-vercel-scope-'))",
		"Vercel OIDC tokens are scoped by their claims",
		"cfg.teamId || (projectId ? cfg.scope || '' : '')",
		"projectId requires teamId or scope",
		"getSandbox(req.sandboxId, true)",
		"getSandbox(req.sandboxId)",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("bridge script missing %q", want)
		}
	}
	if strings.Contains(script, "out.scope = cfg.scope") {
		t.Fatal("bridge script passes unsupported partial scope credentials")
	}
}

func TestBridgeScriptRefreshesStoredLoginToken(t *testing.T) {
	script := vercelSandboxBridgeScript()
	for _, want := range []string{
		"auth?.refreshToken && auth.expiresAt",
		"authMod.OAuth()).refreshToken(auth.refreshToken)",
		"authMod.updateAuthConfig(auth)",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("bridge script missing %q", want)
		}
	}
}

func TestBridgeScriptBoundsStreamedCommandOutput(t *testing.T) {
	script := vercelSandboxBridgeScript()
	for _, want := range []string{"outputLimitBytes = 4 * 1024 * 1024", "truncated after", "if (truncated)", "callback()"} {
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
