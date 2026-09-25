package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProxmoxFixedAuthorizationFailureThroughHTTPAPI(t *testing.T) {
	for _, stage := range []string{"inventory", "clone", "task", "identity"} {
		for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
			t.Run(fmt.Sprintf("%s/%d", stage, code), func(t *testing.T) {
				backend, fake, req := fixedProxmoxFixture(t)
				clones, inventories := 0, 0
				api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var data any
					deny := false
					switch r.URL.Path {
					case "/api2/json/access/permissions":
						data = map[string]any{r.URL.Query().Get("path"): map[string]int{"VM.Audit": 1}}
					case "/api2/json/cluster/resources":
						inventories++
						deny = stage == "inventory" && inventories == 2
						data = []any{}
					case "/api2/json/cluster/nextid":
						data = 417
					case "/api2/json/nodes/pve1/qemu/9400/clone":
						clones++
						deny = stage == "clone"
						data = "UPID:fixture"
					case "/api2/json/nodes/pve1/tasks/UPID:fixture/status":
						deny = stage == "task"
						data = map[string]string{"status": "stopped", "exitstatus": "OK"}
					case "/api2/json/nodes/pve1/qemu/417/status/current":
						data = map[string]any{"vmid": 417, "name": "crabbox-fixed-proxmox", "status": "stopped"}
					case "/api2/json/nodes/pve1/qemu/417/config":
						deny = true
					default:
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
						http.Error(w, "unexpected", 500)
						return
					}
					if deny {
						http.Error(w, "Permission check failed (/sdn/zones/localnetwork/vmbr0, SDN.Use)", code)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
				}))
				t.Cleanup(api.Close)
				backend.Cfg.Proxmox.APIURL = api.URL
				newClient = func(cfg core.Config) (proxmoxClient, error) { return core.NewProxmoxClient(cfg) }
				_, err := backend.Acquire(t.Context(), req)
				if err == nil || !strings.Contains(err.Error(), "SDN.Use") {
					t.Fatalf("missing permission diagnostic: %v", err)
				}
				claim, exists, readErr := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if stage == "inventory" || stage == "clone" {
					if exists {
						t.Fatalf("definite pre-allocation rejection retained claim: %+v", claim)
					}
					newClient = func(core.Config) (proxmoxClient, error) { return fake, nil }
					if _, err := backend.Acquire(t.Context(), req); err != nil {
						t.Fatalf("rejected fixed ID cannot be retried: %v", err)
					}
				} else {
					if !exists || claim.CloudID != "417" || claim.FixedCreateIntent.State != "prepared" ||
						!strings.Contains(err.Error(), "claim retained") || !strings.Contains(err.Error(), "inspect") {
						t.Fatalf("post-allocation failure lost custody or recovery advice: claim=%+v err=%v", claim, err)
					}
					if _, err := backend.Acquire(t.Context(), req); err == nil || clones != 1 {
						t.Fatalf("ambiguous clone retried: clones=%d err=%v", clones, err)
					}
				}
			})
		}
	}
}

func TestProxmoxFixedStopForceCommand(t *testing.T) {
	backend, client, req := fixedProxmoxFixture(t)
	t.Chdir(req.Repo.Root)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_PROVIDER", "proxmox")
	client.fixedCreateErr = fmt.Errorf("clone response lost")
	if _, err := backend.Acquire(t.Context(), req); err == nil {
		t.Fatal("expected clone failure")
	}
	var output strings.Builder
	app := core.App{Stdout: io.Discard, Stderr: &output}
	args := []string{"stop", "--provider", "proxmox", "--id", req.RequestedLeaseID,
		"--proxmox-api-url", backend.Cfg.Proxmox.APIURL, "--proxmox-node", backend.Cfg.Proxmox.Node}
	if err := app.Run(t.Context(), args); err == nil {
		t.Fatal("ordinary stop settled an uncertain clone")
	}
	if err := app.Run(t.Context(), append(args, "--force")); err != nil {
		t.Fatalf("stop --force: %v", err)
	}
	if !strings.Contains(output.String(), "prepared claim settled (VM absent)") || readFixedProxmoxClaim(t, req.RequestedLeaseID).FixedCreateIntent.State != "released" {
		t.Fatalf("missing terminal recovery result: %s", output.String())
	}
	if err := app.Run(t.Context(), append(args, "--force")); err != nil {
		t.Fatalf("terminal force retry: %v", err)
	}
	if _, err := backend.Acquire(t.Context(), req); err == nil {
		t.Fatal("recovered fixed ID was reopened")
	}
	client.fixedCreateErr = nil
	req.RequestedLeaseID, req.RequestedSlug = "cbx_aaaaaaaaaaaa", "replacement"
	if lease, err := backend.Acquire(t.Context(), req); err != nil || lease.Server.CloudID != "417" {
		t.Fatalf("recovery still blocks VMID reuse: lease=%+v err=%v", lease, err)
	}
}

