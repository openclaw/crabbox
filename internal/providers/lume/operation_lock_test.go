package lume

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCapacityLockSerializes(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	unlock, err := lockLumeCapacity(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(bg, 100*time.Millisecond)
	defer cancel()
	if _, err := lockLumeCapacity(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended lock error=%v want context deadline exceeded", err)
	}

	unlock()
	ctx, cancel = context.WithTimeout(bg, time.Second)
	defer cancel()
	secondUnlock, err := lockLumeCapacity(ctx)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	secondUnlock()
}

type terminalClaimWaitContext struct {
	context.Context
	armed   atomic.Bool
	once    sync.Once
	waiting chan struct{}
}

func (c *terminalClaimWaitContext) Done() <-chan struct{} {
	if c.armed.Load() {
		c.once.Do(func() { close(c.waiting) })
	}
	return c.Context.Done()
}

func TestTerminalCancellationReleasesCapacityWhileClaimLocked(t *testing.T) {
	for _, operation := range []string{"release", "cleanup"} {
		for _, missing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/missing=%v", operation, missing), func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("XDG_CONFIG_HOME", join(home, ".config"))
				t.Setenv("XDG_STATE_HOME", join(home, ".local", "state"))
				const leaseID, name = "cbx_cancelterminal", "crabbox-cancel-terminal"
				storage := join(home, ".lume")
				putVM(t, home, name, "bHVtZS1jYW5jZWxsYXRpb24=")
				storageID, err := ensureLumeStorageIdentity(storage)
				must(t, err)
				cfg := configFor()
				identity, err := lumeVMImmutableID(cfg, lumeVM{Name: name, LocationName: "home"})
				must(t, err)
				server := core.Server{CloudID: name, ImmutableID: identity, Provider: providerName, Status: "stopped", Labels: labels{
					"lease": leaseID, "instance": name, "storage": "home", "storage_id": storageID, "state": "stopped", "run_owner_expected": "false",
				}}
				must(t, core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "cancel-terminal", providerName, instanceScope(name), "", t.TempDir(), time.Minute, false, server, core.SSHTarget{}))
				claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
				must(t, err)
				if !exists {
					t.Fatal("missing fixture claim")
				}
				key, _, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
				must(t, err)
				if missing {
					must(t, os.RemoveAll(join(storage, name)))
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				observed := &terminalClaimWaitContext{Context: ctx, waiting: make(chan struct{})}
				runner := &fake{hook: func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
					if len(req.Args) > 0 && (req.Args[0] == "ls" || req.Args[0] == "get") {
						// The fake runner does not consult ctx; capacity admission has
						// already finished before this initial inventory command.
						observed.armed.Store(true)
						if missing {
							if req.Args[0] == "ls" {
								return core.LocalCommandResult{Stdout: "[]"}, nil, true
							}
							return core.LocalCommandResult{ExitCode: 1, Stderr: "Error: Virtual machine not found: " + name}, errors.New("exit status 1"), true
						}
						return core.LocalCommandResult{Stdout: fmt.Sprintf(`[{"name":%q,"os":"macOS","status":"stopped","locationName":"home"}]`, name)}, nil, true
					}
					return core.LocalCommandResult{}, nil, false
				}}
				b := backendFor(cfg, runner)
				held, release, ownerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var ownerErr error
				var unlock sync.Once
				go func() {
					defer close(ownerDone)
					ownerErr = core.WithDurableLeaseClaimLock(leaseID, func(*core.LeaseClaim, bool, func() error) error { close(held); <-release; return nil })
				}()
				defer func() { unlock.Do(func() { close(release) }); <-ownerDone }()
				select {
				case <-held:
				case <-ownerDone:
					t.Fatal(ownerErr)
				}
				done := make(chan struct{})
				var operationErr error
				go func() {
					defer close(done)
					if operation == "release" {
						operationErr = b.ReleaseLease(observed, core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}})
					} else {
						operationErr = b.Cleanup(observed, core.CleanupRequest{})
					}
				}()
				defer func() { cancel(); unlock.Do(func() { close(release) }); <-done }()
				waitObserved := false
				select {
				case <-observed.waiting:
					waitObserved = true
				case <-done:
				case <-time.After(time.Second):
				}
				cancel()
				returnedWhileHeld := false
				select {
				case <-done:
					returnedWhileHeld = true
				case <-time.After(time.Second):
				}
				capacityCtx, capacityCancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
				capacityUnlock, capacityErr := lockLumeCapacity(capacityCtx)
				capacityCancel()
				if capacityErr == nil {
					capacityUnlock()
				}
				unlock.Do(func() { close(release) })
				<-ownerDone
				<-done
				if ownerErr != nil || !waitObserved || !returnedWhileHeld || !errors.Is(operationErr, context.Canceled) || capacityErr != nil {
					t.Fatalf("wait observed=%v returned while held=%v operation=%v owner=%v capacity=%v", waitObserved, returnedWhileHeld, operationErr, ownerErr, capacityErr)
				}
				current, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
				if err != nil || !exists || !reflect.DeepEqual(current, claim) {
					t.Fatal("cancellation changed exact claim")
				}
				if _, err := os.Stat(key); err != nil {
					t.Fatalf("cancellation removed key: %v", err)
				}
				if _, err := os.Stat(join(storage, name)); missing != errors.Is(err, os.ErrNotExist) {
					t.Fatalf("cancellation changed VM fixture: %v", err)
				}
				for _, call := range runner.calls {
					if len(call.Args) == 0 || (call.Args[0] != "ls" && call.Args[0] != "get") {
						t.Fatalf("provider mutation after canceled admission: %v", call.Args)
					}
				}
			})
		}
	}
}
