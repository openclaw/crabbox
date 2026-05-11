package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestProxmoxDescriptionRoundTrip(t *testing.T) {
	labels := map[string]string{
		"crabbox": "true",
		"lease":   "cbx_123456abcdef",
		"slug":    "blue-crab",
	}
	got := proxmoxDescriptionLabels(proxmoxDescription(labels))
	if got["crabbox"] != "true" || got["lease"] != "cbx_123456abcdef" || got["slug"] != "blue-crab" {
		t.Fatalf("labels=%#v", got)
	}
}

func TestNewProxmoxClientStripsAPIPathAndUsesTokenAuth(t *testing.T) {
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/api2/json/cluster/nextid" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": "123"})
	}))
	defer server.Close()

	cfg := baseConfig()
	cfg.Proxmox.APIURL = server.URL + "/api2/json"
	cfg.Proxmox.TokenID = "runner@pve!crabbox"
	cfg.Proxmox.TokenSecret = "secret"
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.TemplateID = 9000
	client, err := NewProxmoxClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	id, err := client.nextID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id != 123 {
		t.Fatalf("id=%d", id)
	}
	if auth != "PVEAPIToken=runner@pve!crabbox=secret" {
		t.Fatalf("auth=%q", auth)
	}
}

func TestProxmoxCreateServerFlow(t *testing.T) {
	var forms []url.Values
	var events []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "PVEAPIToken=runner@pve!crabbox=secret" {
			t.Fatalf("auth=%q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/cluster/nextid":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": 101})
		case r.Method == http.MethodPost && r.URL.Path == "/api2/json/nodes/pve1/qemu/9000/clone":
			forms = append(forms, readForm(t, r))
			events = append(events, "clone")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": "UPID:pve1:clone"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api2/json/nodes/pve1/tasks/"):
			switch {
			case strings.Contains(r.URL.Path, "clone"):
				events = append(events, "wait-clone")
			case strings.Contains(r.URL.Path, "config"):
				events = append(events, "wait-config")
			case strings.Contains(r.URL.Path, "start"):
				events = append(events, "wait-start")
			default:
				t.Fatalf("unexpected task path %s", r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"status": "stopped", "exitstatus": "OK"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api2/json/nodes/pve1/qemu/101/config":
			forms = append(forms, readForm(t, r))
			events = append(events, "config")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": "UPID:pve1:config"})
		case r.Method == http.MethodPost && r.URL.Path == "/api2/json/nodes/pve1/qemu/101/status/start":
			events = append(events, "start")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": "UPID:pve1:start"})
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/qemu/101/agent/network-get-interfaces":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"result": []any{
				map[string]any{"name": "lo", "ip-addresses": []any{map[string]any{"ip-address-type": "ipv4", "ip-address": "127.0.0.1"}}},
				map[string]any{"name": "eth0", "ip-addresses": []any{map[string]any{"ip-address-type": "ipv4", "ip-address": "192.0.2.44"}}},
			}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api2/json/nodes/pve1/qemu/101/agent/exec":
			forms = append(forms, readForm(t, r))
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"pid": 77}})
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/qemu/101/agent/exec-status":
			if r.URL.Query().Get("pid") != "77" {
				t.Fatalf("pid=%s", r.URL.Query().Get("pid"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"exited": true, "exitcode": 0}})
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/qemu/101/status/current":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"vmid": 101, "name": "crabbox-blue-crab-12345678", "status": "running"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/qemu/101/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"description": "crabbox labels\ncrabbox=true\nprovider=proxmox\nlease=cbx_123456abcdef\nslug=blue-crab\nserver_type=template-9000\n"}})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	cfg := baseConfig()
	cfg.Provider = "proxmox"
	cfg.Proxmox.APIURL = server.URL
	cfg.Proxmox.TokenID = "runner@pve!crabbox"
	cfg.Proxmox.TokenSecret = "secret"
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.TemplateID = 9000
	cfg.Proxmox.Storage = "local-lvm"
	cfg.Proxmox.Pool = "ci"
	cfg.Proxmox.Bridge = "vmbr1"
	cfg.ServerType = proxmoxServerTypeForConfig(cfg)
	client, err := NewProxmoxClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := client.CreateServer(ctx, cfg, "ssh-ed25519 AAAA test", "cbx_123456abcdef", "blue-crab", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.CloudID != "101" || got.PublicNet.IPv4.IP != "192.0.2.44" || got.Labels["lease"] != "cbx_123456abcdef" {
		t.Fatalf("server=%#v", got)
	}
	if forms[0].Get("newid") != "101" || forms[0].Get("storage") != "local-lvm" || forms[0].Get("pool") != "ci" {
		t.Fatalf("clone form=%v", forms[0])
	}
	if forms[1].Get("ciuser") != "crabbox" || !strings.Contains(forms[1].Get("sshkeys"), "ssh-ed25519") || forms[1].Get("net0") != "virtio,bridge=vmbr1" {
		t.Fatalf("config form=%v", forms[1])
	}
	if got := forms[2]["command"]; !reflect.DeepEqual(got, []string{"/bin/bash", "-s"}) {
		t.Fatalf("exec command=%v want [/bin/bash -s]", got)
	}
	if !strings.Contains(forms[2].Get("input-data"), "crabbox-ready") {
		t.Fatalf("exec form=%v", forms[2])
	}
	wantEvents := []string{"clone", "wait-clone", "config", "wait-config", "start", "wait-start"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events=%v want %v", events, wantEvents)
	}
}

func TestProxmoxCreateServerCleansUpCloneOnConfigFailure(t *testing.T) {
	stopped := false
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/cluster/nextid":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": 101})
		case r.Method == http.MethodPost && r.URL.Path == "/api2/json/nodes/pve1/qemu/9000/clone":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": "UPID:pve1:clone"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api2/json/nodes/pve1/tasks/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"status": "stopped", "exitstatus": "OK"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api2/json/nodes/pve1/qemu/101/config":
			http.Error(w, "permission denied", http.StatusForbidden)
		case r.Method == http.MethodPost && r.URL.Path == "/api2/json/nodes/pve1/qemu/101/status/stop":
			stopped = true
			_ = json.NewEncoder(w).Encode(map[string]any{"data": "UPID:pve1:stop"})
		case r.Method == http.MethodDelete && r.URL.Path == "/api2/json/nodes/pve1/qemu/101":
			if r.URL.Query().Get("purge") != "1" {
				t.Fatalf("purge=%s", r.URL.Query().Get("purge"))
			}
			deleted = true
			_ = json.NewEncoder(w).Encode(map[string]any{"data": "UPID:pve1:delete"})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	cfg := baseConfig()
	cfg.Provider = "proxmox"
	cfg.Proxmox.APIURL = server.URL
	cfg.Proxmox.TokenID = "runner@pve!crabbox"
	cfg.Proxmox.TokenSecret = "secret"
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.TemplateID = 9000
	client, err := NewProxmoxClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.CreateServer(ctx, cfg, "ssh-ed25519 AAAA test", "cbx_123456abcdef", "blue-crab", false); err == nil {
		t.Fatal("expected config failure")
	}
	if !stopped || !deleted {
		t.Fatalf("cleanup stopped=%v deleted=%v", stopped, deleted)
	}
}

func readForm(t *testing.T, r *http.Request) url.Values {
	t.Helper()
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	return r.Form
}
