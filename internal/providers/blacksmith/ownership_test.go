package blacksmith

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

type ownershipRunner func(context.Context, LocalCommandRequest) (LocalCommandResult, error)

func (f ownershipRunner) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	return f(ctx, req)
}

func isolateBlacksmithOwnership(t *testing.T) {
	t.Helper()
	testutil.IsolateUserDirs(t)
	t.Setenv("BLACKSMITH_API_URL", "")
	t.Setenv("BLACKSMITH_ORG", "")
	t.Setenv("CRABBOX_ENV_ALLOW", "")
}

func TestParseBlacksmithIdentityNeverAssignedCompleted(t *testing.T) {
	const id = "tbx_01aaaaaaaaaaaaaaaaaaaaaaaa"
	// Synthetic identities, retaining the observed native 0.4.57 table framing.
	const output = "ID                              STATUS     IP  WORKFLOW                                  JOB  REF                                     CREATED                      RUN URL\n" +
		"tbx_01aaaaaaaaaaaaaaaaaaaaaaaa  completed      .github/workflows/ci-testing-testbox.yml  go   fix/queue-testbox-cleanup-verification  2026-09-02T13:54:37.000000Z  \n"
	identity, err := parseBlacksmithIdentity(output, id)
	if err != nil || identity != (blacksmithIdentity{ID: id, State: "completed", Workflow: ".github/workflows/ci-testing-testbox.yml", Job: "go", Ref: "fix/queue-testbox-cleanup-verification"}) {
		t.Fatalf("complete native row with empty IP/URL rejected: identity=%+v err=%v", identity, err)
	}
}

func TestParseBlacksmithIdentityRejectsTruncatedCompletion(t *testing.T) {
	const id = "tbx_truncated123"
	for _, output := range []string{nativeNeverAssignedStatus(id, "completed"), nativeStopStatus(id, "completed", "")} {
		for end := 0; end < len(output); end++ {
			if _, err := parseBlacksmithIdentity(output[:end], id); err == nil {
				t.Fatalf("accepted truncated completion: %q", output[:end])
			}
		}
	}
}

func TestBlacksmithStopRejectsUnownedIdentity(t *testing.T) {
	for _, kind := range []string{"missing", "legacy", "provider", "cloud-id", "slug", "revision", "repository", "scope", "workflow", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			const id = "tbx_guard123"
			if kind != "missing" {
				original := testOwnedBlacksmithClaim(t, id, "jade-krill", "/repo")
				changed := original
				changed.Labels = maps.Clone(original.Labels)
				switch kind {
				case "legacy":
					changed.ProviderScope = ""
				case "provider":
					changed.Provider = "foreign"
				case "cloud-id":
					changed.CloudID = "tbx_other"
				case "slug":
					changed.Slug = "different"
				case "revision":
					changed.Revision = ""
				case "repository":
					changed.RepoRoot = ""
				case "scope":
					changed.ProviderScope = `{"api":"https://backend.blacksmith.sh","org":"different"}`
				case "workflow":
					delete(changed.Labels, "workflow")
				case "duplicate":
					other := testOwnedBlacksmithClaim(t, "tbx_other", "other-krill", "/other")
					duplicate := other
					duplicate.CloudID = id
					if err := core.ReplaceLeaseClaimIfUnchanged(other.LeaseID, other, duplicate); err != nil {
						t.Fatal(err)
					}
				}
				// Revision-only absence needs a literal fixture edit; normal claim writes
				// intentionally mint a new revision rather than accepting an empty one.
				if kind == "revision" {
					path, err := testBlacksmithClaimPath(id)
					if err != nil {
						t.Fatal(err)
					}
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					data = []byte(strings.Replace(string(data), original.Revision, "", 1))
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
				} else if kind != "duplicate" {
					if err := core.ReplaceLeaseClaimIfUnchanged(id, original, changed); err != nil {
						t.Fatal(err)
					}
				}
			}
			calls := 0
			cfg := baseConfig()
			cfg.Blacksmith.Org = "example-org"
			backend := newTestBlacksmithBackend(cfg, ownershipRunner(func(context.Context, LocalCommandRequest) (LocalCommandResult, error) {
				calls++
				return LocalCommandResult{}, nil
			}))
			if err := backend.Stop(t.Context(), StopRequest{ID: id}); err == nil {
				t.Fatal("unowned stop succeeded")
			}
			if calls != 0 {
				t.Fatalf("unowned identity reached provider %d times", calls)
			}
		})
	}
}

