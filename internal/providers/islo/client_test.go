package islo

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestIsloClientDeleteSandboxHandlesEmptyAndMissing verifies the raw DELETE
// path: Islo returns an empty body (202/204) on a successful delete and 404 if
// the sandbox is already gone. All of these must be treated as success (the
// generated SDK decoder rejects the empty body, which is why crabbox issues the
// DELETE directly). A real error status must still surface.
func TestIsloClientDeleteSandboxHandlesEmptyAndMissing(t *testing.T) {
	newServer := func(deleteStatus int, deleteBody string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/auth/token":
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"session_token":"test-token"}`)
			case r.Method == http.MethodDelete:
				w.WriteHeader(deleteStatus)
				if deleteBody != "" {
					_, _ = io.WriteString(w, deleteBody)
				}
			default:
				w.WriteHeader(http.StatusInternalServerError)
			}
		}))
	}

	mkClient := func(t *testing.T, srv *httptest.Server) isloAPI {
		t.Helper()
		cfg := Config{}
		cfg.Islo.APIKey = "test-key"
		cfg.Islo.BaseURL = srv.URL
		c, err := newIsloClient(cfg, Runtime{HTTP: srv.Client()})
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		return c
	}

	t.Run("success codes", func(t *testing.T) {
		for _, code := range []int{http.StatusOK, http.StatusAccepted, http.StatusNoContent, http.StatusNotFound} {
			srv := newServer(code, "")
			c := mkClient(t, srv)
			if err := c.DeleteSandbox(context.Background(), "crabbox-x-aa11"); err != nil {
				t.Errorf("delete with status %d: unexpected error %v", code, err)
			}
			srv.Close()
		}
	})

	t.Run("error status surfaces", func(t *testing.T) {
		srv := newServer(http.StatusInternalServerError, `{"code":"INTERNAL_ERROR","message":"boom"}`)
		defer srv.Close()
		c := mkClient(t, srv)
		err := c.DeleteSandbox(context.Background(), "crabbox-x-bb22")
		if err == nil {
			t.Fatal("expected error for 500 delete, got nil")
		}
	})
}

func TestIsloShareFromAPIMarksInvalidExpiryAsSet(t *testing.T) {
	expiresAt := "not-a-time"
	share := isloShareFromAPI(isloShareResponse{
		ShareID:   "shr_123",
		URL:       "https://share.islo.dev",
		Port:      8080,
		ExpiresAt: &expiresAt,
	})
	if !share.ExpiresAtSet {
		t.Fatalf("ExpiresAtSet=false, want true for non-empty API expiry")
	}
	if !share.ExpiresAt.IsZero() {
		t.Fatalf("ExpiresAt=%s want zero for invalid expiry", share.ExpiresAt.Format(time.RFC3339))
	}
}
