package cli

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestClaimsListProjectsPersistedDynamicTargetLifecycle(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	for _, target := range []string{targetLinux, targetWindows} {
		t.Run(target, func(t *testing.T) {
			id := "cbx_" + target
			cfg := baseConfig()
			cfg.Provider, cfg.TargetOS = "hyperv", target
			if target == targetWindows {
				cfg.WindowsMode = windowsModeNormal
			}
			for _, state := range []string{"provisioning", "ready", "stopped", "provisioning", "ready"} {
				labels := DirectLeaseLabels(cfg, id, target, "hyperv", "", true, time.Time{})
				labels["state"], labels["instance"] = state, "crabbox-"+target
				server := Server{CloudID: labels["instance"], Labels: labels}
				if err := ClaimLeaseForRepoProviderScopePondEndpoint(id, target, "hyperv", "instance:"+labels["instance"], "", root, time.Hour, false, server, SSHTarget{TargetOS: target}); err != nil {
					t.Fatal(err)
				}
				stdout, _, err := runClaimsList(t, "--json")
				if err != nil {
					t.Fatal(err)
				}
				var output localClaimsListOutput
				if err := json.Unmarshal([]byte(stdout), &output); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, claim := range output.Claims {
					if claim.LeaseID == id {
						found = true
						if claim.TargetOS != target || filepath.Clean(claim.RepoRoot) != root {
							t.Fatalf("state=%s claim=%+v", state, claim)
						}
						if target == targetWindows && claim.WindowsMode != windowsModeNormal ||
							target == targetLinux && claim.WindowsMode != "" {
							t.Fatalf("incorrect mode: %+v", claim)
						}
					}
				}
				if !found || len(output.Problems) != 0 {
					t.Fatalf("missing claim or invalid projection: %+v", output)
				}
			}
		})
	}
}

func TestClaimsListTargetConflictsFailClosed(t *testing.T) {
	tests := []struct {
		name   string
		claim  leaseClaim
		target string
		mode   string
		bad    bool
	}{
		{"unknown", leaseClaim{}, "", "", false},
		{"static", leaseClaim{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, targetWindows, windowsModeWSL2, false},
		{"label", leaseClaim{Labels: map[string]string{"target": targetLinux}}, targetLinux, "", false},
		{"worker-runtime", leaseClaim{Labels: map[string]string{"target": targetWorkerRuntime}}, targetWorkerRuntime, "", false},
		{"conflict", leaseClaim{TargetOS: targetWindows, Labels: map[string]string{"target": targetLinux}}, "", "", true},
		{"invalid", leaseClaim{Labels: map[string]string{"target": "unsupported"}}, "", "", true},
		{"orphan-mode", leaseClaim{Labels: map[string]string{"windows_mode": windowsModeNormal}}, "", "", true},
		{"legacy-linux-default-mode", leaseClaim{TargetOS: targetLinux, WindowsMode: windowsModeNormal}, targetLinux, "", false},
		{"linux-wsl-mode", leaseClaim{TargetOS: targetLinux, WindowsMode: windowsModeWSL2}, "", "", true},
		{"mode-conflict", leaseClaim{TargetOS: targetWindows, WindowsMode: windowsModeNormal, Labels: map[string]string{"windows_mode": windowsModeWSL2}}, "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			tt.claim.LeaseID, tt.claim.Provider, tt.claim.RepoRoot = "cbx_target", "hyperv", t.TempDir()
			writeClaimsListFixture(t, "cbx_target.json", tt.claim)
			stdout, _, err := runClaimsList(t, "--json")
			if tt.bad {
				assertClaimsExitCode(t, err, 2)
			} else if err != nil {
				t.Fatal(err)
			}
			var output localClaimsListOutput
			if err := json.Unmarshal([]byte(stdout), &output); err != nil {
				t.Fatal(err)
			}
			if tt.bad {
				if len(output.Claims) != 0 || len(output.Problems) != 1 || output.Problems[0].Code != "invalid_target" {
					t.Fatalf("not fail closed: %+v", output)
				}
			} else if len(output.Claims) != 1 || output.Claims[0].TargetOS != tt.target || output.Claims[0].WindowsMode != tt.mode {
				t.Fatalf("projection=%+v", output)
			}
		})
	}
}
