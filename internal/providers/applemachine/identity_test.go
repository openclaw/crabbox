package applemachine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type claimContextRunner struct {
	inner *recordingRunner
	run   func(context.Context, core.LocalCommandRequest) (core.LocalCommandResult, error)
}

func (r claimContextRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if err := ctx.Err(); err != nil {
		return core.LocalCommandResult{}, err
	}
	if r.run != nil {
		return r.run(ctx, req)
	}
	return r.inner.Run(ctx, req)
}

func TestClaimOperationWaitHonorsCallerContext(t *testing.T) {
	for _, operation := range []string{"stop", "reuse", "status", "list"} {
		for _, sharedLock := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/shared=%t", operation, sharedLock), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					b, f, claim := identityFixture(t)
					b.rt.Exec = claimContextRunner{inner: f.runner}
					entered, release := make(chan struct{}), make(chan struct{})
					holderDone := make(chan error, 1)
					go func() {
						hold := func() error { close(entered); <-release; return nil }
						if sharedLock {
							holderDone <- core.WithLeaseClaimUnchangedShared(t.Context(), claim.LeaseID, claim, hold)
						} else {
							holderDone <- core.WithDurableLeaseClaimLockContext(t.Context(), claim.LeaseID, func(*core.LeaseClaim, bool, func() error) error { return hold() })
						}
					}()
					<-entered
					ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
					defer cancel()
					done := make(chan error, 1)
					go func() {
						var err error
						switch operation {
						case "stop":
							err = b.Stop(ctx, core.StopRequest{ID: claim.LeaseID})
						case "reuse":
							_, err = b.resolveLease(ctx, claim.LeaseID, claim.RepoRoot, false)
						case "status":
							_, err = b.Status(ctx, core.StatusRequest{ID: claim.LeaseID})
						case "list":
							_, err = b.List(ctx, core.ListRequest{})
						}
						done <- err
					}()
					time.Sleep(time.Second)
					var err error
					returned := false
					select {
					case err = <-done:
						returned = true
					default:
					}
					close(release)
					holderErr := <-holderDone
					if !returned {
						err = <-done
					}
					if holderErr != nil {
						t.Fatal(holderErr)
					}
					if !returned || !errors.Is(err, context.DeadlineExceeded) {
						t.Errorf("operation=%s returned_before_release=%t err=%v", operation, returned, err)
					}
					wantCalls := 0
					if operation == "list" {
						wantCalls = 1
					}
					if len(f.runner.requests) != wantCalls {
						t.Errorf("native calls=%v want count=%d", f.runner.requests, wantCalls)
					}
					requireNoMachineMutation(t, f)
					requireClaimUnchanged(t, claim)
				})
			})
		}
	}
}

func TestDeletionDefaultBudgetIncludesClaimFence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, f, claim := identityFixture(t)
		b.rt.Exec = claimContextRunner{inner: f.runner}
		entered, release := make(chan struct{}), make(chan struct{})
		holderDone := make(chan error, 1)
		go func() {
			holderDone <- core.WithDurableLeaseClaimLockContext(t.Context(), claim.LeaseID, func(*core.LeaseClaim, bool, func() error) error { close(entered); <-release; return nil })
		}()
		<-entered
		done := make(chan error, 1)
		go func() { done <- b.removeBoundLease(context.Background(), claim) }()
		time.Sleep(31 * time.Second)
		var err error
		returned := false
		select {
		case err = <-done:
			returned = true
		default:
		}
		close(release)
		holderErr := <-holderDone
		if !returned {
			err = <-done
		}
		if holderErr != nil {
			t.Fatal(holderErr)
		}
		if !returned || !errors.Is(err, context.DeadlineExceeded) || len(f.runner.requests) != 0 {
			t.Errorf("default deletion budget returned=%t err=%v native=%v", returned, err, f.runner.requests)
		}
		requireClaimUnchanged(t, claim)
	})
}

