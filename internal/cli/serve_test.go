package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewCoordinatorClientRequiresCoordinator(t *testing.T) {
	cfg := Config{CoordToken: "fixture-token-not-for-output"}
	c, configured, err := newCoordinatorClient(cfg)
	if c != nil || configured || err != nil {
		t.Fatalf("optional coordinator client=%v configured=%t err=%v", c, configured, err)
	}
	c, err = NewCoordinatorClient(cfg)
	if c != nil || err == nil {
		t.Fatalf("required coordinator client=%v err=%v", c, err)
	}
	if ExitCodeForError(err, 1) != 2 || !strings.Contains(err.Error(), "broker.url") ||
		!strings.Contains(err.Error(), "crabbox login --url") || strings.Contains(err.Error(), cfg.CoordToken) {
		t.Fatalf("unexpected missing coordinator diagnostic: %v", err)
	}
}

func TestNewCoordinatorClientConfiguredAgentRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ssh-hosts" || r.Method != http.MethodGet ||
			r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("unexpected coordinator request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"hosts":[]}`))
	}))
	defer server.Close()
	c, err := NewCoordinatorClient(Config{Coordinator: server.URL, CoordToken: "fixture-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AgentRequest(t.Context(), http.MethodGet, "/v1/ssh-hosts", nil, nil); err != nil {
		t.Fatal(err)
	}
}
