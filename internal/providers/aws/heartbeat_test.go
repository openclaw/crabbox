package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func awsHeartbeatFixture(t *testing.T, fixed bool) (*awsLeaseBackend, *fakeAWSClient, LeaseTarget) {
	t.Helper()
	testutil.IsolateUserDirs(t)
	fake := &fakeAWSClient{}
	t.Cleanup(installFixedAWSTestClient(t, fake))
	cfg := fixedAWSTestConfig()
	cfg.AWSSSHCIDRs = []string{"198.51.100.10/32"}
	cfg.IdleTimeout = 45 * time.Minute
	backend := NewAWSLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	request := AcquireRequest{RequestedSlug: "heartbeat"}
	if fixed {
		request.RequestedLeaseID = "cbx_abcdef123499"
	}
	lease, err := backend.Acquire(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !fixed {
		fake.servers = []Server{lease.Server}
		if err := core.ClaimLeaseTargetForConfig(lease.LeaseID, "heartbeat", cfg, lease.Server, lease.SSH, cfg.IdleTimeout); err != nil {
			t.Fatal(err)
		}
	}
	initial, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if fixed && (initial.Provider != core.FixedAWSClaimProvider || initial.ProviderScope != "account:123456789012") {
		t.Fatalf("fixed AWS producer did not bind the account: claim=%+v err=%v", initial, err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("provider: aws\naws:\n  region: us-east-1\nlease:\n  idleTimeout: 15m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_MODE", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	fake.tagged = nil
	fake.tagLabels = nil
	return backend, fake, lease
}

func TestAWSFixedHeartbeatCommandRenewsAccountScopedLease(t *testing.T) {
	_, fake, lease := awsHeartbeatFixture(t, true)
	initial, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	for invocation := 1; invocation <= 2; invocation++ {
		var stdout, stderr bytes.Buffer
		args := []string{"heartbeat", "--provider", "aws", "--id", lease.LeaseID, "--json"}
		if invocation == 1 {
			args = append(args, "--idle-timeout", "90m")
		}
		err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), args)
		if err != nil {
			t.Fatalf("heartbeat invocation %d: %v (stderr=%q)", invocation, err, stderr.String())
		}
		if len(fake.tagged) != invocation || fake.tagged[invocation-1] != lease.Server.CloudID {
			t.Fatalf("heartbeat did not renew the owned instance: %v", fake.tagged)
		}
	}
	updated, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil || updated.ProviderScope != initial.ProviderScope || updated.IdleTimeoutSeconds != 5400 || updated.LastUsedAt == "" || updated.Labels["idle_timeout_secs"] != "5400" || fake.tagLabels[1]["idle_timeout_secs"] != "5400" {
		t.Fatalf("heartbeat did not preserve account ownership and persist renewal: claim=%+v err=%v", updated, err)
	}
}

type runResolutionAWSClient struct {
	*fakeAWSClient
	listCalls int
}

func (c *runResolutionAWSClient) ListCrabboxServers(ctx context.Context) ([]Server, error) {
	c.listCalls++
	return c.fakeAWSClient.ListCrabboxServers(ctx)
}

func TestAWSRunResolutionPreservesClaimAndExactAuthority(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		scenarios := []string{"ready", "legacy missing account"}
		if fixed {
			scenarios = []string{"ready", "wrong account", "replacement resource", "checkpoint hold"}
		}
		for _, scenario := range scenarios {
			t.Run(fmt.Sprintf("fixed=%t/%s", fixed, scenario), func(t *testing.T) {
				b, fake, lease := awsHeartbeatFixture(t, fixed)
				if scenario == "checkpoint hold" {
					if err := core.WithDurableLeaseClaimLock(lease.LeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
						claim.CheckpointCapture = &core.CheckpointCaptureBinding{ID: "chk_runhold", Revision: claim.Revision, BoundRevision: claim.Revision}
						return persist()
					}); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "legacy missing account" {
					if err := core.WithDurableLeaseClaimLock(lease.LeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
						delete(claim.Labels, "aws_account_id")
						delete(claim.Labels, "aws_key_pair_id")
						return persist()
					}); err != nil {
						t.Fatal(err)
					}
					fake.accountErr = errors.New("ordinary run resolution must not require STS")
				}
				before, err := core.ReadLeaseClaim(lease.LeaseID)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "wrong account" {
					fake.accountID = "999999999999"
				}
				if scenario == "replacement resource" {
					fake.servers[0].CloudID = "i-replacement"
				}
				lookup := &runResolutionAWSClient{fakeAWSClient: fake}
				oldClient := newAWSClient
				newAWSClient = func(_ context.Context, cfg Config) (awsClient, error) {
					if cfg.AWSRegion != before.Labels["aws_region"] || len(cfg.Capacity.Regions) != 0 {
						t.Errorf("run lookup escaped claim region: region=%s alternatives=%v", cfg.AWSRegion, cfg.Capacity.Regions)
					}
					return lookup, nil
				}
				t.Cleanup(func() { newAWSClient = oldClient })
				b.Cfg.AWSRegion = "us-west-2"
				b.Cfg.Capacity.Regions = []string{"eu-west-1", before.Labels["aws_region"]}
				fake.getIDs = nil
				creates := fake.createCalls
				resolved, resolveErr := b.ResolveRunLeaseUnderClaim(t.Context(), ResolveRequest{ID: lease.LeaseID, Prepare: true}, before)
				if lookup.listCalls != 0 || len(fake.getIDs) != 1 || fake.getIDs[0] != before.CloudID {
					t.Fatalf("run lookup must read only the bound instance: lists=%d gets=%v", lookup.listCalls, fake.getIDs)
				}
				if b.Cfg.AWSRegion != "us-west-2" || !reflect.DeepEqual(b.Cfg.Capacity.Regions, []string{"eu-west-1", before.Labels["aws_region"]}) {
					t.Fatal("run lookup changed backend region configuration")
				}
				if scenario == "ready" || scenario == "legacy missing account" {
					if resolveErr != nil || resolved.LeaseID != lease.LeaseID || resolved.Server.CloudID != before.CloudID || resolved.SSH.Key != lease.SSH.Key {
						t.Fatalf("run resolution lost its exact resource or stored key: %v", resolveErr)
					}
				} else if resolveErr == nil {
					t.Fatal("run resolution accepted changed authority")
				}
				after, err := core.ReadLeaseClaim(lease.LeaseID)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("provider run resolution published a claim: %v", err)
				}
				if len(fake.tagged)+len(fake.deletedInstances)+len(fake.deletedKeys) != 0 || fake.createCalls != creates {
					t.Fatal("provider run resolution mutated AWS resources")
				}
			})
		}
	}
}

