package daytona

import (
	"bytes"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestDaytonaForgetMissing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		forget bool
	}{
		{"provider absence", 404, `{"statusCode":404,"error":"Not Found","message":"Sandbox not found"}`, true},
		{"proxy absence", 404, `<html>Not Found</html>`, false},
		{"empty absence", 404, ``, false},
		{"incomplete absence", 404, `{"message":"not found"}`, false},
		{"conflicting status", 404, `{"statusCode":403,"error":"Not Found","message":"not found"}`, false},
		{"transport error", 0, ``, false},
		{"unauthorized", 401, `{"statusCode":401,"error":"Unauthorized"}`, false},
		{"forbidden", 403, `{"statusCode":403,"error":"Forbidden"}`, false},
		{"service error", 503, `{"statusCode":503,"error":"Unavailable"}`, false},
		{"live sandbox", 200, `{"id":"sandbox-original"}`, false},
		{"empty success", 200, `null`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dirs := testutil.IsolateUserDirs(t)
			repo := t.TempDir()
			t.Chdir(repo)
			for _, name := range []string{"CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_MODE", "CRABBOX_COORDINATOR_TOKEN_COMMAND", "CRABBOX_POND", "CRABBOX_TAILSCALE", "CRABBOX_DAYTONA_JWT_TOKEN", "DAYTONA_JWT_TOKEN", "CRABBOX_DAYTONA_ORGANIZATION_ID", "DAYTONA_ORGANIZATION_ID"} {
				t.Setenv(name, "")
			}
			t.Setenv("CRABBOX_DAYTONA_API_KEY", "synthetic-credential")
			const leaseID = "cbx_123456abcdef"
			const resourceID = "sandbox-original"
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != "GET" || r.URL.Path != "/sandbox/"+resourceID {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if tc.status == 0 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			t.Setenv("CRABBOX_DAYTONA_API_URL", srv.URL)
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(configPath, []byte("provider: daytona\ntarget: linux\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", configPath)
			if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "missing-fixture", "daytona", "", "", repo, time.Minute, false,
				core.Server{Provider: "daytona", CloudID: resourceID}, core.SSHTarget{}); err != nil {
				t.Fatal(err)
			}
			claimPath := filepath.Join(dirs.StateHome, "crabbox", "claims", leaseID+".json")
			before, err := os.ReadFile(claimPath)
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			err = (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{"stop", "--provider", "daytona", "--daytona-forget-missing", "missing-fixture"})
			after, readErr := os.ReadFile(claimPath)
			if tc.forget {
				if err != nil || !os.IsNotExist(readErr) || !strings.Contains(stderr.String(), "forgot local Daytona claim") {
					t.Fatalf("err=%v claim read=%v output=%q", err, readErr, stderr.String())
				}
				t.Logf("exact GET=404; claim removed; %s", strings.TrimSpace(stderr.String()))
			} else if err == nil || readErr != nil || !bytes.Equal(before, after) {
				t.Fatalf("expected unchanged claim: err=%v read=%v", err, readErr)
			}
			if requests.Load() != 1 || strings.Contains(stdout.String()+stderr.String(), "released") {
				t.Errorf("requests=%d output=%q", requests.Load(), stdout.String()+stderr.String())
			}
		})
	}
}

