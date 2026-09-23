package nvidiabrev

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestNvidiaBrevOrdinaryStatusProbesWithoutPreparing(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX command fixtures")
	}
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH client required")
	}
	for _, tc := range []struct {
		name, state                               string
		missing, scoped, fail                     bool
		noDigest, stale, missingAlias, failedMint bool
		wantHost, wantReady                       bool
		retainedTarget, overrideTarget            string
	}{
		{name: "healthy", state: "RUNNING", wantHost: true, wantReady: true},
		{name: "retained host", state: "RUNNING", retainedTarget: "host", wantHost: true, wantReady: true},
		{name: "explicit container", state: "RUNNING", retainedTarget: "host", overrideTarget: "container", wantHost: true, wantReady: true},
		{name: "explicit host", state: "RUNNING", retainedTarget: "container", overrideTarget: "host", wantHost: true, wantReady: true},
		{name: "failed probe", state: "RUNNING", fail: true, wantHost: true},
		{name: "stopped", state: "STOPPED"},
		{name: "deleting", state: "DELETING"},
		{name: "unprepared", state: "RUNNING", missing: true},
		{name: "inventory organization", state: "RUNNING", scoped: true},
		{name: "no provenance", state: "RUNNING", noDigest: true},
		{name: "changed config", state: "RUNNING", stale: true},
		{name: "foreign organization host route", state: "RUNNING", stale: true, retainedTarget: "host"},
		{name: "missing alias", state: "RUNNING", missingAlias: true},
		{name: "failed mint", state: "RUNNING", failedMint: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, entry := range os.Environ() {
				key, _, _ := strings.Cut(entry, "=")
				if strings.HasPrefix(key, "CRABBOX_") {
					t.Setenv(key, "")
				}
			}
			state, home := isolateNvidiaBrevState(t)
			dir := t.TempDir()
			t.Chdir(dir)
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "absent.yaml"))
			// This package registers only Brev, unlike the complete CLI binary.
			t.Setenv("CRABBOX_PROVIDER", providerName)
			leaseID := "cbx_123456789abc"
			workspace := brevWorkspace{ID: "ws-status", Name: "crabbox-status-123456789abc", Status: tc.state, BuildStatus: "READY", ShellStatus: "READY", HealthStatus: "HEALTHY"}
			server := workspaceToServer(core.Config{}, workspace, leaseID, "status", true)
			if tc.retainedTarget != "" {
				server.Labels["brev_target"] = tc.retainedTarget
			}
			selectedTarget := tc.retainedTarget
			if tc.overrideTarget != "" {
				selectedTarget = tc.overrideTarget
			}
			alias := brevSSHConfigAlias(workspace.Name, selectedTarget)

			inventory, err := json.Marshal(map[string]any{"workspaces": []brevWorkspace{workspace}})
			if err != nil {
				t.Fatal(err)
			}
			brev := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in ls) printf '%%s\\n' '%s' ;; *) echo unexpected-provider-mutation >&2; exit 91 ;; esac\n", inventory)
			if err := os.WriteFile(filepath.Join(dir, "brev"), []byte(brev), 0700); err != nil {
				t.Fatal(err)
			}
			if !tc.missing {
				writeBrevSSHConfig(t, home, "Host "+workspace.Name+"\n User brev\n Port 2222\n IdentityFile /test/brev-key\n IdentitiesOnly yes\n ProxyCommand unused-fixture-proxy\n UserKnownHostsFile /dev/null\nHost "+workspace.Name+"-host\n HostName host.example.test\n User hostuser\n Port 2200\n IdentitiesOnly yes\n")
			}
			if tc.missingAlias {
				writeBrevSSHConfig(t, home, "Host other\n HostName other.example.test\n User brev\n IdentitiesOnly yes\n")
			}
			if tc.failedMint {
				writeBrevSSHConfig(t, home, "Match host "+workspace.Name+" exec \"false\"\n HostName workspace.example.test\n User brev\n IdentitiesOnly yes\n")
			}
			if data, err := os.ReadFile(defaultBrevSSHConfigPath()); err == nil && !tc.noDigest {
				server.Labels[brevSSHConfigDigestLabel] = brevSSHConfigDigest(data)
			}
			if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "status", core.Config{Provider: providerName}, server, core.SSHTarget{}, dir, false); err != nil {
				t.Fatal(err)
			}
			claimPath := filepath.Join(state, "crabbox", "claims", leaseID+".json")
			before, err := os.ReadFile(claimPath)
			if err != nil {
				t.Fatal(err)
			}
			if tc.stale {
				writeBrevSSHConfig(t, home, "Match host "+alias+" exec \"touch "+filepath.Join(dir, "foreign-hook")+"\"\n HostName old-organization.example.test\n User previous\n IdentitiesOnly yes\n")
			}
			calls := filepath.Join(dir, "ssh-calls")
			code := 0
			if tc.fail {
				code = 1
			}
			script := fmt.Sprintf("#!/bin/sh\nfor arg do if [ \"$arg\" = -G ]; then exec %q \"$@\"; fi; done\nprintf probe >> %q\nexit %d\n", ssh, calls, code)
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			var output, diagnostic bytes.Buffer
			app := core.App{Stdout: &output, Stderr: &diagnostic, Stdin: strings.NewReader("")}
			args := []string{"status", "--provider", providerName, "--id", leaseID, "--json"}
			if tc.scoped {
				args = append(args, "--nvidia-brev-org", "other-org")
			}
			if tc.overrideTarget != "" {
				args = append(args, "--nvidia-brev-target", tc.overrideTarget)
			}
			if err := app.Run(t.Context(), args); err != nil {
				t.Fatalf("status: %v: %s", err, diagnostic.String())
			}
			var status core.StatusView
			if err := json.Unmarshal(output.Bytes(), &status); err != nil {
				t.Fatalf("status JSON: %v: %s", err, output.String())
			}
			if status.HasHost != tc.wantHost || status.Ready != tc.wantReady {
				t.Fatalf("hasHost=%v ready=%v; want %v %v", status.HasHost, status.Ready, tc.wantHost, tc.wantReady)
			}
			wantUser, wantPort := "brev", "2222"
			if selectedTarget == "host" {
				wantUser, wantPort = "hostuser", "2200"
			}
			if tc.wantHost && (status.SSHHost != alias || status.SSHUser != wantUser || status.SSHPort != wantPort) {
				t.Fatalf("wrong endpoint: %#v", status)
			}
			probes, err := os.ReadFile(calls)
			if tc.wantHost && len(probes) == 0 {
				t.Fatalf("readiness never probed: %v", err)
			}
			if !tc.wantHost && !os.IsNotExist(err) {
				t.Fatalf("inactive or unprepared workspace probed: %s err=%v", probes, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "foreign-hook")); !os.IsNotExist(err) {
				t.Fatalf("foreign organization config evaluated: %v", err)
			}
			after, err := os.ReadFile(claimPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("ordinary status mutated claim: %v", err)
			}
			if tc.missingAlias {
				if err := app.Run(t.Context(), []string{"heartbeat", "--provider", providerName, "--id", leaseID, "--json"}); err != nil {
					t.Fatalf("missing alias broke heartbeat: %v", err)
				}
			}
			if tc.scoped {
				if err := app.Run(t.Context(), []string{"heartbeat", "--provider", providerName, "--id", leaseID, "--nvidia-brev-org", "other-org", "--json"}); err == nil {
					t.Fatal("inventory-only organization authorized a claim touch")
				}
				after, err := os.ReadFile(claimPath)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("inventory-only heartbeat mutated claim: %v", err)
				}
			}
		})
	}
}