func TestBlacksmithStopRequiresTerminalConfirmation(t *testing.T) {
	for _, mode := range []string{"success", "hydration-failed", "already-terminal", "stop-error", "missing", "duplicate", "changed-workflow", "still-running", "post-status-error"} {
		t.Run(mode, func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			const id = "tbx_terminal123"
			claim := testOwnedBlacksmithClaim(t, id, "jade-krill", "/repo")
			key, _, err := ensureTestboxKey(id)
			if err != nil {
				t.Fatal(err)
			}
			statusCalls, stopCalls := 0, 0
			cfg := baseConfig()
			cfg.Blacksmith.Org = "example-org"
			backend := newTestBlacksmithBackend(cfg, ownershipRunner(func(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
				if testBlacksmithFlag(req.Args, "--org") != "example-org" || testBlacksmithFlag(req.Args, "--api-url") != "https://backend.blacksmith.sh" {
					t.Fatalf("unbound route: %v", req.Args)
				}
				args := req.Args
				if args[0] == "--org" {
					args = args[2:]
				}
				if args[1] == "stop" {
					stopCalls++
					if mode == "stop-error" {
						return LocalCommandResult{ExitCode: 1}, errors.New("uncertain stop")
					}
					return LocalCommandResult{}, nil
				}
				if args[1] != "status" {
					t.Fatalf("unexpected action: %v", req.Args)
				}
				statusCalls++
				state := "ready"
				if mode == "hydration-failed" {
					state = "hydration_failed"
				}
				if mode == "already-terminal" || (stopCalls > 0 && mode != "still-running" && mode != "stop-error") {
					state = "completed"
				}
				output := testBlacksmithStatus(id, state)
				switch mode {
				case "missing":
					output = "ID STATUS IP WORKFLOW JOB REF CREATED RUN URL\n"
				case "duplicate":
					output += strings.Split(output, "\n")[1] + "\n"
				case "changed-workflow":
					output = strings.ReplaceAll(output, "testbox.yml", "foreign.yml")
				case "post-status-error":
					if stopCalls > 0 {
						return LocalCommandResult{ExitCode: 1}, errors.New("status unavailable")
					}
				}
				return LocalCommandResult{Stdout: output}, nil
			}))
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			err = backend.Stop(ctx, StopRequest{ID: id})
			got, readErr := readLeaseClaim(id)
			if readErr != nil {
				t.Fatal(readErr)
			}
			_, keyErr := os.Stat(key)
			success := mode == "success" || mode == "already-terminal" || mode == "hydration-failed"
			if success {
				if err != nil || got.LeaseID != "" || !os.IsNotExist(keyErr) {
					t.Fatalf("cleanup err=%v claim=%+v key=%v", err, got, keyErr)
				}
			} else if err == nil || got.Revision != claim.Revision || keyErr != nil {
				t.Fatalf("uncertain cleanup changed ownership err=%v claim=%+v key=%v", err, got, keyErr)
			}
			if mode == "already-terminal" && stopCalls != 0 {
				t.Fatal("repeated terminal stop")
			}
			if (mode == "success" || mode == "hydration-failed") && (stopCalls != 1 || statusCalls != 3) {
				t.Fatalf("calls stop=%d status=%d", stopCalls, statusCalls)
			}
		})
	}
}

