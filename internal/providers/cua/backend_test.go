package cua

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type bridgeHarness struct {
	t       *testing.T
	actions []string
	execs   [][]string
	deletes []string
	infos   map[string]bridgeSandboxSummary
}

func newBridgeHarness(t *testing.T) *bridgeHarness {
	t.Helper()
	return &bridgeHarness{t: t, infos: make(map[string]bridgeSandboxSummary)}
}

func (h *bridgeHarness) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	var payload bridgeRequest
	data, err := io.ReadAll(req.Stdin)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		h.t.Fatalf("decode bridge request: %v", err)
	}
	h.actions = append(h.actions, payload.Action)
	resp := bridgeResponse{OK: true}
	switch payload.Action {
	case "create":
		name := payload.Sandbox.Name
		resp.Sandbox = bridgeSandboxSummary{ID: name, Name: name, Status: "running", Metadata: payload.Sandbox.Meta}
		h.infos[name] = resp.Sandbox
	case "info":
		sb, ok := h.infos[payload.SandboxID]
		if !ok {
			resp = bridgeResponse{OK: false, Class: "not_found", Error: &bridgeError{Code: "not_found", Class: "not_found", Message: "sandbox not found"}}
		} else {
			resp.Sandbox = sb
		}
	case "delete":
		h.deletes = append(h.deletes, payload.SandboxID)
		delete(h.infos, payload.SandboxID)
	case "exec":
		h.execs = append(h.execs, payload.Command)
		if len(payload.Command) > 0 && strings.Contains(strings.Join(payload.Command, " "), "fail") {
			resp.ExitCode = 7
			resp.Stderr = "boom\n"
		} else {
			resp.ExitCode = 0
			resp.Stdout = "ok\n"
		}
	case "upload_bytes":
		if len(payload.Files) != 1 || payload.Files[0].Path == "" {
			resp = bridgeResponse{OK: false, Class: "validation_failed", Error: &bridgeError{Code: "bad_upload", Class: "validation_failed", Message: "bad upload"}}
		}
	default:
		resp = bridgeResponse{OK: false, Class: "validation_failed", Error: &bridgeError{Code: "unknown_action", Class: "validation_failed", Message: payload.Action}}
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		h.t.Fatal(err)
	}
	_, _ = req.Stdout.Write(encoded)
	return LocalCommandResult{ExitCode: 0}, nil
}

func testBackend(t *testing.T, harness *bridgeHarness) backend {
	t.Helper()
	return backend{
		spec: Provider{}.Spec(),
		cfg:  testConfig(),
		rt: Runtime{
			Exec:   harness,
			Stdout: io.Discard,
			Stderr: io.Discard,
		},
	}
}

func TestRunFreshDeletesSandboxByDefault(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	h := newBridgeHarness(t)
	b := testBackend(t, h)
	result, err := b.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: t.TempDir(), Name: "demo"},
		NoSync:  true,
		Command: []string{"echo", "hello"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Provider != providerName || result.ExitCode != 0 || !result.SyncDelegated {
		t.Fatalf("result=%#v", result)
	}
	if len(h.deletes) != 1 || !strings.HasPrefix(h.deletes[0], sandboxNamePrefix) {
		t.Fatalf("deletes=%#v", h.deletes)
	}
	if claim, err := readLeaseClaim(result.LeaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim should be removed after fresh run, claim=%#v err=%v", claim, err)
	}
}

