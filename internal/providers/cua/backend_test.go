package cua

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

type bridgeHarness struct {
	t       *testing.T
	actions []string
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
	case "list":
		for _, sb := range h.infos {
			resp.Sandboxes = append(resp.Sandboxes, sb)
		}
	case "info":
		sb, ok := h.infos[payload.SandboxID]
		if !ok {
			resp = bridgeResponse{OK: false, Class: "not_found", Error: &bridgeError{Code: "not_found", Class: "not_found", Message: "sandbox not found"}}
		} else {
			resp.Sandbox = sb
		}
	default:
		h.t.Fatalf("unexpected mutating bridge action %q", payload.Action)
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

func seedClaimedSandbox(t *testing.T, h *bridgeHarness, b backend, slug string) LeaseClaim {
	t.Helper()
	scope, err := cuaScope(b.cfg)
	if err != nil {
		t.Fatal(err)
	}
	leaseID := leasePrefix + strings.ReplaceAll(slug, "-", "")
	sandboxID := "existing-" + slug
	createdAt := "2026-07-03T12:00:00Z"
	claim := LeaseClaim{
		LeaseID:       leaseID,
		Slug:          slug,
		Provider:      providerName,
		CloudID:       sandboxID,
		ProviderScope: scope,
		LastUsedAt:    time.Now().UTC().Format(time.RFC3339),
		Labels:        claimLabels(b.cfg, sandboxID, createdAt, false),
	}
	writeClaim(t, claim)
	h.infos[sandboxID] = bridgeSandboxSummary{ID: sandboxID, Name: sandboxID, Status: "running", Metadata: map[string]string{"createdAt": createdAt}}
	return claim
}

func TestWarmupAndEveryRunFailClosedWithoutBridge(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	h := newBridgeHarness(t)
	b := testBackend(t, h)
	err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}})
	if err == nil || !strings.Contains(err.Error(), "idempotency key") || !strings.Contains(err.Error(), cuaTrackingIssue) {
		t.Fatalf("Warmup err=%v, want actionable provisioning guard", err)
	}
	for _, req := range []RunRequest{
		{Repo: Repo{Root: t.TempDir(), Name: "demo"}, NoSync: true, Command: []string{"echo", "hello"}},
		{ID: "existing-claim", Command: []string{"true"}},
	} {
		if _, err := b.Run(context.Background(), req); err == nil || !strings.Contains(err.Error(), cuaTrackingIssue) {
			t.Fatalf("Run err=%v, want tracked fail-closed guard", err)
		}
	}
	if len(h.actions) != 0 {
		t.Fatalf("lifecycle guard reached bridge: %#v", h.actions)
	}
}

func TestStopAndCleanupFailClosedWithoutBridge(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	h := newBridgeHarness(t)
	b := testBackend(t, h)
	for name, err := range map[string]error{
		"stop":    b.Stop(context.Background(), StopRequest{ID: "existing"}),
		"cleanup": b.Cleanup(context.Background(), CleanupRequest{}),
	} {
		if err == nil || !strings.Contains(err.Error(), "read-only") || !strings.Contains(err.Error(), "atomically") || !strings.Contains(err.Error(), cuaTrackingIssue) {
			t.Fatalf("%s err=%v, want actionable mutation guard", name, err)
		}
	}
	if len(h.actions) != 0 {
		t.Fatalf("mutation guard reached bridge: %#v", h.actions)
	}
}

func TestListAndStatusInspectUnclaimedExistingSandboxes(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	h := newBridgeHarness(t)
	h.infos["existing-unclaimed"] = bridgeSandboxSummary{ID: "existing-unclaimed", Name: "existing-unclaimed", Status: "running", OSType: "windows", Metadata: map[string]string{"createdAt": "2026-07-03T12:00:00Z"}}
	b := testBackend(t, h)
	views, err := b.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 1 || views[0].CloudID != "existing-unclaimed" || views[0].Labels["claimed"] != "false" || views[0].Labels["experimental"] != "true" || views[0].Labels["target"] != "windows" {
		t.Fatalf("views=%#v", views)
	}
	view, err := b.Status(context.Background(), StatusRequest{ID: "existing-unclaimed"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if view.ServerID != "existing-unclaimed" || !view.Ready || view.Labels["claimed"] != "false" || view.TargetOS != "windows" {
		t.Fatalf("view=%#v", view)
	}
}

func TestListDoesNotClaimNameReusedSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	h := newBridgeHarness(t)
	b := testBackend(t, h)
	claim := seedClaimedSandbox(t, h, b, "reused-name")
	sandboxID := claimSandboxName(claim)
	h.infos[sandboxID] = bridgeSandboxSummary{ID: sandboxID, Name: sandboxID, Status: "running", OSType: "linux", Metadata: map[string]string{"createdAt": "2026-07-17T12:00:00Z"}}
	views, err := b.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 1 || views[0].Labels["claimed"] != "false" || views[0].Labels["claim_state"] != "identity-mismatch" || views[0].Labels["lease"] != "" {
		t.Fatalf("views=%#v", views)
	}
}
