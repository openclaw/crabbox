package machine0

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type machine0HeartbeatClock time.Time

func (clock machine0HeartbeatClock) Now() time.Time { return time.Time(clock) }

func TestMachine0StatusTouchAuthorizesOrdinaryAndFixedClaims(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		name := "ordinary"
		if fixed {
			name = "fixed"
		}
		t.Run(name, func(t *testing.T) {
			b, _, req := fixedMachine0TestFixture(t)
			if !fixed {
				req.RequestedLeaseID = ""
			}
			lease, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			claim := readFixedMachine0Claim(t, lease.LeaseID)
			authorizer, ok := any(b).(core.StatusTouchClaimAuthorizer)
			if !ok {
				t.Fatal("Machine0 does not authorize its dynamic immutable resource scope")
			}
			if err := authorizer.AuthorizeStatusTouchClaim(context.Background(), lease, claim); err != nil {
				t.Fatalf("rejected provider=%s scope=%s: %v", claim.Provider, claim.ProviderScope, err)
			}
		})
	}
}

func TestMachine0TouchDurablyPersistsLifecycleAndIdleTimeout(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fixed    bool
		override time.Duration
	}{
		{name: "ordinary preserves persisted timeout after config changes"},
		{name: "fixed preserves persisted timeout after config changes", fixed: true},
		{name: "fixed persists explicit timeout override", fixed: true, override: 75 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			b.cfg.IdleTimeout = 45 * time.Minute
			if !tc.fixed {
				req.RequestedLeaseID = ""
			}
			lease, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			initial := readFixedMachine0Claim(t, lease.LeaseID)
			labels := make(map[string]string, len(initial.Labels)+1)
			for key, value := range initial.Labels {
				labels[key] = value
			}
			labels["durable_metadata"] = "preserved"
			initial, err = core.UpdateLeaseClaimLabelsIfUnchanged(lease.LeaseID, initial, labels)
			if err != nil {
				t.Fatal(err)
			}
			api.machine.PricePerHour = 123_000
			api.machines = []machine{api.machine}
			lease, err = b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, StatusOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			core.SetServerLeaseClaimSnapshot(&lease.Server, initial, true)
			previousTouch, err := time.Parse(time.RFC3339, initial.LastUsedAt)
			if err != nil {
				t.Fatal(err)
			}
			now := previousTouch.Add(2 * time.Minute)
			b.rt.Clock = machine0HeartbeatClock(now)
			b.cfg.IdleTimeout = 15 * time.Minute
			touch := TouchRequest{Lease: lease, State: "running", IdleTimeout: b.cfg.IdleTimeout}
			wantTimeout := 45 * time.Minute
			if tc.override > 0 {
				touch.IdleTimeoutOverride = &tc.override
				wantTimeout = tc.override
			}

			server, err := b.Touch(context.Background(), touch)
			if err != nil {
				t.Fatal(err)
			}
			updated := readFixedMachine0Claim(t, lease.LeaseID)
			wantSeconds := strconv.Itoa(int(wantTimeout.Seconds()))
			if updated.LastUsedAt != now.Format(time.RFC3339) || updated.Revision == initial.Revision ||
				updated.IdleTimeoutSeconds != int(wantTimeout.Seconds()) || updated.Labels["idle_timeout_secs"] != wantSeconds ||
				updated.Labels["last_touched_at"] != core.LeaseLabelTime(now) || updated.Labels["state"] != "running" ||
				updated.Labels["durable_metadata"] != "preserved" || updated.Labels["price_per_hour_micro"] != "123000" {
				t.Fatalf("durable touch lost persisted or dynamic lifecycle state: %#v", updated)
			}
			snapshot, exists, set := core.ServerLeaseClaimSnapshot(server)
			if !set || !exists || !reflect.DeepEqual(snapshot, updated) || !reflect.DeepEqual(server.Labels, updated.Labels) {
				t.Fatalf("returned server did not carry committed claim: snapshot=%#v exists=%t set=%t server=%#v", snapshot, exists, set, server)
			}

			fresh := testBackendWithAPI(api)
			fresh.cfg.IdleTimeout = 5 * time.Minute
			resolved, err := fresh.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, StatusOnly: true})
			if err != nil || resolved.Server.Labels["idle_timeout_secs"] != wantSeconds ||
				resolved.Server.Labels["last_touched_at"] != core.LeaseLabelTime(now) {
				t.Fatalf("fresh backend lost committed lifecycle: lease=%#v err=%v", resolved, err)
			}
		})
	}
}