func TestNvidiaBrevPreparedRouteReplacesProvenanceWithClaimFence(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			state, home := isolateNvidiaBrevState(t)
			leaseID := "cbx_123456789abc"
			workspace := brevWorkspace{ID: "ws", Name: "gpu", Status: "RUNNING", BuildStatus: "READY", ShellStatus: "READY", HealthStatus: "HEALTHY"}
			server := workspaceToServer(core.Config{}, workspace, leaseID, "gpu", true)
			server.Labels[brevSSHConfigDigestLabel] = "old-preparation"
			if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "gpu", core.Config{Provider: providerName}, server, core.SSHTarget{}, t.TempDir(), false); err != nil {
				t.Fatal(err)
			}
			config := "Host gpu-host\n HostName workspace.example.test\n User brev\n IdentityFile /test/key\n IdentitiesOnly yes\n"
			writeBrevSSHConfig(t, home, config)
			runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
				{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws","name":"gpu","status":"RUNNING","build_status":"READY","shell_status":"READY","health_status":"HEALTHY"}]}`},
				{args: "refresh"},
			}}
			backend := NewNvidiaBrevBackend(Provider{}.Spec(), core.Config{NvidiaBrev: core.NvidiaBrevConfig{Target: "host"}}, core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
			lease, err := backend.Resolve(t.Context(), core.ResolveRequest{ID: leaseID, StatusOnly: true, ReadyProbe: true, NoLocalStateMutations: true})
			if err != nil {
				t.Fatal(err)
			}
			if changed {
				updateNvidiaBrevClaim(t, state, leaseID, func(claim map[string]any) { claim["labels"].(map[string]any)["keep"] = "false" })
			}
			_, err = backend.Touch(t.Context(), core.TouchRequest{Lease: lease, State: "ready"})
			claim, _, claimErr := resolveLeaseClaimForProvider(leaseID)
			if claimErr != nil {
				t.Fatal(claimErr)
			}
			if changed {
				if err == nil || claim.Labels[brevSSHConfigDigestLabel] != "old-preparation" {
					t.Fatalf("stale preparation published: err=%v labels=%v", err, claim.Labels)
				}
			} else if err != nil || claim.Labels[brevSSHConfigDigestLabel] != brevSSHConfigDigest([]byte("IdentitiesOnly yes\nUserKnownHostsFile /dev/null\n"+config)) {
				t.Fatalf("new preparation lost: err=%v labels=%v", err, claim.Labels)
			} else if claim.Labels["brev_target"] != "host" {
				t.Fatalf("prepared host selection lost: %v", claim.Labels)
			}
		})
	}
}

func TestNvidiaBrevStatusWaitStillRefreshes(t *testing.T) {
	_, home := isolateNvidiaBrevState(t)
	writeBrevSSHConfig(t, home, "Host gpu\n HostName 192.0.2.1\n User brev\n IdentityFile /test/key\n IdentitiesOnly yes\n")
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws","name":"gpu","status":"RUNNING","build_status":"READY","shell_status":"READY","health_status":"HEALTHY"}]}`},
		{args: "refresh"},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), core.Config{}, core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	lease, err := backend.Resolve(t.Context(), core.ResolveRequest{ID: "ws", StatusOnly: true, ReadyProbe: true, NoLocalStateMutations: true})
	if err != nil || lease.SSH.Host == "" {
		t.Fatalf("wait resolve: %#v err=%v", lease.SSH, err)
	}
	if len(runner.responses) != 0 {
		t.Fatalf("wait skipped refresh: %s", runner.joinedCalls())
	}
}
