package hyperv

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// changeCheckpointClaim revokes a lease claim or reassigns it to another repo,
// the two ways another actor can take authority away from a running operation.
func changeCheckpointClaim(t *testing.T, b *backend, mode, leaseID string) string {
	t.Helper()
	if mode == "revoke" {
		core.RemoveLeaseClaim(leaseID)
		return ""
	}
	current, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("resolve claim before reassignment: ok=%v err=%v", ok, err)
	}
	other := t.TempDir()
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, current.Slug, providerName, current.ProviderScope, b.cfg.Pond, other, b.cfg.IdleTimeout, true, core.Server{}, core.SSHTarget{}); err != nil {
		t.Fatalf("reassign claim: %v", err)
	}
	return other
}

// A fork whose provisional claim is revoked or reassigned must stop before its
// next host or guest effect, including each PowerShell Direct and SSH readiness
// attempt. Claim changes between attempts happen in the backoff, outside the
// fence, as a concurrent reclaim would.
func TestForkNativeCheckpointStopsAtNextEffectWhenClaimChanges(t *testing.T) {
	points := []struct {
		name          string
		ran           []string
		refused       []string
		bootProbes    int
		restartProbes int
		sshProbes     int
	}{
		{name: "after-import", ran: []string{"Import-VM"}, refused: []string{"Rename-Computer", "ssh-keygen.exe", "Restart-VM", "MachineGuid", "Connect-VMNetworkAdapter"}},
		{name: "between-boot-probes", ran: []string{"Import-VM"}, refused: []string{"Rename-Computer", "ssh-keygen.exe", "Restart-VM", "MachineGuid", "Connect-VMNetworkAdapter"}, bootProbes: 1},
		{name: "between-restart-probes", ran: []string{"Rename-Computer", "Restart-VM"}, refused: []string{"MachineGuid", "Connect-VMNetworkAdapter"}, bootProbes: 1, restartProbes: 1},
		{name: "between-ssh-probes", ran: []string{"MachineGuid", "Connect-VMNetworkAdapter"}, bootProbes: 1, restartProbes: 1, sshProbes: 1},
	}
	for _, point := range points {
		for _, mode := range []string{"revoke", "reassign"} {
			t.Run(point.name+"/"+mode, func(t *testing.T) {
				setCheckpointTestState(t)
				paths, metadata := createCheckpointArtifact(t)
				oldOS := hypervHostOS
				hypervHostOS = "windows"
				t.Cleanup(func() { hypervHostOS = oldOS })

				var b *backend
				var forkLease string
				var newOwner string
				bootProbes, restartProbes := 0, 0
				swapped := false
				swap := func() {
					if !swapped {
						swapped = true
						newOwner = changeCheckpointClaim(t, b, mode, forkLease)
					}
				}
				runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
				runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
					script := commandScript(req)
					switch {
					case strings.Contains(script, "Import-VM"):
						return jsonResult(t, checkpointImportOutput{ID: "99999999-8888-7777-6666-555555555555"}), nil, true
					case strings.Contains(script, "Select-Object -ExpandProperty IPAddresses"):
						return core.LocalCommandResult{Stdout: `["192.0.2.55"]`}, nil, true
					case strings.Contains(script, "-ScriptBlock { $true }"):
						// The first readiness attempt of the targeted wait fails, so
						// the wait backs off and tries again.
						if findCallIndex(runner.calls, "Restart-VM") < 0 {
							bootProbes++
							if point.name == "between-boot-probes" && bootProbes == 1 {
								return core.LocalCommandResult{Stderr: "guest still booting"}, errors.New("exit 1"), true
							}
						} else {
							restartProbes++
							if point.name == "between-restart-probes" && restartProbes == 1 {
								return core.LocalCommandResult{Stderr: "guest still restarting"}, errors.New("exit 1"), true
							}
						}
						return core.LocalCommandResult{}, nil, true
					case strings.Contains(script, "Invoke-Command"):
						return core.LocalCommandResult{}, nil, true
					case strings.Contains(script, "ConvertTo-Json -InputObject $items"):
						return core.LocalCommandResult{Stdout: "[]"}, nil, true
					default:
						return core.LocalCommandResult{}, nil, false
					}
				}
				b = testBackend(runner)
				b.checkpointSSHBudget = 5 * time.Second
				b.guestReadyBudget = 5 * time.Second
				probes := 0
				b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
					probes++
					if point.name == "between-ssh-probes" && probes == 1 {
						return errors.New("ssh not ready yet")
					}
					return nil
				}
				b.sleep = func(context.Context, time.Duration) error {
					switch {
					case point.name == "between-boot-probes" && bootProbes > 0,
						point.name == "between-restart-probes" && restartProbes > 0,
						point.name == "between-ssh-probes" && probes > 0:
						swap()
					}
					return nil
				}

				_, err := b.ForkNativeCheckpoint(context.Background(), core.NativeCheckpointForkLifecycleRequest{
					Record:        checkpointForkRecord(paths, metadata),
					Repo:          core.Repo{Root: t.TempDir(), Name: "my-app"},
					Keep:          true,
					RequestedSlug: "checkpoint-fork",
					OnAcquired: func(lease core.LeaseTarget) error {
						forkLease = lease.LeaseID
						if point.name == "after-import" {
							swap()
						}
						return nil
					},
				})
				if !swapped {
					t.Fatalf("fork never reached the %s claim change: err=%v", point.name, err)
				}
				if err == nil || !strings.Contains(err.Error(), "claim changed") || core.ExitCodeForError(err, 0) != 2 {
					t.Fatalf("fork err=%v want claim conflict (exit 2)", err)
				}
				for _, effect := range point.ran {
					if findCallIndex(runner.calls, effect) < 0 {
						t.Fatalf("fork did not run %s before the claim changed", effect)
					}
				}
				for _, effect := range point.refused {
					if findCallIndex(runner.calls, effect) >= 0 {
						t.Fatalf("fork ran %s after the claim changed", effect)
					}
				}
				if probes != point.sshProbes {
					t.Fatalf("readiness SSH probes=%d want %d", probes, point.sshProbes)
				}
				if bootProbes != point.bootProbes || restartProbes != point.restartProbes {
					t.Fatalf("PowerShell Direct readiness attempts after import=%d after restart=%d, want %d and %d", bootProbes, restartProbes, point.bootProbes, point.restartProbes)
				}
				removed := findCallIndex(runner.calls, "Remove-VM -VM $candidate") >= 0
				claim, exists, resolveErr := core.ResolveLeaseClaimForProvider(forkLease, providerName)
				if resolveErr != nil {
					t.Fatalf("resolve fork claim: %v", resolveErr)
				}
				switch mode {
				case "reassign":
					if removed {
						t.Fatal("fork rollback removed a VM whose lease was reassigned")
					}
					if !exists || claim.RepoRoot != newOwner {
						t.Fatalf("reassigned claim exists=%v repo=%q want %q", exists, claim.RepoRoot, newOwner)
					}
				case "revoke":
					if exists {
						t.Fatalf("revoked fork claim was recreated: %#v", claim)
					}
					if !removed {
						t.Fatal("fork rollback kept an import whose claim was revoked")
					}
				}
			})
		}
	}
}