func TestBlacksmithRollbackPreservesAppearingClaimAndExistingKey(t *testing.T) {
	for _, mode := range []string{"claim", "partial", "key"} {
		t.Run(mode, func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			const id = "tbx_rollback123"
			pending, _, err := ensureTestboxKey("tbx_pending_owned")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "claim" {
				testOwnedBlacksmithClaim(t, id, "jade-krill", "/repo")
			}
			if mode == "partial" {
				path, err := testBlacksmithClaimPath(id)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(`{"leaseID":`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var existing string
			if mode == "key" {
				existing, _, err = ensureTestboxKey(id)
				if err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			backend := newTestBlacksmithBackend(baseConfig(), ownershipRunner(func(context.Context, LocalCommandRequest) (LocalCommandResult, error) {
				calls++
				return LocalCommandResult{}, nil
			}))
			backend.route = &blacksmithRoute{API: blacksmithDefaultAPI, Org: "example-org"}
			backend.rollbackTestbox(id, "tbx_pending_owned", "/repo")
			if calls != 0 {
				t.Fatalf("conflict rollback reached provider %d times", calls)
			}
			if _, err := os.Stat(pending); err != nil {
				t.Fatal("pending key lost", err)
			}
			if existing != "" {
				if _, err := os.Stat(existing); err != nil {
					t.Fatal("existing key lost", err)
				}
			}
		})
	}
}

func TestBlacksmithCreationReceiptIsUnambiguous(t *testing.T) {
	for _, tc := range []struct{ output, want string }{{"tbx_created123\n", "tbx_created123"}, {"queued tbx_other\n", ""}, {"tbx_first\ntbx_second\n", ""}, {"tbx_same\ntbx_same\n", ""}, {"error: inspect tbx_other\ntbx_created123\n", "tbx_created123"}} {
		if got := blacksmithCreationReceipt(tc.output); got != tc.want {
			t.Errorf("receipt=%q want=%q", got, tc.want)
		}
	}
}

func TestBlacksmithUncertainReceiptRetainsRecoveryKey(t *testing.T) {
	for _, output := range []string{"", "tbx_first\ntbx_second\n", "tbx_same\ntbx_same\n"} {
		for _, failed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%q/failed=%t", output, failed), func(t *testing.T) {
				isolateBlacksmithOwnership(t)
				runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
					if req.Args[1] != "warmup" {
						t.Error("uncertain receipt selected a provider resource")
					}
					if failed {
						return LocalCommandResult{ExitCode: 1, Stdout: output}, errors.New("allocation acknowledgement lost")
					}
					return LocalCommandResult{Stdout: output}, nil
				}}
				cfg := baseConfig()
				cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
				_, err := newTestBlacksmithBackend(cfg, runner).warmupLease(t.Context(), Repo{Root: t.TempDir()}, false, "")
				if err == nil || !strings.Contains(err.Error(), "retained pending_key=tbx_pending_") {
					t.Fatalf("missing recovery diagnostic: %v", err)
				}
				path, pathErr := testboxKeyPath("tbx_placeholder")
				if pathErr != nil {
					t.Fatal(pathErr)
				}
				root := filepath.Dir(filepath.Dir(path))
				entries, readErr := os.ReadDir(root)
				if readErr != nil || len(entries) != 1 || !strings.Contains(err.Error(), entries[0].Name()) {
					t.Fatalf("recovery key directory missing: entries=%v err=%v", entries, readErr)
				}
				for _, name := range []string{"id_ed25519", "id_ed25519.pub"} {
					info, err := os.Stat(filepath.Join(root, entries[0].Name(), name))
					if err != nil || !info.Mode().IsRegular() {
						t.Fatalf("recovery key missing: %s err=%v", name, err)
					}
				}
				claims, claimErr := core.ListLeaseClaims()
				if claimErr != nil || len(claims) != 0 || len(runner.calls) != 2 {
					t.Fatalf("uncertain receipt acquired authority: claims=%v err=%v calls=%d", claims, claimErr, len(runner.calls))
				}
			})
		}
	}
}

