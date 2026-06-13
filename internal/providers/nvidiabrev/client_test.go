package nvidiabrev

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNvidiaBrevParseWorkspaceJSONShapes(t *testing.T) {
	tests := []struct {
		name    string
		stdout  string
		wantLen int
		wantErr string
	}{
		{name: "empty stdout", stdout: "", wantLen: 0},
		{name: "null object", stdout: `{"workspaces": null}`, wantLen: 0},
		{name: "empty object array", stdout: `{"workspaces": []}`, wantLen: 0},
		{name: "populated object array", stdout: `{"workspaces": [{"id":"ws-1","name":"crabbox-demo-123","status":"RUNNING"}]}`, wantLen: 1},
		{name: "raw array", stdout: `[{"id":"ws-1"},{"id":"ws-2"}]`, wantLen: 2},
		{name: "missing field", stdout: `{"items":[]}`, wantErr: "missing workspaces field"},
		{name: "malformed", stdout: `{"workspaces":`, wantErr: "unexpected end"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBrevWorkspaces(tt.stdout)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tt.wantLen {
				t.Fatalf("len=%d want %d: %#v", len(got), tt.wantLen, got)
			}
		})
	}
}

func TestNvidiaBrevClientConstructsNonSecretCommands(t *testing.T) {
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
		{args: "create crabbox-demo-123456789abc --detached --type gpu-l40s --gpu-name L40S --provider aws --mode vm --launchable env-example --startup-script @setup.sh"},
		{args: "refresh"},
		{args: "stop ws-123"},
		{args: "delete ws-123"},
	}}
	cfg := Config{NvidiaBrev: NvidiaBrevConfig{
		CLI:           "brev",
		Type:          "gpu-l40s",
		GPUName:       "L40S",
		Provider:      "aws",
		Mode:          "vm",
		Launchable:    "env-example",
		StartupScript: "@setup.sh",
	}}
	client, err := newBrevClient(cfg, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.list(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := client.create(context.Background(), "crabbox-demo-123456789abc"); err != nil {
		t.Fatal(err)
	}
	if err := client.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.stop(context.Background(), "ws-123"); err != nil {
		t.Fatal(err)
	}
	if err := client.delete(context.Background(), "ws-123"); err != nil {
		t.Fatal(err)
	}
	assertNoNvidiaBrevSecretArgs(t, runner.calls)
}

func TestNvidiaBrevClientPreservesInlineStartupScript(t *testing.T) {
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "create crabbox-inline-123456789abc --detached --gpu-name A100 --mode vm --startup-script pip install torch"},
	}}
	client, err := newBrevClient(Config{NvidiaBrev: NvidiaBrevConfig{StartupScript: "pip install torch"}}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.create(context.Background(), "crabbox-inline-123456789abc"); err != nil {
		t.Fatal(err)
	}
}

func TestNvidiaBrevClientScopesReadOnlyListByOrg(t *testing.T) {
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --org example-org --all", stdout: `{"workspaces":[]}`},
	}}
	client, err := newBrevClient(Config{NvidiaBrev: NvidiaBrevConfig{CLI: "brev", Org: "example-org"}}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.list(context.Background(), true); err != nil {
		t.Fatal(err)
	}
}

