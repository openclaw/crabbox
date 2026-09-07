package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
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

func TestNewProxmoxClientRejectsUnsupportedDefaultTransportForInsecureTLS(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	recorder := &recordingDefaultRoundTripper{}
	http.DefaultTransport = recorder
	cfg := baseConfig()
	cfg.Proxmox.APIURL = "https://proxmox.example.test"
	cfg.Proxmox.TokenID = "runner@pve!crabbox"
	cfg.Proxmox.TokenSecret = "secret"
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.InsecureTLS = true

	client, err := NewProxmoxClient(cfg)
	if client != nil || err == nil || !strings.Contains(err.Error(), "non-nil *http.Transport") {
		t.Fatalf("client=%#v err=%v, want transport setup error", client, err)
	}
	if recorder.calls != 0 {
		t.Fatalf("custom default invoked %d times, want 0", recorder.calls)
	}
}

func TestNewProxmoxClientAllowsUnsupportedDefaultTransportWithoutTLSMutation(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = &recordingDefaultRoundTripper{}
	cfg := baseConfig()
	cfg.Proxmox.APIURL = "https://proxmox.example.test"
	cfg.Proxmox.TokenID = "runner@pve!crabbox"
	cfg.Proxmox.TokenSecret = "secret"
	cfg.Proxmox.Node = "pve1"

	client, err := NewProxmoxClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if client.Client.Transport != nil {
		t.Fatalf("transport=%T, want nil client transport", client.Client.Transport)
	}
}

func TestProxmoxClientRedactsCredentialsFromHTTPErrorBody(t *testing.T) {
	const (
		tokenID     = "runner@pve!crabbox"
		tokenSecret = "super-secret-token"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"authorization": authorization,
			"message":       "permission denied for " + tokenID,
			"token_secret":  tokenSecret,
		})
	}))
	defer server.Close()

	cfg := baseConfig()
	cfg.Proxmox.APIURL = server.URL
	cfg.Proxmox.TokenID = tokenID
	cfg.Proxmox.TokenSecret = tokenSecret
	cfg.Proxmox.Node = "pve1"
	client, err := NewProxmoxClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.nextID(context.Background())
	if err == nil {
		t.Fatal("nextID succeeded, want HTTP error")
	}
	text := err.Error()
	for _, secret := range []string{tokenID, tokenSecret, "PVEAPIToken=" + tokenID + "=" + tokenSecret} {
		if strings.Contains(text, secret) {
			t.Fatalf("Proxmox HTTP error leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "permission denied for") || !strings.Contains(text, "redacted") {
		t.Fatalf("Proxmox HTTP error lost useful redacted context: %s", text)
	}
	var apiErr *ProxmoxError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type=%T, want *ProxmoxError", err)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(apiErr.Body), &payload); err != nil {
		t.Fatalf("redacted error body is not valid JSON: %v: %s", err, apiErr.Body)
	}
	if payload["authorization"] != "PVEAPIToken=<redacted>" || payload["message"] != "permission denied for <redacted>" || payload["token_secret"] != "<redacted>" {
		t.Fatalf("redacted error payload=%#v", payload)
	}
}

func TestProxmoxDoctorReadinessChecksNonMutatingPrerequisites(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Method != http.MethodGet {
			t.Fatalf("doctor readiness used mutating request: %s %s", r.Method, r.URL.String())
		}
		switch r.URL.Path {
		case "/api2/json/version":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"version": "8.2.0"}})
		case "/api2/json/nodes/pve1/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"status": "online"}})
		case "/api2/json/nodes/pve1/storage":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"storage": "local-lvm", "active": 1, "enabled": 1, "content": "images,rootdir"}}})
		case "/api2/json/nodes/pve1/network":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"iface": "vmbr0", "type": "bridge", "active": 1}}})
		case "/api2/json/nodes/pve1/qemu":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"vmid": 9400, "name": "crabbox-template", "status": "stopped", "template": 1},
				map[string]any{"vmid": 101, "name": "crabbox-blue-abcdef12", "status": "running", "template": 0},
			}})
		case "/api2/json/cluster/resources":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"vmid": 9400, "name": "crabbox-template", "node": "pve1", "type": "qemu", "template": 1},
				map[string]any{"vmid": 101, "name": "crabbox-blue-abcdef12", "node": "pve2", "type": "qemu", "template": 0},
			}})
		case "/api2/json/nodes/pve1/qemu/9400/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"name": "crabbox-template", "ide2": "local-lvm:cloudinit"}})
		case "/api2/json/access/permissions":
			permissionPath := r.URL.Query().Get("path")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{permissionPath: map[string]any{"VM.Audit": 1}}})
		case "/api2/json/cluster/nextid":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": "102"})
		default:
			t.Fatalf("unexpected readiness request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	cfg := baseConfig()
	cfg.Proxmox.APIURL = server.URL
	cfg.Proxmox.TokenID = "runner@pve!crabbox"
	cfg.Proxmox.TokenSecret = "secret-value"
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.TemplateID = 9400
	cfg.Proxmox.Storage = "local-lvm"
	cfg.Proxmox.Bridge = "vmbr0"
	client, err := NewProxmoxClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := client.DoctorReadiness(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	byName := proxmoxChecksByName(checks)
	for _, name := range []string{"auth", "node", "storage", "bridge", "template", "nextid", "inventory", "mutation"} {
		check, ok := byName[name]
		if !ok {
			t.Fatalf("missing check %q in %#v", name, checks)
		}
		if check.Status != "ok" {
			t.Fatalf("check %s status=%s message=%s", name, check.Status, check.Message)
		}
	}
	if byName["inventory"].Details["leases"] != "1" || byName["inventory"].Details["mutation"] != "false" {
		t.Fatalf("inventory details=%v", byName["inventory"].Details)
	}
	for _, call := range calls {
		if strings.Contains(call, "/clone") || strings.Contains(call, "/status/start") || strings.Contains(call, "/status/stop") || strings.Contains(call, "DELETE ") {
			t.Fatalf("doctor readiness called mutation endpoint: %v", calls)
		}
	}
}

func TestProxmoxDoctorReadinessClassifiesPermissionGaps(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/version":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"version": "8.2.0"}})
		case "/api2/json/nodes/pve1/status":
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": "permission check failed (/nodes/pve1, Sys.Audit)"})
		case "/api2/json/nodes/pve1/storage":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"storage": "local-lvm", "active": 1, "enabled": 1}}})
		case "/api2/json/nodes/pve1/network":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"iface": "vmbr0", "type": "bridge", "active": 1}}})
		case "/api2/json/nodes/pve1/qemu":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"vmid": 9400, "name": "tmpl", "template": 1}}})
		case "/api2/json/cluster/resources":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		case "/api2/json/nodes/pve1/qemu/9400/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"name": "tmpl"}})
		case "/api2/json/access/permissions":
			permissionPath := r.URL.Query().Get("path")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{permissionPath: map[string]any{"VM.Audit": 1}}})
		case "/api2/json/cluster/nextid":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": 102})
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
		}
	}))
	defer server.Close()

	cfg := baseConfig()
	cfg.Proxmox.APIURL = server.URL
	cfg.Proxmox.TokenID = "runner@pve!crabbox"
	cfg.Proxmox.TokenSecret = "super-secret-token"
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.TemplateID = 9400
	cfg.Proxmox.Storage = "local-lvm"
	cfg.Proxmox.Bridge = "vmbr0"
	client, err := NewProxmoxClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := client.DoctorReadiness(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	node := proxmoxChecksByName(checks)["node"]
	if node.Status != "failed" || node.Details["class"] != "permission" || node.Details["hint"] != "grant_proxmox_node_audit" {
		t.Fatalf("node check=%#v", node)
	}
	if strings.Contains(node.Message, "super-secret-token") || strings.Contains(node.Details["error"], "super-secret-token") {
		t.Fatalf("permission output leaked token secret: %#v", node)
	}
}

