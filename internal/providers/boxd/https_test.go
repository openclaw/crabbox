package boxd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"nhooyr.io/websocket"
)

const testAPIKey = "fixture-api-key-never-log"
const testJWT = "fixture-jwt-never-log"

func TestCreateConsoleWireSchema(t *testing.T) {
	for _, test := range []struct {
		name, response, wantID string
	}{
		{"production shape", `{"name":"schema-test","public_ip":"192.0.2.1","status":"running","url":"https://schema-test.boxd.sh","vm_id":"created-vm"}`, "created-vm"},
		{"no inventory ID fallback", `{"name":"schema-test","public_ip":"192.0.2.1","status":"running","url":"https://schema-test.boxd.sh","id":"unverified-vm"}`, ""},
		{"vm_id is authoritative", `{"name":"schema-test","public_ip":"192.0.2.1","status":"running","url":"https://schema-test.boxd.sh","vm_id":"created-vm","id":"unverified-vm"}`, "created-vm"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			c, _ := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/vms" || r.URL.RawQuery != "" {
					t.Errorf("unexpected create request: %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer "+testJWT {
					t.Error("missing interactive session")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if len(body) != 3 || body["name"] != "schema-test" || body["org"] != "" || body["isolated"] != true {
					t.Errorf("unexpected create body: %#v", body)
				}
				io.WriteString(w, test.response)
			})
			vm, err := c.create(context.Background(), "schema-test")
			if err != nil {
				t.Fatal(err)
			}
			if vm.ID != test.wantID || vm.Name != "schema-test" || vm.PublicIP != "192.0.2.1" || vm.Status != "running" {
				t.Fatalf("incorrect create response mapping: %#v", vm)
			}
			if vm.Isolated || vm.OwnerID != "" {
				t.Fatal("create response invented isolation or ownership proof")
			}
			if calls.Load() != 1 {
				t.Fatal("create issued unexpected additional requests")
			}
		})
	}
}

func tlsClient(t *testing.T, handler http.HandlerFunc) (*consoleClient, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	cfg := core.BaseConfig()
	cfg.Boxd.APIURL = server.URL
	c, err := newConsoleClient(cfg, core.Runtime{HTTP: server.Client()}, testJWT)
	if err != nil {
		t.Fatal(err)
	}
	return c, server
}
func noSecrets(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	for _, secret := range []string{testAPIKey, testJWT, "unsafe-body", "token="} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("unsafe diagnostic: %v", err)
		}
	}
}

