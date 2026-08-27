package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type claimContractOperation struct {
	name     string
	endpoint bool
	run      func(string, leaseClaim, func() error) (leaseClaim, error)
}

func claimContractOperations() []claimContractOperation {
	server := Server{Provider: "aws", CloudID: "i-contract"}
	return []claimContractOperation{
		{"endpoint after", true, func(id string, expected leaseClaim, action func() error) (leaseClaim, error) {
			return updateLeaseClaimEndpointIfUnchangedAfter(id, expected, server, SSHTarget{}, action)
		}},
		{"endpoint action", true, func(id string, expected leaseClaim, action func() error) (leaseClaim, error) {
			updated, _, _, err := updateLeaseClaimEndpointIfUnchangedAction(id, expected, func() (Server, SSHTarget, bool, error) {
				return server, SSHTarget{}, true, action()
			})
			return updated, err
		}},
		{"labels after", false, func(id string, expected leaseClaim, action func() error) (leaseClaim, error) {
			return updateLeaseClaimLabelsIfUnchangedAfter(id, expected, map[string]string{"state": "ready"}, action)
		}},
		{"durable repo after", true, func(id string, expected leaseClaim, action func() error) (leaseClaim, error) {
			return claimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfter(id, "contract", Config{Provider: "aws"}, "", server, SSHTarget{}, "/repo", time.Minute, false, expected, true, action)
		}},
		{"durable replacement after", false, func(id string, expected leaseClaim, action func() error) (leaseClaim, error) {
			return replaceLeaseClaimIfUnchangedDurableAfter(id, expected, expected, action)
		}},
	}
}

func seedClaimContract(t *testing.T) leaseClaim {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const id = "cbx_transaction_contract"
	if err := claimLeaseForRepoProvider(id, "contract", "aws", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim(id)
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func assertClaimContractStored(t *testing.T, id string, want leaseClaim) {
	t.Helper()
	path, err := leaseClaimPath(id)
	if err != nil {
		t.Fatal(err)
	}
	// Actions already hold the claim lock; inspect the atomic file directly.
	got, _, err := readLeaseClaimPathWithPresence(path)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("stored=%#v want=%#v err=%v", got, want, err)
	}
}

func TestClaimTransactionContractGuardsBeforeActions(t *testing.T) {
	for _, op := range claimContractOperations() {
		for _, problem := range []string{"stale revision", "misfiled", "missing", "incomplete", "invalid ID", "invalid JSON"} {
			t.Run(op.name+"/"+problem, func(t *testing.T) {
				expected := seedClaimContract(t)
				id := expected.LeaseID
				path, err := leaseClaimPath(id)
				if err != nil {
					t.Fatal(err)
				}
				wantError := "claim changed"
				switch problem {
				case "stale revision":
					expected.Revision = "older"
				case "misfiled":
					expected.LeaseID = "cbx_other"
					if err := writeLeaseClaimAtomic(path, expected); err != nil {
						t.Fatal(err)
					}
					wantError = "refusing misfiled claim"
				case "missing":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "incomplete":
					if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
						t.Fatal(err)
					}
					if op.endpoint {
						wantError = "claim is incomplete"
					}
				case "invalid ID":
					id = "../escape"
					wantError = "invalid lease claim id"
				case "invalid JSON":
					if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
						t.Fatal(err)
					}
					wantError = "parse claim"
				}
				before, _ := os.ReadFile(path)
				calls := 0
				_, err = op.run(id, expected, func() error { calls++; return nil })
				if err == nil || !strings.Contains(err.Error(), wantError) || calls != 0 {
					t.Fatalf("calls=%d err=%v want=%s", calls, err, wantError)
				}
				after, _ := os.ReadFile(path)
				if string(before) != string(after) {
					t.Fatal("guard rejection changed stored bytes")
				}
			})
		}
	}
}