func TestProxmoxSafeErrorRedactsFullAPITokenHeader(t *testing.T) {
	got := proxmoxSafeError(errors.New("proxy echoed Authorization: PVEAPIToken=runner@pve!crabbox=super-secret-token while checking /version"))
	if strings.Contains(got, "runner@pve!crabbox") || strings.Contains(got, "super-secret-token") {
		t.Fatalf("safe error leaked token material: %q", got)
	}
	if !strings.Contains(got, "PVEAPIToken=<redacted>") {
		t.Fatalf("safe error missing redaction marker: %q", got)
	}
}

func TestProxmoxDoctorReadinessRejectsInactiveBridge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/version":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"version": "8.2.0"}})
		case "/api2/json/nodes/pve1/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"status": "online"}})
		case "/api2/json/nodes/pve1/storage":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"storage": "local-lvm", "active": 1, "enabled": 1}}})
		case "/api2/json/nodes/pve1/network":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"iface": "vmbr0", "type": "bridge", "active": 0}}})
		case "/api2/json/nodes/pve1/qemu":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"vmid": 9400, "name": "tmpl", "template": 1}}})
		case "/api2/json/cluster/resources":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		case "/api2/json/nodes/pve1/qemu/9400/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"name": "tmpl"}})
		case "/api2/json/access/permissions":
			permissionPath := r.URL.Query().Get("path")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{permissionPath: map[string]any{"VM.Audit": 1}}})
		case "/api2/json/cluster/nextid":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": 102})
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
		}
	}))
	defer server.Close()

	cfg := baseConfig()
	cfg.Proxmox.APIURL = server.URL
	cfg.Proxmox.TokenID = "runner@pve!crabbox"
	cfg.Proxmox.TokenSecret = "secret"
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.TemplateID = 9400
	cfg.Proxmox.Storage = "local-lvm"
	cfg.Proxmox.Bridge = "vmbr0"
	client, err := NewProxmoxClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := client.DoctorReadiness(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	bridge := proxmoxChecksByName(checks)["bridge"]
	if bridge.Status != "failed" || bridge.Details["active"] != "0" || bridge.Details["hint"] != "activate_proxmox_bridge" {
		t.Fatalf("bridge check=%#v", bridge)
	}
}

func TestProxmoxDoctorReadinessAcceptsOVSBridge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/network" {
			t.Fatalf("unexpected %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"iface": "vmbr0", "type": "OVSBridge", "active": 1},
		}})
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	cfg := baseConfig()
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.Bridge = "vmbr0"
	check := client.proxmoxNetworkCheck(context.Background(), cfg)
	if check.Status != "ok" || check.Details["type"] != "OVSBridge" {
		t.Fatalf("bridge check=%#v", check)
	}
}

func TestProxmoxDoctorReadinessAcceptsSDNVNet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/network" || r.URL.Query().Get("type") != "include_sdn" {
			t.Fatalf("unexpected %s", r.URL.String())
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"iface": "ci-vnet", "type": "vnet", "active": 1},
		}})
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	cfg := baseConfig()
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.Bridge = "ci-vnet"
	check := client.proxmoxNetworkCheck(context.Background(), cfg)
	if check.Status != "ok" || check.Details["type"] != "vnet" {
		t.Fatalf("bridge check=%#v", check)
	}
}