func TestBlacksmithOneShotUncertainCleanupReportsRetention(t *testing.T) {
	for _, commandExit := range []int{0, 7} {
		t.Run(fmt.Sprint(commandExit), func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
				switch req.Args[1] {
				case "warmup":
					return LocalCommandResult{Stdout: "tbx_retained123\n"}, nil
				case "run":
					if commandExit != 0 {
						return LocalCommandResult{ExitCode: commandExit}, errors.New("command failed")
					}
					return LocalCommandResult{}, nil
				case "stop":
					return LocalCommandResult{ExitCode: 1}, errors.New("stop uncertain")
				}
				return LocalCommandResult{}, nil
			}}
			cfg := baseConfig()
			cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
			var stderr bytes.Buffer
			backend := newTestBlacksmithBackend(cfg, runner)
			backend.rt.Stderr = &stderr
			repo := t.TempDir()
			result, err := backend.Run(t.Context(), RunRequest{Repo: Repo{Root: repo}, Command: []string{"true"}, TimingJSON: true, EmitProof: filepath.Join(repo, "proof.md")})
			if err == nil || result.Session == nil || !result.Session.Kept {
				t.Fatalf("false cleanup success result=%+v err=%v", result, err)
			}
			if commandExit != 0 && result.ExitCode != commandExit {
				t.Fatalf("command exit replaced: %d", result.ExitCode)
			}
			stopCalls := 0
			for _, args := range runner.calls {
				if len(args) > 1 && args[0] == "testbox" && args[1] == "stop" {
					stopCalls++
				}
			}
			if stopCalls != 1 {
				t.Fatalf("cleanup attempted %d times", stopCalls)
			}
			var printed core.TimingReport
			for _, line := range strings.Split(stderr.String(), "\n") {
				if strings.HasPrefix(line, "{") {
					if err := json.Unmarshal([]byte(line), &printed); err != nil {
						t.Fatal(err)
					}
				}
			}
			if printed.ExitCode != result.ExitCode || printed.RunStatus != core.RunStatusFailed {
				t.Fatalf("printed timing=%+v result=%+v", printed, result)
			}
			if commandExit == 0 && (printed.ErrorKind != core.RunErrorProvider || printed.BlockedStage != "cleanup") {
				t.Fatalf("cleanup misclassified: %+v", printed)
			}
			for _, name := range []string{"metadata.json", "timing.json"} {
				data, err := os.ReadFile(filepath.Join(repo, ".crabbox", "runs", "tbx_retained123", name))
				if err != nil {
					t.Fatal(err)
				}
				var persisted struct {
					ExitCode int `json:"exitCode"`
				}
				if err := json.Unmarshal(data, &persisted); err != nil {
					t.Fatal(err)
				}
				if persisted.ExitCode != result.ExitCode || persisted.ExitCode == 0 {
					t.Fatalf("%s falsely reports success: %s", name, data)
				}
			}
			claim, err := readLeaseClaim("tbx_retained123")
			if err != nil || claim.LeaseID == "" {
				t.Fatal("claim lost", err)
			}
			key, err := testboxKeyPath(claim.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(key); err != nil {
				t.Fatal("key lost", err)
			}
		})
	}
}

func testBlacksmithClaimPath(id string) (string, error) {
	dir, err := core.CrabboxStateDir()
	return filepath.Join(dir, "claims", id+".json"), err
}

func TestBlacksmithWarmupPreservesExistingKey(t *testing.T) {
	isolateBlacksmithOwnership(t)
	const id = "tbx_existingkey123"
	path, _, err := ensureTestboxKey(id)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if req.Args[1] == "warmup" {
			return LocalCommandResult{Stdout: id + "\n"}, nil
		}
		if req.Args[1] == "stop" {
			stopped = true
		}
		return LocalCommandResult{}, nil
	}}
	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	if _, err := newTestBlacksmithBackend(cfg, runner).warmupLease(t.Context(), Repo{Root: "/repo"}, false, ""); err == nil {
		t.Fatal("acquisition replaced existing key")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) || stopped {
		t.Fatalf("existing key/resource changed: err=%v stopped=%t", err, stopped)
	}
	claim, err := readLeaseClaim(id)
	if err != nil || claim.LeaseID != "" {
		t.Fatalf("conflict claimed: %+v err=%v", claim, err)
	}
}

