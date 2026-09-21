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
		name, state           string
		missing, scoped, fail bool
		wantHost, wantReady   bool
	}{
		{name: "healthy", state: "RUNNING", wantHost: true, wantReady: true},
		{name: "failed probe", state: "RUNNING", fail: true, wantHost: true},
		{name: "stopped", state: "STOPPED"},
		{name: "deleting", state: "DELETING"},
		{name: "unprepared", state: "RUNNING", missing: true},
		{name: "inventory organization", state: "RUNNING", scoped: true},
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
			if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "status", core.Config{Provider: providerName}, server, core.SSHTarget{}, dir, false); err != nil {
				t.Fatal(err)
			}
			claimPath := filepath.Join(state, "crabbox", "claims", leaseID+".json")
			before, err := os.ReadFile(claimPath)
			if err != nil {
				t.Fatal(err)
			}
			inventory, err := json.Marshal(map[string]any{"workspaces": []brevWorkspace{workspace}})
			if err != nil {
				t.Fatal(err)
			}
			brev := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in ls) printf '%%s\\n' '%s' ;; *) echo unexpected-provider-mutation >&2; exit 91 ;; esac\n", inventory)
			if err := os.WriteFile(filepath.Join(dir, "brev"), []byte(brev), 0700); err != nil {
				t.Fatal(err)
			}
			if !tc.missing {
				writeBrevSSHConfig(t, home, "Host "+workspace.Name+"\n User brev\n Port 2222\n IdentityFile /test/brev-key\n IdentitiesOnly yes\n ProxyCommand unused-fixture-proxy\n UserKnownHostsFile /dev/null\n")
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
			if tc.wantHost && (status.SSHUser != "brev" || status.SSHPort != "2222") {
				t.Fatalf("wrong endpoint: %#v", status)
			}
			probes, err := os.ReadFile(calls)
			if tc.wantHost && len(probes) == 0 {
				t.Fatalf("readiness never probed: %v", err)
			}
			if !tc.wantHost && !os.IsNotExist(err) {
				t.Fatalf("inactive or unprepared workspace probed: %s err=%v", probes, err)
			}
			after, err := os.ReadFile(claimPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("ordinary status mutated claim: %v", err)
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
