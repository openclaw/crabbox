package vast

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientSendsBearerAuthAndRefusesCrossOriginRedirect(t *testing.T) {
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("cross-origin redirect target received request with auth=%q", r.Header.Get("Authorization"))
	}))
	defer redirectTarget.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL+"/capture", http.StatusFound)
	}))
	defer server.Close()

	client, err := newVastClient(VastConfig{APIKey: "vast-secret", APIURL: server.URL}, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CheckAuth(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("err=%v want cross-origin redirect refusal", err)
	}
}

func TestClientCheckAuthUsesCurrentUserEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v0/users/current/" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer vast-secret" {
			t.Fatalf("Authorization=%q", got)
		}
		writeJSON(t, w, map[string]any{"id": 7, "username": "alice"})
	}))
	defer server.Close()

	client := newTestVastClient(t, server)
	user, err := client.CheckAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != 7 || user.Username != "alice" {
		t.Fatalf("user=%#v", user)
	}
}

func TestRedactVastAPIErrorSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"api_key":"vast-secret","instance_api_key":"inst-secret","jupyter_url":"https://host/?token=jupyter-secret","user_data":"secret cloud init","private_key":"-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----"}`))
	}))
	defer server.Close()

	client := newTestVastClient(t, server)
	_, err := client.CheckAuth(context.Background())
	if err == nil {
		t.Fatal("expected API error")
	}
	text := err.Error()
	for _, secret := range []string{"vast-secret", "inst-secret", "jupyter-secret", "secret cloud init", "BEGIN PRIVATE KEY", "abc"} {
		if strings.Contains(text, secret) {
			t.Fatalf("error leaked %q in %q", secret, text)
		}
	}
	if !strings.Contains(text, "<redacted>") {
		t.Fatalf("error was not redacted: %q", text)
	}
}

func TestOfferSearchPayloadAndDecode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v0/bundles/" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["type"] != "ondemand" ||
			body["gpu_name"].(map[string]any)["eq"] != "H100" ||
			body["verified"].(map[string]any)["eq"] != true ||
			body["rentable"].(map[string]any)["eq"] != true ||
			body["rented"].(map[string]any)["eq"] != false {
			t.Fatalf("unexpected search body: %#v", body)
		}
		order := body["order"].([]any)
		if len(order) != 1 || order[0].([]any)[0] != "dlperf_per_dphtotal" || order[0].([]any)[1] != "desc" {
			t.Fatalf("order=%#v", body["order"])
		}
		if _, ok := body["query"]; ok {
			t.Fatalf("search body should use top-level filters: %#v", body)
		}
		if body["direct_port_count"].(map[string]any)["gte"] != float64(1) ||
			body["num_gpus"].(map[string]any)["gte"] != float64(4) ||
			body["reliability"].(map[string]any)["gte"] != 0.95 ||
			body["dph_total"].(map[string]any)["lte"] != 3.5 {
			t.Fatalf("unexpected search filters: %#v", body)
		}
		writeJSON(t, w, map[string]any{"offers": []map[string]any{{"id": 11, "gpu_name": "H100", "ssh_host": "203.0.113.10", "ssh_port": 2201}}})
	}))
	defer server.Close()

	client := newTestVastClient(t, server)
	offers, err := client.SearchOffers(context.Background(), vastOfferSearchInput{Config: VastConfig{
		InstanceType:   "on-demand",
		GPUName:        "H100",
		GPUCount:       4,
		MaxDphTotal:    3.5,
		MinReliability: 0.95,
		Order:          "dlperf_per_dphtotal desc",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) != 1 || offers[0].ID != 11 || offers[0].SSHPort != 2201 {
		t.Fatalf("offers=%#v", offers)
	}
}

func TestOfferSearchPayloadMapsInterruptibleToBid(t *testing.T) {
	body := buildVastOfferSearchPayload(VastConfig{InstanceType: "interruptible"})
	if body["type"] != "bid" {
		t.Fatalf("type=%#v want bid", body["type"])
	}
}

func TestCreateInstancePayloadAndDecodeNewContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v0/asks/42/" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["runtype"] != "ssh_direct" || body["target_state"] != "running" ||
			body["cancel_unavail"] != true || body["vm"] != false ||
			body["image"] != "nvidia/cuda:12" || body["template_hash_id"] != "tpl-123" ||
			body["disk"] != float64(80) || body["label"] != "cbx1|lease|slug|active" ||
			body["ssh_key"] != "ssh-ed25519 AAAA..." {
			t.Fatalf("unexpected create body: %#v", body)
		}
		if _, ok := body["template_id"]; ok {
			t.Fatalf("create body should use template_hash_id: %#v", body)
		}
		if body["env"] != "-e CRABBOX=1" {
			t.Fatalf("env=%#v", body["env"])
		}
		writeJSON(t, w, map[string]any{"success": true, "new_contract": 99})
	}))
	defer server.Close()

	client := newTestVastClient(t, server)
	resp, err := client.CreateInstance(context.Background(), 42, vastCreateInstanceInput{
		Config:      VastConfig{Image: "nvidia/cuda:12", TemplateID: "tpl-123", Runtype: "ssh_direct", DiskGB: 80},
		Label:       "cbx1|lease|slug|active",
		SSHKey:      "ssh-ed25519 AAAA...",
		Environment: map[string]string{"CRABBOX": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.NewContract != 99 || !resp.Success {
		t.Fatalf("resp=%#v", resp)
	}
}

func TestInstanceMethodsAndDecoding(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v0/instances/99/":
			writeJSON(t, w, map[string]any{"instances": map[string]any{"id": 99, "actual_status": "running", "ssh_host": "198.51.100.8", "ssh_port": 2222, "gpu_name": "RTX 4090"}})
		case "GET /api/v1/instances/":
			if r.URL.Query().Get("limit") != "25" {
				t.Fatalf("list query=%s", r.URL.RawQuery)
			}
			switch r.URL.Query().Get("after_token") {
			case "":
				writeJSON(t, w, map[string]any{"next_token": "page-2", "instances": []map[string]any{{"id": 99}}})
			case "page-2":
				writeJSON(t, w, map[string]any{"next_token": nil, "instances": []map[string]any{{"contract_id": 100}}})
			default:
				t.Fatalf("unexpected after_token query=%s", r.URL.RawQuery)
			}
		case "PUT /api/v0/instances/99/":
			var body vastManageInstanceInput
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.State != "stopped" || body.Label != "cbx1|lease|slug|stopped" {
				t.Fatalf("manage body=%#v", body)
			}
			writeJSON(t, w, map[string]any{"instance": map[string]any{"id": 99, "intended_status": "stopped"}})
		case "DELETE /api/v0/instances/99/":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := newTestVastClient(t, server)
	instance, err := client.GetInstance(context.Background(), 99)
	if err != nil {
		t.Fatal(err)
	}
	if instance.ID != 99 || instance.SSHHost != "198.51.100.8" || instance.SSHPort != 2222 {
		t.Fatalf("instance=%#v", instance)
	}
	list, err := client.ListInstances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[1].ID != 100 {
		t.Fatalf("list=%#v", list)
	}
	managed, err := client.ManageInstance(context.Background(), 99, vastManageInstanceInput{State: "stopped", Label: "cbx1|lease|slug|stopped"})
	if err != nil {
		t.Fatal(err)
	}
	if managed.Status != "stopped" {
		t.Fatalf("managed=%#v", managed)
	}
	if err := client.DestroyInstance(context.Background(), 99); err != nil {
		t.Fatal(err)
	}
	wantSeen := "GET /api/v0/instances/99/,GET /api/v1/instances/?limit=25,GET /api/v1/instances/?after_token=page-2&limit=25,PUT /api/v0/instances/99/,DELETE /api/v0/instances/99/"
	if strings.Join(seen, ",") != wantSeen {
		t.Fatalf("seen=%v", seen)
	}
}

func TestInstanceSSHKeyMethods(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v0/instances/99/ssh/":
			writeJSON(t, w, map[string]any{"keys": []map[string]any{{"id": "key-1", "ssh_key": "ssh-ed25519 AAAA..."}}})
		case "POST /api/v0/instances/99/ssh/":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["ssh_key"] != "ssh-ed25519 AAAA..." {
				t.Fatalf("attach body=%#v", body)
			}
			writeJSON(t, w, map[string]any{"success": true, "key": map[string]any{"id": "key-1"}})
		case "DELETE /api/v0/instances/99/ssh/key-1/":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := newTestVastClient(t, server)
	keys, err := client.ListInstanceSSHKeys(context.Background(), 99)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].ID != "key-1" {
		t.Fatalf("keys=%#v", keys)
	}
	attached, err := client.AttachInstanceSSHKey(context.Background(), 99, "ssh-ed25519 AAAA...")
	if err != nil {
		t.Fatal(err)
	}
	if !attached.Success || attached.Key.ID != "key-1" {
		t.Fatalf("attached=%#v", attached)
	}
	if err := client.DetachInstanceSSHKey(context.Background(), 99, "key-1"); err != nil {
		t.Fatal(err)
	}
}

func TestClientRejectsNonHTTPSExceptLoopback(t *testing.T) {
	if _, err := newVastClient(VastConfig{APIKey: "secret", APIURL: "http://vast.example.test"}, Runtime{}); err == nil {
		t.Fatal("expected non-https non-loopback rejection")
	}
	if _, err := newVastClient(VastConfig{APIKey: "secret", APIURL: "http://127.0.0.1:8080/api/v0"}, Runtime{}); err != nil {
		t.Fatalf("loopback rejected: %v", err)
	}
}

func newTestVastClient(t *testing.T, server *httptest.Server) *vastClient {
	t.Helper()
	api, err := newVastClient(VastConfig{APIKey: "vast-secret", APIURL: server.URL + "/api/v0"}, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client, ok := api.(*vastClient)
	if !ok {
		t.Fatalf("client type=%T", api)
	}
	return client
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}