func TestAcquisitionPublicationRollbackIncludesClaimFence(t *testing.T) {
	for _, successor := range []bool{false, true} {
		t.Run(fmt.Sprint(successor), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				t.Setenv("HOME", t.TempDir())
				oldOS, oldArch := hostGOOS, hostGOARCH
				hostGOOS, hostGOARCH = "darwin", "arm64"
				t.Cleanup(func() { hostGOOS, hostGOARCH = oldOS, oldArch })
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				runner := &recordingRunner{}
				f := newMachineFixture(t, runner)
				b := testBackend(runner)
				b.rt.Exec = claimContextRunner{inner: runner}
				entered, release := make(chan struct{}), make(chan struct{})
				holderDone := make(chan error, 1)
				var leaseID, name string
				var successorClaim core.LeaseClaim
				holding := false
				f.before = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
					if len(req.Args) > 2 && req.Args[1] == "inspect" && !holding {
						holding = true
						name = req.Args[2]
						leaseID = "cbx_" + strings.TrimPrefix(name, "crabbox-")
						go func() {
							holderDone <- core.WithDurableLeaseClaimLockContext(t.Context(), leaseID, func(claim *core.LeaseClaim, exists bool, persist func() error) error {
								if exists {
									return errors.New("fixture expected absent publication claim")
								}
								close(entered)
								<-release
								if successor {
									identity, err := readMachineIdentity(f.root, name)
									if err != nil {
										return err
									}
									*claim = core.LeaseClaim{LeaseID: leaseID, Provider: providerName, Slug: "successor", CloudID: name, CloudImmutableID: identity, ProviderScope: f.root, RepoRoot: "successor", Labels: map[string]string{"apple_machine_storage": f.root}}
									if err := persist(); err != nil {
										return err
									}
									successorClaim = *claim
								}
								return nil
							})
						}()
						<-entered
					}
					return core.LocalCommandResult{}, nil, false
				}
				home, err := os.UserHomeDir()
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { _, err := b.createLease(ctx, core.Repo{Root: home}, false, ""); done <- err }()
				<-entered
				synctest.Wait()
				if _, err := readMachineIdentity(f.root, name); err != nil {
					close(release)
					<-holderDone
					<-done
					t.Fatalf("fixture did not reach publication after marker creation: %v", err)
				}
				nativeBefore := len(runner.requests)
				cancel()
				wait := 31 * time.Second
				if successor {
					wait = time.Second
				}
				time.Sleep(wait)
				var runErr error
				returned := false
				select {
				case runErr = <-done:
					returned = true
				default:
				}
				close(release)
				holderErr := <-holderDone
				if !returned {
					runErr = <-done
				}
				if holderErr != nil {
					t.Fatal(holderErr)
				}
				if !errors.Is(runErr, context.Canceled) || !strings.Contains(runErr.Error(), "retained machine="+name) {
					t.Errorf("primary cause/retention lost: %v", runErr)
				}
				publicCode := 1
				var public core.ExitError
				if core.AsExitError(runErr, &public) {
					publicCode = public.Code
				}
				if publicCode != 1 {
					t.Errorf("rollback replaced primary code: %d err=%v", publicCode, runErr)
				}
				if !strings.Contains(public.Message, "retained machine="+name) {
					t.Errorf("CLI lost original recovery diagnostic: %q", public.Message)
				}
				if !successor && (!returned || !errors.Is(runErr, context.DeadlineExceeded)) {
					t.Errorf("detached default budget returned=%t err=%v", returned, runErr)
				}
				if len(runner.requests) != nativeBefore {
					t.Errorf("native calls after canceled publication: %v", runner.requests[nativeBefore:])
				}
				if successor {
					requireClaimUnchanged(t, successorClaim)
				} else if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
					t.Errorf("claim exists=%t err=%v", exists, err)
				}
				if _, err := readMachineIdentity(f.root, name); err != nil {
					t.Errorf("original machine witness lost: %v", err)
				}
			})
		})
	}
}