func TestDaytonaForgetMissingRejectsBrokeredStop(t *testing.T) {
	testutil.IsolateUserDirs(t)
	t.Chdir(t.TempDir())
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	for _, name := range []string{"CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_MODE", "CRABBOX_COORDINATOR_TOKEN_COMMAND", "CRABBOX_POND", "CRABBOX_TAILSCALE"} {
		t.Setenv(name, "")
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("provider: daytona\ntarget: linux\ncoordinator: "+srv.URL+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	var stdout, stderr bytes.Buffer
	err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{"stop", "--provider", "daytona", "--daytona-forget-missing", "cbx_123456abcdef"})
	if err == nil || !strings.Contains(err.Error(), "requires direct provider=daytona") || requests.Load() != 0 {
		t.Fatalf("brokered local cleanup was not rejected before requests: err=%v requests=%d", err, requests.Load())
	}
}

func TestDaytonaForgetMissingRejectsOtherCommandsAndProviders(t *testing.T) {
	for _, tc := range []struct{ command, provider string }{{"run", "daytona"}, {"stop", "other-provider"}} {
		t.Run(tc.command+"/"+tc.provider, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Provider = tc.provider
			fs := flag.NewFlagSet(tc.command, flag.ContinueOnError)
			values := RegisterDaytonaProviderFlags(fs, cfg)
			if tc.command != "stop" {
				if fs.Lookup("daytona-forget-missing") != nil {
					t.Fatal("stop-only recovery flag was registered on another command")
				}
				return
			}
			if err := fs.Parse([]string{"--daytona-forget-missing"}); err != nil {
				t.Fatal(err)
			}
			if err := ApplyDaytonaProviderFlags(&cfg, fs, values); err == nil || cfg.Daytona.ForgetMissing {
				t.Fatalf("incompatible flag accepted: err=%v enabled=%t", err, cfg.Daytona.ForgetMissing)
			}
		})
	}
}

func TestDaytonaForgetMissingRequiresUnheldDirectClaim(t *testing.T) {
	for _, scenario := range []string{"no claim", "no sandbox ID", "checkpoint", "coordinator", "adapter", "pending adapter"} {
		t.Run(scenario, func(t *testing.T) {
			f, b, repo := newDaytonaLifecycleFixture(t)
			_, leaseID, _, err := b.createDaytonaSandbox(t.Context(), repo, true, false, "")
			if err != nil {
				t.Fatal(err)
			}
			claim, _, err := core.ReadLeaseClaimWithPresence(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			changed := claim
			switch scenario {
			case "no sandbox ID":
				changed.CloudID = ""
			case "checkpoint":
				changed.CheckpointCapture = &core.CheckpointCaptureBinding{ID: "chk_0123456789abcdef"}
			case "coordinator":
				changed.CoordinatorRegistrationURL = "https://coordinator.example.test"
			case "adapter":
				changed.RuntimeAdapterRegistrationID = "adapter-fixture"
			case "pending adapter":
				changed.RuntimeAdapterPendingRegistrationID = "adapter-fixture"
			}
			if err := core.ReplaceLeaseClaimIfUnchanged(leaseID, claim, changed); err != nil {
				t.Fatal(err)
			}
			changed, _, err = core.ReadLeaseClaimWithPresence(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "no claim" {
				core.RemoveLeaseClaim(leaseID)
			}
			requests := len(f.paths)
			b.cfg.Daytona.ForgetMissing = true
			err = b.Stop(t.Context(), StopRequest{ID: leaseID})
			after, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID)
			if err == nil || readErr != nil || len(f.paths) != requests || exists != (scenario != "no claim") || exists && !reflect.DeepEqual(changed, after) {
				t.Fatalf("unsafe local cleanup: err=%v read=%v exists=%t", err, readErr, exists)
			}
		})
	}
}

func TestDaytonaForgetMissingPreservesFixedReplayProtection(t *testing.T) {
	for _, released := range []bool{false, true} {
		t.Run(fmt.Sprintf("released=%t", released), func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			if _, err := b.Acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if released {
				if err := b.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID}); err != nil {
					t.Fatal(err)
				}
			}
			before, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			if err != nil || !exists {
				t.Fatalf("missing fixed claim: %v", err)
			}
			requests := len(f.paths)
			b.cfg.Daytona.ForgetMissing = true
			err = b.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID})
			after, exists, readErr := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			if err == nil || !exists || readErr != nil || !reflect.DeepEqual(before, after) || len(f.paths) != requests {
				t.Fatalf("fixed claim changed or provider contacted: err=%v read=%v", err, readErr)
			}
		})
	}
}