func TestProxmoxDoctorReadinessFallsBackForPVE8NetworkInventory(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/network" {
			t.Fatalf("unexpected %s", r.URL.Path)
		}
		requests = append(requests, r.URL.String())
		switch r.URL.Query().Get("type") {
		case "include_sdn":
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errors": map[string]string{"type": "value 'include_sdn' does not have a value in the enumeration"},
			})
			return
		case "any_bridge":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"iface": "vmbr0", "type": "bridge", "active": 1},
				map[string]any{"iface": "ci-vnet", "type": "vnet", "active": 1},
			}})
		default:
			t.Fatalf("unexpected %s", r.URL.String())
		}
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	cfg := baseConfig()
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.Bridge = "ci-vnet"
	check := client.proxmoxNetworkCheck(context.Background(), cfg)
	if check.Status != "ok" || check.Details["bridge"] != "ci-vnet" || check.Details["type"] != "vnet" {
		t.Fatalf("bridge check=%#v", check)
	}
	want := []string{"/api2/json/nodes/pve1/network?type=include_sdn", "/api2/json/nodes/pve1/network?type=any_bridge"}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests=%v, want %v", requests, want)
	}
}

func TestProxmoxDoctorReadinessRequiresUsableTemplateBridgeWhenUnset(t *testing.T) {
	tests := []struct {
		name       string
		config     map[string]any
		networks   []any
		wantStatus string
		wantBridge string
	}{
		{
			name:       "missing template nic",
			config:     map[string]any{"ide2": "local-lvm:cloudinit"},
			networks:   []any{map[string]any{"iface": "vmbr0", "type": "bridge", "active": 1}},
			wantStatus: "failed",
			wantBridge: "missing",
		},
		{
			name:       "active template bridge",
			config:     map[string]any{"ide2": "local-lvm:cloudinit", "net0": "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0"},
			networks:   []any{map[string]any{"iface": "vmbr0", "type": "bridge", "active": 1}},
			wantStatus: "ok",
			wantBridge: "vmbr0",
		},
		{
			name:   "broken net0 with active net1",
			config: map[string]any{"ide2": "local-lvm:cloudinit", "net0": "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0", "net1": "virtio=AA:BB:CC:DD:EE:00,bridge=vmbr1"},
			networks: []any{
				map[string]any{"iface": "vmbr0", "type": "bridge", "active": 0},
				map[string]any{"iface": "vmbr1", "type": "bridge", "active": 1},
			},
			wantStatus: "failed",
			wantBridge: "vmbr0",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api2/json/nodes/pve1/network":
					_ = json.NewEncoder(w).Encode(map[string]any{"data": tc.networks})
				case "/api2/json/nodes/pve1/qemu/9400/config":
					_ = json.NewEncoder(w).Encode(map[string]any{"data": tc.config})
				default:
					t.Fatalf("unexpected %s", r.URL.Path)
				}
			}))
			defer server.Close()

			cfg := baseConfig()
			cfg.Proxmox.APIURL = server.URL
			cfg.Proxmox.TokenID = "runner@pve!crabbox"
			cfg.Proxmox.TokenSecret = "secret"
			cfg.Proxmox.Node = "pve1"
			cfg.Proxmox.TemplateID = 9400
			cfg.Proxmox.Bridge = ""
			client, err := NewProxmoxClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			check := client.proxmoxNetworkCheck(context.Background(), cfg)
			if check.Status != tc.wantStatus || check.Details["bridge"] != tc.wantBridge {
				t.Fatalf("check=%#v", check)
			}
		})
	}
}

func TestProxmoxDoctorReadinessClassifiesACLHiddenBridgeAsPermission(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/nodes/pve1/network":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		case "/api2/json/nodes/pve1/network/vmbr0":
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": "permission check failed (/nodes/pve1/network/vmbr0, Sys.Audit)"})
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	cfg := baseConfig()
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.Bridge = "vmbr0"
	check := client.proxmoxNetworkCheck(context.Background(), cfg)
	if check.Status != "failed" || check.Details["class"] != "permission" || check.Details["hint"] != "grant_proxmox_network_audit" {
		t.Fatalf("bridge check=%#v", check)
	}
}

func TestProxmoxDoctorReadinessValidatesConfiguredPool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/pools/ci" {
			t.Fatalf("unexpected %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": "pool does not exist"})
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	cfg := baseConfig()
	cfg.Proxmox.Pool = "ci"
	check := client.proxmoxPoolCheck(context.Background(), cfg)
	if check.Status != "failed" || check.Details["class"] != "missing_resource" || check.Details["pool"] != "ci" {
		t.Fatalf("pool check=%#v", check)
	}
}

func TestProxmoxDoctorReadinessRejectsEmptyStorageInventory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/storage" {
			t.Fatalf("unexpected %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	cfg := baseConfig()
	cfg.Proxmox.Node = "pve1"
	check := client.proxmoxStorageCheck(context.Background(), cfg)
	if check.Status != "failed" || check.Details["count"] != "0" || check.Details["usable"] != "0" || check.Details["hint"] != "grant_or_enable_proxmox_storage" {
		t.Fatalf("storage check=%#v", check)
	}
}

func TestProxmoxDoctorReadinessRejectsStorageInventoryWithoutUsableStorage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/storage" {
			t.Fatalf("unexpected %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"storage": "disabled", "active": 1, "enabled": 0},
			map[string]any{"storage": "inactive", "active": 0, "enabled": 1},
		}})
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	cfg := baseConfig()
	cfg.Proxmox.Node = "pve1"
	check := client.proxmoxStorageCheck(context.Background(), cfg)
	if check.Status != "failed" || check.Details["count"] != "2" || check.Details["usable"] != "0" || check.Details["hint"] != "grant_or_enable_proxmox_storage" {
		t.Fatalf("storage check=%#v", check)
	}
}

func TestProxmoxDoctorReadinessRejectsStorageWithoutImagesContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/storage" {
			t.Fatalf("unexpected %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"storage": "archive", "active": 1, "enabled": 1, "content": "iso,backup,vztmpl"},
		}})
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	cfg := baseConfig()
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.Storage = "archive"
	check := client.proxmoxStorageCheck(context.Background(), cfg)
	if check.Status != "failed" || check.Details["hint"] != "enable_proxmox_storage_images" {
		t.Fatalf("storage check=%#v", check)
	}
}