func TestClaimTransactionContractActionFailureAndRevision(t *testing.T) {
	for _, op := range claimContractOperations() {
		for _, actionErr := range []error{nil, errors.New("provider failure"), context.Canceled, context.DeadlineExceeded} {
			name := "success"
			if actionErr != nil {
				name = actionErr.Error()
			}
			t.Run(op.name+"/"+name, func(t *testing.T) {
				expected := seedClaimContract(t)
				calls := 0
				updated, err := op.run(expected.LeaseID, expected, func() error {
					calls++
					assertClaimContractStored(t, expected.LeaseID, expected)
					return actionErr
				})
				if !errors.Is(err, actionErr) || calls != 1 {
					t.Fatalf("calls=%d err=%v want=%v", calls, err, actionErr)
				}
				if actionErr != nil {
					assertClaimContractStored(t, expected.LeaseID, expected)
					return
				}
				if updated.Revision == "" || updated.Revision == expected.Revision {
					t.Fatalf("revision did not advance: %#v", updated)
				}
				assertClaimContractStored(t, expected.LeaseID, updated)
			})
		}
	}
}

// The provider hook observes different revision phases for input endpoints and
// action-produced endpoints. It must always run after the exact-claim guard.
type claimContractProvider struct {
	Provider
	prepare func(leaseClaim, bool) error
}

func (p claimContractProvider) PrepareLeaseClaimEndpoint(existing LeaseClaim, _, _ string, server Server, metadata bool) (Server, error) {
	return server, p.prepare(existing, metadata)
}

func TestClaimTransactionContractEndpointPreparationOrder(t *testing.T) {
	for _, mode := range []string{"update", "metadata", "replace metadata", "after", "action", "repo after"} {
		t.Run(mode, func(t *testing.T) {
			expected := seedClaimContract(t)
			original := providerRegistry["aws"]
			t.Cleanup(func() { providerRegistry["aws"] = original })
			var events []string
			var observed leaseClaim
			var admitted bool
			prepareErr := errors.New("endpoint policy rejected")
			providerRegistry["aws"] = claimContractProvider{Provider: original, prepare: func(claim leaseClaim, metadata bool) error {
				events = append(events, "prepare")
				observed = claim
				admitted = metadata
				return prepareErr
			}}
			action := func() error { events = append(events, "action"); return nil }
			server := Server{Provider: "aws"}
			var err error
			switch mode {
			case "update":
				_, err = updateLeaseClaimEndpointIfUnchanged(expected.LeaseID, expected, server, SSHTarget{})
			case "metadata":
				_, err = updateLeaseClaimEndpointIfUnchangedWithProviderMetadata(expected.LeaseID, expected, server, SSHTarget{})
			case "replace metadata":
				_, err = replaceLeaseClaimEndpointIfUnchangedWithProviderMetadata(expected.LeaseID, expected, server, SSHTarget{})
			case "after":
				_, err = updateLeaseClaimEndpointIfUnchangedAfter(expected.LeaseID, expected, server, SSHTarget{}, action)
			case "action":
				_, _, _, err = updateLeaseClaimEndpointIfUnchangedAction(expected.LeaseID, expected, func() (Server, SSHTarget, bool, error) { return server, SSHTarget{}, true, action() })
			case "repo after":
				_, err = claimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfter(expected.LeaseID, "contract", Config{Provider: "aws"}, "", server, SSHTarget{}, "/another-repo", time.Minute, false, expected, true, action)
			}
			if !errors.Is(err, prepareErr) {
				t.Fatalf("err=%v", err)
			}
			wantEvents := []string{"prepare"}
			if mode == "after" || mode == "action" || mode == "repo after" {
				wantEvents = []string{"action", "prepare"}
			}
			if !reflect.DeepEqual(events, wantEvents) {
				t.Fatalf("events=%v want=%v", events, wantEvents)
			}
			if (observed.Revision == expected.Revision) != (mode == "action") {
				t.Fatalf("unexpected hook revision: mode=%s observed=%q expected=%q", mode, observed.Revision, expected.Revision)
			}
			if admitted != (mode == "metadata" || mode == "replace metadata") {
				t.Fatalf("metadata admission=%v mode=%s", admitted, mode)
			}
			assertClaimContractStored(t, expected.LeaseID, expected)
		})
	}
}

