package islo

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
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

func TestIsloClientDeleteSandboxConfinesRedirects(t *testing.T) {
	crossOriginHit := false
	crossOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		crossOriginHit = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer crossOrigin.Close()

	var sameOriginRedirectHit bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/auth/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"session_token":"test-token"}`)
		case r.URL.Path == "/sandboxes/same-origin":
			http.Redirect(w, r, "/redirected-delete", http.StatusTemporaryRedirect)
		case r.URL.Path == "/redirected-delete":
			sameOriginRedirectHit = true
			if r.Header.Get("Authorization") == "" {
				t.Error("same-origin redirected request lost authorization")
			}
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/sandboxes/cross-origin":
			http.Redirect(w, r, crossOrigin.URL+"/stolen", http.StatusTemporaryRedirect)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	cfg := Config{}
	cfg.Islo.APIKey = "test-key"
	cfg.Islo.BaseURL = server.URL
	client, err := newIsloClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteSandbox(context.Background(), "same-origin"); err != nil {
		t.Fatalf("same-origin redirect should be allowed: %v", err)
	}
	if !sameOriginRedirectHit {
		t.Fatal("same-origin redirect target was not reached")
	}
	err = client.DeleteSandbox(context.Background(), "cross-origin")
	if err == nil || !strings.Contains(err.Error(), "refusing islo redirect") {
		t.Fatalf("cross-origin redirect error=%v, want refusal", err)
	}
	if crossOriginHit {
		t.Fatal("cross-origin redirect target received request")
	}
}

func TestIsloRedirectErrorsHideRejectedLocation(t *testing.T) {
	crossOriginHit := false
	crossOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		crossOriginHit = true
		t.Errorf("redirect target received %s %s auth=%q", r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"))
	}))
	defer crossOrigin.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/auth/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"session_token":"test-token"}`)
		case r.URL.Path == "/sandboxes/raw-location":
			http.Redirect(w, r, crossOrigin.URL+"/stolen?token=redirect-secret#fragment-secret", http.StatusTemporaryRedirect)
		case r.URL.Path == "/sandboxes/sdk-location":
			http.Redirect(w, r, crossOrigin.URL+"/sdk-stolen?token=redirect-secret#fragment-secret", http.StatusTemporaryRedirect)
		case r.URL.Path == "/sandboxes/redirect-limit":
			http.Redirect(w, r, "/redirect-limit/1", http.StatusTemporaryRedirect)
		case strings.HasPrefix(r.URL.Path, "/redirect-limit/"):
			hop, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/redirect-limit/"))
			if err != nil {
				t.Errorf("parse redirect hop: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if hop < 9 {
				http.Redirect(w, r, "/redirect-limit/"+strconv.Itoa(hop+1), http.StatusTemporaryRedirect)
				return
			}
			http.Redirect(w, r, "/limit-secret?token=limit-secret#limit-fragment", http.StatusTemporaryRedirect)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	cfg := Config{}
	cfg.Islo.APIKey = "test-key"
	cfg.Islo.BaseURL = server.URL
	client, err := newIsloClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		call func() error
		want string
	}{
		"raw delete": {
			call: func() error { return client.DeleteSandbox(context.Background(), "raw-location") },
			want: "refusing islo redirect",
		},
		"sdk get": {
			call: func() error {
				_, err := client.GetSandbox(context.Background(), "sdk-location")
				return err
			},
			want: "refusing islo redirect",
		},
		"redirect limit": {
			call: func() error { return client.DeleteSandbox(context.Background(), "redirect-limit") },
			want: "islo redirect stopped after 10 redirects",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.call()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("redirect error=%v, want %q", err, tc.want)
			}
			for _, leaked := range []string{"redirect-secret", "fragment-secret", "/stolen", "/sdk-stolen", "limit-secret", "limit-fragment"} {
				if strings.Contains(err.Error(), leaked) {
					t.Fatalf("redirect error leaked %q: %v", leaked, err)
				}
			}
		})
	}
	if crossOriginHit {
		t.Fatal("cross-origin redirect target received request")
	}
}

func TestIsloRedirectGuardUsesEffectiveOrigin(t *testing.T) {
	guard := isloSameOriginRedirectGuard("https://api.islo.dev", nil)
	samePort, err := http.NewRequest(http.MethodGet, "https://api.islo.dev:443/sandboxes", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := guard(samePort, nil); err != nil {
		t.Fatalf("default HTTPS port should share origin: %v", err)
	}
	otherPort, err := http.NewRequest(http.MethodGet, "https://api.islo.dev:444/sandboxes", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := guard(otherPort, nil); err == nil {
		t.Fatal("different effective port should be refused")
	}
}

func TestIsloRawErrorsRedactSessionToken(t *testing.T) {
	const secret = "islo-session-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"session_token":"`+secret+`"}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Bearer `+secret+` quota exceeded"}`)
	}))
	defer server.Close()
	cfg := Config{}
	cfg.Islo.APIKey = "test-key"
	cfg.Islo.BaseURL = server.URL
	client, err := newIsloClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}

	for name, call := range map[string]func() error{
		"delete": func() error { return client.DeleteSandbox(context.Background(), "sandbox") },
		"upload": func() error {
			return client.UploadArchive(context.Background(), "sandbox", "/workspace", strings.NewReader("archive"))
		},
		"create share": func() error {
			_, err := client.CreateShare(context.Background(), "sandbox", 8080, time.Minute)
			return err
		},
		"list shares": func() error {
			_, err := client.ListShares(context.Background(), "sandbox")
			return err
		},
		"exec stream": func() error {
			_, err := client.ExecStream(context.Background(), "sandbox", &gosdk.ExecRequest{}, io.Discard, io.Discard)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), "quota exceeded") {
				t.Fatalf("error=%v, want redacted useful provider error", err)
			}
		})
	}
}

func TestIsloSSEErrorRedactsSessionToken(t *testing.T) {
	const secret = "islo-stream-secret"
	body := "event: error\ndata: Bearer " + secret + " quota exceeded\n\n"
	_, err := parseIsloSSE(strings.NewReader(body), io.Discard, io.Discard, secret)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("parseIsloSSE error=%v, want redacted useful stream error", err)
	}
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