func TestProxmoxDoctorReadinessValidatesTemplateStorageWhenUnset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/nodes/pve1/storage":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"storage": "fast", "active": 1, "enabled": 1, "content": "images"},
				map[string]any{"storage": "slow", "active": 0, "enabled": 1, "content": "images"},
			}})
		case "/api2/json/nodes/pve1/qemu/9400/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"scsi0": "slow:vm-9400-disk-0,size=8G",
				"ide2":  "fast:cloudinit",
			}})
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	cfg := baseConfig()
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.TemplateID = 9400
	cfg.Proxmox.Storage = ""
	check := client.proxmoxStorageCheck(context.Background(), cfg)
	if check.Status != "failed" || check.Details["storage"] != "slow" || check.Details["source"] != "template" || check.Details["hint"] != "enable_proxmox_storage" {
		t.Fatalf("storage check=%#v", check)
	}
}

func TestProxmoxDoctorReadinessValidatesTemplateSourceWithStorageOverride(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/nodes/pve1/storage":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"storage": "target", "active": 1, "enabled": 1, "content": "images"},
				map[string]any{"storage": "source", "active": 0, "enabled": 1, "content": "images"},
			}})
		case "/api2/json/nodes/pve1/qemu/9400/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"scsi0": "source:vm-9400-disk-0,size=8G",
			}})
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	cfg := baseConfig()
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.TemplateID = 9400
	cfg.Proxmox.Storage = "target"
	check := client.proxmoxStorageCheck(context.Background(), cfg)
	if check.Status != "failed" || check.Details["storage"] != "source" || check.Details["source"] != "template" {
		t.Fatalf("storage check=%#v", check)
	}
}

func TestProxmoxDoctorReadinessAcceptsTemplateISOStorage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/nodes/pve1/storage":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"storage": "source", "active": 1, "enabled": 1, "content": "images"},
				map[string]any{"storage": "local", "active": 1, "enabled": 1, "content": "iso,backup"},
				map[string]any{"storage": "local-lvm", "active": 1, "enabled": 1, "content": "images"},
			}})
		case "/api2/json/nodes/pve1/qemu/9400/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"scsi0": "source:vm-9400-disk-0,size=8G",
				"ide0":  "local:iso/installer.iso,media=cdrom",
				"ide2":  "local-lvm:cloudinit,media=cdrom",
			}})
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	cfg := baseConfig()
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.TemplateID = 9400
	cfg.Proxmox.Storage = ""
	check := client.proxmoxStorageCheck(context.Background(), cfg)
	if check.Status != "ok" || check.Details["storage"] != "local,local-lvm,source" {
		t.Fatalf("storage check=%#v", check)
	}
}

func TestProxmoxDoctorReadinessReportsMissingTemplateIDAsCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/version":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"version": "8.2.0"}})
		case "/api2/json/nodes/pve1/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"status": "online"}})
		case "/api2/json/nodes/pve1/storage", "/api2/json/nodes/pve1/network", "/api2/json/nodes/pve1/qemu", "/api2/json/cluster/resources":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		case "/api2/json/access/permissions":
			permissionPath := r.URL.Query().Get("path")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{permissionPath: map[string]any{"VM.Audit": 1}}})
		case "/api2/json/cluster/nextid":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": 102})
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
		}
	}))
	defer server.Close()

	cfg := baseConfig()
	cfg.Proxmox.APIURL = server.URL
	cfg.Proxmox.TokenID = "runner@pve!crabbox"
	cfg.Proxmox.TokenSecret = "secret"
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.TemplateID = 0
	client, err := NewProxmoxClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := client.DoctorReadiness(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	template := proxmoxChecksByName(checks)["template"]
	if template.Status != "failed" || template.Details["class"] != "config" || template.Details["hint"] != "set_proxmox_template_id" {
		t.Fatalf("template check=%#v", template)
	}
}

func TestProxmoxRequiredResponseDataFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "missing data", body: `{}`},
		{name: "null data", body: `{"data":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api2/json/cluster/nextid" {
					t.Fatalf("path=%s", r.URL.Path)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			client := testProxmoxClient(t, server.URL)
			if _, err := client.nextID(context.Background()); err == nil || !strings.Contains(err.Error(), "missing required data") {
				t.Fatalf("err=%v, want missing required data", err)
			}
		})
	}
}

func TestProxmoxDoctorReadinessRequiresResponseData(t *testing.T) {
	for _, tc := range []struct {
		name  string
		path  string
		check func(context.Context, *ProxmoxClient, Config) ProxmoxReadinessCheck
	}{
		{
			name: "auth",
			path: "/api2/json/version",
			check: func(ctx context.Context, client *ProxmoxClient, _ Config) ProxmoxReadinessCheck {
				return client.proxmoxAuthCheck(ctx)
			},
		},
		{
			name: "node",
			path: "/api2/json/nodes/pve1/status",
			check: func(ctx context.Context, client *ProxmoxClient, cfg Config) ProxmoxReadinessCheck {
				return client.proxmoxNodeCheck(ctx, cfg)
			},
		},
		{
			name: "storage",
			path: "/api2/json/nodes/pve1/storage",
			check: func(ctx context.Context, client *ProxmoxClient, cfg Config) ProxmoxReadinessCheck {
				return client.proxmoxStorageCheck(ctx, cfg)
			},
		},
		{
			name: "bridge",
			path: "/api2/json/nodes/pve1/network",
			check: func(ctx context.Context, client *ProxmoxClient, cfg Config) ProxmoxReadinessCheck {
				return client.proxmoxNetworkCheck(ctx, cfg)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path {
					t.Fatalf("path=%s, want %s", r.URL.Path, tc.path)
				}
				_, _ = w.Write([]byte(`{"data":null}`))
			}))
			defer server.Close()

			client := testProxmoxClient(t, server.URL)
			cfg := baseConfig()
			cfg.Proxmox.Node = "pve1"
			check := tc.check(context.Background(), client, cfg)
			if check.Status != "failed" || !strings.Contains(check.Details["error"], "missing_required_data") {
				t.Fatalf("check=%#v, want missing required data failure", check)
			}
		})
	}
}

func TestProxmoxTemplateReadinessRequiresConfigData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/nodes/pve1/qemu":
			_, _ = w.Write([]byte(`{"data":[{"vmid":9000,"name":"template","template":1}]}`))
		case "/api2/json/nodes/pve1/qemu/9000/config":
			_, _ = w.Write([]byte(`{"data":null}`))
		default:
			t.Fatalf("unexpected path=%s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	cfg := baseConfig()
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.TemplateID = 9000
	check := client.proxmoxTemplateCheck(context.Background(), cfg)
	if check.Status != "failed" || !strings.Contains(check.Details["error"], "missing_required_data") {
		t.Fatalf("check=%#v, want missing required data failure", check)
	}
}

func TestProxmoxTemplateReadinessRequiresCloudInitDrive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/nodes/pve1/qemu":
			_, _ = w.Write([]byte(`{"data":[{"vmid":9000,"name":"template","template":1}]}`))
		case "/api2/json/nodes/pve1/qemu/9000/config":
			_, _ = w.Write([]byte(`{"data":{"name":"template","scsi0":"local-lvm:vm-9000-disk-0"}}`))
		default:
			t.Fatalf("unexpected path=%s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	cfg := baseConfig()
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.TemplateID = 9000
	check := client.proxmoxTemplateCheck(context.Background(), cfg)
	if check.Status != "failed" || check.Details["hint"] != "attach_proxmox_cloudinit_drive" {
		t.Fatalf("check=%#v, want missing cloud-init failure", check)
	}
}

func TestProxmoxDeleteServerWrapsAmbiguousRequestFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/status/stop"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":"not found"}`))
		case r.Method == http.MethodDelete:
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("response writer does not support hijacking")
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	err := client.DeleteServer(context.Background(), "101")
	if err == nil || !IsProxmoxDeleteRequestError(err) {
		t.Fatalf("err=%v, want ambiguous delete request error", err)
	}
}

func TestProxmoxWaitTaskRequiresOKExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusData map[string]any
		want       string
	}{
		{
			name:       "missing exitstatus",
			statusData: map[string]any{"status": "stopped"},
			want:       "missing exitstatus",
		},
		{
			name:       "failed exitstatus",
			statusData: map[string]any{"status": "stopped", "exitstatus": "ERROR"},
			want:       "UPID:pve1:test failed: ERROR",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api2/json/nodes/pve1/tasks/UPID:pve1:test/status" {
					t.Fatalf("%s %s", r.Method, r.URL.String())
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": tc.statusData})
			}))
			defer server.Close()

			client := testProxmoxClient(t, server.URL)
			err := client.waitTask(context.Background(), "UPID:pve1:test")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
			var waitErr *proxmoxTaskWaitError
			if errors.As(err, &waitErr) {
				t.Fatalf("terminal task failure classified as ambiguous: %v", err)
			}
		})
	}
}

func TestProxmoxWaitTaskClassifiesStatusReadFailureAsAmbiguous(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	err := client.waitTask(context.Background(), "UPID:pve1:test")
	var waitErr *proxmoxTaskWaitError
	if !errors.As(err, &waitErr) {
		t.Fatalf("err=%v, want ambiguous task wait error", err)
	}
}

func TestProxmoxGuestIPv4FiltersNonRoutableInterfaces(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api2/json/nodes/pve1/qemu/101/agent/network-get-interfaces" {
			t.Fatalf("%s %s", r.Method, r.URL.String())
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"result": []any{
			map[string]any{"name": "lo", "ip-addresses": []any{map[string]any{"ip-address-type": "ipv4", "ip-address": "127.0.0.1"}}},
			map[string]any{"name": "docker0", "ip-addresses": []any{map[string]any{"ip-address-type": "ipv4", "ip-address": "172.17.0.1"}}},
			map[string]any{"name": "veth123", "ip-addresses": []any{map[string]any{"ip-address-type": "ipv4", "ip-address": "169.254.1.2"}}},
			map[string]any{"name": "eth0", "ip-addresses": []any{
				map[string]any{"ip-address-type": "ipv6", "ip-address": "2001:db8::1"},
				map[string]any{"ip-address-type": "ipv4", "ip-address": "192.0.2.44"},
			}},
		}}})
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	got, err := client.guestIPv4(context.Background(), 101)
	if err != nil {
		t.Fatal(err)
	}
	if got != "192.0.2.44" {
		t.Fatalf("ip=%q, want 192.0.2.44", got)
	}
}

func TestProxmoxVMExistsInCluster(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("%s %s", r.Method, r.URL.String())
		}
		switch r.URL.Path {
		case "/api2/json/access/permissions":
			permissionPath := r.URL.Query().Get("path")
			if permissionPath != "/vms/101" && permissionPath != "/vms/202" {
				t.Fatalf("permissions query=%s", r.URL.RawQuery)
			}
			propagate := 1
			if permissionPath == "/vms/101" {
				propagate = 0
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{permissionPath: map[string]any{"VM.Audit": propagate}}})
		case "/api2/json/cluster/resources":
			if r.URL.Query().Get("type") != "vm" {
				t.Fatalf("resources query=%s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"vmid": 101, "node": "pve2", "type": "qemu"},
			}})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	if exists, err := client.VMExistsInCluster(context.Background(), "101"); err != nil || !exists {
		t.Fatalf("exists=%t err=%v, want migrated VM found", exists, err)
	}
	if exists, err := client.VMExistsInCluster(context.Background(), "202"); err != nil || exists {
		t.Fatalf("exists=%t err=%v, want VM absent", exists, err)
	}
}

