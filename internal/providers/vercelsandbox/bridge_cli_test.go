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
	for _, want := range []string{
		"opts.networkPolicy = policy",
		"policy.allow = ['*']",
		"policy.subnets.deny = denyCIDRs",
		"String(entry).trim()",
		"value + '/32'",
		"value + '/128'",
		"expandPortSpecs",
		"opts.ports = ports",
		"cannot update sandbox metadata after creation",
		"process.exit(2)",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("bridge script missing %q", want)
		}
	}
	if strings.Contains(script, "policy.deny") {
		t.Fatal("bridge script emits unsupported domain deny policy")
	}
}

func TestBridgeScriptUsesOfficialSnapshotSourceShape(t *testing.T) {
	script := vercelSandboxBridgeScript()
	for _, want := range []string{"...(!snapshotId && { runtime:", "opts.source = { type: 'snapshot', snapshotId }"} {
		if !strings.Contains(script, want) {
			t.Fatalf("bridge script missing %q", want)
		}
	}
}

func TestBridgeScriptChecksProjectScopeReadOnly(t *testing.T) {
	script := vercelSandboxBridgeScript()
	for _, want := range []string{"case 'check-project':", "sandboxOptions({ sortBy: 'name' }, true)"} {
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
		"linkedProject(process.cwd())",
		"JSON.parse(fs.readFileSync(path.join(current, '.vercel', 'project.json'), 'utf8'))",
		"projectId: value.projectId, teamId: value.orgId",
		"fs.mkdtempSync(path.join(os.tmpdir(), 'crabbox-vercel-scope-'))",
		"project scope cannot be validated read-only",
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