func TestClaimTransactionContractWriteFailures(t *testing.T) {
	for _, afterRename := range []bool{false, true} {
		t.Run(map[bool]string{false: "before rename", true: "directory sync"}[afterRename], func(t *testing.T) {
			expected := seedClaimContract(t)
			replacement := cloneLeaseClaim(expected)
			replacement.Labels = map[string]string{"state": "ready"}
			failure := errors.New("storage failure")
			calls := 0
			updated, err := replaceLeaseClaimIfUnchangedWithWrite(expected.LeaseID, expected, replacement, func(path string, claim leaseClaim) error {
				calls++
				if afterRename {
					return writeLeaseClaimAtomicDurableWithSync(path, claim, filepath.Dir(path), func(string) error { return failure })
				}
				return failure
			})
			if err == nil || !strings.Contains(err.Error(), failure.Error()) || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if afterRename {
				assertClaimContractStored(t, expected.LeaseID, updated)
			} else {
				assertClaimContractStored(t, expected.LeaseID, expected)
			}
		})
	}
}

func TestClaimTransactionContractConcurrentActions(t *testing.T) {
	for _, op := range claimContractOperations() {
		t.Run(op.name, func(t *testing.T) {
			expected := seedClaimContract(t)
			entered := make(chan struct{})
			release := make(chan struct{})
			// Release the first action even if a fence assertion fails.
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			done := make(chan error, 1)
			go func() {
				_, err := op.run(expected.LeaseID, expected, func() error { close(entered); <-release; return nil })
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("action did not enter")
			}
			competing := make(chan error, 1)
			calls := make(chan struct{}, 1)
			go func() {
				_, err := op.run(expected.LeaseID, expected, func() error { calls <- struct{}{}; return nil })
				competing <- err
			}()
			select {
			case err := <-competing:
				t.Fatalf("competing transaction bypassed lock: %v", err)
			case <-calls:
				t.Fatal("competing action bypassed lock")
			case <-time.After(50 * time.Millisecond):
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if err := <-competing; err == nil || !strings.Contains(err.Error(), "claim changed") {
				t.Fatalf("competing err=%v", err)
			}
			select {
			case <-calls:
				t.Fatal("stale action ran")
			default:
			}
		})
	}
}

func TestClaimTransactionContractNoopActions(t *testing.T) {
	for _, replace := range []bool{false, true} {
		for _, outcome := range []string{"nil", "no update", "canceled", "same endpoint"} {
			t.Run(map[bool]string{false: "update", true: "replace"}[replace]+"/"+outcome, func(t *testing.T) {
				expected := seedClaimContract(t)
				run := updateLeaseClaimEndpointIfUnchangedAction
				if replace {
					run = replaceLeaseClaimEndpointIfUnchangedAction
				}
				calls := 0
				var action func() (Server, SSHTarget, bool, error)
				if outcome != "nil" {
					action = func() (Server, SSHTarget, bool, error) {
						calls++
						var err error
						if outcome == "canceled" {
							err = context.Canceled
						}
						return Server{Provider: "aws"}, SSHTarget{}, outcome == "same endpoint", err
					}
				}
				updated, _, _, err := run(expected.LeaseID, expected, action)
				if (outcome == "canceled") != errors.Is(err, context.Canceled) || (err != nil && outcome != "canceled") {
					t.Fatalf("err=%v", err)
				}
				wantCalls := 1
				if outcome == "nil" {
					wantCalls = 0
				}
				if calls != wantCalls {
					t.Fatalf("calls=%d", calls)
				}
				if outcome == "same endpoint" {
					if updated.Revision == expected.Revision {
						t.Fatal("explicit publication must advance revision even for identical values")
					}
				} else if !reflect.DeepEqual(updated, expected) {
					t.Fatalf("no-op snapshot=%#v want=%#v", updated, expected)
				}
				assertClaimContractStored(t, expected.LeaseID, updated)
			})
		}
	}
}

func TestClaimTransactionContractEndpointReplacement(t *testing.T) {
	for _, mode := range []string{"metadata update", "metadata replace", "action replace", "repo replace"} {
		for _, target := range []SSHTarget{{}, {Host: "new.example.test", Port: "invalid"}, {Host: "new.example.test", Port: " 2222 "}} {
			t.Run(mode+"/"+target.Port, func(t *testing.T) {
				expected := seedClaimContract(t)
				expected.SSHHost, expected.SSHPort = "old.example.test", 22
				expected.TailscaleIPv4, expected.TailscaleFQDN = "100.64.0.1", "old.ts.test"
				expected.BridgeURL = "https://old.example.test"
				expected.TailscaleHostname = "configured-hostname"
				expected.Labels = map[string]string{"tailscale": "true", "tailscale_state": "ready", "tailscale_ipv4": expected.TailscaleIPv4, "tailscale_fqdn": expected.TailscaleFQDN}
				path, err := leaseClaimPath(expected.LeaseID)
				if err != nil {
					t.Fatal(err)
				}
				if err := writeLeaseClaimAtomic(path, expected); err != nil {
					t.Fatal(err)
				}
				server := Server{Provider: "aws"}
				var updated leaseClaim
				switch mode {
				case "metadata update":
					updated, err = updateLeaseClaimEndpointIfUnchangedWithProviderMetadata(expected.LeaseID, expected, server, target)
				case "metadata replace":
					updated, err = replaceLeaseClaimEndpointIfUnchangedWithProviderMetadata(expected.LeaseID, expected, server, target)
				case "action replace":
					updated, _, _, err = replaceLeaseClaimEndpointIfUnchangedAction(expected.LeaseID, expected, func() (Server, SSHTarget, bool, error) { return server, target, true, nil })
				case "repo replace":
					updated, err = claimLeaseTargetForRepoConfigScopeReplacingEndpointIfUnchanged(expected.LeaseID, expected.Slug, Config{Provider: "aws"}, "", server, target, "/repo", time.Minute, false, expected, true)
				}
				if err != nil {
					t.Fatal(err)
				}
				if mode == "metadata update" {
					if updated.TailscaleIPv4 != expected.TailscaleIPv4 || updated.BridgeURL != expected.BridgeURL {
						t.Fatal("ordinary update cleared connection state")
					}
					if target.Host == "" && updated.SSHHost != expected.SSHHost {
						t.Fatal("ordinary update cleared host")
					}
					if target.Port != " 2222 " && updated.SSHPort != expected.SSHPort {
						t.Fatal("ordinary update cleared port")
					}
				} else {
					if updated.TailscaleIPv4 != "" || updated.TailscaleFQDN != "" || updated.BridgeURL != "" {
						t.Fatalf("stale connection metadata: %#v", updated)
					}
					for _, key := range []string{"tailscale", "tailscale_state", "tailscale_ipv4", "tailscale_fqdn"} {
						if _, exists := updated.Labels[key]; exists {
							t.Fatalf("stale label %s", key)
						}
					}
					port := 0
					if target.Port == " 2222 " {
						port = 2222
					}
					if updated.SSHHost != target.Host || updated.SSHPort != port {
						t.Fatalf("replacement endpoint=%s:%d", updated.SSHHost, updated.SSHPort)
					}
				}
				if updated.TailscaleHostname != expected.TailscaleHostname {
					t.Fatal("replacement erased configured Tailscale settings")
				}
				assertClaimContractStored(t, expected.LeaseID, updated)
			})
		}
	}
}

func TestClaimTransactionContractMissingClaimDoesNotPrepareEndpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	original := providerRegistry["aws"]
	t.Cleanup(func() { providerRegistry["aws"] = original })
	providerRegistry["aws"] = claimContractProvider{Provider: original, prepare: func(leaseClaim, bool) error { t.Fatal("new claim passed to existing endpoint policy"); return nil }}
	calls := 0
	updated, err := claimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfter("cbx_new_contract", "new", Config{Provider: "aws"}, "account:one", Server{Provider: "aws"}, SSHTarget{}, "/repo", time.Minute, false, leaseClaim{}, false, func() error { calls++; return nil })
	if err != nil || calls != 1 || updated.Revision == "" {
		t.Fatalf("calls=%d updated=%#v err=%v", calls, updated, err)
	}
	assertClaimContractStored(t, updated.LeaseID, updated)
}

