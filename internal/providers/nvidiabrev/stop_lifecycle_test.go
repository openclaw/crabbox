package nvidiabrev

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestNvidiaBrevStopWaitsForProviderConfirmation(t *testing.T) {
	for _, scenario := range []string{"stopping then stopped", "stop interrupted", "interrupted", "inventory failed", "disappeared", "failed", "organization changed", "already stopped", "retry stopping"} {
		t.Run(scenario, func(t *testing.T) {
			isolateNvidiaBrevState(t)
			workspace := brevWorkspace{ID: "ws-stop", Name: "crabbox-stop-111122223333", Status: "RUNNING"}
			if scenario == "already stopped" {
				workspace.Status = "STOPPED"
			} else if scenario == "retry stopping" {
				workspace.Status = "STOPPING"
			}
			cfg := core.Config{Provider: providerName, NvidiaBrev: core.NvidiaBrevConfig{ReleaseAction: "stop"}}
			server := workspaceToServer(cfg, workspace, "cbx_111122223333", "stop", true)
			if err := claimTestNvidiaBrevLeaseTargetForRepoConfig("cbx_111122223333", "stop", cfg, server, core.SSHTarget{Host: "203.0.113.10", Port: "22", User: "brev"}, t.TempDir(), false); err != nil {
				t.Fatal(err)
			}
			claim, _, err := resolveLeaseClaimForProvider("cbx_111122223333")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stops, observations := 0, 0
			runner := &fakeRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				switch strings.Join(req.Args, " ") {
				case "stop ws-stop":
					stops++
					if scenario == "stop interrupted" {
						cancel()
						return core.LocalCommandResult{}, ctx.Err()
					}
					return core.LocalCommandResult{}, nil
				case "ls --json --all":
					observations++
					pending, _, err := resolveLeaseClaimForProvider(claim.LeaseID)
					if err != nil || pending.Labels["state"] != "stopping" || pending.SSHHost != "" || pending.SSHPort != 0 {
						t.Fatalf("stop was not recorded as pending before observation: %#v, %v", pending, err)
					}
					current := workspace
					current.Status = "STOPPED"
					switch scenario {
					case "stopping then stopped":
						if observations == 1 {
							current.Status = "STOPPING"
						}
					case "interrupted":
						cancel()
						return core.LocalCommandResult{}, ctx.Err()
					case "inventory failed":
						return core.LocalCommandResult{}, errors.New("inventory unavailable")
					case "disappeared":
						return core.LocalCommandResult{Stdout: "[]"}, nil
					case "failed":
						current.Status = "FAILED"
					case "organization changed":
						writeBrevActiveOrg(t, "org-changed")
					}
					data, _ := json.Marshal([]brevWorkspace{current})
					return core.LocalCommandResult{Stdout: string(data)}, nil
				default:
					t.Fatalf("unexpected command: %v", req.Args)
					return core.LocalCommandResult{}, nil
				}
			}}
			backend := NewNvidiaBrevBackend(Provider{}.Spec(), cfg, core.Runtime{Exec: runner, Stderr: io.Discard}).(*nvidiaBrevBackend)
			client, err := backend.client()
			if err != nil {
				t.Fatal(err)
			}
			err = backend.stopWorkspaceAndPersistClaim(ctx, client, workspace, claim)
			wantState := "stopping"
			switch scenario {
			case "stopping then stopped", "already stopped", "retry stopping":
				wantState = "stopped"
				if err != nil {
					t.Fatal(err)
				}
			default:
				if err == nil {
					t.Fatal("unconfirmed stop succeeded")
				}
			}
			if strings.Contains(scenario, "interrupted") && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			wantStops := 1
			if scenario == "already stopped" || scenario == "retry stopping" {
				wantStops = 0
			}
			if stops != wantStops {
				t.Fatalf("stop calls=%d, want %d", stops, wantStops)
			}
			got, exists, err := resolveLeaseClaimForProvider(claim.LeaseID)
			if err != nil || !exists || got.Labels["state"] != wantState || got.SSHHost != "" || got.SSHPort != 0 {
				t.Fatalf("claim=%#v exists=%v err=%v, want %s without endpoint", got, exists, err, wantState)
			}
		})
	}
}