func TestNvidiaBrevClientValidatesCachedActiveOrgWithCLI(t *testing.T) {
	isolateBrevContextFiles(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".brev"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".brev", "active_org.json"), []byte(`{"id":"org-cached","name":"cached"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls orgs --json", stdout: `[{"name":"current","id":"org-current","is_active":true}]`},
	}}
	client, err := newBrevClient(Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	org, err := client.activeOrg(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if org.ID != "org-current" || org.Name != "current" {
		t.Fatalf("org=%#v", org)
	}
}

func TestNvidiaBrevClientFallsBackToActiveOrgJSON(t *testing.T) {
	isolateBrevContextFiles(t)
	t.Setenv("HOME", t.TempDir())
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls orgs --json", stdout: `[{"name":"one","id":"org-one","is_active":false},{"name":"two","id":"org-two","is_active":true}]`},
	}}
	client, err := newBrevClient(Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	org, err := client.activeOrg(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if org.ID != "org-two" || org.Name != "two" {
		t.Fatalf("org=%#v", org)
	}
}

func TestNvidiaBrevClientUsesFirstOrganizationWhenNoneIsSelected(t *testing.T) {
	isolateBrevContextFiles(t)
	t.Setenv("HOME", t.TempDir())
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls orgs --json", stdout: `[{"name":"default","id":"org-default","is_active":false},{"name":"other","id":"org-other","is_active":false}]`},
	}}
	client, err := newBrevClient(Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	org, err := client.activeOrg(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if org.ID != "org-default" || org.Name != "default" || !org.IsActive {
		t.Fatalf("org=%#v", org)
	}
}

func TestNvidiaBrevClientUsesAPIKeyOrganizationWithoutCache(t *testing.T) {
	isolateBrevContextFiles(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".brev"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".brev", "credentials.json"), []byte(`{"api_key":"bak-secret","api_key_org_id":"org-api"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := newBrevClient(Config{}, Runtime{Exec: &scriptedBrevRunner{}, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	org, err := client.activeOrg(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if org.ID != "org-api" {
		t.Fatalf("org=%#v", org)
	}
}

func TestNvidiaBrevClientUsesAPIKeyOrganizationBeforeWorkspace(t *testing.T) {
	isolateBrevContextFiles(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".brev"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".brev", "credentials.json"), []byte(`{"api_key":"bak-secret","api_key_org_id":"org-api"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".brev", "active_org.json"), []byte(`{"id":"org-stale","name":"stale"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brevWorkspaceMetaPath, []byte(`{"workspaceId":"ws-local","organizationId":"org-workspace"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := newBrevClient(Config{}, Runtime{Exec: &scriptedBrevRunner{}, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	org, err := client.activeOrg(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if org.ID != "org-api" {
		t.Fatalf("org=%#v", org)
	}
}

func TestNvidiaBrevClientRejectsOrgScopedMutations(t *testing.T) {
	client, err := newBrevClient(Config{NvidiaBrev: NvidiaBrevConfig{CLI: "brev", Org: "example-org"}}, Runtime{Exec: &scriptedBrevRunner{}, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		run  func(context.Context) error
	}{
		{name: "create", run: func(ctx context.Context) error { return client.create(ctx, "crabbox-demo-123456789abc") }},
		{name: "start", run: func(ctx context.Context) error { return client.start(ctx, "ws-123") }},
		{name: "stop", run: func(ctx context.Context) error { return client.stop(ctx, "ws-123") }},
		{name: "delete", run: func(ctx context.Context) error { return client.delete(ctx, "ws-123") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "does not support --org") || strings.Contains(err.Error(), "example-org") {
				t.Fatalf("err=%v, want safe org-scoped mutation rejection without org value", err)
			}
		})
	}
}

func isolateBrevContextFiles(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	oldMeta := brevWorkspaceMetaPath
	brevWorkspaceMetaPath = filepath.Join(dir, "workspace.json")
	t.Cleanup(func() {
		brevWorkspaceMetaPath = oldMeta
	})
}

func TestNvidiaBrevClientAddsStoppableForStopReleaseAction(t *testing.T) {
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "create crabbox-demo-123456789abc --detached --stoppable --gpu-name A100 --mode vm"},
	}}
	client, err := newBrevClient(Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "stop"}}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.create(context.Background(), "crabbox-demo-123456789abc"); err != nil {
		t.Fatal(err)
	}
}

func TestNvidiaBrevClientIncludesProviderDiagnosticsInErrors(t *testing.T) {
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "create crabbox-demo-123456789abc --detached --gpu-name A100 --mode vm", stderr: "insufficient GPU capacity in selected region", err: errors.New("exit status 1")},
	}}
	client, err := newBrevClient(Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	err = client.create(context.Background(), "crabbox-demo-123456789abc")
	if err == nil || !strings.Contains(err.Error(), "insufficient GPU capacity") {
		t.Fatalf("err=%v", err)
	}
}
