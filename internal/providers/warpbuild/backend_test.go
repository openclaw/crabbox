package warpbuild

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"strconv"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type nativeFixture struct {
	item        sandbox
	calls       []core.LocalCommandRequest
	commandExit int
	killed      bool
	killFails   bool
	listFails   bool
}

func (f *nativeFixture) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	f.calls = append(f.calls, req)
	result := core.LocalCommandResult{}
	if len(req.Args) < 4 {
		return result, errors.New("unexpected native command")
	}
	switch req.Args[3] {
	case "create":
		f.item = sandbox{Name: req.Args[4], ID: "sandbox-fixture-id", Created: "2026-11-02T00:00:00Z", State: "ready", Size: "warp-macos-26-arm64-6x"}
	case "info":
		if f.killed {
			return core.LocalCommandResult{ExitCode: 4, Stderr: "not found"}, errors.New("exit 4")
		}
		data, _ := json.Marshal(f.item)
		result.Stdout = string(data)
	case "list":
		if f.listFails {
			return core.LocalCommandResult{ExitCode: 1, Stderr: "network error"}, errors.New("exit 1")
		}
		items := []sandbox{}
		if f.item.Name != "" && !f.killed {
			items = append(items, f.item)
		}
		data, _ := json.Marshal(items)
		result.Stdout = string(data)
	case "exec":
		if req.Args[len(req.Args)-1] == "task-command" {
			result.ExitCode = f.commandExit
			result.Stdout = "task stdout\n"
			result.Stderr = "task stderr\n"
		}
	case "kill":
		if f.killFails {
			return core.LocalCommandResult{ExitCode: 1, Stderr: "delete unavailable"}, errors.New("exit 1")
		}
		f.killed = true
	case "pause":
		f.item.State = "paused"
	case "resume":
		f.item.State = "ready"
	default:
		return result, errors.New("unexpected native operation")
	}
	if req.Stdout != nil {
		_, _ = io.WriteString(req.Stdout, result.Stdout)
	}
	if req.Stderr != nil {
		_, _ = io.WriteString(req.Stderr, result.Stderr)
	}
	return result, nil
}

func fixtureBackend(t *testing.T, native *nativeFixture) (*backend, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetMacOS
	cfg.WarpBuild.Token = "test-token-not-real"
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	return &backend{cfg: cfg, rt: core.Runtime{Exec: native, Stdout: stdout, Stderr: stderr}}, stdout, stderr
}

func TestRunPreservesCommandExitAndDeletesOwnedSandbox(t *testing.T) {
	for _, code := range []int{0, 23} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			native := &nativeFixture{commandExit: code}
			b, stdout, _ := fixtureBackend(t, native)
			result, err := b.Run(t.Context(), core.RunRequest{Repo: core.Repo{Root: t.TempDir()}, NoSync: true, Command: []string{"task-command"}})
			if result.ExitCode != code || (err != nil) != (code != 0) {
				t.Fatalf("exit=%d err=%v", result.ExitCode, err)
			}
			if !native.killed || result.Session == nil || result.Session.Kept {
				t.Fatalf("sandbox cleanup missing: %#v", result.Session)
			}
			if !strings.Contains(stdout.String(), "task stdout") {
				t.Fatalf("missing streamed output: %q", stdout.String())
			}
			claim, exists, err := core.ReadLeaseClaimWithPresence(result.LeaseID)
			if err != nil || exists {
				t.Fatalf("claim remains after deletion: %#v %v", claim, err)
			}
		})
	}
}