func TestAWSHeartbeatClaimReplacementBeforeTagWrite(t *testing.T) {
	_, fake, lease := awsHeartbeatFixture(t, true)
	initial, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	var replacement core.LeaseClaim
	calls := 0
	coordinator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/"+lease.LeaseID+"/heartbeat" {
			t.Errorf("unexpected renewal: %s %s", r.Method, r.URL.Path)
		}
		labels := maps.Clone(initial.Labels)
		labels["replacement_owner"] = "new-owner"
		var err error
		replacement, err = core.UpdateLeaseClaimLabelsIfUnchanged(lease.LeaseID, initial, labels)
		if err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": core.CoordinatorLease{ID: lease.LeaseID, Provider: "aws", State: "active"}})
	}))
	defer coordinator.Close()
	config := fmt.Sprintf("provider: aws\naws:\n  region: us-east-1\nbroker:\n  url: %q\n  mode: registered\n", coordinator.URL)
	if err := os.WriteFile(os.Getenv("CRABBOX_CONFIG"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "fixture-coordinator-token")
	err = (core.App{Stdout: io.Discard, Stderr: io.Discard}).Run(context.Background(), []string{"heartbeat", "--provider", "aws", "--id", lease.LeaseID, "--json"})
	if err == nil || !strings.Contains(err.Error(), "claim changed") || calls != 1 || len(fake.tagged) != 0 {
		t.Fatalf("stale renewal: err=%v coordinatorCalls=%d AWSwrites=%v", err, calls, fake.tagged)
	}
	after, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil || !reflect.DeepEqual(after, replacement) {
		t.Fatalf("replacement claim changed: err=%v", err)
	}
}