func TestClaimTransactionContractAtomicWriteDoesNotFollowSymlink(t *testing.T) {
	expected := seedClaimContract(t)
	path, err := leaseClaimPath(expected.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.json")
	if err := writeLeaseClaimAtomic(target, expected); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	updated, err := updateLeaseClaimLabelsIfUnchangedAfter(expected.LeaseID, expected, map[string]string{"state": "ready"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(target)
	if err != nil || string(before) != string(after) {
		t.Fatalf("symlink target modified: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("atomic claim not installed: %v", err)
	}
	assertClaimContractStored(t, expected.LeaseID, updated)
}

func TestClaimTransactionActionWriteFailureIsNotRetried(t *testing.T) {
	for _, afterRename := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "sync"}[afterRename], func(t *testing.T) {
			expected := seedClaimContract(t)
			calls, writes := 0, 0
			failure := errors.New("write failure")
			updated, err := transactLeaseClaim(expected.LeaseID, leaseClaimTransaction{
				guard:    unchangedLeaseClaimGuard(expected.LeaseID, expected, true),
				action:   claimTransactionAction(func() error { calls++; return nil }),
				revision: claimRevisionBeforeMutation,
				mutate:   func(claim *leaseClaim) error { claim.Labels = map[string]string{"state": "ready"}; return nil },
				write: func(path string, claim leaseClaim) error {
					writes++
					if afterRename {
						return writeLeaseClaimAtomicDurableWithSync(path, claim, filepath.Dir(path), func(string) error { return failure })
					}
					return failure
				},
			})
			if err == nil || !strings.Contains(err.Error(), failure.Error()) || calls != 1 || writes != 1 {
				t.Fatalf("calls=%d writes=%d err=%v", calls, writes, err)
			}
			if afterRename {
				assertClaimContractStored(t, expected.LeaseID, updated)
			} else {
				assertClaimContractStored(t, expected.LeaseID, expected)
			}
		})
	}
}

func TestClaimTransactionContractIncompleteLabels(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "after"}[after], func(t *testing.T) {
			seed := seedClaimContract(t)
			path, err := leaseClaimPath(seed.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			// Label-only helpers historically permit exact incomplete snapshots; endpoint
			// helpers reject them. Ordinary mutations skip empty IDs, while After writes.
			labels := map[string]string{"state": "failed"}
			var updated leaseClaim
			if after {
				updated, err = updateLeaseClaimLabelsIfUnchangedAfter(seed.LeaseID, leaseClaim{}, labels, nil)
			} else {
				updated, err = updateLeaseClaimLabelsIfUnchanged(seed.LeaseID, leaseClaim{}, labels)
			}
			if err != nil {
				t.Fatal(err)
			}
			if after && (updated.Revision == "" || updated.Labels["state"] != "failed") {
				t.Fatalf("updated=%#v", updated)
			}
			if !after && !reflect.DeepEqual(updated, leaseClaim{}) {
				t.Fatalf("empty ordinary mutation published: %#v", updated)
			}
			assertClaimContractStored(t, seed.LeaseID, updated)
		})
	}
}