func TestConfiguredRunUsesNormalizedWorkdir(t *testing.T) {
	native := &nativeFixture{}
	b, _, _ := fixtureBackend(t, native)
	b.cfg.WarpBuild.Workdir = " /Users/runner/project//build/../work "
	configured, err := (Provider{}).Configure(b.cfg, b.rt)
	if err != nil {
		t.Fatal(err)
	}
	_, err = configured.(core.DelegatedRunBackend).Run(t.Context(), core.RunRequest{Repo: core.Repo{Root: t.TempDir()}, NoSync: true, Command: []string{"task-command"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range native.calls {
		if call.Args[len(call.Args)-1] != "task-command" {
			continue
		}
		// wb's -c contract selects the directory used by the remote command.
		if len(call.Args) < 6 || call.Args[4] != "-c" || call.Args[5] != "/Users/runner/project/work" {
			t.Fatalf("command did not use the normalized workspace: %q", call.Args)
		}
		return
	}
	t.Fatal("workload did not reach wb")
}

func TestStopRejectsReplacementAndRetainsFailedDeletion(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed-delete", true: "replacement"}[replacement], func(t *testing.T) {
			native := &nativeFixture{}
			b, _, _ := fixtureBackend(t, native)
			c, err := newClient(b.cfg, b.rt)
			if err != nil {
				t.Fatal(err)
			}
			claim, err := b.create(t.Context(), c, core.Repo{Root: t.TempDir()}, false, "")
			if err != nil {
				t.Fatal(err)
			}
			if replacement {
				native.item.ID = "different-sandbox-id"
			} else {
				native.killFails = true
			}
			if err := b.Stop(t.Context(), core.StopRequest{ID: claim.LeaseID}); err == nil {
				t.Fatal("unsafe or failed deletion succeeded")
			}
			if native.killed {
				t.Fatal("replacement sandbox was deleted")
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || !exists {
				t.Fatalf("recovery claim lost: %v", err)
			}
		})
	}
}

func TestPauseResumePreservesSandboxIdentity(t *testing.T) {
	native := &nativeFixture{}
	b, _, _ := fixtureBackend(t, native)
	c, _ := newClient(b.cfg, b.rt)
	claim, err := b.create(t.Context(), c, core.Repo{Root: t.TempDir()}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Pause(t.Context(), core.PauseRequest{ID: claim.LeaseID}); err != nil {
		t.Fatal(err)
	}
	if native.item.State != "paused" {
		t.Fatal("sandbox did not pause")
	}
	if err := b.Resume(t.Context(), core.ResumeRequest{ID: claim.LeaseID}); err != nil {
		t.Fatal(err)
	}
	if native.item.State != "ready" || native.item.ID != claim.CloudImmutableID {
		t.Fatal("resume lost sandbox identity")
	}
}

func TestNativeInvocationPinsDestinationAndKeepsSecretsOffArgv(t *testing.T) {
	t.Setenv("WB_API_URL", "https://untrusted.example.test")
	t.Setenv("WB_SBX_USER", "root")
	t.Setenv("WB_SBX_RUNNER_LABEL", "unselected-shape")
	native := &nativeFixture{}
	b, _, _ := fixtureBackend(t, native)
	c, _ := newClient(b.cfg, b.rt)
	_, err := c.run(t.Context(), "test-sandbox", "/Users/runner/work", []string{"task-command"}, map[string]string{"TEST_SECRET": "private-fixture-value"}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	req := native.calls[0]
	if !req.DisableOutputCapture {
		t.Fatal("streamed workload output must not accumulate in memory")
	}
	args := strings.Join(req.Args, " ")
	if strings.Contains(args, "private-fixture-value") || strings.Contains(args, b.cfg.WarpBuild.Token) {
		t.Fatal("credential entered argv")
	}
	data, err := io.ReadAll(req.Stdin)
	if err != nil || !bytes.Contains(data, []byte("private-fixture-value")) {
		t.Fatalf("environment did not use stdin: %v", err)
	}
	env := strings.Join(req.Env, "\n")
	if strings.Contains(env, "untrusted.example.test") || strings.Contains(env, "WB_SBX_USER=root") || strings.Contains(env, "unselected-shape") {
		t.Fatal("ambient native override survived")
	}
	if !strings.Contains(env, "WB_API_URL="+apiScope) || !strings.Contains(env, "WB_TOKEN="+b.cfg.WarpBuild.Token) {
		t.Fatal("missing pinned auth destination")
	}
}

func TestReuseRejectsHeldClaimAndReportsReady(t *testing.T) {
	native := &nativeFixture{}
	b, _, _ := fixtureBackend(t, native)
	repo := core.Repo{Root: t.TempDir()}
	first, err := b.Run(t.Context(), core.RunRequest{Repo: repo, NoSync: true, Keep: true, Command: []string{"task-command"}})
	if err != nil {
		t.Fatal(err)
	}
	status, err := b.Status(t.Context(), core.StatusRequest{ID: first.LeaseID})
	if err != nil || !status.Ready {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	second, err := b.Run(t.Context(), core.RunRequest{ID: first.LeaseID, Repo: repo, NoSync: true, Command: []string{"task-command"}})
	if err != nil || second.LeaseID != first.LeaseID || native.killed {
		t.Fatalf("reuse failed: %+v %v", second, err)
	}
	claim, err := core.ReadLeaseClaim(first.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	labels := maps.Clone(claim.Labels)
	labels["state"] = "cleanup"
	if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels); err != nil {
		t.Fatal(err)
	}
	calls := len(native.calls)
	if _, err := b.Run(t.Context(), core.RunRequest{ID: first.LeaseID, Repo: repo, NoSync: true, Command: []string{"task-command"}}); err == nil {
		t.Fatal("held lease was reused")
	}
	if len(native.calls) != calls {
		t.Fatal("held lease reached the provider")
	}
}

func TestStopRetriesAfterUnconfirmedDeletion(t *testing.T) {
	native := &nativeFixture{}
	b, _, _ := fixtureBackend(t, native)
	c, _ := newClient(b.cfg, b.rt)
	claim, err := b.create(t.Context(), c, core.Repo{Root: t.TempDir()}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	native.listFails = true
	if err := b.Stop(t.Context(), core.StopRequest{ID: claim.LeaseID}); err == nil {
		t.Fatal("unconfirmed deletion succeeded")
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || !exists {
		t.Fatalf("recovery claim lost: %v", err)
	}
	if err := b.Stop(t.Context(), core.StopRequest{ID: claim.LeaseID}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
		t.Fatalf("confirmed absence retained claim: %v", err)
	}
}