func TestAWSTouchRejectsChangedAuthorityBeforeMutation(t *testing.T) {
	for _, name := range []string{
		"missing snapshot", "wrong requested resource", "wrong requested lease", "wrong provider", "wrong scope",
		"wrong caller account", "unavailable caller account", "reassigned live resource", "live account mismatch",
		"live provider key mismatch", "live region mismatch", "live fixed fingerprint mismatch", "terminated instance",
		"missing instance", "removed claim", "invalid override", "failed tag write",
	} {
		t.Run(name, func(t *testing.T) {
			b, fake, lease := awsHeartbeatFixture(t, true)
			claim, err := core.ReadLeaseClaim(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			touch := TouchRequest{Lease: lease, State: "running"}
			live := fake.servers[0]
			live.Labels = maps.Clone(live.Labels)
			live.ProviderMetadata = maps.Clone(live.ProviderMetadata)
			switch name {
			case "missing snapshot":
				touch.Lease.Server = live
			case "wrong requested resource":
				touch.Lease.Server.CloudID = "i-other"
			case "wrong requested lease":
				touch.Lease.LeaseID = "cbx_abcdef123497"
			case "wrong provider":
				touch.Lease.Server.Provider = "gcp"
			case "wrong scope":
				claim.ProviderScope = "account:999999999999"
				core.SetServerLeaseClaimSnapshot(&touch.Lease.Server, claim, true)
			case "wrong caller account":
				fake.accountID = "999999999999"
			case "unavailable caller account":
				fake.accountErr = errors.New("STS unavailable")
			case "reassigned live resource":
				live.Labels["lease"] = "cbx_abcdef123497"
			case "live account mismatch":
				live.Labels["aws_account_id"] = "999999999999"
			case "live provider key mismatch":
				live.Labels["provider_key"] = "crabbox-cbx-abcdef123497"
			case "live region mismatch":
				live.Labels["aws_region"] = "us-west-2"
			case "live fixed fingerprint mismatch":
				live.Labels["fixed_intent_sha256"] = strings.Repeat("f", 64)
			case "terminated instance":
				live.Status = "terminated"
			case "missing instance":
				fake.getErr = errors.New("instance absent")
			case "removed claim":
				if err := core.RemoveLeaseClaimIfUnchanged(lease.LeaseID, claim); err != nil {
					t.Fatal(err)
				}
			case "invalid override":
				zero := time.Duration(0)
				touch.IdleTimeoutOverride = &zero
			case "failed tag write":
				fake.setTagsErr = errors.New("CreateTags rejected")
			}
			fake.get = map[string]Server{lease.Server.CloudID: live}
			before, existed, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = b.Touch(context.Background(), touch)
			writes := 0
			if name == "failed tag write" {
				writes = 1
			}
			if err == nil || len(fake.tagged) != writes {
				t.Fatalf("expected denied renewal without a tag write: err=%v writes=%v", err, fake.tagged)
			}
			after, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if err != nil || exists != existed || !reflect.DeepEqual(after, before) {
				t.Fatalf("denied renewal changed the claim: err=%v", err)
			}
		})
	}
}

func TestAWSStatusWaitAndConnectRenewOrdinaryAndFixedClaims(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH fixture")
	}
	for _, fixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("fixed=%t", fixed), func(t *testing.T) {
			_, fake, lease := awsHeartbeatFixture(t, fixed)
			runAWSLifecycleCommandFixture(t, fake, lease)
		})
	}
}

func runAWSLifecycleCommandFixture(t *testing.T, fake *fakeAWSClient, lease LeaseTarget) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fake.servers[0].PublicNet.IPv4.IP = "127.0.0.1"
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ssh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	config := fmt.Sprintf("provider: aws\nnetwork: public\naws:\n  region: us-east-1\nssh:\n  port: %q\nlease:\n  idleTimeout: 45m\n", port)
	if err := os.WriteFile(os.Getenv("CRABBOX_CONFIG"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	initial, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	fake.tagged, fake.tagLabels = nil, nil
	for _, command := range []string{"status", "status-wait", "connect"} {
		var stdout, stderr bytes.Buffer
		args := []string{command, "--provider", "aws", "--id", lease.LeaseID}
		if command == "status-wait" {
			args[0] = "status"
			args = append(args, "--wait", "--wait-timeout", "5s", "--json")
		}
		if command == "status" {
			args = append(args, "--json")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := (core.App{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr}).Run(ctx, args)
		cancel()
		if err != nil || strings.Contains(stderr.String(), "touch skipped") || strings.Contains(stderr.String(), "touch failed") {
			t.Fatalf("%s failed: err=%v stderr=%s", command, err, stderr.String())
		}
		if command == "status" && len(fake.tagged) != 0 {
			t.Fatal("plain status mutated the provider")
		}
		if command != "status" && len(fake.tagged) == 0 {
			t.Fatalf("%s did not renew provider lifecycle", command)
		}
	}
	updated, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil || updated.ProviderScope != initial.ProviderScope || updated.CloudID != initial.CloudID || updated.Labels["state"] != "ready" || updated.IdleTimeoutSeconds != 2700 {
		t.Fatalf("commands changed ownership or failed to settle lifecycle: claim=%+v err=%v", updated, err)
	}
	if !reflect.DeepEqual(initial.FixedCreateIntent, updated.FixedCreateIntent) {
		t.Fatal("commands rewrote the fixed intent")
	}
	if n := len(fake.tagLabels); n < 3 || fake.tagLabels[n-2]["state"] != "running" || fake.tagLabels[n-1]["state"] != "ready" {
		t.Fatalf("connect did not publish running then ready: %v", fake.tagLabels)
	}
}
