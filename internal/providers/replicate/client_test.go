package replicate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestClientResolvesTokenAndSendsAuthorization(t *testing.T) {
	t.Setenv(envReplicateToken, "vendor-token")
	t.Setenv(envCrabboxReplicateToken, "crabbox-token")
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
	}))
	defer srv.Close()

	client, err := newReplicateClient(Config{Replicate: ReplicateConfig{APIURL: srv.URL}}, Runtime{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListPredictions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer crabbox-token" {
		t.Fatalf("Authorization=%q", auth)
	}
}

func TestClientRequiresToken(t *testing.T) {
	t.Setenv(envReplicateToken, "")
	t.Setenv(envCrabboxReplicateToken, "")
	_, err := newReplicateClient(Config{Replicate: ReplicateConfig{APIURL: "https://api.replicate.com/v1"}}, Runtime{})
	if err == nil || !strings.Contains(err.Error(), "needs an API token") {
		t.Fatalf("newReplicateClient error=%v", err)
	}
}

func TestAPIURLValidation(t *testing.T) {
	valid, err := validateReplicateAPIURL("https://API.Replicate.Com:443/v1/")
	if err != nil {
		t.Fatal(err)
	}
	if valid != "https://api.replicate.com/v1" {
		t.Fatalf("validated URL=%q", valid)
	}
	if _, err := validateReplicateAPIURL("http://api.replicate.com/v1"); err == nil {
		t.Fatal("non-loopback http URL unexpectedly passed")
	}
	if loopback, err := validateReplicateAPIURL("http://127.0.0.1:8080/v1"); err != nil || loopback != "http://127.0.0.1:8080/v1" {
		t.Fatalf("loopback URL=%q err=%v", loopback, err)
	}
	if _, err := validateReplicateAPIURL("https://user:pass@api.replicate.com/v1"); err == nil {
		t.Fatal("URL with userinfo unexpectedly passed")
	}
}

func TestRedirectRefusesCrossOriginBeforeTokenReplay(t *testing.T) {
	var targetRequests int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests++
		t.Errorf("redirect target received %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
	}))
	defer target.Close()

	trusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer trusted.Close()

	t.Setenv(envCrabboxReplicateToken, "redirect-secret")
	client, err := newReplicateClient(Config{Replicate: ReplicateConfig{APIURL: trusted.URL}}, Runtime{HTTP: trusted.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListPredictions(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("ListPredictions error=%v", err)
	}
	if strings.Contains(err.Error(), "redirect-secret") {
		t.Fatalf("error leaked token: %v", err)
	}
	if targetRequests != 0 {
		t.Fatalf("redirect target requests=%d, want 0", targetRequests)
	}
}

func TestRedirectFollowsSameOrigin(t *testing.T) {
	var redirectedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/predictions":
			http.Redirect(w, r, "/v1/redirected", http.StatusTemporaryRedirect)
		case "/v1/redirected":
			redirectedAuth = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv(envCrabboxReplicateToken, "same-origin-token")
	client, err := newReplicateClient(Config{Replicate: ReplicateConfig{APIURL: server.URL + "/v1"}}, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListPredictions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if redirectedAuth != "Bearer same-origin-token" {
		t.Fatalf("redirected Authorization=%q", redirectedAuth)
	}
}

func TestRedactResponseErrors(t *testing.T) {
	const token = "secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"Authorization: Bearer secret-token token=secret-token"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	client := &replicateClient{http: server.Client(), baseURL: server.URL, token: token}
	_, err := client.ListPredictions(context.Background())
	if err == nil {
		t.Fatal("ListPredictions unexpectedly passed")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked token: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error did not redact: %v", err)
	}
}

func TestPredictionCreateDeploymentPreferAndCancelAfter(t *testing.T) {
	var path, method, prefer, cancelAfter string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		method = r.Method
		prefer = r.Header.Get("Prefer")
		cancelAfter = r.Header.Get("Cancel-After")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "pred_123",
			"status": "processing",
			"logs":   "booting",
			"output": map[string]any{"exit_code": 0, "stdout": "ok\n"},
			"urls":   map[string]string{"get": "https://api.replicate.com/v1/predictions/pred_123", "cancel": "https://api.replicate.com/v1/predictions/pred_123/cancel"},
		})
	}))
	defer server.Close()

	client := &replicateClient{http: server.Client(), baseURL: server.URL, token: "tok"}
	pred, err := client.CreatePrediction(context.Background(), replicateCreatePredictionRequest{
		Deployment:      "alice/runner",
		Input:           map[string]any{"archive_url": "data:application/gzip;base64,abc"},
		WaitSecs:        5,
		CancelAfterSecs: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/deployments/alice/runner/predictions" {
		t.Fatalf("request=%s %s", method, path)
	}
	if prefer != "wait=5" || cancelAfter != "30s" {
		t.Fatalf("Prefer=%q Cancel-After=%q", prefer, cancelAfter)
	}
	input, ok := body["input"].(map[string]any)
	if !ok || input["archive_url"] != "data:application/gzip;base64,abc" {
		t.Fatalf("body=%v", body)
	}
	if pred.ID != "pred_123" || pred.Status != "processing" || pred.Logs != "booting" || pred.URLs.Cancel == "" {
		t.Fatalf("prediction=%#v", pred)
	}
	out, err := parsePredictionOutput(pred)
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 0 || out.Stdout != "ok\n" {
		t.Fatalf("runner output=%#v", out)
	}
}