func TestClaimContextLookupsRemainReadOnly(t *testing.T) {
	b, f, claim := identityFixture(t)
	b.rt.Exec = claimContextRunner{inner: f.runner}
	if _, err := b.List(t.Context(), core.ListRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Status(t.Context(), core.StatusRequest{ID: claim.LeaseID}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.resolveLease(t.Context(), claim.LeaseID, "", false); err != nil {
		t.Fatal(err)
	}
	requireNoMachineMutation(t, f)
	requireClaimUnchanged(t, claim)
}

func TestStandaloneControlKeepsDefaultBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := testBackend(&recordingRunner{})
		b.rt.Exec = claimContextRunner{run: func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) != 30*time.Second || req.MaxCapturedOutputBytes != 1024*1024 || req.CancelGracePeriod != time.Second {
				t.Errorf("standalone control contract: deadline=%v request=%+v", deadline, req)
			}
			<-ctx.Done()
			return core.LocalCommandResult{}, ctx.Err()
		}}
		if _, err := b.control(context.Background(), []string{"machine", "list", "--format", "json"}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("control did not retain default deadline: %v", err)
		}
	})
}

func TestNativeControlFailuresPreserveCauseAndPublicCode(t *testing.T) {
	for _, action := range []string{"create", "inspect", "list", "remove"} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deadline=%t", action, deadline), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					cause := error(context.Canceled)
					if deadline {
						var c context.CancelFunc
						ctx, c = context.WithTimeout(t.Context(), 100*time.Millisecond)
						defer c()
						cause = context.DeadlineExceeded
					}
					b := testBackend(&recordingRunner{})
					b.rt.Exec = claimContextRunner{run: func(callCtx context.Context, _ core.LocalCommandRequest) (core.LocalCommandResult, error) {
						if !deadline {
							cancel()
						}
						<-callCtx.Done()
						return core.LocalCommandResult{Stderr: "synthetic diagnostic"}, callCtx.Err()
					}}
					var err error
					wantCode := 5
					var wantMessage string
					switch action {
					case "create":
						err = b.createMachine(ctx, "crabbox-test")
						wantMessage = "create Apple container machine: synthetic diagnostic"
					case "inspect":
						_, err = b.inspectMachine(ctx, "crabbox-test")
						wantCode = 4
						wantMessage = "Apple container machine \"crabbox-test\" not found: synthetic diagnostic"
					case "list":
						_, err = b.listMachines(ctx)
						wantMessage = "list Apple container machines: synthetic diagnostic"
					case "remove":
						err = b.removeMachine(ctx, "crabbox-test")
						wantMessage = "delete Apple container machine \"crabbox-test\": synthetic diagnostic"
					}
					var public core.ExitError
					if !errors.Is(err, cause) || !core.AsExitError(err, &public) || public.Code != wantCode || public.Message != wantMessage {
						t.Fatalf("cause/code/message changed: err=%v public=%+v want cause=%v code=%d message=%q", err, public, cause, wantCode, wantMessage)
					}
				})
			})
		}
	}
}

type machineFixture struct {
	root     string
	machines map[string]machine
	runner   *recordingRunner
	before   func(core.LocalCommandRequest) (core.LocalCommandResult, error, bool)
}

func newMachineFixture(t *testing.T, runner *recordingRunner) *machineFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &machineFixture{root: root, machines: map[string]machine{}, runner: runner}
	runner.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if f.before != nil {
			if result, err, handled := f.before(req); handled {
				return result, err, true
			}
		}
		args := req.Args
		if strings.Join(args, " ") == "system status --format json" {
			data, _ := json.Marshal(map[string]string{"status": "running", "appRoot": f.root})
			return core.LocalCommandResult{Stdout: string(data)}, nil, true
		}
		if len(args) < 3 || args[0] != "machine" {
			return core.LocalCommandResult{}, nil, false
		}
		switch args[1] {
		case "create":
			name := args[3]
			f.add(t, name)
			return core.LocalCommandResult{}, nil, true
		case "inspect":
			item, ok := f.machines[args[2]]
			if !ok {
				return core.LocalCommandResult{}, errors.New("not found"), true
			}
			data, _ := json.Marshal([]machine{item})
			return core.LocalCommandResult{Stdout: string(data)}, nil, true
		case "list":
			items := []machine{}
			for _, item := range f.machines {
				items = append(items, item)
			}
			data, _ := json.Marshal(items)
			return core.LocalCommandResult{Stdout: string(data)}, nil, true
		case "rm":
			if _, ok := f.machines[args[2]]; !ok {
				return core.LocalCommandResult{}, errors.New("not found"), true
			}
			dir, err := machineDirectory(f.root, args[2])
			if err != nil {
				return core.LocalCommandResult{}, err, true
			}
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			delete(f.machines, args[2])
			return core.LocalCommandResult{}, nil, true
		}
		return core.LocalCommandResult{}, nil, false
	}
	return f
}