func TestMachine0TouchRejectsOwnershipChangesWithoutMutation(t *testing.T) {
	zero := time.Duration(0)
	negative := -time.Minute
	tooShort := 100 * time.Millisecond
	tests := []struct {
		name   string
		mutate func(*testing.T, *LeaseTarget, *LeaseClaim, *TouchRequest)
		want   string
	}{
		{name: "missing snapshot", mutate: func(_ *testing.T, lease *LeaseTarget, _ *LeaseClaim, _ *TouchRequest) {
			core.SetServerLeaseClaimSnapshot(&lease.Server, LeaseClaim{}, false)
		}, want: "no exact claim snapshot"},
		{name: "noncanonical lease", mutate: func(_ *testing.T, lease *LeaseTarget, _ *LeaseClaim, _ *TouchRequest) {
			lease.LeaseID = "not-canonical"
		}, want: "canonical lease"},
		{name: "wrong lease", mutate: func(_ *testing.T, lease *LeaseTarget, _ *LeaseClaim, _ *TouchRequest) {
			lease.LeaseID = "cbx_abcdef123457"
		}, want: "canonical lease"},
		{name: "wrong server provider", mutate: func(_ *testing.T, lease *LeaseTarget, _ *LeaseClaim, _ *TouchRequest) {
			lease.Server.Provider = "external"
		}, want: "provider"},
		{name: "wrong claim provider", mutate: func(_ *testing.T, _ *LeaseTarget, claim *LeaseClaim, _ *TouchRequest) {
			claim.Provider = "external"
		}, want: "provider"},
		{name: "missing server resource", mutate: func(_ *testing.T, lease *LeaseTarget, _ *LeaseClaim, _ *TouchRequest) {
			lease.Server.CloudID = ""
		}, want: "immutable Machine0 identity"},
		{name: "wrong server resource", mutate: func(_ *testing.T, lease *LeaseTarget, _ *LeaseClaim, _ *TouchRequest) {
			lease.Server.CloudID = "vm-replacement"
			lease.Server.ImmutableID = "vm-replacement"
		}, want: "immutable Machine0 identity"},
		{name: "missing server immutable identity", mutate: func(_ *testing.T, lease *LeaseTarget, _ *LeaseClaim, _ *TouchRequest) {
			lease.Server.ImmutableID = ""
		}, want: "immutable Machine0 identity"},
		{name: "wrong claim resource", mutate: func(_ *testing.T, _ *LeaseTarget, claim *LeaseClaim, _ *TouchRequest) {
			claim.CloudID = "vm-replacement"
		}, want: "Machine0 id mismatch"},
		{name: "missing claim immutable identity", mutate: func(_ *testing.T, _ *LeaseTarget, claim *LeaseClaim, _ *TouchRequest) {
			claim.CloudImmutableID = ""
		}, want: "immutable Machine0 identity"},
		{name: "wrong claim scope", mutate: func(_ *testing.T, _ *LeaseTarget, claim *LeaseClaim, _ *TouchRequest) {
			claim.ProviderScope = machineScope("vm-replacement")
		}, want: "provider scope mismatch"},
		{name: "missing claim scope", mutate: func(_ *testing.T, _ *LeaseTarget, claim *LeaseClaim, _ *TouchRequest) {
			claim.ProviderScope = ""
		}, want: "provider scope mismatch"},
		{name: "zero timeout override", mutate: func(_ *testing.T, _ *LeaseTarget, _ *LeaseClaim, touch *TouchRequest) {
			touch.IdleTimeoutOverride = &zero
		}, want: "must be positive"},
		{name: "negative timeout override", mutate: func(_ *testing.T, _ *LeaseTarget, _ *LeaseClaim, touch *TouchRequest) {
			touch.IdleTimeoutOverride = &negative
		}, want: "must be positive"},
		{name: "subsecond timeout override", mutate: func(_ *testing.T, _ *LeaseTarget, _ *LeaseClaim, touch *TouchRequest) {
			touch.IdleTimeoutOverride = &tooShort
		}, want: "at least one second"},
		{name: "concurrent claim replacement", mutate: func(t *testing.T, lease *LeaseTarget, claim *LeaseClaim, _ *TouchRequest) {
			labels := make(map[string]string, len(claim.Labels)+1)
			for key, value := range claim.Labels {
				labels[key] = value
			}
			labels["replacement_owner"] = "preserved"
			if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(lease.LeaseID, *claim, labels); err != nil {
				t.Fatal(err)
			}
		}, want: "claim changed"},
		{name: "removed claim", mutate: func(t *testing.T, lease *LeaseTarget, claim *LeaseClaim, _ *TouchRequest) {
			if err := core.RemoveLeaseClaimIfUnchanged(lease.LeaseID, *claim); err != nil {
				t.Fatal(err)
			}
		}, want: "claim changed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, _, req := fixedMachine0TestFixture(t)
			lease, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			leaseID := lease.LeaseID
			claim := readFixedMachine0Claim(t, leaseID)
			core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
			touch := TouchRequest{State: "running"}
			tc.mutate(t, &lease, &claim, &touch)
			if tc.name != "missing snapshot" {
				core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
			}
			touch.Lease = lease
			before, existed, err := core.ReadLeaseClaimWithPresence(leaseID)
			if err != nil {
				t.Fatal(err)
			}

			if _, err := b.Touch(context.Background(), touch); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("touch error=%v, want %q", err, tc.want)
			}
			after, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
			if err != nil || exists != existed || !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected touch mutated or recreated claim: before=%#v existed=%t after=%#v exists=%t err=%v", before, existed, after, exists, err)
			}
		})
	}
}