// Restore readiness SSH authenticates to the reserved VM, so a claim revoked or
// reassigned after the VM starts must stop the next probe.
func TestRestoreNativeCheckpointStopsReadinessSSHWhenClaimChanges(t *testing.T) {
	points := []struct {
		name      string
		sshProbes int
	}{
		{name: "before-ssh"},
		{name: "between-ssh-probes", sshProbes: 1},
	}
	for _, point := range points {
		for _, mode := range []string{"revoke", "reassign"} {
			t.Run(point.name+"/"+mode, func(t *testing.T) {
				setCheckpointTestState(t)
				paths, metadata := createCheckpointArtifact(t)
				var b *backend
				var newOwner string
				swapped := false
				swap := func() {
					if !swapped {
						swapped = true
						newOwner = changeCheckpointClaim(t, b, mode, testCheckpointLeaseID)
					}
				}
				runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
				runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
					script := commandScript(req)
					switch {
					case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
						return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 3}), nil, true
					case strings.Contains(script, "Restore-VMSnapshot"), strings.Contains(script, "Start-VM"):
						return core.LocalCommandResult{}, nil, true
					case strings.Contains(script, "Select-Object -ExpandProperty IPAddresses"):
						// The guest address wait runs after Start-VM and outside the fence.
						if point.name == "before-ssh" {
							swap()
						}
						return core.LocalCommandResult{Stdout: `["192.0.2.45"]`}, nil, true
					default:
						return core.LocalCommandResult{}, nil, false
					}
				}
				b = testBackend(runner)
				b.checkpointSSHBudget = 5 * time.Second
				persistCheckpointSource(t, b)
				probes := 0
				b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
					probes++
					if point.name == "between-ssh-probes" && probes == 1 {
						return errors.New("ssh not ready yet")
					}
					return nil
				}
				b.sleep = func(context.Context, time.Duration) error {
					if point.name == "between-ssh-probes" && probes > 0 {
						swap()
					}
					return nil
				}

				_, err := b.restoreNativeCheckpoint(context.Background(), core.NativeCheckpointRestoreRequest{
					Config:  b.cfg,
					Record:  checkpointForkRecord(paths, metadata),
					LeaseID: testCheckpointLeaseID,
					Repo:    core.Repo{Root: t.TempDir()},
					Reclaim: true,
				})
				if !swapped {
					t.Fatalf("restore never reached the %s claim change: err=%v", point.name, err)
				}
				if err == nil || !strings.Contains(err.Error(), "claim changed") || core.ExitCodeForError(err, 0) != 2 {
					t.Fatalf("restore err=%v want claim conflict (exit 2)", err)
				}
				if probes != point.sshProbes {
					t.Fatalf("readiness SSH probes=%d want %d", probes, point.sshProbes)
				}
				claim, exists, resolveErr := core.ResolveLeaseClaimForProvider(testCheckpointLeaseID, providerName)
				if resolveErr != nil {
					t.Fatalf("resolve claim: %v", resolveErr)
				}
				switch mode {
				case "reassign":
					if !exists || claim.RepoRoot != newOwner {
						t.Fatalf("reassigned claim exists=%v repo=%q want %q", exists, claim.RepoRoot, newOwner)
					}
				case "revoke":
					if exists {
						t.Fatalf("revoked claim was recreated: %#v", claim)
					}
				}
			})
		}
	}
}