func (f *machineFixture) add(t *testing.T, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(f.root, "plugin-state", "machine-apiserver", "machines", name), 0o700); err != nil {
		t.Fatal(err)
	}
	f.machines[name] = machine{ID: name, Status: "stopped"}
}

func (f *machineFixture) claim(t *testing.T, leaseID, slug, repo string) core.LeaseClaim {
	t.Helper()
	name := machineName(leaseID)
	f.add(t, name)
	identity, err := createMachineIdentity(f.root, name)
	if err != nil {
		t.Fatal(err)
	}
	b := testBackend(f.runner)
	server := machineServer(f.machines[name], leaseID, slug, b.cfg)
	server.ImmutableID = identity
	server.Labels["apple_machine_storage"] = f.root
	claim, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(leaseID, slug, b.cfg, f.root, server, core.SSHTarget{}, repo, time.Hour, false, core.LeaseClaim{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Revision == "" {
		t.Fatal("claim not published")
	}
	return claim
}

func identityFixture(t *testing.T) (*backend, *machineFixture, core.LeaseClaim) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := &recordingRunner{}
	f := newMachineFixture(t, runner)
	claim := f.claim(t, "cbx_0123456789ab", "test-machine", t.TempDir())
	return testBackend(runner), f, claim
}

func requireClaimUnchanged(t *testing.T, claim core.LeaseClaim) {
	t.Helper()
	got, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID)
	if err != nil || !exists || !reflect.DeepEqual(got, claim) {
		t.Fatalf("claim changed: exists=%v err=%v", exists, err)
	}
}

func requireNoMachineMutation(t *testing.T, f *machineFixture) {
	t.Helper()
	for _, req := range f.runner.requests {
		if len(req.Args) > 1 && req.Args[0] == "machine" && (req.Args[1] == "rm" || req.Args[1] == "run" || req.Args[1] == "create") {
			t.Fatalf("unexpected mutation: %v", req.Args)
		}
	}
}

