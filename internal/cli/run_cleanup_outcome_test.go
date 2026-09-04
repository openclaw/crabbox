package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunCoordinatorCleanupOutcomes(t *testing.T) {
	configureCoordinatorReleaseTestTiming(t, time.Second, 0)
	deleting, retained := true, false
	for _, tc := range []struct {
		name                                  string
		lease                                 CoordinatorLease
		terminal, releaseError, artifactError bool
	}{
		{name: "deleted", lease: CoordinatorLease{State: "released", ReleaseDeletesServer: &deleting}, terminal: true},
		{name: "retained", lease: CoordinatorLease{State: "released", ReleaseDeletesServer: &retained}},
		{name: "pending", lease: CoordinatorLease{State: "released", CleanupStartedAt: "2026-08-31T00:00:00Z", ReleaseDeletesServer: &deleting}},
		{name: "failed", lease: CoordinatorLease{State: "released", CleanupError: "synthetic provider failure", ReleaseDeletesServer: &deleting}},
		{name: "retry scheduled", lease: CoordinatorLease{State: "released", CleanupRetryAt: "2026-08-31T00:05:00Z", ReleaseDeletesServer: &deleting}},
		{name: "unknown", lease: CoordinatorLease{State: "released", CleanupStatus: "unknown"}},
		{name: "release rejected", releaseError: true},
		{name: "deleted with artifact error", lease: CoordinatorLease{State: "released"}, terminal: true, artifactError: true},
	} {
		for _, timing := range []bool{false, true} {
			name := tc.name + "/text"
			if timing {
				name = tc.name + "/timing"
			}
			t.Run(name, func(t *testing.T) {
				setupRunCleanupWorkspaceOwnerTest(t)
				const id = "cbx_abcdef123456"
				provider := runReadyPoolPreflightTestProvider{}.Name()
				t.Setenv("CRABBOX_OWNER", "alice@example.com")
				t.Setenv("CRABBOX_SSH_CONFIG_PROXY", "true")
				key, err := testboxKeyPath(id)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.MkdirAll(filepath.Dir(key), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(key, []byte("synthetic SSH artifact"), 0600); err != nil {
					t.Fatal(err)
				}
				var posts atomic.Int32
				var artifactFailure atomic.Bool
				outside := t.TempDir()
				active := CoordinatorLease{ID: id, Provider: provider, State: "active", Host: "127.0.0.1", SSHPort: "22", SSHUser: "crabbox", TargetOS: targetLinux}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/release"):
						posts.Add(1)
						if tc.releaseError {
							http.Error(w, "synthetic release rejection", http.StatusConflict)
							return
						}
						if tc.artifactError {
							if err := os.Rename(filepath.Dir(key), filepath.Join(outside, "saved")); err != nil {
								t.Error(err)
							}
							if err := os.Symlink(outside, filepath.Dir(key)); err != nil {
								t.Error(err)
							} else {
								artifactFailure.Store(true)
							}
						}
						lease := tc.lease
						lease.ID = id
						lease.Provider = provider
						_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
					case strings.HasPrefix(r.URL.Path, "/v1/leases/"):
						_ = json.NewEncoder(w).Encode(map[string]any{"lease": active})
					case r.URL.Path == "/v1/runs" || strings.HasSuffix(r.URL.Path, "/finish"):
						_ = json.NewEncoder(w).Encode(map[string]any{"run": map[string]any{"id": "run_abcdef123456"}})
					case strings.HasSuffix(r.URL.Path, "/events"):
						_, _ = io.WriteString(w, `{"event":{"seq":1}}`)
					default:
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				t.Setenv("CRABBOX_COORDINATOR", server.URL)
				t.Setenv("CRABBOX_COORDINATOR_TOKEN", "synthetic-token")
				var stdout, stderr bytes.Buffer
				args := []string{"--provider", provider, "--id", id, "--stop-after", "always", "--no-sync", "--no-hydrate"}
				if timing {
					args = append(args, "--timing-json")
				}
				args = append(args, "--", "renewal-cleanup-exit-23")
				err = (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), args)
				assertRunCleanupExitCode(t, err, 23, stdout.String(), stderr.String())
				out := stderr.String()
				if timing {
					lines := strings.Split(strings.TrimSpace(out), "\n")
					var report TimingReport
					if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
						t.Fatalf("final timing: %v\n%s", err, out)
					}
					if report.LeaseStopped == nil || *report.LeaseStopped != tc.terminal || report.ExitCode != 23 || report.RunStatus != "failed" || (report.LeaseStopErr != "") != (tc.releaseError || tc.artifactError) {
						t.Errorf("wrong timing: %+v\n%s", report, out)
					}
				} else if !strings.Contains(out, "lease cleanup stopped="+map[bool]string{true: "true", false: "false"}[tc.terminal]) {
					t.Errorf("missing cleanup outcome:\n%s", out)
				}
				digest := strings.SplitN(out, "failure digest\n", 2)
				if len(digest) != 2 {
					t.Fatalf("missing digest:\n%s", out)
				}
				for _, command := range []string{"ssh", "run", "stop"} {
					if got := strings.Contains(digest[1], "next: crabbox "+command+" "); got == tc.terminal {
						t.Errorf("recovery %s present=%t terminal=%t\n%s", command, got, tc.terminal, out)
					}
				}
				wantPosts := int32(1)
				if tc.releaseError {
					wantPosts = 5 // Existing pre-acceptance retry policy.
				}
				if got := posts.Load(); got != wantPosts {
					t.Errorf("release POSTs=%d want %d", got, wantPosts)
				}
				_, exists, claimErr := readLeaseClaimWithPresence(id)
				wantClaim := !tc.terminal || tc.artifactError
				if claimErr != nil || exists != wantClaim {
					t.Errorf("claim exists=%t err=%v want=%t", exists, claimErr, wantClaim)
				}
				if tc.artifactError {
					if !artifactFailure.Load() || !strings.Contains(out, "local SSH artifact cleanup failed") {
						t.Errorf("artifact failure not exercised:\n%s", out)
					}
					if err := os.Remove(filepath.Dir(key)); err != nil {
						t.Fatal(err)
					}
				} else {
					data, readErr := os.ReadFile(key)
					if wantClaim && (readErr != nil || string(data) != "synthetic SSH artifact") {
						t.Errorf("recovery artifact changed: %q %v", data, readErr)
					}
					if !wantClaim && !errors.Is(readErr, os.ErrNotExist) {
						t.Errorf("deleted artifact still present: %v", readErr)
					}
				}
			})
		}
	}
}