func TestMachine0FixedHeartbeatCommandRenewsRegisteredLease(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	b.cfg.IdleTimeout = 45 * time.Minute
	b.rt.Clock = machine0HeartbeatClock(time.Now().Add(-2 * time.Minute))
	req.Keep = true
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	initial := readFixedMachine0Claim(t, lease.LeaseID)
	if initial.Provider != core.FixedMachine0ClaimProvider || initial.ProviderScope != machineScope(api.machine.ID) {
		t.Fatalf("fixed Machine0 claim was not durably bound: %#v", initial)
	}

	var renewals atomic.Int32
	coordinator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		renewals.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/v1/leases/"+lease.LeaseID+"/heartbeat" {
			t.Errorf("coordinator request=%s %s", request.Method, request.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": core.CoordinatorLease{
			ID: lease.LeaseID, Slug: req.RequestedSlug, Provider: providerName,
			State: "active", IdleTimeoutSeconds: initial.IdleTimeoutSeconds,
		}})
	}))
	defer coordinator.Close()

	configureMachine0CommandFixture(t, api.machine, coordinator.URL, "15m")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "fixture-coordinator-token")

	for invocation := 1; invocation <= 2; invocation++ {
		var stdout, stderr bytes.Buffer
		err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{
			"heartbeat", "--provider", providerName, "--id", lease.LeaseID, "--json",
		})
		if err != nil {
			t.Fatalf("fresh heartbeat invocation %d failed before coordinator renewal: %v (renewals=%d stderr=%q)", invocation, err, renewals.Load(), stderr.String())
		}
		if renewals.Load() != int32(invocation) {
			t.Fatalf("fresh heartbeat invocation %d renewals=%d", invocation, renewals.Load())
		}
	}

	updated := readFixedMachine0Claim(t, lease.LeaseID)
	if updated.LastUsedAt == initial.LastUsedAt || updated.IdleTimeoutSeconds != initial.IdleTimeoutSeconds || updated.Labels["idle_timeout_secs"] != "2700" {
		t.Fatalf("fresh heartbeat invocations did not preserve and renew durable lifecycle: initial=%#v updated=%#v", initial, updated)
	}
}