func TestBlacksmithStopFencesConcurrentWriter(t *testing.T) {
	isolateBlacksmithOwnership(t)
	const id = "tbx_writer123"
	claim := testOwnedBlacksmithClaim(t, id, "jade-krill", "/repo")
	entered, release := make(chan struct{}), make(chan struct{})
	stopped := false
	cfg := baseConfig()
	cfg.Blacksmith.Org = "example-org"
	backend := newTestBlacksmithBackend(cfg, ownershipRunner(func(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
		args := req.Args
		if args[0] == "--org" {
			args = args[2:]
		}
		if args[1] == "stop" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return LocalCommandResult{ExitCode: 1}, ctx.Err()
			}
			stopped = true
			return LocalCommandResult{}, nil
		}
		state := "ready"
		if stopped {
			state = "completed"
		}
		return LocalCommandResult{Stdout: testBlacksmithStatus(id, state)}, nil
	}))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- backend.Stop(ctx, StopRequest{ID: id}) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("stop never entered")
	}
	writer := make(chan error, 1)
	replacement := claim
	replacement.RepoRoot = "/new-owner"
	go func() { writer <- core.ReplaceLeaseClaimIfUnchanged(id, claim, replacement) }()
	select {
	case err := <-writer:
		close(release)
		t.Fatalf("writer crossed termination fence: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	stopErr, writerErr := <-done, <-writer
	current, err := readLeaseClaim(id)
	if err != nil {
		t.Fatal(err)
	}
	if writerErr == nil {
		if stopErr == nil || current.RepoRoot != replacement.RepoRoot {
			t.Fatalf("replacement lost during finalization: claim=%+v stop=%v", current, stopErr)
		}
	} else if stopErr != nil || current.LeaseID != "" {
		t.Fatalf("claim survived successful stop: %+v stop=%v writer=%v", current, stopErr, writerErr)
	}
}

func TestBlacksmithStopCancelsActiveRun(t *testing.T) {
	for _, hung := range []bool{false, true} {
		t.Run(fmt.Sprintf("hung=%t", hung), func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			const id = "tbx_cancel123"
			repo := t.TempDir()
			testOwnedBlacksmithClaim(t, id, "jade-krill", repo)
			key, _, err := ensureTestboxKey(id)
			if err != nil {
				t.Fatal(err)
			}
			entered, remoteStopped := make(chan struct{}), make(chan struct{})
			var stopped atomic.Bool
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cfg := baseConfig()
			cfg.Blacksmith.Org = "example-org"
			backend := newTestBlacksmithBackend(cfg, ownershipRunner(func(runCtx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
				args := req.Args
				if args[0] == "--org" {
					args = args[2:]
				}
				switch args[1] {
				case "status":
					state := "ready"
					if stopped.Load() {
						state = "completed"
					}
					return LocalCommandResult{Stdout: testBlacksmithStatus(id, state)}, nil
				case "run":
					close(entered)
					if hung {
						<-runCtx.Done()
					} else {
						select {
						case <-remoteStopped:
						case <-runCtx.Done():
						}
					}
					return LocalCommandResult{ExitCode: 7}, errors.New("remote workload cancelled")
				case "stop":
					stopped.Store(true)
					close(remoteStopped)
					return LocalCommandResult{}, nil
				default:
					return LocalCommandResult{}, fmt.Errorf("unexpected native operation: %v", args)
				}
			}))
			backend.rt.Stdout, backend.rt.Stderr = io.Discard, io.Discard
			runDone := make(chan error, 1)
			go func() {
				_, err := backend.Run(ctx, RunRequest{ID: id, Repo: Repo{Root: repo}, Command: []string{"long-running"}})
				runDone <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("run never entered")
			}
			// A second run must wait to touch the claim, but cannot block stop.
			admissionDone := make(chan error, 1)
			go func() { _, _, err := backend.ownedTestbox(ctx, id, repo, false); admissionDone <- err }()
			select {
			case err := <-admissionDone:
				t.Fatalf("admission crossed active command: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			stopCtx, cancelStop := context.WithTimeout(ctx, 250*time.Millisecond)
			stopErr := backend.Stop(stopCtx, StopRequest{ID: id})
			cancelStop()
			if !stopped.Load() {
				t.Fatal("stop could not reach active workload")
			}
			if hung {
				claim, err := readLeaseClaim(id)
				_, keyErr := os.Stat(key)
				if !errors.Is(stopErr, context.DeadlineExceeded) || err != nil || claim.LeaseID != id || keyErr != nil {
					t.Fatalf("hung command cleanup lost state: stop=%v claim=%+v err=%v key=%v", stopErr, claim, err, keyErr)
				}
			} else if stopErr != nil {
				t.Fatal(stopErr)
			}
			cancel()
			if err := <-runDone; err == nil {
				t.Fatal("cancelled command reported success")
			}
			if err := <-admissionDone; err == nil {
				t.Fatal("admitted run on terminated Testbox")
			}
		})
	}
}

func TestBlacksmithRejectsHydrationFailedReuse(t *testing.T) {
	isolateBlacksmithOwnership(t)
	const id = "tbx_hydration123"
	claim := testOwnedBlacksmithClaim(t, id, "jade-krill", "/repo")
	cfg := baseConfig()
	cfg.Blacksmith.Org = "example-org"
	backend := newTestBlacksmithBackend(cfg, ownershipRunner(func(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
		if req.Args[1] != "status" {
			t.Error("hydration failure reached mutation")
		}
		return LocalCommandResult{Stdout: testBlacksmithStatus(id, "hydration_failed")}, nil
	}))
	_, _, err := backend.ownedTestbox(t.Context(), id, "/repo", false)
	after, readErr := readLeaseClaim(id)
	if err == nil || !strings.Contains(err.Error(), "hydration failed") || readErr != nil || after.Revision != claim.Revision {
		t.Fatalf("hydration failure admitted or changed claim: err=%v read=%v claim=%+v", err, readErr, after)
	}
}