func TestHTTPSOriginAndRouting(t *testing.T) {
	for _, raw := range []string{"http://app.boxd.sh", "https://user:pass@app.boxd.sh", "https://app.boxd.sh/api", "https://app.boxd.sh?token=unsafe", "https://app.boxd.sh#unsafe", "https://app.boxd.sh?", "https://app.boxd.sh#", "//app.boxd.sh", "https://app.boxd.sh/%2f"} {
		cfg := core.BaseConfig()
		cfg.Boxd.APIURL = raw
		if _, err := newConsoleClient(cfg, core.Runtime{}, testJWT); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	for _, raw := range []string{"", "https://APP.BOXD.SH:443/"} {
		u, err := consoleURL(raw)
		if err != nil || u.String() != defaultConsoleURL {
			t.Fatalf("normalize %q: %v %v", raw, u, err)
		}
	}
	for _, id := range []string{"", "..", "x/y", "x?token=y", "x%2fy", "a\n"} {
		if _, err := machineRoute(id, "start"); err == nil {
			t.Fatalf("accepted ID %q", id)
		}
	}
}
func TestHTTPSRedirectRefusedBeforeAuthReplay(t *testing.T) {
	var destination atomic.Int32
	sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destination.Add(1) }))
	defer sink.Close()
	for _, code := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var calls atomic.Int32
			c, _ := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", sink.URL+"/?token="+testJWT)
				w.WriteHeader(code)
			})
			_, err := c.whoami(context.Background())
			noSecrets(t, err)
			if calls.Load() != 1 || destination.Load() != 0 {
				t.Fatal("redirect replayed")
			}
		})
	}
}
func TestHTTPSSafeErrorsAndBounds(t *testing.T) {
	for _, mode := range []string{"status", "json", "large", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "status":
					w.WriteHeader(500)
					fmt.Fprint(w, testAPIKey+testJWT+"unsafe-body")
				case "json":
					fmt.Fprint(w, testJWT+"unsafe-body")
				case "large":
					fmt.Fprint(w, strings.Repeat("x", maxAPIResponse+1))
				case "cancel":
					io.Copy(io.Discard, r.Body)
					select {
					case <-r.Context().Done():
					case <-time.After(time.Second):
					}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := c.whoami(ctx)
			noSecrets(t, err)
		})
	}
	c, _ := tlsClient(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected request") })
	c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New(testAPIKey + testJWT + "?token=unsafe-body")
	})
	_, err := c.whoami(context.Background())
	noSecrets(t, err)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHTTPSInteractiveSessionAndInventoryContract(t *testing.T) {
	var authenticated atomic.Int32
	c, server := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testJWT {
			t.Error("missing interactive bearer")
		}
		authenticated.Add(1)
		if r.URL.Path == "/api/v1/whoami" {
			io.WriteString(w, `{"user_id":"alice"}`)
			return
		}
		if r.URL.Path != "/api/v1/vms" || r.URL.Query().Get("org") != "my org" {
			t.Error("unexpected request: no auth exchange or fallback is allowed")
		}
		io.WriteString(w, `[]`)
	})
	c.org = "my org"
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.whoami(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, err := c.machines(context.Background()); err != nil {
		t.Fatal(err)
	}
	if authenticated.Load() != 11 {
		t.Fatal("unexpected authentication exchange")
	}
	if server.Client().CheckRedirect != nil || server.Client().Timeout != 0 {
		t.Fatal("mutated runtime client")
	}
}

func TestWSSBootstrapProtocolAndFailures(t *testing.T) {
	key := fixturePublicKey(t)
	for _, mode := range []string{"success", "disconnect", "exit", "missing-code", "bad-json", "bad-key", "duplicate", "missing-key", "large", "cancel", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/vms/vm-1/term" || r.URL.Query().Get("token") != testJWT {
					t.Error("wrong WSS route")
				}
				if mode == "redirect" {
					http.Redirect(w, r, "https://invalid.example/?token="+testJWT, http.StatusTemporaryRedirect)
					return
				}
				ws, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer ws.CloseNow()
				kind, data, err := ws.Read(r.Context())
				if err != nil {
					return
				}
				if kind != websocket.MessageBinary || strings.Contains(string(data), hostKeyMarker) || !strings.HasPrefix(string(data), "stty -echo; exec bash") {
					t.Error("unsafe terminal input")
				}
				if mode == "cancel" {
					_, _, _ = ws.Read(r.Context())
					return
				}
				output := "\r\n" + hostKeyMarker + key + "\r\n"
				switch mode {
				case "bad-key":
					output = hostKeyMarker + testJWT
				case "duplicate":
					output += output
				case "missing-key":
					output = "unsafe-body"
				case "large":
					output = strings.Repeat("x", maxTerminalOutput+1)
				}
				_ = ws.Write(r.Context(), websocket.MessageBinary, []byte(output))
				if mode == "disconnect" {
					return
				}
				event := `{"type":"exit","code":0}`
				switch mode {
				case "exit":
					event = `{"type":"exit","code":1,"reason":"` + testJWT + `"}`
				case "missing-code":
					event = `{"type":"exit"}`
				case "bad-json":
					event = testJWT
				}
				_ = ws.Write(r.Context(), websocket.MessageText, []byte(event))
			})
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			got, err := c.bootstrap(ctx, "vm-1", key)
			if mode == "success" {
				if err != nil || got != key {
					t.Fatalf("key=%q error=%v", got, err)
				}
			} else {
				noSecrets(t, err)
				if mode == "cancel" && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("cancellation=%v", err)
				}
			}
		})
	}
}

