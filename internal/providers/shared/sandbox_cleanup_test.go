package shared

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestSandboxCleanupRetainsLockedAdmissionAndMutationOrder(t *testing.T) {
	for _, mode := range []string{"delete", "delete-canceled", "dry-run", "forget-missing", "retain-missing", "disappeared", "foreign-scope"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const id = "cbx_aaaaaaaaaaaa"
			scope := "scope"
			if mode == "foreign-scope" {
				scope = "foreign"
			}
			server := core.Server{Provider: "example", CloudID: "sandbox", Labels: map[string]string{"lease": id, "slug": "alpha"}}
			if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(id, "alpha", "example", scope, "", t.TempDir(), time.Minute, false, server, core.SSHTarget{}); err != nil {
				t.Fatal(err)
			}
			stored, err := core.ReadLeaseClaim(id)
			if err != nil {
				t.Fatal(err)
			}
			listed := stored
			listed.ProviderScope = "scope"
			var stdout, stderr bytes.Buffer
			var events []string
			locked := false
			observe := func(event string) {
				if !locked {
					t.Errorf("%s escaped the operation lock", event)
				}
				events = append(events, event)
			}
			missing := errors.New("missing")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cleanup := SandboxClaimCleanup[string]{
				Provider: "example", Runtime: core.Runtime{Stdout: &stdout, Stderr: &stderr},
				MatchesScope: func(claim core.LeaseClaim) bool { return claim.ProviderScope == "scope" },
				Lock: func(context.Context, string) (func(), error) {
					locked = true
					events = append(events, "lock")
					if mode == "disappeared" {
						if err := core.RemoveLeaseClaimIfUnchanged(id, stored); err != nil {
							return nil, err
						}
					}
					return func() { events = append(events, "unlock"); locked = false }, nil
				},
				SandboxID: func(claim core.LeaseClaim) string { return claim.CloudID },
				Get: func(context.Context, string) (string, error) {
					observe("get")
					if strings.HasSuffix(mode, "missing") {
						return "", missing
					}
					return "sandbox", nil
				},
				IsNotFound:    func(err error) bool { return err == missing },
				ForgetMissing: mode == "forget-missing", ForgetMissingHint: "example.forgetMissing",
				Due:      func(core.LeaseClaim, time.Time) (bool, string) { observe("due"); return true, "expired" },
				Validate: func(core.LeaseClaim, string) error { observe("validate"); return nil },
				Delete: func(context.Context, string) error {
					observe("delete")
					if mode == "delete-canceled" {
						cancel()
					}
					return nil
				},
			}
			if err := CleanupSandboxClaims(ctx, core.CleanupRequest{DryRun: mode == "dry-run"}, []core.LeaseClaim{listed}, cleanup); err != nil {
				t.Fatal(err)
			}
			want := []string{"lock", "get", "due", "validate", "delete", "unlock"}
			if mode == "dry-run" {
				want = []string{"lock", "get", "due", "validate", "unlock"}
			} else if strings.HasSuffix(mode, "missing") {
				want = []string{"lock", "get", "unlock"}
			} else if mode == "disappeared" || mode == "foreign-scope" {
				want = []string{"lock", "unlock"}
			}
			if !reflect.DeepEqual(events, want) || locked {
				t.Fatalf("events=%v want=%v locked=%v", events, want, locked)
			}
			current, err := core.ReadLeaseClaim(id)
			if err != nil {
				t.Fatal(err)
			}
			wantRemoved := mode == "delete" || mode == "delete-canceled" || mode == "forget-missing" || mode == "disappeared"
			if (current.LeaseID == "") != wantRemoved {
				t.Fatalf("claim removal=%v want=%v", current.LeaseID == "", wantRemoved)
			}
			if mode == "dry-run" && (!strings.Contains(stdout.String(), "would delete sandbox=sandbox") || strings.Contains(stdout.String(), "cleanup removed=")) {
				t.Fatalf("dry-run output=%q", stdout.String())
			}
		})
	}
}

type sandboxClaimWaitContext struct {
	context.Context
	armed   atomic.Bool
	once    sync.Once
	waiting chan struct{}
}

func (c *sandboxClaimWaitContext) Done() <-chan struct{} {
	if c.armed.Load() {
		c.once.Do(func() { close(c.waiting) })
	}
	return c.Context.Done()
}

func TestSandboxForgetMissingCancellationReleasesOperationLock(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const id = "cbx_aaaaaaaaaaaa"
	server := core.Server{Provider: "example", CloudID: "sandbox", Labels: map[string]string{"lease": id, "slug": "alpha"}}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(id, "alpha", "example", "scope", "", t.TempDir(), time.Minute, false, server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := &sandboxClaimWaitContext{Context: ctx, waiting: make(chan struct{})}
	var stdout, stderr bytes.Buffer
	deletes := 0
	missing := errors.New("synthetic missing or inaccessible")
	cleanup := SandboxClaimCleanup[string]{
		Provider: "example", Runtime: core.Runtime{Stdout: &stdout, Stderr: &stderr},
		MatchesScope: func(c core.LeaseClaim) bool { return c.ProviderScope == "scope" },
		Lock:         func(ctx context.Context, id string) (func(), error) { return LockLeaseOperation(ctx, "example", id) },
		SandboxID:    func(c core.LeaseClaim) string { return c.CloudID },
		Get:          func(context.Context, string) (string, error) { observed.armed.Store(true); return "", missing },
		IsNotFound:   func(err error) bool { return err == missing }, ForgetMissing: true,
		Delete: func(context.Context, string) error { deletes++; return nil },
	}
	held, release, ownerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var ownerErr error
	var unlock sync.Once
	go func() {
		defer close(ownerDone)
		ownerErr = core.WithDurableLeaseClaimLock(id, func(*core.LeaseClaim, bool, func() error) error { close(held); <-release; return nil })
	}()
	defer func() { unlock.Do(func() { close(release) }); <-ownerDone }()
	select {
	case <-held:
	case <-ownerDone:
		t.Fatal(ownerErr)
	}
	done := make(chan struct{})
	var cleanupErr error
	go func() {
		defer close(done)
		cleanupErr = CleanupSandboxClaims(observed, core.CleanupRequest{}, []core.LeaseClaim{claim}, cleanup)
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
	lockCtx, lockCancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	operationUnlock, operationErr := LockLeaseOperation(lockCtx, "example", id)
	lockCancel()
	if operationErr == nil {
		operationUnlock()
	}
	unlock.Do(func() { close(release) })
	<-ownerDone
	<-done
	if ownerErr != nil || !waitObserved || !returnedWhileHeld || !errors.Is(cleanupErr, context.Canceled) || operationErr != nil {
		t.Fatalf("wait observed=%v returned while held=%v cleanup=%v owner=%v operation lock=%v", waitObserved, returnedWhileHeld, cleanupErr, ownerErr, operationErr)
	}
	current, exists, err := core.ReadLeaseClaimWithPresence(id)
	if err != nil || !exists || !reflect.DeepEqual(current, claim) {
		t.Fatal("canceled cleanup changed exact claim")
	}
	if deletes != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("deletes=%d stdout=%q stderr=%q", deletes, stdout.String(), stderr.String())
	}
}
