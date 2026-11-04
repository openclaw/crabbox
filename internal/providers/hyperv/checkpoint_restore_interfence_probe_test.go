//go:build crabbox_claim_fence_probe

package hyperv

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestRestoreNativeCheckpointStopsAtInterFenceClaimChanges(t *testing.T) {
	const childEnv = "CRABBOX_TEST_HYPERV_INTERFENCE_CHILD"
	points := []struct {
		name     string
		fence    string
		restores int
	}{
		{name: "before-restore", fence: "3"},
		{name: "before-start", fence: "4", restores: 1},
	}
	child := os.Getenv(childEnv)
	if child == "" {
		// The existing probe fires once per process. Each arm gets a fresh
		// test process rather than sharing or resetting the CLI's probe state.
		for _, point := range points {
			for _, mode := range []string{"revoke", "reassign"} {
				t.Run(point.name+"/"+mode, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, os.Args[0],
						"-test.run=^TestRestoreNativeCheckpointStopsAtInterFenceClaimChanges$",
						"-test.timeout=20s")
					cmd.Env = append(os.Environ(), childEnv+"="+point.name+"/"+mode)
					output, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("isolated restore probe: %v\n%s", err, output)
					}
					marker := "swap mode=" + mode + " site=hyperv.(*backend).restoreNativeCheckpoint "
					if strings.Count(string(output), marker) != 1 {
						t.Fatalf("expected one restore claim swap\n%s", output)
					}
				})
			}
		}
		return
	}

	pointName, mode, ok := strings.Cut(child, "/")
	if !ok || (mode != "revoke" && mode != "reassign") {
		t.Fatalf("invalid isolated restore arm %q", child)
	}
	for _, point := range points {
		if point.name != pointName {
			continue
		}
		setCheckpointTestState(t)
		t.Setenv("CRABBOX_CLAIM_FENCE_PROBE", "hyperv.(*backend).restoreNativeCheckpoint#"+point.fence+":"+mode)
		paths, metadata := createCheckpointArtifact(t)
		queries, restores, starts, addressQueries, sshProbes := 0, 0, 0, 0, 0
		runner := &recordingRunner{}
		runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
			script := commandScript(req)
			switch {
			case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
				queries++
				return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 3}), nil, true
			case strings.Contains(script, "Restore-VMSnapshot"):
				restores++
			case strings.Contains(script, "Start-VM"):
				starts++
			case strings.Contains(script, "Select-Object -ExpandProperty IPAddresses"):
				addressQueries++
				return core.LocalCommandResult{Stdout: `["192.0.2.45"]`}, nil, true
			default:
				t.Fatalf("unexpected fake host operation: %s", script)
			}
			return core.LocalCommandResult{}, nil, true
		}
		b := testBackend(runner)
		b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
			sshProbes++
			return nil
		}
		persistCheckpointSource(t, b)
		previous, err := core.ReadLeaseClaim(testCheckpointLeaseID)
		if err != nil {
			t.Fatal(err)
		}
		if previous.CloudID == "" || previous.ProviderScope == "" {
			t.Fatal("source fixture must have a concrete VM and provider scope")
		}
		requestRepo := t.TempDir()
		_, err = b.restoreNativeCheckpoint(context.Background(), core.NativeCheckpointRestoreRequest{
			Config:  b.cfg,
			Record:  checkpointForkRecord(paths, metadata),
			LeaseID: testCheckpointLeaseID,
			Repo:    core.Repo{Root: requestRepo},
			Reclaim: true,
		})
		if err == nil || !strings.Contains(err.Error(), "claim changed") || core.ExitCodeForError(err, 0) != 2 {
			t.Fatalf("restore err=%v want claim conflict (exit 2)", err)
		}
		if queries != 1 || restores != point.restores || starts != 0 || addressQueries != 0 || sshProbes != 0 {
			t.Fatalf("effects: query=%d restore=%d start=%d address=%d ssh=%d; want query=1 restore=%d and no later effects",
				queries, restores, starts, addressQueries, sshProbes, point.restores)
		}
		current, exists, err := core.ReadLeaseClaimWithPresence(testCheckpointLeaseID)
		if err != nil {
			t.Fatal(err)
		}
		if mode == "revoke" {
			if exists {
				t.Fatalf("revoked claim was resurrected: %#v", current)
			}
		} else if !exists || current.ProviderScope != previous.ProviderScope+"-reassigned" ||
			current.CloudID != previous.CloudID+"-reassigned" || current.RepoRoot != requestRepo {
			t.Fatalf("reassigned reservation was replaced: exists=%v claim=%#v", exists, current)
		}
		return
	}
	t.Fatalf("unknown isolated restore point %q", pointName)
}
