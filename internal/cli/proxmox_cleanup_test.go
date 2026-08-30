package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxmoxCheckedDeleteReadsGenerationBeforeEachMutation(t *testing.T) {
	const generation = "8be39656-32b0-4c47-b68c-0a9e1d3ef901"
	for _, variant := range []string{"success", "changed before stop", "changed after stop", "config read failure", "stop failure"} {
		t.Run(variant, func(t *testing.T) {
			stops, deletes, configs := 0, 0, 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var data any
				switch r.URL.Path {
				case "/api2/json/nodes/pve2/qemu/101/status/current":
					data = map[string]any{"vmid": 101, "name": "crabbox-owned", "status": "stopped"}
				case "/api2/json/nodes/pve2/qemu/101/config":
					configs++
					if variant == "config read failure" {
						http.Error(w, "config unavailable", 503)
						return
					}
					id := generation
					if variant == "changed before stop" || variant == "changed after stop" && stops > 0 {
						id = "b71e0631-55ba-4cf7-9013-8fb79484b00c"
					}
					data = map[string]any{"vmgenid": id, "description": "crabbox labels\nlease=cbx_owned\n"}
				case "/api2/json/nodes/pve2/qemu/101/agent/network-get-interfaces":
					http.Error(w, "no agent", 404)
					return
				case "/api2/json/nodes/pve2/qemu/101/status/stop":
					stops++
					if r.Method != http.MethodPost {
						t.Errorf("stop method=%s", r.Method)
					}
					if variant == "stop failure" {
						http.Error(w, "stop rejected", 403)
						return
					}
					data = "UPID:pve2:stop"
				case "/api2/json/nodes/pve2/qemu/101":
					deletes++
					if r.Method != http.MethodDelete || r.URL.Query().Get("purge") != "1" || configs != 2 {
						t.Errorf("purge method=%s query=%s config reads=%d", r.Method, r.URL.RawQuery, configs)
					}
					data = "UPID:pve2:delete"
				case "/api2/json/nodes/pve2/tasks/UPID:pve2:stop/status", "/api2/json/nodes/pve2/tasks/UPID:pve2:delete/status":
					data = map[string]any{"status": "stopped", "exitstatus": "OK"}
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", 400)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))
			defer api.Close()
			client := testProxmoxClient(t, api.URL)
			err := client.DeleteServerOnNodeChecked(context.Background(), "pve2", "101", func(server Server) error {
				if server.CloudID != "101" || server.HostID != "pve2" || server.ImmutableID != generation || server.Labels["lease"] != "cbx_owned" {
					return errors.New("changed VM ownership")
				}
				return nil
			})
			if variant == "success" {
				if err != nil || stops != 1 || deletes != 1 {
					t.Fatalf("err=%v stop=%d delete=%d", err, stops, deletes)
				}
			} else if err == nil || deletes != 0 {
				t.Fatalf("err=%v deletes=%d, want rejection before purge", err, deletes)
			}
			if (variant == "changed before stop" || variant == "config read failure") && stops != 0 {
				t.Fatal("stop called before identity validation")
			}
		})
	}
}

func TestProxmoxGenerationIdentityRequiresNonzeroUUID(t *testing.T) {
	const id = "8be39656-32b0-4c47-b68c-0a9e1d3ef901"
	for _, tc := range []struct{ value, want string }{
		{id, id}, {strings.ToUpper(id), id}, {"", ""}, {"0", ""}, {"1", ""},
		{"00000000-0000-0000-0000-000000000000", ""}, {" " + id, ""},
		{"8be39656_32b0-4c47-b68c-0a9e1d3ef901", ""}, {"zbe39656-32b0-4c47-b68c-0a9e1d3ef901", ""},
	} {
		t.Run(tc.value, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var data any
				switch {
				case strings.HasSuffix(r.URL.Path, "/status/current"):
					data = map[string]any{"vmid": 101, "name": "crabbox-owned"}
				case strings.HasSuffix(r.URL.Path, "/config"):
					data = map[string]any{"vmgenid": tc.value}
				default:
					http.Error(w, "no agent", 404)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))
			defer api.Close()
			server, err := testProxmoxClient(t, api.URL).GetServerOnNode(context.Background(), "pve1", "101")
			if err != nil || server.ImmutableID != tc.want {
				t.Fatalf("identity=%q err=%v, want %q", server.ImmutableID, err, tc.want)
			}
		})
	}
}
