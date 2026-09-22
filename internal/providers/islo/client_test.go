package islo

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
	"github.com/islo-labs/go-sdk/customauth"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
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
		cfg := core.Config{}
		cfg.Islo.APIKey = "test-key"
		cfg.Islo.BaseURL = srv.URL
		c, err := newIsloClient(cfg, core.Runtime{HTTP: srv.Client()})
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

	cfg := core.Config{}
	cfg.Islo.APIKey = "test-key"
	cfg.Islo.BaseURL = server.URL
	client, err := newIsloClient(cfg, core.Runtime{HTTP: server.Client()})
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
		case r.URL.Path == "/sandboxes":
			http.Redirect(w, r, crossOrigin.URL+"/sdk-stolen?token=redirect-secret#fragment-secret", http.StatusTemporaryRedirect)
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

	cfg := core.Config{}
	cfg.Islo.APIKey = "test-key"
	cfg.Islo.BaseURL = server.URL
	client, err := newIsloClient(cfg, core.Runtime{HTTP: server.Client()})
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
		"default create override": {
			call: func() error {
				defaultClient, err := newIsloClient(cfg, core.Runtime{})
				if err != nil {
					return err
				}
				_, err = defaultClient.CreateSandbox(context.Background(), &gosdk.CreateSandboxRequest{})
				return err
			},
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
	cfg := core.Config{}
	cfg.Islo.APIKey = "test-key"
	cfg.Islo.BaseURL = server.URL
	client, err := newIsloClient(cfg, core.Runtime{HTTP: server.Client()})
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

type isloCreateRoundTripFunc func(*http.Request) (*http.Response, error)

func (f isloCreateRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type isloCreateContextBody struct{ ctx context.Context }

func (b isloCreateContextBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (isloCreateContextBody) Close() error               { return nil }

func TestIsloCreateHasBoundedOperationContext(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parent time.Duration
		body   bool
		want   time.Duration
	}{
		{"headers", 6 * time.Minute, false, 5 * time.Minute},
		{"body", 6 * time.Minute, true, 5 * time.Minute},
		{"earlier caller deadline", time.Minute, false, time.Minute},
		{"earlier body deadline", time.Minute, true, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				creates := 0
				transport := isloCreateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/auth/token" {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"session_token":"synthetic-token"}`)), Header: http.Header{}}, nil
					}
					creates++
					if tc.body {
						return &http.Response{StatusCode: 201, Body: isloCreateContextBody{r.Context()}, Header: http.Header{}}, nil
					}
					<-r.Context().Done()
					return nil, r.Context().Err()
				})
				api, err := newIsloClient(core.Config{Islo: core.IsloConfig{APIKey: "synthetic-key", BaseURL: "https://example.invalid"}}, core.Runtime{HTTP: &http.Client{Transport: transport}})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), tc.parent)
				defer cancel()
				start := time.Now()
				_, err = api.CreateSandbox(ctx, &gosdk.CreateSandboxRequest{})
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != tc.want || creates != 1 {
					t.Fatalf("error=%v elapsed=%s creates=%d; want deadline after %s, one create", err, time.Since(start), creates, tc.want)
				}
			})
		})
	}
}

func TestIsloCreateSeparatesDefaultHeaderBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server := testutil.NewPipeHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/auth/token" {
				io.WriteString(w, `{"session_token":"synthetic-token"}`)
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			io.WriteString(w, `{"id":"synthetic-id","name":"crabbox-proof-abcdef"}`)
		}))
		defer server.Close()
		api, err := newIsloClient(core.Config{Islo: core.IsloConfig{APIKey: "synthetic-key", BaseURL: server.URL}}, core.Runtime{})
		if err != nil {
			t.Fatal(err)
		}
		client := api.(*isloSDKClient)
		for _, httpClient := range []*http.Client{client.httpClient, client.createHTTPClient} {
			if httpClient != nil {
				transport := httpClient.Transport
				if auth, ok := transport.(*customauth.Transport); ok {
					transport = auth.Base
				}
				transport.(*http.Transport).DialContext = server.Client().Transport.(*http.Transport).DialContext
			}
		}
		if _, err := client.auth.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
		// Scale the ordinary API guard without weakening the create transport.
		normal := client.httpClient.Transport.(*http.Transport)
		normal.ResponseHeaderTimeout = 10 * time.Millisecond
		sandbox, err := client.CreateSandbox(context.Background(), &gosdk.CreateSandboxRequest{})
		if err != nil || sandbox.GetID() != "synthetic-id" {
			t.Fatalf("create did not survive ordinary header bound: sandbox=%v err=%v", sandbox, err)
		}
		if _, err := client.GetSandbox(context.Background(), "crabbox-proof-abcdef"); err == nil {
			t.Fatal("ordinary read lost header bound")
		}
		if normal.ResponseHeaderTimeout != 10*time.Millisecond {
			t.Fatal("create mutated ordinary transport")
		}
	})
}