func TestMachine0DirectCommandsRenewLifecycle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Machine0 fixes SSH port 22, which macOS cannot bind unprivileged")
	}

	for _, tc := range []struct {
		name string
		args func(string) []string
	}{
		{name: "run", args: func(id string) []string {
			return []string{"run", "--provider", providerName, "--id", id, "--no-sync", "--", "true"}
		}},
		{name: "connect", args: func(id string) []string {
			return []string{"connect", "--provider", providerName, "--id", id}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			repoRoot, err := filepath.Abs("../../..")
			if err != nil {
				t.Fatal(err)
			}
			req.Repo.Root = repoRoot
			b.cfg.IdleTimeout = 45 * time.Minute
			b.rt.Clock = machine0HeartbeatClock(time.Now().Add(-2 * time.Minute))
			req.Keep = true
			lease, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			initial := readFixedMachine0Claim(t, lease.LeaseID)
			if initial.Provider != core.FixedMachine0ClaimProvider || initial.CloudID != api.machine.ID ||
				initial.CloudImmutableID != api.machine.ID || initial.ProviderScope != machineScope(api.machine.ID) {
				t.Fatalf("fixed Machine0 claim was not immutably bound: %#v", initial)
			}
			initialUsedAt, err := time.Parse(time.RFC3339, initial.LastUsedAt)
			if err != nil {
				t.Fatal(err)
			}
			api.machine.IP = "127.0.0.1"

			listener, err := net.Listen("tcp", "127.0.0.1:22")
			if err != nil {
				if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EADDRINUSE) {
					t.Skipf("Machine0 requires an available privileged SSH port 22: %v", err)
				}
				t.Fatalf("bind Machine0 SSH readiness listener: %v", err)
			}
			listenerDone := make(chan struct{})
			go func() {
				defer close(listenerDone)
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					_ = conn.Close()
				}
			}()
			t.Cleanup(func() {
				_ = listener.Close()
				<-listenerDone
			})

			binDir := configureMachine0CommandFixture(t, api.machine, "", "45m")
			stateDir, err := core.CrabboxStateDir()
			if err != nil {
				t.Fatal(err)
			}
			claimLog := filepath.Join(binDir, "ssh-claims.json")
			t.Setenv("CRABBOX_MACHINE0_CLAIM_PATH", filepath.Join(stateDir, "claims", lease.LeaseID+".json"))
			t.Setenv("CRABBOX_MACHINE0_CLAIM_LOG", claimLog)
			ssh := `#!/bin/sh
/bin/cat "$CRABBOX_MACHINE0_CLAIM_PATH" >> "$CRABBOX_MACHINE0_CLAIM_LOG" || exit 1
current=
for arg do current="$arg"; done
case "$current" in
  *'payload_b64="'*'"; decoded=; if command -v base64'*)
    payload=${current#*'payload_b64="'}
    payload=${payload%%'"; decoded=; if command -v base64'*}
    current=$(printf %s "$payload" | /usr/bin/base64 --decode) || exit 1
    ;;
esac
case "$current" in
  *"protocol_action='acquire'"*) printf ACQUIRED ;;
  *"protocol_action='renew'"*) printf RENEWED ;;
  *"protocol_action='inspect'"*) printf OWNED ;;
  *"protocol_action='release'"*) printf RELEASED ;;
esac
exit 0
`
			if err := os.WriteFile(filepath.Join(binDir, "ssh"), []byte(ssh), 0o700); err != nil {
				t.Fatal(err)
			}

			var stdout, stderr bytes.Buffer
			if err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), tc.args(lease.LeaseID)); err != nil {
				t.Fatalf("direct %s failed: %v\nstdout=%s\nstderr=%s", tc.name, err, stdout.String(), stderr.String())
			}
			for _, warning := range []string{"direct touch", "no exact claim snapshot", "refusing touch"} {
				if strings.Contains(stderr.String(), warning) {
					t.Fatalf("direct %s reported %q: %s", tc.name, warning, stderr.String())
				}
			}
			observedClaims, err := os.ReadFile(claimLog)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(observedClaims, []byte(`"state": "running"`)) {
				t.Fatalf("direct %s never durably entered running during SSH execution: %s", tc.name, observedClaims)
			}

			updated := readFixedMachine0Claim(t, lease.LeaseID)
			updatedUsedAt, err := time.Parse(time.RFC3339, updated.LastUsedAt)
			if err != nil {
				t.Fatal(err)
			}
			if updated.Revision == initial.Revision || !updatedUsedAt.After(initialUsedAt) ||
				updated.Labels["state"] != "ready" || updated.IdleTimeoutSeconds != initial.IdleTimeoutSeconds ||
				updated.Labels["idle_timeout_secs"] != strconv.Itoa(initial.IdleTimeoutSeconds) ||
				updated.Provider != initial.Provider || updated.CloudID != initial.CloudID ||
				updated.CloudImmutableID != initial.CloudImmutableID || updated.ProviderScope != initial.ProviderScope {
				t.Fatalf("direct %s lost durable lifecycle or immutable Machine0 ownership: initial=%#v updated=%#v", tc.name, initial, updated)
			}
		})
	}
}

func configureMachine0CommandFixture(t *testing.T, item machine, coordinatorURL, idleTimeout string) string {
	t.Helper()
	machineJSON, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	cliPath := filepath.Join(binDir, "machine0")
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  'ls --json') printf '%%s\n' '[%s]' ;;
  'get %s --json') printf '%%s\n' '%s' ;;
  *) exit 2 ;;
esac
`, machineJSON, item.Name, machineJSON)
	if err := os.WriteFile(cliPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_MODE", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")

	mode := "managed"
	if coordinatorURL != "" {
		mode = "registered"
	}
	configPath := filepath.Join(binDir, "config.yaml")
	config := fmt.Sprintf("provider: machine0\nlease:\n  idleTimeout: %s\nbroker:\n  url: %q\n  mode: %s\nmachine0:\n  cliPath: %q\n", idleTimeout, coordinatorURL, mode, cliPath)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	return binDir
}