func TestRunReuseUsesClaimedSandboxAndKeepsClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	h := newBridgeHarness(t)
	b := testBackend(t, h)
	var stdout bytes.Buffer
	b.rt.Stdout = &stdout
	repoRoot := t.TempDir()
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: repoRoot, Name: "demo"}, RequestedSlug: "reuse"}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	if len(h.deletes) != 0 {
		t.Fatalf("warmup must retain sandbox, deletes=%#v", h.deletes)
	}
	if _, err := b.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: repoRoot, Name: "demo"},
		ID:      "reuse",
		NoSync:  true,
		Command: []string{"true"},
	}); err != nil {
		t.Fatalf("reuse Run: %v", err)
	}
	if len(h.deletes) != 0 {
		t.Fatalf("reuse run should not delete retained sandbox, deletes=%#v", h.deletes)
	}
	claim, ok, err := resolveCUALeaseClaim("reuse", b.cfg)
	if err != nil || !ok {
		t.Fatalf("resolve claim after reuse: ok=%v err=%v", ok, err)
	}
	if claim.LeaseID == "" || claimSandboxName(claim) == "" {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestStopRejectsUnownedSandboxBeforeDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	scope, err := cuaScope(cfg)
	if err != nil {
		t.Fatal(err)
	}
	writeClaim(t, core.LeaseClaim{
		LeaseID:       "cuabx_manual",
		Slug:          "manual",
		Provider:      providerName,
		CloudID:       "manual-sandbox",
		ProviderScope: scope,
		Labels:        claimLabels(cfg, "manual-sandbox", false),
	})
	h := newBridgeHarness(t)
	h.infos["manual-sandbox"] = bridgeSandboxSummary{ID: "manual-sandbox", Name: "manual-sandbox", Status: "running"}
	b := testBackend(t, h)
	err = b.Stop(context.Background(), StopRequest{ID: "manual"})
	if err == nil {
		t.Fatal("expected ownership rejection")
	}
	if len(h.deletes) != 0 {
		t.Fatalf("delete should not run for unowned sandbox, deletes=%#v", h.deletes)
	}
}

func TestCleanupDryRunOnlyTargetsExpiredClaims(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	scope, err := cuaScope(cfg)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	writeClaim(t, core.LeaseClaim{
		LeaseID:            "cuabx_expired",
		Slug:               "expired",
		Provider:           providerName,
		CloudID:            "crabbox-cua-expired-aaaaaa",
		ProviderScope:      scope,
		LastUsedAt:         old,
		IdleTimeoutSeconds: 60,
		Labels:             claimLabels(cfg, "crabbox-cua-expired-aaaaaa", false),
	})
	h := newBridgeHarness(t)
	h.infos["crabbox-cua-expired-aaaaaa"] = bridgeSandboxSummary{ID: "crabbox-cua-expired-aaaaaa", Name: "crabbox-cua-expired-aaaaaa", Status: "running"}
	var stdout bytes.Buffer
	b := testBackend(t, h)
	b.rt.Stdout = &stdout
	if err := b.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatalf("Cleanup dry-run: %v", err)
	}
	if len(h.deletes) != 0 {
		t.Fatalf("dry-run must not delete, deletes=%#v", h.deletes)
	}
	if !strings.Contains(stdout.String(), "would delete sandbox=crabbox-cua-expired-aaaaaa") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestCommandEnvStripsCUAAuth(t *testing.T) {
	env, stripped := commandEnv(map[string]string{
		"CUA_API_KEY":             "secret",
		"CRABBOX_CUA_API_KEY":     "secret",
		"CRABBOX_CUA_SMOKE_VALUE": "ok",
	})
	if env["CRABBOX_CUA_SMOKE_VALUE"] != "ok" {
		t.Fatalf("env=%#v", env)
	}
	if _, ok := env["CUA_API_KEY"]; ok {
		t.Fatalf("CUA_API_KEY should be stripped: %#v", env)
	}
	if len(stripped) != 2 {
		t.Fatalf("stripped=%#v", stripped)
	}
}

func TestSyncWorkspaceUploadsArchiveAndRunsExtraction(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "proof.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.email", "smoke@example.com")
	runGit(t, repo, "config", "user.name", "Crabbox Smoke")
	runGit(t, repo, "add", "proof.txt")
	runGit(t, repo, "commit", "-qm", "test: seed")
	h := newBridgeHarness(t)
	h.infos["crabbox-cua-sync-aaaaaa"] = bridgeSandboxSummary{ID: "crabbox-cua-sync-aaaaaa", Name: "crabbox-cua-sync-aaaaaa", Status: "running"}
	b := testBackend(t, h)
	phases, _, err := b.syncWorkspace(context.Background(), b.client(), "crabbox-cua-sync-aaaaaa", RunRequest{Repo: Repo{Root: repo}}, "/workspace/crabbox")
	if err != nil {
		t.Fatalf("syncWorkspace: %v", err)
	}
	if len(phases) == 0 {
		t.Fatal("expected timing phases")
	}
	if !containsAction(h.actions, "upload_bytes") || !containsAction(h.actions, "exec") {
		t.Fatalf("actions=%#v", h.actions)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func containsAction(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
