package proxmox

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (c *fixedProxmoxClient) SetLabelsOnNode(ctx context.Context, node, id string, labels map[string]string) error {
	for i := range c.servers {
		if c.servers[i].CloudID == id {
			c.servers[i].Labels = maps.Clone(labels)
		}
	}
	return c.fakeProxmoxDoctorClient.SetLabelsOnNode(ctx, node, id, labels)
}

func TestProxmoxFixedReplayAndConflictThroughHTTPAPI(t *testing.T) {
	for _, scenario := range []string{"replay", "generation", "fingerprint", "missing source node", "multiple"} {
		t.Run(scenario, func(t *testing.T) {
			backend, fake, req := fixedProxmoxFixture(t)
			var remote core.Server
			var mutations int
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var data any
				switch {
				case r.URL.Path == "/api2/json/access/permissions":
					data = map[string]any{r.URL.Query().Get("path"): map[string]int{"VM.Audit": 1}}
				case r.URL.Path == "/api2/json/cluster/resources":
					entries := []map[string]any{{"vmid": 417, "node": "pve1", "type": "qemu", "template": 0}}
					if scenario == "multiple" {
						entries = append(entries, map[string]any{"vmid": 418, "node": "pve1", "type": "qemu", "template": 0})
					}
					data = entries
				case strings.HasSuffix(r.URL.Path, "/status/current"):
					id := 417
					if strings.Contains(r.URL.Path, "/418/") {
						id = 418
					}
					data = map[string]any{"vmid": id, "name": remote.Name, "status": "running"}
				case strings.HasSuffix(r.URL.Path, "/config") && r.Method == http.MethodGet:
					description := "crabbox labels\n"
					for key, value := range remote.Labels {
						description += key + "=" + value + "\n"
					}
					data = map[string]any{"name": remote.Name, "description": description, "vmgenid": remote.ImmutableID}
				case strings.HasSuffix(r.URL.Path, "/agent/network-get-interfaces"):
					data = map[string]any{"result": []any{map[string]any{"name": "eth0", "ip-addresses": []any{map[string]any{"ip-address": "192.0.2.17", "ip-address-type": "ipv4"}}}}}
				case strings.HasSuffix(r.URL.Path, "/config") && r.Method == http.MethodPost:
					mutations++
					data = nil
				default:
					mutations++
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", 500)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))
			defer api.Close()
			backend.Cfg.Proxmox.APIURL = api.URL
			first, err := backend.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			remote = first.Server
			remote.Labels = maps.Clone(remote.Labels)
			switch scenario {
			case "generation":
				remote.ImmutableID = replacementGeneration
			case "fingerprint":
				remote.Labels["fixed_intent_sha256"] = "different"
			case "missing source node":
				delete(remote.Labels, "node")
			}
			newClient = func(cfg core.Config) (proxmoxClient, error) { return core.NewProxmoxClient(cfg) }
			replay, err := backend.Acquire(context.Background(), req)
			if scenario == "replay" {
				if err != nil || replay.Server.CloudID != first.Server.CloudID || replay.Server.ImmutableID != first.Server.ImmutableID {
					t.Fatalf("replay=%+v err=%v", replay, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") || mutations != 0 {
				t.Fatalf("scenario=%s err=%v mutations=%d", scenario, err, mutations)
			}
			if fake.fixedCreates != 1 {
				t.Fatal("replay cloned a second VM")
			}
		})
	}
}

func TestProxmoxFixedReleaseRejectsGenerationAndDuplicateClaims(t *testing.T) {
	for _, scenario := range []string{"generation", "duplicate claim", "missing claim"} {
		t.Run(scenario, func(t *testing.T) {
			backend, client, req := fixedProxmoxFixture(t)
			lease, err := backend.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			before := readFixedProxmoxClaim(t, req.RequestedLeaseID)
			switch scenario {
			case "generation":
				client.servers[0].ImmutableID = replacementGeneration
			case "duplicate claim":
				err = core.WithDurableLeaseClaimLock("cbx_aaaaaaaaaaaa", func(claim *core.LeaseClaim, _ bool, persist func() error) error {
					*claim = before
					claim.LeaseID = "cbx_aaaaaaaaaaaa"
					return persist()
				})
			case "missing claim":
				err = core.RemoveLeaseClaimIfUnchanged(req.RequestedLeaseID, before)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
				t.Fatal("unsafe release accepted")
			}
			if client.deleteCalls != 0 {
				t.Fatal("unsafe release deleted VM")
			}
			if scenario != "missing claim" && !reflect.DeepEqual(before, readFixedProxmoxClaim(t, req.RequestedLeaseID)) {
				t.Fatal("failed release changed claim")
			}
		})
	}
}

func TestProxmoxFixedAcquiredAbsenceLeavesTombstone(t *testing.T) {
	backend, client, req := fixedProxmoxFixture(t)
	lease, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	client.servers = nil
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	claim := readFixedProxmoxClaim(t, req.RequestedLeaseID)
	if claim.FixedCreateIntent.State != "released" || claim.CloudImmutableID != fixedTestGeneration {
		t.Fatalf("claim=%+v", claim)
	}
	if _, err := backend.Acquire(context.Background(), req); err == nil {
		t.Fatal("terminal ID was reused")
	}
}

func TestProxmoxFixedUnreadyReplayDoesNotSkipBootstrap(t *testing.T) {
	backend, client, req := fixedProxmoxFixture(t)
	if _, err := backend.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	client.servers[0].Labels["state"] = "booting"
	if _, err := backend.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("incomplete bootstrap adopted: %v", err)
	}
}