func TestIsloCreatePreservesAuthAndInjectedTimeouts(t *testing.T) {
	for _, mode := range []string{"default auth", "injected create"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var creates atomic.Int32
				server := testutil.NewPipeHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/auth/token" {
						creates.Add(1)
					}
					if (mode == "default auth" && r.URL.Path == "/auth/token") || r.URL.Path != "/auth/token" {
						select {
						case <-r.Context().Done():
							return
						case <-time.After(100 * time.Millisecond):
						}
					}
					if r.URL.Path == "/auth/token" {
						io.WriteString(w, `{"session_token":"synthetic-token"}`)
					} else {
						io.WriteString(w, `{"id":"synthetic-id","name":"crabbox-proof-abcdef"}`)
					}
				}))
				defer server.Close()
				rt := core.Runtime{}
				var injected *http.Client
				if mode == "injected create" {
					injected = server.Client()
					injected.Timeout = 10 * time.Millisecond
					rt.HTTP = injected
				}
				api, err := newIsloClient(core.Config{Islo: core.IsloConfig{APIKey: "synthetic-key", BaseURL: server.URL}}, rt)
				if err != nil {
					t.Fatal(err)
				}
				client := api.(*isloSDKClient)
				for _, httpClient := range []*http.Client{client.httpClient, client.createHTTPClient} {
					if httpClient != nil {
						transport := httpClient.Transport
						if auth, ok := transport.(*customauth.Transport); ok {
							transport = auth.Base
						}
						transport.(*http.Transport).DialContext = server.Client().Transport.(*http.Transport).DialContext
					}
				}
				if injected == nil {
					client.httpClient.Transport.(*http.Transport).ResponseHeaderTimeout = 10 * time.Millisecond
				} else {
					if client.createHTTPClient != nil || client.httpClient.Transport != injected.Transport || injected.Timeout != 10*time.Millisecond {
						t.Fatal("explicit HTTP client changed")
					}
					// Prime auth outside the deliberately tiny create deadline.
					authTimeout := client.httpClient.Timeout
					client.httpClient.Timeout = time.Second
					_, authErr := client.auth.Token(context.Background())
					client.httpClient.Timeout = authTimeout
					if authErr != nil {
						t.Fatal(authErr)
					}
				}
				if _, err := client.CreateSandbox(context.Background(), &gosdk.CreateSandboxRequest{}); err == nil {
					t.Fatal("create relaxed existing auth/injected timeout")
				}
				if mode == "default auth" && creates.Load() != 0 {
					t.Fatal("create followed failed authentication")
				}
			})
		})
	}
}

func TestIsloCreateCallerCancellationBeforeRequest(t *testing.T) {
	calls := 0
	api, err := newIsloClient(core.Config{Islo: core.IsloConfig{APIKey: "synthetic-key", BaseURL: "https://example.invalid"}}, core.Runtime{HTTP: &http.Client{Transport: isloCreateRoundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected request") })}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := api.CreateSandbox(ctx, &gosdk.CreateSandboxRequest{}); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("cancellation err=%v calls=%d", err, calls)
	}
}

type isloStreamSignalWriter chan struct{}

func (w isloStreamSignalWriter) Write(p []byte) (int, error) {
	select {
	case w <- struct{}{}:
	default:
	}
	return len(p), nil
}

func TestIsloCreateTransportKeepsStreamingBodyUnbounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		original := http.DefaultTransport
		originalHeader := original.(*http.Transport).ResponseHeaderTimeout
		releaseBody := make(chan struct{})
		server := testutil.NewPipeHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/auth/token" {
				io.WriteString(w, `{"session_token":"synthetic-token"}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: stdout\ndata: started\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-releaseBody:
			}
			io.WriteString(w, "event: exit\ndata: 0\n\n")
		}))
		defer server.Close()
		api, err := newIsloClient(core.Config{Islo: core.IsloConfig{APIKey: "synthetic-key", BaseURL: server.URL}}, core.Runtime{})
		if err != nil {
			t.Fatal(err)
		}
		client := api.(*isloSDKClient)
		for _, httpClient := range []*http.Client{client.httpClient, client.createHTTPClient} {
			if httpClient != nil {
				transport := httpClient.Transport
				if auth, ok := transport.(*customauth.Transport); ok {
					transport = auth.Base
				}
				transport.(*http.Transport).DialContext = server.Client().Transport.(*http.Transport).DialContext
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := client.auth.Token(ctx); err != nil {
			t.Fatal(err)
		}
		const headerBound = time.Second
		client.httpClient.Transport.(*http.Transport).ResponseHeaderTimeout = headerBound
		observed := make(isloStreamSignalWriter, 1)
		type streamResult struct {
			code int
			err  error
		}
		finished := make(chan streamResult, 1)
		go func() {
			code, err := client.ExecStream(ctx, "crabbox-proof-abcdef", &gosdk.ExecRequest{}, observed, io.Discard)
			finished <- streamResult{code, err}
		}()
		select {
		case <-observed:
		case result := <-finished:
			t.Fatalf("stream ended before the client observed its first record: %+v", result)
		case <-ctx.Done():
			t.Fatal("client did not observe the first streamed record")
		}
		// Start the body-duration proof only after the client has received headers
		// and consumed data; server-side Flush alone does not establish that boundary.
		select {
		case result := <-finished:
			t.Fatalf("stream ended while the remaining body was held: %+v", result)
		case <-time.After(2 * headerBound):
		case <-ctx.Done():
			t.Fatal("stream harness deadline expired during body hold")
		}
		close(releaseBody)
		select {
		case result := <-finished:
			if result.err != nil || result.code != 0 {
				t.Fatalf("stream body stopped at header bound: %d %v", result.code, result.err)
			}
		case <-ctx.Done():
			t.Fatal("stream did not complete after releasing its remaining body")
		}
		if http.DefaultTransport != original || original.(*http.Transport).ResponseHeaderTimeout != originalHeader {
			t.Fatal("global transport mutated")
		}
	})
}
