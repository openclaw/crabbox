package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func capacityTestConfig(t *testing.T) {
	t.Helper()
	clearConfigEnv(t)
	t.Chdir(t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_OWNER", "local-hint@example.com")
	t.Setenv("CRABBOX_ORG", "local-org")
}

func TestCapacityOutput(t *testing.T) {
	for _, tc := range []struct {
		name         string
		count, limit int
		json         bool
	}{
		{"text", 9, 10, false}, {"full", 10, 10, false}, {"above", 11, 10, false},
		{"off", 3, 0, false}, {"json", 10, 10, true}, {"zero", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capacityTestConfig(t)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" || r.URL.RequestURI() != "/v1/capacity" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer synthetic-normal" {
					t.Errorf("wrong credential routing")
				}
				if r.Header.Get("X-Crabbox-Owner") != "local-hint@example.com" || r.Header.Get("X-Crabbox-Org") != "local-org" {
					t.Errorf("normal identity hints missing")
				}
				fmt.Fprintf(w, `{"owner":"github:12345","activeLeases":%d,"effectiveLimit":%d,"observedAt":"2026-09-02T12:00:00.000Z","leases":["must-not-escape"],"limits":{"capacityAdminOwners":["hidden"]}}`, tc.count, tc.limit)
			}))
			defer server.Close()
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			t.Setenv("CRABBOX_COORDINATOR_TOKEN", "synthetic-normal")
			t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "synthetic-admin")
			var out, stderr bytes.Buffer
			args := []string{"capacity"}
			if tc.json {
				args = append(args, "--json")
			}
			err := (App{Stdout: &out, Stderr: &stderr}).Run(context.Background(), args)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || stderr.Len() != 0 {
				t.Fatalf("calls=%d stderr=%s", calls, &stderr)
			}
			if tc.json {
				var got map[string]any
				if err := json.Unmarshal(out.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if len(got) != 4 || got["owner"] != "github:12345" || got["activeLeases"] != float64(tc.count) || got["effectiveLimit"] != float64(tc.limit) || got["observedAt"] != "2026-09-02T12:00:00.000Z" {
					t.Fatalf("unexpected JSON: %s", &out)
				}
			} else {
				for _, want := range []string{fmt.Sprintf("self-owner admission count: owner=github:12345 activeLeases=%d", tc.count), "effective owner limit: " + formatIntLimit(tc.limit), "observed at: 2026-09-02T12:00:00.000Z", "Snapshot only; not a reservation or approval to allocate."} {
					if !strings.Contains(out.String(), want) {
						t.Fatalf("missing %q in %s", want, &out)
					}
				}
			}
			if strings.Contains(out.String(), "hidden") || strings.Contains(out.String(), "must-not-escape") {
				t.Fatalf("extra response fields escaped: %s", &out)
			}
		})
	}
}

func TestCapacitySyntaxAndConfig(t *testing.T) {
	capacityTestConfig(t)
	for _, args := range [][]string{{"capacity", "alice"}, {"capacity", "--json", "alice"}, {"capacity", "--owner", "alice"}, {"capacity", "--user", "alice"}, {"capacity", "--org", "org"}, {"capacity", "--month", "2026-09"}, {"capacity", "--scope", "all"}, {"capacity", "--json=invalid"}, {"capacity"}} {
		var out, stderr bytes.Buffer
		err := (App{Stdout: &out, Stderr: &stderr}).Run(context.Background(), args)
		var exitErr ExitError
		if !errors.As(err, &exitErr) || exitErr.Code != 2 || out.Len() != 0 {
			t.Fatalf("args=%v err=%v stdout=%s", args, err, &out)
		}
	}
	for _, args := range [][]string{{"capacity", "--help"}, {"help", "capacity"}} {
		var out, stderr bytes.Buffer
		err := (App{Stdout: &out, Stderr: &stderr}).Run(context.Background(), args)
		var exitErr ExitError
		if err != nil && (!errors.As(err, &exitErr) || exitErr.Code != 0) {
			t.Fatal(err)
		}
		if !strings.Contains(out.String()+stderr.String(), "capacity") || !strings.Contains(out.String()+stderr.String(), "--json") && !strings.Contains(stderr.String(), "-json") {
			t.Fatalf("missing help: %s %s", &out, &stderr)
		}
	}
	t.Setenv("CRABBOX_COORDINATOR", "not-a-url")
	var out, stderr bytes.Buffer
	if err := (App{Stdout: &out, Stderr: &stderr}).Run(context.Background(), []string{"capacity"}); err == nil || out.Len() != 0 {
		t.Fatalf("invalid config err=%v stdout=%s", err, &out)
	}
}

func TestCapacityErrorsAndMalformedResponse(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
		want string
	}{
		{"unsupported", 404, `{"error":"not_found"}`, "capacity is unsupported"},
		{"method", 405, `{"error":"method_not_allowed"}`, "capacity is unsupported"},
		{"server", 500, `{"error":"synthetic_failure"}`, "http 500"},
		{"auth", 401, `{"error":"unauthorized"}`, "http 401"},
		{"empty", 200, `{}`, "invalid capacity response"},
		{"null", 200, `null`, "invalid capacity response"},
		{"missing-count", 200, `{"owner":"unknown","effectiveLimit":0,"observedAt":"2026-09-02T12:00:00Z"}`, "invalid capacity response"},
		{"null-limit", 200, `{"owner":"unknown","activeLeases":0,"effectiveLimit":null,"observedAt":"2026-09-02T12:00:00Z"}`, "invalid capacity response"},
		{"negative", 200, `{"owner":"unknown","activeLeases":-1,"effectiveLimit":0,"observedAt":"2026-09-02T12:00:00Z"}`, "invalid capacity response"},
		{"bad-time", 200, `{"owner":"unknown","activeLeases":0,"effectiveLimit":0,"observedAt":"yesterday"}`, "invalid capacity response"},
		{"fraction", 200, `{"owner":"unknown","activeLeases":0.5,"effectiveLimit":0,"observedAt":"2026-09-02T12:00:00Z"}`, "cannot unmarshal"},
		{"html", 200, `<html>old coordinator</html>`, "invalid character"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capacityTestConfig(t)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.RequestURI() != "/v1/capacity" {
					t.Errorf("fallback request: %s", r.URL)
				}
				w.WriteHeader(tc.code)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			t.Setenv("CRABBOX_COORDINATOR_TOKEN", "synthetic-normal")
			t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "synthetic-admin")
			var out, stderr bytes.Buffer
			err := (App{Stdout: &out, Stderr: &stderr}).Run(context.Background(), []string{"capacity", "--json"})
			if err == nil || !strings.Contains(err.Error(), tc.want) || out.Len() != 0 || calls != 1 {
				t.Fatalf("err=%v stdout=%s calls=%d", err, &out, calls)
			}
		})
	}
}