func TestProxmoxListsCrabboxServersAcrossClusterNodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/access/permissions":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"/vms": map[string]any{"VM.Audit": 1}}})
		case "/api2/json/cluster/resources":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"vmid": 101, "node": "pve2", "type": "qemu", "name": "crabbox-cross-node", "template": 0},
			}})
		case "/api2/json/nodes/pve2/qemu/101/status/current":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"vmid": 101, "name": "crabbox-cross-node", "status": "running"}})
		case "/api2/json/nodes/pve2/qemu/101/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"description": proxmoxDescription(map[string]string{"crabbox": "true", "provider": "proxmox", "lease": "cbx_cross_node", "slug": "cross-node"}),
			}})
		case "/api2/json/nodes/pve2/qemu/101/agent/network-get-interfaces":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": "guest agent unavailable"})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	servers, err := client.ListCrabboxServersCluster(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].CloudID != "101" || servers[0].HostID != "pve2" || servers[0].Labels["lease"] != "cbx_cross_node" {
		t.Fatalf("servers=%#v", servers)
	}
}

func TestProxmoxListCrabboxServersClusterRetriesMigratedVM(t *testing.T) {
	resourceCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/access/permissions":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"/vms": map[string]any{"VM.Audit": 1}}})
		case "/api2/json/cluster/resources":
			resourceCalls++
			node := "pve1"
			if resourceCalls > 1 {
				node = "pve2"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"vmid": 101, "node": node, "type": "qemu", "name": "crabbox-migrated", "template": 0},
			}})
		case "/api2/json/nodes/pve1/qemu/101/status/current":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": "not found"})
		case "/api2/json/nodes/pve2/qemu/101/status/current":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"vmid": 101, "name": "crabbox-migrated", "status": "running"}})
		case "/api2/json/nodes/pve2/qemu/101/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"description": proxmoxDescription(map[string]string{"crabbox": "true", "provider": "proxmox", "lease": "cbx_migrated", "slug": "migrated"}),
			}})
		case "/api2/json/nodes/pve2/qemu/101/agent/network-get-interfaces":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": "guest agent unavailable"})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	servers, err := client.ListCrabboxServersCluster(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resourceCalls != 2 || len(servers) != 1 || servers[0].HostID != "pve2" || servers[0].Labels["lease"] != "cbx_migrated" {
		t.Fatalf("resourceCalls=%d servers=%#v", resourceCalls, servers)
	}
}

func TestProxmoxListCrabboxServersClusterFailsClosedDuringMigration(t *testing.T) {
	resourceCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/access/permissions":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"/vms": map[string]any{"VM.Audit": 1}}})
		case "/api2/json/cluster/resources":
			resourceCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"vmid": 101, "node": "pve1", "type": "qemu", "name": "crabbox-migrating", "template": 0},
			}})
		case "/api2/json/nodes/pve1/qemu/101/status/current":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": "not found"})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	if _, err := client.ListCrabboxServersCluster(context.Background()); err == nil || !strings.Contains(err.Error(), "did not stabilize") {
		t.Fatalf("err=%v, want migration reconciliation failure", err)
	}
	if resourceCalls < 2 {
		t.Fatalf("resourceCalls=%d, want refreshed cluster inventory", resourceCalls)
	}
}

func TestProxmoxListCrabboxServersClusterRequiresPropagatedVMAudit(t *testing.T) {
	resourcesCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/access/permissions":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"/vms": map[string]any{"VM.Audit": 0}}})
		case "/api2/json/cluster/resources":
			resourcesCalled = true
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	if _, err := client.ListCrabboxServersCluster(context.Background()); err == nil || !strings.Contains(err.Error(), "propagated VM.Audit") {
		t.Fatalf("err=%v, want authoritative inventory failure", err)
	}
	if resourcesCalled {
		t.Fatal("cluster inventory queried without propagated VM.Audit")
	}
}

func TestProxmoxListCrabboxServersClusterFailsWhenConfigUnreadable(t *testing.T) {
	agentCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/access/permissions":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"/vms": map[string]any{"VM.Audit": 1}}})
		case "/api2/json/cluster/resources":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"vmid": 101, "node": "pve2", "type": "qemu", "name": "crabbox-cross-node", "template": 0},
			}})
		case "/api2/json/nodes/pve2/qemu/101/status/current":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"vmid": 101, "name": "crabbox-cross-node", "status": "running"}})
		case "/api2/json/nodes/pve2/qemu/101/config":
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": "permission denied"})
		case "/api2/json/nodes/pve2/qemu/101/agent/network-get-interfaces":
			agentCalled = true
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	if _, err := client.ListCrabboxServersCluster(context.Background()); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err=%v, want config read failure", err)
	}
	if agentCalled {
		t.Fatal("guest agent queried after ownership config failed")
	}
}

func TestProxmoxListCrabboxServersClusterRejectsNullConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/access/permissions":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"/vms": map[string]any{"VM.Audit": 1}}})
		case "/api2/json/cluster/resources":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"vmid": 101, "node": "pve2", "type": "qemu", "name": "crabbox-null-config", "template": 0},
			}})
		case "/api2/json/nodes/pve2/qemu/101/status/current":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"vmid": 101, "name": "crabbox-null-config", "status": "running"}})
		case "/api2/json/nodes/pve2/qemu/101/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": nil})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	if _, err := client.ListCrabboxServersCluster(context.Background()); err == nil || !strings.Contains(err.Error(), "missing required data") {
		t.Fatalf("err=%v, want null config rejection", err)
	}
}