func TestStopRejectsUnboundAndReplacedMachines(t *testing.T) {
	for _, kind := range []string{"legacy", "wrong-provider", "wrong-scope", "wrong-name", "wrong-label", "missing-marker", "replacement", "symlink-marker", "symlink-bundle", "malformed-marker", "daemon-root-changed"} {
		t.Run(kind, func(t *testing.T) {
			b, f, claim := identityFixture(t)
			dir, _ := machineDirectory(f.root, claim.CloudID)
			marker := filepath.Join(dir, machineOwnershipFile)
			changed := claim
			changed.Labels = map[string]string{}
			for k, v := range claim.Labels {
				changed.Labels[k] = v
			}
			switch kind {
			case "legacy":
				changed.CloudID = ""
				changed.CloudImmutableID = ""
				changed.ProviderScope = ""
				changed.Labels = nil
			case "wrong-provider":
				changed.Provider = "tart"
			case "wrong-scope":
				changed.ProviderScope = f.root + "-other"
			case "wrong-name":
				changed.CloudID = "crabbox-other"
			case "wrong-label":
				changed.Labels["lease"] = "cbx_ffffffffffff"
			case "missing-marker":
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
			case "replacement":
				if err := os.WriteFile(marker, []byte(strings.Repeat("a", 64)+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink-marker":
				if err := os.Rename(marker, marker+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(marker+"-saved", marker); err != nil {
					t.Skip(err)
				}
			case "symlink-bundle":
				if err := os.Rename(dir, dir+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dir+"-saved", dir); err != nil {
					t.Skip(err)
				}
			case "malformed-marker":
				if err := os.WriteFile(marker, []byte("not an identity"), 0600); err != nil {
					t.Fatal(err)
				}
			case "daemon-root-changed":
				f.root = t.TempDir()
			}
			if !reflect.DeepEqual(claim, changed) {
				var err error
				changed, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, changed)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := b.Stop(t.Context(), core.StopRequest{ID: claim.LeaseID}); err == nil {
				t.Fatal("unsafe stop succeeded")
			}
			requireNoMachineMutation(t, f)
			requireClaimUnchanged(t, changed)
		})
	}
}

func TestStopBoundStoppedMachineAndConfirmAbsence(t *testing.T) {
	b, f, claim := identityFixture(t)
	t.Setenv("CONTAINER_APP_ROOT", t.TempDir())
	if err := b.Stop(t.Context(), core.StopRequest{ID: claim.Slug}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
		t.Fatalf("claim remains: %v %v", exists, err)
	}
	if len(f.machines) != 0 {
		t.Fatal("machine remains")
	}
	for _, req := range f.runner.requests {
		if len(req.Args) > 1 && req.Args[1] == "run" {
			t.Fatal("stop booted machine")
		}
	}
	last := f.runner.requests[len(f.runner.requests)-1]
	if strings.Join(last.Args, " ") != "machine list --format json" {
		t.Fatalf("no final inventory: %v", last.Args)
	}
}

func TestDeleteRetainsClaimWhenRemovalOrInventoryIsUncertain(t *testing.T) {
	for _, kind := range []string{"rm-failure", "still-present", "empty", "null", "invalid", "duplicate", "missing-id", "missing-status", "wrong-shape", "root-switch"} {
		t.Run(kind, func(t *testing.T) {
			b, f, claim := identityFixture(t)
			removed := false
			f.before = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				key := strings.Join(req.Args, " ")
				if key == "machine rm "+claim.CloudID {
					if kind == "rm-failure" {
						return core.LocalCommandResult{}, errors.New("delete failed"), true
					}
					removed = true
					if kind == "still-present" {
						return core.LocalCommandResult{}, nil, true
					}
				}
				if removed && key == "system status --format json" && kind == "root-switch" {
					return core.LocalCommandResult{Stdout: `{"status":"running","appRoot":"/"}`}, nil, true
				}
				if removed && key == "machine list --format json" {
					outputs := map[string]string{"empty": "", "null": "null", "invalid": "[", "duplicate": `[{"id":"other","status":"stopped"},{"id":"other","status":"stopped"}]`, "missing-id": `[{"status":"stopped"}]`, "missing-status": `[{"id":"other"}]`, "wrong-shape": "{}"}
					if output, ok := outputs[kind]; ok {
						return core.LocalCommandResult{Stdout: output}, nil, true
					}
				}
				return core.LocalCommandResult{}, nil, false
			}
			if err := b.Stop(t.Context(), core.StopRequest{ID: claim.LeaseID}); err == nil {
				t.Fatal("uncertain deletion succeeded")
			}
			requireClaimUnchanged(t, claim)
		})
	}
}

func TestStopRetriesConfirmedAbsenceWithoutAnotherDelete(t *testing.T) {
	b, f, claim := identityFixture(t)
	removed := false
	f.before = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		key := strings.Join(req.Args, " ")
		if key == "machine rm "+claim.CloudID {
			removed = true
		}
		if removed && key == "machine list --format json" {
			return core.LocalCommandResult{}, errors.New("transient inventory failure"), true
		}
		return core.LocalCommandResult{}, nil, false
	}
	if err := b.Stop(t.Context(), core.StopRequest{ID: claim.LeaseID}); err == nil {
		t.Fatal("uncertain deletion succeeded")
	}
	requireClaimUnchanged(t, claim)
	if len(f.machines) != 0 {
		t.Fatal("fixture did not delete machine")
	}
	f.before = nil
	f.runner.requests = nil
	if err := b.Stop(t.Context(), core.StopRequest{ID: claim.LeaseID}); err != nil {
		t.Fatal(err)
	}
	requireNoMachineMutation(t, f)
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
		t.Fatal("confirmed absence retained claim")
	}
}