func TestInvalidInventoryAndForwardFailClosed(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `[{"id":"vm-1","owner_id":"alice"},{"id":"vm-1","owner_id":"alice"}]`, `[{"id":"vm-1"}]`} {
		c, _ := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, body)
		})
		if _, err := c.machines(context.Background()); err == nil {
			t.Fatalf("accepted inventory %s", body)
		}
	}
	for _, body := range []string{`{}`, `{"public_port":65536,"vm_port":2222,"protocol":"tcp"}`, `{"public_port":1234,"vm_port":22,"protocol":"tcp"}`, `{"public_port":1234,"vm_port":2222,"protocol":"udp"}`} {
		c, _ := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, body)
		})
		if _, err := c.exposeSSH(context.Background(), "vm-1"); err == nil {
			t.Fatalf("accepted forward %s", body)
		}
	}
}
func TestIdentityComesFromWhoamiNotJWT(t *testing.T) {
	c, _ := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/whoami" {
			t.Error("unexpected auth route")
		}
		io.WriteString(w, `{"user_id":"server-account"}`)
	})
	c.token = "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":"forged-account","sub":"forged-account","exp":1}`)) + ".fixture"
	user, err := c.whoami(context.Background())
	if err != nil || user != "server-account" {
		t.Fatalf("identity=%s err=%v", user, err)
	}
}

func TestAPIKeysRejectedBeforeHTTPWithoutFallback(t *testing.T) {
	c, server := tlsClient(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid token reached network") })
	cfg := core.BaseConfig()
	cfg.Boxd.APIURL = server.URL
	for _, token := range []string{"bxd_fixture-api-key", " bxd_fixture-api-key", "token\n", ""} {
		_, err := newConsoleClient(cfg, core.Runtime{HTTP: c.http}, token)
		if err == nil {
			t.Fatal("accepted invalid interactive session")
		}
		if strings.HasPrefix(strings.TrimSpace(token), "bxd_") && !strings.Contains(err.Error(), "interactive session") {
			t.Fatal("missing API-key guidance")
		}
		if token != "" && strings.Contains(err.Error(), token) {
			t.Fatal("echoed credential")
		}
	}
	t.Setenv("BOXD_TOKEN", "")
	t.Setenv("CRABBOX_BOXD_TOKEN", "")
	t.Setenv("BOXD_API_KEY", "bxd_fixture-api-key")
	t.Setenv("CRABBOX_BOXD_API_KEY", "bxd_other-api-key")
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{HTTP: c.http})
	if _, err := b.Doctor(context.Background(), core.DoctorRequest{}); err == nil {
		t.Fatal("doctor used an API-key fallback")
	}
}
func TestWSSUsesRuntimeTrustAndSanitizesTransportErrors(t *testing.T) {
	c, _ := tlsClient(t, func(http.ResponseWriter, *http.Request) { t.Error("WSS bypassed supplied transport") })
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, errors.New(r.URL.String() + testAPIKey) })
	_, err := c.bootstrap(context.Background(), "vm-1", fixturePublicKey(t))
	noSecrets(t, err)
}

func TestLifecycleWritesAreSingleAttemptAndActionsWhitelisted(t *testing.T) {
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var writes atomic.Int32
			c, _ := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
				writes.Add(1)
				w.Header().Set("Location", "https://invalid.example/?token="+testJWT)
				w.WriteHeader(code)
				io.WriteString(w, testJWT)
			})
			noSecrets(t, c.action(context.Background(), "vm-1", "destroy"))
			if writes.Load() != 1 {
				t.Fatal("retried lifecycle write")
			}
			if err := c.action(context.Background(), "vm-1", "arbitrary"); err == nil || writes.Load() != 1 {
				t.Fatal("action whitelist bypassed")
			}
		})
	}
}
