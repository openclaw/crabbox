package nvidiabrev

import (
	"context"
	"io"
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
		{args: "create crabbox-demo-123456789abc --detached --type gpu-l40s --gpu-name L40S --provider aws --mode vm --launchable env-example --startup-script setup.sh"},
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
		StartupScript: "setup.sh",
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