func TestProxmoxVMExistsInClusterRejectsFilteredInventory(t *testing.T) {
	resourcesCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/access/permissions":
			permissionPath := r.URL.Query().Get("path")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{permissionPath: map[string]any{"Sys.Audit": 1}}})
		case "/api2/json/cluster/resources":
			resourcesCalled = true
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	if _, err := client.VMExistsInCluster(context.Background(), "101"); err == nil || !strings.Contains(err.Error(), "/vms/101") {
		t.Fatalf("err=%v, want non-authoritative inventory rejection", err)
	}
	if resourcesCalled {
		t.Fatal("cluster resources queried without effective VM.Audit on the target")
	}
}

func TestProxmoxInventoryReadinessRequiresPropagatedVMAudit(t *testing.T) {
	resourcesCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/access/permissions":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"/vms": map[string]any{"VM.Audit": 0}}})
		case "/api2/json/cluster/resources":
			resourcesCalled = true
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	check := client.proxmoxInventoryCheck(context.Background())
	if check.Status != "failed" || check.Details["hint"] != "grant_proxmox_cluster_vm_audit" {
		t.Fatalf("inventory check=%#v", check)
	}
	if resourcesCalled {
		t.Fatal("cluster inventory queried without propagated VM.Audit")
	}
}

func TestProxmoxConfigureVMAcceptsNullSuccessData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api2/json/nodes/pve1/qemu/101/config" {
			t.Fatalf("%s %s", r.Method, r.URL.String())
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": nil})
	}))
	defer server.Close()

	client := testProxmoxClient(t, server.URL)
	if err := client.configureVM(context.Background(), 101, url.Values{"description": {"crabbox labels\n"}}); err != nil {
		t.Fatal(err)
	}
}

func TestProxmoxReadinessErrorClassifiesTimeout(t *testing.T) {
	if got := proxmoxReadinessErrorClass(context.DeadlineExceeded); got != "timeout" {
		t.Fatalf("class=%q, want timeout", got)
	}
	if got := proxmoxReadinessErrorClass(&url.Error{Op: "Get", URL: "https://pve.example", Err: context.DeadlineExceeded}); got != "timeout" {
		t.Fatalf("class=%q, want timeout", got)
	}
	if got := proxmoxReadinessErrorClass(errors.New("request timed out waiting for proxmox")); got != "timeout" {
		t.Fatalf("class=%q, want timeout", got)
	}
}

func proxmoxChecksByName(checks []ProxmoxReadinessCheck) map[string]ProxmoxReadinessCheck {
	byName := make(map[string]ProxmoxReadinessCheck, len(checks))
	for _, check := range checks {
		byName[check.Check] = check
	}
	return byName
}

// The owned child consumes the bootstrap script without executing it. Only the
// SSH readiness probe is stubbed; input delivery and native process exit are real.
func installProxmoxBootstrapChild(t *testing.T, output string, code int) (Config, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("synthetic SSH child uses a POSIX shell")
	}
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "bootstrap-input")
	script := "#!/bin/sh\ncat > " + shellQuote(inputPath) + "\n" + output + "\nexit " + fmt.Sprint(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	original := proxmoxRunSSHQuietWithOptions
	proxmoxRunSSHQuietWithOptions = func(context.Context, SSHTarget, string, string, string) error { return nil }
	t.Cleanup(func() { proxmoxRunSSHQuietWithOptions = original })
	cfg := baseConfig()
	cfg.SSHUser = "fixture-user"
	cfg.SSHPort = "22"
	cfg.SSHKey = filepath.Join(dir, "unused-key")
	cfg.SSHFallbackPorts = []string{}
	return cfg, inputPath
}

func TestProxmoxBootstrapNativeDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		code         int
		want         []string
		absent       []string
	}{
		{name: "quiet_success", output: "printf 'success-output\\n'; printf 'success-stderr\\n' >&2", code: 0},
		{name: "stderr", output: "printf 'fixture-bootstrap-apt-failed\\n' >&2", code: 1, want: []string{"proxmox guest bootstrap", "exit=1", "fixture-bootstrap-apt-failed"}},
		{name: "stdout", output: "printf 'fixture-bootstrap-stdout\\n'", code: 7, want: []string{"exit=7", "fixture-bootstrap-stdout"}},
		{name: "transport_exit", output: "printf 'fixture-ssh-disconnected\\n' >&2", code: 255, want: []string{"exit=255", "fixture-ssh-disconnected"}},
		{name: "empty", code: 1, want: []string{"proxmox guest bootstrap", "exit=1"}},
		{name: "redacted", output: "printf 'operation-failed token=synthetic-token-value PVEAPIToken=fixture@pve!proof=synthetic-pve-secret\\n' >&2", code: 1, want: []string{"operation-failed", "redacted"}, absent: []string{"synthetic-token-value", "synthetic-pve-secret", "fixture@pve!proof"}},
		{name: "truncated", output: "printf 'discard-this-prefix\\n'; printf '%s' " + shellQuote(strings.Repeat("x", 128<<10)) + "; printf 'discard-this-tail\\n' >&2", code: 1, want: []string{"diagnostics truncated", "exit=1"}, absent: []string{"discard-this-prefix", "discard-this-tail", strings.Repeat("x", 100)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, inputPath := installProxmoxBootstrapChild(t, tc.output, tc.code)
			client := &ProxmoxClient{TokenID: "fixture@pve!proof", TokenSecret: "synthetic-pve-secret"}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := client.bootstrapSSH(ctx, "fixture.invalid", cfg)
			input, readErr := os.ReadFile(inputPath)
			if readErr != nil || string(input) != proxmoxBootstrapScript(cfg) {
				t.Fatalf("bootstrap input delivery mismatch: %v", readErr)
			}
			if tc.code == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var native *exec.ExitError
			if !errors.As(err, &native) || native.ExitCode() != tc.code {
				t.Fatalf("native exit not preserved: %v", err)
			}
			if got := ExitCodeForError(err, 1); got != 1 {
				t.Fatalf("public fallback=%d, want1", got)
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("ordinary child exit acquired a context cause: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("diagnostic %q missing %q", err, want)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(err.Error(), absent) {
					t.Errorf("diagnostic includes excluded content %q", absent)
				}
			}
			if len(err.Error()) > 20<<10 {
				t.Fatalf("diagnostic unbounded: %d", len(err.Error()))
			}
		})
	}
}