func TestAbsentInventoryDoesNotRetireExistingStorage(t *testing.T) {
	b, f, claim := identityFixture(t)
	delete(f.machines, claim.CloudID)
	if err := b.Stop(t.Context(), core.StopRequest{ID: claim.LeaseID}); err == nil {
		t.Fatal("retired claim while bundle still exists")
	}
	requireNoMachineMutation(t, f)
	requireClaimUnchanged(t, claim)
}

func TestCleanupRejectsChangedClaimBeforeProviderAdmission(t *testing.T) {
	b, f, claim := identityFixture(t)
	changed := claim
	changed.RepoRoot = t.TempDir()
	changed, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.removeBoundLease(t.Context(), claim); err == nil {
		t.Fatal("stale cleanup succeeded")
	}
	if len(f.runner.requests) != 0 {
		t.Fatal("provider called before claim admission")
	}
	requireClaimUnchanged(t, changed)
}

func TestCleanupFencesClaimThroughProviderDeletion(t *testing.T) {
	b, f, claim := identityFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.before = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if len(req.Args) > 1 && req.Args[1] == "rm" {
			close(entered)
			<-release
		}
		return core.LocalCommandResult{}, nil, false
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- b.removeBoundLease(context.Background(), claim) }()
	<-entered
	writerStarted, writerDone := make(chan struct{}), make(chan error, 1)
	go func() { close(writerStarted); writerDone <- core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim) }()
	<-writerStarted
	select {
	case err := <-writerDone:
		close(release)
		t.Fatalf("claim fence escaped: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
	if err := <-writerDone; err == nil {
		t.Fatal("stale writer succeeded after deletion")
	}
}

func TestLegacyReuseCannotAdoptMachine(t *testing.T) {
	b, f, claim := identityFixture(t)
	legacy := claim
	legacy.CloudImmutableID = ""
	legacy, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.resolveLease(t.Context(), claim.LeaseID, t.TempDir(), true); err == nil {
		t.Fatal("legacy reuse succeeded")
	}
	requireClaimUnchanged(t, legacy)
	requireNoMachineMutation(t, f)
	if len(f.runner.requests) != 0 {
		t.Fatal("legacy binding reached native runtime")
	}
}

func TestReuseRetainsIdentityAndHonorsRepositoryReclaim(t *testing.T) {
	b, f, claim := identityFixture(t)
	repo := t.TempDir()
	if _, err := b.resolveLease(t.Context(), claim.LeaseID, repo, false); err == nil {
		t.Fatal("cross-repo reuse without reclaim succeeded")
	}
	requireClaimUnchanged(t, claim)
	updated, err := b.resolveLease(t.Context(), claim.LeaseID, repo, true)
	if err != nil {
		t.Fatal(err)
	}
	if updated.RepoRoot != repo || updated.CloudID != claim.CloudID || updated.CloudImmutableID != claim.CloudImmutableID || updated.ProviderScope != claim.ProviderScope || updated.Revision == claim.Revision {
		t.Fatalf("unexpected refresh: %+v", updated)
	}
	requireNoMachineMutation(t, f)
}

func TestAcquisitionRollbackRequiresOriginalBindingAndClaim(t *testing.T) {
	for _, kind := range []string{"ready-failure", "replacement", "binding-failure", "claim-appeared"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			originalGOOS, originalGOARCH := hostGOOS, hostGOARCH
			hostGOOS, hostGOARCH = "darwin", "arm64"
			t.Cleanup(func() { hostGOOS, hostGOARCH = originalGOOS, originalGOARCH })
			home, err := os.UserHomeDir()
			if err != nil {
				t.Fatal(err)
			}
			runner := &recordingRunner{}
			f := newMachineFixture(t, runner)
			b := testBackend(runner)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var leaseID string
			var successor core.LeaseClaim
			f.before = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				if len(req.Args) > 3 && req.Args[1] == "create" {
					name := req.Args[3]
					leaseID = "cbx_" + strings.TrimPrefix(name, "crabbox-")
					if kind == "binding-failure" {
						return core.LocalCommandResult{}, nil, true
					}
					if kind == "claim-appeared" {
						if err := core.ClaimLeaseForRepoProvider(leaseID, "successor", providerName, home, time.Hour, false); err != nil {
							t.Fatal(err)
						}
						successor, _, err = core.ReadLeaseClaimWithPresence(leaseID)
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				if len(req.Args) > 3 && req.Args[1] == "run" {
					if kind == "replacement" {
						dir, err := machineDirectory(f.root, req.Args[3])
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(dir, machineOwnershipFile), []byte(strings.Repeat("a", 64)+"\n"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					cancel()
					return core.LocalCommandResult{}, context.Canceled, true
				}
				return core.LocalCommandResult{}, nil, false
			}
			if _, err := b.createLease(ctx, core.Repo{Root: filepath.Join(home, "src", "fixture")}, false, ""); err == nil {
				t.Fatal("failed acquisition succeeded")
			}
			removed := false
			for _, req := range runner.requests {
				if len(req.Args) > 1 && req.Args[1] == "rm" {
					removed = true
				}
			}
			if removed != (kind == "ready-failure") {
				t.Fatalf("cleanup=%v for %s", removed, kind)
			}
			if kind == "claim-appeared" {
				requireClaimUnchanged(t, successor)
			}
			if kind == "replacement" {
				if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || !exists {
					t.Fatal("replacement lost claim")
				}
			}
			if kind == "ready-failure" {
				if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
					t.Fatal("rollback left claim")
				}
				if len(f.machines) != 0 {
					t.Fatal("rollback left machine")
				}
			}
		})
	}
}

func TestAcquisitionRejectsEmptyRepositoryBeforeMutation(t *testing.T) {
	b, f, _ := identityFixture(t)
	originalGOOS, originalGOARCH := hostGOOS, hostGOARCH
	hostGOOS, hostGOARCH = "darwin", "arm64"
	defer func() { hostGOOS, hostGOARCH = originalGOOS, originalGOARCH }()
	if _, err := b.createLease(t.Context(), core.Repo{}, false, ""); err == nil {
		t.Fatal("empty repo accepted")
	}
	requireNoMachineMutation(t, f)
}

func TestControlRejectsPartialOrInvalidInspection(t *testing.T) {
	for _, output := range []string{"", "null", "[]", `{}`, `[{"id":"other","status":"running"}]`, `[{"id":"crabbox-test"}]`, `[{"id":"crabbox-test","status":"running"}] trailing`} {
		runner := &recordingRunner{fallback: core.LocalCommandResult{Stdout: output}}
		if _, err := testBackend(runner).inspectMachine(t.Context(), "crabbox-test"); err == nil {
			t.Fatalf("accepted %q", output)
		}
	}
	for _, result := range []core.LocalCommandResult{{ExitCode: 1, Stdout: `[{"id":"crabbox-test","status":"running"}]`}, {Stdout: strings.Repeat("x", 1024*1024+1)}} {
		runner := &recordingRunner{fallback: result}
		if _, err := testBackend(runner).inspectMachine(t.Context(), "crabbox-test"); err == nil {
			t.Fatal("accepted failed/oversized response")
		}
		if runner.requests[0].MaxCapturedOutputBytes == 0 {
			t.Fatal("unbounded control capture")
		}
	}
}

func TestStorageRootRequiresDaemonEvidence(t *testing.T) {
	for _, output := range []string{"", "null", `{}`, `{"status":"stopped","appRoot":"/"}`, `{"status":"running","appRoot":"relative"}`, `{"status":"running"}`} {
		runner := &recordingRunner{fallback: core.LocalCommandResult{Stdout: output}}
		if _, err := testBackend(runner).storageRoot(t.Context()); err == nil {
			t.Fatalf("accepted %q", output)
		}
	}
}
