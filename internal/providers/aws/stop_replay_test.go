package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

// The fake models the inventory response after EC2 excludes terminated instances.
// App.Run still owns the real canonical Resolve -> ReleaseLease command boundary.
type stopReplayAWSClient struct {
	*fakeAWSClient
	lists   int
	listErr error
}

func (c *stopReplayAWSClient) ListCrabboxServers(ctx context.Context) ([]Server, error) {
	c.lists++
	if c.listErr != nil {
		return nil, c.listErr
	}
	return c.fakeAWSClient.ListCrabboxServers(ctx)
}

func TestAWSFixedStopReplayAfterInventoryDisappears(t *testing.T) {
	for _, tc := range []struct {
		name             string
		active           bool
		keyFailure       bool
		account          string
		accountError     bool
		region           string
		fallback         bool
		differentCWD     bool
		expectedLease    string
		expectedSlug     string
		expectedCloud    string
		expectedAttempt  string
		expectedExact    bool
		lookup           string
		inspect          bool
		conflict         func(Server) Server
		inventoryError   bool
		malformedVersion bool
		mutate           func(*core.LeaseClaim)
		wantSuccess      bool
	}{
		{name: "released_tombstone", wantSuccess: true},
		{name: "different_stop_directory", differentCWD: true, wantSuccess: true},
		{name: "exact_expected_identity", expectedExact: true, wantSuccess: true},
		{name: "configured_fallback_region", region: "us-west-2", fallback: true, wantSuccess: true},
		{name: "active_claim_remaining_key", active: true},
		{name: "failed_key_cleanup", keyFailure: true},
		{name: "wrong_account", account: "999999999999"},
		{name: "account_read_error", accountError: true},
		{name: "wrong_region", region: "us-west-2"},
		{name: "wrong_caller_lease", expectedLease: "cbx_000000000001"},
		{name: "wrong_caller_attempt", expectedAttempt: "cbx_000000000001"},
		{name: "wrong_expected_slug", expectedSlug: "other-slug"},
		{name: "wrong_expected_resource", expectedCloud: "i-other"},
		{name: "inspect", inspect: true},
		{name: "slug", lookup: "stop-replay"},
		{name: "raw_instance", lookup: "i-fixed"},
		{name: "inventory_error", inventoryError: true},
		{name: "visible_original", conflict: func(s Server) Server { return s }},
		{name: "visible_same_lease_wrong_owner", conflict: func(s Server) Server {
			s.CloudID = "i-conflict"
			s.Labels["provider"] = "other"
			return s
		}},
		{name: "visible_original_wrong_lease", conflict: func(s Server) Server {
			s.Labels["lease"] = "cbx_000000000001"
			return s
		}},
		{name: "visible_terminal_word", conflict: func(s Server) Server { s.Status = "terminated"; return s }},
		{name: "wrong_provider_discriminator", mutate: func(c *core.LeaseClaim) { c.Provider = "aws" }},
		{name: "non_fixed", mutate: func(c *core.LeaseClaim) { c.Provider = "aws"; c.FixedCreateIntent = nil }},
		{name: "wrong_stored_lease", mutate: func(c *core.LeaseClaim) { c.LeaseID = "cbx_000000000002" }},
		{name: "missing_intent", mutate: func(c *core.LeaseClaim) { c.FixedCreateIntent = nil }},
		{name: "unsupported_version", mutate: func(c *core.LeaseClaim) { c.FixedCreateIntent.Version++ }},
		{name: "malformed_version", malformedVersion: true},
		{name: "missing_fingerprint", mutate: func(c *core.LeaseClaim) { c.FixedCreateIntent.Fingerprint = "" }},
		{name: "invalid_fingerprint", mutate: func(c *core.LeaseClaim) {
			c.FixedCreateIntent.Fingerprint = "invalid"
			c.Labels["fixed_intent_sha256"] = "invalid"
		}},
		{name: "changed_valid_fingerprint", mutate: func(c *core.LeaseClaim) { c.FixedCreateIntent.Fingerprint = strings.Repeat("b", 64) }},
		{name: "missing_original_attestation", mutate: func(c *core.LeaseClaim) { delete(c.Labels, "fixed_intent_sha256") }},
		{name: "changed_original_attestation", mutate: func(c *core.LeaseClaim) { c.Labels["fixed_intent_sha256"] = strings.Repeat("b", 64) }},
		{name: "inconsistent_account_scope", mutate: func(c *core.LeaseClaim) { c.ProviderScope = "account:999999999999" }},
		{name: "inconsistent_intent_scope", mutate: func(c *core.LeaseClaim) { c.FixedCreateIntent.ProviderScope = "account:999999999999" }},
		{name: "inconsistent_account_label", mutate: func(c *core.LeaseClaim) { c.Labels["aws_account_id"] = "999999999999" }},
		{name: "missing_region", mutate: func(c *core.LeaseClaim) { delete(c.Labels, "aws_region") }},
		{name: "missing_resource", mutate: func(c *core.LeaseClaim) { c.CloudID = "" }},
		{name: "label_provider_mismatch", mutate: func(c *core.LeaseClaim) { c.Labels["provider"] = "other" }},
		{name: "label_lease_mismatch", mutate: func(c *core.LeaseClaim) { c.Labels["lease"] = "cbx_000000000001" }},
		{name: "label_slug_mismatch", mutate: func(c *core.LeaseClaim) { c.Labels["slug"] = "other-slug" }},
		{name: "inconsistent_slug", mutate: func(c *core.LeaseClaim) { c.Slug = "other-owner" }},
		{name: "incomplete_intent", mutate: func(c *core.LeaseClaim) { c.FixedCreateIntent.State = "prepared" }},
		{name: "failed_intent", mutate: func(c *core.LeaseClaim) { c.FixedCreateIntent.State = "failed" }},
		{name: "active_attempt", mutate: func(c *core.LeaseClaim) { c.FixedCreateIntent.Attempt = map[string]string{"aws": "active"} }},
		{name: "failed_attempt", mutate: func(c *core.LeaseClaim) { c.FixedCreateIntent.FailedAttempts = []string{"failed"} }},
		{name: "ssh_host", mutate: func(c *core.LeaseClaim) { c.SSHHost = "127.0.0.1" }},
		{name: "ssh_port", mutate: func(c *core.LeaseClaim) { c.SSHPort = 22 }},
		{name: "active_label", mutate: func(c *core.LeaseClaim) { c.Labels["state"] = "ready" }},
		{name: "old_compact_tombstone", mutate: func(c *core.LeaseClaim) {
			compact := fixedAWSLeaseKind
			compact.TerminalIdentityLabels = nil
			*c = compact.TerminalClaim(*c, time.Now().UTC())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dirs := testutil.IsolateUserDirs(t)
			repo := t.TempDir()
			t.Chdir(repo)
			for _, name := range []string{"CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_MODE", "CRABBOX_COORDINATOR_TOKEN_COMMAND", "CRABBOX_POND", "CRABBOX_TAILSCALE"} {
				t.Setenv(name, "")
			}
			configPath := filepath.Join(repo, "config.yaml")
			if err := os.WriteFile(configPath, []byte("provider: aws\ntarget: linux\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", configPath)
			t.Setenv("CRABBOX_AWS_REGION", "us-east-1")

			fake := &fakeAWSClient{}
			t.Cleanup(installFixedAWSTestClient(t, fake))
			client := &stopReplayAWSClient{fakeAWSClient: fake}
			newAWSClient = func(context.Context, Config) (awsClient, error) { return client, nil }
			cfg := fixedAWSTestConfig()
			// Prevent public-IP discovery during the real fixed acquisition setup.
			cfg.AWSSSHCIDRs = []string{"127.0.0.1/32"}
			req := AcquireRequest{Repo: core.Repo{Root: repo}, Keep: true, RequestedLeaseID: "cbx_abcdef123490", RequestedSlug: "stop-replay"}
			backend := NewAWSLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
			lease, err := backend.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			live, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || live.FixedCreateIntent == nil || live.FixedCreateIntent.State != "acquired" {
				t.Fatalf("expected persisted acquired claim: %v", err)
			}
			// There is no SSH target in this fixture; stop must only use the fake API.
			fake.servers[0].PublicNet.IPv4.IP = ""
			keyErr := errors.New("synthetic provider SSH key deletion failure")
			if tc.keyFailure {
				fake.deleteKeyErr = keyErr
			}
			if !tc.active {
				var output bytes.Buffer
				err = (core.App{Stdout: &output, Stderr: &output}).Run(t.Context(), []string{"stop", "--provider", "aws", "--id", lease.LeaseID})
				if tc.keyFailure {
					if !errors.Is(err, keyErr) {
						t.Fatalf("first stop error=%v, want provider key failure", err)
					}
				} else if err != nil {
					t.Fatalf("first canonical stop failed: %v; output=%q", err, output.String())
				}
				if !tc.keyFailure {
					key, err := core.TestboxKeyPath(lease.LeaseID)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := os.Stat(filepath.Dir(key)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("successful stop retained local SSH artifacts: %v", err)
					}
				}
				if !reflect.DeepEqual(fake.deletedInstances, []string{lease.Server.CloudID}) ||
					!reflect.DeepEqual(fake.deletedKeys, []string{core.ServerProviderKey(lease.Server)}) {
					t.Fatalf("first stop did not attempt exact instance/key cleanup: instances=%v keys=%v", fake.deletedInstances, fake.deletedKeys)
				}
			}
			claim, err := core.ReadLeaseClaim(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.active || tc.keyFailure {
				if !reflect.DeepEqual(claim, live) {
					t.Fatal("incomplete cleanup changed the durable acquired claim")
				}
			} else if err := fixedAWSLeaseKind.ValidateTerminalClaim(claim, live, lease.LeaseID, nil); err != nil {
				t.Fatalf("ordinary stop did not persist a valid exact tombstone: %v", err)
			} else {
				assertAWSReceiptIdentity(t, claim, live)
			}
			claimPath := filepath.Join(dirs.StateHome, "crabbox", "claims", lease.LeaseID+".json")
			if tc.mutate != nil || tc.malformedVersion {
				if tc.mutate != nil {
					tc.mutate(&claim)
				}
				data, err := json.Marshal(claim)
				if err != nil {
					t.Fatal(err)
				}
				if tc.malformedVersion {
					data = bytes.Replace(data, []byte(`"version":2`), []byte(`"version":"invalid"`), 1)
				}
				if err := os.WriteFile(claimPath, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(claimPath)
			if err != nil {
				t.Fatal(err)
			}
			// Model local files left by an older stop or interrupted cleanup.
			key, err := core.TestboxKeyPath(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
				t.Fatal(err)
			}
			trust := filepath.Join(filepath.Dir(key), "known_hosts")
			if err := os.WriteFile(trust, []byte("synthetic lease trust"), 0o600); err != nil {
				t.Fatal(err)
			}
			// Discard all prior client/backend state. Only the persisted claim and
			// unchanged account/region survive, as after a consumer restart.
			reloaded := &stopReplayAWSClient{fakeAWSClient: &fakeAWSClient{accountID: tc.account, deleteKeyErr: fake.deleteKeyErr}}
			if tc.accountError {
				reloaded.accountErr = errors.New("synthetic account lookup failure")
			}
			if tc.conflict != nil {
				server := lease.Server
				server.Labels = maps.Clone(server.Labels)
				reloaded.servers = []Server{tc.conflict(server)}
			}
			if tc.active {
				// A future owner reconciliation may try the remaining key, but it
				// cannot acknowledge completion while that deletion still fails.
				reloaded.deleteKeyErr = keyErr
			}
			if tc.inventoryError {
				reloaded.listErr = errors.New("synthetic inventory unavailable")
			}
			newAWSClient = func(context.Context, Config) (awsClient, error) { return reloaded, nil }
			if tc.region != "" {
				t.Setenv("CRABBOX_AWS_REGION", tc.region)
			}
			if tc.fallback {
				t.Setenv("CRABBOX_CAPACITY_REGIONS", "us-east-1")
			}
			if tc.differentCWD {
				t.Chdir(t.TempDir())
			}
			args := []string{"stop", "--provider", "aws", "--id", lease.LeaseID}
			if tc.lookup != "" {
				args[4] = tc.lookup
			}
			if tc.inspect {
				args[0] = "inspect"
			}
			if tc.expectedLease != "" || tc.expectedAttempt != "" || tc.expectedSlug != "" || tc.expectedCloud != "" || tc.expectedExact {
				args = append(args,
					"--expected-provider-lease-id", blank(tc.expectedLease, lease.LeaseID),
					"--expected-provider-attempt-lease-id", blank(tc.expectedAttempt, lease.LeaseID),
					"--expected-provider-slug", blank(tc.expectedSlug, req.RequestedSlug),
					"--expected-provider-resource-id", blank(tc.expectedCloud, lease.Server.CloudID),
				)
			}
			var output bytes.Buffer
			err = (core.App{Stdout: &output, Stderr: &output}).Run(t.Context(), args)
			var exitErr core.ExitError
			code := 0
			if err != nil {
				code = 1
				if core.AsExitError(err, &exitErr) {
					code = exitErr.Code
				}
			}
			t.Logf("canonical stop after reload: exit=%d error=%v output=%q inventory_reads=%d", code, err, output.String(), reloaded.lists)
			after, readErr := os.ReadFile(claimPath)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Errorf("stop replay changed persisted identity: %v", readErr)
			}
			if reloaded.createCalls != 0 {
				t.Error("stop replay created a new resource")
			}
			if len(reloaded.deletedInstances) != 0 || len(reloaded.deletedKeys) != 0 {
				t.Error("replay mutated provider resources without a live ownership binding")
			}
			_, trustErr := os.Stat(trust)
			if tc.wantSuccess && !errors.Is(trustErr, os.ErrNotExist) {
				t.Errorf("successful stop replay retained local SSH artifacts: %v", trustErr)
			} else if !tc.wantSuccess && trustErr != nil {
				t.Errorf("rejected stop replay changed local SSH artifacts: %v", trustErr)
			}
			if tc.wantSuccess {
				if err != nil {
					t.Fatalf("completed fixed lease must acknowledge canonical stop after reload; got exit=%d: %v (valid released tombstone retained)", code, err)
				}
				// A second fresh client/App must still use only the durable receipt.
				again := &stopReplayAWSClient{fakeAWSClient: &fakeAWSClient{}}
				newAWSClient = func(context.Context, Config) (awsClient, error) { return again, nil }
				output.Reset()
				if err := (core.App{Stdout: &output, Stderr: &output}).Run(t.Context(), args); err != nil {
					t.Fatal(err)
				}
				after, err := os.ReadFile(claimPath)
				if err != nil || !bytes.Equal(before, after) || again.createCalls != 0 || len(again.deletedInstances)+len(again.deletedKeys) != 0 {
					t.Fatalf("repeated replay changed durable/provider state: %v", err)
				}
			} else if err == nil {
				t.Fatal("missing inventory acknowledged cleanup without exact completed ownership evidence")
			} else if strings.Contains(output.String(), "deleted lease=") || strings.Contains(output.String(), "released lease=") {
				t.Fatalf("failed replay printed a success acknowledgement: %q", output.String())
			}
		})
	}
}

func assertAWSReceiptIdentity(t *testing.T, receipt, acquired core.LeaseClaim) {
	t.Helper()
	if err := validateAWSTerminalReceipt(receipt, acquired.LeaseID); err != nil {
		t.Fatal(err)
	}
	if receipt.CloudID == "" || receipt.CloudID != acquired.CloudID || receipt.Slug != acquired.Slug ||
		receipt.ProviderScope != acquired.ProviderScope || receipt.RepoRoot != acquired.RepoRoot || receipt.ClaimedAt != acquired.ClaimedAt {
		t.Fatal("cleanup lost the acquired resource/repository identity")
	}
	for _, label := range []string{"crabbox", "created_by", "provider", "lease", "slug", "aws_account_id", "aws_region", "fixed_intent_sha256", "provider_key", "aws_key_pair_id"} {
		if receipt.Labels[label] == "" || receipt.Labels[label] != acquired.Labels[label] {
			t.Fatalf("receipt lost original label %s", label)
		}
	}
	if len(receipt.Labels) != 10 || receipt.SSHHost != "" || receipt.SSHPort != 0 || receipt.TargetOS != "" || receipt.IdleTimeoutSeconds != 0 {
		t.Fatal("receipt retained active-only state")
	}
	want := *acquired.FixedCreateIntent
	want.State = "released"
	want.Attempt = nil
	want.FailedAttempts = nil
	if !reflect.DeepEqual(*receipt.FixedCreateIntent, want) {
		t.Fatal("receipt changed immutable intent fields")
	}
}

func setupAWSReplayClaim(t *testing.T) (*awsLeaseBackend, *fakeAWSClient, AcquireRequest, LeaseTarget, string) {
	t.Helper()
	dirs := testutil.IsolateUserDirs(t)
	fake := &fakeAWSClient{}
	t.Cleanup(installFixedAWSTestClient(t, fake))
	cfg := fixedAWSTestConfig()
	cfg.AWSSSHCIDRs = []string{"127.0.0.1/32"}
	req := AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedLeaseID: "cbx_abcdef123491", RequestedSlug: "receipt-fence"}
	backend := NewAWSLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	lease, err := backend.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	lease.SSH = SSHTarget{}
	lease.Server.PublicNet.IPv4.IP = ""
	fake.servers[0].PublicNet.IPv4.IP = ""
	return backend, fake, req, lease, filepath.Join(dirs.StateHome, "crabbox", "claims", lease.LeaseID+".json")
}

func TestAWSFixedStopRetriesLocalSSHCleanup(t *testing.T) {
	backend, fake, req, lease, path := setupAWSReplayClaim(t)
	acquired, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	key, err := core.TestboxKeyPath(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(key)
	saved := filepath.Join(t.TempDir(), "lease-ssh")
	if err := os.Rename(dir, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("synthetic cleanup obstacle"), 0o600); err != nil {
		t.Fatal(err)
	}
	outcome, err := backend.ReleaseLeaseWithOutcome(t.Context(), ReleaseLeaseRequest{Lease: lease})
	if err == nil || !outcome.Terminal || !strings.Contains(err.Error(), "local SSH cleanup") {
		t.Fatalf("confirmed remote deletion must report pending local cleanup: outcome=%+v err=%v", outcome, err)
	}
	receipt, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	assertAWSReceiptIdentity(t, receipt, acquired)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.deletedInstances, []string{lease.Server.CloudID}) ||
		!reflect.DeepEqual(fake.deletedKeys, []string{core.ServerProviderKey(lease.Server)}) {
		t.Fatal("local cleanup failure did not follow exact provider cleanup")
	}
	fresh := &fakeAWSClient{}
	newAWSClient = func(context.Context, Config) (awsClient, error) { return fresh, nil }
	backend = NewAWSLeaseBackend(Provider{}.Spec(), backend.Cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	target, err := backend.Resolve(t.Context(), ResolveRequest{ID: lease.LeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err = backend.ReleaseLeaseWithOutcome(t.Context(), ReleaseLeaseRequest{Lease: target})
	if err == nil || !outcome.Terminal || !strings.Contains(err.Error(), "local SSH cleanup") {
		t.Fatalf("terminal replay must report pending local cleanup: outcome=%+v err=%v", outcome, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed local cleanup replay changed terminal receipt")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, dir); err != nil {
		t.Fatal(err)
	}
	t.Chdir(req.Repo.Root)
	configPath := filepath.Join(req.Repo.Root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("provider: aws\naws:\n  region: us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	var output bytes.Buffer
	if err := (core.App{Stdout: &output, Stderr: &output}).Run(t.Context(), []string{"stop", "--provider", "aws", "--id", lease.LeaseID}); err != nil {
		t.Fatalf("canonical local cleanup retry failed: %v; output=%q", err, output.String())
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical retry retained SSH artifacts: %v", err)
	}
	after, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || fresh.createCalls != 0 || len(fresh.deletedInstances)+len(fresh.deletedKeys) != 0 {
		t.Fatal("local cleanup retry changed terminal/provider state")
	}
}

func TestAWSFixedStopReleaseOwnerFence(t *testing.T) {
	backend, _, _, lease, path := setupAWSReplayClaim(t)
	if err := backend.ReleaseLease(t.Context(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		claim       func(*core.LeaseClaim)
		target      func(*LeaseTarget)
		client      func(*stopReplayAWSClient)
		expected    core.ProviderIdentityExpectation
		remove      bool
		wrongRegion bool
	}{
		{name: "unchanged"},
		{name: "durable_account", claim: func(c *core.LeaseClaim) {
			c.Labels["aws_account_id"] = "999999999999"
			c.ProviderScope = "account:999999999999"
			c.FixedCreateIntent.ProviderScope = c.ProviderScope
		}, client: func(c *stopReplayAWSClient) { c.accountID = "999999999999" }},
		{name: "durable_region", claim: func(c *core.LeaseClaim) { c.Labels["aws_region"] = "us-west-2" }},
		{name: "durable_intent", claim: func(c *core.LeaseClaim) {
			c.FixedCreateIntent.Fingerprint = strings.Repeat("b", 64)
			c.Labels["fixed_intent_sha256"] = c.FixedCreateIntent.Fingerprint
		}},
		{name: "durable_resource", claim: func(c *core.LeaseClaim) { c.CloudID = "i-replacement" }},
		{name: "durable_slug", claim: func(c *core.LeaseClaim) {
			c.Slug = "new-slug"
			c.Labels["slug"] = c.Slug
			c.FixedCreateIntent.Slug = c.Slug
		}},
		{name: "durable_repository", claim: func(c *core.LeaseClaim) { c.RepoRoot = "/other" }},
		{name: "durable_checkpoint", claim: func(c *core.LeaseClaim) { c.FixedCreateIntent.CheckpointID = "checkpoint-other" }},
		{name: "durable_created_at", claim: func(c *core.LeaseClaim) {
			c.FixedCreateIntent.CreatedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
		}},
		{name: "durable_revision", claim: func(c *core.LeaseClaim) { c.Revision = "replacement" }},
		{name: "durable_active_again", claim: func(c *core.LeaseClaim) { c.FixedCreateIntent.State = "acquired" }},
		{name: "durable_missing", remove: true},
		{name: "account_changed_after_resolve", client: func(c *stopReplayAWSClient) { c.accountID = "999999999999" }},
		{name: "account_error_after_resolve", client: func(c *stopReplayAWSClient) { c.accountErr = errors.New("account read failed") }},
		{name: "region_changed_after_resolve", wrongRegion: true},
		{name: "inventory_error_after_resolve", client: func(c *stopReplayAWSClient) { c.listErr = errors.New("inventory read failed") }},
		{name: "resource_appeared_after_resolve", client: func(c *stopReplayAWSClient) {
			s := lease.Server
			s.CloudID = "i-conflict"
			s.Labels = maps.Clone(s.Labels)
			s.Labels["provider"] = "other"
			c.servers = []Server{s}
		}},
		{name: "forged_terminal_word_without_snapshot", target: func(l *LeaseTarget) {
			l.Server = Server{Provider: "aws", CloudID: l.Server.CloudID, Name: l.Server.Name, Status: "released", Labels: l.Server.Labels}
		}},
		{name: "forged_target_resource", target: func(l *LeaseTarget) { l.Server.CloudID = "i-forged" }},
		{name: "forged_target_slug", target: func(l *LeaseTarget) { l.Server.Labels["slug"] = "forged-slug" }},
		{name: "forged_target_ssh", target: func(l *LeaseTarget) { l.SSH.Host = "127.0.0.1" }},
		{name: "forged_matching_snapshot_invalid_receipt", claim: func(c *core.LeaseClaim) { c.FixedCreateIntent.Fingerprint = strings.Repeat("c", 64) }, target: func(l *LeaseTarget) {
			claim, err := core.ReadLeaseClaim(l.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			core.SetServerLeaseClaimSnapshot(&l.Server, claim, true)
		}},
		{name: "expected_lease", expected: core.ProviderIdentityExpectation{LeaseID: "cbx_000000000001"}},
		{name: "expected_attempt", expected: core.ProviderIdentityExpectation{AttemptLeaseID: "cbx_000000000001"}},
		{name: "expected_slug", expected: core.ProviderIdentityExpectation{LeaseID: lease.LeaseID, Slug: "wrong-slug"}},
		{name: "expected_resource", expected: core.ProviderIdentityExpectation{LeaseID: lease.LeaseID, ResourceID: "i-wrong"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			client := &stopReplayAWSClient{fakeAWSClient: &fakeAWSClient{}}
			newAWSClient = func(context.Context, Config) (awsClient, error) { return client, nil }
			cfg := backend.Cfg
			cfg.Capacity.Regions = []string{"us-west-2"}
			fresh := NewAWSLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
			target, err := fresh.Resolve(t.Context(), ResolveRequest{ID: lease.LeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if tc.claim != nil {
				if err := core.WithDurableLeaseClaimLock(lease.LeaseID, func(c *core.LeaseClaim, _ bool, persist func() error) error { tc.claim(c); return persist() }); err != nil {
					t.Fatal(err)
				}
			}
			if tc.remove {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if tc.target != nil {
				tc.target(&target)
			}
			if tc.client != nil {
				tc.client(client)
			}
			if tc.wrongRegion {
				fresh.Cfg.AWSRegion = "us-west-2"
				fresh.Cfg.Capacity.Regions = nil
			}
			before, _ := os.ReadFile(path)
			outcome, err := fresh.ReleaseLeaseWithOutcome(t.Context(), ReleaseLeaseRequest{Lease: target, ExpectedProviderIdentity: tc.expected})
			if outcome.Terminal != (tc.name == "unchanged") {
				t.Errorf("terminal receipt outcome=%+v case=%s", outcome, tc.name)
			}
			if tc.name == "unchanged" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("release owner accepted stale/forged evidence")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) || client.createCalls != 0 || len(client.deletedInstances)+len(client.deletedKeys) != 0 {
				t.Fatal("replay mutated durable/provider state")
			}
		})
	}
}

func TestAWSFixedCleanupFailureThenStopReplay(t *testing.T) {
	for _, mode := range []string{"orphan", "missing_visible", "claimed_live"} {
		t.Run(mode, func(t *testing.T) {
			backend, fake, req, lease, path := setupAWSReplayClaim(t)
			fake.servers[0].Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
			if err := core.WithDurableLeaseClaimLock(lease.LeaseID, func(c *core.LeaseClaim, _ bool, persist func() error) error {
				c.Labels["expires_at"] = fake.servers[0].Labels["expires_at"]
				return persist()
			}); err != nil {
				t.Fatal(err)
			}
			acquired, err := core.ReadLeaseClaim(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			visible := append([]Server(nil), fake.servers...)
			if mode == "orphan" {
				fake.servers = nil
			}
			if mode == "missing_visible" {
				fake.getErr = core.Exit(4, "aws instance not found: %s", lease.Server.CloudID)
			}
			keyErr := errors.New("synthetic exact key cleanup failure")
			fake.deleteKeyErr = keyErr
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.Cleanup(t.Context(), CleanupRequest{}); !errors.Is(err, keyErr) {
				t.Fatalf("cleanup error=%v, want key failure", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failed exact cleanup changed the acquired claim")
			}
			if !reflect.DeepEqual(fake.deletedKeys, []string{acquired.Labels["aws_key_pair_id"]}) {
				t.Fatalf("wrong exact key cleanup: %v", fake.deletedKeys)
			}
			fake.servers = nil
			if _, err := backend.Resolve(t.Context(), ResolveRequest{ID: lease.LeaseID, ReleaseOnly: true}); err == nil {
				t.Fatal("missing acquired inventory acknowledged outstanding cleanup")
			}
			if mode != "orphan" {
				fake.servers = visible
			}
			fake.deleteKeyErr = nil
			if err := backend.Cleanup(t.Context(), CleanupRequest{}); err != nil {
				t.Fatal(err)
			}
			key, err := core.TestboxKeyPath(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Dir(key)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("successful %s cleanup retained local SSH artifacts: %v", mode, err)
			}
			receipt, err := core.ReadLeaseClaim(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			assertAWSReceiptIdentity(t, receipt, acquired)
			if !reflect.DeepEqual(fake.deletedKeys, []string{acquired.Labels["aws_key_pair_id"], acquired.Labels["aws_key_pair_id"]}) {
				t.Fatalf("successful cleanup did not own exact key transition: %v", fake.deletedKeys)
			}
			completed, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			fresh := &fakeAWSClient{}
			newAWSClient = func(context.Context, Config) (awsClient, error) { return fresh, nil }
			t.Chdir(req.Repo.Root)
			configPath := filepath.Join(req.Repo.Root, "config.yaml")
			if err := os.WriteFile(configPath, []byte("provider: aws\naws:\n  region: us-east-1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", configPath)
			var output bytes.Buffer
			if err := (core.App{Stdout: &output, Stderr: &output}).Run(t.Context(), []string{"stop", "--provider", "aws", "--id", lease.LeaseID}); err != nil {
				t.Fatal(err)
			}
			after, err = os.ReadFile(path)
			if err != nil || !bytes.Equal(completed, after) || fresh.createCalls != 0 || len(fresh.deletedKeys)+len(fresh.deletedInstances) != 0 {
				t.Fatal("fresh stop did not replay exact successful cleanup")
			}
		})
	}
}

func TestAWSFixedOldCompactReceiptRemainsSingleUse(t *testing.T) {
	backend, fake, req, lease, path := setupAWSReplayClaim(t)
	if err := backend.ReleaseLease(t.Context(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	compact := fixedAWSLeaseKind
	compact.TerminalIdentityLabels = nil
	claim = compact.TerminalClaim(claim, time.Now().UTC())
	before, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	fake.servers = nil
	creates, deletes, keys := fake.createCalls, len(fake.deletedInstances), len(fake.deletedKeys)
	if _, err := backend.Resolve(t.Context(), ResolveRequest{ID: lease.LeaseID, ReleaseOnly: true}); err == nil {
		t.Fatal("old compact receipt acknowledged scope it cannot prove")
	}
	if _, err := backend.Acquire(t.Context(), req); err == nil {
		t.Fatal("old compact receipt allowed fixed-ID reallocation")
	}
	if err := backend.Cleanup(t.Context(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || fake.createCalls != creates || len(fake.deletedInstances) != deletes || len(fake.deletedKeys) != keys {
		t.Fatal("old compact receipt changed or permitted provider mutations")
	}
}