// Each fork host or guest effect must hold the provisional claim exclusively, so
// a concurrent reclaim cannot land while the effect runs.
func TestForkNativeCheckpointHoldsClaimFenceDuringEffects(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	writerBlocked := func() bool {
		claims, err := core.ListLeaseClaims()
		if err != nil || len(claims) != 1 {
			t.Fatalf("fork claims=%d err=%v", len(claims), err)
		}
		waitCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		return errors.Is(core.WithLeaseClaimUnchangedContext(waitCtx, claims[0].LeaseID, claims[0], nil), context.DeadlineExceeded)
	}
	effects := []string{"Import-VM", "-ScriptBlock { $true }", "Rename-Computer", "Restart-VM", "MachineGuid", "Connect-VMNetworkAdapter"}
	blocked := map[string]bool{}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		for _, effect := range effects {
			if _, seen := blocked[effect]; !seen && strings.Contains(script, effect) {
				blocked[effect] = writerBlocked()
			}
		}
		switch {
		case strings.Contains(script, "Import-VM"):
			return jsonResult(t, checkpointImportOutput{ID: "99999999-8888-7777-6666-555555555555"}), nil, true
		case strings.Contains(script, "Select-Object -ExpandProperty IPAddresses"):
			return core.LocalCommandResult{Stdout: `["192.0.2.55"]`}, nil, true
		case strings.Contains(script, "Invoke-Command"):
			return core.LocalCommandResult{}, nil, true
		case strings.Contains(script, "ConvertTo-Json -InputObject $items"):
			return core.LocalCommandResult{Stdout: "[]"}, nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	b.checkpointSSHBudget = 5 * time.Second
	sshBlocked := false
	b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		sshBlocked = writerBlocked()
		return nil
	}

	if _, err := b.ForkNativeCheckpoint(context.Background(), core.NativeCheckpointForkLifecycleRequest{
		Record:        checkpointForkRecord(paths, metadata),
		Repo:          core.Repo{Root: t.TempDir(), Name: "my-app"},
		Keep:          true,
		RequestedSlug: "checkpoint-fork",
	}); err != nil {
		t.Fatalf("ForkNativeCheckpoint: %v", err)
	}
	for _, effect := range effects {
		if held, seen := blocked[effect]; !seen || !held {
			t.Fatalf("a claim writer was admitted while fork ran %s (seen=%v)", effect, seen)
		}
	}
	if !sshBlocked {
		t.Fatal("a claim writer was admitted during fork readiness SSH")
	}
}