func TestProxmoxBootstrapPreservesCause(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errors.Join(context.Canceled, errors.New("token=synthetic-error-secret"))} {
		t.Run(fmt.Sprint(cause), func(t *testing.T) {
			probe, input := proxmoxRunSSHQuietWithOptions, proxmoxRunSSHInput
			proxmoxRunSSHQuietWithOptions = func(context.Context, SSHTarget, string, string, string) error { return nil }
			proxmoxRunSSHInput = func(context.Context, SSHTarget, string, io.Reader, io.Writer, io.Writer) error { return cause }
			t.Cleanup(func() { proxmoxRunSSHQuietWithOptions, proxmoxRunSSHInput = probe, input })
			err := (&ProxmoxClient{}).bootstrapSSH(context.Background(), "fixture.invalid", baseConfig())
			if !errors.Is(err, cause) || ExitCodeForError(err, 1) != 1 {
				t.Fatalf("cause or code lost: %v", err)
			}
			if !strings.Contains(err.Error(), "proxmox guest bootstrap") || strings.Contains(err.Error(), "synthetic-error-secret") {
				t.Fatalf("unsafe or unstaged error: %v", err)
			}
		})
	}
}

func TestProxmoxBootstrapProbeFailureDoesNotRunInput(t *testing.T) {
	probe, input := proxmoxRunSSHQuietWithOptions, proxmoxRunSSHInput
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxmoxRunSSHQuietWithOptions = func(context.Context, SSHTarget, string, string, string) error { cancel(); return context.Canceled }
	proxmoxRunSSHInput = func(context.Context, SSHTarget, string, io.Reader, io.Writer, io.Writer) error {
		t.Fatal("bootstrap ran after failed transport probe")
		return nil
	}
	t.Cleanup(func() { proxmoxRunSSHQuietWithOptions, proxmoxRunSSHInput = probe, input })
	if err := (&ProxmoxClient{}).bootstrapSSH(ctx, "fixture.invalid", baseConfig()); !errors.Is(err, context.Canceled) {
		t.Fatalf("probe cause=%v", err)
	}
}

func TestProxmoxCreateServerFlow(t *testing.T) {
	for _, failBootstrap := range []bool{false, true} {
		t.Run(fmt.Sprintf("bootstrap_failure=%t", failBootstrap), func(t *testing.T) {
			testProxmoxCreateServerFlow(t, failBootstrap)
		})
	}
}

func testProxmoxCreateServerFlow(t *testing.T, failBootstrap bool) {
	var nativeCfg Config
	if failBootstrap {
		nativeCfg, _ = installProxmoxBootstrapChild(t, "printf 'fixture-bootstrap-cleanup-diagnostic\\n' >&2", 7)
	}
	var forms []url.Values
	var events []string
	var bootstrapInput string
	origProbe := proxmoxRunSSHQuietWithOptions
	origInput := proxmoxRunSSHInput
	proxmoxRunSSHQuietWithOptions = func(context.Context, SSHTarget, string, string, string) error { return nil }
	proxmoxRunSSHInput = func(ctx context.Context, _ SSHTarget, _ string, input io.Reader, stdout, stderr io.Writer) error {
		data, err := io.ReadAll(input)
		if err != nil {
			return err
		}
		bootstrapInput = string(data)
		if failBootstrap {
			return runSSHInput(ctx, SSHTargetFromConfig(nativeCfg, "fixture.invalid"), "sudo /bin/bash -s", strings.NewReader(bootstrapInput), stdout, stderr)
		}
		return nil
	}
	t.Cleanup(func() {
		proxmoxRunSSHQuietWithOptions = origProbe
		proxmoxRunSSHInput = origInput
	})
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
			case strings.Contains(r.URL.Path, "stop"), strings.Contains(r.URL.Path, "delete"):
				events = append(events, "wait-cleanup")
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
		case r.Method == http.MethodPost && r.URL.Path == "/api2/json/nodes/pve1/qemu/101/status/stop":
			events = append(events, "stop")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": "UPID:pve1:stop"})
		case r.Method == http.MethodDelete && r.URL.Path == "/api2/json/nodes/pve1/qemu/101":
			events = append(events, "delete")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": "UPID:pve1:delete"})
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
	if failBootstrap {
		var native *exec.ExitError
		if !errors.As(err, &native) || native.ExitCode() != 7 || !strings.Contains(err.Error(), "fixture-bootstrap-cleanup-diagnostic") || !strings.Contains(err.Error(), "proxmox guest bootstrap") {
			t.Errorf("bootstrap diagnostic/cause lost after cleanup: %v", err)
		}
		want := []string{"clone", "wait-clone", "config", "wait-config", "start", "wait-start", "stop", "wait-cleanup", "delete", "wait-cleanup"}
		if !reflect.DeepEqual(events, want) {
			t.Fatalf("events=%v want %v", events, want)
		}
		return
	}
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
	if !strings.Contains(bootstrapInput, "crabbox-ready") {
		t.Fatalf("bootstrap input=%q", bootstrapInput)
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

func testProxmoxClient(t *testing.T, serverURL string) *ProxmoxClient {
	t.Helper()
	cfg := baseConfig()
	cfg.Provider = "proxmox"
	cfg.Proxmox.APIURL = serverURL
	cfg.Proxmox.TokenID = "runner@pve!crabbox"
	cfg.Proxmox.TokenSecret = "secret"
	cfg.Proxmox.Node = "pve1"
	cfg.Proxmox.TemplateID = 9000
	client, err := NewProxmoxClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func readForm(t *testing.T, r *http.Request) url.Values {
	t.Helper()
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	return r.Form
}
