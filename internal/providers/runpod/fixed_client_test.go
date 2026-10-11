package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestRunpodStableGraphQLIdentity(t *testing.T) {
	for _, tc := range []struct{ name, response, want string }{
		{"identity", "{\"data\":{\"myself\":{\"id\":\"account-one\"}}}", "account-one"},
		{"empty", "{\"data\":{\"myself\":{\"id\":\"\"}}}", ""},
		{"null", "{\"data\":{\"myself\":null}}", ""},
		{"errors", "{\"data\":{\"myself\":{\"id\":\"account-one\"}},\"errors\":[{\"message\":\"denied\"}]}", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/graphql" || r.Method != http.MethodPost || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer test-key" {
					t.Errorf("unexpected identity request")
				}
				var input map[string]string
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input["query"] != "query { myself { id } }" {
					t.Errorf("query=%v err=%v", input, err)
				}
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()
			cfg := core.Config{Runpod: core.RunpodConfig{APIKey: "test-key", APIURL: server.URL + "/v1"}}
			client, err := newRunpodClient(cfg, core.Runtime{HTTP: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			account, err := client.Whoami(t.Context())
			if tc.want == "" {
				if err == nil {
					t.Fatal("missing identity accepted")
				}
				return
			}
			if err != nil || account.ID != tc.want {
				t.Fatalf("identity=%+v err=%v", account, err)
			}
		})
	}
}

type fixedRoundTripper func(*http.Request) (*http.Response, error)

func (f fixedRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRunpodIdentityRoutingAndRedaction(t *testing.T) {
	key := "synthetic key/+"
	cfg := core.Config{Runpod: core.RunpodConfig{APIKey: key, APIURL: core.RunpodConfigDefaultAPIURL}}
	transport := fixedRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.runpod.io" || r.URL.Path != "/graphql" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("wrong identity route")
		}
		return nil, context.DeadlineExceeded
	})
	client, err := newRunpodClient(cfg, core.Runtime{HTTP: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Whoami(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), url.QueryEscape(key)) {
		t.Fatalf("unredacted or lost cause: %v", err)
	}
}

func TestRunpodRedirectGuardBothAPIHosts(t *testing.T) {
	for _, graphql := range []bool{false, true} {
		host, other := "rest.runpod.io", "api.runpod.io"
		if graphql {
			host, other = other, host
		}
		for _, destination := range []string{host, other, "unrelated.example"} {
			t.Run(host+"/"+destination, func(t *testing.T) {
				calls := 0
				transport := fixedRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.URL.Host != host {
						t.Fatal("redirect escaped the original API host")
					}
					if r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer test-key" {
						t.Fatal("request did not use header-only authentication")
					}
					if calls == 1 {
						return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": {"https://" + destination + "/redirected"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
					}
					body := "[]"
					if graphql {
						body = "{\"data\":{\"myself\":{\"id\":\"account-one\"}}}"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})
				client, err := newRunpodClient(core.Config{Runpod: core.RunpodConfig{APIKey: "test-key", APIURL: core.RunpodConfigDefaultAPIURL}}, core.Runtime{HTTP: &http.Client{Transport: transport}})
				if err != nil {
					t.Fatal(err)
				}
				if graphql {
					_, err = client.Whoami(t.Context())
				} else {
					_, err = client.ListPods(t.Context())
				}
				if destination == host {
					if err != nil || calls != 2 {
						t.Fatalf("same-origin redirect: calls=%d err=%v", calls, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") || calls != 1 {
					t.Fatalf("cross-origin guard: calls=%d err=%v", calls, err)
				}
			})
		}
	}
}

func TestRunpodFixedCreateMarkersAndSingleSubmission(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "metadata round trip", true: "capacity never retries"}[fail], func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/pods" {
					t.Errorf("unexpected create")
				}
				var payload map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				var env map[string]string
				if err := json.Unmarshal(payload["env"], &env); err != nil {
					t.Error(err)
				}
				if env[fixedLeaseMarker] != "cbx_abcdef123456" || env[fixedAttemptMarker] != "attempt" || env[fixedIntentMarker] != "intent" || env["PUBLIC_KEY"] != "ssh-ed25519 fixture" {
					t.Errorf("metadata=%v", env)
				}
				if fail {
					w.WriteHeader(500)
					_, _ = io.WriteString(w, "no instances currently available")
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "pod-one", "name": "fixture", "env": env})
			}))
			defer server.Close()
			client, err := newRunpodClient(core.Config{Runpod: core.RunpodConfig{APIKey: "test-key", APIURL: server.URL}}, core.Runtime{HTTP: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			input := runpodDeployInput{InstanceID: "GPU-one,GPU-two", PublicKey: "ssh-ed25519 fixture", Env: map[string]string{fixedLeaseMarker: "cbx_abcdef123456", fixedAttemptMarker: "attempt", fixedIntentMarker: "intent"}}
			pod, err := client.(fixedPodCreator).DeployFixedPod(t.Context(), input)
			if (err != nil) != fail || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if !fail && pod.Env[fixedLeaseMarker] != input.Env[fixedLeaseMarker] {
				t.Fatal("response marker lost")
			}
			if input.Env["PUBLIC_KEY"] != "" {
				t.Fatal("input mutated")
			}
		})
	}
}