func TestClaimTransactionContractEmptyIDAndRepo(t *testing.T) {
	for _, op := range claimContractOperations() {
		t.Run(op.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			calls := 0
			_, err := op.run("", leaseClaim{}, func() error { calls++; return nil })
			wantErr := op.name == "endpoint action" || op.name == "durable replacement after"
			if (err != nil) != wantErr || calls != 0 {
				t.Fatalf("calls=%d err=%v wantErr=%v", calls, err, wantErr)
			}
		})
	}
	t.Run("empty repo does not run action", func(t *testing.T) {
		expected := seedClaimContract(t)
		calls := 0
		updated, err := claimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfter(expected.LeaseID, expected.Slug, Config{Provider: "aws"}, "", Server{}, SSHTarget{}, "", time.Minute, false, expected, true, func() error { calls++; return nil })
		if err != nil || calls != 0 || !reflect.DeepEqual(updated, leaseClaim{}) {
			t.Fatalf("calls=%d updated=%#v err=%v", calls, updated, err)
		}
		assertClaimContractStored(t, expected.LeaseID, expected)
	})
}

func TestClaimTransactionContractNamespaceFailureBeforeAction(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "durable"}[durable], func(t *testing.T) {
			stateRoot := t.TempDir()
			t.Setenv("XDG_STATE_HOME", stateRoot)
			if err := os.WriteFile(filepath.Join(stateRoot, "crabbox"), []byte("blocked"), 0o600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			directory := claimDirectoryCreate
			if durable {
				directory = claimDirectoryDurableNamespace
			}
			_, err := transactLeaseClaim("cbx_blocked", leaseClaimTransaction{
				directory: directory,
				guard:     unchangedLeaseClaimGuard("cbx_blocked", leaseClaim{}, false),
				action:    claimTransactionAction(func() error { calls++; return nil }),
				mutate:    func(claim *leaseClaim) error { claim.LeaseID = "cbx_blocked"; return nil },
			})
			if err == nil || calls != 0 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}