func TestPredictionCreateVersionPath(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/predictions" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "pred_version", "status": "starting"})
	}))
	defer server.Close()
	client := &replicateClient{http: server.Client(), baseURL: server.URL, token: "tok"}
	pred, err := client.CreatePrediction(context.Background(), replicateCreatePredictionRequest{
		Version: "version-sha",
		Input:   map[string]any{"command": []string{"go", "test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if body["version"] != "version-sha" {
		t.Fatalf("body=%v", body)
	}
	if pred.ID != "pred_version" {
		t.Fatalf("prediction=%#v", pred)
	}
}

func TestPredictionGetListAndCancel(t *testing.T) {
	seen := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/predictions":
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{{"id": "pred_1", "status": "succeeded"}}, "next": "cursor"})
		case "/predictions/pred_1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "pred_1", "status": "succeeded", "output": map[string]any{"exit_code": 0}})
		case "/predictions/pred_1/cancel":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "pred_1", "status": "canceled"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &replicateClient{http: server.Client(), baseURL: server.URL, token: "tok"}
	list, err := client.ListPredictions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Results) != 1 || list.Results[0].ID != "pred_1" || list.Next != "cursor" {
		t.Fatalf("list=%#v", list)
	}
	got, err := client.GetPrediction(context.Background(), "pred_1")
	if err != nil {
		t.Fatal(err)
	}
	if !predictionTerminal(got.Status) {
		t.Fatalf("status %q should be terminal", got.Status)
	}
	canceled, err := client.CancelPrediction(context.Background(), "pred_1")
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Status != "canceled" {
		t.Fatalf("canceled=%#v", canceled)
	}
	want := []string{"GET /predictions", "GET /predictions/pred_1", "POST /predictions/pred_1/cancel"}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("seen=%v want=%v", seen, want)
	}
}

func TestPredictionAPIErrorsAndMalformedJSON(t *testing.T) {
	t.Run("api error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"detail":"bad"}`, http.StatusBadRequest)
		}))
		defer server.Close()
		client := &replicateClient{http: server.Client(), baseURL: server.URL, token: "tok"}
		_, err := client.GetPrediction(context.Background(), "pred")
		if err == nil || !strings.Contains(err.Error(), "status=400") {
			t.Fatalf("GetPrediction error=%v", err)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{`))
		}))
		defer server.Close()
		client := &replicateClient{http: server.Client(), baseURL: server.URL, token: "tok"}
		_, err := client.GetPrediction(context.Background(), "pred")
		if err == nil || !strings.Contains(err.Error(), "decode") {
			t.Fatalf("GetPrediction error=%v", err)
		}
	})
}

func TestPredictionCreateValidatesDeploymentTarget(t *testing.T) {
	client := &replicateClient{http: http.DefaultClient, baseURL: "https://api.replicate.com/v1", token: "tok"}
	_, err := client.CreatePrediction(context.Background(), replicateCreatePredictionRequest{Deployment: "missing-slash"})
	if err == nil || !strings.Contains(err.Error(), "owner/name") {
		t.Fatalf("CreatePrediction error=%v", err)
	}
}

func TestClientUsesConfiguredRuntimeHTTP(t *testing.T) {
	t.Setenv(envCrabboxReplicateToken, "tok")
	client, err := newReplicateClient(Config{Replicate: ReplicateConfig{APIURL: "https://api.replicate.com/v1"}}, Runtime{HTTP: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	if client.http == nil {
		t.Fatal("client HTTP is nil")
	}
}

func TestClientURLDefault(t *testing.T) {
	t.Setenv(envCrabboxReplicateToken, "tok")
	client, err := newReplicateClient(core.Config{}, Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if client.baseURL != defaultAPIURL {
		t.Fatalf("baseURL=%q", client.baseURL)
	}
}