func TestProxmoxFixedForceRecoveryPreparedClaim(t *testing.T) {
	for _, scenario := range []string{"absent", "legacy", "inventory error", "exact error", "VMID present", "name present", "lease present", "active clone", "bound", "scope"} {
		t.Run(scenario, func(t *testing.T) {
			backend, client, req := fixedProxmoxFixture(t)
			client.fixedCreateErr = fmt.Errorf("clone response lost")
			if _, err := backend.Acquire(t.Context(), req); err == nil {
				t.Fatal("expected clone failure")
			}
			before := readFixedProxmoxClaim(t, req.RequestedLeaseID)
			switch scenario {
			case "legacy":
				legacy := core.CloneLeaseClaim(before)
				legacy.FixedCreateIntent.Journal = nil
				var err error
				before, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(before.LeaseID, before, legacy)
				if err != nil {
					t.Fatal(err)
				}
			case "inventory error":
				client.clusterListErr = fmt.Errorf("inventory unavailable")
			case "exact error":
				client.clusterExistsErr = fmt.Errorf("inventory forbidden")
			case "VMID present":
				client.clusterExistsByID = map[string]bool{"417": true}
			case "lease present":
				client.servers = []core.Server{{CloudID: "418", Labels: map[string]string{"lease": req.RequestedLeaseID}}}
			case "name present":
				client.servers = []core.Server{{CloudID: "418", Name: core.LeaseProviderName(before.LeaseID, before.Slug)}}
			case "active clone":
				client.activeCloneErr = fmt.Errorf("clone task still active")
			case "bound":
				bound := core.CloneLeaseClaim(before)
				bound.CloudImmutableID = fixedTestGeneration
				var err error
				before, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(before.LeaseID, before, bound)
				if err != nil {
					t.Fatal(err)
				}
			case "scope":
				backend.Cfg.Proxmox.Node = "another-node"
			}
			recovery, ok := any(backend).(core.StopReclaimBackend)
			if !ok {
				t.Fatal("Proxmox has no stop --force recovery")
			}
			err := recovery.ReclaimAndStop(context.Background(), core.StopRequest{ID: req.RequestedLeaseID})
			after := readFixedProxmoxClaim(t, req.RequestedLeaseID)
			if scenario == "absent" || scenario == "legacy" {
				if err != nil || after.FixedCreateIntent.State != "released" {
					t.Fatalf("force recovery failed: %v, claim=%+v", err, after)
				}
			} else if err == nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("unsafe recovery changed custody: %v", err)
			}
			if client.deleteCalls != 0 || client.fixedCreates != 1 {
				t.Fatal("recovery mutated provider resources")
			}
		})
	}
}

func TestProxmoxFixedLegacyPreparedRecoveryThroughHTTPAPI(t *testing.T) {
	for _, scenario := range []string{"absent", "VMID present", "clone name present", "inventory error", "exact inventory error", "filtered", "active clone"} {
		t.Run(scenario, func(t *testing.T) {
			backend, _, req := fixedProxmoxFixture(t)
			// The saved v1 format has no journal or rejection-evidence fields.
			data, err := os.ReadFile("../../cli/testdata/fixed-lease-v1/proxmox.json")
			if err != nil {
				t.Fatal(err)
			}
			var legacy core.LeaseClaim
			if err := json.Unmarshal(data, &legacy); err != nil {
				t.Fatal(err)
			}
			legacy.CloudImmutableID, legacy.SSHHost = "", ""
			legacy.FixedCreateIntent.State = "prepared"
			delete(legacy.Labels, "state")
			inventoryReads, exactReads, mutations := 0, 0, 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutations++
					http.Error(w, "unexpected mutation", 500)
					return
				}
				var data any
				switch r.URL.Path {
				case "/api2/json/access/permissions":
					path := r.URL.Query().Get("path")
					grants := map[string]int{"VM.Audit": 1, "Sys.Audit": 1}
					if scenario == "filtered" {
						grants["VM.Audit"] = 0
					}
					if path == "/vms/417" {
						exactReads++
					}
					data = map[string]any{path: grants}
				case "/api2/json/nodes/pve1/tasks":
					data = []any{}
					if scenario == "active clone" {
						data = []any{map[string]string{"type": "qmclone", "upid": "UPID:fixture"}}
					}
				case "/api2/json/cluster/resources":
					inventoryReads++
					if scenario == "inventory error" || scenario == "exact inventory error" && exactReads != 0 {
						http.Error(w, "inventory unavailable", http.StatusServiceUnavailable)
						return
					}
					data = []any{}
					if scenario == "VMID present" {
						data = []any{map[string]any{"vmid": 417, "name": "unrelated", "node": "pve2", "type": "lxc"}}
					}
					if scenario == "clone name present" {
						data = []any{map[string]any{"vmid": 418, "name": core.LeaseProviderName(legacy.LeaseID, legacy.Slug), "node": "pve2", "type": "lxc"}}
					}
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.Error(w, "unexpected read", 500)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))
			t.Cleanup(api.Close)
			backend.Cfg.Proxmox.APIURL = api.URL
			legacy.ProviderScope = core.ProviderClaimScope("proxmox", backend.Cfg)
			legacy.FixedCreateIntent.ProviderScope = legacy.ProviderScope
			if err := core.WithDurableLeaseClaimLock(req.RequestedLeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
				*claim = legacy
				return persist()
			}); err != nil {
				t.Fatal(err)
			}
			before := readFixedProxmoxClaim(t, legacy.LeaseID)
			newClient = func(cfg core.Config) (proxmoxClient, error) { return core.NewProxmoxClient(cfg) }
			err = backend.ReclaimAndStop(t.Context(), core.StopRequest{ID: legacy.LeaseID})
			after := readFixedProxmoxClaim(t, legacy.LeaseID)
			if scenario == "absent" {
				if err != nil || after.FixedCreateIntent.State != "released" || exactReads == 0 || inventoryReads < 3 {
					t.Fatalf("legacy absence recovery: err=%v state=%s exact=%d inventories=%d", err, after.FixedCreateIntent.State, exactReads, inventoryReads)
				}
			} else if err == nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("unsafe legacy recovery: err=%v", err)
			}
			if mutations != 0 {
				t.Fatal("recovery mutated provider resources")
			}
		})
	}
}