func TestCoordinatorReleaseOutcomeRejectsSuccessorClaim(t *testing.T) {
	clearConfigEnv(t)
	const id = "cbx_abcdef123456"
	key, claimPath := managedStopLocalState(t, id)
	successor, err := readLeaseClaim(id)
	if err != nil {
		t.Fatal(err)
	}
	successor.Revision = "successor-revision"
	successor.RepoRoot = "/successor/repo"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected provider request", 500)
	}))
	defer server.Close()
	backend := coordinatorReleaseTestBackend(server, io.Discard)
	type result struct {
		outcome ReleaseLeaseOutcome
		err     error
	}
	done := make(chan result, 1)
	err = withLeaseClaimLockContext(t.Context(), claimPath, true, func() error {
		go func() {
			outcome, err := backend.ReleaseLeaseWithOutcome(t.Context(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: id, Server: Server{Provider: "aws"}}, DeferProviderCleanupObservation: true})
			done <- result{outcome, err}
		}()
		waitClaimWriter(t, claimPath)
		// Publish the successor while release is waiting on the file fence, after
		// its snapshot read. The under-fence guard must reject that older snapshot.
		return writeLeaseClaimAtomic(claimPath, successor)
	})
	if err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err == nil || got.outcome.Terminal || requests.Load() != 0 {
		t.Fatalf("outcome=%+v err=%v requests=%d", got.outcome, got.err, requests.Load())
	}
	current, err := readLeaseClaim(id)
	if err != nil || current.Revision != successor.Revision || current.RepoRoot != successor.RepoRoot {
		t.Fatalf("successor changed: %+v %v", current, err)
	}
	if data, err := os.ReadFile(key); err != nil || string(data) != "private" {
		t.Fatalf("successor SSH artifact changed: %q %v", data, err)
	}
}

func TestRunSuccessfulRetainedCleanup(t *testing.T) {
	for _, policy := range []string{"", "success", "always"} {
		t.Run("policy="+policy, func(t *testing.T) {
			setupRunCleanupWorkspaceOwnerTest(t)
			runEnvProfileTestRetainsLease = true
			calls := 0
			runEnvProfileTestReleaseHook = func() error { calls++; return nil }
			var stdout, stderr bytes.Buffer
			args := []string{"--provider", runEnvProfileTestProvider{}.Name(), "--no-sync", "--no-hydrate", "--timing-json"}
			if policy != "" {
				args = append(args, "--stop-after", policy)
			}
			args = append(args, "--", "renewal-cleanup-success")
			err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(t.Context(), args)
			if err != nil {
				t.Fatalf("retention became an execution error: %v\n%s", err, stderr.String())
			}
			lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
			var report TimingReport
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
				t.Fatal(err)
			}
			if report.LeaseStopped == nil || *report.LeaseStopped || report.LeaseStopErr != "" || report.ExitCode != 0 || report.RunStatus != "succeeded" || calls != 1 {
				t.Fatalf("report=%+v calls=%d", report, calls)
			}
		})
	}
}

type runReleaseOutcomeBackend struct {
	*warmupFailureReleaseBackend
	outcome ReleaseLeaseOutcome
	err     error
}

func (b runReleaseOutcomeBackend) ReleaseLeaseWithOutcome(context.Context, ReleaseLeaseRequest) (ReleaseLeaseOutcome, error) {
	b.releases++
	return b.outcome, b.err
}

func TestReleaseBackendLeaseOutcomeContract(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		capability, terminal bool
		err                  error
	}{
		{name: "default terminal", terminal: true},
		{name: "retained", capability: true},
		{name: "terminal with local error", capability: true, terminal: true, err: errors.New("local finalization failed")},
		{name: "release error", capability: true, err: errors.New("provider release failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateTestUserDirs(t)
			base := &warmupFailureReleaseBackend{}
			var backend SSHLeaseBackend = base
			if tc.capability {
				backend = runReleaseOutcomeBackend{base, ReleaseLeaseOutcome{Terminal: tc.terminal}, tc.err}
			}
			outcome, err := (App{Stdout: io.Discard, Stderr: io.Discard}).releaseBackendLeaseWithOutcomeBestEffort(t.Context(), backend, Config{}, LeaseTarget{LeaseID: "cbx_abcdef123456"})
			if outcome.Terminal != tc.terminal || !errors.Is(err, tc.err) || base.releases != 1 || base.resolves != 0 {
				t.Fatalf("outcome=%+v err=%v releases=%d resolves=%d", outcome, err, base.releases, base.resolves)
			}
		})
	}
}
